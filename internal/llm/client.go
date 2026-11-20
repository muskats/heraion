// Package llm runs the agent loop against an OpenAI-compatible
// chat-completions endpoint through openai-go, e.g.
// https://api.openai.com/v1 or http://litellm:4000/v1.
package llm

import (
	"context"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"github.com/muskats/heraion/internal/secret"
)

// Usage is token accounting summed over a run.
type Usage struct {
	PromptTokens, CompletionTokens, TotalTokens int
}

// Chatter is anything that can run a chat completion. *Client implements it.
type Chatter interface {
	Chat(ctx context.Context, req openai.ChatCompletionNewParams) (*openai.ChatCompletion, error)
}

// Client calls an OpenAI-compatible endpoint. The SDK retries 408, 409,
// 429, 5xx and connection errors, honouring Retry-After.
type Client struct {
	c      openai.Client
	apiKey *secret.Source
}

// NewClient returns a Client for baseURL. apiKey is read on every call, so
// a rotated token file is picked up; nil or unset sends no Authorization
// header (e.g. Ollama). opts override the defaults, e.g. option.WithMaxRetries(0).
func NewClient(baseURL string, apiKey *secret.Source, opts ...option.RequestOption) *Client {
	base := []option.RequestOption{
		option.WithBaseURL(baseURL),
		option.WithMaxRetries(4),
		option.WithRequestTimeout(5 * time.Minute),
	}
	return &Client{c: openai.NewClient(append(base, opts...)...), apiKey: apiKey}
}

// Chat sends one request.
func (c *Client) Chat(ctx context.Context, req openai.ChatCompletionNewParams) (*openai.ChatCompletion, error) {
	auth := option.WithHeaderDel("Authorization")
	if c.apiKey != nil && c.apiKey.Set() {
		key, err := c.apiKey.Value()
		if err != nil {
			return nil, fmt.Errorf("llm api key: %w", err)
		}
		auth = option.WithHeader("Authorization", "Bearer "+key)
	}
	return c.c.Chat.Completions.New(ctx, req, auth)
}
