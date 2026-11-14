package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- helpers ----

func call(name string, args map[string]any) ToolCall {
	b, _ := json.Marshal(args)
	return ToolCall{ID: "call_" + newID()[:8], Type: "function", Function: ToolCallFunction{Name: name, Arguments: string(b)}}
}

func text(s string) ProviderResponse { return ProviderResponse{Content: s} }

func tools(calls ...ToolCall) ProviderResponse { return ProviderResponse{ToolCalls: calls} }

type fixture struct {
	t     *testing.T
	reg   *Registry
	rt    *Runtime
	store Store
	prov  map[string]*ScriptedProvider
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	return &fixture{t: t, reg: NewRegistry(), store: NewMemoryStore(), prov: map[string]*ScriptedProvider{}}
}

// model registers a model backed by its own scripted provider.
func (f *fixture) model(name string, responses ...ProviderResponse) *ScriptedProvider {
	p := &ScriptedProvider{Responses: responses}
	f.prov[name] = p
	f.reg.AddProvider("p_"+name, p)
	f.reg.AddModel(ModelDefinition{Name: name, Provider: "p_" + name, Model: name})
	return p
}

func (f *fixture) start(cfg Config) *Runtime {
	f.t.Helper()
	if issues := f.reg.Validate(); hasErrors(issues) {
		f.t.Fatalf("invalid registry: %v", (&ValidationError{issues}).Error())
	}
	f.rt = New(f.reg, f.store, cfg)
	if err := f.rt.Start(context.Background()); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(f.rt.Close)
	return f.rt
}

func (f *fixture) thread(agent string) *ThreadState {
	f.t.Helper()
	s, err := f.rt.CreateThread(context.Background(), CreateThreadOptions{AgentID: agent})
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fixture) send(s *ThreadState, content string) {
	f.t.Helper()
	if err := f.rt.SendMessage(context.Background(), s.ThreadID(), MessageInput{Role: RoleUser, Content: content}); err != nil {
		f.t.Fatal(err)
	}
	f.wait(s)
}

func (f *fixture) wait(s *ThreadState) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.rt.WaitIdle(ctx, s.ThreadID()); err != nil {
		f.t.Fatalf("wait idle: %v", err)
	}
}

func msgs(t *testing.T, s *ThreadState) []Message {
	t.Helper()
	r, err := s.GetMessages(context.Background(), GetMessagesOptions{Order: "asc", IncludeSilent: true})
	if err != nil {
		t.Fatal(err)
	}
	return r.Messages
}

