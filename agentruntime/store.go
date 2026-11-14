package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound is returned when a record does not exist.
var ErrNotFound = errors.New("agentruntime: not found")

// ThreadRunState is the externally visible run status.
type ThreadRunState string

const (
	RunIdle    ThreadRunState = "idle"
	RunRunning ThreadRunState = "running"
	RunFailed  ThreadRunState = "failed"
)

// ThreadStatus is the authoritative run status (Execution State §4).
type ThreadStatus struct {
	State  ThreadRunState `json:"state"`
	Since  int64          `json:"since"`
	Reason *string        `json:"reason"`
}

// SubagentRegistryEntry tracks one child instance (Subagents §4.2).
type SubagentRegistryEntry struct {
	Reference           string         `json:"reference"`
	Relationship        string         `json:"relationship"`
	Name                string         `json:"name"`
	Title               string         `json:"title,omitempty"`
	Description         string         `json:"description"`
	Blocking            bool           `json:"blocking,omitempty"`
	ThreadName          string         `json:"threadName,omitempty"`
	InitialArguments    map[string]any `json:"initialArguments,omitempty"`
	SpawnGroupID        string         `json:"spawnGroupId,omitempty"`
	CreatedAt           int64          `json:"createdAt,omitempty"`
	Status              string         `json:"status"`
	ParentCommunication string         `json:"parentCommunication,omitempty"`
	// StatusText is the working-status label set via setStatus/sessionStatus.
	StatusText string `json:"statusText,omitempty"`
	// ThreadID is the child thread id (runtime-internal linkage).
	ThreadID string `json:"threadId"`
}

// Registry entry status values.
const (
	ChildRunning       = "running"
	ChildIdle          = "idle"
	ChildFailed        = "failed"
	ChildAwaitingHuman = "awaiting_human"
	ChildTerminated    = "terminated"
)

// EnvValue is a thread-level environment value.
type EnvValue struct {
	Value  string `json:"value"`
	Type   string `json:"type"` // "text" | "secret"
	Scoped bool   `json:"scoped,omitempty"`
}

// sensitive reports whether the value must be encrypted at rest.
func (e EnvValue) sensitive() bool { return e.Type == "secret" || e.Scoped }

// ThreadRecord is the durable per-thread record.
type ThreadRecord struct {
	ID         string  `json:"id"`
	AgentID    string  `json:"agentId"`
	UserID     *string `json:"userId"`
	CreatedAt  int64   `json:"createdAt"`
	Terminated *int64  `json:"terminated"`

	ParentID            string `json:"parentId,omitempty"`
	ParentReference     string `json:"parentReference,omitempty"`
	Relationship        string `json:"relationship,omitempty"`
	OwningSide          Side   `json:"owningSide,omitempty"`
	ParentCommunication string `json:"parentCommunication,omitempty"`
	Name                string `json:"name,omitempty"`

	Arguments map[string]any          `json:"arguments"`
	Env       map[string]EnvValue     `json:"env"`
	Children  []SubagentRegistryEntry `json:"children"`
	Status    ThreadStatus            `json:"status"`

	SessionTurns int `json:"sessionTurns"`
}

// QueueItem is a durable unit of queued work.
type QueueItem struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"` // "message" | "tool" | "effect"
	Message   *MessageInput  `json:"message,omitempty"`
	ToolName  string         `json:"toolName,omitempty"`
	ToolArgs  map[string]any `json:"toolArgs,omitempty"`
	EffectID  string         `json:"effectId,omitempty"`
	CreatedAt int64          `json:"createdAt"`
}

// Queue item kinds.
const (
	QueueMessage = "message"
	QueueTool    = "tool"
	QueueEffect  = "effect"
)

// FileRecord describes a file in a thread's file system.
type FileRecord struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	MimeType  string `json:"mimeType"`
	Size      int64  `json:"size"`
	CreatedAt int64  `json:"createdAt"`
}

