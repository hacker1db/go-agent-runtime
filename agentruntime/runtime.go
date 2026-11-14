package agentruntime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"
)

// ErrTerminated is returned for execute/queue/send operations on a
// terminated thread (Runtime §10).
var ErrTerminated = errors.New("agentruntime: thread is terminated")

// ErrClosed is returned after Runtime.Close.
var ErrClosed = errors.New("agentruntime: runtime is closed")

// ThreadUser is a thread user's identity profile (never credentials).
type ThreadUser struct {
	ID          string   `json:"id"`
	Username    string   `json:"username,omitempty"`
	Role        string   `json:"role,omitempty"`
	Roles       []string `json:"roles,omitempty"`
	Permissions []string `json:"permissions,omitempty"`
	Email       string   `json:"email,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
	AvatarURL   string   `json:"avatar_url,omitempty"`
}

// Config configures a Runtime.
type Config struct {
	// MaxTotalSteps is the runtime hard cap on model steps per execution.
	// Default 100.
	MaxTotalSteps int
	// InstanceEnv holds runtime-instance environment values (lowest precedence).
	InstanceEnv map[string]string
	// UserEnv resolves user-level environment values.
	UserEnv func(ctx context.Context, userID, name string) (string, bool)
	// UserLookup resolves a thread's user profile.
	UserLookup func(ctx context.Context, userID string) (*ThreadUser, error)
	// EventSink receives state.emit events and runtime thread events.
	EventSink func(threadID, event string, data any)
	// Logger for non-fatal errors. Default discards.
	Logger *slog.Logger
	// KV caps (0 = defaults: key 512 bytes, value 1 MiB, 10000 keys).
	MaxKeyBytes   int
	MaxValueBytes int
	MaxKeys       int
	// BootstrapTTL is how long scoped-variable bootstrap requests live.
	// Default 1h.
	BootstrapTTL time.Duration
}

// Runtime executes Standard Agents on threads.
type Runtime struct {
	reg   *Registry
	store Store
	cfg   Config

	baseCtx    context.Context
	baseCancel context.CancelFunc

	recMu sync.Mutex // serializes ThreadRecord read-modify-write

	mu         sync.Mutex
	ctl        map[string]*threadCtl
	waiters    map[string][]chan SessionOutcome
	timers     map[string]*time.Timer
	bootstraps map[string]*pendingBootstrap
	closed     bool
	wg         sync.WaitGroup
}

type threadCtl struct {
	running bool
	pending bool
	cancel  context.CancelFunc
	exec    *ExecutionState
	idle    []chan struct{}
}

// New constructs a runtime. Call Start to recover persisted work.
func New(reg *Registry, store Store, cfg Config) *Runtime {
	if cfg.MaxTotalSteps <= 0 {
		cfg.MaxTotalSteps = 100
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.MaxKeyBytes <= 0 {
		cfg.MaxKeyBytes = 512
	}
	if cfg.MaxValueBytes <= 0 {
		cfg.MaxValueBytes = 1 << 20
	}
	if cfg.MaxKeys <= 0 {
		cfg.MaxKeys = 10000
	}
	if cfg.BootstrapTTL <= 0 {
		cfg.BootstrapTTL = time.Hour
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runtime{
		reg: reg, store: store, cfg: cfg,
		baseCtx: ctx, baseCancel: cancel,
		ctl:        map[string]*threadCtl{},
		waiters:    map[string][]chan SessionOutcome{},
		timers:     map[string]*time.Timer{},
		bootstraps: map[string]*pendingBootstrap{},
	}
}

// Registry returns the runtime's registry.
func (rt *Runtime) Registry() *Registry { return rt.reg }

// Start recovers persisted state: executions lost to a crash/eviction settle
// as failed/execution_lost, scheduled effects are re-armed, and threads with
// durable queued work are resumed.
func (rt *Runtime) Start(ctx context.Context) error {
	threads, err := rt.store.ListThreads(ctx)
	if err != nil {
		return err
	}
	for _, t := range threads {
		if t.Status.State == RunRunning {
			if err := rt.setRunStatus(ctx, t.ID, RunFailed, ptr(ReasonExecutionLost)); err != nil {
				return err
			}
			if err := rt.repairDanglingToolCalls(ctx, t.ID); err != nil {
				return err
			}
		}
		if t.Terminated != nil {
			continue
		}
		effects, err := rt.store.ListEffects(ctx, t.ID)
		if err != nil {
			return err
		}
		for _, e := range effects {
			rt.armEffect(t.ID, e)
		}
		q, err := rt.store.PeekQueue(ctx, t.ID)
		if err != nil {
			return err
		}
		if len(q) > 0 {
			rt.schedule(t.ID)
		}
	}
	return nil
}

// Close aborts in-flight executions and waits for them to settle.
func (rt *Runtime) Close() {
	rt.mu.Lock()
	rt.closed = true
	for _, t := range rt.timers {
		t.Stop()
	}
	rt.mu.Unlock()
	rt.baseCancel()
	rt.wg.Wait()
}

// CreateThreadOptions configures CreateThread.
type CreateThreadOptions struct {
	AgentID   string
	UserID    *string
	Arguments map[string]any
	Env       map[string]EnvValue
	Name      string
}

// CreateThread creates a root thread. Immediate subagent relationships are
// spawned (recursively) before after_thread_created runs and before any side
// takes a turn.
func (rt *Runtime) CreateThread(ctx context.Context, opts CreateThreadOptions) (*ThreadState, error) {
	if rt.isClosed() {
		return nil, ErrClosed
	}
	return rt.createThread(ctx, opts, nil)
}

type childLink struct {
	parentID   string
	reference  string
	rel        *SubagentRelationship
	owningSide Side
}

func (rt *Runtime) createThread(ctx context.Context, opts CreateThreadOptions, link *childLink) (*ThreadState, error) {
	agent := rt.reg.Agent(opts.AgentID)
	if agent == nil {
		return nil, errf("agent %q is not defined", opts.AgentID)
	}
	if issues := rt.reg.ValidateAgent(agent); hasErrors(issues) {
		return nil, &ValidationError{Issues: issues}
	}
	rec := ThreadRecord{
		ID:        newID(),
		AgentID:   agent.Name,
		UserID:    opts.UserID,
		CreatedAt: nowMicros(),
		Arguments: copyMap(opts.Arguments),
		Env:       map[string]EnvValue{},
		Name:      opts.Name,
		Status:    ThreadStatus{State: RunIdle, Since: 0},
	}
	for k, v := range opts.Env {
		rec.Env[k] = v
	}
	if link != nil {
		rec.ParentID = link.parentID
		rec.ParentReference = link.reference
		rec.Relationship = link.rel.Name()
		rec.OwningSide = link.owningSide
		rec.ParentCommunication = link.rel.parentComm()
	}
	s := rt.state(rec.ID, nil)
	// Fail composition closed before the thread becomes visible.
	if _, _, err := rt.childHookRequirements(ctx, &ThreadState{rt: rt, threadID: rec.ID, preRecord: &rec}, agent); err != nil {
		return nil, err
	}
	if err := rt.store.PutThread(ctx, rec); err != nil {
		return nil, err
	}
	// Creation is all-or-nothing: on any later failure remove the thread and
	// whatever children it already spawned.
	committed := false
	defer func() {
		if !committed {
			rt.deleteThreadTree(context.Background(), rec.ID)
		}
	}()
	// Immediate spawning (Runtime §7.3, Subagents §3.2). Recursion happens
	// naturally because child creation goes through createThread.
	for _, side := range []Side{SideA, SideB} {
		for i := range agent.Side(side).Subagents {
			rel := &agent.Side(side).Subagents[i]
			if rel.Immediate == nil || !rt.relationshipEnabled(ctx, s, rel) {
				continue
			}
			if _, err := rt.spawnImmediate(ctx, s, side, rel); err != nil {
				return nil, errf("immediate subagent %q: %v", rel.Name(), err)
			}
		}
	}
	hooks, err := rt.resolveHooks(ctx, s, HookAfterThreadCreated)
	if err != nil {
		return nil, err
	}
	for _, h := range hooks {
		if err := h.AfterThreadCreated(ctx, s); err != nil {
			return nil, errf("after_thread_created hook %q: %v", h.ID, err)
		}
	}
	committed = true
	return s, nil
}

// deleteThreadTree removes a thread that never became visible to callers,
// together with its spawned descendants (compensation for failed creation).
func (rt *Runtime) deleteThreadTree(ctx context.Context, id string) {
	rec, err := rt.store.GetThread(ctx, id)
	if err != nil {
		return
	}
	for _, c := range rec.Children {
		rt.deleteThreadTree(ctx, c.ThreadID)
	}
	rt.mu.Lock()
	for key, t := range rt.timers {
		if len(key) > len(id) && key[:len(id)] == id && key[len(id)] == '/' {
			t.Stop()
			delete(rt.timers, key)
		}
	}
	rt.mu.Unlock()
	if err := rt.store.DeleteThread(ctx, id); err != nil {
		rt.cfg.Logger.Error("compensating delete", "thread", id, "err", err)
	}
}

// repairDanglingToolCalls closes tool calls left without results by a lost
// execution, so history stays well-formed. Interrupted tools are not
// re-executed (at-most-once side effects).
func (rt *Runtime) repairDanglingToolCalls(ctx context.Context, threadID string) error {
	msgs, err := rt.store.ListMessages(ctx, threadID)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, m := range msgs {
		if m.Role == RoleTool && m.ToolCallID != nil {
			have[*m.ToolCallID] = true
		}
	}
	s := rt.state(threadID, nil)
	for _, m := range msgs {
		calls, err := m.DecodeToolCalls()
		if err != nil {
			continue
		}
		for _, c := range calls {
			if have[c.ID] {
				continue
			}
			tm := Message{
				Role:       RoleTool,
				Content:    ptr("Error: interrupted: the runtime stopped before this tool call completed; it was not retried."),
				Name:       ptr(c.Function.Name),
				ToolCallID: ptr(c.ID),
				ToolStatus: ptr(ToolStatusError),
				Status:     ptr(MessageStatusCompleted),
				Depth:      m.Depth,
				Metadata:   map[string]any{MetaSide: string(m.authorSide())},
			}
			if m.Depth > 0 {
				tm.ParentID = m.ParentID
			}
			for _, k := range []string{MetaSubpromptRun, MetaQueuedTool} {
				if v, ok := m.Metadata[k]; ok {
					tm.Metadata[k] = v
				}
			}
			if _, err := s.createMessage(ctx, tm); err != nil {
				return err
			}
		}
	}
	return nil
}

// Thread returns an at-rest ThreadState handle (execution == nil).
func (rt *Runtime) Thread(ctx context.Context, threadID string) (*ThreadState, error) {
	if _, err := rt.store.GetThread(ctx, threadID); err != nil {
		return nil, err
	}
	return rt.state(threadID, nil), nil
}

// SendMessage is the "user message" invocation trigger: the message is
// accepted durably into the thread's FIFO queue and an execution is started
// if the thread is idle. It returns before the execution completes.
func (rt *Runtime) SendMessage(ctx context.Context, threadID string, in MessageInput) error {
	if in.Role == "" {
		in.Role = RoleUser
	}
	return rt.enqueue(ctx, threadID, QueueItem{Kind: QueueMessage, Message: &in})
}

// WaitIdle blocks until the thread has no running execution and no queued work.
func (rt *Runtime) WaitIdle(ctx context.Context, threadID string) error {
	for {
		rt.mu.Lock()
		c := rt.ctlFor(threadID)
		if !c.running {
			rt.mu.Unlock()
			q, err := rt.store.PeekQueue(ctx, threadID)
			if err != nil {
				return err
			}
			rec, err := rt.store.GetThread(ctx, threadID)
			if err != nil {
				return err
			}
			if len(q) == 0 || rec.Terminated != nil {
				return nil
			}
			rt.schedule(threadID)
			continue
		}
		ch := make(chan struct{})
		c.idle = append(c.idle, ch)
		rt.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (rt *Runtime) isClosed() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.closed
}

func (rt *Runtime) ctlFor(id string) *threadCtl {
	c, ok := rt.ctl[id]
	if !ok {
		c = &threadCtl{}
		rt.ctl[id] = c
	}
	return c
}

func (rt *Runtime) state(id string, exec *ExecutionState) *ThreadState {
	return &ThreadState{rt: rt, threadID: id, exec: exec}
}

// enqueue durably appends work and schedules execution (Runtime §9).
func (rt *Runtime) enqueue(ctx context.Context, threadID string, item QueueItem) error {
	if rt.isClosed() {
		return ErrClosed
	}
	rec, err := rt.store.GetThread(ctx, threadID)
	if err != nil {
		return err
	}
	if rec.Terminated != nil {
		return ErrTerminated
	}
	item.ID = newID()
	item.CreatedAt = nowMicros()
	if err := rt.store.Enqueue(ctx, threadID, item); err != nil {
		return err
	}
	rt.schedule(threadID)
	return nil
}

// schedule starts an execution loop for the thread unless one is running,
// in which case the running loop picks the new work up (Runtime §2, §11).
func (rt *Runtime) schedule(threadID string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.closed {
		return
	}
	c := rt.ctlFor(threadID)
	if c.running {
		c.pending = true
		return
	}
	c.running = true
	c.pending = false
	rt.wg.Add(1)
	go rt.runLoop(threadID)
}

func (rt *Runtime) runLoop(threadID string) {
	defer rt.wg.Done()
	ctx := rt.baseCtx
	for {
		rt.mu.Lock()
		c := rt.ctlFor(threadID)
		c.pending = false
		rt.mu.Unlock()

		rec, err := rt.store.GetThread(context.Background(), threadID)
		var q []QueueItem
		if err == nil {
			q, err = rt.store.PeekQueue(context.Background(), threadID)
		}
		stop := err != nil || rec.Terminated != nil || len(q) == 0 || ctx.Err() != nil
		if err != nil {
			rt.cfg.Logger.Error("run loop", "thread", threadID, "err", err)
		}
		if stop {
			rt.mu.Lock()
			if c.pending && err == nil && rec.Terminated == nil && ctx.Err() == nil {
				rt.mu.Unlock()
				continue
			}
			c.running = false
			for _, ch := range c.idle {
				close(ch)
			}
			c.idle = nil
			rt.mu.Unlock()
			return
		}
		head := q[0].ID
		res := rt.executeOnce(threadID)
		if res.failed {
			// Avoid a hot loop on persistent failures: if the queue head did
			// not advance, leave it durable for the next trigger or Start.
			if q2, err := rt.store.PeekQueue(context.Background(), threadID); err == nil && len(q2) > 0 && q2[0].ID == head {
				rt.mu.Lock()
				c.running = false
				c.pending = false
				for _, ch := range c.idle {
					close(ch)
				}
				c.idle = nil
				rt.mu.Unlock()
				return
			}
		}
	}
}

// updateThread performs a serialized read-modify-write of a thread record.
func (rt *Runtime) updateThread(ctx context.Context, id string, fn func(*ThreadRecord) error) (*ThreadRecord, error) {
	rt.recMu.Lock()
	defer rt.recMu.Unlock()
	rec, err := rt.store.GetThread(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := fn(rec); err != nil {
		return nil, err
	}
	if err := rt.store.PutThread(ctx, *rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// setRunStatus persists run status; Since changes only when state or
// reason changes (Execution State §4.3).
func (rt *Runtime) setRunStatus(ctx context.Context, id string, st ThreadRunState, reason *string) error {
	var changed bool
	var snapshot ThreadStatus
	_, err := rt.updateThread(ctx, id, func(r *ThreadRecord) error {
		if r.Status.State == st && deref(r.Status.Reason) == deref(reason) && (r.Status.Reason == nil) == (reason == nil) {
			return nil
		}
		r.Status = ThreadStatus{State: st, Since: nowMicros(), Reason: reason}
		changed = true
		snapshot = r.Status
		return nil
	})
	if err == nil && changed {
		rt.emit(id, "thread_status", map[string]any{"status": snapshot})
	}
	return err
}

func (rt *Runtime) emit(threadID, event string, data any) {
	if rt.cfg.EventSink != nil {
		rt.cfg.EventSink(threadID, event, data)
	}
}

// terminate implements soft shutdown (Runtime §10).
func (rt *Runtime) terminate(ctx context.Context, threadID string) error {
	rec, err := rt.updateThread(ctx, threadID, func(r *ThreadRecord) error {
		if r.Terminated == nil {
			r.Terminated = ptr(nowMicros())
		}
		return nil
	})
	if err != nil {
		return err
	}
	rt.mu.Lock()
	if c, ok := rt.ctl[threadID]; ok && c.cancel != nil {
		c.cancel()
	}
	for id, t := range rt.timers {
		if len(id) > len(threadID) && id[:len(threadID)] == threadID && id[len(threadID)] == '/' {
			t.Stop()
			delete(rt.timers, id)
		}
	}
	rt.mu.Unlock()
	if rec.ParentID != "" {
		_ = rt.updateRegistryEntry(ctx, rec.ParentID, rec.ParentReference, func(e *SubagentRegistryEntry) {
			e.Status = ChildTerminated
		})
	}
	rt.resolveWaiters(threadID, SessionOutcome{Kind: OutcomeTerminated, Reason: "terminated"})
	return nil
}
