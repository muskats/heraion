package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"

	"github.com/muskats/heraion/internal/secret"

	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/llm/llmtest"
)

type echo struct{ name string }

func (e echo) Name() string            { return e.name }
func (e echo) Description() string     { return "echo" }
func (e echo) Schema() json.RawMessage { return nil }
func (e echo) Call(_ context.Context, a json.RawMessage) (string, error) {
	if strings.Contains(string(a), "fail") {
		return "", errors.New("boom")
	}
	return "echo:" + string(a), nil
}

func newAgent(t *testing.T, srv *llmtest.Server, tools ...llm.Tool) *llm.Agent {
	t.Helper()
	reg, err := llm.NewRegistry(tools...)
	if err != nil {
		t.Fatal(err)
	}
	c := llm.NewClient(srv.BaseURL(), secret.Static("sk-test"))
	return &llm.Agent{LLM: c, Model: "m", Tools: reg, Limits: llm.Limits{MaxSteps: 5}}
}

func TestToolLoop(t *testing.T) {
	srv := llmtest.New(t,
		llmtest.Turn{Calls: []llmtest.Call{
			{Name: "k8s__get", Args: map[string]string{"q": "secret-value-xyz"}},
			{Name: "nope", Args: map[string]string{}},
			{Name: "k8s__get", Args: map[string]string{"q": "fail"}},
		}},
		llmtest.Turn{Final: "all done"},
	)
	a := newAgent(t, srv, echo{"k8s.get"})
	a.Redact = func(s string) string { return strings.ReplaceAll(s, "secret-value-xyz", "[REDACTED]") }

	res, err := a.Run(context.Background(), "sys", "user")
	if err != nil {
		t.Fatal(err)
	}
	if res.Final != "all done" || res.Steps != 2 || res.Usage.TotalTokens != 220 {
		t.Fatalf("%+v", res)
	}
	if srv.Auth[0] != "Bearer sk-test" {
		t.Errorf("auth header %q", srv.Auth[0])
	}
	// "k8s.get" is sent as "k8s__get".
	if got := srv.Requests[0].Tools[0].Function.Name; got != "k8s__get" {
		t.Errorf("wire name %q", got)
	}
	// Request 2: system, user, assistant, then three tool results.
	msgs := srv.Requests[1].Messages
	if len(msgs) != 6 {
		t.Fatalf("got %d messages", len(msgs))
	}
	results := []string{msgs[3].Text(), msgs[4].Text(), msgs[5].Text()}
	if strings.Contains(results[0], "secret-value-xyz") || !strings.Contains(results[0], "[REDACTED]") {
		t.Errorf("tool result not redacted: %q", results[0])
	}
	if !strings.Contains(results[1], "not available") || !strings.Contains(results[2], "error: boom") {
		t.Errorf("errors not reported to model: %q", results[1:])
	}
	if msgs[3].ToolCallID == "" || msgs[3].Role != "tool" {
		t.Errorf("tool message malformed: %+v", msgs[3])
	}
}

func TestStepLimit(t *testing.T) {
	loop := llmtest.Turn{Calls: []llmtest.Call{{Name: "t", Args: map[string]int{}}}}
	srv := llmtest.New(t, loop, loop, loop, loop, loop, loop)
	a := newAgent(t, srv, echo{"t"})
	a.Limits.MaxSteps = 3
	_, err := a.Run(context.Background(), "s", "u")
	if !errors.Is(err, llm.ErrLimit) || len(srv.Requests) != 3 {
		t.Fatalf("err=%v requests=%d", err, len(srv.Requests))
	}
}

func TestTokenLimit(t *testing.T) {
	loop := llmtest.Turn{Calls: []llmtest.Call{{Name: "t", Args: map[string]int{}}}}
	srv := llmtest.New(t, loop, loop, loop)
	a := newAgent(t, srv, echo{"t"})
	a.Limits.MaxTokens = 200 // each turn reports 110
	if _, err := a.Run(context.Background(), "s", "u"); !errors.Is(err, llm.ErrLimit) || len(srv.Requests) != 2 {
		t.Fatalf("err=%v requests=%d", err, len(srv.Requests))
	}
}

