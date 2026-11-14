// Example 04: handler side, hooks, and scheduled effects.
//
// No model at all: side A is a Go handler (one invocation per turn) that
// replies to each inbound message. A before_create_message hook stamps
// metadata on every persisted message, and a scheduled effect fires later
// and queues a "reminder" message that triggers another handler turn.
//
// Run: go run ./examples/04-handler-hooks-effects
package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"go-agent-runtime/agentruntime"
)

func main() {
	ctx := context.Background()
	reg := agentruntime.NewRegistry()

	// Handler side: uppercase echo bot.
	reg.AddHandler(agentruntime.HandlerDefinition{
		Name: "shout",
		Execute: func(ctx context.Context, s *agentruntime.ThreadState, turn agentruntime.HandlerTurn) error {
			for _, m := range turn.Inbound {
				reply := "BOT: " + strings.ToUpper(m.ContentString())
				if _, err := s.InjectMessage(ctx, agentruntime.MessageInput{Role: agentruntime.RoleAssistant, Content: reply}); err != nil {
					return err
				}
			}
			return nil
		},
	})

	// Hook: tag every message before it is persisted.
	reg.AddHook("", agentruntime.Hook{
		ID:   "stamp",
		Name: agentruntime.HookBeforeCreateMessage,
		BeforeCreateMessage: func(ctx context.Context, s *agentruntime.ThreadState, m agentruntime.Message) (agentruntime.Message, error) {
			if m.Metadata == nil {
				m.Metadata = map[string]any{}
			}
			m.Metadata["stamped_by"] = "stamp-hook"
			return m, nil
		},
	})

	// Effect: runs after a delay; queues a message onto the thread.
	fired := make(chan struct{}, 1)
	reg.AddEffect(agentruntime.EffectDefinition{
		Name: "reminder",
		Execute: func(ctx context.Context, s *agentruntime.ThreadState, args map[string]any) error {
			err := s.QueueMessage(ctx, agentruntime.MessageInput{Role: agentruntime.RoleUser, Content: "reminder: " + args["text"].(string)})
			fired <- struct{}{}
			return err
		},
	})

	reg.AddAgent(agentruntime.AgentDefinition{
		Name:  "shouter",
		Hooks: []string{"stamp"},
		SideA: agentruntime.SideConfig{Handler: "shout"},
		SideB: agentruntime.SideConfig{Human: true},
	})

	rt := agentruntime.New(reg, agentruntime.NewMemoryStore(), agentruntime.Config{
		EventSink: func(threadID, event string, data any) {
			fmt.Printf("  [event] %s\n", event)
		},
	})
	if err := rt.Start(ctx); err != nil {
		log.Fatal(err)
	}
	defer rt.Close()

	th, err := rt.CreateThread(ctx, agentruntime.CreateThreadOptions{AgentID: "shouter"})
	if err != nil {
		log.Fatal(err)
	}

	if err := rt.SendMessage(ctx, th.ThreadID(), agentruntime.MessageInput{Content: "hello there"}); err != nil {
		log.Fatal(err)
	}
	waitIdle(ctx, rt, th.ThreadID())

	id, err := th.ScheduleEffect(ctx, "reminder", map[string]any{"text": "stand up and stretch"}, 50*time.Millisecond)
	if err != nil {
		log.Fatal(err)
	}
	pending, _ := th.GetScheduledEffects(ctx, "reminder")
	fmt.Printf("scheduled effect %s (pending=%d)\n", id, len(pending))

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		log.Fatal("effect did not fire")
	}
	waitIdle(ctx, rt, th.ThreadID())

	res, _ := th.GetMessages(ctx, agentruntime.GetMessagesOptions{Order: "asc"})
	fmt.Println("\n== transcript ==")
	for _, m := range res.Messages {
		fmt.Printf("%-9s %-32s stamped_by=%v\n", m.Role+":", m.ContentString(), m.Metadata["stamped_by"])
	}
}

func waitIdle(ctx context.Context, rt *agentruntime.Runtime, id string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rt.WaitIdle(ctx, id); err != nil {
		log.Fatal(err)
	}
}
