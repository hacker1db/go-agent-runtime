// Example 01: basic chat.
//
// The smallest useful agent: side A is prompt-backed, side B is a human.
// Shows registry setup, {{var}} interpolation from thread arguments,
// SendMessage + WaitIdle, the persisted transcript, and run status.
//
// Run: go run ./examples/01-basic-chat
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/hacker1db/go-agent-runtime/agentruntime"
)

func main() {
	ctx := context.Background()

	// A scripted provider replays canned responses, so no API key is needed.
	// Swap in any type implementing agentruntime.Provider for a real model.
	prov := &agentruntime.ScriptedProvider{Responses: []agentruntime.ProviderResponse{
		{Content: "Hi Ada! Ask me anything about Go."},
		{Content: "Goroutines are lightweight threads managed by the Go runtime."},
	}}

	reg := agentruntime.NewRegistry()
	reg.AddProvider("scripted", prov)
	reg.AddModel(agentruntime.ModelDefinition{Name: "fast", Provider: "scripted", Model: "demo-model"})
	reg.AddPrompt(agentruntime.PromptDefinition{
		Name:   "tutor",
		Model:  "fast",
		System: "You are a friendly Go tutor helping {{student}}.",
	})
	reg.AddAgent(agentruntime.AgentDefinition{
		Name:  "go_tutor",
		SideA: agentruntime.SideConfig{Prompt: "tutor"},
		SideB: agentruntime.SideConfig{Human: true},
	})

	rt := agentruntime.New(reg, agentruntime.NewMemoryStore(), agentruntime.Config{})
	if err := rt.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	th, err := rt.CreateThread(ctx, agentruntime.CreateThreadOptions{
		AgentID:   "go_tutor",
		Arguments: map[string]any{"student": "Ada"},
	})
	if err != nil {
		log.Fatal(err)
	}

	for _, q := range []string{"hello", "what is a goroutine?"} {
		if err := rt.SendMessage(ctx, th.ThreadID(), agentruntime.MessageInput{Role: agentruntime.RoleUser, Content: q}); err != nil {
			log.Fatal(err)
		}
		waitIdle(ctx, rt, th.ThreadID())
	}

	res, err := th.GetMessages(ctx, agentruntime.GetMessagesOptions{Order: "asc"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("== transcript ==")
	for _, m := range res.Messages {
		fmt.Printf("%-9s %s\n", m.Role+":", m.ContentString())
	}

	st, _ := th.Status(ctx)
	fmt.Printf("\nstatus: %s (reason=%s)\n", st.State, deref(st.Reason))
	fmt.Printf("rendered system prompt: %q\n", prov.RequestsCopy()[0].Messages[0].Content)
}

func waitIdle(ctx context.Context, rt *agentruntime.Runtime, id string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rt.WaitIdle(ctx, id); err != nil {
		log.Fatal(err)
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
