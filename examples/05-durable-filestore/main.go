// Example 05: durable FileStore across a restart.
//
// Phase 1 runs a conversation on a FileStore, sets a secret thread env value
// and a KV entry, then shuts the runtime down. Phase 2 opens the same
// directory with a brand-new runtime, recovers the thread, and continues the
// conversation. Secret env values are AES-GCM encrypted at rest.
//
// Run: go run ./examples/05-durable-filestore
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"go-agent-runtime/agentruntime"
)

// Demo key only. In production load a 32-byte key from a secret manager.
var key = bytes.Repeat([]byte("k"), 32)

func newRuntime(ctx context.Context, dir string, replies ...string) *agentruntime.Runtime {
	store, err := agentruntime.OpenFileStore(dir, key)
	if err != nil {
		log.Fatal(err)
	}
	var resps []agentruntime.ProviderResponse
	for _, r := range replies {
		resps = append(resps, agentruntime.ProviderResponse{Content: r})
	}
	reg := agentruntime.NewRegistry()
	reg.AddProvider("scripted", &agentruntime.ScriptedProvider{Responses: resps})
	reg.AddModel(agentruntime.ModelDefinition{Name: "m", Provider: "scripted", Model: "demo"})
	reg.AddPrompt(agentruntime.PromptDefinition{Name: "notes_p", Model: "m", System: "You keep notes."})
	reg.AddAgent(agentruntime.AgentDefinition{
		Name:  "notes",
		SideA: agentruntime.SideConfig{Prompt: "notes_p"},
		SideB: agentruntime.SideConfig{Human: true},
	})
	rt := agentruntime.New(reg, store, agentruntime.Config{})
	// Start recovers lost runs, re-arms effects, and resumes durable queues.
	if err := rt.Start(ctx); err != nil {
		log.Fatal(err)
	}
	return rt
}

func main() {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "agentruntime-example-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	fmt.Println("store dir:", dir)

	// ---- Phase 1 ----
	rt1 := newRuntime(ctx, dir, "Noted: buy milk.")
	th, err := rt1.CreateThread(ctx, agentruntime.CreateThreadOptions{
		AgentID: "notes",
		Env:     map[string]agentruntime.EnvValue{"API_TOKEN": {Value: "super-secret-token", Type: "secret"}},
	})
	if err != nil {
		log.Fatal(err)
	}
	threadID := th.ThreadID()
	send(ctx, rt1, threadID, "remember to buy milk")
	if err := th.SetValue(ctx, "note_count", 1); err != nil {
		log.Fatal(err)
	}
	rt1.Close()
	fmt.Println("phase 1 done; runtime closed")

	raw, _ := os.ReadFile(filepath.Join(dir, threadID+".json"))
	fmt.Printf("secret visible in plaintext on disk? %v\n", bytes.Contains(raw, []byte("super-secret-token")))

	// ---- Phase 2: fresh runtime, same directory ----
	rt2 := newRuntime(ctx, dir, "Noted: call mom. You have 2 notes.")
	defer rt2.Close()
	th2, err := rt2.Thread(ctx, threadID)
	if err != nil {
		log.Fatal(err)
	}
	tok, _ := th2.Env(ctx, "API_TOKEN")
	var count int
	_, _ = th2.GetValueInto(ctx, "note_count", &count)
	fmt.Printf("recovered env API_TOKEN=%q, KV note_count=%d\n", tok, count)

	send(ctx, rt2, threadID, "also call mom")
	_ = th2.SetValue(ctx, "note_count", count+1)

	res, _ := th2.GetMessages(ctx, agentruntime.GetMessagesOptions{Order: "asc"})
	fmt.Println("\n== transcript across restart ==")
	for _, m := range res.Messages {
		fmt.Printf("%-9s %s\n", m.Role+":", m.ContentString())
	}
}

func send(ctx context.Context, rt *agentruntime.Runtime, id, content string) {
	if err := rt.SendMessage(ctx, id, agentruntime.MessageInput{Content: content}); err != nil {
		log.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rt.WaitIdle(wctx, id); err != nil {
		log.Fatal(err)
	}
}
