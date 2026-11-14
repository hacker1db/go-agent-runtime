package agentruntime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"
	"time"
)

// Role is a persisted message role. Roles are the single source of
// perspective truth: "user" is the side B perspective (side A acts on it) and
// "assistant" is the side A perspective (side B acts on it).
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Side identifies one side of a dual-sided agent.
type Side string

const (
	SideA Side = "a"
	SideB Side = "b"
)

// Other returns the opposite side.
func (s Side) Other() Side {
	if s == SideA {
		return SideB
	}
	return SideA
}

// PerspectiveRole returns the role in which this side's own turns are stored.
func (s Side) PerspectiveRole() Role {
	if s == SideA {
		return RoleAssistant
	}
	return RoleUser
}

// sideForRole returns the side that *acts on* a message of the given role.
func sideActingOn(r Role) Side {
	if r == RoleAssistant {
		return SideB
	}
	return SideA
}

// sideOwningRole returns the side that *authored* a message of the given role.
func sideOwningRole(r Role) Side {
	if r == RoleUser {
		return SideB
	}
	return SideA
}

// Message status values.
const (
	MessageStatusPending   = "pending"
	MessageStatusCompleted = "completed"
	MessageStatusFailed    = "failed"
)

// Tool status values.
const (
	ToolStatusSuccess = "success"
	ToolStatusError   = "error"
)

// Metadata keys used by this runtime.
const (
	MetaSide          = "side"           // runtime-internal: authoring side ("a"/"b")
	MetaStatusKind    = "status_kind"    // synthesized status messages (Runtime §12)
	MetaSubagentID    = "subagent_id"    // provenance (Messages §1.1)
	MetaForwardedFrom = "forwarded_from" // provenance (Messages §1.1)
	MetaSubpromptRun  = "subprompt_run"  // runtime-internal: prompt-tool run id
	MetaQueuedTool    = "queued_tool"    // runtime-internal: synthetic call for queueTool
)

// AttachmentRef is a file in a thread's file system that a message references.
type AttachmentRef struct {
	ID          string `json:"id"`
	Type        string `json:"type"` // always "file"
	Path        string `json:"path"`
	Name        string `json:"name"`
	MimeType    string `json:"mimeType"`
	Size        int64  `json:"size"`
	Width       *int   `json:"width,omitempty"`
	Height      *int   `json:"height,omitempty"`
	Description string `json:"description,omitempty"`
}

// Message is the persisted message record (Messages §1).
type Message struct {
	ID               string          `json:"id"`
	Role             Role            `json:"role"`
	Content          *string         `json:"content"`
	Name             *string         `json:"name"`
	ToolCalls        *string         `json:"tool_calls"`
	ToolCallID       *string         `json:"tool_call_id"`
	ToolStatus       *string         `json:"tool_status"`
	Status           *string         `json:"status"`
	Attachments      []AttachmentRef `json:"attachments"`
	ReasoningContent *string         `json:"reasoning_content"`
	ReasoningDetails *string         `json:"reasoning_details"`
	Prompt           *string         `json:"prompt"`
	CreatedAt        int64           `json:"created_at"`
	ParentID         *string         `json:"parent_id"`
	Depth            int             `json:"depth"`
	Silent           bool            `json:"silent"`
	Metadata         map[string]any  `json:"metadata"`

	SubagentID           *string `json:"subagent_id"`
	SubagentRelationship *string `json:"subagent_relationship"`
	SubagentName         *string `json:"subagent_name"`
	SubagentTitle        *string `json:"subagent_title"`
	SubagentDescription  *string `json:"subagent_description"`
	SubagentStatus       *string `json:"subagent_status"`
	SubagentBlocking     *bool   `json:"subagent_blocking"`
	SubagentThreadName   *string `json:"subagent_thread_name"`
	SubagentSpawnGroupID *string `json:"subagent_spawn_group_id"`
}

// ContentString returns the message content or "".
func (m Message) ContentString() string { return deref(m.Content) }

// authorSide returns the side that produced the message.
func (m Message) authorSide() Side {
	if s, ok := m.Metadata[MetaSide].(string); ok && (s == string(SideA) || s == string(SideB)) {
		return Side(s)
	}
	return sideOwningRole(m.Role)
}

// ToolCall is one entry of the JSON-encoded Message.tool_calls array.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the tool name and JSON-encoded arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// DecodeToolCalls parses a message's tool_calls field.
func (m Message) DecodeToolCalls() ([]ToolCall, error) {
	if m.ToolCalls == nil || *m.ToolCalls == "" {
		return nil, nil
	}
	var out []ToolCall
	err := json.Unmarshal([]byte(*m.ToolCalls), &out)
	return out, err
}

// MessageInput is the input for injectMessage / queueMessage.
type MessageInput struct {
	Role        Role            `json:"role"`
	Content     string          `json:"content"`
	Silent      bool            `json:"silent,omitempty"`
	Metadata    map[string]any  `json:"metadata,omitempty"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`

	// Subagent projection fields (optional).
	SubagentID           string `json:"subagent_id,omitempty"`
	SubagentRelationship string `json:"subagent_relationship,omitempty"`
	SubagentName         string `json:"subagent_name,omitempty"`
	SubagentStatus       string `json:"subagent_status,omitempty"`
}

// GetMessagesOptions are the query options for getMessages (Messages §2.1).
type GetMessagesOptions struct {
	Limit         int
	Offset        int
	Order         string // "asc" | "desc" (default "desc")
	IncludeSilent bool
	MaxDepth      *int
}

// MessagesResult is the result of getMessages.
type MessagesResult struct {
	Messages []Message `json:"messages"`
	Total    int       `json:"total"`
	HasMore  bool      `json:"hasMore"`
}

// LLMMessage is a provider-format chat message (Hooks §6.4).
type LLMMessage struct {
	Role             Role            `json:"role"`
	Content          string          `json:"content,omitempty"`
	Parts            []ContentPart   `json:"parts,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ReasoningDetails json.RawMessage `json:"reasoning_details,omitempty"`
}

// ContentPart is a multimodal content part.
type ContentPart struct {
	Type       string         `json:"type"` // "text" | "image" | "file"
	Text       string         `json:"text,omitempty"`
	Attachment *AttachmentRef `json:"attachment,omitempty"`
	Data       []byte         `json:"data,omitempty"`
}

// ---- small helpers ----

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

var lastMicros atomic.Int64

// nowMicros returns a strictly increasing microsecond timestamp so persisted
// ordering is stable even for messages created within the same microsecond.
func nowMicros() int64 {
	for {
		now := time.Now().UnixMicro()
		last := lastMicros.Load()
		if now <= last {
			now = last + 1
		}
		if lastMicros.CompareAndSwap(last, now) {
			return now
		}
	}
}

// cloneJSON deep-copies a JSON-compatible value.
func cloneJSON[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		panic("agentruntime: clone of non-JSON value: " + err.Error())
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		panic("agentruntime: clone decode: " + err.Error())
	}
	return out
}

func copyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return cloneJSON(m)
}
