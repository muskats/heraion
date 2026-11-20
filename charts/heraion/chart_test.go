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

	"sigs.k8s.io/yaml"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/dispatch"
	"github.com/muskats/heraion/internal/playbook"
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
	if j := cfg.Jira; j == nil || j.Server != "atlassian" || len(j.Projects) != 2 || j.IssueType != "Task" {
		t.Errorf("jira %+v", cfg.Jira)
	}
	if a := cfg.MCPServers["atlassian"]; a == nil || !strings.HasSuffix(a.URL, ":9000/mcp") {
		t.Errorf("atlassian %+v", a)
	}
	if cfg.Mode != config.ModeYolo {
		t.Errorf("mode %q", cfg.Mode)
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
	if m := loaded["trivy-remediate"].EffectiveMode(cfg.Mode); m != config.ModeSafe {
		t.Errorf("trivy-remediate mode %q", m)
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
	for _, want := range []string{"HERAION_CONFIG", "HERAION_LLM_API_KEY", "HERAION_GIT_TOKEN_FALLBACK",
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

func TestNamespacesBindRBACPerNamespace(t *testing.T) {
	base := []string{"--set", "llm.baseURL=https://api.openai.com/v1", "--set", "llm.model=m", "--set", "mcpServers.k8s.enabled=true"}
	count := func(objs []object, kind string) int {
		n := 0
		for _, o := range objs {
			if o.Kind == kind {
				n++
			}
		}
		return n
	}
	objs := render(t, base...)
	if count(objs, "ClusterRoleBinding") != 1 || count(objs, "RoleBinding") != 0 {
		t.Errorf("no namespaces: want one ClusterRoleBinding")
	}
	objs = render(t, append(base, "--set", "namespaces={shop,payments}")...)
	if count(objs, "ClusterRoleBinding") != 0 || count(objs, "RoleBinding") != 2 {
		t.Errorf("namespaces set: want a RoleBinding per namespace, no ClusterRoleBinding")
	}
	for _, o := range objs {
		if o.Kind == "RoleBinding" && !strings.Contains(string(o.Raw), "kind: ClusterRole\n") {
			t.Errorf("RoleBinding must reference the ClusterRole:\n%s", o.Raw)
		}
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(find(objs, "ConfigMap", "t-heraion-config").Data["config.yaml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil || strings.Join(cfg.Namespaces, ",") != "shop,payments" {
		t.Errorf("config namespaces: %v %v", cfg, err)
	}
}