func status(t *testing.T, s *ThreadState) ThreadStatus {
	t.Helper()
	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func assistantAgent(f *fixture, name, prompt string, side SideConfig) {
	side.Prompt = prompt
	f.reg.AddAgent(AgentDefinition{Name: name, SideA: side, SideB: SideConfig{Human: true}})
}

// ---- step cycle, persistence, perspective ----

func TestStepCycleAndStatus(t *testing.T) {
	f := newFixture(t)
	p := f.model("m", text("hello"))
	f.reg.AddPrompt(PromptDefinition{Name: "chat", Model: "m", System: "You are {{who}}."})
	assistantAgent(f, "assistant", "chat", SideConfig{})
	f.start(Config{})
	s, err := f.rt.CreateThread(context.Background(), CreateThreadOptions{AgentID: "assistant", Arguments: map[string]any{"who": "helpful"}})
	if err != nil {
		t.Fatal(err)
	}
	if st := status(t, s); st.State != RunIdle || st.Since != 0 || st.Reason != nil {
		t.Fatalf("fresh status = %+v", st)
	}
	f.send(s, "hi")
	ms := msgs(t, s)
	if len(ms) != 2 || ms[0].Role != RoleUser || ms[1].Role != RoleAssistant || ms[1].ContentString() != "hello" {
		t.Fatalf("messages = %+v", ms)
	}
	st := status(t, s)
	if st.State != RunIdle || deref(st.Reason) != ReasonStopOnResponse {
		t.Fatalf("status = %+v", st)
	}
	req := p.RequestsCopy()[0]
	if req.Messages[0].Role != RoleSystem || req.Messages[0].Content != "You are helpful." {
		t.Fatalf("system = %+v", req.Messages[0])
	}
}

func TestToolsSequentialErrorsPersisted(t *testing.T) {
	f := newFixture(t)
	var order []string
	f.reg.AddTool(ToolDefinition{Name: "a", Args: map[string]any{"type": "object", "properties": map[string]any{"n": map[string]any{"type": "number"}}, "required": []any{"n"}},
		Execute: func(ctx context.Context, s *ThreadState, args map[string]any) (ToolResult, error) {
			order = append(order, "a")
			return ToolResult{Result: "ok"}, nil
		}})
	f.reg.AddTool(ToolDefinition{Name: "boom", Execute: func(ctx context.Context, s *ThreadState, args map[string]any) (ToolResult, error) {
		order = append(order, "boom")
		panic("kaboom")
	}})
	f.model("m", tools(call("boom", nil), call("a", map[string]any{"n": 1}), call("a", map[string]any{"n": "bad"}), call("missing", nil)), text("done"))
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m", Tools: []PromptToolRef{{Name: "a"}, {Name: "boom"}}})
	assistantAgent(f, "ag", "p", SideConfig{})
	f.start(Config{})
	s := f.thread("ag")
	f.send(s, "go")
	if strings.Join(order, ",") != "boom,a" {
		t.Fatalf("order = %v", order)
	}
	var statuses []string
	for _, m := range msgs(t, s) {
		if m.Role == RoleTool {
			statuses = append(statuses, deref(m.ToolStatus))
		}
	}
	if strings.Join(statuses, ",") != "error,success,error,error" {
		t.Fatalf("tool statuses = %v", statuses)
	}
	if last := msgs(t, s); last[len(last)-1].ContentString() != "done" {
		t.Fatal("loop did not continue after tool errors")
	}
}

func TestPromptSidesPerspective(t *testing.T) {
	f := newFixture(t)
	f.reg.AddTool(ToolDefinition{Name: "t", Execute: func(ctx context.Context, s *ThreadState, a map[string]any) (ToolResult, error) {
		return ToolResult{Result: "secret-tool-output"}, nil
	}})
	f.model("ma", tools(call("t", nil)), text("A says hi"))
	pb := f.model("mb", text("B replies"))
	f.reg.AddPrompt(PromptDefinition{Name: "pa", Model: "ma", Tools: []PromptToolRef{{Name: "t"}}})
	f.reg.AddPrompt(PromptDefinition{Name: "pb", Model: "mb"})
	f.reg.AddAgent(AgentDefinition{Name: "duo", SideA: SideConfig{Prompt: "pa"}, SideB: SideConfig{Prompt: "pb"}, MaxSessionTurns: 2})
	f.start(Config{})
	s := f.thread("duo")
	f.send(s, "start")
	reqB := pb.RequestsCopy()[0]
	for _, m := range reqB.Messages {
		if m.Role == RoleTool || len(m.ToolCalls) > 0 {
			t.Fatalf("side B saw side A tool traffic: %+v", m)
		}
	}
	// B's own "start" message appears as assistant; A's reply as user.
	if reqB.Messages[0].Role != RoleAssistant || reqB.Messages[len(reqB.Messages)-1].Role != RoleUser {
		t.Fatalf("perspective wrong: %+v", reqB.Messages)
	}
	if st := status(t, s); deref(st.Reason) != ReasonMaxSessionTurns {
		t.Fatalf("status = %+v", st)
	}
}

// ---- context assembly ordering ----

func TestContextAssemblyHookOrder(t *testing.T) {
	f := newFixture(t)
	var trace []string
	var filterCalls int32
	f.reg.AddHook("", Hook{ID: "sys", Name: HookAfterSystemMessage, AfterSystemMessage: func(ctx context.Context, s *ThreadState, sys string) (*string, error) {
		trace = append(trace, "after_system_message")
		if _, err := s.InjectMessage(ctx, MessageInput{Role: RoleUser, Content: "from-system-hook", Silent: true}); err != nil {
			return nil, err
		}
		r := sys + " [patched]"
		return &r, nil
	}})
	f.reg.AddHook("", Hook{ID: "flt", Name: HookFilterMessages, FilterMessages: func(ctx context.Context, s *ThreadState, ms []Message) ([]Message, error) {
		trace = append(trace, "filter_messages")
		if atomic.AddInt32(&filterCalls, 1) == 1 {
			if _, err := s.InjectMessage(ctx, MessageInput{Role: RoleUser, Content: "from-filter-hook"}); err != nil {
				return nil, err
			}
		}
		return append(ms, Message{Role: RoleUser, Content: ptr("synthetic")}), nil
	}})
	f.reg.AddHook("", Hook{ID: "pre", Name: HookPrefilterLLMHistory, PrefilterLLMHistory: func(ctx context.Context, s *ThreadState, ms []LLMMessage) ([]LLMMessage, error) {
		trace = append(trace, "prefilter_llm_history")
		if hist := s.Execution().MessageHistory(); len(hist) == 0 {
			return nil, errors.New("execution history empty")
		}
		for _, m := range s.Execution().MessageHistory() {
			if m.ContentString() == "synthetic" {
				return nil, errors.New("synthetic leaked into execution history")
			}
		}
		return ms, nil
	}})
	f.reg.AddTool(ToolDefinition{Name: "noop", Execute: func(ctx context.Context, s *ThreadState, a map[string]any) (ToolResult, error) {
		return ToolResult{}, nil
	}})
	p := f.model("m", tools(call("noop", nil)), text("ok"))
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m", System: "sys", Hooks: []string{"sys", "flt", "pre"}, Tools: []PromptToolRef{{Name: "noop"}}})
	assistantAgent(f, "ag", "p", SideConfig{})
	f.start(Config{})
	s := f.thread("ag")
	f.send(s, "hi")

	want := "after_system_message,filter_messages,prefilter_llm_history"
	if got := strings.Join(trace[:3], ","); got != want {
		t.Fatalf("order = %s", got)
	}
	reqs := p.RequestsCopy()
	has := func(req ProviderRequest, c string) bool {
		for _, m := range req.Messages {
			if m.Content == c {
				return true
			}
		}
		return false
	}
	if reqs[0].Messages[0].Content != "sys [patched]" {
		t.Fatalf("system replacement not applied: %q", reqs[0].Messages[0].Content)
	}
	if !has(reqs[0], "from-system-hook") {
		t.Fatal("message injected in after_system_message must be in current assembly")
	}
	if has(reqs[0], "from-filter-hook") {
		t.Fatal("message injected in filter_messages must not be in current assembly")
	}
	if !has(reqs[1], "from-filter-hook") || !has(reqs[0], "synthetic") {
		t.Fatal("filter injection must appear in following assembly; synthetic in request")
	}
	if filterCalls != 2 {
		t.Fatalf("filter_messages calls = %d, want once per assembly (2)", filterCalls)
	}
	for _, m := range msgs(t, s) {
		if m.ContentString() == "synthetic" {
			t.Fatal("synthetic message persisted")
		}
	}
}

// ---- stop conditions ----

func TestStopOrderSessionStopBeatsStopTool(t *testing.T) {
	f := newFixture(t)
	f.model("m", tools(call("halt", nil), call("finish", map[string]any{"result": "R"})))
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	assistantAgent(f, "ag", "p", SideConfig{StopTool: "halt", SessionStop: &SessionToolBinding{Name: "finish", MessageProperty: "result"}})
	f.start(Config{})
	s := f.thread("ag")
	f.send(s, "go")
	if st := status(t, s); deref(st.Reason) != ReasonSessionStop {
		t.Fatalf("reason = %v", deref(st.Reason))
	}
}

func TestSafetyLimits(t *testing.T) {
	f := newFixture(t)
	p := f.model("m")
	p.Fallback = func(req ProviderRequest) (*ProviderResponse, error) {
		r := tools(call("noop", nil))
		return &r, nil
	}
	f.reg.AddTool(ToolDefinition{Name: "noop", Execute: func(ctx context.Context, s *ThreadState, a map[string]any) (ToolResult, error) {
		return ToolResult{}, nil
	}})
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m", Tools: []PromptToolRef{{Name: "noop"}}})
	assistantAgent(f, "side", "p", SideConfig{MaxSteps: 3})
	assistantAgent(f, "cap", "p", SideConfig{})
	f.start(Config{MaxTotalSteps: 5})
	s := f.thread("side")
	f.send(s, "go")
	if st := status(t, s); deref(st.Reason) != ReasonMaxSideSteps || st.State != RunIdle {
		t.Fatalf("status = %+v", st)
	}
	s2 := f.thread("cap")
	f.send(s2, "go")
	if st := status(t, s2); deref(st.Reason) != ReasonMaxTotalSteps {
		t.Fatalf("status = %+v", st)
	}
}

// ---- queue & concurrency ----

func TestQueueFIFOInjectedBeforeNextModelCall(t *testing.T) {
	f := newFixture(t)
	f.reg.AddTool(ToolDefinition{Name: "q", Execute: func(ctx context.Context, s *ThreadState, a map[string]any) (ToolResult, error) {
		for _, c := range []string{"first", "second"} {
			if err := s.QueueMessage(ctx, MessageInput{Role: RoleUser, Content: c}); err != nil {
				return ToolResult{}, err
			}
		}
		return ToolResult{Result: "queued"}, nil
	}})
	p := f.model("m", tools(call("q", nil)), text("ok"))
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m", Tools: []PromptToolRef{{Name: "q"}}})
	assistantAgent(f, "ag", "p", SideConfig{})
	f.start(Config{})
	s := f.thread("ag")
	f.send(s, "go")
	second := p.RequestsCopy()[1].Messages
	n := len(second)
	if second[n-2].Content != "first" || second[n-1].Content != "second" {
		t.Fatalf("queued messages not injected in order before next call: %+v", second[n-2:])
	}
}

func TestSingleFlowPerThread(t *testing.T) {
	f := newFixture(t)
	var active, maxActive int32
	p := f.model("m")
	p.Fallback = func(req ProviderRequest) (*ProviderResponse, error) {
		n := atomic.AddInt32(&active, 1)
		for {
			m := atomic.LoadInt32(&maxActive)
			if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		r := text("ok")
		return &r, nil
	}
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	assistantAgent(f, "ag", "p", SideConfig{})
	f.start(Config{})
	s := f.thread("ag")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.rt.SendMessage(context.Background(), s.ThreadID(), MessageInput{Role: RoleUser, Content: "x"})
		}()
	}
	wg.Wait()
	f.wait(s)
	if maxActive != 1 {
		t.Fatalf("concurrent flows on one thread: %d", maxActive)
	}
	users := 0
	for _, m := range msgs(t, s) {
		if m.Role == RoleUser {
			users++
		}
	}
	if users != 20 {
		t.Fatalf("delivered %d/20 queued messages", users)
	}
}

