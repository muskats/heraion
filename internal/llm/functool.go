package llm

import (
	"context"
	"encoding/json"
	"fmt"
)

// DecodeArgs unmarshals a tool's JSON arguments into v, e.g.
//
//	var a struct{ Repo, Path string }
//	if err := llm.DecodeArgs(raw, &a); err != nil { ... }
func DecodeArgs(raw json.RawMessage, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// FuncTool is a Tool backed by a function, used by the built-in tools, e.g.
//
//	&llm.FuncTool{ToolName: "repo_list", Desc: "...", Params: `{"type":"object"}`, Fn: list}
type FuncTool struct {
	ToolName, Desc string
	Params         string // JSON Schema of the arguments object
	Fn             func(context.Context, json.RawMessage) (string, error)
}

func (t *FuncTool) Name() string            { return t.ToolName }
func (t *FuncTool) Description() string     { return t.Desc }
func (t *FuncTool) Schema() json.RawMessage { return json.RawMessage(t.Params) }
func (t *FuncTool) Call(ctx context.Context, a json.RawMessage) (string, error) {
	return t.Fn(ctx, a)
}
