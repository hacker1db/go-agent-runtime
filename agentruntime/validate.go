package agentruntime

import (
	"fmt"
	"strings"
)

// ValidationIssue is one structured definition-time problem (Agents §8).
type ValidationIssue struct {
	Code     string `json:"code"`
	Severity string `json:"severity"` // "error" | "warning"
	Message  string `json:"message"`
	Path     string `json:"path"`
}

// ValidationError wraps definition-time issues.
type ValidationError struct{ Issues []ValidationIssue }

func (e *ValidationError) Error() string {
	var b strings.Builder
	b.WriteString("agentruntime: invalid definitions:")
	for _, i := range e.Issues {
		fmt.Fprintf(&b, "\n  [%s] %s: %s", i.Code, i.Path, i.Message)
	}
	return b.String()
}

func hasErrors(issues []ValidationIssue) bool {
	for _, i := range issues {
		if i.Severity == "error" {
			return true
		}
	}
	return false
}

// Validation codes.
const (
	CodeBothSidesHuman          = "both_sides_human"
	CodeSubagentsOnHumanSide    = "subagents_on_human_side"
	CodeInvalidSideBacking      = "invalid_side_backing"
	CodeUndefinedHandler        = "undefined_handler"
	CodeUndefinedPrompt         = "undefined_prompt"
	CodeUndefinedModel          = "undefined_model"
	CodeUndefinedTool           = "undefined_tool"
	CodeUndefinedSubagentAgent  = "undefined_subagent_agent"
	CodeRelationshipBothSides   = "relationship_name_on_both_sides"
	CodeRelationshipDuplicate   = "relationship_name_duplicate"
	CodeRelationshipToolClash   = "relationship_name_tool_collision"
	CodeReservedRelationship    = "reserved_relationship_name"
	CodeRelationshipSlash       = "relationship_name_contains_slash"
	CodeNonPromptSideBinding    = "non_prompt_side_binding"
	CodeInvalidParentComm       = "invalid_parent_communication"
	CodeRequiredHookUnresolved  = "required_parent_hook_unresolvable"
	CodeDuplicateParentHook     = "duplicate_parent_hook"
	CodeHookNotImplemented      = "hook_not_implemented"
	CodeHandoffWithoutHumanSide = "handoff_without_human_side"
)

// Validate checks every registered agent, prompt, and hook.
func (r *Registry) Validate() []ValidationIssue {
	var out []ValidationIssue
	for _, n := range r.AgentNames() {
		out = append(out, r.ValidateAgent(r.Agent(n))...)
	}
	r.mu.RLock()
	hookIDs := sortedKeys(r.hooks)
	r.mu.RUnlock()
	for _, id := range hookIDs {
		if h := r.hook(id); !h.implemented() {
			out = append(out, ValidationIssue{CodeHookNotImplemented, "error", fmt.Sprintf("hook %q has no implementation for %q", id, h.Name), "hooks." + id})
		}
	}
	return out
}

