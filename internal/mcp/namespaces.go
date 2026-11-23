package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/muskats/heraion/internal/llm"
)

// LimitNamespaces wraps the tools that take a "namespace" or
// "all_namespaces" argument, e.g. k8s.resources_list, so they only reach the
// allowed namespaces:
//
//	allowed [shop]   {"namespace": "shop"} ok, {"namespace": "kube-system"},
//	                 {} and {"all_namespaces": true} refused
//
// Empty allowed: every namespace, tools are returned unchanged. Tools without
// such an argument (e.g. pods_list) are left to the server's RBAC.
func LimitNamespaces(tools []llm.Tool, allowed []string) []llm.Tool {
	if len(allowed) == 0 {
		return tools
	}
	out := make([]llm.Tool, 0, len(tools))
	for _, t := range tools {
		if takesNamespace(t.Schema()) {
			t = &nsTool{Tool: t, allowed: allowed}
		}
		out = append(out, t)
	}
	return out
}

func takesNamespace(schema json.RawMessage) bool {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return false
	}
	_, ns := s.Properties["namespace"]
	_, all := s.Properties["all_namespaces"]
	return ns || all
}

type nsTool struct {
	llm.Tool
	allowed []string
}

// Call checks the exact keys "namespace" and "all_namespaces", refuses keys
// that differ from them only in case (Go's JSON decoding matches those, a
// map lookup does not), and passes the checked arguments on re-encoded, so
// the server sees what was checked, e.g. no second "namespace" key.
func (t *nsTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var m map[string]json.RawMessage
	if err := llm.DecodeArgs(args, &m); err != nil {
		return "", err
	}
	for k := range m {
		for _, want := range []string{"namespace", "all_namespaces"} {
			if k != want && strings.EqualFold(k, want) {
				return "", fmt.Errorf("argument %q is not allowed; use %q", k, want)
			}
		}
	}
	var ns string
	var all bool
	if raw, ok := m["namespace"]; ok && json.Unmarshal(raw, &ns) != nil {
		return "", errors.New("namespace must be a string")
	}
	if raw, ok := m["all_namespaces"]; ok && json.Unmarshal(raw, &all) != nil {
		return "", errors.New("all_namespaces must be a boolean")
	}
	switch {
	case all:
		return "", fmt.Errorf("all_namespaces is not allowed; call once per namespace in %v", t.allowed)
	case ns == "":
		return "", fmt.Errorf("namespace is required; allowed: %v", t.allowed)
	case !slices.Contains(t.allowed, ns):
		return "", fmt.Errorf("namespace %q is not allowed; allowed: %v", ns, t.allowed)
	}
	checked, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return t.Tool.Call(ctx, checked)
}