// ScheduledEffect is a pending effect (Threads §4.2).
type ScheduledEffect struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Args        map[string]any `json:"args"`
	ScheduledAt int64          `json:"scheduledAt"`
	RunAt       int64          `json:"runAt"`
	Attempts    int            `json:"attempts,omitempty"`
}

// Store is the durable persistence boundary. Implementations must make
// writes durable before returning.
type Store interface {
	PutThread(ctx context.Context, rec ThreadRecord) error
	GetThread(ctx context.Context, id string) (*ThreadRecord, error)
	ListThreads(ctx context.Context) ([]ThreadRecord, error)
	DeleteThread(ctx context.Context, id string) error

	InsertMessage(ctx context.Context, threadID string, m Message) error
	// InsertMessageAck atomically inserts a message and acknowledges queue
	// items, so a crash can neither duplicate nor lose a queued delivery.
	InsertMessageAck(ctx context.Context, threadID string, m Message, ackIDs []string) error
	UpdateMessage(ctx context.Context, threadID string, m Message) error
	DeleteMessages(ctx context.Context, threadID string, ids []string) (int, error)
	ListMessages(ctx context.Context, threadID string) ([]Message, error)

	Enqueue(ctx context.Context, threadID string, item QueueItem) error
	PeekQueue(ctx context.Context, threadID string) ([]QueueItem, error)
	AckQueue(ctx context.Context, threadID string, ids []string) error

	GetValue(ctx context.Context, threadID, key string) (json.RawMessage, bool, error)
	SetValue(ctx context.Context, threadID, key string, v json.RawMessage) error
	DeleteValue(ctx context.Context, threadID, key string) error
	CountValues(ctx context.Context, threadID string) (int, error)

	WriteFile(ctx context.Context, threadID string, f FileRecord, data []byte) error
	ReadFile(ctx context.Context, threadID, path string) ([]byte, *FileRecord, error)
	ListFiles(ctx context.Context, threadID string) ([]FileRecord, error)
	DeleteFile(ctx context.Context, threadID, path string) error

	PutEffect(ctx context.Context, threadID string, e ScheduledEffect) error
	DeleteEffect(ctx context.Context, threadID, id string) (bool, error)
	ListEffects(ctx context.Context, threadID string) ([]ScheduledEffect, error)
}

// ---- MemoryStore ----

type threadData struct {
	Record   ThreadRecord               `json:"record"`
	Messages []Message                  `json:"messages"`
	Queue    []QueueItem                `json:"queue"`
	Values   map[string]json.RawMessage `json:"values"`
	Files    map[string]storedFile      `json:"files"`
	Effects  map[string]ScheduledEffect `json:"effects"`
}

type storedFile struct {
	Record FileRecord `json:"record"`
	Data   []byte     `json:"data"`
}

func newThreadData() *threadData {
	return &threadData{Values: map[string]json.RawMessage{}, Files: map[string]storedFile{}, Effects: map[string]ScheduledEffect{}}
}

// MemoryStore is an in-process Store. It is not durable across restarts;
// use FileStore (or another durable Store) in production.
type MemoryStore struct {
	mu      sync.Mutex
	threads map[string]*threadData
	// persist, when set, must durably store td (nil td = delete) before the
	// mutation becomes visible. It is invoked with mu held.
	persist func(id string, td *threadData) error
}

// NewMemoryStore returns an empty in-memory store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{threads: map[string]*threadData{}} }

// view runs fn against the live thread data under the lock (read-only).
func (s *MemoryStore) view(id string, fn func(t *threadData) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.threads[id]
	if !ok {
		return ErrNotFound
	}
	return fn(t)
}

// errNoChange lets a mutation report success without persisting.
var errNoChange = errors.New("no change")

