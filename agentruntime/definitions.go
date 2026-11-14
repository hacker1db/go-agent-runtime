package agentruntime

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// ---- Agents ----

// AgentDefinition is a dual-sided agent (Agents §2).
type AgentDefinition struct {
	Name            string
	SideA           SideConfig
	SideB           SideConfig
	MaxSessionTurns int // 0 = unset
	Title           string
	Description     string
	Icon            string
	Primary         *bool
	ExposeAsTool    bool
	ToolDescription string
	Env             map[string]string
	Hooks           []string
	ParentHooks     []ParentHookRequirement
	PackageName     string // namespace for qualified hook/tool resolution
}

// Side returns the configuration of the given side.
func (a *AgentDefinition) Side(s Side) *SideConfig {
	if s == SideA {
		return &a.SideA
	}
	return &a.SideB
}

// humanSide reports which side (if any) is human-backed.
func (a *AgentDefinition) humanSide() (Side, bool) {
	if a.SideA.Human {
		return SideA, true
	}
	if a.SideB.Human {
		return SideB, true
	}
	return "", false
}

// SideConfig is discriminated by backing: exactly one of Human, Prompt, Handler.
type SideConfig struct {
	Human   bool
	Prompt  string
	Handler string
	Label   string

	Subagents []SubagentRelationship

	// Prompt-side only.
	StopOnResponse           *bool // default true
	StopTool                 string
	StopToolResponseProperty string
	MaxSteps                 int
	SessionStop              *SessionToolBinding
	SessionFail              *SessionToolBinding
	SessionStatus            *SessionToolBinding
}

func (s *SideConfig) stopOnResponse() bool { return s.StopOnResponse == nil || *s.StopOnResponse }

func (s *SideConfig) backings() int {
	n := 0
	if s.Human {
		n++
	}
	if s.Prompt != "" {
		n++
	}
	if s.Handler != "" {
		n++
	}
	return n
}

// SessionToolBinding binds a lifecycle action to a tool (Agents §3).
type SessionToolBinding struct {
	Name                string
	MessageProperty     string
	AttachmentsProperty string
}

// ParentHookRequirement is a hook an exposed agent contributes to its direct
// parent composition (Agents §6).
type ParentHookRequirement struct {
	Hook   string
	Reason string
	Stage  string // "before_parent" | "after_parent" (default)
}

// ---- Subagents ----

// Parent communication modes.
const (
	ParentCommImplicit = "implicit"
	ParentCommExplicit = "explicit"
)

// SubagentRelationship is a side-declared child relationship (Subagents §1).
type SubagentRelationship struct {
	Agent                   string
	As                      string
	Description             string
	MaxInstances            int              // 0 = unbounded
	Immediate               *ImmediateConfig // nil = not immediate; &ImmediateConfig{} = `immediate: true`
	Blocking                bool
	ParentCommunication     string // "" = implicit
	ScopedEnv               []string
	InitUserMessageProperty string
	InitAttachmentsProperty string
	InitAgentNameProperty   string
	Optional                string
	Hidden                  bool
}

// ImmediateConfig is the object form of `immediate`.
type ImmediateConfig struct {
	NameEnv        string
	DescriptionEnv string
	ScopedEnv      []string
}

// Name returns the relationship name (`as`, defaulting to the unqualified agent name).
func (r *SubagentRelationship) Name() string {
	if r.As != "" {
		return r.As
	}
	return unqualified(r.Agent)
}

func (r *SubagentRelationship) parentComm() string {
	if r.ParentCommunication == ParentCommExplicit {
		return ParentCommExplicit
	}
	return ParentCommImplicit
}