// ValidateAgent applies the definition-time rules of Agents §8 and
// Subagents §2 to one agent.
func (r *Registry) ValidateAgent(a *AgentDefinition) []ValidationIssue {
	var out []ValidationIssue
	add := func(code, path, format string, args ...any) {
		out = append(out, ValidationIssue{Code: code, Severity: "error", Message: fmt.Sprintf(format, args...), Path: "agents." + a.Name + path})
	}
	if a.SideA.Human && a.SideB.Human {
		add(CodeBothSidesHuman, "", "both sides human is reserved in 0.2.0")
	}
	relSide := map[string]Side{}
	for _, side := range []Side{SideA, SideB} {
		cfg := a.Side(side)
		p := ".side" + strings.ToUpper(string(side))
		if cfg.backings() != 1 {
			add(CodeInvalidSideBacking, p, "exactly one of human, prompt, handler is required")
		}
		if cfg.Human && len(cfg.Subagents) > 0 {
			add(CodeSubagentsOnHumanSide, p+".subagents", "human-backed sides may not own subagent relationships")
		}
		if cfg.Prompt == "" && (cfg.SessionStop != nil || cfg.SessionFail != nil || cfg.SessionStatus != nil || cfg.MaxSteps != 0 || cfg.StopTool != "" || cfg.StopOnResponse != nil || cfg.StopToolResponseProperty != "") {
			add(CodeNonPromptSideBinding, p, "session/stop bindings and maxSteps are only legal on prompt-backed sides")
		}
		if cfg.Handler != "" && r.Handler(cfg.Handler) == nil {
			add(CodeUndefinedHandler, p+".handler", "handler %q is not defined", cfg.Handler)
		}
		toolNames := map[string]bool{}
		if cfg.Prompt != "" {
			prompt := r.Prompt(cfg.Prompt)
			if prompt == nil {
				add(CodeUndefinedPrompt, p+".prompt", "prompt %q is not defined", cfg.Prompt)
			} else {
				out = append(out, r.validatePrompt(prompt, "agents."+a.Name+p+".prompt", map[string]bool{})...)
				for _, t := range prompt.Tools {
					toolNames[unqualified(t.Name)] = true
				}
			}
			for _, b := range []*SessionToolBinding{cfg.SessionStop, cfg.SessionFail, cfg.SessionStatus} {
				if b != nil {
					toolNames[b.Name] = true
				}
			}
			if cfg.StopTool != "" {
				toolNames[cfg.StopTool] = true
			}
		}
		seen := map[string]bool{}
		for i := range cfg.Subagents {
			rel := &cfg.Subagents[i]
			rp := fmt.Sprintf("%s.subagents[%d]", p, i)
			name := rel.Name()
			switch {
			case isReservedLifecycleName(name):
				add(CodeReservedRelationship, rp, "relationship name %q is reserved for lifecycle tools", name)
			case strings.Contains(name, "/"):
				add(CodeRelationshipSlash, rp, "relationship name %q must not contain a slash", name)
			case toolNames[name]:
				add(CodeRelationshipToolClash, rp, "relationship name %q collides with a tool name", name)
			}
			if seen[name] {
				add(CodeRelationshipDuplicate, rp, "relationship name %q declared twice on one side", name)
			}
			seen[name] = true
			if other, ok := relSide[name]; ok && other != side {
				add(CodeRelationshipBothSides, rp, "relationship name %q appears on both sides", name)
			}
			relSide[name] = side
			if rel.ParentCommunication != "" && rel.ParentCommunication != ParentCommImplicit && rel.ParentCommunication != ParentCommExplicit {
				add(CodeInvalidParentComm, rp, "parentCommunication must be implicit or explicit")
			}
			child := r.Agent(rel.Agent)
			if child == nil {
				add(CodeUndefinedSubagentAgent, rp, "agent %q is not defined", rel.Agent)
				continue
			}
			ids := map[string]bool{}
			for _, req := range child.ParentHooks {
				if ids[req.Hook] {
					add(CodeDuplicateParentHook, rp, "child %q declares parent hook %q twice", child.Name, req.Hook)
				}
				ids[req.Hook] = true
				h := r.hook(qualify(child.PackageName, req.Hook))
				switch {
				case h == nil:
					add(CodeRequiredHookUnresolved, rp, "required parent hook %q from %q cannot be loaded", req.Hook, child.Name)
				case h.Scope == HookScopeUser:
					add(CodeRequiredHookUnresolved, rp, "required parent hook %q from %q is user-scoped", req.Hook, child.Name)
				case !h.implemented():
					add(CodeRequiredHookUnresolved, rp, "required parent hook %q from %q has no implementation", req.Hook, child.Name)
				}
			}
		}
	}
	return out
}

func (r *Registry) validatePrompt(p *PromptDefinition, path string, seen map[string]bool) []ValidationIssue {
	if seen[p.Name] {
		return nil
	}
	seen[p.Name] = true
	var out []ValidationIssue
	if r.Model(p.Model) == nil {
		out = append(out, ValidationIssue{CodeUndefinedModel, "error", fmt.Sprintf("model %q is not defined", p.Model), path + ".model"})
	}
	for i, t := range p.Tools {
		tp := fmt.Sprintf("%s.tools[%d]", path, i)
		switch {
		case r.Tool(t.Name) != nil:
		case r.Prompt(t.Name) != nil:
			out = append(out, r.validatePrompt(r.Prompt(t.Name), tp, seen)...)
		case r.Agent(t.Name) != nil:
			if a := r.Agent(t.Name); !a.ExposeAsTool {
				out = append(out, ValidationIssue{CodeUndefinedTool, "error", fmt.Sprintf("agent %q is not exposeAsTool", t.Name), tp})
			} else if _, ok := a.humanSide(); !ok {
				out = append(out, ValidationIssue{CodeHandoffWithoutHumanSide, "error", fmt.Sprintf("handoff target %q has no human side", t.Name), tp})
			}
		default:
			out = append(out, ValidationIssue{CodeUndefinedTool, "error", fmt.Sprintf("tool %q is not defined", t.Name), tp})
		}
	}
	return out
}
