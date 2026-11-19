// Package chart_test renders the chart with `helm template` and loads the
// output with the binary's own loaders. A config key the binary does not
// know, e.g. a typo in a template, fails here. Skipped without helm.
package chart_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/dispatch"
	"github.com/muskats/heraion/internal/playbook"
	"github.com/muskats/heraion/internal/secret"
)

type object struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Data map[string]string `json:"data"`
	Raw  []byte            `json:"-"`
}

func render(t *testing.T, args ...string) []object {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	if _, err := os.Stat("charts"); err != nil {
		t.Skip("chart dependencies missing: run `helm dependency build charts/heraion`")
	}
	cmd := exec.Command("helm", append([]string{"template", "t", "."}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	var objs []object
	for _, doc := range strings.Split(string(out), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var o object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("invalid YAML: %v\n%s", err, doc)
		}
		if o.Kind == "" {
			continue
		}
		o.Raw = []byte(doc)
		objs = append(objs, o)
	}
	return objs
}

func find(objs []object, kind, name string) *object {
	for i := range objs {
		if objs[i].Kind == kind && objs[i].Metadata.Name == name {
			return &objs[i]
		}
	}
	return nil
}

func TestFullValuesAreAcceptedByTheBinary(t *testing.T) {
	objs := render(t, "-f", "ci/full-values.yaml")
	dir := t.TempDir()

	cm := find(objs, "ConfigMap", "t-heraion-config")
	if cm == nil {
		t.Fatal("config ConfigMap missing")
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cm.Data["config.yaml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("binary rejects rendered config: %v\n%s", err, cm.Data["config.yaml"])
	}
	if got := cfg.MCPServers["k8s"].URL; got != "http://t-heraion-mcp-k8s.default.svc:8080/mcp" {
		t.Errorf("k8s url %s", got)
	}
	if got := cfg.MCPServers["grafana"].URL; got != "http://t-grafana-mcp:8000/mcp" {
		t.Errorf("grafana url %s", got)
	}
	if h := cfg.MCPServers["datadog"].HeadersFromEnv; h["DD-API-KEY"] != "HERAION_MCP_DATADOG_DD_API_KEY" {
		t.Errorf("datadog headers %v", h)
	}
	if j := cfg.Jira; j == nil || j.Deployment != config.JiraCloud || len(j.Projects) != 2 || len(j.ReadSpaces) != 2 || j.Email == "" {
		t.Errorf("jira %+v", cfg.Jira)
	}
	if cf := cfg.Confluence; cf == nil || cf.Deployment != config.DeploymentCloud || len(cf.Spaces) != 2 || cf.MaxPageBytes != 32768 {
		t.Errorf("confluence %+v", cfg.Confluence)
	}
	if cfg.Gateway.Hooks["alertmanager"].SecretEnv != "HERAION_HOOK_ALERTMANAGER_SECRET" {
		t.Errorf("hook %+v", cfg.Gateway.Hooks["alertmanager"])
	}

	pbs := find(objs, "ConfigMap", "t-heraion-playbooks")
	pbDir := filepath.Join(dir, "playbooks")
	_ = os.Mkdir(pbDir, 0o755)
	for f, c := range pbs.Data {
		_ = os.WriteFile(filepath.Join(pbDir, f), []byte(c), 0o600)
	}
	loaded, err := playbook.LoadDir(pbDir)
	if err != nil || len(loaded) != 3 {
		t.Fatalf("playbooks: %v (%d)", err, len(loaded))
	}

	jt := find(objs, "ConfigMap", "t-heraion-job-template")
	jtPath := filepath.Join(dir, "job.yaml")
	_ = os.WriteFile(jtPath, []byte(jt.Data["job-template.yaml"]), 0o600)
	job, err := dispatch.LoadTemplate(jtPath)
	if err != nil {
		t.Fatalf("gateway rejects job template: %v", err)
	}
	env := map[string]bool{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = true
	}
	// existingSecret refs arrive as <ENV>, inline values as <ENV>_FALLBACK.
	for _, want := range []string{"HERAION_CONFIG", "HERAION_LLM_API_KEY", "HERAION_GIT_TOKEN_FALLBACK", "HERAION_JIRA_TOKEN", "HERAION_CONFLUENCE_TOKEN",
		"HERAION_MCP_DATADOG_DD_API_KEY", "HERAION_MCP_DATADOG_DD_APPLICATION_KEY_FALLBACK"} {
		if !env[want] {
			t.Errorf("worker env missing %s", want)
		}
	}
	if job.Spec.TTLSecondsAfterFinished == nil || job.Spec.ActiveDeadlineSeconds == nil {
		t.Error("worker jobs need ttlSecondsAfterFinished and activeDeadlineSeconds")
	}
	if a := job.Spec.Template.Spec.AutomountServiceAccountToken; a == nil || *a {
		t.Error("workers must not mount a service account token")
	}
}

func TestSecretsStayOutOfConfigMaps(t *testing.T) {
	objs := render(t, "-f", "ci/full-values.yaml")
	for _, o := range objs {
		if o.Kind != "ConfigMap" {
			continue
		}
		for _, secret := range []string{"gitea-token-value", "am-secret-value", "dd-app-key-value", "custom-token-value"} {
			if bytes.Contains(o.Raw, []byte(secret)) {
				t.Errorf("ConfigMap %s contains secret %q", o.Metadata.Name, secret)
			}
		}
	}
}

func TestMinimalInstall(t *testing.T) {
	objs := render(t, "--set", "llm.baseURL=https://api.openai.com/v1", "--set", "llm.model=m",
		"--set", "llm.apiKey.existingSecret=llm", "--set", "playbooks.trivy-remediate.enabled=true")
	kinds := map[string]int{}
	for _, o := range objs {
		kinds[o.Kind]++
	}
	if kinds["Deployment"] != 0 || kinds["Service"] != 0 || kinds["ClusterRole"] != 0 || kinds["Secret"] != 0 {
		t.Errorf("minimal install must be a CronJob and its config only: %v", kinds)
	}
	if kinds["CronJob"] != 1 {
		t.Errorf("want 1 CronJob, got %v", kinds)
	}
}

func TestFailsWithoutRequiredValues(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	cases := [][]string{
		{"--set", "llm.model=m"},
		{"--set", "llm.baseURL=http://x", "--set", "llm.model=m", "--set", "playbooks.alertmanager-triage.enabled=true"},
	}
	for _, c := range cases {
		if out, err := exec.Command("helm", append([]string{"template", "t", "."}, c...)...).CombinedOutput(); err == nil {
			t.Errorf("%v: expected failure, got\n%s", c, out[:min(len(out), 300)])
		}
	}
}

// Every examples/helm/values-*.yaml must render and load.
func TestExampleValues(t *testing.T) {
	files, _ := filepath.Glob("../../examples/helm/values-*.yaml")
	if len(files) == 0 {
		t.Fatal("no example values found")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			objs := render(t, "-f", f)
			cm := find(objs, "ConfigMap", "t-heraion-config")
			p := filepath.Join(t.TempDir(), "config.yaml")
			_ = os.WriteFile(p, []byte(cm.Data["config.yaml"]), 0o600)
			if _, err := config.Load(p); err != nil {
				t.Fatalf("%v\n%s", err, cm.Data["config.yaml"])
			}
			dir := t.TempDir()
			for name, c := range find(objs, "ConfigMap", "t-heraion-playbooks").Data {
				_ = os.WriteFile(filepath.Join(dir, name), []byte(c), 0o600)
			}
			if _, err := playbook.LoadDir(dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// litellm.enabled without llm.baseURL must point workers at the real
// LiteLLM Service, e.g. http://t-litellm-gateway:4000/v1.
func TestLiteLLMDefaultURL(t *testing.T) {
	objs := render(t, "-f", "../../examples/helm/values-selfhosted.yaml")
	var svc bool
	for _, o := range objs {
		if o.Kind == "Service" && o.Metadata.Name == "t-litellm-gateway" {
			svc = true
		}
	}
	cm := find(objs, "ConfigMap", "t-heraion-config")
	if !svc || !strings.Contains(cm.Data["config.yaml"], "baseURL: http://t-litellm-gateway:4000/v1") {
		t.Fatalf("service found=%v, config:\n%s", svc, cm.Data["config.yaml"])
	}
}

// All three levels on one credential, plus a shared Secret for the others.
// The rendered env is then resolved with the binary's own resolver.
func TestSecretHierarchy(t *testing.T) {
	objs := render(t, "--set", "llm.baseURL=http://x", "--set", "llm.model=m",
		"--set", "playbooks.trivy-remediate.enabled=true",
		"--set", "secrets.existingSecret=shared",
		"--set", "llm.apiKey.serviceAccountToken.audience=litellm",
		"--set", "llm.apiKey.existingSecret=llm", "--set", "llm.apiKey.key=api-key",
		"--set", "llm.apiKey.value=sk-literal")
	var cron object
	for _, o := range objs {
		if o.Kind == "CronJob" {
			cron = o
		}
	}
	var cj batchv1.CronJob
	if err := yaml.Unmarshal(cron.Raw, &cj); err != nil {
		t.Fatal(err)
	}
	pod := cj.Spec.JobTemplate.Spec.Template.Spec
	env := map[string]corev1.EnvVar{}
	for _, e := range pod.Containers[0].Env {
		env[e.Name] = e
	}

	if got := env["HERAION_LLM_API_KEY_FILE"].Value; got != "/var/run/secrets/heraion/sa/llm-api-key" {
		t.Errorf("_FILE = %q", got)
	}
	ref := env["HERAION_LLM_API_KEY"].ValueFrom.SecretKeyRef
	if ref.Name != "llm" || ref.Key != "api-key" || ref.Optional == nil || !*ref.Optional {
		t.Errorf("existingSecret ref %+v", ref)
	}
	if fb := env["HERAION_LLM_API_KEY_FALLBACK"].ValueFrom.SecretKeyRef; fb.Name != "t-heraion-secrets" || fb.Key != "llm-api-key" {
		t.Errorf("fallback ref %+v", fb)
	}
	// git.token sets nothing itself: it uses the shared Secret's default key.
	if g := env["HERAION_GIT_TOKEN"].ValueFrom.SecretKeyRef; g.Name != "shared" || g.Key != "git-token" {
		t.Errorf("shared secret ref %+v", g)
	}

	var sa *corev1.ServiceAccountTokenProjection
	for _, v := range pod.Volumes {
		if v.Projected != nil {
			sa = v.Projected.Sources[0].ServiceAccountToken
		}
	}
	if sa == nil || sa.Audience != "litellm" || sa.Path != "llm-api-key" {
		t.Fatalf("projected token %+v", sa)
	}

	// Runtime: no token file and the optional key missing -> the literal wins.
	src := secret.FromMap("HERAION_LLM_API_KEY", map[string]string{
		"HERAION_LLM_API_KEY_FILE":     "/nonexistent/llm-api-key",
		"HERAION_LLM_API_KEY":          "",
		"HERAION_LLM_API_KEY_FALLBACK": "sk-literal",
	})
	if v, _ := src.Value(); v != "sk-literal" || src.Origin() != secret.OriginFallback {
		t.Errorf("runtime resolution: %q from %s", v, src.Origin())
	}
}

func TestCSIRequiresProviderClass(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not installed")
	}
	out, err := exec.Command("helm", "template", "t", ".", "--set", "llm.baseURL=http://x", "--set", "llm.model=m",
		"--set", "playbooks.trivy-remediate.enabled=true", "--set", "llm.apiKey.csiKey=llm-api-key").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "secrets.csi.enabled") {
		t.Fatalf("csiKey without secrets.csi must fail, got %v\n%s", err, out)
	}
}
