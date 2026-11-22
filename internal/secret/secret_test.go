package secret

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOrder(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "token")
	_ = os.WriteFile(tok, []byte("from-file\n"), 0o600)

	cases := []struct {
		env        map[string]string
		want, from string
	}{
		{map[string]string{"X_FILE": tok, "X": "from-env", "X_FALLBACK": "lit"}, "from-file", OriginFile},
		{map[string]string{"X": "from-env", "X_FALLBACK": "lit"}, "from-env", OriginEnv},
		{map[string]string{"X_FALLBACK": "lit"}, "lit", OriginFallback},
		// An optional secretKeyRef whose key is missing leaves X unset.
		{map[string]string{"X": "", "X_FALLBACK": "lit"}, "lit", OriginFallback},
	}
	for _, c := range cases {
		s := FromMap("X", c.env)
		v, err := s.Value()
		if err != nil || v != c.want || s.Origin() != c.from {
			t.Errorf("%v: got %q/%s/%v want %q/%s", c.env, v, s.Origin(), err, c.want, c.from)
		}
	}
}

func TestBrokenFileDoesNotFallBack(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	_ = os.WriteFile(empty, []byte(" \n"), 0o600)
	missing := filepath.Join(dir, "nope")
	cases := []struct {
		env  map[string]string
		want []string
	}{
		{map[string]string{"X_FILE": missing, "X_FALLBACK": "lit"}, []string{"X_FILE", missing, "not falling back to X_FALLBACK"}},
		{map[string]string{"X_FILE": empty, "X": "from-env"}, []string{"X_FILE", "is empty", "not falling back to X"}},
		{map[string]string{"X_FILE": missing}, []string{"X_FILE", missing}},
	}
	for _, c := range cases {
		s := FromMap("X", c.env)
		v, err := s.Value()
		if err == nil || v != "" || s.Origin() != OriginNone {
			t.Errorf("%v: got %q/%s/%v, want an error", c.env, v, s.Origin(), err)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%v: error %q lacks %q", c.env, err, w)
			}
		}
	}
}

func TestMissing(t *testing.T) {
	if _, err := FromMap("X", nil).Value(); !errors.Is(err, ErrMissing) {
		t.Fatalf("want ErrMissing, got %v", err)
	}
	if _, err := FromMap("X", map[string]string{"X_FILE": "/nonexistent"}).Value(); err == nil {
		t.Fatal("unreadable file with no fallback must fail")
	}
	var nilSrc *Source
	if nilSrc.Set() || nilSrc.String() != "" {
		t.Fatal("nil source must be empty")
	}
}

func TestFileRotation(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("v1"), 0o600)
	s := FromMap("X", map[string]string{"X_FILE": tok})
	if v, _ := s.Value(); v != "v1" {
		t.Fatal(v)
	}
	_ = os.WriteFile(tok, []byte("v2"), 0o600)
	s.expires = s.expires.Add(-2 * fileTTL) // simulate the TTL passing
	if v, _ := s.Value(); v != "v2" {
		t.Fatalf("rotated token not picked up: %s", v)
	}
}
