package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// HookBlockedError lets a request-context hook act as a policy gate that
// refuses to let a request proceed (settles with reason hook_blocked).
type HookBlockedError struct{ Reason string }

func (e *HookBlockedError) Error() string {
	return "agentruntime: request blocked by hook: " + e.Reason
}

// contextScope selects the eligible history for an assembly. The zero value
// selects top-level (depth 0) history; a non-empty runID selects one
// prompt-tool run.
type contextScope struct {
	runID string
}

func (c contextScope) eligible(m Message) bool {
	if m.Status != nil && *m.Status == MessageStatusPending {
		return false
	}
	if c.runID == "" {
		return m.Depth == 0
	}
	r, _ := m.Metadata[MetaSubpromptRun].(string)
	return r == c.runID
}

// assembleContext builds one provider request in the order mandated by
// Runtime §4: interpolate system prompt -> after_system_message -> stable
// snapshot -> filter_messages (once) -> provider transform ->
// prefilter_llm_history -> (caller dispatches).
func (rt *Runtime) assembleContext(ctx context.Context, s *ThreadState, side Side, prompt *PromptDefinition, model *ModelDefinition, surface *toolSurface, scope contextScope) (ProviderRequest, error) {
	rec, err := s.record(ctx)
	if err != nil {
		return ProviderRequest{}, err
	}
	// 1. Interpolate the system prompt.
	system, err := rt.renderPrompt(prompt, rec.Arguments, map[string]bool{})
	if err != nil {
		return ProviderRequest{}, err
	}
	// 2. after_system_message.
	hooks, err := rt.resolveHooks(ctx, s, HookAfterSystemMessage)
	if err != nil {
		return ProviderRequest{}, err
	}
	for _, h := range hooks {
		repl, err := h.AfterSystemMessage(ctx, s, system)
		if err != nil {
			return ProviderRequest{}, hookErr(h, err)
		}
		if repl != nil {
			system = *repl
		}
	}
	// 3. Stable snapshot of eligible persisted history (silent included).
	all, err := rt.store.ListMessages(ctx, s.threadID)
	if err != nil {
		return ProviderRequest{}, err
	}
	snapshot := make([]Message, 0, len(all))
	for _, m := range all {
		if scope.eligible(m) {
			snapshot = append(snapshot, m)
		}
	}
	sortMessages(snapshot)
	// 4. filter_messages exactly once on the snapshot.
	filtered := snapshot
	fhooks, err := rt.resolveHooks(ctx, s, HookFilterMessages)
	if err != nil {
		return ProviderRequest{}, err
	}
	for _, h := range fhooks {
		filtered, err = h.FilterMessages(ctx, s, cloneJSON(filtered))
		if err != nil {
			return ProviderRequest{}, hookErr(h, err)
		}
	}
	// 5. Transform into provider messages.
	llm, err := rt.toProviderMessages(ctx, s, side, prompt, model, system, filtered)
	if err != nil {
		return ProviderRequest{}, err
	}
	// 6. prefilter_llm_history.
	phooks, err := rt.resolveHooks(ctx, s, HookPrefilterLLMHistory)
	if err != nil {
		return ProviderRequest{}, err
	}
	for _, h := range phooks {
		llm, err = h.PrefilterLLMHistory(ctx, s, llm)
		if err != nil {
			return ProviderRequest{}, hookErr(h, err)
		}
	}
	return ProviderRequest{
		Model:    model.Model,
		Messages: llm,
		Tools:    surface.specs(),
		Params:   model.Params,
	}, nil
}

func hookErr(h *Hook, err error) error {
	var blocked *HookBlockedError
	if errors.As(err, &blocked) {
		return err
	}
	return fmt.Errorf("agentruntime: %s hook %q: %w", h.Name, h.ID, err)
}

