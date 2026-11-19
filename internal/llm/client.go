// Package llm is a small OpenAI chat-completions client with tool calling,
// plus the agent loop. Any compatible endpoint works, e.g.
// https://api.openai.com/v1 or http://litellm:4000/v1.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"github.com/muskats/heraion/internal/secret"
)

// Message is a chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"` // pointer: assistant tool-call turns may have null content
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// Text returns the message content or "".
func (m Message) Text() string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall holds the name and JSON-encoded arguments of a call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolSpec describes a tool to the model.
type ToolSpec struct {
	Type     string       `json:"type"`
	Function FunctionSpec `json:"function"`
}

// FunctionSpec is the function part of a ToolSpec.
type FunctionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ChatRequest is a chat-completions request.
type ChatRequest struct {
	Model       string     `json:"model"`
	Messages    []Message  `json:"messages"`
	Tools       []ToolSpec `json:"tools,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
}

// Usage is token accounting reported by the server.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is a chat-completions response.
type ChatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

// Chatter is anything that can run a chat completion. *Client implements it.
type Chatter interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// Client is an HTTP client for an OpenAI-compatible endpoint.
type Client struct {
	BaseURL    string         // e.g. https://api.openai.com/v1
	APIKey     *secret.Source // nil or unset: no Authorization header (e.g. Ollama)
	HTTPClient *http.Client
	MaxRetries int
	RetryBase  time.Duration // first backoff delay, doubled per attempt
}

// NewClient returns a Client with sensible defaults.
func NewClient(baseURL string, apiKey *secret.Source) *Client {
	return &Client{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 5 * time.Minute},
		MaxRetries: 4,
		RetryBase:  time.Second,
	}
}

// APIError is a non-2xx response from the endpoint.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("llm endpoint returned %d: %s", e.StatusCode, e.Body)
}

func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code >= 500
}

// Chat sends one request. It retries 408, 429, 5xx and network errors with
// backoff and honours Retry-After.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if attempt > 0 {
			wait := c.backoff(attempt, lastErr)
			select {
			case <-ctx.Done():
				return nil, errors.Join(ctx.Err(), lastErr)
			case <-time.After(wait):
			}
		}
		resp, err := c.do(ctx, body)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		var apiErr *APIError
		if errors.As(err, &apiErr) && !retryable(apiErr.StatusCode) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("after %d retries: %w", c.MaxRetries, lastErr)
}

type retryAfterError struct {
	*APIError
	after time.Duration
}

func (e *retryAfterError) Unwrap() error { return e.APIError }

func (c *Client) backoff(attempt int, err error) time.Duration {
	var ra *retryAfterError
	if errors.As(err, &ra) && ra.after > 0 && ra.after <= time.Minute {
		return ra.after
	}
	base := time.Duration(1<<(attempt-1)) * c.RetryBase
	return base + rand.N(base/2+1)
}

func (c *Client) do(ctx context.Context, body []byte) (*ChatResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey.Set() {
		// Read per request: a projected ServiceAccount token rotates.
		key, err := c.APIKey.Value()
		if err != nil {
			return nil, &APIError{StatusCode: http.StatusUnauthorized, Body: "llm api key: " + err.Error()}
		}
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		snippet := string(raw)
		if len(snippet) > 2000 {
			snippet = snippet[:2000] + "..."
		}
		apiErr := &APIError{StatusCode: resp.StatusCode, Body: snippet}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			return nil, &retryAfterError{APIError: apiErr, after: time.Duration(s) * time.Second}
		}
		return nil, apiErr
	}
	out := &ChatResponse{}
	if err := json.Unmarshal(raw, out); err != nil {
		return nil, fmt.Errorf("decode llm response: %w", err)
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("llm response has no choices")
	}
	return out, nil
}
