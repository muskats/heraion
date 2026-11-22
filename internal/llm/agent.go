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
	"time"

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
	// MaxToolResultBytes truncates tool output (default 16 KiB).
	MaxToolResultBytes int
	// MaxContextBytes caps the history sent per request (default 200000).
	// Above it the oldest tool results, except the last keepToolResults,
	// become a one-line stub.
	MaxContextBytes int
}

// Defaults of Limits.
const (
	defaultToolResultBytes = 16 << 10
	defaultContextBytes    = 200_000
	// keepToolResults is how many of the newest tool results are never trimmed.
	keepToolResults = 4
)

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
		maxResult = defaultToolResultBytes
	}
	hist := &history{limit: a.Limits.MaxContextBytes, log: log}
	if hist.limit == 0 {
		hist.limit = defaultContextBytes
	}
	tools := a.Tools
	if tools == nil {
		tools = &Registry{byWire: map[string]Tool{}}
	}
	specs, err := tools.specs()
	if err != nil {
		return nil, err
	}
	system, user = redact(system), redact(user)
	hist.add(openai.SystemMessage(system), len(system))
	hist.add(openai.UserMessage(user), len(user))
	res := &Result{}
	for res.Steps < a.Limits.MaxSteps {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Steps++
		hist.trim()
		req := openai.ChatCompletionNewParams{Model: a.Model, Messages: hist.msgs, Tools: specs}
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
		hist.add(msg.ToParam(), assistantBytes(msg))

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
			hist.addTool(tc.Function.Name, tc.ID, out)
		}
	}
	return res, fmt.Errorf("%w: %d steps without a final answer", ErrLimit, a.Limits.MaxSteps)
}

// callTool runs one call and logs it as one "tool call" line, e.g.
//
//	tool=k8s.pods_log args={"name":"web-1","namespace":"shop"} resultBytes=5120 durationMs=84 error=false
func (a *Agent) callTool(ctx context.Context, log *slog.Logger, tools *Registry, tc openai.ChatCompletionMessageToolCallUnion) string {
	start := time.Now()
	name := tc.Function.Name
	args := json.RawMessage(tc.Function.Arguments)
	if len(strings.TrimSpace(tc.Function.Arguments)) == 0 {
		args = json.RawMessage(`{}`)
	}
	var out string
	var err error
	t, ok := tools.byWire[name]
	switch {
	case !ok:
		err = fmt.Errorf("tool %q is not available", name)
	case !json.Valid(args):
		name, err = t.Name(), errors.New("arguments are not valid JSON")
	default:
		name = t.Name()
		out, err = t.Call(ctx, args)
	}
	if err != nil {
		out = "error: " + err.Error()
	}
	attrs := []any{"tool", name, "args", logArgs(name, args, a.Redact), "resultBytes", len(out),
		"durationMs", time.Since(start).Milliseconds(), "error", err != nil}
	if err != nil {
		attrs = append(attrs, "err", err.Error())
	}
	log.Info("tool call", attrs...)
	return out
}

// Limits of logArgs.
const (
	maxLoggedString = 256
	maxLoggedArgs   = 1 << 10
)

// logArgs returns the arguments for the log: strings longer than 256 bytes
// and the content of repo_write_file become their length, e.g.
// {"content":"<5120 bytes>","path":"Dockerfile"}, then secrets are masked
// and the result is cut at 1 KiB.
func logArgs(tool string, raw json.RawMessage, redact func(string) string) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Sprintf("<%d bytes, not JSON>", len(raw))
	}
	if m, ok := v.(map[string]any); ok && tool == "repo_write_file" {
		if c, ok := m["content"].(string); ok {
			m["content"] = fmt.Sprintf("<%d bytes>", len(c))
		}
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep "<5120 bytes>" readable
	if err := enc.Encode(shorten(v)); err != nil {
		return fmt.Sprintf("<%d bytes>", len(raw))
	}
	s := strings.TrimSuffix(b.String(), "\n")
	if redact != nil {
		s = redact(s)
	}
	if len(s) > maxLoggedArgs {
		s = textutil.Truncate(s, maxLoggedArgs) + "...[truncated]"
	}
	return s
}

// shorten replaces long strings anywhere in v with "<N bytes>".
func shorten(v any) any {
	switch x := v.(type) {
	case string:
		if len(x) > maxLoggedString {
			return fmt.Sprintf("<%d bytes>", len(x))
		}
	case map[string]any:
		for k, e := range x {
			x[k] = shorten(e)
		}
	case []any:
		for i, e := range x {
			x[i] = shorten(e)
		}
	}
	return v
}

// history is the message list with the size of each message, so the oldest
// tool results can be replaced by a stub when it grows over limit.
type history struct {
	msgs    []openai.ChatCompletionMessageParamUnion
	sizes   []int
	tools   []toolMsg // tool results, oldest first
	total   int
	limit   int
	trimmed bool // "context trimmed" was logged
	log     *slog.Logger
}

type toolMsg struct {
	index    int // in msgs
	tool, id string
	elided   bool
}

func (h *history) add(m openai.ChatCompletionMessageParamUnion, size int) {
	h.msgs = append(h.msgs, m)
	h.sizes = append(h.sizes, size)
	h.total += size
}

func (h *history) addTool(tool, id, out string) {
	h.tools = append(h.tools, toolMsg{index: len(h.msgs), tool: tool, id: id})
	h.add(openai.ToolMessage(out, id), len(out))
}

// trim replaces the oldest tool results with a stub until the history fits,
// e.g. "[earlier k8s__pods_log result of 16384 bytes removed ...]". The
// newest keepToolResults results, the prompts and the model's messages stay.
func (h *history) trim() {
	if h.total <= h.limit {
		return
	}
	n, freed := 0, 0
	for i := 0; i < len(h.tools)-keepToolResults && h.total > h.limit; i++ {
		t := &h.tools[i]
		if t.elided {
			continue
		}
		old := h.sizes[t.index]
		stub := fmt.Sprintf("[earlier %s result of %d bytes removed to fit the context; call the tool again if you need it]", t.tool, old)
		if len(stub) >= old {
			continue
		}
		h.msgs[t.index] = openai.ToolMessage(stub, t.id)
		h.sizes[t.index] = len(stub)
		h.total -= old - len(stub)
		t.elided = true
		n++
		freed += old - len(stub)
	}
	if n > 0 && !h.trimmed {
		h.trimmed = true
		h.log.Info("context trimmed", "results", n, "bytesRemoved", freed, "bytes", h.total, "limit", h.limit)
	}
}

// assistantBytes is the size of a model message: its text and tool call arguments.
func assistantBytes(m openai.ChatCompletionMessage) int {
	n := len(m.Content)
	for _, tc := range m.ToolCalls {
		n += len(tc.Function.Name) + len(tc.Function.Arguments)
	}
	return n
}
