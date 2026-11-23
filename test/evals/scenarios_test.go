package evals

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/playbook"
)

const repo = "acme/web"

// scenario is one scenarios/<name>/scenario.yaml.
type scenario struct {
	Description string `json:"description"`
	// Safety scenarios must pass every run, e.g. prompt injection.
	Safety   bool              `json:"safety"`
	Mode     config.Mode       `json:"mode"` // global mode, default full
	Playbook string            `json:"playbook"`
	Event    string            `json:"event,omitempty"`
	Files    map[string]string `json:"files"` // repo contents
	// Tools are the k8s MCP server's tools; each returns its result.
	Tools []struct {
		Name, Description, Result string
	} `json:"tools"`
	Expect struct {
		PRs *int `json:"prs,omitempty"` // exact number of PRs
		// Paths every PR may change, e.g. [Dockerfile].
		Paths []string `json:"paths,omitempty"`
		// Changed must appear in the added lines of some PR, e.g. "alpine:3.20.3".
		Changed []string `json:"changed,omitempty"`
		// Forbidden must not appear in PRs, diffs, comments, issues or logs.
		Forbidden []string `json:"forbidden,omitempty"`
	} `json:"expect"`
}

// loadScenarios reads every scenarios/<name>/scenario.yaml, by name.
func loadScenarios(t *testing.T) map[string]*scenario {
	t.Helper()
	files, _ := filepath.Glob("scenarios/*/scenario.yaml")
	if len(files) == 0 {
		t.Fatal("no scenarios")
	}
	out := map[string]*scenario{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var sc scenario
		if err := yaml.UnmarshalStrict(b, &sc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[filepath.Base(filepath.Dir(f))] = &sc
	}
	return out
}

// playbook parses the scenario's playbook and checks it against the
// global mode and repos the runner uses.
func (sc *scenario) playbook(t *testing.T) *playbook.Playbook {
	t.Helper()
	pb, err := playbook.Parse([]byte(sc.Playbook))
	if err != nil {
		t.Fatal(err)
	}
	if err := pb.ValidateAgainst(sc.globalMode(), []string{repo}); err != nil {
		t.Fatal(err)
	}
	return pb
}

func (sc *scenario) globalMode() config.Mode {
	if sc.Mode == "" {
		return config.ModeFull
	}
	return sc.Mode
}

// The scenarios load and their playbooks are valid, without a model, so a
// broken fixture fails CI and not only the eval run.
func TestScenariosAreValid(t *testing.T) {
	for name, sc := range loadScenarios(t) {
		sc.playbook(t)
		if len(sc.Files) == 0 || len(sc.Tools) == 0 {
			t.Errorf("%s: needs files and tools", name)
		}
		if sc.Safety && len(sc.Expect.Forbidden) == 0 && len(sc.Expect.Paths) == 0 {
			t.Errorf("%s: a safety scenario must expect something", name)
		}
	}
}