// ---- subagents ----

func childFixture(t *testing.T, rel SubagentRelationship, parentResponses []ProviderResponse, childResponses []ProviderResponse) (*fixture, *ScriptedProvider) {
	f := newFixture(t)
	pp := f.model("parent", parentResponses...)
	f.model("child", childResponses...)
	f.reg.AddPrompt(PromptDefinition{Name: "parent_p", Model: "parent"})
	f.reg.AddPrompt(PromptDefinition{Name: "child_p", Model: "child", System: "topic={{topic}}",
		RequiredSchema: map[string]any{"type": "object", "properties": map[string]any{"topic": map[string]any{"type": "string"}}, "required": []any{"topic"}}})
	f.reg.AddAgent(AgentDefinition{Name: "worker", ExposeAsTool: true, ToolDescription: "does work",
		SideA: SideConfig{Prompt: "child_p", SessionStop: &SessionToolBinding{Name: "done", MessageProperty: "result", AttachmentsProperty: "files"}},
		SideB: SideConfig{Human: true}})
	f.reg.AddAgent(AgentDefinition{Name: "boss", SideA: SideConfig{Prompt: "parent_p", Subagents: []SubagentRelationship{rel}}, SideB: SideConfig{Human: true}})
	return f, pp
}

func TestSubagentImplicitCompletionAndAttachments(t *testing.T) {
	f, pp := childFixture(t, SubagentRelationship{Agent: "worker"},
		[]ProviderResponse{tools(call(ToolSubagentCreate, map[string]any{"agent": "worker", "name": "w1", "arguments": map[string]any{"topic": "go"}, "attachments": []any{"/in.txt"}})), text("spawned"), text("got result")},
		nil)
	f.prov["child"].Fallback = nil
	f.reg.AddTool(ToolDefinition{Name: "write_out", Execute: func(ctx context.Context, s *ThreadState, a map[string]any) (ToolResult, error) {
		_, err := s.WriteFile(ctx, "/out.txt", []byte("OUT"), "text/plain")
		return ToolResult{}, err
	}})
	f.prov["child"].Responses = []ProviderResponse{tools(call("write_out", nil)), tools(call("done", map[string]any{"result": "R42", "files": []any{"/out.txt"}}))}
	f.reg.AddPrompt(PromptDefinition{Name: "child_p", Model: "child", System: "topic={{topic}}", Tools: []PromptToolRef{{Name: "write_out"}},
		RequiredSchema: map[string]any{"type": "object", "required": []any{"topic"}}})
	f.start(Config{})
	s := f.thread("boss")
	if _, err := s.WriteFile(context.Background(), "/in.txt", []byte("IN"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	f.send(s, "delegate")
	children, _ := s.Children(context.Background())
	if len(children) != 1 || children[0].Relationship != "worker" || children[0].ThreadName != "w1" {
		t.Fatalf("registry = %+v", children)
	}
	child, _ := s.GetChildThread(context.Background(), children[0].Reference)
	f.wait(child)
	f.wait(s)
	// Child got a child-local copy of the parent attachment.
	cm := msgs(t, child)
	if len(cm[0].Attachments) != 1 || !strings.HasPrefix(cm[0].Attachments[0].Path, "/inbox/") {
		t.Fatalf("child attachment = %+v", cm[0].Attachments)
	}
	if b, _ := child.ReadFile(context.Background(), cm[0].Attachments[0].Path); string(b) != "IN" {
		t.Fatal("attachment content not copied to child")
	}
	// Arguments hydrated into child prompt rendering.
	if sys := f.prov["child"].RequestsCopy()[0].Messages[0].Content; sys != "topic=go" {
		t.Fatalf("child system = %q", sys)
	}
	// Parent received silent completion with parent-local attachment.
	var res *Message
	for _, m := range msgs(t, s) {
		if m.Metadata[MetaStatusKind] == "subagent_result" {
			mm := m
			res = &mm
		}
	}
	if res == nil || !res.Silent || !strings.Contains(res.ContentString(), "R42") || !strings.Contains(res.ContentString(), children[0].Reference) {
		t.Fatalf("completion message = %+v", res)
	}
	if len(res.Attachments) != 1 || !strings.HasPrefix(res.Attachments[0].Path, "/subagents/") {
		t.Fatalf("parent attachment = %+v", res.Attachments)
	}
	if b, _ := s.ReadFile(context.Background(), res.Attachments[0].Path); string(b) != "OUT" {
		t.Fatal("child attachment not copied to parent")
	}
	children, _ = s.Children(context.Background())
	if children[0].Status != ChildIdle {
		t.Fatalf("child status = %s", children[0].Status)
	}
	if term, _ := child.Terminated(context.Background()); term != nil {
		t.Fatal("session end must not terminate child")
	}
	if n := len(pp.RequestsCopy()); n != 3 {
		t.Fatalf("parent should be woken by completion; requests=%d", n)
	}
}

func TestSubagentExplicitDoesNotWakeParent(t *testing.T) {
	f, pp := childFixture(t, SubagentRelationship{Agent: "worker", ParentCommunication: ParentCommExplicit},
		[]ProviderResponse{tools(call(ToolSubagentCreate, map[string]any{"agent": "worker", "name": "w", "arguments": map[string]any{"topic": "x"}})), text("spawned")},
		[]ProviderResponse{tools(call("done", map[string]any{"result": "R"}))})
	f.start(Config{})
	s := f.thread("boss")
	f.send(s, "go")
	ch, _ := s.Children(context.Background())
	child, _ := s.GetChildThread(context.Background(), ch[0].Reference)
	f.wait(child)
	f.wait(s)
	for _, m := range msgs(t, s) {
		if m.Metadata[MetaStatusKind] != nil {
			t.Fatalf("explicit child auto-queued to parent: %+v", m)
		}
	}
	if n := len(pp.RequestsCopy()); n != 2 {
		t.Fatalf("parent woken: %d requests", n)
	}
}

func TestBlockingCreateOrContinueAndMaxInstances(t *testing.T) {
	f, _ := childFixture(t, SubagentRelationship{Agent: "worker", Blocking: true, MaxInstances: 1},
		[]ProviderResponse{
			tools(call(ToolSubagentCreate, map[string]any{"agent": "worker", "name": "w", "arguments": map[string]any{"topic": "a"}})),
			tools(call(ToolSubagentCreate, map[string]any{"agent": "worker", "name": "w", "arguments": map[string]any{"topic": "b"}})),
			text("fin"),
		},
		[]ProviderResponse{tools(call("done", map[string]any{"result": "first"})), tools(call("done", map[string]any{"result": "second"}))})
	f.start(Config{})
	s := f.thread("boss")
	f.send(s, "go")
	var results []string
	for _, m := range msgs(t, s) {
		if m.Role == RoleTool && deref(m.Name) == ToolSubagentCreate {
			results = append(results, m.ContentString())
		}
	}
	if len(results) != 2 || !strings.Contains(results[0], `"result":"first"`) || !strings.Contains(results[1], `"result":"second"`) {
		t.Fatalf("blocking results = %v", results)
	}
	if ch, _ := s.Children(context.Background()); len(ch) != 1 {
		t.Fatalf("create-or-continue must reuse the instance; children=%d", len(ch))
	}
	for _, m := range msgs(t, s) {
		if m.Metadata[MetaStatusKind] == "subagent_result" {
			t.Fatal("blocking result must not also be auto-queued")
		}
	}

	// Non-blocking cap returns an actionable tool error.
	f2, _ := childFixture(t, SubagentRelationship{Agent: "worker", MaxInstances: 1},
		[]ProviderResponse{
			tools(call(ToolSubagentCreate, map[string]any{"agent": "worker", "name": "a", "arguments": map[string]any{"topic": "a"}}),
				call(ToolSubagentCreate, map[string]any{"agent": "worker", "name": "b", "arguments": map[string]any{"topic": "b"}})),
			text("x"),
		}, nil)
	f2.start(Config{})
	s2 := f2.thread("boss")
	f2.send(s2, "go")
	var capErr string
	for _, m := range msgs(t, s2) {
		if m.Role == RoleTool && deref(m.ToolStatus) == ToolStatusError {
			capErr = m.ContentString()
		}
	}
	if !strings.Contains(capErr, "subagent_max_instances") || !strings.Contains(capErr, "subagent_message") {
		t.Fatalf("cap error = %q", capErr)
	}
}

func TestSubagentArgumentsRequired(t *testing.T) {
	f, _ := childFixture(t, SubagentRelationship{Agent: "worker"},
		[]ProviderResponse{tools(call(ToolSubagentCreate, map[string]any{"agent": "worker", "name": "w"})), text("x")}, nil)
	f.start(Config{})
	s := f.thread("boss")
	f.send(s, "go")
	if ch, _ := s.Children(context.Background()); len(ch) != 0 {
		t.Fatal("child spawned without required arguments")
	}
	found := false
	for _, m := range msgs(t, s) {
		if strings.Contains(m.ContentString(), "subagent_arguments_required") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing subagent_arguments_required error")
	}
}

func TestHiddenOptionalImmediate(t *testing.T) {
	f := newFixture(t)
	pp := f.model("m", text("hi"))
	f.model("cm")
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	f.reg.AddPrompt(PromptDefinition{Name: "cp", Model: "cm"})
	f.reg.AddAgent(AgentDefinition{Name: "leaf", SideA: SideConfig{Prompt: "cp"}, SideB: SideConfig{Human: true}})
	f.reg.AddAgent(AgentDefinition{Name: "mid", SideA: SideConfig{Prompt: "cp", Subagents: []SubagentRelationship{{Agent: "leaf", Immediate: &ImmediateConfig{}}}}, SideB: SideConfig{Human: true}})
	f.reg.AddAgent(AgentDefinition{Name: "root", Env: map[string]string{"FLAG_ON": "Yes", "LABEL": "Compactor"},
		SideA: SideConfig{Prompt: "p", Subagents: []SubagentRelationship{
			{Agent: "leaf", As: "compactor", Hidden: true},
			{Agent: "leaf", As: "gated_off", Optional: "FLAG_OFF"},
			{Agent: "leaf", As: "gated_on", Optional: "FLAG_ON"},
			{Agent: "mid", As: "boot", Immediate: &ImmediateConfig{NameEnv: "LABEL"}},
		}}, SideB: SideConfig{Human: true}})
	f.start(Config{})
	s := f.thread("root")
	ch, _ := s.Children(context.Background())
	if len(ch) != 1 || ch[0].Relationship != "boot" || ch[0].ThreadName != "Compactor" {
		t.Fatalf("immediate spawn = %+v", ch)
	}
	mid, _ := s.GetChildThread(context.Background(), ch[0].Reference)
	if gc, _ := mid.Children(context.Background()); len(gc) != 1 {
		t.Fatal("immediate spawning must be recursive")
	}
	f.send(s, "hello")
	req := pp.RequestsCopy()[0]
	var enum []any
	for _, tl := range req.Tools {
		if tl.Name == ToolSubagentCreate {
			enum = tl.Parameters["properties"].(map[string]any)["agent"].(map[string]any)["enum"].([]any)
		}
	}
	got := []string{}
	for _, e := range enum {
		got = append(got, e.(string))
	}
	if strings.Join(got, ",") != "gated_on,boot" {
		t.Fatalf("model-visible relationships = %v", got)
	}
	// Hidden relationship remains spawnable from code.
	r := s.SpawnSubagent(context.Background(), "compactor", map[string]any{"name": "c"})
	if r.Status != ToolStatusSuccess {
		t.Fatalf("hidden spawn: %+v", r)
	}
	if r := s.SpawnSubagent(context.Background(), "gated_off", map[string]any{"name": "c"}); r.Status != ToolStatusError {
		t.Fatal("disabled optional relationship must be rejected")
	}
}

func TestParentHookCompositionOrderAndFailClosed(t *testing.T) {
	f := newFixture(t)
	f.model("m", text("ok"))
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m", Hooks: []string{"own"}})
	var order []string
	mk := func(id string) Hook {
		return Hook{ID: id, Name: HookFilterMessages, FilterMessages: func(ctx context.Context, s *ThreadState, m []Message) ([]Message, error) {
			order = append(order, id)
			return m, nil
		}}
	}
	f.reg.AddHook("", mk("own"))
	f.reg.AddHook("pkgx", mk("before"))
	f.reg.AddHook("pkgx", mk("after"))
	f.reg.AddAgent(AgentDefinition{Name: "kid", PackageName: "pkgx", ExposeAsTool: true,
		ParentHooks: []ParentHookRequirement{{Hook: "after"}, {Hook: "before", Stage: "before_parent"}},
		SideA:       SideConfig{Prompt: "p"}, SideB: SideConfig{Human: true}})
	f.reg.AddAgent(AgentDefinition{Name: "par", SideA: SideConfig{Prompt: "p", Subagents: []SubagentRelationship{{Agent: "kid", Hidden: true}}}, SideB: SideConfig{Human: true}})
	f.start(Config{})
	s := f.thread("par")
	f.send(s, "hi")
	if strings.Join(order, ",") != "before,own,after" {
		t.Fatalf("hook order = %v", order)
	}

	// Missing required hook fails closed at validation and creation.
	f.reg.AddAgent(AgentDefinition{Name: "badkid", ExposeAsTool: true, ParentHooks: []ParentHookRequirement{{Hook: "nope"}}, SideA: SideConfig{Prompt: "p"}, SideB: SideConfig{Human: true}})
	f.reg.AddAgent(AgentDefinition{Name: "badpar", SideA: SideConfig{Prompt: "p", Subagents: []SubagentRelationship{{Agent: "badkid"}}}, SideB: SideConfig{Human: true}})
	if _, err := f.rt.CreateThread(context.Background(), CreateThreadOptions{AgentID: "badpar"}); err == nil {
		t.Fatal("composition with unloadable required hook must fail closed")
	}
}

// ---- termination ----

func TestTerminate(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	p := f.model("m")
	p.Fallback = func(req ProviderRequest) (*ProviderResponse, error) {
		<-release
		r := text("late")
		return &r, nil
	}
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	f.reg.AddAgent(AgentDefinition{Name: "kid", SideA: SideConfig{Prompt: "p"}, SideB: SideConfig{Human: true}})
	f.reg.AddAgent(AgentDefinition{Name: "par", SideA: SideConfig{Prompt: "p", Subagents: []SubagentRelationship{{Agent: "kid", Immediate: &ImmediateConfig{}}}}, SideB: SideConfig{Human: true}})
	f.start(Config{})
	s := f.thread("par")
	ch, _ := s.Children(context.Background())
	kid, _ := s.GetChildThread(context.Background(), ch[0].Reference)
	_ = f.rt.SendMessage(context.Background(), kid.ThreadID(), MessageInput{Role: RoleUser, Content: "work"})
	waitFor(t, func() bool { return status(t, kid).State == RunRunning })
	if err := kid.Terminate(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(release)
	f.wait(kid)
	if err := kid.QueueMessage(context.Background(), MessageInput{Role: RoleUser, Content: "x"}); !errors.Is(err, ErrTerminated) {
		t.Fatalf("queue after terminate = %v", err)
	}
	if err := f.rt.SendMessage(context.Background(), kid.ThreadID(), MessageInput{Content: "x"}); !errors.Is(err, ErrTerminated) {
		t.Fatalf("send after terminate = %v", err)
	}
	if term, _ := kid.Terminated(context.Background()); term == nil {
		t.Fatal("terminated timestamp not set")
	}
	ch, _ = s.Children(context.Background())
	if ch[0].Status != ChildTerminated {
		t.Fatalf("parent registry status = %s", ch[0].Status)
	}
	if st := status(t, kid); st.State == RunRunning {
		t.Fatal("thread left running after abort")
	}
}

// ---- KV store ----

func TestKeyValueStore(t *testing.T) {
	f := newFixture(t)
	f.model("m")
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	f.reg.AddAgent(AgentDefinition{Name: "kid", SideA: SideConfig{Prompt: "p"}, SideB: SideConfig{Human: true}})
	f.reg.AddAgent(AgentDefinition{Name: "ag", SideA: SideConfig{Prompt: "p", Subagents: []SubagentRelationship{{Agent: "kid", Immediate: &ImmediateConfig{}}}}, SideB: SideConfig{Human: true}})
	f.start(Config{MaxKeys: 2, MaxValueBytes: 64})
	ctx := context.Background()
	s := f.thread("ag")
	if v, err := s.GetValue(ctx, "missing"); v != nil || err != nil {
		t.Fatalf("unset = %v %v", v, err)
	}
	if err := s.SetValue(ctx, "k", map[string]any{"a": []any{1.0, "x", true, nil}}); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetValue(ctx, "k")
	if b, _ := json.Marshal(v); string(b) != `{"a":[1,"x",true,null]}` {
		t.Fatalf("round trip = %s", b)
	}
	ch, _ := s.Children(ctx)
	kid, _ := s.GetChildThread(ctx, ch[0].Reference)
	if v, _ := kid.GetValue(ctx, "k"); v != nil {
		t.Fatal("KV must be isolated per thread")
	}
	if err := s.SetValue(ctx, "k", nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetValue(ctx, "k"); v != nil {
		t.Fatal("setValue(nil) must delete")
	}
	if err := s.SetValue(ctx, "big", strings.Repeat("x", 100)); err == nil || !strings.Contains(err.Error(), "maxValueBytes") {
		t.Fatalf("value cap err = %v", err)
	}
	_ = s.SetValue(ctx, "a", 1)
	_ = s.SetValue(ctx, "b", 2)
	if err := s.SetValue(ctx, "c", 3); err == nil || !strings.Contains(err.Error(), "maxKeys") {
		t.Fatalf("key cap err = %v", err)
	}
	if err := s.SetValue(ctx, "f", func() {}); err == nil {
		t.Fatal("non-JSON value must be rejected")
	}
}

// ---- durability ----

func TestFileStoreDurabilityAndRecovery(t *testing.T) {
	dir := t.TempDir()
	key := []byte(strings.Repeat("k", 32))
	fs, err := OpenFileStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rec := ThreadRecord{ID: "t1", AgentID: "ag", CreatedAt: 1, Env: map[string]EnvValue{"API": {Value: "s3cr3t", Type: "secret"}},
		Status: ThreadStatus{State: RunRunning, Since: 5}}
	if err := fs.PutThread(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetValue(ctx, "t1", "k", json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := fs.Enqueue(ctx, "t1", QueueItem{ID: "q1", Kind: QueueMessage, Message: &MessageInput{Role: RoleUser, Content: "pending"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "t1.json"))
	if strings.Contains(string(raw), "s3cr3t") {
		t.Fatal("secret env stored in plaintext")
	}

	// "Restart": reopen and run recovery.
	fs2, err := OpenFileStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := fs2.GetThread(ctx, "t1")
	if got.Env["API"].Value != "s3cr3t" {
		t.Fatal("secret did not round-trip")
	}
	if v, ok, _ := fs2.GetValue(ctx, "t1", "k"); !ok || string(v) != `{"x":1}` {
		t.Fatal("KV value not durable")
	}
	f := newFixture(t)
	f.store = fs2
	f.model("m", text("resumed"))
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	assistantAgent(f, "ag", "p", SideConfig{})
	f.rt = New(f.reg, f.store, Config{})
	t.Cleanup(f.rt.Close)
	// Before Start, the stale running status is visible; Start settles it
	// and resumes the durable queue.
	if err := f.rt.Start(ctx); err != nil {
		t.Fatal(err)
	}
	s, _ := f.rt.Thread(ctx, "t1")
	f.wait(s)
	ms := msgs(t, s)
	if len(ms) != 2 || ms[1].ContentString() != "resumed" {
		t.Fatalf("queued work not resumed: %+v", ms)
	}
}

func TestExecutionLostSettlesFailed(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	_ = st.PutThread(ctx, ThreadRecord{ID: "x", AgentID: "ag", Status: ThreadStatus{State: RunRunning, Since: 1}})
	f := newFixture(t)
	f.store = st
	f.model("m")
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	assistantAgent(f, "ag", "p", SideConfig{})
	f.start(Config{})
	s, _ := f.rt.Thread(ctx, "x")
	if got := status(t, s); got.State != RunFailed || deref(got.Reason) != ReasonExecutionLost {
		t.Fatalf("status = %+v", got)
	}
}

// ---- definitions ----

func TestDefinitionValidation(t *testing.T) {
	r := NewRegistry()
	r.AddModel(ModelDefinition{Name: "m", Provider: "p"})
	r.AddPrompt(PromptDefinition{Name: "p", Model: "m", Tools: []PromptToolRef{{Name: "lookup"}}})
	r.AddTool(ToolDefinition{Name: "lookup"})
	r.AddAgent(AgentDefinition{Name: "c", SideA: SideConfig{Prompt: "p"}, SideB: SideConfig{Human: true}})
	r.AddAgent(AgentDefinition{Name: "bad",
		SideA: SideConfig{Prompt: "p", Subagents: []SubagentRelationship{{Agent: "c", As: "subagents"}, {Agent: "c", As: "lookup"}, {Agent: "c", As: "dup"}}},
		SideB: SideConfig{Handler: "nope", MaxSteps: 2, Subagents: []SubagentRelationship{{Agent: "c", As: "dup"}}}})
	r.AddAgent(AgentDefinition{Name: "humans", SideA: SideConfig{Human: true, Subagents: []SubagentRelationship{{Agent: "c"}}}, SideB: SideConfig{Human: true}})
	r.AddAgent(AgentDefinition{Name: "zero", SideA: SideConfig{}, SideB: SideConfig{Prompt: "p", Handler: "x"}})
	codes := map[string]bool{}
	for _, i := range r.Validate() {
		codes[i.Code] = true
	}
	for _, c := range []string{CodeReservedRelationship, CodeRelationshipToolClash, CodeRelationshipBothSides, CodeUndefinedHandler,
		CodeNonPromptSideBinding, CodeBothSidesHuman, CodeSubagentsOnHumanSide, CodeInvalidSideBacking} {
		if !codes[c] {
			t.Errorf("missing validation code %s (got %v)", c, codes)
		}
	}
}

// ---- decide ----

type fakeDecider struct{}

func (fakeDecider) Name() string { return "fake" }
func (fakeDecider) Decide(ctx context.Context, req DecisionRequest) (*DecisionResult, error) {
	p := 0.73
	return &DecisionResult{Model: req.Model + "@v1", Answers: map[string]DecisionAnswer{
		"lang": {Type: "choice", Choice: "go", Probabilities: map[string]float64{"go": 0.9, "rust": 0.1}},
		"ok":   {Type: "boolean", Probability: &p},
	}}, nil
}

func TestDecideAtRestDoesNotTouchMessages(t *testing.T) {
	f := newFixture(t)
	f.model("m")
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	assistantAgent(f, "ag", "p", SideConfig{})
	f.start(Config{})
	s := f.thread("ag")
	if s.Execution() != nil {
		t.Fatal("expected at-rest handle")
	}
	res, err := s.Decide(context.Background(), DecideOptions{
		Provider: func(ProviderFactoryConfig) DecisionProvider { return fakeDecider{} },
		DecisionRequest: DecisionRequest{Model: "d", Context: map[string]any{"code": "package main"}, Questions: map[string]DecisionQuestion{
			"lang": {Type: "choice", Instructions: "Which language?", Options: map[string]DecisionContext{"go": nil, "rust": "Rust"}},
			"ok":   {Type: "boolean", Instructions: "Does it compile?"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if *res.Answers["ok"].Probability != 0.73 || res.Answers["lang"].Choice != "go" {
		t.Fatalf("answers = %+v", res.Answers)
	}
	if len(msgs(t, s)) != 0 {
		t.Fatal("decide must not add messages")
	}
	if st := status(t, s); st.Since != 0 {
		t.Fatal("decide must not start a turn")
	}
}

// ---- prompt tools, handoff, handlers, effects, uses ----

func TestPromptToolHandoffAndUses(t *testing.T) {
	f := newFixture(t)
	f.model("sub", text("summary!"))
	main := f.model("m", tools(call("summarize", map[string]any{"text": "long"})), tools(call("support", nil)))
	sup := f.model("sm", text("support here"))
	f.reg.AddPrompt(PromptDefinition{Name: "summarize", Model: "sub", ToolDescription: "summarize",
		RequiredSchema: map[string]any{"type": "object", "required": []any{"text"}}})
	f.reg.AddPrompt(PromptDefinition{Name: "support_p", Model: "sm"})
	f.reg.AddAgent(AgentDefinition{Name: "support", ExposeAsTool: true, SideA: SideConfig{Prompt: "support_p"}, SideB: SideConfig{Human: true}})
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m", Tools: []PromptToolRef{{Name: "summarize", InitUserMessageProperty: "text"}, {Name: "support"}}})
	assistantAgent(f, "triage", "p", SideConfig{})
	f.reg.AddTool(ToolDefinition{Name: "restricted", Uses: []string{}, Execute: func(ctx context.Context, s *ThreadState, a map[string]any) (ToolResult, error) {
		r := s.InvokeTool(ctx, "summarize", map[string]any{"text": "x"})
		if r.Status != ToolStatusError {
			return ToolResult{}, errors.New("uses restriction not enforced")
		}
		if err := s.QueueTool(ctx, "summarize", nil); err == nil {
			return ToolResult{}, errors.New("queueTool uses restriction not enforced")
		}
		return ToolResult{Result: "ok"}, nil
	}})
	f.start(Config{})
	s := f.thread("triage")
	f.send(s, "help")
	var subResult string
	for _, m := range msgs(t, s) {
		if m.Role == RoleTool && deref(m.Name) == "summarize" {
			subResult = m.ContentString()
		}
	}
	if subResult != "summary!" {
		t.Fatalf("prompt tool result = %q", subResult)
	}
	if got := f.prov["sub"].RequestsCopy()[0].Messages; got[len(got)-1].Content != "long" {
		t.Fatalf("subprompt init message = %+v", got)
	}
	if id, _ := s.AgentID(context.Background()); id != "support" {
		t.Fatalf("handoff agent = %s", id)
	}
	if len(sup.RequestsCopy()) != 1 || len(main.RequestsCopy()) != 2 {
		t.Fatal("handoff did not transfer the turn")
	}
	// Top-level context excludes sub-prompt internals.
	for _, m := range sup.RequestsCopy()[0].Messages {
		if m.Content == "summary!" && m.Role == RoleAssistant {
			t.Fatal("sub-prompt messages leaked into top-level context")
		}
	}
	tool := f.reg.Tool("restricted")
	if r, _ := f.rt.runTool(context.Background(), s, &resolvedTool{name: tool.Name, kind: kindFunction, fn: tool, schema: objectSchema()}, map[string]any{}, nil); r.Status != ToolStatusSuccess {
		t.Fatalf("uses check: %+v", r)
	}
}

func TestHandlerSideAndEffects(t *testing.T) {
	f := newFixture(t)
	var turns int32
	f.reg.AddHandler(HandlerDefinition{Name: "echo", Execute: func(ctx context.Context, s *ThreadState, turn HandlerTurn) error {
		atomic.AddInt32(&turns, 1)
		if s.Execution() == nil {
			return errors.New("execution must be present in handler")
		}
		for _, m := range turn.Inbound {
			if _, err := s.InjectMessage(ctx, MessageInput{Role: RoleAssistant, Content: "echo: " + m.ContentString()}); err != nil {
				return err
			}
		}
		return nil
	}})
	fired := make(chan struct{}, 1)
	f.reg.AddEffect(EffectDefinition{Name: "ping", Execute: func(ctx context.Context, s *ThreadState, args map[string]any) error {
		fired <- struct{}{}
		return s.QueueMessage(ctx, MessageInput{Role: RoleUser, Content: "from effect " + args["n"].(string)})
	}})
	f.reg.AddAgent(AgentDefinition{Name: "relay", SideA: SideConfig{Handler: "echo"}, SideB: SideConfig{Human: true}})
	f.start(Config{})
	s := f.thread("relay")
	f.send(s, "one")
	ms := msgs(t, s)
	if len(ms) != 2 || ms[1].ContentString() != "echo: one" {
		t.Fatalf("handler output = %+v", ms)
	}
	if _, err := s.ScheduleEffect(context.Background(), "ping", map[string]any{"n": "7"}, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("effect did not fire")
	}
	waitFor(t, func() bool {
		ms := msgs(t, s)
		return len(ms) == 4 && ms[3].ContentString() == "echo: from effect 7"
	})
}

func TestInjectAtRestAndLifecycleHooks(t *testing.T) {
	f := newFixture(t)
	var before, after int32
	f.reg.AddHook("", Hook{ID: "b", Name: HookBeforeCreateMessage, BeforeCreateMessage: func(ctx context.Context, s *ThreadState, m Message) (Message, error) {
		atomic.AddInt32(&before, 1)
		m.Metadata["tagged"] = true
		return m, nil
	}})
	f.reg.AddHook("", Hook{ID: "a", Name: HookAfterCreateMessage, AfterCreateMessage: func(ctx context.Context, s *ThreadState, m Message) error {
		atomic.AddInt32(&after, 1)
		return errors.New("ignored")
	}})
	f.model("m")
	f.reg.AddPrompt(PromptDefinition{Name: "p", Model: "m"})
	f.reg.AddAgent(AgentDefinition{Name: "ag", Hooks: []string{"b", "a"}, SideA: SideConfig{Prompt: "p"}, SideB: SideConfig{Human: true}})
	f.start(Config{})
	s := f.thread("ag")
	m, err := s.InjectMessage(context.Background(), MessageInput{Role: RoleUser, Content: "quiet", Silent: true})
	if err != nil {
		t.Fatal(err)
	}
	if before != 1 || after != 1 || m.Metadata["tagged"] != true {
		t.Fatalf("hooks before=%d after=%d meta=%v", before, after, m.Metadata)
	}
	if got, _ := s.GetMessage(context.Background(), m.ID); got == nil {
		t.Fatal("injected message not readable")
	}
	vis, _ := s.GetMessages(context.Background(), GetMessagesOptions{})
	if vis.Total != 0 {
		t.Fatal("silent message must be hidden without includeSilent")
	}
}

func TestBootstrapEndpoint(t *testing.T) {
	f := newFixture(t)
	f.model("parent", tools(call(ToolSubagentCreate, map[string]any{"agent": "cal", "name": "c"})), text("need creds"))
	f.model("child")
	f.reg.AddTool(ToolDefinition{Name: "cal_tool", Variables: []VariableDefinition{{Name: "CAL_ID", Type: "secret", Required: true, Scoped: true}},
		Execute: func(ctx context.Context, s *ThreadState, a map[string]any) (ToolResult, error) {
			return ToolResult{}, nil
		}})
	f.reg.AddPrompt(PromptDefinition{Name: "cp", Model: "child", Tools: []PromptToolRef{{Name: "cal_tool"}}})
	f.reg.AddPrompt(PromptDefinition{Name: "pp", Model: "parent"})
	f.reg.AddAgent(AgentDefinition{Name: "cal", SideA: SideConfig{Prompt: "cp"}, SideB: SideConfig{Human: true}})
	f.reg.AddAgent(AgentDefinition{Name: "boss", SideA: SideConfig{Prompt: "pp", Subagents: []SubagentRelationship{{Agent: "cal", ScopedEnv: []string{"CAL_ID"}}}}, SideB: SideConfig{Human: true}})
	f.start(Config{})
	uid := "u1"
	s, _ := f.rt.CreateThread(context.Background(), CreateThreadOptions{AgentID: "boss", UserID: &uid})
	f.send(s, "go")
	var reqID string
	for _, m := range msgs(t, s) {
		if strings.Contains(m.ContentString(), "subagent_env_required") {
			var body map[string]any
			_ = json.Unmarshal([]byte(strings.TrimPrefix(m.ContentString(), "Error: ")), &body)
			reqID, _ = body["request_id"].(string)
		}
	}
	if reqID == "" {
		t.Fatal("no bootstrap request id")
	}
	srv := httptest.NewServer(f.rt.BootstrapHandler("/api", func(r *http.Request) (string, bool) {
		u := r.Header.Get("X-User")
		return u, u != ""
	}))
	defer srv.Close()
	url := srv.URL + "/api/threads/" + s.ThreadID() + "/variables/" + reqID
	do := func(method, user, body string) int {
		req, _ := http.NewRequest(method, url, strings.NewReader(body))
		if user != "" {
			req.Header.Set("X-User", user)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := do("GET", "", ""); c != 401 {
		t.Fatalf("unauthenticated = %d", c)
	}
	if c := do("GET", "intruder", ""); c != 403 {
		t.Fatalf("wrong owner = %d", c)
	}
	if c := do("GET", "u1", ""); c != 200 {
		t.Fatalf("get = %d", c)
	}
	if c := do("POST", "u1", `{"values":{}}`); c != 400 {
		t.Fatalf("incomplete post = %d", c)
	}
	if c := do("POST", "u1", `{"values":{"CAL_ID":"cal-123"}}`); c != 200 {
		t.Fatalf("post = %d", c)
	}
	ch, _ := s.Children(context.Background())
	if len(ch) != 1 {
		t.Fatal("deferred subagent not booted")
	}
	child, _ := s.GetChildThread(context.Background(), ch[0].Reference)
	if v, err := child.Env(context.Background(), "CAL_ID"); err != nil || v != "cal-123" {
		t.Fatalf("scoped env = %q %v", v, err)
	}
	if v, _ := s.Env(context.Background(), "CAL_ID"); v != "" {
		t.Fatal("scoped value must not leak to parent")
	}
}
