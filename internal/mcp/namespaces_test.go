package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/muskats/heraion/internal/llm"
	"github.com/muskats/heraion/internal/mcp"
)

type fakeTool struct {
	name, schema string
	calls        *int
}

func (f fakeTool) Name() string            { return f.name }
func (f fakeTool) Description() string     { return "" }
func (f fakeTool) Schema() json.RawMessage { return json.RawMessage(f.schema) }
func (f fakeTool) Call(context.Context, json.RawMessage) (string, error) {
	*f.calls++
	return "ok", nil
}

func TestLimitNamespaces(t *testing.T) {
	calls := 0
	list := fakeTool{"k8s.resources_list", `{"type":"object","properties":{"kind":{},"namespace":{}}}`, &calls}
	top := fakeTool{"k8s.pods_top", `{"type":"object","properties":{"all_namespaces":{},"namespace":{}}}`, &calls}
	other := fakeTool{"grafana.search", `{"type":"object","properties":{"query":{}}}`, &calls}

	if got := mcp.LimitNamespaces([]llm.Tool{list}, nil); got[0] != llm.Tool(list) {
		t.Error("no namespaces: tools must be unchanged")
	}
	tools := mcp.LimitNamespaces([]llm.Tool{list, top, other}, []string{"shop"})
	for _, c := range []struct {
		tool llm.Tool
		args string
		ok   bool
	}{
		{tools[0], `{"kind":"Pod","namespace":"shop"}`, true},
		{tools[0], `{"kind":"Pod","namespace":"kube-system"}`, false},
		{tools[0], `{"kind":"Pod"}`, false},
		{tools[1], `{"namespace":"shop","all_namespaces":true}`, false},
		{tools[1], `{"namespace":"shop"}`, true},
		{tools[2], `{"query":"x"}`, true},
		{tools[0], `not json`, false},
	} {
		before := calls
		_, err := c.tool.Call(context.Background(), json.RawMessage(c.args))
		if (err == nil) != c.ok || (calls > before) != c.ok {
			t.Errorf("%s %s: err=%v, want ok=%v", c.tool.Name(), c.args, err, c.ok)
		}
	}
	if tools[0].Name() != "k8s.resources_list" || string(tools[0].Schema()) != list.schema {
		t.Error("the wrapper must keep name and schema")
	}
}