func unqualified(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// Reserved lifecycle tool names.
const (
	ToolSubagents       = "subagents"
	ToolSubagentCreate  = "subagent_create"
	ToolSubagentMessage = "subagent_message"
)

func isReservedLifecycleName(n string) bool {
	return n == ToolSubagents || n == ToolSubagentCreate || n == ToolSubagentMessage
}

// ---- Prompts ----

// PromptDefinition drives a prompt-backed side or a prompt tool.
type PromptDefinition struct {
	Name            string
	Model           string
	System          string // supports {{var}} interpolation and {{>prompt}} includes
	ToolDescription string
	Tools           []PromptToolRef
	Hooks           []string
	RequiredSchema  map[string]any // JSON Schema for invocation arguments
	Variables       []VariableDefinition
	Env             map[string]string
	MaxSteps        int // prompt-tool step cap (0 = runtime hard cap)
	Reasoning       *ReasoningConfig
}

// ReasoningConfig controls persisted reasoning replay.
type ReasoningConfig struct {
	Include *bool // default true
}

func (p *PromptDefinition) includeReasoning() bool {
	return p.Reasoning == nil || p.Reasoning.Include == nil || *p.Reasoning.Include
}

// PromptToolRef names a tool, prompt, or exposed agent in a prompt's tools.
// Prompt-tool shaping fields apply only when Name refers to a prompt.
type PromptToolRef struct {
	Name                    string
	IncludeTextResponse     *bool // default true
	IncludeToolCalls        *bool // default false
	IncludeErrors           *bool // default true
	InitUserMessageProperty string
	InitAttachmentsProperty string
}

// VariableDefinition declares an environment variable (Tools §3).
type VariableDefinition struct {
	Name        string
	Type        string // "text" | "secret"
	Required    bool
	Scoped      bool
	Description string
}

// ---- Tools ----

// ToolResult is the outcome of a tool call.
type ToolResult struct {
	Status      string          `json:"status"` // "success" | "error"
	Result      any             `json:"result,omitempty"`
	Error       string          `json:"error,omitempty"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
}

// ToolFunc implements a function tool.
type ToolFunc func(ctx context.Context, state *ThreadState, args map[string]any) (ToolResult, error)

// ToolDefinition is a function tool (Tools §2).
type ToolDefinition struct {
	Name        string
	Description string
	Args        map[string]any // JSON Schema (object)
	Execute     ToolFunc
	Variables   []VariableDefinition
	// Uses restricts invokeTool/queueTool targets. nil = unrestricted;
	// a non-nil empty slice permits no targets.
	Uses             []string
	ProgressArgument string
	Annotations      map[string]any // preserved, never interpreted
}

// ---- Models / handlers / effects ----

// ModelDefinition maps a model name to a provider.
type ModelDefinition struct {
	Name                    string
	Provider                string
	Model                   string
	SupportsVision          bool
	AcceptsReasoningDetails bool
	Params                  map[string]any
}

// HandlerTurn is the input to a handler invocation.
type HandlerTurn struct {
	Side    Side
	Inbound []Message // messages delivered for this turn, in order
}

// HandlerDefinition backs a handler side (one invocation per turn).
type HandlerDefinition struct {
	Name    string
	Execute func(ctx context.Context, state *ThreadState, turn HandlerTurn) error
}

// EffectDefinition is a schedulable effect.
type EffectDefinition struct {
	Name    string
	Execute func(ctx context.Context, state *ThreadState, args map[string]any) error
}

// ---- Registry ----

// Registry holds all definitions available to a runtime.
type Registry struct {
	mu        sync.RWMutex
	agents    map[string]*AgentDefinition
	prompts   map[string]*PromptDefinition
	tools     map[string]*ToolDefinition
	models    map[string]*ModelDefinition
	handlers  map[string]*HandlerDefinition
	effects   map[string]*EffectDefinition
	hooks     map[string]*Hook
	providers map[string]Provider
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		agents:    map[string]*AgentDefinition{},
		prompts:   map[string]*PromptDefinition{},
		tools:     map[string]*ToolDefinition{},
		models:    map[string]*ModelDefinition{},
		handlers:  map[string]*HandlerDefinition{},
		effects:   map[string]*EffectDefinition{},
		hooks:     map[string]*Hook{},
		providers: map[string]Provider{},
	}
}

func (r *Registry) AddAgent(a AgentDefinition) { r.mu.Lock(); r.agents[a.Name] = &a; r.mu.Unlock() }
func (r *Registry) AddPrompt(p PromptDefinition) {
	r.mu.Lock()
	r.prompts[p.Name] = &p
	r.mu.Unlock()
}
func (r *Registry) AddTool(t ToolDefinition) { r.mu.Lock(); r.tools[t.Name] = &t; r.mu.Unlock() }
func (r *Registry) AddModel(m ModelDefinition) {
	r.mu.Lock()
	r.models[m.Name] = &m
	r.mu.Unlock()
}
func (r *Registry) AddHandler(h HandlerDefinition) {
	r.mu.Lock()
	r.handlers[h.Name] = &h
	r.mu.Unlock()
}
func (r *Registry) AddEffect(e EffectDefinition) {
	r.mu.Lock()
	r.effects[e.Name] = &e
	r.mu.Unlock()
}

// AddHook registers a hook under its ID, qualified by pkg when non-empty
// ("pkg/id"), so child-required hooks resolve in the child's namespace.
func (r *Registry) AddHook(pkg string, h Hook) {
	r.mu.Lock()
	r.hooks[qualify(pkg, h.ID)] = &h
	r.mu.Unlock()
}
func (r *Registry) AddProvider(name string, p Provider) {
	r.mu.Lock()
	r.providers[name] = p
	r.mu.Unlock()
}

func qualify(pkg, id string) string {
	if pkg == "" {
		return id
	}
	return pkg + "/" + id
}

func (r *Registry) Agent(n string) *AgentDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.agents[n]
}
func (r *Registry) Prompt(n string) *PromptDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.prompts[n]
}
func (r *Registry) Tool(n string) *ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tools[n]
}
func (r *Registry) Model(n string) *ModelDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.models[n]
}
func (r *Registry) Handler(n string) *HandlerDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.handlers[n]
}
func (r *Registry) Effect(n string) *EffectDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.effects[n]
}
func (r *Registry) hook(id string) *Hook {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hooks[id]
}
func (r *Registry) Provider(n string) Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[n]
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// insertion sort keeps this dependency-free and small maps are typical
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// AgentNames, PromptNames, ModelNames list registered definitions.
func (r *Registry) AgentNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedKeys(r.agents)
}
func (r *Registry) PromptNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedKeys(r.prompts)
}
func (r *Registry) ModelNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedKeys(r.models)
}

// scopedVarNames returns variable names declared scoped anywhere reachable
// from the agent's prompts and their tools.
func (r *Registry) agentVariables(agent *AgentDefinition) []VariableDefinition {
	var out []VariableDefinition
	seenPrompts := map[string]bool{}
	var walkPrompt func(name string)
	walkPrompt = func(name string) {
		if seenPrompts[name] {
			return
		}
		seenPrompts[name] = true
		p := r.Prompt(name)
		if p == nil {
			return
		}
		out = append(out, p.Variables...)
		for _, ref := range p.Tools {
			if t := r.Tool(ref.Name); t != nil {
				out = append(out, t.Variables...)
			} else if r.Prompt(ref.Name) != nil {
				walkPrompt(ref.Name)
			}
		}
	}
	for _, s := range []*SideConfig{&agent.SideA, &agent.SideB} {
		if s.Prompt != "" {
			walkPrompt(s.Prompt)
		}
	}
	return out
}

func (r *Registry) isScopedVar(agent *AgentDefinition, name string) bool {
	if agent == nil {
		return false
	}
	for _, v := range r.agentVariables(agent) {
		if v.Name == name && v.Scoped {
			return true
		}
	}
	return false
}

func (r *Registry) varType(agent *AgentDefinition, name string) string {
	if agent != nil {
		for _, v := range r.agentVariables(agent) {
			if v.Name == name && v.Type == "secret" {
				return "secret"
			}
		}
	}
	return "text"
}

// requiredScopedVars lists required scoped variables for an agent.
func (r *Registry) requiredScopedVars(agent *AgentDefinition) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range r.agentVariables(agent) {
		if v.Required && v.Scoped && !seen[v.Name] {
			seen[v.Name] = true
			out = append(out, v.Name)
		}
	}
	return out
}

// errorf helper
func errf(format string, a ...any) error { return fmt.Errorf("agentruntime: "+format, a...) }
