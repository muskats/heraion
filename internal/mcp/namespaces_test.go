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
	got          *string // the arguments of the last call, if set
}

func (f fakeTool) Name() string            { return f.name }
func (f fakeTool) Description() string     { return "" }
func (f fakeTool) Schema() json.RawMessage { return json.RawMessage(f.schema) }
func (f fakeTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	*f.calls++
	if f.got != nil {
		*f.got = string(args)
	}
	return "ok", nil
}

func TestLimitNamespaces(t *testing.T) {
	calls := 0
	list := fakeTool{"k8s.resources_list", `{"type":"object","properties":{"kind":{},"namespace":{}}}`, &calls, nil}
	top := fakeTool{"k8s.pods_top", `{"type":"object","properties":{"all_namespaces":{},"namespace":{}}}`, &calls, nil}
	other := fakeTool{"grafana.search", `{"type":"object","properties":{"query":{}}}`, &calls, nil}

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
		// Go's JSON decoding matches keys in any case, a server's map lookup
		// does not: both would see a different namespace.
		{tools[0], `{"namespace":"kube-system","NAMESPACE":"shop"}`, false},
		{tools[0], `{"Namespace":"shop"}`, false},
		{tools[1], `{"namespace":"shop","All_Namespaces":true}`, false},
		{tools[0], `{"namespace":["shop"]}`, false},
		{tools[1], `{"namespace":"shop","all_namespaces":"true"}`, false},
		{tools[1], `{"namespace":"shop","all_namespaces":false}`, true},
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

// The server gets the checked arguments, e.g. one "namespace" key where the
// model sent two.
func TestLimitNamespacesPassesCheckedArgs(t *testing.T) {
	calls, got := 0, ""
	list := fakeTool{"k8s.resources_list", `{"type":"object","properties":{"kind":{},"namespace":{}}}`, &calls, &got}
	tool := mcp.LimitNamespaces([]llm.Tool{list}, []string{"shop"})[0]
	if _, err := tool.Call(context.Background(), json.RawMessage(`{"namespace":"kube-system","kind":"Pod","namespace":"shop"}`)); err != nil {
		t.Fatal(err)
	}
	if want := `{"kind":"Pod","namespace":"shop"}`; got != want {
		t.Errorf("server got %s, want %s", got, want)
	}
}
