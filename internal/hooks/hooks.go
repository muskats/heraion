// Package hooks serves POST /hooks/<name>: it checks the secret, then
// dispatches every playbook bound to that hook.
package hooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/muskats/heraion/internal/config"
	"github.com/muskats/heraion/internal/dispatch"
	"github.com/muskats/heraion/internal/playbook"
)

// Dispatcher starts an investigation. *dispatch.Dispatcher implements it.
type Dispatcher interface {
	Dispatch(ctx context.Context, playbook, hook string, event []byte) (name string, created bool, err error)
}

// Server serves POST /hooks/{name}, /healthz and /readyz.
type Server struct {
	hooks     map[string]*config.Hook
	playbooks map[string][]string // hook -> playbook names
	dispatch  Dispatcher
	maxBody   int64
	log       *slog.Logger
}

// New binds hooks to webhook playbooks. A missing hook or an empty secret
// fails startup rather than the first alert.
func New(gw config.Gateway, pbs map[string]*playbook.Playbook, d Dispatcher, log *slog.Logger) (*Server, error) {
	s := &Server{hooks: gw.Hooks, playbooks: map[string][]string{}, dispatch: d, maxBody: gw.MaxBodyBytes, log: log}
	var errs []error
	for _, pb := range pbs {
		name := pb.Trigger.Webhook
		if name == "" {
			continue
		}
		h := gw.Hooks[name]
		if h == nil {
			errs = append(errs, fmt.Errorf("playbook %s uses webhook %q, but gateway.hooks.%s is not configured", pb.Name, name, name))
			continue
		}
		if !h.Insecure && h.Secret == "" {
			errs = append(errs, fmt.Errorf("gateway.hooks.%s: env %s is empty", name, h.SecretEnv))
			continue
		}
		s.playbooks[name] = append(s.playbooks[name], pb.Name)
	}
	for name := range gw.Hooks {
		if len(s.playbooks[name]) == 0 {
			log.Warn("hook has no playbook and will return 404", "hook", name)
		}
		sort.Strings(s.playbooks[name])
	}
	if len(s.playbooks) == 0 && len(errs) == 0 {
		errs = append(errs, errors.New("no webhook playbooks: the gateway has nothing to serve"))
	}
	return s, errors.Join(errs...)
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /hooks/{name}", s.handleHook)
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") }
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /readyz", ok)
	return mux
}

// JobResult is one entry of the 202 response.
type JobResult struct {
	Playbook  string `json:"playbook"`
	Job       string `json:"job,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	h, pbs := s.hooks[name], s.playbooks[name]
	if h == nil || len(pbs) == 0 {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		http.Error(w, "payload too large or unreadable", http.StatusRequestEntityTooLarge)
		return
	}
	if !h.Insecure && !verify(h, r.Header, body) {
		s.log.Warn("webhook authentication failed", "hook", name, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	status := http.StatusAccepted
	results := make([]JobResult, 0, len(pbs))
	for _, pb := range pbs {
		job, created, err := s.dispatch.Dispatch(r.Context(), pb, name, body)
		res := JobResult{Playbook: pb, Job: job, Duplicate: err == nil && !created}
		switch {
		case errors.Is(err, dispatch.ErrAtCapacity):
			// 429 makes the sender retry; the retry gets the same Job name.
			status = http.StatusTooManyRequests
			res.Error = err.Error()
		case err != nil:
			if status != http.StatusTooManyRequests {
				status = http.StatusInternalServerError
			}
			res.Error = "dispatch failed"
			s.log.Error("dispatch failed", "hook", name, "playbook", pb, "err", err)
		default:
			s.log.Info("investigation dispatched", "hook", name, "playbook", pb, "job", job, "duplicate", !created)
		}
		results = append(results, res)
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"jobs": results})
}

// verify checks the request against the hook's secret in constant time.
func verify(h *config.Hook, hdr http.Header, body []byte) bool {
	got := strings.TrimSpace(hdr.Get(h.Header))
	if got == "" || h.Secret == "" {
		return false
	}
	switch h.Auth {
	case config.HookAuthToken:
		got = strings.TrimPrefix(got, "Bearer ")
		return subtle.ConstantTimeCompare([]byte(got), []byte(h.Secret)) == 1
	default: // hmac-sha256
		got = strings.TrimPrefix(got, "sha256=")
		sig, err := hex.DecodeString(got)
		if err != nil {
			return false
		}
		mac := hmac.New(sha256.New, []byte(h.Secret))
		mac.Write(body)
		return hmac.Equal(sig, mac.Sum(nil))
	}
}
