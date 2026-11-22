// Package secret resolves a credential from up to three env vars, in order:
//
//	HERAION_GIT_TOKEN_FILE      a file, e.g. a projected ServiceAccount token or a
//	                            Secrets Store CSI mount; re-read so rotation works
//	HERAION_GIT_TOKEN           e.g. from an existing Secret (optional key)
//	HERAION_GIT_TOKEN_FALLBACK  the literal value from the chart's Secret
//
// A set _FILE whose file is missing or empty is an error: the lower levels are
// not tried, so a broken CSI mount never runs on the literal fallback.
package secret

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// Origins reported by Source.Origin.
const (
	OriginFile     = "file"
	OriginEnv      = "env"
	OriginFallback = "fallback"
	OriginNone     = "none"
)

// Suffixes of the file and fallback env vars.
const (
	SuffixFile     = "_FILE"
	SuffixFallback = "_FALLBACK"
)

// fileTTL is how long a file value is cached. Projected tokens are
// refreshed by the kubelet well before they expire, so 30s is plenty.
const fileTTL = 30 * time.Second

// ErrMissing means none of the env vars yields a value.
var ErrMissing = errors.New("secret not set")

// Source is one credential. The zero value and nil are valid and empty.
type Source struct {
	Name   string // base env var, e.g. HERAION_GIT_TOKEN
	getenv func(string) string

	mu      sync.Mutex
	cached  string
	expires time.Time
}

// New returns a Source that looks env vars up with getenv.
func New(name string, getenv func(string) string) *Source {
	return &Source{Name: name, getenv: getenv}
}

// Static returns a Source with a fixed value (tests, CLI flags).
func Static(v string) *Source {
	return &Source{Name: "static", getenv: func(n string) string {
		if n == "static" {
			return v
		}
		return ""
	}}
}

// FromMap returns a Source that reads env vars from m (tests).
func FromMap(name string, m map[string]string) *Source {
	return New(name, func(k string) string { return m[k] })
}

// Value returns the value, an error naming a set but unreadable or empty
// _FILE, or ErrMissing.
func (s *Source) Value() (string, error) {
	v, _, err := s.resolve()
	return v, err
}

// String returns the value or "" (for redaction lists).
func (s *Source) String() string {
	v, _ := s.Value()
	return v
}

// Origin says where the value comes from, e.g. "file", without exposing it.
func (s *Source) Origin() string {
	_, o, err := s.resolve()
	if err != nil {
		return OriginNone
	}
	return o
}

// Set reports whether any of the env vars has a value. It does not read the
// file, so Set can be true while Value fails, e.g. for a missing file.
func (s *Source) Set() bool {
	if s == nil || s.getenv == nil {
		return false
	}
	return s.getenv(s.Name+SuffixFile) != "" || s.getenv(s.Name) != "" || s.getenv(s.Name+SuffixFallback) != ""
}

func (s *Source) resolve() (string, string, error) {
	if s == nil || s.getenv == nil {
		return "", OriginNone, ErrMissing
	}
	if path := s.getenv(s.Name + SuffixFile); path != "" {
		v, err := s.readFile(path)
		switch {
		case err != nil:
			return "", OriginNone, fmt.Errorf("%s: %w%s", s.Name+SuffixFile, err, s.notTried())
		case v == "":
			return "", OriginNone, fmt.Errorf("%s: file %s is empty%s", s.Name+SuffixFile, path, s.notTried())
		}
		return v, OriginFile, nil
	}
	if v, o := s.lower(); v != "" {
		return v, o, nil
	}
	return "", OriginNone, fmt.Errorf("%s: %w (also tried %s%s and %s%s)", s.Name, ErrMissing, s.Name, SuffixFile, s.Name, SuffixFallback)
}

// notTried names the lower levels a broken file did not fall back to, e.g.
// " (not falling back to HERAION_GIT_TOKEN_FALLBACK: fix the file or unset HERAION_GIT_TOKEN_FILE)".
func (s *Source) notTried() string {
	var set []string
	for _, n := range []string{s.Name, s.Name + SuffixFallback} {
		if strings.TrimSpace(s.getenv(n)) != "" {
			set = append(set, n)
		}
	}
	if len(set) == 0 {
		return ""
	}
	return fmt.Sprintf(" (not falling back to %s: fix the file or unset %s)", strings.Join(set, " or "), s.Name+SuffixFile)
}

// lower reads <ENV>, then <ENV>_FALLBACK. An empty <ENV> means "not
// configured", e.g. an optional secretKeyRef whose key is missing, so the
// fallback is used.
func (s *Source) lower() (string, string) {
	if v := strings.TrimSpace(s.getenv(s.Name)); v != "" {
		return v, OriginEnv
	}
	if v := strings.TrimSpace(s.getenv(s.Name + SuffixFallback)); v != "" {
		return v, OriginFallback
	}
	return "", OriginNone
}

func (s *Source) readFile(path string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != "" && time.Now().Before(s.expires) {
		return s.cached, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s.cached, s.expires = strings.TrimSpace(string(b)), time.Now().Add(fileTTL)
	return s.cached, nil
}
