//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/muskats/heraion/internal/dispatch"
	"github.com/muskats/heraion/internal/llm/llmtest"
)

const (
	hookSecret = "hook-secret-value-1"
	gitToken   = "git-token-value-123456"
	llmKey     = "sk-test-llm-key-0123456789abcdef"
	ghToken    = "ghp_abcdefghijklmnopqrstuvwxyz0123"
	repo       = "acme/web"
	playbook   = "alertmanager-triage"
)

// TestWebhookToPullRequest sends an Alertmanager webhook to `heraion serve`,
// takes the payload from the Secret it creates, and runs the worker on it
// with `heraion run`, as the Job would.
func TestWebhookToPullRequest(t *testing.T) {
	kube, gt := newKube(t), newGitea(t)
	c := renderChart(t,
		"llm.baseURL=http://llm.invalid/v1", "llm.model=mock", // per run: HERAION_LLM_BASE_URL
		"git.provider=gitea", "git.endpoint="+gt.URL, "repos={"+repo+"}",
		"gateway.maxConcurrentJobs=1", "playbooks."+playbook+".enabled=true",
		"gateway.hooks.alertmanager.auth=token", "gateway.hooks.alertmanager.secret.existingSecret=am")

	dir := t.TempDir()
	for name, pb := range c.Playbooks {
		writeFile(t, filepath.Join(dir, "playbooks", name), pb)
	}
	remotes := seedRemote(t)
	addr, metricsAddr := freeAddr(t), freeAddr(t)
	cfg := writeFile(t, filepath.Join(dir, "config.yaml"), localConfig(t, c.Config, map[string]string{
		`listen: :8080`:     "listen: " + addr + "\n  namespace: heraion",
		`metricsListen: .*`: "metricsListen: " + metricsAddr,
		`jobTemplate: .*`:   "jobTemplate: " + writeFile(t, filepath.Join(dir, "job-template.yaml"), c.JobTemplate),
		`playbooksDir: .*`:  "playbooksDir: " + filepath.Join(dir, "playbooks"),
		`workDir: .*`:       "workDir: " + dir,
		`endpoint: (.*)`:    "endpoint: $1\n  cloneBaseURL: file://" + remotes,
	}))

	gw := start(t, []string{"KUBECONFIG=" + kube.kubeconfig(t), "HERAION_HOOK_ALERTMANAGER_SECRET=" + hookSecret},
		"serve", "--config", cfg)
	waitHTTP(t, "http://"+addr+"/readyz")
	hook := "http://" + addr + "/hooks/alertmanager"

	event := alertPayload(100) // ~200 KiB: over Linux's 128 KiB limit for one env var
	var jobName string

	t.Run("gateway", func(t *testing.T) {
		if code, _ := post(t, hook, "", event); code != http.StatusUnauthorized {
			t.Errorf("no auth: %d", code)
		}
		if code, _ := post(t, hook, "wrong", event); code != http.StatusUnauthorized {
			t.Errorf("wrong token: %d", code)
		}
		code, res := post(t, hook, hookSecret, event)
		if code != http.StatusAccepted {
			t.Fatalf("200 KiB alert: %d %v", code, res)
		}
		jobName = res.Jobs[0].Job

		job := kube.job(jobName)
		sec := kube.secret(dispatch.EventSecretName(jobName))
		if sec == nil || !bytes.Equal(sec.Data[dispatch.EventKey], event) {
			t.Fatal("the event Secret must hold the exact payload")
		}
		if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != job.UID || sec.OwnerReferences[0].Kind != "Job" {
			t.Errorf("Secret must be owned by its Job: %+v", sec.OwnerReferences)
		}
		ct := job.Spec.Template.Spec.Containers[0]
		for _, e := range ct.Env {
			if len(e.Value) > 1024 {
				t.Errorf("env %s carries the payload (%d bytes)", e.Name, len(e.Value))
			}
		}
		if !slices.ContainsFunc(ct.Env, func(e corev1.EnvVar) bool {
			return e.Name == dispatch.EnvEventFile && e.Value == dispatch.EventDir+"/"+dispatch.EventKey
		}) {
			t.Errorf("HERAION_EVENT_FILE missing: %+v", ct.Env)
		}
		if !slices.Equal(ct.Args, []string{"run", "--playbook", playbook}) {
			t.Errorf("args %v", ct.Args)
		}

		// At capacity (maxConcurrentJobs 1): a redelivery is still a duplicate.
		if code, res := post(t, hook, hookSecret, event); code != http.StatusAccepted || !res.Jobs[0].Duplicate {
			t.Errorf("redelivery: %d %+v", code, res)
		}
		if code, _ := post(t, hook, hookSecret, []byte(`{"alerts":[]}`)); code != http.StatusTooManyRequests {
			t.Errorf("new alert at capacity: %d", code)
		}
		if code, _ := post(t, hook, hookSecret, bytes.Repeat([]byte("x"), 300<<10)); code != http.StatusRequestEntityTooLarge {
			t.Errorf("body over maxBodyBytes: %d", code)
		}

		// Metrics are on their own port.
		resp, err := http.Get("http://" + metricsAddr + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		for _, want := range []string{
			`heraion_webhook_requests_total{code="202",hook="alertmanager"} 2`,
			`heraion_webhook_requests_total{code="401",hook="alertmanager"} 2`,
			`heraion_dispatches_total{hook="alertmanager",playbook="` + playbook + `",result="at_capacity"} 1`,
			`heraion_jobs{playbook="` + playbook + `",status="active"} 1`,
			"go_goroutines",
		} {
			if !strings.Contains(string(body), want) {
				t.Errorf("metrics lack %s:\n%s", want, body)
			}
		}
		// The webhook port does not serve metrics.
		r, err := http.Get("http://" + addr + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode == http.StatusOK {
			t.Error("/metrics must not be served on the webhook port")
		}

		// A failed Job create leaves no Secret behind.
		kube.finishAll()
		kube.setFailJobCreate(true)
		if code, _ := post(t, hook, hookSecret, []byte(`{"third":1}`)); code != http.StatusInternalServerError {
			t.Errorf("failing create: %d", code)
		}
		kube.setFailJobCreate(false)
		if j, s := kube.counts(); j != 1 || s != 1 {
			t.Errorf("want 1 Job and 1 Secret, got %d and %d", j, s)
		}

		for v := range kube.verbs {
			if !granted(c.GatewayRole, v[0], v[1]) {
				t.Errorf("the gateway used %s %s, which the chart's Role does not grant", v[1], v[0])
			}
		}
		if strings.Contains(gw.out.String(), hookSecret) {
			t.Error("the hook secret is in the gateway log")
		}
	})
	// The worker reads the payload from the mounted Secret, as in the pod. If
	// the gateway part failed, use the payload itself, so worker failures are
	// still reported.
	payload := event
	if sec := kube.secret(dispatch.EventSecretName(jobName)); sec != nil {
		payload = sec.Data[dispatch.EventKey]
	}
	eventFile := writeFile(t, filepath.Join(dir, "event", "payload"), string(payload))
	worker := func(t *testing.T, eventFile string, turns ...llmtest.Turn) (*llmtest.Server, string) {
		t.Helper()
		llm := llmtest.New(t, turns...)
		code, out := runHeraion(t, []string{
			"HERAION_LLM_BASE_URL=" + llm.BaseURL(), "HERAION_LLM_API_KEY=" + llmKey,
			"HERAION_GIT_TOKEN=" + gitToken, "HERAION_EVENT_FILE=" + eventFile,
		}, "run", "--config", cfg, "--playbook", playbook)
		if code != 0 {
			t.Fatalf("heraion run exited %d:\n%s", code, out)
		}
		for _, s := range []string{gitToken, llmKey, ghToken} {
			if strings.Contains(out, s) {
				t.Errorf("secret %q in the worker log", s[:8])
			}
		}
		return llm, out
	}
	write := func(path, content string) llmtest.Call {
		return llmtest.Call{Name: "repo_write_file", Args: map[string]string{"repo": repo, "path": path, "content": content}}
	}
	openPR := func(title, branch string) llmtest.Call {
		return llmtest.Call{Name: "open_pr", Args: map[string]string{"repo": repo, "title": title, "branch": branch,
			"body": "Fixes CVEs. ref " + gitToken}}
	}
	bump := []llmtest.Call{write("Dockerfile", "FROM alpine:3.20\n"), openPR("Bump alpine to 3.20", "bump-alpine")}
	branch := "heraion/" + playbook + "/bump-alpine"

	t.Run("worker refuses unsafe changes and opens a PR", func(t *testing.T) {
		llm, _ := worker(t, eventFile,
			llmtest.Turn{Calls: []llmtest.Call{{Name: "repo_list", Args: map[string]any{"repo": repo, "recursive": true}}}},
			llmtest.Turn{Calls: []llmtest.Call{
				write(".github/workflows/ci.yml", "on: pull_request\n"),
				write("deploy/values.yaml", "image: web:1\ntoken: "+ghToken+"\n"),
			}},
			llmtest.Turn{Calls: []llmtest.Call{openPR("leak", "leak")}},
			llmtest.Turn{Calls: bump},
			llmtest.Turn{Final: "done"},
		)
		user := llm.Requests[0].Messages[1].Text()
		if !strings.Contains(user, "KubePodCrashLooping") || !strings.Contains(user, "[truncated]") || len(user) > 70<<10 {
			t.Errorf("event not passed, or not cut at 64 KiB (%d bytes)", len(user))
		}
		if llm.Auth[0] != "Bearer "+llmKey {
			t.Errorf("LLM auth %q", llm.Auth[0])
		}
		results := toolResults(llm)
		for _, want := range []string{"writing .github/workflows/ci.yml is not allowed", "look like credentials", "opened PR #1"} {
			if !strings.Contains(results, want) {
				t.Errorf("tool results lack %q:\n%s", want, results)
			}
		}
		if got := git(t, remotes, "--git-dir", repo+".git", "show", branch+":Dockerfile"); got != "FROM alpine:3.20" {
			t.Errorf("pushed Dockerfile %q", got)
		}
		if got := git(t, remotes, "--git-dir", repo+".git", "show", branch+":.github/workflows/ci.yml"); got != "on: push" {
			t.Errorf("workflow changed on the pushed branch: %q", got)
		}
		if refs := git(t, remotes, "--git-dir", repo+".git", "branch", "--list", "heraion/*/leak"); refs != "" {
			t.Errorf("the change with a secret was pushed: %s", refs)
		}
		prs := gt.list(repo)
		if len(prs) != 1 || strings.Contains(prs[0]["body"].(string), gitToken) || !strings.Contains(prs[0]["body"].(string), "[REDACTED]") {
			t.Errorf("PR body not redacted: %+v", prs)
		}
		if gt.unauthed != 0 {
			t.Errorf("%d Gitea API calls without a token", gt.unauthed)
		}
	})

	t.Run("a rerun reuses the open PR", func(t *testing.T) {
		llm, _ := worker(t, eventFile, llmtest.Turn{Calls: bump}, llmtest.Turn{Final: "done"})
		if !strings.Contains(toolResults(llm), "already exists") || len(gt.list(repo)) != 1 {
			t.Errorf("not deduplicated: %s", toolResults(llm))
		}
	})

	t.Run("a PR closed without merging is not reopened", func(t *testing.T) {
		gt.close(repo, 1)
		llm, _ := worker(t, eventFile, llmtest.Turn{Calls: bump}, llmtest.Turn{Final: "done"})
		if !strings.Contains(toolResults(llm), "closed without merging") || len(gt.list(repo)) != 1 {
			t.Errorf("closed PR reopened: %s", toolResults(llm))
		}
		if !slices.Contains(gt.listStates, "closed") {
			t.Errorf("closed PRs were never listed explicitly (states %v); Gitea defaults to open only", gt.listStates)
		}
	})

	t.Run("fork and untitled PRs do not interfere", func(t *testing.T) {
		gt.add(repo, "evil", "heraion/"+playbook+"/bump-musl", "mallory/web")
		gt.add(repo, "", "x", repo) // go-scm's Gitea driver turns this into a nil entry
		llm, _ := worker(t, eventFile,
			llmtest.Turn{Calls: []llmtest.Call{write("Dockerfile", "FROM alpine:3.21\n"), openPR("Bump musl", "bump-musl")}},
			llmtest.Turn{Final: "done"})
		if !strings.Contains(toolResults(llm), "opened PR #4") {
			t.Errorf("fork PR treated as ours: %s", toolResults(llm))
		}
	})

	t.Run("secrets in the event are masked, also at the 64 KiB cut", func(t *testing.T) {
		head := `{"note":"` + ghToken + `","pad":"`
		pad := strings.Repeat("x", 64<<10-len(head)-10) // the git token starts 10 bytes before the cut
		f := writeFile(t, filepath.Join(dir, "event-boundary.json"), head+pad+gitToken+strings.Repeat("y", 100)+`"}`)
		llm, _ := worker(t, f, llmtest.Turn{Final: "done"})
		user := llm.Requests[0].Messages[1].Text()
		if strings.Contains(user, "ghp_") || strings.Contains(user, "git-token") || !strings.Contains(user, "x[REDACTED]y") {
			t.Errorf("event not masked before the cut: ...%s", user[len(user)-80:])
		}
	})
}

type hookResponse struct {
	Jobs []struct {
		Job       string `json:"job"`
		Duplicate bool   `json:"duplicate"`
	} `json:"jobs"`
}

func post(t *testing.T, url, token string, body []byte) (int, hookResponse) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var res hookResponse
	_ = json.Unmarshal(b, &res)
	return resp.StatusCode, res
}

func alertPayload(n int) []byte {
	type alert struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	}
	var alerts []alert
	for i := range n {
		alerts = append(alerts, alert{"firing",
			map[string]string{"alertname": "KubePodCrashLooping", "namespace": "payments", "pod": fmt.Sprintf("api-%d", i)},
			map[string]string{"description": strings.Repeat("x", 1900)}})
	}
	b, _ := json.Marshal(map[string]any{"version": "4", "status": "firing", "alerts": alerts})
	return b
}

func granted(rules []rbacRule, resource, verb string) bool {
	for _, r := range rules {
		if slices.Contains(r.Resources, resource) && slices.Contains(r.Verbs, verb) {
			return true
		}
	}
	return false
}

func toolResults(llm *llmtest.Server) string {
	var b strings.Builder
	last := llm.Requests[len(llm.Requests)-1]
	for _, m := range last.Messages {
		if m.Role == "tool" {
			b.WriteString(m.Text() + "\n")
		}
	}
	return b.String()
}

// seedRemote creates the bare repository acme/web.git and returns the
// directory to use as git.cloneBaseURL.
func seedRemote(t *testing.T) string {
	t.Helper()
	src, remotes := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "Dockerfile"), "FROM alpine:3.18\n")
	writeFile(t, filepath.Join(src, ".github", "workflows", "ci.yml"), "on: push\n")
	writeFile(t, filepath.Join(src, "deploy", "values.yaml"), "image: web:1\n")
	git(t, src, "init", "-q", "-b", "main")
	git(t, src, "add", "-A")
	git(t, src, "commit", "-qm", "init")
	if err := os.MkdirAll(filepath.Join(remotes, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, remotes, "clone", "-q", "--bare", src, repo+".git")
	return remotes
}