// mutate applies fn to a copy of the thread data, persists the copy, and
// only then publishes it, so a failed durable write never leaves the
// unpersisted mutation visible.
func (s *MemoryStore) mutate(id string, create bool, fn func(t *threadData) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.threads[id]
	if !ok && !create {
		return ErrNotFound
	}
	var next *threadData
	switch {
	case !ok:
		next = newThreadData()
	case s.persist != nil:
		c := cloneJSON(*cur)
		next = &c
		if next.Values == nil {
			next.Values = map[string]json.RawMessage{}
		}
		if next.Files == nil {
			next.Files = map[string]storedFile{}
		}
		if next.Effects == nil {
			next.Effects = map[string]ScheduledEffect{}
		}
	default:
		next = cur // no durability layer: mutate in place
	}
	if err := fn(next); err != nil {
		if errors.Is(err, errNoChange) {
			return nil
		}
		return err
	}
	if s.persist != nil {
		if err := s.persist(id, next); err != nil {
			return err
		}
	}
	s.threads[id] = next
	return nil
}

func (s *MemoryStore) PutThread(_ context.Context, rec ThreadRecord) error {
	return s.mutate(rec.ID, true, func(t *threadData) error {
		t.Record = cloneJSON(rec)
		return nil
	})
}

func (s *MemoryStore) DeleteThread(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.threads[id]; !ok {
		return ErrNotFound
	}
	if s.persist != nil {
		if err := s.persist(id, nil); err != nil {
			return err
		}
	}
	delete(s.threads, id)
	return nil
}

func (s *MemoryStore) GetThread(_ context.Context, id string) (*ThreadRecord, error) {
	var r ThreadRecord
	err := s.view(id, func(t *threadData) error { r = cloneJSON(t.Record); return nil })
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *MemoryStore) ListThreads(_ context.Context) ([]ThreadRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ThreadRecord, 0, len(s.threads))
	for _, t := range s.threads {
		out = append(out, cloneJSON(t.Record))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out, nil
}

func (s *MemoryStore) InsertMessage(ctx context.Context, id string, m Message) error {
	return s.InsertMessageAck(ctx, id, m, nil)
}

func (s *MemoryStore) InsertMessageAck(_ context.Context, id string, m Message, ackIDs []string) error {
	return s.mutate(id, false, func(t *threadData) error {
		for _, ex := range t.Messages {
			if ex.ID == m.ID {
				return errf("message %s already exists", m.ID)
			}
		}
		t.Messages = append(t.Messages, cloneJSON(m))
		t.Queue = without(t.Queue, ackIDs)
		return nil
	})
}

func without(q []QueueItem, ids []string) []QueueItem {
	if len(ids) == 0 {
		return q
	}
	del := map[string]bool{}
	for _, x := range ids {
		del[x] = true
	}
	kept := make([]QueueItem, 0, len(q))
	for _, it := range q {
		if !del[it.ID] {
			kept = append(kept, it)
		}
	}
	return kept
}

func (s *MemoryStore) UpdateMessage(_ context.Context, id string, m Message) error {
	return s.mutate(id, false, func(t *threadData) error {
		for i := range t.Messages {
			if t.Messages[i].ID == m.ID {
				t.Messages[i] = cloneJSON(m)
				return nil
			}
		}
		return ErrNotFound
	})
}

func (s *MemoryStore) DeleteMessages(_ context.Context, id string, ids []string) (int, error) {
	n := 0
	err := s.mutate(id, false, func(t *threadData) error {
		del := map[string]bool{}
		for _, x := range ids {
			del[x] = true
		}
		kept := make([]Message, 0, len(t.Messages))
		for _, m := range t.Messages {
			if del[m.ID] {
				n++
				continue
			}
			kept = append(kept, m)
		}
		if n == 0 {
			return errNoChange
		}
		t.Messages = kept
		return nil
	})
	return n, err
}

func (s *MemoryStore) ListMessages(_ context.Context, id string) ([]Message, error) {
	var out []Message
	err := s.view(id, func(t *threadData) error { out = cloneJSON(t.Messages); return nil })
	return out, err
}

