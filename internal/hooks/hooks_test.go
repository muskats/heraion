package hooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/dispatch"
	"github.com/muskats/heraion/internal/playbook"
	"github.com/muskats/heraion/internal/secret"
)

type fakeDispatch struct {
	calls []string
	err   error
}

func (f *fakeDispatch) Dispatch(_ context.Context, pb, hook string, ev []byte) (string, bool, error) {
	f.calls = append(f.calls, pb+"|"+hook+"|"+string(ev))
	if f.err != nil {
		return "", false, f.err
	}
	return "job-" + pb, true, nil
}

func pbs(t *testing.T, srcs ...string) map[string]*playbook.Playbook {
	out := map[string]*playbook.Playbook{}
	for _, s := range srcs {
		p, err := playbook.Parse([]byte(s))
		if err != nil {
			t.Fatal(err)
		}
		out[p.Name] = p
	}
	return out
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func gateway() config.Gateway {
	return config.Gateway{MaxBodyBytes: 100, Hooks: map[string]*config.Hook{
		"alertmanager": {Auth: config.HookAuthToken, Header: "Authorization", Secret: secret.Static("am-secret")},
		"gitea":        {Auth: config.HookAuthHMAC, Header: "X-Gitea-Signature", Secret: secret.Static("gitea-secret")},
	}}
}

func setup(t *testing.T, d Dispatcher) http.Handler {
	t.Helper()
	s, err := New(gateway(), pbs(t,
		"name: triage\ntrigger: {webhook: alertmanager}\ntools: ['*']\nprompt: p\n",
		"name: triage-2\ntrigger: {webhook: alertmanager}\ntools: ['*']\nprompt: p\n",
		"name: on-push\ntrigger: {webhook: gitea}\ntools: ['*']\nprompt: p\n",
		"name: nightly\ntrigger: {schedule: '0 1 * * *'}\ntools: ['*']\nprompt: p\n",
	), d, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return s.Handler()
}

func post(h http.Handler, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func TestTokenHookFansOutToPlaybooks(t *testing.T) {
	d := &fakeDispatch{}
	w := post(setup(t, d), "/hooks/alertmanager", `{"alerts":[]}`, map[string]string{"Authorization": "Bearer am-secret"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(d.calls) != 2 || d.calls[0] != `triage|alertmanager|{"alerts":[]}` || !strings.HasPrefix(d.calls[1], "triage-2|") {
		t.Fatalf("calls %v", d.calls)
	}
}

func TestAuthFailures(t *testing.T) {
	d := &fakeDispatch{}
	h := setup(t, d)
	cases := []struct {
		path, body string
		hdr        map[string]string
		code       int
	}{
		{"/hooks/alertmanager", "{}", map[string]string{"Authorization": "Bearer wrong"}, 401},
		{"/hooks/alertmanager", "{}", nil, 401},
		{"/hooks/gitea", "{}", map[string]string{"X-Gitea-Signature": sign("wrong", "{}")}, 401},
		{"/hooks/gitea", "{}", map[string]string{"X-Gitea-Signature": "not-hex"}, 401},
		{"/hooks/gitea", "{}", map[string]string{"X-Gitea-Signature": sign("gitea-secret", "{ }")}, 401}, // body tampered
		{"/hooks/unknown", "{}", nil, 404},
		{"/hooks/alertmanager", strings.Repeat("x", 200), map[string]string{"Authorization": "am-secret"}, 413},
	}
	for _, c := range cases {
		if w := post(h, c.path, c.body, c.hdr); w.Code != c.code {
			t.Errorf("%s %v: got %d want %d", c.path, c.hdr, w.Code, c.code)
		}
	}
	if len(d.calls) != 0 {
		t.Fatalf("nothing must be dispatched: %v", d.calls)
	}
	if w := post(h, "/hooks/gitea", "{}", map[string]string{"X-Gitea-Signature": sign("gitea-secret", "{}")}); w.Code != 202 {
		t.Fatalf("valid HMAC rejected: %d", w.Code)
	}
}

func TestCapacityReturns429(t *testing.T) {
	w := post(setup(t, &fakeDispatch{err: dispatch.ErrAtCapacity}), "/hooks/alertmanager", "{}", map[string]string{"Authorization": "am-secret"})
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("%d", w.Code)
	}
	w = post(setup(t, &fakeDispatch{err: errors.New("apiserver down")}), "/hooks/alertmanager", "{}", map[string]string{"Authorization": "am-secret"})
	if w.Code != 500 || bytes.Contains(w.Body.Bytes(), []byte("apiserver")) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestStartupValidation(t *testing.T) {
	gw := gateway()
	gw.Hooks["alertmanager"].Secret = secret.FromMap("HERAION_HOOK_ALERTMANAGER_SECRET", nil)
	_, err := New(gw, pbs(t, "name: triage\ntrigger: {webhook: alertmanager}\ntools: ['*']\nprompt: p\n"), &fakeDispatch{}, quiet)
	if err == nil {
		t.Fatal("empty secret must fail startup")
	}
	_, err = New(gateway(), pbs(t, "name: x\ntrigger: {webhook: jira}\ntools: ['*']\nprompt: p\n"), &fakeDispatch{}, quiet)
	if err == nil || !strings.Contains(err.Error(), "gateway.hooks.jira") {
		t.Fatalf("missing hook config must fail: %v", err)
	}
}

func TestHealth(t *testing.T) {
	h := setup(t, &fakeDispatch{})
	for _, p := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != 200 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
}

func TestMetrics(t *testing.T) {
	s, err := New(gateway(), pbs(t, "name: triage\ntrigger: {webhook: alertmanager}\ntools: ['*']\nprompt: p\n"), &fakeDispatch{}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	post(h, "/hooks/alertmanager", "{}", map[string]string{"Authorization": "am-secret"})
	post(h, "/hooks/alertmanager", "{}", map[string]string{"Authorization": "wrong"})
	post(h, "/hooks/random-path-1", "{}", nil)
	post(h, "/hooks/random-path-2", "{}", nil)

	for _, c := range []struct {
		got  prometheus.Collector
		want float64
	}{
		{s.requests.WithLabelValues("alertmanager", "202"), 1},
		{s.requests.WithLabelValues("alertmanager", "401"), 1},
		{s.requests.WithLabelValues("unknown", "404"), 2}, // random paths share one label value
		{s.dispatches.WithLabelValues("alertmanager", "triage", "created"), 1},
	} {
		if got := testutil.ToFloat64(c.got); got != c.want {
			t.Errorf("%v = %v, want %v", c.got, got, c.want)
		}
	}
	if n := testutil.CollectAndCount(s.requests); n != 3 {
		t.Errorf("want 3 request series, got %d", n)
	}
}
