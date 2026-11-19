package llm

import (
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 19, 7, 28, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":                              0,
		"30":                            30 * time.Second,
		"Thu, 19 Nov 2026 07:28:20 GMT": 20 * time.Second,
		"soon":                          0,
	} {
		if got := retryAfter(v, now); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}