func TestRetryOn429ButNot400(t *testing.T) {
	srv := llmtest.New(t, llmtest.Turn{Status: 429}, llmtest.Turn{Status: 503}, llmtest.Turn{Final: "ok"})
	res, err := newAgent(t, srv).Run(context.Background(), "s", "u")
	if err != nil || res.Final != "ok" {
		t.Fatalf("%v %v", res, err)
	}

	srv = llmtest.New(t, llmtest.Turn{Status: 400}, llmtest.Turn{Final: "never"})
	_, err = newAgent(t, srv).Run(context.Background(), "s", "u")
	var apiErr *openai.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || len(srv.Requests) != 1 {
		t.Fatalf("400 must not be retried: %v (%d requests)", err, len(srv.Requests))
	}
}

func TestRegistryCollision(t *testing.T) {
	if _, err := llm.NewRegistry(echo{"a.b"}, echo{"a__b"}); err == nil {
		t.Fatal("expected collision error")
	}
}

type counter struct {
	name  string
	calls *int
	out   string
}

func (c counter) Name() string            { return c.name }
func (c counter) Description() string     { return "counter" }
func (c counter) Schema() json.RawMessage { return nil }
func (c counter) Call(context.Context, json.RawMessage) (string, error) {
	*c.calls++
	return c.out, nil
}

func TestTokenLimitStopsBeforeTools(t *testing.T) {
	loop := llmtest.Turn{Calls: []llmtest.Call{{Name: "t", Args: map[string]int{}}}}
	srv := llmtest.New(t, loop, loop, loop)
	n := 0
	a := newAgent(t, srv, counter{name: "t", calls: &n})
	a.Limits.MaxTokens = 200 // each turn reports 110: over budget after turn 2
	if _, err := a.Run(context.Background(), "s", "u"); !errors.Is(err, llm.ErrLimit) {
		t.Fatalf("err=%v", err)
	}
	if n != 1 {
		t.Fatalf("tool ran %d times; the over-budget turn's calls must not run", n)
	}
}

func TestSecretAtTruncationBoundaryIsMasked(t *testing.T) {
	const secret = "super-secret-token-value"
	srv := llmtest.New(t,
		llmtest.Turn{Calls: []llmtest.Call{{Name: "t", Args: map[string]int{}}}},
		llmtest.Turn{Final: "done"},
	)
	n := 0
	// The secret starts 10 bytes before the cut.
	out := strings.Repeat("x", 90) + secret + strings.Repeat("y", 50)
	a := newAgent(t, srv, counter{name: "t", calls: &n, out: out})
	a.Limits.MaxToolResultBytes = 100
	a.Redact = func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") }
	if _, err := a.Run(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	got := srv.Requests[1].Messages[3].Text()
	if strings.Contains(got, secret[:10]) {
		t.Fatalf("partial secret leaked: %q", got)
	}
}

func TestCutOffAnswerIsAnError(t *testing.T) {
	srv := llmtest.New(t, llmtest.Turn{Final: "the root cause is", FinishReason: "length"})
	res, err := newAgent(t, srv).Run(context.Background(), "s", "u")
	if !errors.Is(err, llm.ErrLimit) || res.Final != "the root cause is" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

type emptyChat struct{}

func (emptyChat) Chat(context.Context, openai.ChatCompletionNewParams) (*openai.ChatCompletion, error) {
	return &openai.ChatCompletion{}, nil
}

func TestNoChoicesIsAnError(t *testing.T) {
	a := &llm.Agent{LLM: emptyChat{}, Model: "m", Limits: llm.Limits{MaxSteps: 3}}
	if _, err := a.Run(context.Background(), "s", "u"); err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Fatalf("got %v", err)
	}
}

func TestMissingAPIKeyIsNotRetried(t *testing.T) {
	srv := llmtest.New(t)
	c := llm.NewClient(srv.BaseURL(), secret.FromMap("KEY", map[string]string{"KEY_FILE": "/nonexistent"}))
	if _, err := c.Chat(context.Background(), openai.ChatCompletionNewParams{Model: "m"}); err == nil || !strings.Contains(err.Error(), "llm api key") {
		t.Fatalf("got %v", err)
	}
	if len(srv.Requests) != 0 {
		t.Errorf("no request may be sent without a key, got %d", len(srv.Requests))
	}
}
