// Command heraion is the single HeraIon binary.
//
//	heraion run   --playbook NAME   run one investigation (worker Job / CronJob)
//	heraion serve                   webhook gateway that creates worker Jobs
//	heraion check --connect         validate the setup (used by helm test)
//	heraion version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/dispatch"
	"github.com/muskats/heraion/internal/guardrails"
	"github.com/muskats/heraion/internal/playbook"
	"github.com/muskats/heraion/internal/worker"
)

// Set with -ldflags "-X main.version=...".
var version = "dev"

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
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
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

func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", envOr("HERAION_CONFIG", "/etc/heraion/config.yaml"), "config file (env HERAION_CONFIG)")
	name := fs.String("playbook", os.Getenv("HERAION_PLAYBOOK"), "playbook name (env HERAION_PLAYBOOK)")
	dir := fs.String("playbooks-dir", "", "playbooks directory (overrides config playbooksDir)")
	eventPath := fs.String("event", os.Getenv("HERAION_EVENT_FILE"), "file with the trigger payload, '-' for stdin (env HERAION_EVENT_FILE)")
	dryRun := fs.Bool("dry-run", false, "log proposed PRs instead of opening them")
	level := fs.String("log-level", envOr("HERAION_LOG_LEVEL", "info"), "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		fs.Usage()
		return errors.New("--playbook is required")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if *dryRun {
		cfg.DryRun = true
	}
	if *dir != "" {
		cfg.PlaybooksDir = *dir
	}
	red := guardrails.NewRedactor()
	red.AddSources(cfg.Secrets()...)
	log := newLogger(*level, red)
	slog.SetDefault(log)

	pbs, err := playbook.LoadDir(cfg.PlaybooksDir)
	if err != nil {
		return err
	}
	pb, ok := pbs[*name]
	if !ok {
		return fmt.Errorf("playbook %q not found in %s", *name, cfg.PlaybooksDir)
	}
	var event []byte
	switch *eventPath {
	case "":
		// Jobs created by the gateway pass the payload in HERAION_EVENT.
		event = []byte(os.Getenv(dispatch.EnvEvent))
	case "-":
		event, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	default:
		event, err = os.ReadFile(*eventPath)
	}
	if err != nil {
		return fmt.Errorf("read event: %w", err)
	}

	rep, err := worker.Run(ctx, worker.Options{Config: cfg, Playbook: pb, Event: event, Redactor: red, Log: log, Version: version})
	if rep != nil {
		log.Info("run finished", "dryRun", rep.DryRun, "steps", rep.Steps, "tokens", rep.Tokens,
			"prs", rep.PRs, "summary", rep.Final)
	}
	return err
}

// newLogger returns a JSON logger that masks secrets in string and error
// values, e.g. "token ghp_abc..." is logged as "token [REDACTED]".
func newLogger(level string, red *guardrails.Redactor) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			switch a.Value.Kind() {
			case slog.KindString:
				a.Value = slog.StringValue(red.Redact(a.Value.String()))
			case slog.KindAny:
				if e, ok := a.Value.Any().(error); ok {
					a.Value = slog.StringValue(red.Redact(e.Error()))
				}
			}
			return a
		},
	}))
}