// toProviderMessages applies perspective mapping (Runtime §4): the side's
// own turns appear as assistant, the opposite side's as user, and the
// opposite side's tool calls/results are not visible.
func (rt *Runtime) toProviderMessages(ctx context.Context, s *ThreadState, side Side, prompt *PromptDefinition, model *ModelDefinition, system string, msgs []Message) ([]LLMMessage, error) {
	out := []LLMMessage{}
	if system != "" {
		out = append(out, LLMMessage{Role: RoleSystem, Content: system})
	}
	includeReasoning := prompt.includeReasoning()
	for _, m := range msgs {
		own := m.authorSide() == side
		switch m.Role {
		case RoleSystem:
			out = append(out, LLMMessage{Role: RoleSystem, Content: m.ContentString()})
		case RoleTool:
			if !own {
				continue
			}
			out = append(out, LLMMessage{Role: RoleTool, Content: m.ContentString(), Name: deref(m.Name), ToolCallID: deref(m.ToolCallID)})
		case RoleUser, RoleAssistant:
			parts, err := rt.attachmentParts(ctx, s, model, m)
			if err != nil {
				return nil, err
			}
			if own {
				lm := LLMMessage{Role: RoleAssistant, Content: m.ContentString(), Parts: parts}
				calls, err := m.DecodeToolCalls()
				if err != nil {
					return nil, fmt.Errorf("agentruntime: message %s: bad tool_calls: %w", m.ID, err)
				}
				lm.ToolCalls = calls
				if includeReasoning {
					lm.ReasoningContent = deref(m.ReasoningContent)
					if m.ReasoningDetails != nil && model.AcceptsReasoningDetails {
						lm.ReasoningDetails = json.RawMessage(*m.ReasoningDetails)
					}
				}
				if lm.Content == "" && len(lm.ToolCalls) == 0 && len(parts) == 0 {
					continue
				}
				out = append(out, lm)
			} else {
				if m.ContentString() == "" && len(parts) == 0 {
					continue
				}
				out = append(out, LLMMessage{Role: RoleUser, Content: m.ContentString(), Parts: parts})
			}
		}
	}
	return out, nil
}

// attachmentParts keeps attachments as multimodal content. Image parts are
// dropped at request time for non-vision models; stored attachments are
// never modified.
func (rt *Runtime) attachmentParts(ctx context.Context, s *ThreadState, model *ModelDefinition, m Message) ([]ContentPart, error) {
	var parts []ContentPart
	for i := range m.Attachments {
		a := m.Attachments[i]
		isImage := strings.HasPrefix(a.MimeType, "image/")
		if isImage && !model.SupportsVision {
			continue
		}
		p := ContentPart{Type: "file", Attachment: &a}
		if isImage {
			p.Type = "image"
			data, err := s.ReadFile(ctx, a.Path)
			if err != nil {
				return nil, err
			}
			p.Data = data
		}
		parts = append(parts, p)
	}
	return parts, nil
}

var (
	varRe     = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)
	includeRe = regexp.MustCompile(`\{\{>\s*([A-Za-z0-9_./@-]+)\s*\}\}`)
)

// renderPrompt interpolates top-level argument keys ({{key}}) and included
// prompts ({{>name}}). Included prompts see the same arguments.
func (rt *Runtime) renderPrompt(p *PromptDefinition, args map[string]any, seen map[string]bool) (string, error) {
	if seen[p.Name] {
		return "", errf("prompt include cycle at %q", p.Name)
	}
	seen[p.Name] = true
	defer delete(seen, p.Name)
	var ierr error
	text := includeRe.ReplaceAllStringFunc(p.System, func(tok string) string {
		name := includeRe.FindStringSubmatch(tok)[1]
		inc := rt.reg.Prompt(name)
		if inc == nil {
			ierr = errf("included prompt %q not defined", name)
			return ""
		}
		r, err := rt.renderPrompt(inc, args, seen)
		if err != nil {
			ierr = err
		}
		return r
	})
	if ierr != nil {
		return "", ierr
	}
	return varRe.ReplaceAllStringFunc(text, func(tok string) string {
		key := varRe.FindStringSubmatch(tok)[1]
		v, ok := args[key]
		if !ok {
			return tok
		}
		if s, ok := v.(string); ok {
			return s
		}
		b, _ := json.Marshal(v)
		return string(b)
	}), nil
}
