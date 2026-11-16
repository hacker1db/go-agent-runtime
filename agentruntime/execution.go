package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Stop reason codes (Execution State §4.2).
const (
	ReasonStopOnResponse   = "stop_on_response"
	ReasonStopTool         = "stop_tool"
	ReasonAwaitingHuman    = "awaiting_human"
	ReasonForcedHumanTurn  = "forced_human_turn"
	ReasonHookBlocked      = "hook_blocked"
	ReasonAborted          = "aborted"
	ReasonSessionStop      = "session_stop"
	ReasonStateStopSession = "state_stop_session"
	ReasonMaxTotalSteps    = "max_total_steps"
	ReasonMaxSideSteps     = "max_side_steps"
	ReasonMaxSessionTurns  = "max_session_turns"
	ReasonHandlerIdle      = "handler_idle"
	ReasonStepError        = "step_error"
	ReasonSessionFail      = "session_fail"
	ReasonFatalError       = "fatal_error"
	ReasonExecutionLost    = "execution_lost"
	// Runtime-specific codes (clients treat unknown codes as opaque).
	ReasonExecutionStopped = "execution_stopped"
	ReasonEffectCompleted  = "effect_completed"
)

// Session outcome kinds.
const (
	OutcomeCompleted  = "completed"
	OutcomeFailed     = "failed"
	OutcomeTerminated = "terminated"
)

