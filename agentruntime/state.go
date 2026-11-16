package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"time"
)

// ThreadState is the unified interface for thread operations (Threads §1.2).
// It is handed to tools, hooks, handlers, and effects, and is available at
// rest via Runtime.Thread. Execution() is non-nil only during execution.
type ThreadState struct {
	rt       *Runtime
	threadID string
	exec     *ExecutionState

	// callerTool is the tool currently executing through this handle, used
	// to enforce ToolDefinition.Uses.
	callerTool *ToolDefinition
	// preRecord is used only while a thread is being created.
	preRecord *ThreadRecord
	// atRestCtx is scratch storage for at-rest handles.
	atRestCtx map[string]any
}

func (s *ThreadState) withTool(t *ToolDefinition) *ThreadState {
	cp := *s
	cp.callerTool = t
	return &cp
}

func (s *ThreadState) record(ctx context.Context) (*ThreadRecord, error) {
	if s.preRecord != nil {
		r := cloneJSON(*s.preRecord)
		return &r, nil
	}
	return s.rt.store.GetThread(ctx, s.threadID)
}

// ---- Properties ----

// ThreadID returns the thread identifier.
func (s *ThreadState) ThreadID() string { return s.threadID }

// AgentID returns the owning agent name (it changes on handoff).
func (s *ThreadState) AgentID(ctx context.Context) (string, error) {
	r, err := s.record(ctx)
	if err != nil {
		return "", err
	}
	return r.AgentID, nil
}

// UserID returns the associated user or nil.
func (s *ThreadState) UserID(ctx context.Context) (*string, error) {
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	return r.UserID, nil
}

// CreatedAt returns the creation timestamp in microseconds.
func (s *ThreadState) CreatedAt(ctx context.Context) (int64, error) {
	r, err := s.record(ctx)
	if err != nil {
		return 0, err
	}
	return r.CreatedAt, nil
}

// Children returns the child subagent registry.
func (s *ThreadState) Children(ctx context.Context) ([]SubagentRegistryEntry, error) {
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	return r.Children, nil
}

// Terminated returns the termination timestamp or nil.
func (s *ThreadState) Terminated(ctx context.Context) (*int64, error) {
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	return r.Terminated, nil
}

// Arguments returns the persisted invocation arguments.
func (s *ThreadState) Arguments(ctx context.Context) (map[string]any, error) {
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	return copyMap(r.Arguments), nil
}

// Context returns mutable per-execution scratch storage.
func (s *ThreadState) Context() map[string]any {
	if s.exec != nil {
		return s.exec.scratch
	}
	if s.atRestCtx == nil {
		s.atRestCtx = map[string]any{}
	}
	return s.atRestCtx
}

// Execution returns the active execution state, or nil at rest.
func (s *ThreadState) Execution() *ExecutionState { return s.exec }

func (s *ThreadState) activePrompt() *PromptDefinition {
	if s.exec == nil {
		return nil
	}
	pp := s.exec.PromptPath()
	if len(pp) == 0 {
		return nil
	}
	return s.rt.reg.Prompt(pp[len(pp)-1])
}

// Status returns the authoritative run status.
func (s *ThreadState) Status(ctx context.Context) (ThreadStatus, error) {
	r, err := s.record(ctx)
	if err != nil {
		return ThreadStatus{}, err
	}
	return r.Status, nil
}

// User returns the thread user's profile, or nil for user-less threads.
func (s *ThreadState) User(ctx context.Context) (*ThreadUser, error) {
	r, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	if r.UserID == nil || s.rt.cfg.UserLookup == nil {
		return nil, nil
	}
	return s.rt.cfg.UserLookup(ctx, *r.UserID)
}

// ---- Messages ----

// GetMessages returns messages with pagination (Messages §2).
func (s *ThreadState) GetMessages(ctx context.Context, opts GetMessagesOptions) (MessagesResult, error) {
	all, err := s.rt.store.ListMessages(ctx, s.threadID)
	if err != nil {
		return MessagesResult{}, err
	}
	var filtered []Message
	for _, m := range all {
		if m.Silent && !opts.IncludeSilent {
			continue
		}
		if opts.MaxDepth != nil && m.Depth > *opts.MaxDepth {
			continue
		}
		filtered = append(filtered, m)
	}
	if opts.Order != "asc" {
		for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
			filtered[i], filtered[j] = filtered[j], filtered[i]
		}
	}
	total := len(filtered)
	start := min(opts.Offset, total)
	end := total
	if opts.Limit > 0 && start+opts.Limit < end {
		end = start + opts.Limit
	}
	return MessagesResult{Messages: filtered[start:end], Total: total, HasMore: end < total}, nil
}

