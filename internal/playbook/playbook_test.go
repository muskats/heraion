package playbook

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/muskats/heraion/internal/config"
)

const trivy = `
name: trivy-remediate
trigger: { schedule: "0 */6 * * *" }
tools: [k8s.*, repo_*, open_pr]
repos: [acme/app, acme/other]
prompt: |
  List VulnerabilityReports and open PRs.
output: pr
guardrails: { maxPRs: 3, dryRun: false }
`

func TestParse(t *testing.T) {
	p, err := Parse([]byte(trivy))
	if err != nil {
		t.Fatal(err)
	}
	if p.Guardrails.MaxPRs != 3 || p.Trigger.Schedule != "0 */6 * * *" {
		t.Errorf("%+v", p)
	}
	for name, want := range map[string]bool{
		"k8s.resources_list": true, "repo_read_file": true, "open_pr": true,
		"grafana.query": false, "k8s": false,
	} {
		if got := p.AllowsTool(name); got != want {
			t.Errorf("AllowsTool(%s) = %v", name, got)
		}
	}
}

func TestReportOutputForbidsOpenPR(t *testing.T) {
	p, err := Parse([]byte(strings.Replace(trivy, "output: pr", "output: report", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if p.AllowsTool("open_pr") {
		t.Error("report playbooks must not open PRs")
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"bad name":     "name: Bad_Name\ntrigger: {manual: true}\ntools: [x]\nprompt: p\n",
		"no trigger":   "name: a\ntools: [x]\nprompt: p\n",
		"two triggers": "name: a\ntrigger: {manual: true, webhook: am}\ntools: [x]\nprompt: p\n",
		"bad cron":     "name: a\ntrigger: {schedule: '* *'}\ntools: [x]\nprompt: p\n",
		"cron range":   "name: a\ntrigger: {schedule: '99 * * * *'}\ntools: [x]\nprompt: p\n",
		"cron macro":   "name: a\ntrigger: {schedule: '@bogus'}\ntools: [x]\nprompt: p\n",
		"no tools":     "name: a\ntrigger: {manual: true}\nprompt: p\n",
		"no prompt":    "name: a\ntrigger: {manual: true}\ntools: [x]\n",
		"bad output":   "name: a\ntrigger: {manual: true}\ntools: [x]\nprompt: p\noutput: email\n",
		"bad pattern":  "name: a\ntrigger: {manual: true}\ntools: ['[']\nprompt: p\n",
		"unknown key":  "name: a\ntrigger: {manual: true}\ntools: [x]\nprompt: p\nfoo: 1\n",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestEffectiveReposAndDryRun(t *testing.T) {
	p, _ := Parse([]byte(trivy))
	if got := p.EffectiveRepos([]string{"acme/app", "acme/third"}); !slices.Equal(got, []string{"acme/app"}) {
		t.Errorf("intersection = %v", got)
	}
	if got := p.EffectiveRepos(nil); len(got) != 0 {
		t.Errorf("empty global must allow nothing, got %v", got)
	}
	p.Repos = nil
	if got := p.EffectiveRepos([]string{"x/y"}); !slices.Equal(got, []string{"x/y"}) {
		t.Errorf("inherit = %v", got)
	}
	if p.DryRun(false) || !p.DryRun(true) {
		t.Error("dry-run resolution wrong")
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	must := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("trivy.yaml", trivy)
	must("README.md", "ignored")
	must(".hidden.yaml", "ignored: true")
	pbs, err := LoadDir(dir)
	if err != nil || len(pbs) != 1 || pbs["trivy-remediate"] == nil {
		t.Fatalf("%v %v", pbs, err)
	}
	must("dup.yml", trivy)
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got %v", err)
	}
}

func TestExamplesAreValid(t *testing.T) {
	if _, err := LoadDir("../../examples/playbooks"); err != nil {
		t.Fatal(err)
	}
}

func TestGuardrailDefaultsAndExplicitZero(t *testing.T) {
	base := "name: x\ntrigger: {manual: true}\ntools: ['*']\nprompt: p\n"
	p, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if p.Guardrails.MaxPRs != 1 || p.Guardrails.MaxIssues != 1 || p.Guardrails.MaxFileBytes != 1<<20 ||
		p.Guardrails.ReopenAfter.Hours() != 168 {
		t.Errorf("defaults: %+v", p.Guardrails)
	}
	p, err = Parse([]byte(base + "guardrails: {maxPRs: 0, maxIssues: 0, reopenAfter: 0s, writePaths: ['deploy/**']}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Guardrails.MaxPRs != 0 || p.Guardrails.MaxIssues != 0 || p.Guardrails.ReopenAfter.Duration != 0 {
		t.Errorf("explicit zero must be kept: %+v", p.Guardrails)
	}
	if _, err := Parse([]byte(base + "guardrails: {denyPaths: ['deploy/[']}\n")); err == nil {
		t.Error("malformed path pattern must be rejected")
	}
}

func TestEffectiveMode(t *testing.T) {
	for _, c := range []struct{ pb, global, want config.Mode }{
		{"", config.ModeSafe, config.ModeSafe},
		{"", "", config.ModeFull},
		{config.ModeSafe, config.ModeFull, config.ModeSafe},
		{config.ModeFull, config.ModeSafe, config.ModeSafe},
		{config.ModeFull, config.ModeFull, config.ModeFull},
	} {
		p := &Playbook{Mode: c.pb}
		if got := p.EffectiveMode(c.global); got != c.want {
			t.Errorf("playbook %q, global %q: %q, want %q", c.pb, c.global, got, c.want)
		}
	}
	if _, err := Parse([]byte("name: a\ntrigger: {manual: true}\ntools: [x]\nprompt: p\nmode: reckless\n")); err == nil {
		t.Error("invalid mode must be rejected")
	}
	if _, err := Parse([]byte("name: a\ntrigger: {manual: true}\ntools: [x]\nprompt: p\nmode: yolo\n")); err == nil ||
		!strings.Contains(err.Error(), config.ErrYoloRemoved) {
		t.Errorf("mode yolo: %v", err)
	}
	if p, err := Parse([]byte("name: a\ntrigger: {manual: true}\ntools: [x]\nprompt: p\nmode: safe\n")); err != nil || p.Mode != config.ModeSafe {
		t.Errorf("mode: %v %v", p, err)
	}
}
