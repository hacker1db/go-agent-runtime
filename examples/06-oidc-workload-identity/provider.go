package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/hacker1db/go-agent-runtime/agentruntime"
)

// TokenSource yields a bearer token for each request.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// ChatProvider is an agentruntime.Provider for an OpenAI-compatible
// chat-completions endpoint that authenticates every call with a short-lived
// bearer token from Tokens. It never sees a static API key.
type ChatProvider struct {
	BaseURL string // e.g. https://my-gateway.example.com/v1
	Tokens  TokenSource
	Client  *http.Client
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Generate sends the conversation and returns the first choice.
func (p *ChatProvider) Generate(ctx context.Context, req agentruntime.ProviderRequest) (*agentruntime.ProviderResponse, error) {
	endpoint := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
	if err := requireSecureURL(endpoint); err != nil {
		return nil, err
	}
	tok, err := p.Tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	msgs := make([]chatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, chatMessage{Role: string(m.Role), Content: m.Content})
	}
	body, err := json.Marshal(map[string]any{"model": req.Model, "messages": msgs})
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+tok)
	var out struct {
		Model   string `json:"model"`
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := doJSON(client(p.Client), hreq, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("model returned no choices")
	}
	return &agentruntime.ProviderResponse{
		Model:   out.Model,
		Content: out.Choices[0].Message.Content,
		Usage: agentruntime.ProviderUsage{
			InputTokens:  out.Usage.PromptTokens,
			OutputTokens: out.Usage.CompletionTokens,
			TotalTokens:  out.Usage.TotalTokens,
		},
	}, nil
}
