package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Review fix: a failed durable write must not leave the mutation visible.
func TestPersistFailureLeavesStateUnchanged(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	_ = s.PutThread(ctx, ThreadRecord{ID: "t"})
	s.persist = func(string, *threadData) error { return errors.New("disk full") }
	if err := s.Enqueue(ctx, "t", QueueItem{ID: "q"}); err == nil {
		t.Fatal("expected persist error")
	}
	if err := s.InsertMessageAck(ctx, "t", Message{ID: "m"}, nil); err == nil {
		t.Fatal("expected persist error")
	}
	q, _ := s.PeekQueue(ctx, "t")
	m, _ := s.ListMessages(ctx, "t")
	if len(q) != 0 || len(m) != 0 {
		t.Fatalf("unpersisted mutation visible: queue=%d messages=%d", len(q), len(m))
	}
}

// Review fix: message insert and queue ack are atomic and idempotent.
func TestInsertMessageAckAtomic(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	_ = s.PutThread(ctx, ThreadRecord{ID: "t"})
	_ = s.Enqueue(ctx, "t", QueueItem{ID: "q1", Kind: QueueMessage})
	if err := s.InsertMessageAck(ctx, "t", Message{ID: "q-q1"}, []string{"q1"}); err != nil {
		t.Fatal(err)
	}
	q, _ := s.PeekQueue(ctx, "t")
	if len(q) != 0 {
		t.Fatal("queue item not acknowledged with insert")
	}
	if err := s.InsertMessageAck(ctx, "t", Message{ID: "q-q1"}, nil); err == nil {
		t.Fatal("duplicate delivery id must be rejected")
	}
}

// Review fix: tool calls orphaned by a lost execution get error results on
// Start and are not re-executed.
func TestDanglingToolCallsRepairedOnStart(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	_ = st.PutThread(ctx, ThreadRecord{ID: "x", AgentID: "ag", Status: ThreadStatus{State: RunRunning, Since: 1}})
	calls, _ := json.Marshal([]ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "side_effect", Arguments: "{}"}}})
	_ = st.InsertMessage(ctx, "x", Message{ID: "a1", Role: RoleAssistant, ToolCalls: ptr(string(calls)), CreatedAt: 1, Metadata: map[string]any{}})
	f := newFixture(t)
	f.store = st
	var ran int32
	f.reg.AddTool(ToolDefinition{Name: "side_effect", Execute: func(context.Context, *ThreadState, map[string]any) (ToolResult, error) {
		atomic.AddInt32(&ran, 1)
		return ToolResult{}, nil
	}})
	f.model("m")
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m", Tools: []PromptToolRef{{Name: "side_effect"}}})
	assistantAgent(f, "ag", "p", SideConfig{})
	f.start(Config{})
	s, _ := f.rt.Thread(ctx, "x")
	ms := msgs(t, s)
	if len(ms) != 2 || ms[1].Role != RoleTool || deref(ms[1].ToolCallID) != "c1" || deref(ms[1].ToolStatus) != ToolStatusError {
		t.Fatalf("not repaired: %+v", ms)
	}
	if ran != 0 {
		t.Fatal("interrupted tool must not be re-executed")
	}
}

// Review fix: an effect is removed only after it succeeds; failures retry.
func TestEffectRetriedUntilSuccess(t *testing.T) {
	old := effectRetryBackoff
	effectRetryBackoff = 10 * time.Millisecond
	t.Cleanup(func() { effectRetryBackoff = old })
	f := newFixture(t)
	var runs int32
	f.reg.AddEffect(EffectDefinition{Name: "flaky", Execute: func(context.Context, *ThreadState, map[string]any) error {
		if atomic.AddInt32(&runs, 1) == 1 {
			return errors.New("transient")
		}
		return nil
	}})
	f.model("m")
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	assistantAgent(f, "ag", "p", SideConfig{})
	f.start(Config{})
	s := f.thread("ag")
	if _, err := s.ScheduleEffect(context.Background(), "flaky", nil, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		left, _ := s.GetScheduledEffects(context.Background(), "")
		return atomic.LoadInt32(&runs) == 2 && len(left) == 0
	})
}

// Review fix: failed creation leaves no thread and no orphaned children.
func TestFailedCreationIsCompensated(t *testing.T) {
	f := newFixture(t)
	f.model("m")
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	f.reg.AddHook("", Hook{ID: "deny", Name: HookAfterThreadCreated, AfterThreadCreated: func(context.Context, *ThreadState) error {
		return errors.New("denied")
	}})
	f.reg.AddAgent(AgentDefinition{Name: "kid", SideA: SideConfig{Prompt: "p"}, SideB: SideConfig{Human: true}})
	f.reg.AddAgent(AgentDefinition{Name: "par", Hooks: []string{"deny"},
		SideA: SideConfig{Prompt: "p", Subagents: []SubagentRelationship{{Agent: "kid", Immediate: &ImmediateConfig{}}}}, SideB: SideConfig{Human: true}})
	f.start(Config{})
	if _, err := f.rt.CreateThread(context.Background(), CreateThreadOptions{AgentID: "par"}); err == nil {
		t.Fatal("expected creation failure")
	}
	all, _ := f.store.ListThreads(context.Background())
	if len(all) != 0 {
		t.Fatalf("orphaned threads left behind: %d", len(all))
	}
}

// Review fix: ownerless threads are not reachable through the bootstrap API.
func TestBootstrapRejectsOwnerlessThreads(t *testing.T) {
	f := newFixture(t)
	f.model("m")
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	assistantAgent(f, "ag", "p", SideConfig{})
	f.start(Config{})
	s := f.thread("ag")
	pb := f.rt.newBootstrap(s.ThreadID(), SideA, "x", nil, []string{"V"})
	srv := httptest.NewServer(f.rt.BootstrapHandler("", func(*http.Request) (string, bool) { return "anyone", true }))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/threads/" + s.ThreadID() + "/variables/" + pb.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}
