package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"runtime/debug"
)

type toolKind int

const (
	kindFunction toolKind = iota
	kindPrompt
	kindHandoff
	kindLifecycle
	kindSessionStop
	kindSessionFail
	kindSessionStatus
	kindStopOnly // synthesized stopTool with no implementation
)

// resolvedTool is one entry of a tool surface.
type resolvedTool struct {
	name        string
	description string
	schema      map[string]any
	kind        toolKind
	fn          *ToolDefinition
	prompt      *PromptDefinition
	promptRef   PromptToolRef
	agent       *AgentDefinition
	binding     *SessionToolBinding
	ownerSide   Side                    // lifecycle tools: the owning side
	rels        []*SubagentRelationship // lifecycle tools: reachable relationships
	isStop      bool
	hidden      bool // never presented to a model
}

type toolSurface struct {
	tools  []*resolvedTool
	byName map[string]*resolvedTool
}

func newSurface() *toolSurface { return &toolSurface{byName: map[string]*resolvedTool{}} }

func (ts *toolSurface) add(t *resolvedTool) {
	if _, dup := ts.byName[t.name]; dup {
		return
	}
	ts.tools = append(ts.tools, t)
	ts.byName[t.name] = t
}

// specs returns the model-visible tool list; hidden tools never appear.
func (ts *toolSurface) specs() []ToolSpec {
	var out []ToolSpec
	for _, t := range ts.tools {
		if t.hidden {
			continue
		}
		out = append(out, ToolSpec{Name: t.name, Description: t.description, Parameters: t.schema})
	}
	return out
}

func objectSchema() map[string]any { return map[string]any{"type": "object"} }

// resolveRef resolves a prompt `tools` entry: function tool, prompt tool, or
// agent handoff.
func (rt *Runtime) resolveRef(ref PromptToolRef) (*resolvedTool, error) {
	if t := rt.reg.Tool(ref.Name); t != nil {
		schema := t.Args
		if schema == nil {
			schema = objectSchema()
		}
		return &resolvedTool{name: t.Name, description: t.Description, schema: schema, kind: kindFunction, fn: t}, nil
	}
	if p := rt.reg.Prompt(ref.Name); p != nil {
		schema := p.RequiredSchema
		if schema == nil {
			schema = objectSchema()
		}
		return &resolvedTool{name: p.Name, description: p.ToolDescription, schema: schema, kind: kindPrompt, prompt: p, promptRef: ref}, nil
	}
	if a := rt.reg.Agent(ref.Name); a != nil {
		if !a.ExposeAsTool {
			return nil, errf("agent %q is not exposed as a tool", a.Name)
		}
		if _, ok := a.humanSide(); !ok {
			return nil, errf("handoff target %q has no human side", a.Name)
		}
		return &resolvedTool{name: unqualified(a.Name), description: a.ToolDescription, schema: objectSchema(), kind: kindHandoff, agent: a}, nil
	}
	return nil, errf("tool %q is not defined", ref.Name)
}

// toolSurface builds the tool list for a prompt on a side. topLevel adds the
// side's session bindings, stop tool, and injected lifecycle tools.
func (rt *Runtime) toolSurface(ctx context.Context, s *ThreadState, agent *AgentDefinition, side Side, prompt *PromptDefinition, topLevel bool) (*toolSurface, error) {
	ts := newSurface()
	for _, ref := range prompt.Tools {
		t, err := rt.resolveRef(ref)
		if err != nil {
			return nil, err
		}
		ts.add(t)
	}
	if !topLevel || agent == nil {
		return ts, nil
	}
	cfg := agent.Side(side)
	bind := func(b *SessionToolBinding, kind toolKind, desc string) {
		if b == nil {
			return
		}
		if t, ok := ts.byName[b.Name]; ok && t.kind == kindFunction {
			t.binding, t.kind = b, kind
			return
		}
		props := map[string]any{}
		if b.MessageProperty != "" {
			props[b.MessageProperty] = map[string]any{"type": "string"}
		}
		if b.AttachmentsProperty != "" {
			props[b.AttachmentsProperty] = map[string]any{"type": []any{"array", "string"}, "items": map[string]any{"type": "string"}}
		}
		ts.add(&resolvedTool{name: b.Name, description: desc, kind: kind, binding: b,
			schema: map[string]any{"type": "object", "properties": props}})
	}
	bind(cfg.SessionStop, kindSessionStop, "End the session successfully and return the result.")
	bind(cfg.SessionFail, kindSessionFail, "End the session with a failure.")
	bind(cfg.SessionStatus, kindSessionStatus, "Publish a status update without ending the session.")
	if cfg.StopTool != "" {
		if t, ok := ts.byName[cfg.StopTool]; ok {
			t.isStop = true
		} else {
			ts.add(&resolvedTool{name: cfg.StopTool, description: "End your turn.", schema: objectSchema(), kind: kindStopOnly, isStop: true})
		}
	}
	for _, lt := range rt.lifecycleTools(ctx, s, agent, side, false) {
		ts.add(lt)
	}
	return ts, nil
}

