package agentruntime

import (
	"context"
	"fmt"
)

// HookName identifies a hook position (Hooks §1.1).
type HookName string

const (
	HookAfterThreadCreated   HookName = "after_thread_created"
	HookAfterSubagentCreated HookName = "after_subagent_created"
	HookAfterSystemMessage   HookName = "after_system_message"
	HookFilterMessages       HookName = "filter_messages"
	HookPrefilterLLMHistory  HookName = "prefilter_llm_history"
	HookBeforeCreateMessage  HookName = "before_create_message"
	HookAfterCreateMessage   HookName = "after_create_message"
	HookBeforeUpdateMessage  HookName = "before_update_message"
	HookAfterUpdateMessage   HookName = "after_update_message"
	HookBeforeStoreToolRes   HookName = "before_store_tool_result"
	HookToolCallStarted      HookName = "tool_call_started"
	HookAfterToolCallSuccess HookName = "after_tool_call_success"
	HookAfterToolCallFailure HookName = "after_tool_call_failure"
	HookToolCallDone         HookName = "tool_call_done"
)

// Hook scopes.
const (
	HookScopeThread = "thread"
	HookScopeUser   = "user"
)

// HookToolCall is the tool call passed to tool hooks.
type HookToolCall = ToolCall

// HookToolResult is the tool result passed to tool hooks.
type HookToolResult = ToolResult

// Hook is a typed hook definition. Exactly the function matching Name must
// be set.
type Hook struct {
	ID    string
	Name  HookName
	Scope string // "thread" (default) | "user"

	AfterThreadCreated   func(ctx context.Context, s *ThreadState) error
	AfterSubagentCreated func(ctx context.Context, s *ThreadState, child SubagentRegistryEntry) error
	// AfterSystemMessage returns a replacement (non-nil) or nil to keep.
	AfterSystemMessage   func(ctx context.Context, s *ThreadState, system string) (*string, error)
	FilterMessages       func(ctx context.Context, s *ThreadState, msgs []Message) ([]Message, error)
	PrefilterLLMHistory  func(ctx context.Context, s *ThreadState, msgs []LLMMessage) ([]LLMMessage, error)
	BeforeCreateMessage  func(ctx context.Context, s *ThreadState, m Message) (Message, error)
	AfterCreateMessage   func(ctx context.Context, s *ThreadState, m Message) error
	BeforeUpdateMessage  func(ctx context.Context, s *ThreadState, id string, updates map[string]any) (map[string]any, error)
	AfterUpdateMessage   func(ctx context.Context, s *ThreadState, m Message) error
	BeforeStoreToolRes   func(ctx context.Context, s *ThreadState, tc HookToolCall, r HookToolResult) (HookToolResult, error)
	ToolCallStarted      func(ctx context.Context, s *ThreadState, tc HookToolCall, progress *string) error
	AfterToolCallSuccess func(ctx context.Context, s *ThreadState, tc HookToolCall, r HookToolResult) (*HookToolResult, error)
	AfterToolCallFailure func(ctx context.Context, s *ThreadState, tc HookToolCall, r HookToolResult) (*HookToolResult, error)
	ToolCallDone         func(ctx context.Context, s *ThreadState, tc HookToolCall, r HookToolResult) error
}

func (h *Hook) implemented() bool {
	switch h.Name {
	case HookAfterThreadCreated:
		return h.AfterThreadCreated != nil
	case HookAfterSubagentCreated:
		return h.AfterSubagentCreated != nil
	case HookAfterSystemMessage:
		return h.AfterSystemMessage != nil
	case HookFilterMessages:
		return h.FilterMessages != nil
	case HookPrefilterLLMHistory:
		return h.PrefilterLLMHistory != nil
	case HookBeforeCreateMessage:
		return h.BeforeCreateMessage != nil
	case HookAfterCreateMessage:
		return h.AfterCreateMessage != nil
	case HookBeforeUpdateMessage:
		return h.BeforeUpdateMessage != nil
	case HookAfterUpdateMessage:
		return h.AfterUpdateMessage != nil
	case HookBeforeStoreToolRes:
		return h.BeforeStoreToolRes != nil
	case HookToolCallStarted:
		return h.ToolCallStarted != nil
	case HookAfterToolCallSuccess:
		return h.AfterToolCallSuccess != nil
	case HookAfterToolCallFailure:
		return h.AfterToolCallFailure != nil
	case HookToolCallDone:
		return h.ToolCallDone != nil
	}
	return false
}

