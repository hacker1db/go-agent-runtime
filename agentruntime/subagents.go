package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// relationshipEnabled evaluates `optional` (Runtime §7.3): enabled only when
// the named env flag resolves to true, 1, or yes (case-insensitive).
func (rt *Runtime) relationshipEnabled(ctx context.Context, s *ThreadState, rel *SubagentRelationship) bool {
	if rel.Optional == "" {
		return true
	}
	v, err := s.Env(ctx, rel.Optional)
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	}
	return false
}

func (rt *Runtime) enabledRelationships(ctx context.Context, s *ThreadState, agent *AgentDefinition, side Side, includeHidden bool) []*SubagentRelationship {
	var out []*SubagentRelationship
	rels := agent.Side(side).Subagents
	for i := range rels {
		r := &rels[i]
		if r.Hidden && !includeHidden {
			continue
		}
		if rt.relationshipEnabled(ctx, s, r) {
			out = append(out, r)
		}
	}
	return out
}

// lifecycleTools builds the injected subagents / subagent_create /
// subagent_message tools for a side (Runtime §7.2). For model surfaces
// (includeHidden=false) hidden relationships are excluded entirely; code
// paths (includeHidden=true) may reach them.
func (rt *Runtime) lifecycleTools(ctx context.Context, s *ThreadState, agent *AgentDefinition, side Side, includeHidden bool) []*resolvedTool {
	if agent.Side(side).Human {
		return nil
	}
	rels := rt.enabledRelationships(ctx, s, agent, side, includeHidden)
	if len(rels) == 0 {
		return nil
	}
	names := make([]any, 0, len(rels))
	var desc strings.Builder
	for _, r := range rels {
		names = append(names, r.Name())
		d := r.Description
		if d == "" {
			if c := rt.reg.Agent(r.Agent); c != nil {
				d = c.ToolDescription
				if d == "" {
					d = c.Description
				}
			}
		}
		fmt.Fprintf(&desc, "\n- %s: %s", r.Name(), d)
	}
	attachments := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	return []*resolvedTool{
		{
			name:        ToolSubagents,
			description: "List your existing subagent instances (reference, relationship, agent, status, working status, initial arguments, creation time). Call this before subagent_create so existing instances are reused instead of duplicated.",
			schema:      map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
			kind:        kindLifecycle, ownerSide: side, rels: rels,
		},
		{
			name:        ToolSubagentCreate,
			description: "Create a subagent instance. Call `subagents` first to reuse existing instances. Available relationships:" + desc.String(),
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent":       map[string]any{"type": "string", "enum": names, "description": "Relationship name"},
					"name":        map[string]any{"type": "string", "minLength": 1, "description": "Human-readable name for this instance"},
					"arguments":   map[string]any{"type": "object", "description": "Invocation arguments defined by the child agent"},
					"message":     map[string]any{"type": "string", "description": "Initial message for the instance"},
					"attachments": attachments,
				},
				"required":             []any{"agent", "name"},
				"additionalProperties": false,
			},
			kind: kindLifecycle, ownerSide: side, rels: rels,
		},
		{
			name:        ToolSubagentMessage,
			description: "Send a message to an existing subagent instance by reference.",
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"agent":       map[string]any{"type": "string", "enum": names},
					"reference":   map[string]any{"type": "string", "minLength": 1},
					"message":     map[string]any{"type": "string", "minLength": 1},
					"attachments": attachments,
				},
				"required":             []any{"agent", "reference", "message"},
				"additionalProperties": false,
			},
			kind: kindLifecycle, ownerSide: side, rels: rels,
		},
	}
}

func findRel(rels []*SubagentRelationship, name string) *SubagentRelationship {
	for _, r := range rels {
		if r.Name() == name {
			return r
		}
	}
	return nil
}