// GetMessage returns one message or nil.
func (s *ThreadState) GetMessage(ctx context.Context, id string) (*Message, error) {
	all, err := s.rt.store.ListMessages(ctx, s.threadID)
	if err != nil {
		return nil, err
	}
	for _, m := range all {
		if m.ID == id {
			return &m, nil
		}
	}
	return nil, nil
}

// InjectMessage persists a message immediately (Messages §3). It runs the
// message lifecycle hooks and never restarts request-context hooks.
func (s *ThreadState) InjectMessage(ctx context.Context, in MessageInput) (Message, error) {
	rec, err := s.record(ctx)
	if err != nil {
		return Message{}, err
	}
	if rec.Terminated != nil {
		return Message{}, ErrTerminated
	}
	if in.Role == "" {
		return Message{}, errf("injectMessage: role is required")
	}
	return s.createMessage(ctx, messageFromInput(in))
}

// QueueMessage queues a message for delivery on the next step; if the
// thread is idle it forces a new turn (Messages §4, Runtime §9).
func (s *ThreadState) QueueMessage(ctx context.Context, in MessageInput) error {
	if in.Role == "" {
		return errf("queueMessage: role is required")
	}
	return s.rt.enqueue(ctx, s.threadID, QueueItem{Kind: QueueMessage, Message: &in})
}

// UpdateMessage updates an existing message (Messages §5). Updatable
// fields: content, metadata, status, silent, attachments.
func (s *ThreadState) UpdateMessage(ctx context.Context, id string, updates map[string]any) (Message, error) {
	cur, err := s.GetMessage(ctx, id)
	if err != nil {
		return Message{}, err
	}
	if cur == nil {
		return Message{}, ErrNotFound
	}
	hooks, err := s.rt.resolveHooks(ctx, s, HookBeforeUpdateMessage)
	if err != nil {
		return Message{}, err
	}
	for _, h := range hooks {
		updates, err = h.BeforeUpdateMessage(ctx, s, id, updates)
		if err != nil {
			return Message{}, errf("before_update_message hook %q: %v", h.ID, err)
		}
	}
	m := *cur
	for k, v := range updates {
		switch k {
		case "content":
			if v == nil {
				m.Content = nil
			} else if str, ok := v.(string); ok {
				m.Content = &str
			} else {
				return Message{}, errf("updateMessage: content must be a string")
			}
		case "metadata":
			md, ok := v.(map[string]any)
			if !ok {
				return Message{}, errf("updateMessage: metadata must be an object")
			}
			if m.Metadata == nil {
				m.Metadata = map[string]any{}
			}
			for mk, mv := range md {
				m.Metadata[mk] = mv
			}
		case "status":
			str, _ := v.(string)
			m.Status = &str
		case "silent":
			b, _ := v.(bool)
			m.Silent = b
		case "attachments":
			b, err := json.Marshal(v)
			if err != nil {
				return Message{}, err
			}
			var atts []AttachmentRef
			if err := json.Unmarshal(b, &atts); err != nil {
				return Message{}, errf("updateMessage: invalid attachments: %v", err)
			}
			m.Attachments = atts
		default:
			return Message{}, errf("updateMessage: field %q is not updatable", k)
		}
	}
	if err := s.rt.store.UpdateMessage(ctx, s.threadID, m); err != nil {
		return Message{}, err
	}
	s.rt.historyUpdate(s.threadID, m)
	after, err := s.rt.resolveHooks(ctx, s, HookAfterUpdateMessage)
	if err == nil {
		for _, h := range after {
			s.rt.logHookErr(HookAfterUpdateMessage, h.ID, h.AfterUpdateMessage(ctx, s, m))
		}
	}
	s.rt.emit(s.threadID, "message_data", m)
	return m, nil
}

