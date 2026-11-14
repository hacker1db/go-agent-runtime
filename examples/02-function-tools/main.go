// Example 02: function tools + per-thread KV.
//
// A fake "model" (ProviderFunc) decides to call tools, then answers from the
// tool results. Shows JSON-schema tool args, sequential execution, tool
// errors persisted as results (the loop keeps going), and the durable
// per-thread key/value store used from inside a tool.
//
// Run: go run ./examples/02-function-tools
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"go-agent-runtime/agentruntime"
)

func main() {
	ctx := context.Background()
	reg := agentruntime.NewRegistry()

	// add: numeric addition with a strict schema.
	reg.AddTool(agentruntime.ToolDefinition{
		Name:        "add",
		Description: "Add two numbers.",
		Args: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"a": map[string]any{"type": "number"},
				"b": map[string]any{"type": "number"},
			},
			"required": []any{"a", "b"},
		},
		Execute: func(ctx context.Context, s *agentruntime.ThreadState, args map[string]any) (agentruntime.ToolResult, error) {
			sum := args["a"].(float64) + args["b"].(float64)
			// Count calls in the thread's durable KV store.
			var n int
			if _, err := s.GetValueInto(ctx, "add_calls", &n); err != nil {
				return agentruntime.ToolResult{}, err
			}
			if err := s.SetValue(ctx, "add_calls", n+1); err != nil {
				return agentruntime.ToolResult{}, err
			}
			return agentruntime.ToolResult{Status: agentruntime.ToolStatusSuccess, Result: sum}, nil
		},
	})

	// divide: returns an error result on divide-by-zero instead of crashing.
	reg.AddTool(agentruntime.ToolDefinition{
		Name:        "divide",
		Description: "Divide a by b.",
		Args: map[string]any{
			"type":       "object",
			"properties": map[string]any{"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"}},
			"required":   []any{"a", "b"},
		},
		Execute: func(ctx context.Context, s *agentruntime.ThreadState, args map[string]any) (agentruntime.ToolResult, error) {
			if args["b"].(float64) == 0 {
				return agentruntime.ToolResult{Status: agentruntime.ToolStatusError, Error: "division by zero"}, nil
			}
			return agentruntime.ToolResult{Status: agentruntime.ToolStatusSuccess, Result: args["a"].(float64) / args["b"].(float64)}, nil
		},
	})

	// Fake model: first step calls tools, second step summarizes tool output.
	fake := agentruntime.ProviderFunc(func(ctx context.Context, req agentruntime.ProviderRequest) (*agentruntime.ProviderResponse, error) {
		last := req.Messages[len(req.Messages)-1]
		if last.Role != agentruntime.RoleTool {
			return &agentruntime.ProviderResponse{ToolCalls: []agentruntime.ToolCall{
				toolCall("c1", "add", map[string]any{"a": 2, "b": 40}),
				toolCall("c2", "divide", map[string]any{"a": 1, "b": 0}),
			}}, nil
		}
		var results []string
		for _, m := range req.Messages {
			if m.Role == agentruntime.RoleTool {
				results = append(results, m.Name+"="+m.Content)
			}
		}
		return &agentruntime.ProviderResponse{Content: "Tool results: " + strings.Join(results, ", ")}, nil
	})

	reg.AddProvider("fake", fake)
	reg.AddModel(agentruntime.ModelDefinition{Name: "fake", Provider: "fake", Model: "fake-1"})
	reg.AddPrompt(agentruntime.PromptDefinition{
		Name:   "calc",
		Model:  "fake",
		System: "You are a calculator. Use tools.",
		Tools:  []agentruntime.PromptToolRef{{Name: "add"}, {Name: "divide"}},
	})
	reg.AddAgent(agentruntime.AgentDefinition{
		Name:  "calculator",
		SideA: agentruntime.SideConfig{Prompt: "calc"},
		SideB: agentruntime.SideConfig{Human: true},
	})

	rt := agentruntime.New(reg, agentruntime.NewMemoryStore(), agentruntime.Config{})
	if err := rt.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	th, err := rt.CreateThread(ctx, agentruntime.CreateThreadOptions{AgentID: "calculator"})
	if err != nil {
		log.Fatal(err)
	}
	if err := rt.SendMessage(ctx, th.ThreadID(), agentruntime.MessageInput{Content: "compute 2+40 and 1/0"}); err != nil {
		log.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rt.WaitIdle(wctx, th.ThreadID()); err != nil {
		log.Fatal(err)
	}

	res, _ := th.GetMessages(ctx, agentruntime.GetMessagesOptions{Order: "asc"})
	fmt.Println("== transcript ==")
	for _, m := range res.Messages {
		switch m.Role {
		case agentruntime.RoleTool:
			fmt.Printf("tool      [%s %s] %s\n", deref(m.Name), deref(m.ToolStatus), m.ContentString())
		default:
			if calls, _ := m.DecodeToolCalls(); len(calls) > 0 {
				for _, c := range calls {
					fmt.Printf("%-9s -> %s(%s)\n", m.Role+":", c.Function.Name, c.Function.Arguments)
				}
				continue
			}
			fmt.Printf("%-9s %s\n", m.Role+":", m.ContentString())
		}
	}

	var calls int
	_, _ = th.GetValueInto(ctx, "add_calls", &calls)
	fmt.Printf("\nKV add_calls = %d\n", calls)
}

func toolCall(id, name string, args map[string]any) agentruntime.ToolCall {
	b, _ := json.Marshal(args)
	return agentruntime.ToolCall{ID: id, Type: "function", Function: agentruntime.ToolCallFunction{Name: name, Arguments: string(b)}}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
