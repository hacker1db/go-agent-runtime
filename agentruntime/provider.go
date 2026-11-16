package agentruntime

import (
	"context"
	"encoding/json"
	"sync"
)

// ToolSpec is a tool as presented to a model.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ProviderRequest is a conversational model request.
type ProviderRequest struct {
	Model    string         `json:"model"`
	Messages []LLMMessage   `json:"messages"`
	Tools    []ToolSpec     `json:"tools,omitempty"`
	Params   map[string]any `json:"params,omitempty"`
}

// ProviderUsage reports token usage.
type ProviderUsage struct {
	InputTokens  int      `json:"inputTokens"`
	OutputTokens int      `json:"outputTokens"`
	TotalTokens  int      `json:"totalTokens"`
	Cost         *float64 `json:"cost,omitempty"`
}

// ProviderResponse is a model response.
type ProviderResponse struct {
	Model            string          `json:"model"`
	Content          string          `json:"content"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
	Usage            ProviderUsage   `json:"usage"`
}

// Provider is a conversational model provider.
type Provider interface {
	Generate(ctx context.Context, req ProviderRequest) (*ProviderResponse, error)
}

// ProviderFunc adapts a function to Provider.
type ProviderFunc func(ctx context.Context, req ProviderRequest) (*ProviderResponse, error)

// Generate calls f.
func (f ProviderFunc) Generate(ctx context.Context, req ProviderRequest) (*ProviderResponse, error) {
	return f(ctx, req)
}

// ScriptedProvider replays canned responses; useful for tests and examples.
// Each call pops the next response; Requests records every request.
type ScriptedProvider struct {
	mu        sync.Mutex
	Responses []ProviderResponse
	Requests  []ProviderRequest
	// Fallback, when non-nil, is used once Responses is exhausted.
	Fallback func(req ProviderRequest) (*ProviderResponse, error)
}

// Generate returns the next scripted response, or Fallback once Responses is exhausted.
func (p *ScriptedProvider) Generate(ctx context.Context, req ProviderRequest) (*ProviderResponse, error) {
	p.mu.Lock()
	p.Requests = append(p.Requests, cloneJSON(req))
	if len(p.Responses) == 0 {
		fb := p.Fallback
		p.mu.Unlock()
		if fb != nil {
			return fb(req)
		}
		return &ProviderResponse{Content: "done"}, nil
	}
	r := p.Responses[0]
	p.Responses = p.Responses[1:]
	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &r, nil
}

// RequestsCopy returns a snapshot of recorded requests.
func (p *ScriptedProvider) RequestsCopy() []ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ProviderRequest(nil), p.Requests...)
}