// DeleteMessage deletes a message; tool results cascade with their parent
// assistant message (Messages §6).
func (s *ThreadState) DeleteMessage(ctx context.Context, id string) (bool, error) {
	all, err := s.rt.store.ListMessages(ctx, s.threadID)
	if err != nil {
		return false, err
	}
	var target *Message
	for i := range all {
		if all[i].ID == id {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return false, nil
	}
	ids := []string{id}
	if calls, _ := target.DecodeToolCalls(); len(calls) > 0 {
		callIDs := map[string]bool{}
		for _, c := range calls {
			callIDs[c.ID] = true
		}
		for _, m := range all {
			if m.Role == RoleTool && m.ToolCallID != nil && callIDs[*m.ToolCallID] {
				ids = append(ids, m.ID)
			}
		}
	}
	n, err := s.rt.store.DeleteMessages(ctx, s.threadID, ids)
	if err != nil {
		return false, err
	}
	s.rt.historyDelete(s.threadID, ids)
	return n > 0, nil
}

func messageFromInput(in MessageInput) Message {
	m := Message{
		Role:        in.Role,
		Content:     ptr(in.Content),
		Silent:      in.Silent,
		Metadata:    copyMap(in.Metadata),
		Attachments: in.Attachments,
		Status:      ptr(MessageStatusCompleted),
	}
	if in.SubagentID != "" {
		m.SubagentID = ptr(in.SubagentID)
	}
	if in.SubagentRelationship != "" {
		m.SubagentRelationship = ptr(in.SubagentRelationship)
	}
	if in.SubagentName != "" {
		m.SubagentName = ptr(in.SubagentName)
	}
	if in.SubagentStatus != "" {
		m.SubagentStatus = ptr(in.SubagentStatus)
	}
	return m
}

// createMessage is the single insertion path: before_create_message ->
// persist -> live history -> after_create_message.
func (s *ThreadState) createMessage(ctx context.Context, m Message) (Message, error) {
	return s.createMessageAck(ctx, m, nil)
}

// HookRejectedError reports that before_create_message refused a message.
type HookRejectedError struct {
	HookID string
	Err    error
}

func (e *HookRejectedError) Error() string {
	return fmt.Sprintf("agentruntime: before_create_message hook %q: %v", e.HookID, e.Err)
}
func (e *HookRejectedError) Unwrap() error { return e.Err }

// createMessageAck inserts a message and atomically acknowledges queue items.
func (s *ThreadState) createMessageAck(ctx context.Context, m Message, ackIDs []string) (Message, error) {
	if m.ID == "" {
		m.ID = newID()
	}
	if m.Metadata == nil {
		m.Metadata = map[string]any{}
	}
	hooks, err := s.rt.resolveHooks(ctx, s, HookBeforeCreateMessage)
	if err != nil {
		return Message{}, err
	}
	for _, h := range hooks {
		m, err = h.BeforeCreateMessage(ctx, s, m)
		if err != nil {
			return Message{}, &HookRejectedError{HookID: h.ID, Err: err}
		}
	}
	m.CreatedAt = nowMicros()
	if err := s.rt.store.InsertMessageAck(ctx, s.threadID, m, ackIDs); err != nil {
		return Message{}, err
	}
	s.rt.historyAppend(s.threadID, m)
	after, err := s.rt.resolveHooks(ctx, s, HookAfterCreateMessage)
	if err == nil {
		for _, h := range after {
			s.rt.logHookErr(HookAfterCreateMessage, h.ID, h.AfterCreateMessage(ctx, s, m))
		}
	}
	s.rt.emit(s.threadID, "message_data", m)
	return m, nil
}

// ---- File system (subset) ----

func cleanPath(p string) (string, error) {
	if p == "" {
		return "", errf("empty path")
	}
	c := path.Clean("/" + p)
	if c == "/" {
		return "", errf("invalid file path %q", p)
	}
	return c, nil
}

// WriteFile writes a file into the thread file system.
func (s *ThreadState) WriteFile(ctx context.Context, p string, data []byte, mimeType string) (FileRecord, error) {
	cp, err := cleanPath(p)
	if err != nil {
		return FileRecord{}, err
	}
	f := FileRecord{Path: cp, Name: path.Base(cp), MimeType: mimeType, Size: int64(len(data)), CreatedAt: nowMicros()}
	return f, s.rt.store.WriteFile(ctx, s.threadID, f, data)
}

// ReadFile reads a file; it returns nil, nil when the file does not exist.
func (s *ThreadState) ReadFile(ctx context.Context, p string) ([]byte, error) {
	cp, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	b, _, err := s.rt.store.ReadFile(ctx, s.threadID, cp)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return b, err
}

// StatFile returns file metadata or nil.
func (s *ThreadState) StatFile(ctx context.Context, p string) (*FileRecord, error) {
	cp, err := cleanPath(p)
	if err != nil {
		return nil, err
	}
	_, f, err := s.rt.store.ReadFile(ctx, s.threadID, cp)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return f, err
}

// UnlinkFile deletes a file.
func (s *ThreadState) UnlinkFile(ctx context.Context, p string) error {
	cp, err := cleanPath(p)
	if err != nil {
		return err
	}
	return s.rt.store.DeleteFile(ctx, s.threadID, cp)
}

// ListFiles lists all files.
func (s *ThreadState) ListFiles(ctx context.Context) ([]FileRecord, error) {
	return s.rt.store.ListFiles(ctx, s.threadID)
}

// ---- Resource loading ----

// LoadModel returns the named model definition or an error if it is not registered.
func (s *ThreadState) LoadModel(name string) (*ModelDefinition, error) {
	if m := s.rt.reg.Model(name); m != nil {
		return m, nil
	}
	return nil, errf("model %q not found", name)
}

// LoadPrompt returns the named prompt definition or an error if it is not registered.
func (s *ThreadState) LoadPrompt(name string) (*PromptDefinition, error) {
	if p := s.rt.reg.Prompt(name); p != nil {
		return p, nil
	}
	return nil, errf("prompt %q not found", name)
}

// LoadAgent returns the named agent definition or an error if it is not registered.
func (s *ThreadState) LoadAgent(name string) (*AgentDefinition, error) {
	if a := s.rt.reg.Agent(name); a != nil {
		return a, nil
	}
	return nil, errf("agent %q not found", name)
}

// GetPromptNames lists registered prompt names in sorted order.
func (s *ThreadState) GetPromptNames() []string { return s.rt.reg.PromptNames() }

// GetAgentNames lists registered agent names in sorted order.
func (s *ThreadState) GetAgentNames() []string { return s.rt.reg.AgentNames() }

// GetModelNames lists registered model names in sorted order.
func (s *ThreadState) GetModelNames() []string { return s.rt.reg.ModelNames() }

// ---- Environment ----

// ErrEnvNotFound is returned when a variable cannot be resolved.
var ErrEnvNotFound = errors.New("agentruntime: environment variable not found")

// Env resolves a variable with thread > (inherited ancestor threads, unless
// scoped) > agent > prompt > user > instance precedence.
func (s *ThreadState) Env(ctx context.Context, name string) (string, error) {
	rec, err := s.record(ctx)
	if err != nil {
		return "", err
	}
	if v, ok := rec.Env[name]; ok {
		return v.Value, nil
	}
	agent := s.rt.reg.Agent(rec.AgentID)
	if !s.rt.reg.isScopedVar(agent, name) {
		pid := rec.ParentID
		for depth := 0; pid != "" && depth < 64; depth++ {
			p, err := s.rt.store.GetThread(ctx, pid)
			if err != nil {
				break
			}
			if v, ok := p.Env[name]; ok && !v.Scoped {
				return v.Value, nil
			}
			pid = p.ParentID
		}
	}
	if agent != nil {
		if v, ok := agent.Env[name]; ok {
			return v, nil
		}
	}
	if p := s.activePrompt(); p != nil {
		if v, ok := p.Env[name]; ok {
			return v, nil
		}
	}
	if rec.UserID != nil && s.rt.cfg.UserEnv != nil {
		if v, ok := s.rt.cfg.UserEnv(ctx, *rec.UserID, name); ok {
			return v, nil
		}
	}
	if v, ok := s.rt.cfg.InstanceEnv[name]; ok {
		return v, nil
	}
	return "", fmt.Errorf("%w: %s", ErrEnvNotFound, name)
}

// InstanceEnv resolves only the runtime-instance value.
func (s *ThreadState) InstanceEnv(name string) (string, bool) {
	v, ok := s.rt.cfg.InstanceEnv[name]
	return v, ok
}

// EnvType returns the display/redaction classification of a variable.
func (s *ThreadState) EnvType(ctx context.Context, name string) (string, error) {
	rec, err := s.record(ctx)
	if err != nil {
		return "", err
	}
	if v, ok := rec.Env[name]; ok && v.Type != "" {
		return v.Type, nil
	}
	return s.rt.reg.varType(s.rt.reg.Agent(rec.AgentID), name), nil
}

// SetEnv sets a thread variable and propagates it to active descendants.
// typ is "text" or "secret" ("" = classification from declarations).
func (s *ThreadState) SetEnv(ctx context.Context, name, value, typ string) error {
	rec, err := s.record(ctx)
	if err != nil {
		return err
	}
	if typ == "" {
		typ = s.rt.reg.varType(s.rt.reg.Agent(rec.AgentID), name)
	}
	scoped := s.rt.reg.isScopedVar(s.rt.reg.Agent(rec.AgentID), name)
	if v, ok := rec.Env[name]; ok && v.Scoped {
		scoped = true
	}
	val := EnvValue{Value: value, Type: typ, Scoped: scoped}
	var walk func(id string, depth int) error
	walk = func(id string, depth int) error {
		r, err := s.rt.updateThread(ctx, id, func(r *ThreadRecord) error {
			if depth > 0 && !scoped {
				// Non-scoped values are inherited dynamically; only
				// overwrite a descendant's own copy if it has one.
				if _, ok := r.Env[name]; !ok {
					return nil
				}
			}
			r.Env[name] = val
			return nil
		})
		if err != nil {
			return err
		}
		for _, c := range r.Children {
			if c.Status == ChildTerminated || depth > 64 {
				continue
			}
			if err := walk(c.ThreadID, depth+1); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		return nil
	}
	return walk(s.threadID, 0)
}

// ---- Events ----

// Emit sends an event to connected clients via Config.EventSink.
func (s *ThreadState) Emit(event string, data any) {
	s.rt.emit(s.threadID, "event", map[string]any{"event": event, "data": data})
}

// ---- Durable key-value store (Runtime §14) ----

// GetValue returns the JSON value for key, or nil when unset.
func (s *ThreadState) GetValue(ctx context.Context, key string) (any, error) {
	raw, ok, err := s.rt.store.GetValue(ctx, s.threadID, key)
	if err != nil || !ok {
		return nil, err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// GetValueInto decodes the value for key into out; it reports false when unset.
func (s *ThreadState) GetValueInto(ctx context.Context, key string, out any) (bool, error) {
	raw, ok, err := s.rt.store.GetValue(ctx, s.threadID, key)
	if err != nil || !ok {
		return false, err
	}
	return true, json.Unmarshal(raw, out)
}

// SetValue writes a JSON-serializable value durably; nil deletes the key.
func (s *ThreadState) SetValue(ctx context.Context, key string, value any) error {
	if len(key) == 0 {
		return errf("setValue: key must not be empty")
	}
	if len(key) > s.rt.cfg.MaxKeyBytes {
		return errf("setValue: key length %d exceeds limit maxKeyBytes=%d", len(key), s.rt.cfg.MaxKeyBytes)
	}
	if value == nil {
		return s.rt.store.DeleteValue(ctx, s.threadID, key)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return errf("setValue: value is not JSON-serializable: %v", err)
	}
	if string(raw) == "null" {
		return s.rt.store.DeleteValue(ctx, s.threadID, key)
	}
	if len(raw) > s.rt.cfg.MaxValueBytes {
		return errf("setValue: value size %d exceeds limit maxValueBytes=%d", len(raw), s.rt.cfg.MaxValueBytes)
	}
	if _, exists, err := s.rt.store.GetValue(ctx, s.threadID, key); err != nil {
		return err
	} else if !exists {
		n, err := s.rt.store.CountValues(ctx, s.threadID)
		if err != nil {
			return err
		}
		if n >= s.rt.cfg.MaxKeys {
			return errf("setValue: key count would exceed limit maxKeys=%d", s.rt.cfg.MaxKeys)
		}
	}
	return s.rt.store.SetValue(ctx, s.threadID, key, raw)
}

// ---- Tool invocation (Threads §3) ----

func (s *ThreadState) checkUses(name string) error {
	if s.callerTool == nil || s.callerTool.Uses == nil {
		return nil
	}
	for _, u := range s.callerTool.Uses {
		if u == name || unqualified(u) == name {
			return nil
		}
	}
	return errf("tool %q may not invoke undeclared target %q (uses: %v)", s.callerTool.Name, name, s.callerTool.Uses)
}

// QueueTool queues a tool for execution on the next step. It returns an
// error for targets outside the calling tool's `uses` list.
func (s *ThreadState) QueueTool(ctx context.Context, name string, args map[string]any) error {
	if err := s.checkUses(name); err != nil {
		return err
	}
	return s.rt.enqueue(ctx, s.threadID, QueueItem{Kind: QueueTool, ToolName: name, ToolArgs: copyMap(args)})
}

// InvokeTool runs a tool immediately and returns its result. Undeclared
// targets yield an error result.
func (s *ThreadState) InvokeTool(ctx context.Context, name string, args map[string]any) ToolResult {
	if err := s.checkUses(name); err != nil {
		return ToolResult{Status: ToolStatusError, Error: err.Error()}
	}
	rec, err := s.record(ctx)
	if err != nil {
		return ToolResult{Status: ToolStatusError, Error: err.Error()}
	}
	if rec.Terminated != nil {
		return ToolResult{Status: ToolStatusError, Error: ErrTerminated.Error()}
	}
	side := SideA
	if s.exec != nil {
		side = s.exec.CurrentSide()
	}
	tool, err := s.rt.resolveCodeTool(ctx, s, rec, side, name)
	if err != nil {
		return ToolResult{Status: ToolStatusError, Error: err.Error()}
	}
	argsCopy := copyMap(args)
	if err := validateSchema(tool.schema, any(argsCopy)); err != nil {
		return ToolResult{Status: ToolStatusError, Error: "invalid arguments: " + err.Error()}
	}
	res, _ := s.rt.runTool(ctx, s, tool, argsCopy, nil)
	return res
}

// ---- Effects (Threads §4) ----

// ScheduleEffect schedules a named effect after delay and returns its ID.
func (s *ThreadState) ScheduleEffect(ctx context.Context, name string, args map[string]any, delay time.Duration) (string, error) {
	if s.rt.reg.Effect(name) == nil {
		return "", errf("effect %q not defined", name)
	}
	rec, err := s.record(ctx)
	if err != nil {
		return "", err
	}
	if rec.Terminated != nil {
		return "", ErrTerminated
	}
	now := time.Now()
	e := ScheduledEffect{ID: newID(), Name: name, Args: copyMap(args), ScheduledAt: now.UnixMicro(), RunAt: now.Add(delay).UnixMicro()}
	if err := s.rt.store.PutEffect(ctx, s.threadID, e); err != nil {
		return "", err
	}
	s.rt.armEffect(s.threadID, e)
	return e.ID, nil
}

// GetScheduledEffects lists pending effects, optionally filtered by name.
func (s *ThreadState) GetScheduledEffects(ctx context.Context, name string) ([]ScheduledEffect, error) {
	all, err := s.rt.store.ListEffects(ctx, s.threadID)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return all, nil
	}
	var out []ScheduledEffect
	for _, e := range all {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out, nil
}

// RemoveScheduledEffect cancels a pending effect.
func (s *ThreadState) RemoveScheduledEffect(ctx context.Context, id string) (bool, error) {
	s.rt.mu.Lock()
	if t, ok := s.rt.timers[s.threadID+"/"+id]; ok {
		t.Stop()
		delete(s.rt.timers, s.threadID+"/"+id)
	}
	s.rt.mu.Unlock()
	return s.rt.store.DeleteEffect(ctx, s.threadID, id)
}

// ---- Lifecycle ----

// Terminate soft-terminates the thread (Runtime §10).
func (s *ThreadState) Terminate(ctx context.Context) error { return s.rt.terminate(ctx, s.threadID) }

// ---- Subagent hierarchy (Threads §9) ----

// GetChildThread resolves a child by subagent reference.
func (s *ThreadState) GetChildThread(ctx context.Context, reference string) (*ThreadState, error) {
	rec, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range rec.Children {
		if c.Reference == reference {
			return s.rt.state(c.ThreadID, nil), nil
		}
	}
	return nil, nil
}

// GetParentThread resolves the parent thread or nil.
func (s *ThreadState) GetParentThread(ctx context.Context) (*ThreadState, error) {
	rec, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	if rec.ParentID == "" {
		return nil, nil
	}
	return s.rt.state(rec.ParentID, nil), nil
}

// NotifyParent queues a silent message to the parent on the owning side's
// perspective. Without a parent it queues nothing.
func (s *ThreadState) NotifyParent(ctx context.Context, content string) error {
	rec, err := s.record(ctx)
	if err != nil {
		return err
	}
	if rec.ParentID == "" {
		return nil
	}
	return s.rt.queueToParent(ctx, rec, content, nil, "subagent_notification")
}

// SetStatus writes a working-status label into this thread's parent registry
// entry; it is a no-op without one. It never ends execution.
func (s *ThreadState) SetStatus(ctx context.Context, status string) error {
	rec, err := s.record(ctx)
	if err != nil {
		return err
	}
	if rec.ParentID == "" {
		return nil
	}
	err = s.rt.updateRegistryEntry(ctx, rec.ParentID, rec.ParentReference, func(e *SubagentRegistryEntry) {
		e.StatusText = status
	})
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// StopSessionOptions configures StopSession.
type StopSessionOptions struct {
	Result       string
	NotifyParent string // when non-empty, also notifyParent(content)
	Status       string // when non-empty, also setStatus(status)
}

// StopSession marks the active session terminal (a successful terminal
// child run) without ending the thread.
func (s *ThreadState) StopSession(ctx context.Context, opts StopSessionOptions) error {
	if opts.Status != "" {
		if err := s.SetStatus(ctx, opts.Status); err != nil {
			return err
		}
	}
	if opts.NotifyParent != "" {
		if err := s.NotifyParent(ctx, opts.NotifyParent); err != nil {
			return err
		}
	}
	if s.exec != nil {
		s.exec.requestStopSession(opts.Result)
		return nil
	}
	// At rest: finalize the session immediately.
	return s.rt.finishSession(ctx, s.threadID, SessionOutcome{Kind: OutcomeCompleted, Reason: ReasonStateStopSession, Result: opts.Result})
}

// SpawnSubagent is the runtime API for creating a child from code (tools,
// hooks, handlers). Hidden relationships are spawnable here.
func (s *ThreadState) SpawnSubagent(ctx context.Context, relationship string, args map[string]any) ToolResult {
	return s.InvokeTool(ctx, ToolSubagentCreate, mergeArgs(args, map[string]any{"agent": relationship}))
}

func mergeArgs(a, b map[string]any) map[string]any {
	out := copyMap(a)
	for k, v := range b {
		out[k] = v
	}
	return out
}

// ---- live execution history ----

func (rt *Runtime) liveExec(threadID string) *ExecutionState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if c, ok := rt.ctl[threadID]; ok {
		return c.exec
	}
	return nil
}

func (rt *Runtime) historyAppend(threadID string, m Message) {
	if e := rt.liveExec(threadID); e != nil {
		e.mu.Lock()
		e.history = append(e.history, m)
		e.mu.Unlock()
	}
}

func (rt *Runtime) historyUpdate(threadID string, m Message) {
	if e := rt.liveExec(threadID); e != nil {
		e.mu.Lock()
		for i := range e.history {
			if e.history[i].ID == m.ID {
				e.history[i] = m
			}
		}
		e.mu.Unlock()
	}
}

func (rt *Runtime) historyDelete(threadID string, ids []string) {
	if e := rt.liveExec(threadID); e != nil {
		del := map[string]bool{}
		for _, id := range ids {
			del[id] = true
		}
		e.mu.Lock()
		kept := e.history[:0]
		for _, m := range e.history {
			if !del[m.ID] {
				kept = append(kept, m)
			}
		}
		e.history = kept
		e.mu.Unlock()
	}
}

// sortMessages orders messages chronologically.
func sortMessages(ms []Message) {
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].CreatedAt < ms[j].CreatedAt })
}
