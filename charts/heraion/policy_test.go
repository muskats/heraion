package chart_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/ext"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/muskats/heraion/internal/dispatch"
)

type policy struct {
	Spec struct {
		MatchConditions []struct{ Name, Expression string }    `json:"matchConditions"`
		Variables       []struct{ Name, Expression string }    `json:"variables"`
		Validations     []struct{ Expression, Message string } `json:"validations"`
	} `json:"spec"`
}

// check evaluates a policy's match conditions and validations with cel-go, as
// the API server would: it returns false and the first failed message.
func (p *policy) check(t *testing.T, object map[string]any, user string) (matched, ok bool, msg string) {
	t.Helper()
	// As strict as the API server: homogeneous literals, the strings library.
	env, err := cel.NewEnv(
		cel.HomogeneousAggregateLiterals(),
		ext.Strings(),
		cel.Variable("object", cel.DynType),
		cel.Variable("request", cel.DynType),
		cel.Variable("variables", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]any{}
	in := map[string]any{"object": object, "request": map[string]any{"userInfo": map[string]any{"username": user}}, "variables": vars}
	eval := func(expr string) any {
		ast, iss := env.Compile(expr)
		if iss.Err() != nil {
			t.Fatalf("compile %q: %v", expr, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		out, _, err := prg.Eval(in)
		if err != nil {
			t.Fatalf("eval %q: %v", expr, err)
		}
		return out.Value()
	}
	for _, m := range p.Spec.MatchConditions {
		if eval(m.Expression) != true {
			return false, true, ""
		}
	}
	for _, v := range p.Spec.Variables {
		vars[v.Name] = eval(v.Expression)
	}
	for _, v := range p.Spec.Validations {
		if eval(v.Expression) != true {
			return true, false, v.Message
		}
	}
	return true, true, ""
}

func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func get(m map[string]any, path ...string) map[string]any {
	for _, p := range path {
		m = m[p].(map[string]any)
	}
	return m
}

// The shipped admission policies admit the Jobs and Secrets the dispatcher
// creates from the chart's template, and refuse anything else the gateway's
// ServiceAccount could try.
func TestAdmissionPolicies(t *testing.T) {
	objs := render(t, "--set", "llm.baseURL=https://api.openai.com/v1", "--set", "llm.model=m",
		"--set", "repos={acme/web}", "--set", "secrets.existingSecret=heraion",
		"--set", "mcpServers.k8s.enabled=true", "--set", "mcpServers.atlassian.enabled=true",
		"--set", "playbooks.alertmanager-triage.enabled=true",
		"--set", "gateway.hooks.alertmanager.auth=token", "--set", "gateway.hooks.alertmanager.secret.existingSecret=am",
		"--set", "worker.hostAliases[0].ip=10.96.0.5", "--set", "worker.hostAliases[0].hostnames={git.corp.example}",
		"--set", "worker.dnsConfig.searches={corp.example}", "--set", "worker.caBundle.configMap=corp-ca")
	pol := func(name string) *policy {
		o := find(objs, "ValidatingAdmissionPolicy", name)
		if o == nil {
			t.Fatalf("no policy %s", name)
		}
		var p policy
		if err := yaml.Unmarshal(o.Raw, &p); err != nil {
			t.Fatal(err)
		}
		return &p
	}
	jobsPolicy, secretsPolicy := pol("default-t-heraion-jobs"), pol("default-t-heraion-event-secrets")

	// A Job and Secret exactly as the gateway creates them.
	tmplFile := filepath.Join(t.TempDir(), "job-template.yaml")
	_ = os.WriteFile(tmplFile, []byte(find(objs, "ConfigMap", "t-heraion-job-template").Data["job-template.yaml"]), 0o600)
	tmpl, err := dispatch.LoadTemplate(tmplFile)
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientset()
	d := &dispatch.Dispatcher{Client: c, Namespace: "default", Template: tmpl}
	name, _, err := d.Dispatch(context.Background(), "alertmanager-triage", "alertmanager", []byte("id"), []byte(`{"alerts":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	job, _ := c.BatchV1().Jobs("default").Get(context.Background(), name, metav1.GetOptions{})
	sec, _ := c.CoreV1().Secrets("default").Get(context.Background(), dispatch.EventSecretName(name), metav1.GetOptions{})
	const gw = "system:serviceaccount:default:t-heraion-gateway"

	for _, tc := range []struct {
		name string
		p    *policy
		obj  func() map[string]any
	}{{"job", jobsPolicy, func() map[string]any { return toMap(t, job) }}, {"secret", secretsPolicy, func() map[string]any { return toMap(t, sec) }}} {
		if matched, ok, msg := tc.p.check(t, tc.obj(), gw); !matched || !ok {
			t.Errorf("the gateway's own %s is refused: %s", tc.name, msg)
		}
		if matched, _, _ := tc.p.check(t, tc.obj(), "system:serviceaccount:default:someone-else"); matched {
			t.Errorf("%s policy applies to other users", tc.name)
		}
	}

	podSpec := func(m map[string]any) map[string]any { return get(m, "spec", "template", "spec") }
	if podSpec(toMap(t, job))["hostAliases"] == nil {
		t.Fatal("the gateway's Job has no hostAliases, so the policy's hostAliases check is not exercised")
	}
	containers := func(m map[string]any) []any { return podSpec(m)["containers"].([]any) }
	for _, tc := range []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"no playbook label", func(m map[string]any) { delete(get(m, "metadata", "labels"), "heraion.io/playbook") }},
		{"other ServiceAccount", func(m map[string]any) { podSpec(m)["serviceAccountName"] = "t-heraion-mcp-k8s" }},
		{"other image", func(m map[string]any) { containers(m)[0].(map[string]any)["image"] = "evil.example/x:1" }},
		{"command override", func(m map[string]any) {
			podSpec(m)["initContainers"].([]any)[0].(map[string]any)["command"] = []any{"python", "-c", "steal()"}
		}},
		{"extra container", func(m map[string]any) {
			podSpec(m)["containers"] = append(containers(m), map[string]any{"name": "x", "image": "docker.io/muskats/heraion:3.0.1"})
		}},
		{"other Secret in env", func(m map[string]any) {
			c := containers(m)[0].(map[string]any)
			c["env"] = append(c["env"].([]any), map[string]any{"name": "X", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "db-admin", "key": "password"}}})
		}},
		{"Secret volume named config", func(m map[string]any) {
			for _, v := range podSpec(m)["volumes"].([]any) {
				if v := v.(map[string]any); v["name"] == "config" {
					delete(v, "configMap")
					v["secret"] = map[string]any{"secretName": "db-admin"}
				}
			}
		}},
		{"other event Secret", func(m map[string]any) {
			for _, v := range podSpec(m)["volumes"].([]any) {
				if v := v.(map[string]any); v["name"] == "heraion-event" {
					v["secret"] = map[string]any{"secretName": "db-admin"}
				}
			}
		}},
		{"hostPath", func(m map[string]any) {
			podSpec(m)["volumes"] = append(podSpec(m)["volumes"].([]any), map[string]any{"name": "tmp", "hostPath": map[string]any{"path": "/"}})
		}},
		{"privileged", func(m map[string]any) {
			containers(m)[0].(map[string]any)["securityContext"].(map[string]any)["privileged"] = true
		}},
		{"hostNetwork", func(m map[string]any) { podSpec(m)["hostNetwork"] = true }},
		{"other hostAlias IP", func(m map[string]any) {
			podSpec(m)["hostAliases"] = []any{map[string]any{"ip": "203.0.113.9", "hostnames": []any{"git.corp.example"}}}
		}},
		{"extra hostAlias name", func(m map[string]any) {
			podSpec(m)["hostAliases"].([]any)[0].(map[string]any)["hostnames"] = []any{"git.corp.example", "api.openai.com"}
		}},
		{"other dnsPolicy", func(m map[string]any) { podSpec(m)["dnsPolicy"] = "None" }},
		{"other nameserver", func(m map[string]any) {
			podSpec(m)["dnsConfig"] = map[string]any{"nameservers": []any{"203.0.113.53"}}
		}},
		{"other CA ConfigMap", func(m map[string]any) {
			for _, v := range podSpec(m)["volumes"].([]any) {
				if v := v.(map[string]any); v["name"] == "ca-bundle" {
					v["configMap"] = map[string]any{"name": "evil-ca"}
				}
			}
		}},
	} {
		m := toMap(t, job)
		tc.mutate(m)
		if _, ok, _ := jobsPolicy.check(t, m, gw); ok {
			t.Errorf("job with %s is admitted", tc.name)
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"other name", func(m map[string]any) { get(m, "metadata")["name"] = "db-admin" }},
		{"no owner", func(m map[string]any) { delete(get(m, "metadata"), "ownerReferences") }},
		{"mutable", func(m map[string]any) { m["immutable"] = false }},
		{"extra key", func(m map[string]any) { get(m, "data")["token"] = "eA==" }},
	} {
		m := toMap(t, sec)
		tc.mutate(m)
		if _, ok, _ := secretsPolicy.check(t, m, gw); ok {
			t.Errorf("secret with %s is admitted", tc.name)
		}
	}
}