func (s *MemoryStore) Enqueue(_ context.Context, id string, item QueueItem) error {
	return s.mutate(id, false, func(t *threadData) error {
		t.Queue = append(t.Queue, cloneJSON(item))
		return nil
	})
}

func (s *MemoryStore) PeekQueue(_ context.Context, id string) ([]QueueItem, error) {
	var out []QueueItem
	err := s.view(id, func(t *threadData) error { out = cloneJSON(t.Queue); return nil })
	return out, err
}

func (s *MemoryStore) AckQueue(_ context.Context, id string, ids []string) error {
	return s.mutate(id, false, func(t *threadData) error {
		t.Queue = without(t.Queue, ids)
		return nil
	})
}

func (s *MemoryStore) GetValue(_ context.Context, id, key string) (json.RawMessage, bool, error) {
	var out json.RawMessage
	var ok bool
	err := s.view(id, func(t *threadData) error {
		v, found := t.Values[key]
		if found {
			out, ok = append(json.RawMessage(nil), v...), true
		}
		return nil
	})
	return out, ok, err
}

func (s *MemoryStore) SetValue(_ context.Context, id, key string, v json.RawMessage) error {
	return s.mutate(id, false, func(t *threadData) error {
		t.Values[key] = append(json.RawMessage(nil), v...)
		return nil
	})
}

func (s *MemoryStore) DeleteValue(_ context.Context, id, key string) error {
	return s.mutate(id, false, func(t *threadData) error {
		if _, ok := t.Values[key]; !ok {
			return errNoChange
		}
		delete(t.Values, key)
		return nil
	})
}

func (s *MemoryStore) CountValues(_ context.Context, id string) (int, error) {
	n := 0
	err := s.view(id, func(t *threadData) error { n = len(t.Values); return nil })
	return n, err
}

func (s *MemoryStore) WriteFile(_ context.Context, id string, f FileRecord, data []byte) error {
	return s.mutate(id, false, func(t *threadData) error {
		t.Files[f.Path] = storedFile{Record: f, Data: append([]byte(nil), data...)}
		return nil
	})
}

func (s *MemoryStore) ReadFile(_ context.Context, id, path string) ([]byte, *FileRecord, error) {
	var data []byte
	var rec *FileRecord
	err := s.view(id, func(t *threadData) error {
		f, ok := t.Files[path]
		if !ok {
			return ErrNotFound
		}
		r := f.Record
		data, rec = append([]byte(nil), f.Data...), &r
		return nil
	})
	return data, rec, err
}

func (s *MemoryStore) ListFiles(_ context.Context, id string) ([]FileRecord, error) {
	var out []FileRecord
	err := s.view(id, func(t *threadData) error {
		for _, f := range t.Files {
			out = append(out, f.Record)
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return strings.Compare(out[i].Path, out[j].Path) < 0 })
	return out, err
}

func (s *MemoryStore) DeleteFile(_ context.Context, id, path string) error {
	return s.mutate(id, false, func(t *threadData) error {
		if _, ok := t.Files[path]; !ok {
			return ErrNotFound
		}
		delete(t.Files, path)
		return nil
	})
}

func (s *MemoryStore) PutEffect(_ context.Context, id string, e ScheduledEffect) error {
	return s.mutate(id, false, func(t *threadData) error {
		t.Effects[e.ID] = cloneJSON(e)
		return nil
	})
}

func (s *MemoryStore) DeleteEffect(_ context.Context, id, eid string) (bool, error) {
	found := false
	err := s.mutate(id, false, func(t *threadData) error {
		if _, ok := t.Effects[eid]; !ok {
			return errNoChange
		}
		found = true
		delete(t.Effects, eid)
		return nil
	})
	return found, err
}

func (s *MemoryStore) ListEffects(_ context.Context, id string) ([]ScheduledEffect, error) {
	var out []ScheduledEffect
	err := s.view(id, func(t *threadData) error {
		for _, e := range t.Effects {
			out = append(out, cloneJSON(e))
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].RunAt < out[j].RunAt })
	return out, err
}