// SessionOutcome describes how an agent session closed.
type SessionOutcome struct {
	Kind        string          `json:"kind"`
	Reason      string          `json:"reason"`
	Result      string          `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	Attachments []AttachmentRef `json:"attachments,omitempty"` // thread-local to the session's thread
}

// ExecutionState is present while a thread executes (Execution State §2).
type ExecutionState struct {
	mu          sync.Mutex
	flowID      string
	currentSide Side
	stepCount   int
	sideSteps   map[Side]int
	stopped     bool
	stoppedBy   *Side
	history     []Message
	ctx         context.Context
	promptPath  []string
	forced      *Side
	stopSession *string
	scratch     map[string]any
}

// FlowID returns the identifier of the current execution flow.
func (e *ExecutionState) FlowID() string { return e.flowID }

// CurrentSide returns the side whose turn is executing.
func (e *ExecutionState) CurrentSide() Side {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentSide
}

// StepCount returns the total steps taken in this flow.
func (e *ExecutionState) StepCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stepCount
}

// SideAStepCount returns the steps taken by side A in this flow.
func (e *ExecutionState) SideAStepCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sideSteps[SideA]
}

// SideBStepCount returns the steps taken by side B in this flow.
func (e *ExecutionState) SideBStepCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sideSteps[SideB]
}

// Stopped reports whether the flow has stopped.
func (e *ExecutionState) Stopped() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopped
}

// StoppedBy returns the side that stopped the flow, or nil.
func (e *ExecutionState) StoppedBy() *Side {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stoppedBy
}

// MessageHistory returns live chronological persisted execution history,
// including silent messages.
func (e *ExecutionState) MessageHistory() []Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneJSON(e.history)
}

// AbortSignal is closed when the execution is aborted.
func (e *ExecutionState) AbortSignal() <-chan struct{} { return e.ctx.Done() }

// Context returns the execution's cancellation context.
func (e *ExecutionState) Context() context.Context { return e.ctx }

// PromptPath returns the path from the root prompt to the current prompt.
func (e *ExecutionState) PromptPath() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.promptPath...)
}

// ForceTurn forces the next turn to the given side.
func (e *ExecutionState) ForceTurn(s Side) {
	e.mu.Lock()
	e.forced = &s
	e.mu.Unlock()
}

// Stop stops the current execution loop after the current operation. It
// does not by itself end the agent session.
func (e *ExecutionState) Stop() {
	e.mu.Lock()
	if !e.stopped {
		e.stopped = true
		s := e.currentSide
		e.stoppedBy = &s
	}
	e.mu.Unlock()
}

func (e *ExecutionState) requestStopSession(result string) {
	e.mu.Lock()
	e.stopSession = &result
	e.mu.Unlock()
}

func (e *ExecutionState) takeStopSession() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopSession == nil {
		return "", false
	}
	r := *e.stopSession
	e.stopSession = nil
	return r, true
}

func (e *ExecutionState) takeForced() *Side {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.forced
	e.forced = nil
	return f
}

func (e *ExecutionState) setSide(s Side) {
	e.mu.Lock()
	e.currentSide = s
	e.mu.Unlock()
}

func (e *ExecutionState) pushPrompt(p string) {
	e.mu.Lock()
	e.promptPath = append(e.promptPath, p)
	e.mu.Unlock()
}

func (e *ExecutionState) popPrompt() {
	e.mu.Lock()
	if len(e.promptPath) > 0 {
		e.promptPath = e.promptPath[:len(e.promptPath)-1]
	}
	e.mu.Unlock()
}

func (e *ExecutionState) setRootPrompt(p string) {
	e.mu.Lock()
	if p == "" {
		e.promptPath = nil
	} else {
		e.promptPath = []string{p}
	}
	e.mu.Unlock()
}

// runResult is the result of one execution.
type runResult struct {
	reason  string
	failed  bool
	session *SessionOutcome // non-nil when the agent session closed
	parked  bool            // turn handed to a human-backed side
}

// executeOnce runs one execution flow for the thread. Only one flow runs
// per thread at a time (enforced by runLoop/schedule).
func (rt *Runtime) executeOnce(threadID string) runResult {
	ctx, cancel := context.WithCancel(rt.baseCtx)
	defer cancel()
	msgs, err := rt.store.ListMessages(ctx, threadID)
	if err != nil {
		rt.cfg.Logger.Error("load history", "thread", threadID, "err", err)
		return runResult{reason: ReasonStepError, failed: true}
	}
	exec := &ExecutionState{
		flowID:    newID(),
		sideSteps: map[Side]int{},
		ctx:       ctx,
		history:   msgs,
		scratch:   map[string]any{},
	}
	rt.mu.Lock()
	c := rt.ctlFor(threadID)
	c.exec = exec
	c.cancel = cancel
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		c.exec = nil
		c.cancel = nil
		rt.mu.Unlock()
	}()

	bg := context.Background() // settlement must persist even after abort
	if err := rt.setRunStatus(bg, threadID, RunRunning, nil); err != nil {
		rt.cfg.Logger.Error("set running", "thread", threadID, "err", err)
		return runResult{reason: ReasonStepError, failed: true}
	}
	rec, _ := rt.store.GetThread(bg, threadID)
	if rec != nil && rec.ParentID != "" {
		_ = rt.updateRegistryEntry(bg, rec.ParentID, rec.ParentReference, func(e *SubagentRegistryEntry) {
			if e.Status != ChildTerminated {
				e.Status = ChildRunning
			}
		})
	}

	res := func() (res runResult) {
		defer func() {
			if p := recover(); p != nil {
				res = runResult{reason: ReasonFatalError, failed: true,
					session: &SessionOutcome{Kind: OutcomeFailed, Reason: ReasonFatalError, Error: fmt.Sprint(p)}}
			}
		}()
		return rt.drive(ctx, rt.state(threadID, exec))
	}()

	state := RunIdle
	if res.failed {
		state = RunFailed
	}
	if err := rt.setRunStatus(bg, threadID, state, ptr(res.reason)); err != nil {
		rt.cfg.Logger.Error("settle status", "thread", threadID, "err", err)
	}
	switch {
	case res.session != nil:
		if err := rt.finishSession(bg, threadID, *res.session); err != nil {
			rt.cfg.Logger.Error("finish session", "thread", threadID, "err", err)
		}
	case rec != nil && rec.ParentID != "":
		st := ChildIdle
		if res.parked {
			st = ChildAwaitingHuman
		}
		_ = rt.updateRegistryEntry(bg, rec.ParentID, rec.ParentReference, func(e *SubagentRegistryEntry) {
			if e.Status != ChildTerminated {
				e.Status = st
			}
		})
	}
	return res
}

// drive runs side turns until the execution ends.
func (rt *Runtime) drive(ctx context.Context, s *ThreadState) runResult {
	exec := s.exec
	side, err := rt.initialSide(ctx, s.threadID)
	if err != nil {
		return stepErr(err)
	}
	effectOnly := true
	for {
		if ctx.Err() != nil {
			return runResult{reason: ReasonAborted}
		}
		rec, err := rt.store.GetThread(ctx, s.threadID)
		if err != nil {
			return stepErr(err)
		}
		agent := rt.reg.Agent(rec.AgentID)
		if agent == nil {
			return stepErr(errf("agent %q not defined", rec.AgentID))
		}
		exec.setSide(side)
		cfg := agent.Side(side)

		var turn turnResult
		switch {
		case cfg.Human:
			drained, err := rt.drain(ctx, s, side)
			if err != nil {
				return stepErr(err)
			}
			if drained.handoff != "" {
				turn = turnResult{handoffAgent: drained.handoff}
				break
			}
			if drained.messages > 0 || drained.tools > 0 {
				// New inbound traffic may belong to the other side's turn.
				if next := rt.sideFromDrain(drained); next != "" && next != side {
					side = next
					continue
				}
			}
			reason := ReasonAwaitingHuman
			if f := exec.takeForced(); f != nil && *f == side {
				reason = ReasonForcedHumanTurn
			}
			return runResult{reason: reason, parked: true}
		case cfg.Handler != "":
			exec.setRootPrompt("")
			turn = rt.handlerTurn(ctx, s, side, cfg)
		default:
			exec.setRootPrompt(cfg.Prompt)
			turn = rt.promptTurn(ctx, s, agent, side, cfg, &effectOnly)
		}
		if turn.end != nil {
			return *turn.end
		}

		// Side turn ended without closing the session.
		if turn.handoffAgent != "" {
			target := rt.reg.Agent(turn.handoffAgent)
			if _, err := rt.updateThread(ctx, s.threadID, func(r *ThreadRecord) error {
				r.AgentID = target.Name
				return nil
			}); err != nil {
				return stepErr(err)
			}
			hs, _ := target.humanSide()
			side = hs.Other()
			continue
		}
		upd, err := rt.updateThread(ctx, s.threadID, func(r *ThreadRecord) error {
			r.SessionTurns++
			return nil
		})
		if err != nil {
			return stepErr(err)
		}
		if agent.MaxSessionTurns > 0 && upd.SessionTurns >= agent.MaxSessionTurns {
			return sessionEnd(ReasonMaxSessionTurns, OutcomeFailed, "", "session turn cap reached", nil)
		}
		if exec.Stopped() {
			return runResult{reason: ReasonExecutionStopped}
		}
		next := side.Other()
		if f := exec.takeForced(); f != nil {
			next = *f
			if agent.Side(next).Human {
				return runResult{reason: ReasonForcedHumanTurn, parked: true}
			}
		}
		if agent.Side(next).Human {
			// Before parking, deliver any work that arrived meanwhile.
			q, err := rt.store.PeekQueue(ctx, s.threadID)
			if err != nil {
				return stepErr(err)
			}
			if len(q) == 0 {
				return runResult{reason: turn.reason, parked: true}
			}
		}
		if turn.idle {
			return runResult{reason: turn.reason}
		}
		side = next
	}
}

type turnResult struct {
	end          *runResult // execution (and possibly session) ends
	reason       string     // why the side turn ended
	handoffAgent string
	idle         bool // handler produced no reply
}

func stepErr(err error) runResult {
	return sessionEnd(ReasonStepError, OutcomeFailed, "", err.Error(), nil)
}

func sessionEnd(reason, kind, result, errMsg string, atts []AttachmentRef) runResult {
	return runResult{
		reason: reason,
		failed: kind == OutcomeFailed && (reason == ReasonStepError || reason == ReasonSessionFail || reason == ReasonFatalError),
		session: &SessionOutcome{
			Kind: kind, Reason: reason, Result: result, Error: errMsg, Attachments: atts,
		},
	}
}

// initialSide picks the side to act: the side acting on the newest queued
// message, else on the newest persisted message, else side A.
func (rt *Runtime) initialSide(ctx context.Context, threadID string) (Side, error) {
	q, err := rt.store.PeekQueue(ctx, threadID)
	if err != nil {
		return "", err
	}
	for i := len(q) - 1; i >= 0; i-- {
		if q[i].Kind == QueueMessage && q[i].Message != nil {
			return sideActingOn(q[i].Message.Role), nil
		}
	}
	msgs, err := rt.store.ListMessages(ctx, threadID)
	if err != nil {
		return "", err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Depth == 0 && (m.Role == RoleUser || m.Role == RoleAssistant) {
			return sideActingOn(m.Role), nil
		}
	}
	return SideA, nil
}

type drainResult struct {
	messages int
	tools    int
	effects  int
	lastRole Role
	inbound  []Message
	handoff  string
}

func (rt *Runtime) sideFromDrain(d drainResult) Side {
	if d.lastRole == "" {
		return ""
	}
	return sideActingOn(d.lastRole)
}

// drain delivers durable queued work in FIFO order: messages are persisted,
// queued tools executed (and persisted), effects run. Each item is
// acknowledged only after its effects are persisted.
func (rt *Runtime) drain(ctx context.Context, s *ThreadState, side Side) (drainResult, error) {
	var res drainResult
	items, err := rt.store.PeekQueue(ctx, s.threadID)
	if err != nil {
		return res, err
	}
	for _, it := range items {
		if ctx.Err() != nil {
			// Cancellation stops draining; unacked items stay queued for the next run.
			return res, nil //nolint:nilerr // intentional: cancellation is not a drain failure
		}
		switch it.Kind {
		case QueueMessage:
			if it.Message == nil {
				if err := rt.store.AckQueue(ctx, s.threadID, []string{it.ID}); err != nil {
					return res, err
				}
				continue
			}
			// Deterministic ID + atomic insert/ack: a crash can neither
			// duplicate nor drop this delivery.
			in := messageFromInput(*it.Message)
			in.ID = "q-" + it.ID
			m, err := s.createMessageAck(ctx, in, []string{it.ID})
			var rej *HookRejectedError
			if errors.As(err, &rej) {
				rt.cfg.Logger.Warn("queued message rejected by hook", "thread", s.threadID, "item", it.ID, "err", err)
				if err := rt.store.AckQueue(ctx, s.threadID, []string{it.ID}); err != nil {
					return res, err
				}
				continue
			}
			if err != nil {
				return res, err
			}
			res.messages++
			res.lastRole = m.Role
			res.inbound = append(res.inbound, m)
		case QueueTool:
			// runQueuedTool acknowledges the item atomically with the
			// persisted tool call, so the tool runs at most once.
			h, err := rt.runQueuedTool(ctx, s, side, it)
			if err != nil {
				return res, err
			}
			if h != "" && res.handoff == "" {
				res.handoff = h
			}
			res.tools++
		case QueueEffect:
			if err := rt.runEffect(ctx, s, it.EffectID); err != nil {
				rt.cfg.Logger.Warn("effect failed", "thread", s.threadID, "effect", it.EffectID, "err", err)
			}
			res.effects++
			if err := rt.store.AckQueue(ctx, s.threadID, []string{it.ID}); err != nil {
				return res, err
			}
		default:
			if err := rt.store.AckQueue(ctx, s.threadID, []string{it.ID}); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// promptTurn runs the step cycle (Runtime §3) for a prompt-backed side.
func (rt *Runtime) promptTurn(ctx context.Context, s *ThreadState, agent *AgentDefinition, side Side, cfg *SideConfig, effectOnly *bool) turnResult {
	exec := s.exec
	prompt := rt.reg.Prompt(cfg.Prompt)
	if prompt == nil {
		return turnResult{end: ptr(stepErr(errf("prompt %q not defined", cfg.Prompt)))}
	}
	for {
		// 1. Drain queued messages/tools for this step.
		drained, err := rt.drain(ctx, s, side)
		if err != nil {
			return turnResult{end: ptr(stepErr(err))}
		}
		if drained.handoff != "" {
			return turnResult{handoffAgent: drained.handoff, reason: ReasonStopTool}
		}
		if drained.messages > 0 || drained.tools > 0 {
			*effectOnly = false
		}
		if *effectOnly && drained.effects > 0 {
			// An effect fired but produced no conversational work.
			if q, _ := rt.store.PeekQueue(ctx, s.threadID); len(q) == 0 {
				return turnResult{end: &runResult{reason: ReasonEffectCompleted}}
			}
			*effectOnly = false
			continue
		}
		*effectOnly = false
		if ctx.Err() != nil {
			return turnResult{end: &runResult{reason: ReasonAborted}}
		}
		if exec.Stopped() {
			return turnResult{end: &runResult{reason: ReasonExecutionStopped}}
		}
		if r, ok := exec.takeStopSession(); ok {
			return turnResult{end: ptr(sessionEnd(ReasonStateStopSession, OutcomeCompleted, r, "", nil))}
		}

		// 2–3. Assemble context and call the model.
		model := rt.reg.Model(prompt.Model)
		if model == nil {
			return turnResult{end: ptr(stepErr(errf("model %q not defined", prompt.Model)))}
		}
		provider := rt.reg.Provider(model.Provider)
		if provider == nil {
			return turnResult{end: ptr(stepErr(errf("provider %q not registered", model.Provider)))}
		}
		surface, err := rt.toolSurface(ctx, s, agent, side, prompt, true)
		if err != nil {
			return turnResult{end: ptr(stepErr(err))}
		}
		req, err := rt.assembleContext(ctx, s, side, prompt, model, surface, contextScope{})
		if err != nil {
			var hb *HookBlockedError
			if errors.As(err, &hb) {
				return turnResult{end: &runResult{reason: ReasonHookBlocked}}
			}
			return turnResult{end: ptr(stepErr(err))}
		}
		resp, err := provider.Generate(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return turnResult{end: &runResult{reason: ReasonAborted}}
			}
			return turnResult{end: ptr(stepErr(fmt.Errorf("model call: %w", err)))}
		}
		exec.mu.Lock()
		exec.stepCount++
		exec.sideSteps[side]++
		sideSteps, total := exec.sideSteps[side], exec.stepCount
		exec.mu.Unlock()

		// 4. Persist the assistant response before acting on it.
		asst, err := rt.persistAssistant(ctx, s, side, prompt, resp, 0, nil, "")
		if err != nil {
			return turnResult{end: ptr(stepErr(err))}
		}

		// 5–6. Execute tool calls sequentially; persist each result.
		out, err := rt.executeToolCalls(ctx, s, side, surface, resp.ToolCalls, asst, 0, "")
		if err != nil {
			return turnResult{end: ptr(stepErr(err))}
		}

		// 7. Stop conditions, in order (Runtime §6, Agents §4).
		if out.sessionStop != nil {
			return turnResult{end: ptr(sessionEnd(ReasonSessionStop, OutcomeCompleted, out.sessionStop.Result, "", out.sessionStop.Attachments))}
		}
		if out.sessionFail != nil {
			return turnResult{end: ptr(sessionEnd(ReasonSessionFail, OutcomeFailed, "", out.sessionFail.Result, out.sessionFail.Attachments))}
		}
		if r, ok := exec.takeStopSession(); ok {
			return turnResult{end: ptr(sessionEnd(ReasonStateStopSession, OutcomeCompleted, r, "", nil))}
		}
		if out.handoff != "" {
			return turnResult{handoffAgent: out.handoff, reason: ReasonStopTool}
		}
		if out.stopTool {
			return turnResult{reason: ReasonStopTool}
		}
		if cfg.stopOnResponse() && len(resp.ToolCalls) == 0 {
			return turnResult{reason: ReasonStopOnResponse}
		}
		if cfg.MaxSteps > 0 && sideSteps >= cfg.MaxSteps {
			return turnResult{end: ptr(sessionEnd(ReasonMaxSideSteps, OutcomeFailed, "", "side step limit reached", nil))}
		}
		if total >= rt.cfg.MaxTotalSteps {
			return turnResult{end: ptr(sessionEnd(ReasonMaxTotalSteps, OutcomeFailed, "", "runtime step cap reached", nil))}
		}
		if exec.Stopped() {
			return turnResult{end: &runResult{reason: ReasonExecutionStopped}}
		}
		if ctx.Err() != nil {
			return turnResult{end: &runResult{reason: ReasonAborted}}
		}
	}
}

// handlerTurn is a single handler invocation (Runtime §3).
func (rt *Runtime) handlerTurn(ctx context.Context, s *ThreadState, side Side, cfg *SideConfig) turnResult {
	h := rt.reg.Handler(cfg.Handler)
	if h == nil {
		return turnResult{end: ptr(stepErr(errf("handler %q not defined", cfg.Handler)))}
	}
	drained, err := rt.drain(ctx, s, side)
	if err != nil {
		return turnResult{end: ptr(stepErr(err))}
	}
	if drained.handoff != "" {
		return turnResult{handoffAgent: drained.handoff}
	}
	before := nowMicros()
	if err := h.Execute(ctx, s, HandlerTurn{Side: side, Inbound: drained.inbound}); err != nil {
		if ctx.Err() != nil {
			return turnResult{end: &runResult{reason: ReasonAborted}}
		}
		return turnResult{end: ptr(stepErr(fmt.Errorf("handler %q: %w", h.Name, err)))}
	}
	if r, ok := s.exec.takeStopSession(); ok {
		return turnResult{end: ptr(sessionEnd(ReasonStateStopSession, OutcomeCompleted, r, "", nil))}
	}
	// Did the handler reply in its own perspective?
	replied := false
	for _, m := range s.exec.MessageHistory() {
		if m.CreatedAt > before && m.Depth == 0 && m.Role == side.PerspectiveRole() {
			replied = true
			break
		}
	}
	if !replied {
		return turnResult{reason: ReasonHandlerIdle, idle: true}
	}
	return turnResult{reason: ReasonHandlerIdle}
}

// persistAssistant stores a model response in the side's perspective.
func (rt *Runtime) persistAssistant(ctx context.Context, s *ThreadState, side Side, prompt *PromptDefinition, resp *ProviderResponse, depth int, parentID *string, runID string) (Message, error) {
	m := Message{
		Role:     side.PerspectiveRole(),
		Content:  ptr(resp.Content),
		Status:   ptr(MessageStatusCompleted),
		Prompt:   ptr(prompt.Name),
		Depth:    depth,
		ParentID: parentID,
		Metadata: map[string]any{MetaSide: string(side)},
	}
	if runID != "" {
		m.Metadata[MetaSubpromptRun] = runID
	}
	if len(resp.ToolCalls) > 0 {
		for i := range resp.ToolCalls {
			if resp.ToolCalls[i].ID == "" {
				resp.ToolCalls[i].ID = "call_" + newID()
			}
			resp.ToolCalls[i].Type = "function"
		}
		b, err := json.Marshal(resp.ToolCalls)
		if err != nil {
			return Message{}, err
		}
		m.ToolCalls = ptr(string(b))
	}
	if resp.ReasoningContent != nil {
		m.ReasoningContent = ptr(*resp.ReasoningContent)
	}
	if len(resp.ReasoningDetails) > 0 && string(resp.ReasoningDetails) != "null" {
		m.ReasoningDetails = ptr(string(resp.ReasoningDetails))
	}
	return s.createMessage(ctx, m)
}

// finishSession closes an agent session: resets the turn counter, updates
// the parent registry, and delivers the outcome to blocking waiters or —
// for implicit parent communication — queues it to the parent. Session end
// never terminates the thread (Runtime §10).
func (rt *Runtime) finishSession(ctx context.Context, threadID string, out SessionOutcome) error {
	rec, err := rt.updateThread(ctx, threadID, func(r *ThreadRecord) error {
		r.SessionTurns = 0
		return nil
	})
	if err != nil {
		return err
	}
	if rec.ParentID == "" {
		rt.resolveWaiters(threadID, out)
		return nil
	}
	st := ChildIdle
	if out.Kind == OutcomeFailed {
		st = ChildFailed
	}
	if err := rt.updateRegistryEntry(ctx, rec.ParentID, rec.ParentReference, func(e *SubagentRegistryEntry) {
		if e.Status != ChildTerminated {
			e.Status = st
		}
	}); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if rt.resolveWaiters(threadID, out) {
		return nil // a blocking caller receives the result inline
	}
	if rec.ParentCommunication == ParentCommExplicit {
		return nil // Runtime §7.2.1: never auto-queue for explicit children
	}
	var content, kind string
	if out.Kind == OutcomeCompleted {
		content = fmt.Sprintf("Subagent (reference: %s) has returned the following result:\n\n%s", rec.ParentReference, out.Result)
		kind = "subagent_result"
	} else {
		msg := out.Error
		if msg == "" {
			msg = out.Result
		}
		content = fmt.Sprintf("Subagent (reference: %s) has failed (%s):\n\n%s", rec.ParentReference, out.Reason, msg)
		kind = "subagent_failure"
	}
	return rt.queueToParent(ctx, rec, content, out.Attachments, kind)
}

// waitSession registers a waiter for the child's next session close.
func (rt *Runtime) waitSession(childID string) chan SessionOutcome {
	ch := make(chan SessionOutcome, 1)
	rt.mu.Lock()
	rt.waiters[childID] = append(rt.waiters[childID], ch)
	rt.mu.Unlock()
	return ch
}

func (rt *Runtime) cancelWait(childID string, ch chan SessionOutcome) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	ws := rt.waiters[childID]
	for i, w := range ws {
		if w == ch {
			rt.waiters[childID] = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(rt.waiters[childID]) == 0 {
		delete(rt.waiters, childID)
	}
}

// resolveWaiters delivers an outcome; it reports whether anyone was waiting.
func (rt *Runtime) resolveWaiters(childID string, out SessionOutcome) bool {
	rt.mu.Lock()
	ws := rt.waiters[childID]
	delete(rt.waiters, childID)
	rt.mu.Unlock()
	for _, w := range ws {
		w <- out
	}
	return len(ws) > 0
}
