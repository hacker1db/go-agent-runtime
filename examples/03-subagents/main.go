// Example 03: parent/child subagents.
//
// A "lead" agent spawns a "researcher" child via the reserved
// subagent_create lifecycle tool. The child's arguments are validated
// against its prompt's RequiredSchema and interpolated into its system
// prompt. The child ends its session with a SessionStop tool ("done");
// with implicit parent communication the result is queued back to the
// parent as a silent status message, which wakes the parent for a final turn.
//
// Run: go run ./examples/03-subagents
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"go-agent-runtime/agentruntime"
)

func main() {
	ctx := context.Background()
	reg := agentruntime.NewRegistry()

	leadModel := &agentruntime.ScriptedProvider{Responses: []agentruntime.ProviderResponse{
		{ToolCalls: []agentruntime.ToolCall{toolCall(agentruntime.ToolSubagentCreate, map[string]any{
			"agent":     "researcher",
			"name":      "r1",
			"arguments": map[string]any{"topic": "Go generics"},
		})}},
		{Content: "I've asked a researcher; I'll report back."},
		{Content: "Summary from the researcher: generics landed in Go 1.18."},
	}}
	childModel := &agentruntime.ScriptedProvider{Responses: []agentruntime.ProviderResponse{
		{ToolCalls: []agentruntime.ToolCall{toolCall("done", map[string]any{"result": "Generics shipped in Go 1.18 (March 2022)."})}},
	}}

	reg.AddProvider("lead", leadModel)
	reg.AddProvider("child", childModel)
	reg.AddModel(agentruntime.ModelDefinition{Name: "lead", Provider: "lead", Model: "lead-1"})
	reg.AddModel(agentruntime.ModelDefinition{Name: "child", Provider: "child", Model: "child-1"})

	reg.AddPrompt(agentruntime.PromptDefinition{
		Name:   "research_p",
		Model:  "child",
		System: "Research the topic: {{topic}}. Call done with your findings.",
		RequiredSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"topic": map[string]any{"type": "string"}},
			"required":   []any{"topic"},
		},
	})
	reg.AddAgent(agentruntime.AgentDefinition{
		Name:            "researcher",
		ExposeAsTool:    true,
		ToolDescription: "Researches a topic and reports findings.",
		SideA: agentruntime.SideConfig{
			Prompt:      "research_p",
			SessionStop: &agentruntime.SessionToolBinding{Name: "done", MessageProperty: "result"},
		},
		SideB: agentruntime.SideConfig{Human: true},
	})

	reg.AddPrompt(agentruntime.PromptDefinition{Name: "lead_p", Model: "lead", System: "You coordinate researchers."})
	reg.AddAgent(agentruntime.AgentDefinition{
		Name: "lead",
		SideA: agentruntime.SideConfig{
			Prompt:    "lead_p",
			Subagents: []agentruntime.SubagentRelationship{{Agent: "researcher", MaxInstances: 3}},
		},
		SideB: agentruntime.SideConfig{Human: true},
	})

	for _, issue := range reg.Validate() {
		if issue.Severity == "error" {
			log.Fatalf("validation: [%s] %s: %s", issue.Code, issue.Path, issue.Message)
		}
	}

	rt := agentruntime.New(reg, agentruntime.NewMemoryStore(), agentruntime.Config{})
	if err := rt.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	parent, err := rt.CreateThread(ctx, agentruntime.CreateThreadOptions{AgentID: "lead"})
	if err != nil {
		log.Fatal(err)
	}
	if err := rt.SendMessage(ctx, parent.ThreadID(), agentruntime.MessageInput{Content: "What's the history of Go generics?"}); err != nil {
		log.Fatal(err)
	}
	waitIdle(ctx, rt, parent.ThreadID())

	children, _ := parent.Children(ctx)
	child, err := parent.GetChildThread(ctx, children[0].Reference)
	if err != nil {
		log.Fatal(err)
	}
	waitIdle(ctx, rt, child.ThreadID())  // child finishes its session
	waitIdle(ctx, rt, parent.ThreadID()) // parent woken by the result

	fmt.Println("== child thread ==")
	fmt.Printf("system prompt: %q\n", childModel.RequestsCopy()[0].Messages[0].Content)
	printThread(ctx, child)

	fmt.Println("\n== parent thread (incl. silent status messages) ==")
	printThread(ctx, parent)

	children, _ = parent.Children(ctx)
	for _, c := range children {
		fmt.Printf("\nregistry: ref=%s relationship=%s name=%s status=%s\n", c.Reference, c.Relationship, c.ThreadName, c.Status)
	}
}

func printThread(ctx context.Context, s *agentruntime.ThreadState) {
	res, err := s.GetMessages(ctx, agentruntime.GetMessagesOptions{Order: "asc", IncludeSilent: true})
	if err != nil {
		log.Fatal(err)
	}
	for _, m := range res.Messages {
		tag := ""
		if k, ok := m.Metadata[agentruntime.MetaStatusKind]; ok {
			tag = fmt.Sprintf(" [status_kind=%v silent=%v]", k, m.Silent)
		}
		if calls, _ := m.DecodeToolCalls(); len(calls) > 0 {
			for _, c := range calls {
				fmt.Printf("%-9s -> %s(%s)\n", m.Role+":", c.Function.Name, c.Function.Arguments)
			}
			continue
		}
		fmt.Printf("%-9s %s%s\n", m.Role+":", m.ContentString(), tag)
	}
}

func waitIdle(ctx context.Context, rt *agentruntime.Runtime, id string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rt.WaitIdle(ctx, id); err != nil {
		log.Fatal(err)
	}
}

func toolCall(name string, args map[string]any) agentruntime.ToolCall {
	b, _ := json.Marshal(args)
	return agentruntime.ToolCall{ID: "call_" + name, Type: "function", Function: agentruntime.ToolCallFunction{Name: name, Arguments: string(b)}}
}
