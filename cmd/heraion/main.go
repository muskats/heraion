// Command heraion is the single HeraIon binary.
//
//	heraion run   --playbook NAME   run one investigation (worker Job / CronJob)
//	heraion serve                   webhook gateway that creates worker Jobs
//	heraion check --connect         validate the setup (used by helm test)
//	heraion version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/dispatch"
	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/playbook"
	"github.com/muskats/heraion/internal/worker"
)

// Set with -ldflags "-X main.version=...".
var version = "dev"

// Output streams, replaced in tests.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

const usage = `HeraIon investigates problems with an LLM and proposes fixes as pull requests.

Usage:
  heraion run --playbook NAME [flags]   run one investigation
  heraion serve [flags]                 webhook gateway: creates one worker Job per event
  heraion check [--connect]             validate config, playbooks, secrets (and connectivity)
  heraion version                       print the version

Run "heraion run -h" or "heraion serve -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "serve":
		err = cmdServe(ctx, os.Args[2:])
	case "check":
		err = cmdCheck(ctx, os.Args[2:])
	case "version", "--version":
		fmt.Fprintln(stdout, version)
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			slog.Error("heraion failed", "err", err)
		}
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// configFlag adds --config, default $HERAION_CONFIG or /etc/heraion/config.yaml.
func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", envOr("HERAION_CONFIG", "/etc/heraion/config.yaml"), "config file (env HERAION_CONFIG)")
}

// logLevelFlag adds --log-level, default $HERAION_LOG_LEVEL or info.
func logLevelFlag(fs *flag.FlagSet) *string {
	return fs.String("log-level", envOr("HERAION_LOG_LEVEL", "info"), "debug, info, warn or error")
}

// load reads the config and returns a redactor for its secrets and a
// logger that uses it. The logger also becomes slog's default.
func load(cfgPath, level string) (*config.Config, *guardrails.Redactor, *slog.Logger, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, nil, err
	}
	red := guardrails.NewRedactor()
	red.AddSources(cfg.Secrets()...)
	log := newLogger(level, red)
	slog.SetDefault(log)
	return cfg, red, log, nil
}

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := configFlag(fs)
	name := fs.String("playbook", os.Getenv("HERAION_PLAYBOOK"), "playbook name (env HERAION_PLAYBOOK)")
	dir := fs.String("playbooks-dir", "", "playbooks directory (overrides config playbooksDir)")
	eventPath := fs.String("event", os.Getenv("HERAION_EVENT_FILE"), "file with the trigger payload, '-' for stdin (env HERAION_EVENT_FILE)")
	dryRun := fs.Bool("dry-run", false, "log proposed PRs instead of opening them")
	level := logLevelFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		fs.Usage()
		return errors.New("--playbook is required")
	}

	cfg, red, log, err := load(*cfgPath, *level)
	if err != nil {
		return err
	}
	if *dryRun {
		cfg.DryRun = true
	}
	if *dir != "" {
		cfg.PlaybooksDir = *dir
	}

	pbs, err := playbook.LoadDir(cfg.PlaybooksDir)
	if err != nil {
		return err
	}
	if err := playbook.ValidateAll(pbs, cfg.Mode, cfg.Repos); err != nil {
		return err
	}
	pb, ok := pbs[*name]
	if !ok {
		return fmt.Errorf("playbook %q not found in %s", *name, cfg.PlaybooksDir)
	}
	event, err := readEvent(*eventPath)
	if err != nil {
		return fmt.Errorf("read event: %w", err)
	}

	start := time.Now()
	rep, err := worker.Run(ctx, worker.Options{Config: cfg, Playbook: pb, Event: event, EventID: os.Getenv(dispatch.EnvEventID),
		Redactor: red, Log: log, Version: version})
	// One line per run for log-based monitoring, e.g. in Loki:
	//   {app_kubernetes_io_component="worker"} | json | msg="run finished" | status="failed"
	status, errText := "ok", ""
	if err != nil {
		status, errText = "failed", err.Error()
	}
	attrs := []any{"playbook", pb.Name, "status", status, "error", errText, "durationSeconds", int(time.Since(start).Seconds())}
	if rep != nil {
		attrs = append(attrs, "dryRun", rep.DryRun, "mode", rep.Mode, "steps", rep.Steps, "tokens", rep.Tokens,
			"prs", rep.PRs, "issues", rep.Issues, "summary", rep.Final)
	}
	log.Info("run finished", attrs...)
	return err
}

// maxEventBytes caps the event read by `heraion run`: the gateway stores a
// payload in a Secret, which holds at most 1 MiB.
const maxEventBytes = 1 << 20

// readEvent reads the trigger payload from path, "-" for stdin, or returns
// nil for "" (e.g. a scheduled run). A larger payload is an error.
func readEvent(path string) ([]byte, error) {
	var r io.Reader
	switch path {
	case "":
		return nil, nil
	case "-":
		r = os.Stdin
	default:
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxEventBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxEventBytes {
		return nil, fmt.Errorf("event is larger than %d bytes", maxEventBytes)
	}
	return b, nil
}

// newLogger returns a JSON logger that masks secrets in every value, e.g.
// "token ghp_abc..." is logged as "token [REDACTED]". Slices and structs are
// JSON-encoded first; one that contained a secret is logged as that string.
func newLogger(level string, red *guardrails.Redactor) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Value.Kind() {
			case slog.KindString:
				a.Value = slog.StringValue(red.Redact(a.Value.String()))
			case slog.KindAny:
				if e, ok := a.Value.Any().(error); ok {
					a.Value = slog.StringValue(red.Redact(e.Error()))
				} else if b, err := json.Marshal(a.Value.Any()); err == nil {
					if r := red.Redact(string(b)); r != string(b) {
						a.Value = slog.StringValue(r)
					}
				}
			}
			return a
		},
	}))
}