// structuredError renders a tool error with a machine-readable code.
func structuredError(code, msg string, extra map[string]any) ToolResult {
	body := map[string]any{"code": code, "message": msg}
	for k, v := range extra {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return ToolResult{Status: ToolStatusError, Error: string(b), Result: body}
}

func (rt *Runtime) runLifecycleTool(ctx context.Context, s *ThreadState, t *resolvedTool, args map[string]any) ToolResult {
	rec, err := s.record(ctx)
	if err != nil {
		return ToolResult{Status: ToolStatusError, Error: err.Error()}
	}
	switch t.name {
	case ToolSubagents:
		allowed := map[string]bool{}
		for _, r := range t.rels {
			allowed[r.Name()] = true
		}
		list := []map[string]any{}
		for _, c := range rec.Children {
			if !allowed[c.Relationship] {
				continue
			}
			list = append(list, map[string]any{
				"reference": c.Reference, "relationship": c.Relationship, "agent": c.Name,
				"status": c.Status, "workingStatus": c.StatusText, "threadName": c.ThreadName,
				"initialArguments": c.InitialArguments, "createdAt": c.CreatedAt,
			})
		}
		return ToolResult{Status: ToolStatusSuccess, Result: list}
	case ToolSubagentCreate:
		relName, _ := args["agent"].(string)
		rel := findRel(t.rels, relName)
		if rel == nil {
			return structuredError("subagent_relationship_unavailable", fmt.Sprintf("relationship %q is not declared or not enabled", relName), nil)
		}
		return rt.createChild(ctx, s, t.ownerSide, rel, args, nil, false)
	case ToolSubagentMessage:
		relName, _ := args["agent"].(string)
		rel := findRel(t.rels, relName)
		if rel == nil {
			return structuredError("subagent_relationship_unavailable", fmt.Sprintf("relationship %q is not declared or not enabled", relName), nil)
		}
		ref, _ := args["reference"].(string)
		msg, _ := args["message"].(string)
		var entry *SubagentRegistryEntry
		for i := range rec.Children {
			if rec.Children[i].Reference == ref && rec.Children[i].Relationship == rel.Name() {
				entry = &rec.Children[i]
			}
		}
		if entry == nil {
			return structuredError("subagent_not_found", fmt.Sprintf("no %s instance with reference %q; call `subagents` to list instances", rel.Name(), ref), nil)
		}
		if entry.Status == ChildTerminated {
			return structuredError("subagent_terminated", fmt.Sprintf("instance %q is terminated; create a new one", ref), nil)
		}
		if err := rt.messageChild(ctx, rec, *entry, msg, args["attachments"]); err != nil {
			return ToolResult{Status: ToolStatusError, Error: err.Error()}
		}
		return ToolResult{Status: ToolStatusSuccess, Result: map[string]any{"reference": ref, "status": "queued"}}
	}
	return ToolResult{Status: ToolStatusError, Error: "unknown lifecycle tool"}
}

// createChild implements subagent_create, including argument validation,
// maxInstances / create-or-continue, scoped variable bootstrap, initial
// payload mapping, and blocking waits.
func (rt *Runtime) createChild(ctx context.Context, parent *ThreadState, side Side, rel *SubagentRelationship, args map[string]any, extraEnv map[string]EnvValue, forceAsync bool) ToolResult {
	childAgent := rt.reg.Agent(rel.Agent)
	if childAgent == nil {
		return structuredError("subagent_agent_undefined", fmt.Sprintf("agent %q is not defined", rel.Agent), nil)
	}
	name, _ := args["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return structuredError("subagent_name_required", "subagent_create requires a non-empty `name`", nil)
	}
	arguments, _ := args["arguments"].(map[string]any)
	if arguments == nil {
		arguments = map[string]any{}
	}
	// Validate against the child's side A prompt requiredSchema (§7.4).
	if childAgent.SideA.Prompt != "" {
		if p := rt.reg.Prompt(childAgent.SideA.Prompt); p != nil && p.RequiredSchema != nil {
			var missing []string
			for _, r := range toStrings(p.RequiredSchema["required"]) {
				if _, ok := arguments[r]; !ok {
					missing = append(missing, r)
				}
			}
			if len(missing) > 0 {
				return structuredError("subagent_arguments_required", "missing required arguments for "+rel.Name(),
					map[string]any{"missing": missing, "schema": p.RequiredSchema})
			}
			if err := validateSchema(p.RequiredSchema, any(arguments)); err != nil {
				return structuredError("subagent_arguments_invalid", err.Error(), map[string]any{"schema": p.RequiredSchema})
			}
		}
	}
	parentRec, err := parent.record(ctx)
	if err != nil {
		return ToolResult{Status: ToolStatusError, Error: err.Error()}
	}
	content := initialContent(rel, args, arguments)
	attSrc := args["attachments"]
	if rel.InitAttachmentsProperty != "" {
		if v, ok := arguments[rel.InitAttachmentsProperty]; ok {
			attSrc = mergePaths(attSrc, v)
		}
	}

	// maxInstances (§7.2) and create-or-continue (Subagents §3.3).
	var active []SubagentRegistryEntry
	for _, c := range parentRec.Children {
		if c.Relationship == rel.Name() && c.Status != ChildTerminated {
			active = append(active, c)
		}
	}
	if rel.MaxInstances > 0 && len(active) >= rel.MaxInstances {
		if rel.Blocking && rel.MaxInstances == 1 && !forceAsync {
			existing := active[0]
			if content == "" {
				content = stringify(arguments)
			}
			return rt.awaitChild(ctx, parentRec, existing, func() error {
				return rt.messageChild(ctx, parentRec, existing, content, attSrc)
			})
		}
		refs := make([]string, 0, len(active))
		for _, a := range active {
			refs = append(refs, a.Reference)
		}
		return structuredError("subagent_max_instances",
			fmt.Sprintf("relationship %q already has %d of %d allowed instances. Reuse one with subagent_message (references: %s), or wait for an instance to be terminated.",
				rel.Name(), len(active), rel.MaxInstances, strings.Join(refs, ", ")),
			map[string]any{"references": refs, "maxInstances": rel.MaxInstances})
	}

	// Scoped env transfer and bootstrap (§7.5).
	env := map[string]EnvValue{}
	for _, n := range rel.ScopedEnv {
		if v, err := parent.Env(ctx, n); err == nil {
			env[n] = EnvValue{Value: v, Type: rt.reg.varType(childAgent, n), Scoped: true}
		}
	}
	for k, v := range extraEnv {
		v.Scoped = true
		env[k] = v
	}
	var missing []string
	for _, n := range rt.reg.requiredScopedVars(childAgent) {
		if _, ok := env[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		pb := rt.newBootstrap(parentRec.ID, side, rel.Name(), args, missing)
		return structuredError("subagent_env_required",
			"required scoped variables are missing; ask the user to supply them via the bootstrap endpoint",
			map[string]any{
				"request_id": pb.ID,
				"missing":    missing,
				"endpoint":   fmt.Sprintf("/threads/%s/variables/%s", parentRec.ID, pb.ID),
			})
	}

	threadName := name
	title := childAgent.Title
	if rel.InitAgentNameProperty != "" {
		if v, ok := arguments[rel.InitAgentNameProperty].(string); ok && v != "" {
			threadName = v
		}
	}
	entry, err := rt.spawnChild(ctx, parent, side, rel, CreateThreadOptions{
		AgentID: childAgent.Name, UserID: parentRec.UserID, Arguments: arguments, Env: env, Name: threadName,
	}, title, "")
	if err != nil {
		return ToolResult{Status: ToolStatusError, Error: err.Error()}
	}
	if content == "" && attSrc == nil {
		return ToolResult{Status: ToolStatusSuccess, Result: map[string]any{"reference": entry.Reference, "status": entry.Status, "name": threadName}}
	}
	if rel.Blocking && !forceAsync {
		return rt.awaitChild(ctx, parentRec, entry, func() error {
			return rt.messageChild(ctx, parentRec, entry, content, attSrc)
		})
	}
	if err := rt.messageChild(ctx, parentRec, entry, content, attSrc); err != nil {
		return ToolResult{Status: ToolStatusError, Error: err.Error()}
	}
	return ToolResult{Status: ToolStatusSuccess, Result: map[string]any{"reference": entry.Reference, "status": ChildRunning, "name": threadName}}
}

func initialContent(rel *SubagentRelationship, args, arguments map[string]any) string {
	if rel.InitUserMessageProperty != "" {
		if v, ok := arguments[rel.InitUserMessageProperty]; ok {
			return stringify(v)
		}
	}
	if m, ok := args["message"].(string); ok && m != "" {
		return m
	}
	if len(arguments) > 0 {
		return stringify(arguments)
	}
	return ""
}

func mergePaths(a, b any) any {
	var out []any
	for _, v := range []any{a, b} {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case []any:
			out = append(out, x...)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// awaitChild registers a waiter, delivers work, and blocks until the child
// session closes (blocking relationships).
func (rt *Runtime) awaitChild(ctx context.Context, parentRec *ThreadRecord, entry SubagentRegistryEntry, deliver func() error) ToolResult {
	ch := rt.waitSession(entry.ThreadID)
	if err := deliver(); err != nil {
		rt.cancelWait(entry.ThreadID, ch)
		return ToolResult{Status: ToolStatusError, Error: err.Error()}
	}
	select {
	case out := <-ch:
		atts, err := rt.copyAttachments(context.Background(), entry.ThreadID, parentRec.ID, out.Attachments, "/subagents/"+entry.Reference)
		if err != nil {
			return ToolResult{Status: ToolStatusError, Error: err.Error()}
		}
		body := map[string]any{"reference": entry.Reference, "status": out.Kind, "reason": out.Reason}
		if out.Kind == OutcomeCompleted {
			body["result"] = out.Result
			return ToolResult{Status: ToolStatusSuccess, Result: body, Attachments: atts}
		}
		msg := out.Error
		if msg == "" {
			msg = out.Result
		}
		body["error"] = msg
		b, _ := json.Marshal(body)
		return ToolResult{Status: ToolStatusError, Error: string(b), Result: body, Attachments: atts}
	case <-ctx.Done():
		rt.cancelWait(entry.ThreadID, ch)
		return ToolResult{Status: ToolStatusError, Error: "aborted while waiting for subagent " + entry.Reference}
	}
}

// spawnChild creates a child thread with parent linkage and registers it in
// the parent's registry (Runtime §7.1).
func (rt *Runtime) spawnChild(ctx context.Context, parent *ThreadState, side Side, rel *SubagentRelationship, opts CreateThreadOptions, title, description string) (SubagentRegistryEntry, error) {
	childAgent := rt.reg.Agent(rel.Agent)
	ref := newID()
	cs, err := rt.createThread(ctx, opts, &childLink{parentID: parent.threadID, reference: ref, rel: rel, owningSide: side})
	if err != nil {
		return SubagentRegistryEntry{}, err
	}
	if description == "" {
		description = rel.Description
	}
	if description == "" {
		description = childAgent.ToolDescription
	}
	if description == "" {
		description = childAgent.Description
	}
	entry := SubagentRegistryEntry{
		Reference: ref, Relationship: rel.Name(), Name: childAgent.Name, Title: title,
		Description: description, Blocking: rel.Blocking, ThreadName: opts.Name,
		InitialArguments: copyMap(opts.Arguments), CreatedAt: nowMicros(), Status: ChildIdle,
		ParentCommunication: rel.parentComm(), ThreadID: cs.threadID,
	}
	if _, err := rt.updateThread(ctx, parent.threadID, func(r *ThreadRecord) error {
		r.Children = append(r.Children, entry)
		return nil
	}); err != nil {
		rt.deleteThreadTree(context.Background(), cs.threadID) // no orphan children
		return SubagentRegistryEntry{}, err
	}
	hooks, err := rt.resolveHooks(ctx, parent, HookAfterSubagentCreated)
	if err != nil {
		return entry, err
	}
	for _, h := range hooks {
		rt.logHookErr(HookAfterSubagentCreated, h.ID, h.AfterSubagentCreated(ctx, parent, entry))
	}
	return entry, nil
}

// spawnImmediate spawns an `immediate` relationship at thread creation.
func (rt *Runtime) spawnImmediate(ctx context.Context, parent *ThreadState, side Side, rel *SubagentRelationship) (SubagentRegistryEntry, error) {
	childAgent := rt.reg.Agent(rel.Agent)
	if childAgent == nil {
		return SubagentRegistryEntry{}, errf("agent %q not defined", rel.Agent)
	}
	env := map[string]EnvValue{}
	scoped := append(append([]string(nil), rel.ScopedEnv...), rel.Immediate.ScopedEnv...)
	for _, n := range scoped {
		if v, err := parent.Env(ctx, n); err == nil {
			// Runtime-only transfer data: stored scoped (encrypted at rest),
			// never interpolated into model-visible text.
			env[n] = EnvValue{Value: v, Type: rt.reg.varType(childAgent, n), Scoped: true}
		}
	}
	name, desc := rel.Name(), ""
	if rel.Immediate.NameEnv != "" {
		if v, err := parent.Env(ctx, rel.Immediate.NameEnv); err == nil && v != "" {
			name = v
		}
	}
	if rel.Immediate.DescriptionEnv != "" {
		if v, err := parent.Env(ctx, rel.Immediate.DescriptionEnv); err == nil {
			desc = v
		}
	}
	return rt.spawnChild(ctx, parent, side, rel, CreateThreadOptions{
		AgentID: childAgent.Name, Env: env, Name: name, Arguments: map[string]any{},
	}, childAgent.Title, desc)
}

// messageChild copies attachments parent -> child and queues the message in
// the child's side B perspective (role "user") (Runtime §8.1).
func (rt *Runtime) messageChild(ctx context.Context, parentRec *ThreadRecord, entry SubagentRegistryEntry, content string, attachmentPaths any) error {
	refs, err := rt.attachmentsFromPaths(ctx, parentRec.ID, attachmentPaths)
	if err != nil {
		return err
	}
	copied, err := rt.copyAttachments(ctx, parentRec.ID, entry.ThreadID, refs, "/inbox/"+shortID(parentRec.ID))
	if err != nil {
		return err
	}
	return rt.enqueue(ctx, entry.ThreadID, QueueItem{Kind: QueueMessage, Message: &MessageInput{
		Role:        RoleUser,
		Content:     content,
		Attachments: copied,
		Metadata:    map[string]any{MetaSubagentID: entry.Reference, MetaForwardedFrom: parentRec.ID},
	}})
}

// queueToParent converts child traffic into a silent parent queued message
// on the owning side's perspective, copying attachments child -> parent
// (Runtime §8.2, Subagents §2.1).
func (rt *Runtime) queueToParent(ctx context.Context, child *ThreadRecord, content string, atts []AttachmentRef, kind string) error {
	parentRec, err := rt.store.GetThread(ctx, child.ParentID)
	if err != nil {
		return err
	}
	var entry *SubagentRegistryEntry
	for i := range parentRec.Children {
		if parentRec.Children[i].Reference == child.ParentReference {
			entry = &parentRec.Children[i]
		}
	}
	copied, err := rt.copyAttachments(ctx, child.ID, parentRec.ID, atts, "/subagents/"+child.ParentReference)
	if err != nil {
		return err
	}
	role := RoleUser // side-A-owned relationship: queued as "user" so side A acts
	if child.OwningSide == SideB {
		role = RoleAssistant
	}
	in := MessageInput{
		Role: role, Content: content, Silent: true, Attachments: copied,
		Metadata: map[string]any{
			MetaSubagentID: child.ParentReference, MetaForwardedFrom: child.ID, MetaStatusKind: kind,
		},
		SubagentID: child.ParentReference, SubagentRelationship: child.Relationship,
	}
	if entry != nil {
		in.SubagentName = entry.Name
		in.SubagentStatus = entry.Status
	}
	return rt.enqueue(ctx, parentRec.ID, QueueItem{Kind: QueueMessage, Message: &in})
}

// copyAttachments copies files between thread file systems and returns
// destination-local references (Runtime §8: linked source paths are not
// valid in the destination thread).
func (rt *Runtime) copyAttachments(ctx context.Context, fromID, toID string, atts []AttachmentRef, destDir string) ([]AttachmentRef, error) {
	var out []AttachmentRef
	for _, a := range atts {
		data, f, err := rt.store.ReadFile(ctx, fromID, a.Path)
		if err != nil {
			return nil, errf("copy attachment %q: %v", a.Path, err)
		}
		name := a.Name
		if name == "" {
			name = f.Name
		}
		dest := destDir + "/" + shortID(newID()) + "-" + name
		cp, err := cleanPath(dest)
		if err != nil {
			return nil, err
		}
		rec := FileRecord{Path: cp, Name: name, MimeType: f.MimeType, Size: int64(len(data)), CreatedAt: nowMicros()}
		if err := rt.store.WriteFile(ctx, toID, rec, data); err != nil {
			return nil, err
		}
		n := a
		n.ID = newID()
		n.Path = cp
		n.Name = name
		n.Size = rec.Size
		out = append(out, n)
	}
	return out, nil
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// updateRegistryEntry mutates one entry in the parent's child registry.
func (rt *Runtime) updateRegistryEntry(ctx context.Context, parentID, ref string, fn func(*SubagentRegistryEntry)) error {
	_, err := rt.updateThread(ctx, parentID, func(r *ThreadRecord) error {
		for i := range r.Children {
			if r.Children[i].Reference == ref {
				fn(&r.Children[i])
				return nil
			}
		}
		return ErrNotFound
	})
	return err
}

// ---- scoped variable bootstrap (Runtime §7.5) ----

type pendingBootstrap struct {
	ID        string
	ParentID  string
	Side      Side
	RelName   string
	Args      map[string]any
	Missing   []string
	ExpiresAt time.Time
}

func (rt *Runtime) newBootstrap(parentID string, side Side, rel string, args map[string]any, missing []string) *pendingBootstrap {
	pb := &pendingBootstrap{ID: newID(), ParentID: parentID, Side: side, RelName: rel, Args: copyMap(args), Missing: missing, ExpiresAt: time.Now().Add(rt.cfg.BootstrapTTL)}
	rt.mu.Lock()
	now := time.Now()
	for id, b := range rt.bootstraps {
		if now.After(b.ExpiresAt) {
			delete(rt.bootstraps, id)
		}
	}
	rt.bootstraps[pb.ID] = pb
	rt.mu.Unlock()
	return pb
}

func (rt *Runtime) getBootstrap(parentID, id string) *pendingBootstrap {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	b, ok := rt.bootstraps[id]
	if !ok || b.ParentID != parentID || time.Now().After(b.ExpiresAt) {
		return nil
	}
	return b
}

// ErrBootstrapIncomplete is returned when not all missing values are supplied.
var ErrBootstrapIncomplete = errors.New("agentruntime: missing scoped variable values")

// CompleteBootstrap stores provided scoped values and immediately boots the
// deferred subagent with its original creation payload.
func (rt *Runtime) CompleteBootstrap(ctx context.Context, parentID, requestID string, values map[string]string) (ToolResult, error) {
	pb := rt.getBootstrap(parentID, requestID)
	if pb == nil {
		return ToolResult{}, ErrNotFound
	}
	for _, m := range pb.Missing {
		if _, ok := values[m]; !ok {
			return ToolResult{}, fmt.Errorf("%w: %s", ErrBootstrapIncomplete, m)
		}
	}
	parent := rt.state(parentID, nil)
	rec, err := parent.record(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	agent := rt.reg.Agent(rec.AgentID)
	if agent == nil {
		return ToolResult{}, errf("agent %q not defined", rec.AgentID)
	}
	rel := findRel(rt.enabledRelationships(ctx, parent, agent, pb.Side, true), pb.RelName)
	if rel == nil {
		return ToolResult{}, errf("relationship %q is no longer enabled", pb.RelName)
	}
	child := rt.reg.Agent(rel.Agent)
	extra := map[string]EnvValue{}
	for k, v := range values {
		extra[k] = EnvValue{Value: v, Type: rt.reg.varType(child, k), Scoped: true}
	}
	rt.mu.Lock()
	delete(rt.bootstraps, requestID)
	rt.mu.Unlock()
	return rt.createChild(ctx, parent, pb.Side, rel, pb.Args, extra, true), nil
}