// ErrHookComposition is returned when a required parent hook cannot be
// resolved; composition fails closed (Runtime §7.3.1).
type ErrHookComposition struct {
	Child  string
	HookID string
	Reason string
}

func (e *ErrHookComposition) Error() string {
	return fmt.Sprintf("agentruntime: required parent hook %q from child agent %q: %s", e.HookID, e.Child, e.Reason)
}

// childHookRequirements resolves the before/after_parent hooks contributed
// by the agent's enabled direct relationships on both sides, side A then
// side B, in declaration order. Hidden relationships still contribute.
func (rt *Runtime) childHookRequirements(ctx context.Context, s *ThreadState, agent *AgentDefinition) (before, after []*Hook, err error) {
	for _, side := range []Side{SideA, SideB} {
		for i := range agent.Side(side).Subagents {
			rel := &agent.Side(side).Subagents[i]
			if !rt.relationshipEnabled(ctx, s, rel) {
				continue
			}
			child := rt.reg.Agent(rel.Agent)
			if child == nil {
				return nil, nil, &ErrHookComposition{Child: rel.Agent, Reason: "child agent is not defined"}
			}
			for _, req := range child.ParentHooks {
				h := rt.reg.hook(qualify(child.PackageName, req.Hook))
				switch {
				case h == nil:
					return nil, nil, &ErrHookComposition{Child: child.Name, HookID: req.Hook, Reason: "hook cannot be loaded"}
				case h.Scope == HookScopeUser:
					return nil, nil, &ErrHookComposition{Child: child.Name, HookID: req.Hook, Reason: "user-scoped hooks cannot be required on a parent"}
				case !h.implemented():
					return nil, nil, &ErrHookComposition{Child: child.Name, HookID: req.Hook, Reason: "hook has no implementation for " + string(h.Name)}
				}
				if req.Stage == "before_parent" {
					before = append(before, h)
				} else {
					after = append(after, h)
				}
			}
		}
	}
	return before, after, nil
}

// resolveHooks returns the complete, ordered hook set for one hook position
// (Hooks §1.3): child before_parent, then prompt hooks (or agent fallback),
// then child after_parent.
func (rt *Runtime) resolveHooks(ctx context.Context, s *ThreadState, name HookName) ([]*Hook, error) {
	rec, err := s.record(ctx)
	if err != nil {
		return nil, err
	}
	agent := rt.reg.Agent(rec.AgentID)
	if agent == nil {
		return nil, errf("agent %q not defined", rec.AgentID)
	}
	before, after, err := rt.childHookRequirements(ctx, s, agent)
	if err != nil {
		return nil, err
	}
	var base []string
	if p := s.activePrompt(); p != nil && len(p.Hooks) > 0 {
		base = p.Hooks
	} else {
		base = agent.Hooks
	}
	var out []*Hook
	add := func(h *Hook) {
		if h != nil && h.Name == name && h.implemented() {
			out = append(out, h)
		}
	}
	for _, h := range before {
		add(h)
	}
	for _, id := range base {
		add(rt.reg.hook(id))
	}
	for _, h := range after {
		add(h)
	}
	return out, nil
}

func (rt *Runtime) logHookErr(name HookName, id string, err error) {
	if err != nil {
		rt.cfg.Logger.Warn("hook failed", "hook", string(name), "id", id, "err", err)
	}
}
