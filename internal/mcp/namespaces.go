package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

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

func (t *nsTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Namespace     string `json:"namespace"`
		AllNamespaces bool   `json:"all_namespaces"`
	}
	if err := llm.DecodeArgs(args, &a); err != nil {
		return "", err
	}
	switch {
	case a.AllNamespaces:
		return "", fmt.Errorf("all_namespaces is not allowed; call once per namespace in %v", t.allowed)
	case a.Namespace == "":
		return "", fmt.Errorf("namespace is required; allowed: %v", t.allowed)
	case !slices.Contains(t.allowed, a.Namespace):
		return "", fmt.Errorf("namespace %q is not allowed; allowed: %v", a.Namespace, t.allowed)
	}
	return t.Tool.Call(ctx, args)
}
