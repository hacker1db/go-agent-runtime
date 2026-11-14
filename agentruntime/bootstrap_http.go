package agentruntime

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Authenticator authenticates an HTTP request and returns the caller's user
// ID. ok=false yields 401.
type Authenticator func(r *http.Request) (userID string, ok bool)

// BootstrapHandler serves the temporary scoped-variable completion flow
// (Runtime §7.5):
//
//	GET  {prefix}/threads/{parent_thread_id}/variables/{request_id}
//	POST {prefix}/threads/{parent_thread_id}/variables/{request_id}
//
// Every request is authenticated, and the caller must own the parent thread.
// Internal failures return 5xx.
func (rt *Runtime) BootstrapHandler(prefix string, auth Authenticator) http.Handler {
	mux := http.NewServeMux()
	pattern := prefix + "/threads/{thread}/variables/{request}"
	authorize := func(w http.ResponseWriter, r *http.Request) (*pendingBootstrap, bool) {
		if auth == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return nil, false
		}
		uid, ok := auth(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return nil, false
		}
		threadID := r.PathValue("thread")
		rec, err := rt.store.GetThread(r.Context(), threadID)
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return nil, false
		}
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return nil, false
		}
		if rec.UserID == nil || *rec.UserID != uid {
			http.Error(w, "forbidden", http.StatusForbidden)
			return nil, false
		}
		pb := rt.getBootstrap(threadID, r.PathValue("request"))
		if pb == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return nil, false
		}
		return pb, true
	}
	mux.HandleFunc("GET "+pattern, func(w http.ResponseWriter, r *http.Request) {
		pb, ok := authorize(w, r)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"request_id":   pb.ID,
			"relationship": pb.RelName,
			"required":     pb.Missing,
			"missing":      pb.Missing,
			"expires_at":   pb.ExpiresAt.UnixMicro(),
		})
	})
	mux.HandleFunc("POST "+pattern, func(w http.ResponseWriter, r *http.Request) {
		pb, ok := authorize(w, r)
		if !ok {
			return
		}
		var body struct {
			Values map[string]string `json:"values"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		res, err := rt.CompleteBootstrap(r.Context(), pb.ParentID, pb.ID, body.Values)
		switch {
		case errors.Is(err, ErrBootstrapIncomplete):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "missing": pb.Missing})
			return
		case errors.Is(err, ErrNotFound):
			http.Error(w, "not found", http.StatusNotFound)
			return
		case err != nil:
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		status := http.StatusOK
		if res.Status == ToolStatusError {
			status = http.StatusUnprocessableEntity
		}
		writeJSON(w, status, res)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