// resolveCodeTool resolves a tool for invokeTool/queueTool from code. Code
// may reach lifecycle tools (including hidden relationships) and any
// registered tool, prompt, or exposed agent by name.
func (rt *Runtime) resolveCodeTool(ctx context.Context, s *ThreadState, rec *ThreadRecord, side Side, name string) (*resolvedTool, error) {
	agent := rt.reg.Agent(rec.AgentID)
	if agent == nil {
		return nil, errf("agent %q not defined", rec.AgentID)
	}
	if isReservedLifecycleName(name) {
		// Prefer the current side; fall back to the other side so handler
		// and hook code can reach relationships declared on either side.
		for _, sd := range []Side{side, side.Other()} {
			for _, lt := range rt.lifecycleTools(ctx, s, agent, sd, true) {
				if lt.name == name {
					return lt, nil
				}
			}
		}
		return nil, errf("no enabled subagent relationships for %q", name)
	}
	return rt.resolveRef(PromptToolRef{Name: name})
}

// toolEffects carries control-flow effects of a tool call.
type toolEffects struct {
	sessionStop *lifecyclePayload
	sessionFail *lifecyclePayload
	stopTool    bool
	handoff     string
}

type lifecyclePayload struct {
	Result      string
	Attachments []AttachmentRef
}

// callCtx describes a model-issued tool call being executed.
type callCtx struct {
	side   Side
	asst   Message
	depth  int
	runID  string
	call   ToolCall
	queued bool
}

// toolOutcome aggregates a batch of executed tool calls.
type toolOutcome struct {
	toolEffects
	errors []string
	calls  []ToolCall
}

