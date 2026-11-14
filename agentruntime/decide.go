package agentruntime

import (
	"context"
	"fmt"
	"strconv"
)

// Decision inference (Decisions spec; Runtime §1.1).

// DecisionContext is a string, JSON array, or JSON object. Structured values
// retain their JSON structure.
type DecisionContext = any

// DecisionQuestion is a typed question.
type DecisionQuestion struct {
	Type         string                     `json:"type"` // "choice" | "score" | "boolean"
	Instructions DecisionContext            `json:"instructions"`
	Options      map[string]DecisionContext `json:"options,omitempty"` // choice; nil value = no description
	Levels       []DecisionContext          `json:"levels,omitempty"`  // score
	Criteria     *DecisionCriteria          `json:"criteria,omitempty"`
}

// DecisionCriteria describes boolean outcomes.
type DecisionCriteria struct {
	True  DecisionContext `json:"true,omitempty"`
	False DecisionContext `json:"false,omitempty"`
}

// DecisionRequest is a provider decision request.
type DecisionRequest struct {
	Model     string                      `json:"model"`
	Context   DecisionContext             `json:"context"`
	Questions map[string]DecisionQuestion `json:"questions"`
}

// DecisionAnswer is a typed answer. Fields are populated per Type.
type DecisionAnswer struct {
	Type          string                     `json:"type"`
	Choice        string                     `json:"choice,omitempty"`
	Score         float64                    `json:"score,omitempty"`
	Probability   *float64                   `json:"probability,omitempty"`
	Probabilities map[string]float64         `json:"probabilities,omitempty"`
	Legend        map[string]DecisionContext `json:"legend,omitempty"`
	Confidence    *float64                   `json:"confidence,omitempty"`
}

// DecisionResult is returned by decide().
type DecisionResult struct {
	Model    string                    `json:"model"`
	Answers  map[string]DecisionAnswer `json:"answers"`
	Usage    ProviderUsage             `json:"usage"`
	Metadata map[string]any            `json:"metadata,omitempty"`
}

// DecisionProvider is a decision provider (kind: "decision", spec version "1").
type DecisionProvider interface {
	Name() string
	Decide(ctx context.Context, req DecisionRequest) (*DecisionResult, error)
}

// ProviderFactoryConfig is passed to provider factories. Credentials are
// resolved by the runtime from thread-scoped configuration.
type ProviderFactoryConfig struct {
	ThreadID string
	Env      func(ctx context.Context, name string) (string, error)
}

// DecisionProviderFactory builds a decision provider.
type DecisionProviderFactory func(cfg ProviderFactoryConfig) DecisionProvider

// DecideOptions are the options for ThreadState.Decide.
type DecideOptions struct {
	DecisionRequest
	Provider DecisionProviderFactory
}

// Decide evaluates typed questions against explicit context. It never reads
// or writes conversation messages, executes tools, or starts a turn, and it
// is available whether or not the thread is executing.
func (s *ThreadState) Decide(ctx context.Context, opts DecideOptions) (*DecisionResult, error) {
	if opts.Provider == nil {
		return nil, errf("decide: provider is required")
	}
	if err := validateDecisionRequest(opts.DecisionRequest); err != nil {
		return nil, err
	}
	p := opts.Provider(ProviderFactoryConfig{ThreadID: s.threadID, Env: s.Env})
	if p == nil {
		return nil, errf("decide: provider factory returned nil")
	}
	res, err := p.Decide(ctx, opts.DecisionRequest)
	if err != nil {
		return nil, err
	}
	if err := validateDecisionResult(opts.DecisionRequest, res); err != nil {
		return nil, err
	}
	return res, nil
}

func validateDecisionRequest(r DecisionRequest) error {
	if r.Model == "" {
		return errf("decide: model is required")
	}
	if r.Context == nil {
		return errf("decide: context is required")
	}
	if !isDecisionContext(r.Context) {
		return errf("decide: context must be a string, array, or object")
	}
	if len(r.Questions) == 0 {
		return errf("decide: at least one question is required")
	}
	for id, q := range r.Questions {
		if q.Instructions == nil || !isDecisionContext(q.Instructions) {
			return errf("decide: question %q: instructions must be a string, array, or object", id)
		}
		switch q.Type {
		case "choice":
			if len(q.Options) == 0 {
				return errf("decide: question %q: choice requires options", id)
			}
		case "score":
			if len(q.Levels) == 0 {
				return errf("decide: question %q: score requires levels", id)
			}
		case "boolean":
		default:
			return errf("decide: question %q: unknown type %q", id, q.Type)
		}
	}
	return nil
}

func isDecisionContext(v any) bool {
	switch v.(type) {
	case string, []any, map[string]any:
		return true
	}
	return false
}

func inUnit(f float64) bool { return f >= 0 && f <= 1 }

func validateDecisionResult(req DecisionRequest, res *DecisionResult) error {
	if res == nil {
		return errf("decide: provider returned nil result")
	}
	for id, q := range req.Questions {
		a, ok := res.Answers[id]
		if !ok {
			return errf("decide: provider omitted answer for %q", id)
		}
		if a.Type != q.Type {
			return errf("decide: answer %q has type %q, want %q", id, a.Type, q.Type)
		}
		if a.Confidence != nil && !inUnit(*a.Confidence) {
			return errf("decide: answer %q confidence out of [0,1]", id)
		}
		for k, p := range a.Probabilities {
			if !inUnit(p) {
				return errf("decide: answer %q probability for %q out of [0,1]", id, k)
			}
		}
		switch q.Type {
		case "choice":
			if _, ok := q.Options[a.Choice]; !ok {
				return errf("decide: answer %q chose unknown option %q", id, a.Choice)
			}
		case "score":
			if a.Score < 0 || a.Score > float64(len(q.Levels)-1) {
				return errf("decide: answer %q score %v outside level range", id, a.Score)
			}
			for k := range a.Probabilities {
				i, err := strconv.Atoi(k)
				if err != nil || i < 0 || i >= len(q.Levels) {
					return errf("decide: answer %q has probability for unknown level %q", id, k)
				}
			}
		case "boolean":
			if a.Probability == nil || !inUnit(*a.Probability) {
				return fmt.Errorf("agentruntime: decide: answer %q requires probability in [0,1]", id)
			}
		}
	}
	return nil
}
