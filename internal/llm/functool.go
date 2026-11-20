package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
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

// SchemaFor returns the JSON Schema of the arguments struct T, with enums
// set on some properties, e.g.
//
//	type args struct {
//		Repo string `json:"repo" jsonschema:"Repository as owner/name"`
//		Path string `json:"path,omitempty"` // omitempty: optional
//	}
//	llm.SchemaFor[args](map[string][]string{"repo": {"acme/web"}})
func SchemaFor[T any](enums map[string][]string) string {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(fmt.Sprintf("schema for %T: %v", *new(T), err)) // a bug in the struct tags
	}
	for k, vals := range enums {
		p := s.Properties[k]
		if p == nil {
			panic(fmt.Sprintf("schema for %T: no property %q", *new(T), k))
		}
		p.Enum = make([]any, len(vals)) // empty, never null
		for i, v := range vals {
			p.Enum[i] = v
		}
	}
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
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
