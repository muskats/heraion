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
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"

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

	requests   *prometheus.CounterVec // hook, code
	dispatches *prometheus.CounterVec // hook, playbook, result
}

// New binds hooks to webhook playbooks. A missing hook or an empty secret
// fails startup rather than the first alert.
func New(gw config.Gateway, pbs map[string]*playbook.Playbook, d Dispatcher, log *slog.Logger) (*Server, error) {
	s := &Server{hooks: gw.Hooks, playbooks: map[string][]string{}, dispatch: d, maxBody: gw.MaxBodyBytes, log: log,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "heraion_webhook_requests_total",
			Help: "Webhook requests by hook and HTTP status code. Unknown hook names count as hook=\"unknown\".",
		}, []string{"hook", "code"}),
		dispatches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "heraion_dispatches_total",
			Help: "Worker Jobs requested by webhooks, by result: created, duplicate, at_capacity or error.",
		}, []string{"hook", "playbook", "result"}),
	}
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
		if !h.Insecure {
			if _, err := h.Secret.Value(); err != nil {
				errs = append(errs, fmt.Errorf("gateway.hooks.%s: %w", name, err))
				continue
			}
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
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return s, nil
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

// Collectors returns the gateway's metrics, for a Prometheus registry.
func (s *Server) Collectors() []prometheus.Collector {
	return []prometheus.Collector{s.requests, s.dispatches}
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	code := s.serveHook(w, r, name)
	if s.hooks[name] == nil {
		name = "unknown" // any path is accepted, so do not make it a label value
	}
	s.requests.WithLabelValues(name, strconv.Itoa(code)).Inc()
}

// serveHook handles one webhook and returns the HTTP status it sent.
func (s *Server) serveHook(w http.ResponseWriter, r *http.Request, name string) int {
	h, pbs := s.hooks[name], s.playbooks[name]
	if h == nil || len(pbs) == 0 {
		http.NotFound(w, r)
		return http.StatusNotFound
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		http.Error(w, "payload too large or unreadable", http.StatusRequestEntityTooLarge)
		return http.StatusRequestEntityTooLarge
	}
	if !h.Insecure && !verify(h, r.Header, body) {
		s.log.Warn("webhook authentication failed", "hook", name, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return http.StatusUnauthorized
	}

	status := http.StatusAccepted
	results := make([]JobResult, 0, len(pbs))
	for _, pb := range pbs {
		job, created, err := s.dispatch.Dispatch(r.Context(), pb, name, body)
		res := JobResult{Playbook: pb, Job: job, Duplicate: err == nil && !created}
		result := "created"
		switch {
		case errors.Is(err, dispatch.ErrAtCapacity):
			// 429 makes the sender retry; the retry gets the same Job name.
			status = http.StatusTooManyRequests
			res.Error = err.Error()
			result = "at_capacity"
		case err != nil:
			if status != http.StatusTooManyRequests {
				status = http.StatusInternalServerError
			}
			res.Error = "dispatch failed"
			result = "error"
			s.log.Error("dispatch failed", "hook", name, "playbook", pb, "err", err)
		default:
			if !created {
				result = "duplicate"
			}
			s.log.Info("investigation dispatched", "hook", name, "playbook", pb, "job", job, "duplicate", !created)
		}
		s.dispatches.WithLabelValues(name, pb, result).Inc()
		results = append(results, res)
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"jobs": results})
	return status
}

// verify checks the request against the hook's secret in constant time.
func verify(h *config.Hook, hdr http.Header, body []byte) bool {
	got := strings.TrimSpace(hdr.Get(h.Header))
	want := h.Secret.String() // re-read per request, so rotation needs no restart
	if got == "" || want == "" {
		return false
	}
	switch h.Auth {
	case config.HookAuthToken:
		got = strings.TrimPrefix(got, "Bearer ")
		return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
	default: // hmac-sha256
		got = strings.TrimPrefix(got, "sha256=")
		sig, err := hex.DecodeString(got)
		if err != nil {
			return false
		}
		mac := hmac.New(sha256.New, []byte(want))
		mac.Write(body)
		return hmac.Equal(sig, mac.Sum(nil))
	}
}
