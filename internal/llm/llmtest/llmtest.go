// Package llmtest provides a scripted mock OpenAI chat-completions server.
package llmtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/muskats/heraion/internal/llm"
)

// Turn is one scripted model response: either tool calls or final text.
type Turn struct {
	Calls  []Call
	Final  string
	Status int // non-zero: reply with this HTTP status instead
	// FinishReason defaults to "stop", e.g. "length" for a cut-off answer.
	FinishReason string
}

// Call is a scripted tool call; Args is marshalled to JSON.
type Call struct {
	Name string
	Args any
}

// Server is a mock endpoint that replays Turns in order and records requests.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	turns    []Turn
	Requests []llm.ChatRequest
	Auth     []string
}

// New starts a mock server. It is closed when the test ends.
func New(t testing.TB, turns ...Turn) *Server {
	s := &Server{turns: turns}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.Close)
	return s
}

// BaseURL is the value for llm.baseURL, e.g. http://127.0.0.1:41234/v1.
func (s *Server) BaseURL() string { return s.URL + "/v1" }

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var req llm.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.Requests = append(s.Requests, req)
	s.Auth = append(s.Auth, r.Header.Get("Authorization"))
	if len(s.turns) == 0 {
		s.mu.Unlock()
		http.Error(w, "script exhausted", http.StatusInternalServerError)
		return
	}
	turn := s.turns[0]
	s.turns = s.turns[1:]
	n := len(s.Requests)
	s.mu.Unlock()

	if turn.Status != 0 {
		w.Header().Set("Retry-After", "0")
		http.Error(w, `{"error":"scripted"}`, turn.Status)
		return
	}
	msg := map[string]any{"role": "assistant", "content": nil}
	if len(turn.Calls) == 0 {
		msg["content"] = turn.Final
	} else {
		var calls []map[string]any
		for i, c := range turn.Calls {
			args, _ := json.Marshal(c.Args)
			calls = append(calls, map[string]any{
				"id": fmt.Sprintf("call_%d_%d", n, i), "type": "function",
				"function": map[string]any{"name": c.Name, "arguments": string(args)},
			})
		}
		msg["tool_calls"] = calls
	}
	finish := turn.FinishReason
	if finish == "" {
		finish = "stop"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110},
	})
}
