//go:build e2e

// Package e2e runs the heraion binary against the chart's rendered config
// and mock servers: a kube-apiserver (Jobs, Secrets), an OpenAI-compatible
// LLM, a Gitea API, and a real bare Git remote. Run it with
// scripts/test-e2e.sh or:
//
//	go test -tags e2e ./test/e2e/
//
// It needs helm (to render the chart) and git (to seed the remote).
package e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

var (
	bin     string // the heraion binary under test
	repoDir string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	wd, _ := os.Getwd()
	repoDir = filepath.Join(wd, "..", "..")
	for _, tool := range []string{"helm", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: %s is required\n", tool)
			return 1
		}
	}
	tmp, err := os.MkdirTemp("", "heraion-e2e-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)
	bin = filepath.Join(tmp, "heraion")
	if b := os.Getenv("HERAION_BIN"); b != "" {
		bin = b // e.g. a release build
	} else {
		build := exec.Command("go", "build", "-o", bin, "./cmd/heraion")
		build.Dir, build.Stdout, build.Stderr = repoDir, os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: build:", err)
			return 1
		}
	}
	return m.Run()
}

// chart holds what `helm template` renders for the gateway and workers.
type chart struct {
	Config      string            // config.yaml
	JobTemplate string            // job-template.yaml
	Playbooks   map[string]string // file name -> playbook
	GatewayRole []rbacRule
}

type rbacRule struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

// renderChart runs helm template with the values in set (key=value).
func renderChart(t *testing.T, set ...string) chart {
	t.Helper()
	args := []string{"template", "t", filepath.Join(repoDir, "charts", "heraion"), "-n", "heraion"}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	c := chart{Playbooks: map[string]string{}}
	for _, doc := range strings.Split(string(out), "\n---") {
		var o struct {
			Kind     string                `json:"kind"`
			Metadata struct{ Name string } `json:"metadata"`
			Data     map[string]string     `json:"data"`
			Rules    []rbacRule            `json:"rules"`
		}
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("invalid YAML: %v", err)
		}
		switch {
		case o.Kind == "ConfigMap" && o.Metadata.Name == "t-heraion-config":
			c.Config = o.Data["config.yaml"]
		case o.Kind == "ConfigMap" && o.Metadata.Name == "t-heraion-job-template":
			c.JobTemplate = o.Data["job-template.yaml"]
		case o.Kind == "ConfigMap" && o.Metadata.Name == "t-heraion-playbooks":
			c.Playbooks = o.Data
		case o.Kind == "Role" && o.Metadata.Name == "t-heraion-gateway":
			c.GatewayRole = o.Rules
		}
	}
	if c.Config == "" {
		t.Fatal("rendered chart has no config")
	}
	return c
}

// localConfig rewrites the in-cluster paths and addresses of the rendered
// config for a local run. repl maps a regexp to its replacement.
func localConfig(t *testing.T, cfg string, repl map[string]string) string {
	t.Helper()
	for re, to := range repl {
		r := regexp.MustCompile(re)
		if !r.MatchString(cfg) {
			t.Fatalf("config has no %q:\n%s", re, cfg)
		}
		cfg = r.ReplaceAllString(cfg, to)
	}
	return cfg
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// process is a running heraion command; its output is kept for failures.
type process struct {
	cmd *exec.Cmd
	out *bytes.Buffer
}

func start(t *testing.T, env []string, args ...string) *process {
	t.Helper()
	p := &process{cmd: exec.Command(bin, args...), out: &bytes.Buffer{}}
	p.cmd.Env = append(os.Environ(), env...)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		_ = p.cmd.Wait()
		if t.Failed() {
			t.Logf("heraion %s output:\n%s", args[0], p.out)
		}
	})
	return p
}

// runHeraion runs a command to completion and returns its exit code and output.
func runHeraion(t *testing.T, env []string, args ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	} else if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not become ready", url)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