// executeToolCalls runs tool calls sequentially in the returned order
// (Runtime §5.1). Tool errors are persisted as tool results and never crash
// the loop (§5.2); only persistence failures return an error.
func (rt *Runtime) executeToolCalls(ctx context.Context, s *ThreadState, side Side, surface *toolSurface, calls []ToolCall, asst Message, depth int, runID string) (toolOutcome, error) {
	var out toolOutcome
	for _, tc := range calls {
		out.calls = append(out.calls, tc)
		tool := surface.byName[tc.Function.Name]
		var args map[string]any
		var res ToolResult
		var eff toolEffects
		argErr := ""
		if tc.Function.Arguments != "" {
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
				argErr = "invalid JSON arguments: " + err.Error()
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		rt.fireToolStarted(ctx, s, tool, tc, args)
		switch {
		case tool == nil:
			res = ToolResult{Status: ToolStatusError, Error: fmt.Sprintf("unknown tool %q", tc.Function.Name)}
		case argErr != "":
			res = ToolResult{Status: ToolStatusError, Error: argErr}
		default:
			if err := validateSchema(tool.schema, any(args)); err != nil {
				res = ToolResult{Status: ToolStatusError, Error: "invalid arguments: " + err.Error()}
			} else {
				res, eff = rt.runTool(ctx, s, tool, args, &callCtx{side: side, asst: asst, depth: depth, runID: runID, call: tc})
			}
		}
		if tool != nil && tool.isStop {
			eff.stopTool = true
		}
		mergeEffects(&out.toolEffects, eff)
		if _, err := rt.storeToolResult(ctx, s, side, tool, tc, res, depth, asst, runID); err != nil {
			return out, err
		}
		if res.Status == ToolStatusError {
			out.errors = append(out.errors, res.Error)
		}
	}
	return out, nil
}

func mergeEffects(dst *toolEffects, e toolEffects) {
	if e.sessionStop != nil && dst.sessionStop == nil {
		dst.sessionStop = e.sessionStop
	}
	if e.sessionFail != nil && dst.sessionFail == nil {
		dst.sessionFail = e.sessionFail
	}
	if e.stopTool {
		dst.stopTool = true
	}
	if e.handoff != "" && dst.handoff == "" {
		dst.handoff = e.handoff
	}
}

func (rt *Runtime) fireToolStarted(ctx context.Context, s *ThreadState, tool *resolvedTool, tc ToolCall, args map[string]any) {
	var progress *string
	if tool != nil && tool.fn != nil && tool.fn.ProgressArgument != "" {
		if v, ok := args[tool.fn.ProgressArgument].(string); ok {
			progress = &v
		}
	}
	hooks, err := rt.resolveHooks(ctx, s, HookToolCallStarted)
	if err == nil {
		for _, h := range hooks {
			rt.logHookErr(HookToolCallStarted, h.ID, h.ToolCallStarted(ctx, s, tc, progress))
		}
	}
	rt.emit(s.threadID, "tool_call_started", map[string]any{"id": tc.ID, "name": tc.Function.Name, "progress": progress})
}

// storeToolResult applies result hooks and persists the tool message.
func (rt *Runtime) storeToolResult(ctx context.Context, s *ThreadState, side Side, tool *resolvedTool, tc ToolCall, res ToolResult, depth int, asst Message, runID string) (Message, error) {
	if res.Status == "" {
		res.Status = ToolStatusSuccess
	}
	name := HookAfterToolCallSuccess
	if res.Status == ToolStatusError {
		name = HookAfterToolCallFailure
	}
	if hooks, err := rt.resolveHooks(ctx, s, name); err == nil {
		for _, h := range hooks {
			var repl *HookToolResult
			var herr error
			if name == HookAfterToolCallSuccess {
				repl, herr = h.AfterToolCallSuccess(ctx, s, tc, res)
			} else {
				repl, herr = h.AfterToolCallFailure(ctx, s, tc, res)
			}
			rt.logHookErr(name, h.ID, herr)
			if herr == nil && repl != nil {
				res = *repl
			}
		}
	} else {
		return Message{}, err
	}
	hooks, err := rt.resolveHooks(ctx, s, HookBeforeStoreToolRes)
	if err != nil {
		return Message{}, err
	}
	for _, h := range hooks {
		r2, herr := h.BeforeStoreToolRes(ctx, s, tc, res)
		rt.logHookErr(HookBeforeStoreToolRes, h.ID, herr)
		if herr == nil {
			res = r2
		}
	}
	if res.Status == "" {
		res.Status = ToolStatusSuccess
	}
	m := Message{
		Role:        RoleTool,
		Content:     ptr(resultContent(res)),
		Name:        ptr(tc.Function.Name),
		ToolCallID:  ptr(tc.ID),
		ToolStatus:  ptr(res.Status),
		Status:      ptr(MessageStatusCompleted),
		Attachments: res.Attachments,
		Depth:       depth,
		Metadata:    map[string]any{MetaSide: string(side)},
	}
	if depth > 0 {
		m.ParentID = asst.ParentID
	}
	if runID != "" {
		m.Metadata[MetaSubpromptRun] = runID
	}
	if v, ok := asst.Metadata[MetaQueuedTool]; ok {
		m.Metadata[MetaQueuedTool] = v
	}
	stored, err := s.createMessage(ctx, m)
	if err != nil {
		return Message{}, err
	}
	if dh, err := rt.resolveHooks(ctx, s, HookToolCallDone); err == nil {
		for _, h := range dh {
			rt.logHookErr(HookToolCallDone, h.ID, h.ToolCallDone(ctx, s, tc, res))
		}
	}
	rt.emit(s.threadID, "tool_call_done", map[string]any{"id": tc.ID, "name": tc.Function.Name, "status": res.Status})
	return stored, nil
}

func resultContent(r ToolResult) string {
	if r.Status == ToolStatusError {
		if r.Error != "" {
			return "Error: " + r.Error
		}
		if s, ok := r.Result.(string); ok {
			return "Error: " + s
		}
		return "Error: tool failed"
	}
	switch v := r.Result.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

// runTool executes a resolved tool. Panics are converted to error results.
// cc is nil for invokeTool (no persistence).
func (rt *Runtime) runTool(ctx context.Context, s *ThreadState, t *resolvedTool, args map[string]any, cc *callCtx) (res ToolResult, eff toolEffects) {
	defer func() {
		if p := recover(); p != nil {
			rt.cfg.Logger.Error("tool panic", "tool", t.name, "panic", p, "stack", string(debug.Stack()))
			res = ToolResult{Status: ToolStatusError, Error: fmt.Sprintf("tool %q panicked: %v", t.name, p)}
			eff = toolEffects{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return ToolResult{Status: ToolStatusError, Error: "aborted: " + err.Error()}, eff
	}
	callFn := func() ToolResult {
		r, err := t.fn.Execute(ctx, s.withTool(t.fn), args)
		if err != nil {
			return ToolResult{Status: ToolStatusError, Error: err.Error()}
		}
		if r.Status == "" {
			r.Status = ToolStatusSuccess
		}
		return r
	}
	switch t.kind {
	case kindFunction:
		if t.fn.Execute == nil {
			return ToolResult{Status: ToolStatusError, Error: "tool has no implementation"}, eff
		}
		return callFn(), eff
	case kindStopOnly:
		return ToolResult{Status: ToolStatusSuccess, Result: "Turn ended."}, eff
	case kindSessionStop, kindSessionFail, kindSessionStatus:
		if t.fn != nil && t.fn.Execute != nil {
			if r := callFn(); r.Status == ToolStatusError {
				return r, eff
			}
		}
		payload, err := rt.bindingPayload(ctx, s, t.binding, args)
		if err != nil {
			return ToolResult{Status: ToolStatusError, Error: err.Error()}, eff
		}
		switch t.kind {
		case kindSessionStop:
			eff.sessionStop = payload
			return ToolResult{Status: ToolStatusSuccess, Result: "Session completed."}, eff
		case kindSessionFail:
			eff.sessionFail = payload
			return ToolResult{Status: ToolStatusSuccess, Result: "Session failed."}, eff
		default:
			if err := s.SetStatus(ctx, payload.Result); err != nil {
				return ToolResult{Status: ToolStatusError, Error: err.Error()}, eff
			}
			return ToolResult{Status: ToolStatusSuccess, Result: "Status updated."}, eff
		}
	case kindHandoff:
		if cc == nil {
			// Programmatic handoff via invokeTool: apply immediately.
			if _, err := rt.updateThread(ctx, s.threadID, func(r *ThreadRecord) error {
				r.AgentID = t.agent.Name
				return nil
			}); err != nil {
				return ToolResult{Status: ToolStatusError, Error: err.Error()}, eff
			}
		}
		eff.handoff = t.agent.Name
		return ToolResult{Status: ToolStatusSuccess, Result: "Handed off to " + t.agent.Name + "."}, eff
	case kindPrompt:
		return rt.runSubprompt(ctx, s, t, args, cc), eff
	case kindLifecycle:
		return rt.runLifecycleTool(ctx, s, t, args), eff
	}
	return ToolResult{Status: ToolStatusError, Error: "unsupported tool kind"}, eff
}

// bindingPayload maps session binding args to a lifecycle payload.
func (rt *Runtime) bindingPayload(ctx context.Context, s *ThreadState, b *SessionToolBinding, args map[string]any) (*lifecyclePayload, error) {
	p := &lifecyclePayload{}
	if b.MessageProperty != "" {
		p.Result = stringify(args[b.MessageProperty])
	} else if len(args) > 0 {
		p.Result = stringify(args)
	}
	if b.AttachmentsProperty != "" {
		atts, err := rt.attachmentsFromPaths(ctx, s.threadID, args[b.AttachmentsProperty])
		if err != nil {
			return nil, err
		}
		p.Attachments = atts
	}
	return p, nil
}

func stringify(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// attachmentsFromPaths resolves a string or []string of paths into refs
// valid in the given thread.
func (rt *Runtime) attachmentsFromPaths(ctx context.Context, threadID string, v any) ([]AttachmentRef, error) {
	var paths []string
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		paths = []string{x}
	case []any:
		for _, e := range x {
			if s, ok := e.(string); ok {
				paths = append(paths, s)
			}
		}
	case []string:
		paths = x
	default:
		return nil, errf("attachments must be a path or list of paths")
	}
	var out []AttachmentRef
	for _, p := range paths {
		cp, err := cleanPath(p)
		if err != nil {
			return nil, err
		}
		_, f, err := rt.store.ReadFile(ctx, threadID, cp)
		if err != nil {
			return nil, errf("attachment %q: %v", p, err)
		}
		out = append(out, AttachmentRef{ID: newID(), Type: "file", Path: f.Path, Name: path.Base(f.Path), MimeType: f.MimeType, Size: f.Size})
	}
	return out, nil
}

// runQueuedTool executes a queueTool item: a synthetic assistant tool call
// is persisted, then the result, so history stays well-formed.
func (rt *Runtime) runQueuedTool(ctx context.Context, s *ThreadState, side Side, it QueueItem) (string, error) {
	rec, err := s.record(ctx)
	if err != nil {
		return "", err
	}
	tc := ToolCall{ID: "call_q-" + it.ID, Type: "function", Function: ToolCallFunction{Name: it.ToolName}}
	b, _ := json.Marshal(it.ToolArgs)
	tc.Function.Arguments = string(b)
	calls, _ := json.Marshal([]ToolCall{tc})
	asst, err := s.createMessageAck(ctx, Message{
		ID:        "q-" + it.ID,
		Role:      side.PerspectiveRole(),
		Content:   ptr(""),
		ToolCalls: ptr(string(calls)),
		Status:    ptr(MessageStatusCompleted),
		Metadata:  map[string]any{MetaSide: string(side), MetaQueuedTool: true},
	}, []string{it.ID})
	var rej *HookRejectedError
	if errors.As(err, &rej) {
		rt.cfg.Logger.Warn("queued tool call rejected by hook", "thread", s.threadID, "item", it.ID, "err", err)
		return "", rt.store.AckQueue(ctx, s.threadID, []string{it.ID})
	}
	if err != nil {
		return "", err
	}
	tool, rerr := rt.resolveCodeTool(ctx, s, rec, side, it.ToolName)
	args := it.ToolArgs
	if args == nil {
		args = map[string]any{}
	}
	rt.fireToolStarted(ctx, s, tool, tc, args)
	var res ToolResult
	var eff toolEffects
	if rerr != nil {
		res = ToolResult{Status: ToolStatusError, Error: rerr.Error()}
	} else if err := validateSchema(tool.schema, any(args)); err != nil {
		res = ToolResult{Status: ToolStatusError, Error: "invalid arguments: " + err.Error()}
	} else {
		res, eff = rt.runTool(ctx, s, tool, args, &callCtx{side: side, asst: asst, call: tc, queued: true})
	}
	if _, err := rt.storeToolResult(ctx, s, side, tool, tc, res, 0, asst, ""); err != nil {
		return "", err
	}
	return eff.handoff, nil
}

// runSubprompt runs a prompt tool as a nested prompt within the same thread.
func (rt *Runtime) runSubprompt(ctx context.Context, s *ThreadState, t *resolvedTool, args map[string]any, cc *callCtx) ToolResult {
	p := t.prompt
	ref := t.promptRef
	side := SideA
	depth := 1
	var parentID *string
	if cc != nil {
		side = cc.side
		depth = cc.depth + 1
		parentID = ptr(cc.asst.ID)
	} else if s.exec != nil {
		side = s.exec.CurrentSide()
	}
	model := rt.reg.Model(p.Model)
	if model == nil {
		return ToolResult{Status: ToolStatusError, Error: fmt.Sprintf("model %q not defined", p.Model)}
	}
	provider := rt.reg.Provider(model.Provider)
	if provider == nil {
		return ToolResult{Status: ToolStatusError, Error: fmt.Sprintf("provider %q not registered", model.Provider)}
	}
	runID := newID()
	content := stringify(args)
	if ref.InitUserMessageProperty != "" {
		content = stringify(args[ref.InitUserMessageProperty])
	}
	var atts []AttachmentRef
	if ref.InitAttachmentsProperty != "" {
		var err error
		if atts, err = rt.attachmentsFromPaths(ctx, s.threadID, args[ref.InitAttachmentsProperty]); err != nil {
			return ToolResult{Status: ToolStatusError, Error: err.Error()}
		}
	}
	// The invocation enters the sub-prompt from the opposite perspective.
	if _, err := s.createMessage(ctx, Message{
		Role: side.Other().PerspectiveRole(), Content: ptr(content), Attachments: atts,
		Status: ptr(MessageStatusCompleted), Depth: depth, ParentID: parentID,
		Metadata: map[string]any{MetaSide: string(side.Other()), MetaSubpromptRun: runID},
	}); err != nil {
		return ToolResult{Status: ToolStatusError, Error: err.Error()}
	}
	if s.exec != nil {
		s.exec.pushPrompt(p.Name)
		defer s.exec.popPrompt()
	}
	maxSteps := p.MaxSteps
	if maxSteps <= 0 {
		maxSteps = rt.cfg.MaxTotalSteps
	}
	var final string
	var allCalls []ToolCall
	var errs []string
	completed := false
	for i := 0; i < maxSteps; i++ {
		if ctx.Err() != nil {
			return ToolResult{Status: ToolStatusError, Error: "aborted"}
		}
		surface, err := rt.toolSurface(ctx, s, nil, side, p, false)
		if err != nil {
			return ToolResult{Status: ToolStatusError, Error: err.Error()}
		}
		req, err := rt.assembleContext(ctx, s, side, p, model, surface, contextScope{runID: runID})
		if err != nil {
			return ToolResult{Status: ToolStatusError, Error: err.Error()}
		}
		resp, err := provider.Generate(ctx, req)
		if err != nil {
			return ToolResult{Status: ToolStatusError, Error: "model call: " + err.Error()}
		}
		if s.exec != nil {
			s.exec.mu.Lock()
			s.exec.stepCount++
			s.exec.mu.Unlock()
		}
		asst, err := rt.persistAssistant(ctx, s, side, p, resp, depth, parentID, runID)
		if err != nil {
			return ToolResult{Status: ToolStatusError, Error: err.Error()}
		}
		final = resp.Content
		if len(resp.ToolCalls) == 0 {
			completed = true
			break
		}
		out, err := rt.executeToolCalls(ctx, s, side, surface, resp.ToolCalls, asst, depth, runID)
		if err != nil {
			return ToolResult{Status: ToolStatusError, Error: err.Error()}
		}
		allCalls = append(allCalls, out.calls...)
		errs = append(errs, out.errors...)
	}
	includeText := ref.IncludeTextResponse == nil || *ref.IncludeTextResponse
	includeCalls := ref.IncludeToolCalls != nil && *ref.IncludeToolCalls
	includeErrors := ref.IncludeErrors == nil || *ref.IncludeErrors
	if !includeCalls && (!includeErrors || len(errs) == 0) {
		r := ToolResult{Status: ToolStatusSuccess}
		if includeText {
			r.Result = final
		}
		if !completed {
			r.Status = ToolStatusError
			r.Error = "sub-prompt step limit reached"
		}
		return r
	}
	body := map[string]any{}
	if includeText {
		body["text"] = final
	}
	if includeCalls {
		body["toolCalls"] = allCalls
	}
	if includeErrors && len(errs) > 0 {
		body["errors"] = errs
	}
	r := ToolResult{Status: ToolStatusSuccess, Result: body}
	if !completed {
		r.Status = ToolStatusError
		r.Error = "sub-prompt step limit reached"
	}
	return r
}
