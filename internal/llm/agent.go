package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"

	"github.com/muskats/heraion/internal/textutil"
)

// Tool is a built-in Git tool or an MCP tool.
type Tool interface {
	// Name is "repo_list" for built-ins and "<server>.<tool>" for MCP tools,
	// e.g. "k8s.pods_log". Playbook allowlists match this name.
	Name() string
	Description() string
	// Schema is the JSON Schema of the arguments object.
	Schema() json.RawMessage
	Call(ctx context.Context, args json.RawMessage) (string, error)
}

// wireName makes a name the OpenAI API accepts, e.g. "k8s.pods_log" becomes
// "k8s__pods_log".
var invalidWire = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func wireName(name string) string {
	w := invalidWire.ReplaceAllString(strings.ReplaceAll(name, ".", "__"), "_")
	if len(w) > 64 {
		w = w[:64]
	}
	return w
}

// Registry is a set of tools keyed by wire name.
type Registry struct {
	byWire map[string]Tool
}

// NewRegistry fails if two names map to the same wire name, e.g. "a.b" and "a__b".
func NewRegistry(tools ...Tool) (*Registry, error) {
	r := &Registry{byWire: map[string]Tool{}}
	for _, t := range tools {
		w := wireName(t.Name())
		if prev, ok := r.byWire[w]; ok {
			return nil, fmt.Errorf("tools %q and %q collide on wire name %q", prev.Name(), t.Name(), w)
		}
		r.byWire[w] = t
	}
	return r, nil
}

// Filter returns a registry holding only tools for which allow is true.
func (r *Registry) Filter(allow func(name string) bool) *Registry {
	out := &Registry{byWire: map[string]Tool{}}
	for w, t := range r.byWire {
		if allow(t.Name()) {
			out.byWire[w] = t
		}
	}
	return out
}

// Names returns the canonical names, sorted.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.byWire))
	for _, t := range r.byWire {
		names = append(names, t.Name())
	}
	sort.Strings(names)
	return names
}

func (r *Registry) specs() ([]openai.ChatCompletionToolUnionParam, error) {
	wires := make([]string, 0, len(r.byWire))
	for w := range r.byWire {
		wires = append(wires, w)
	}
	sort.Strings(wires) // stable order helps prompt caching
	specs := make([]openai.ChatCompletionToolUnionParam, 0, len(wires))
	for _, w := range wires {
		t := r.byWire[w]
		params := shared.FunctionParameters{"type": "object", "properties": map[string]any{}}
		if s := t.Schema(); len(s) > 0 {
			if err := json.Unmarshal(s, &params); err != nil {
				return nil, fmt.Errorf("tool %s: schema: %w", t.Name(), err)
			}
		}
		fn := shared.FunctionDefinitionParam{Name: w, Parameters: params}
		if d := t.Description(); d != "" {
			fn.Description = openai.String(d)
		}
		specs = append(specs, openai.ChatCompletionFunctionTool(fn))
	}
	return specs, nil
}

// Limits bound a single agent run.
type Limits struct {
	MaxSteps    int // model turns
	MaxTokens   int // total tokens across turns; 0 = unlimited
	Temperature *float64
	// MaxToolResultBytes truncates tool output (default 64 KiB).
	MaxToolResultBytes int
}

// Agent runs the tool-calling loop.
type Agent struct {
	LLM    Chatter
	Model  string
	Tools  *Registry
	Limits Limits
	// Redact masks secrets in the prompt and tool results. Nil masks nothing.
	Redact func(string) string
	Log    *slog.Logger
}

// Result is the outcome of a run.
type Result struct {
	Final string // the model's final answer
	Steps int
	Usage Usage
}

// ErrLimit is returned (wrapped) when a run stops on a step or token limit.
var ErrLimit = errors.New("agent limit reached")

