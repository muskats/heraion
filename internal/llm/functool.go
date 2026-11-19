package llm

import (
	"context"
	"encoding/json"
)

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