// Run loops until the model answers without calling a tool, a limit is hit,
// or ctx is done.
func (a *Agent) Run(ctx context.Context, system, user string) (*Result, error) {
	redact := a.Redact
	if redact == nil {
		redact = func(s string) string { return s }
	}
	log := a.Log
	if log == nil {
		log = slog.Default()
	}
	maxResult := a.Limits.MaxToolResultBytes
	if maxResult == 0 {
		maxResult = 64 << 10
	}
	tools := a.Tools
	if tools == nil {
		tools = &Registry{byWire: map[string]Tool{}}
	}
	specs, err := tools.specs()
	if err != nil {
		return nil, err
	}
	msgs := []openai.ChatCompletionMessageParamUnion{
		openai.SystemMessage(redact(system)),
		openai.UserMessage(redact(user)),
	}
	res := &Result{}
	for res.Steps < a.Limits.MaxSteps {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Steps++
		req := openai.ChatCompletionNewParams{Model: a.Model, Messages: msgs, Tools: specs}
		if a.Limits.Temperature != nil {
			req.Temperature = openai.Float(*a.Limits.Temperature)
		}
		resp, err := a.LLM.Chat(ctx, req)
		if err != nil {
			return res, fmt.Errorf("step %d: %w", res.Steps, err)
		}
		if len(resp.Choices) == 0 {
			return res, fmt.Errorf("step %d: llm response has no choices", res.Steps)
		}
		res.Usage.PromptTokens += int(resp.Usage.PromptTokens)
		res.Usage.CompletionTokens += int(resp.Usage.CompletionTokens)
		res.Usage.TotalTokens += int(resp.Usage.TotalTokens)

		choice := resp.Choices[0]
		msg := choice.Message
		msgs = append(msgs, msg.ToParam())

		if len(msg.ToolCalls) == 0 {
			res.Final = msg.Content
			if choice.FinishReason == "length" {
				return res, fmt.Errorf("%w: the model's answer was cut off at its output limit", ErrLimit)
			}
			log.Info("agent finished", "steps", res.Steps, "tokens", res.Usage.TotalTokens)
			return res, nil
		}
		// Check the budget before running tools: a call such as open_pr must
		// not happen once the run is over budget.
		if a.Limits.MaxTokens > 0 && res.Usage.TotalTokens >= a.Limits.MaxTokens {
			return res, fmt.Errorf("%w: used %d of %d tokens", ErrLimit, res.Usage.TotalTokens, a.Limits.MaxTokens)
		}
		for _, tc := range msg.ToolCalls {
			// Redact before truncating, so a secret cut in half at the
			// limit cannot slip through as an unrecognisable prefix.
			out := redact(a.callTool(ctx, log, tools, tc))
			if len(out) > maxResult {
				out = textutil.Truncate(out, maxResult) + fmt.Sprintf("\n...[truncated, %d bytes total]", len(out))
			}
			msgs = append(msgs, openai.ToolMessage(out, tc.ID))
		}
	}
	return res, fmt.Errorf("%w: %d steps without a final answer", ErrLimit, a.Limits.MaxSteps)
}

func (a *Agent) callTool(ctx context.Context, log *slog.Logger, tools *Registry, tc openai.ChatCompletionMessageToolCallUnion) string {
	t, ok := tools.byWire[tc.Function.Name]
	if !ok {
		log.Warn("model called unknown or disallowed tool", "tool", tc.Function.Name)
		return fmt.Sprintf("error: tool %q is not available", tc.Function.Name)
	}
	args := json.RawMessage(tc.Function.Arguments)
	if len(strings.TrimSpace(tc.Function.Arguments)) == 0 {
		args = json.RawMessage(`{}`)
	}
	if !json.Valid(args) {
		return "error: arguments are not valid JSON"
	}
	log.Info("tool call", "tool", t.Name())
	out, err := t.Call(ctx, args)
	if err != nil {
		log.Warn("tool error", "tool", t.Name(), "err", err)
		return "error: " + err.Error()
	}
	return out
}
