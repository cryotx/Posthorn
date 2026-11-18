// Package queryapi serves the project-scoped read API (FR136-FR140):
// a project asking Posthorn what happened to its own mail.
//
// Read-only by construction. Every route is GET, nothing here mutates
// anything, and the routes are registered as GET patterns so a POST to
// one is a 405 from the mux rather than something the handler has to
// remember to check.
//
// Two invariants hold across every route, and both are enforced in
// storage rather than here:
//
//	Isolation — the scope comes from the AUTHENTICATED credential and
//	            nothing else. A `project` query parameter is not read;
//	            a client-supplied project name is not a fact (NFR46).
//	No content — message bodies are not selected by these queries at
//	            all, so there is nothing to mask or forget to mask.
package queryapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/response"
	"github.com/craigmccaskill/posthorn/storage"
)

// Deps are the handler's collaborators.
type Deps struct {
	// Store answers the queries. Required.
	Store *storage.Store

	// Projects authenticates the Bearer credential. Required: without
	// projects there is no scope, and an unscoped read API would be a
	// cross-tenant leak by default.
	Projects *project.Registry

	// Logger records auth failures. Never receives a credential.
	Logger *slog.Logger

	// Now is injectable for tests. Nil means time.Now.
	Now func() time.Time
}

// Handler serves the /v1 read API.
type Handler struct {
	deps Deps
	mux  *http.ServeMux
}

// New builds the handler and its routes.
func New(d Deps) *Handler {
	h := &Handler{deps: d, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET /v1/messages", h.listMessages)
	h.mux.HandleFunc("GET /v1/messages/{id}", h.getMessage)
	h.mux.HandleFunc("GET /v1/messages/{id}/attempts", h.getAttempts)
	h.mux.HandleFunc("GET /v1/messages/{id}/events", h.getEvents)
	h.mux.HandleFunc("GET /v1/usage", h.getUsage)
	return h
}

func (h *Handler) now() time.Time {
	if h.deps.Now != nil {
		return h.deps.Now()
	}
	return time.Now()
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// Routes returns the patterns this handler owns, so the composition
// root mounts exactly these and nothing accidentally wider.
func Routes() []string {
	return []string{
		"GET /v1/messages",
		"GET /v1/messages/{id}",
		"GET /v1/messages/{id}/attempts",
		"GET /v1/messages/{id}/events",
		"GET /v1/usage",
	}
}

// scope authenticates the request and returns its query scope.
//
// The ONLY input is the Bearer credential. There is deliberately no
// code path here that reads a project name from the request.
func (h *Handler) scope(w http.ResponseWriter, r *http.Request) (storage.Scope, string, bool) {
	token, ok := bearerToken(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "a project API key is required")
		return storage.Scope{}, "", false
	}
	p, err := h.deps.Projects.Authenticate(token)
	if err != nil {
		// One answer for an unknown key and a disabled project: the
		// difference is information about which credentials exist.
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid project API key")
		if h.deps.Logger != nil {
			h.deps.Logger.Warn("query_api_auth_failed", slog.String("reason", authReason(err)))
		}
		return storage.Scope{}, "", false
	}
	sc, serr := storage.ProjectScope(p.Name)
	if serr != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not scope the query")
		return storage.Scope{}, "", false
	}
	return sc, p.Name, true
}

func authReason(err error) string {
	if ae, ok := err.(*project.AuthError); ok {
		return ae.Reason
	}
	return "auth_failed"
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

func (h *Handler) listMessages(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := h.scope(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, err := storage.ParseLimit(q.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	filter := storage.MessageFilter{
		Status:    q.Get("status"),
		Provider:  q.Get("provider"),
		MailKind:  q.Get("mail_kind"),
		Recipient: q.Get("recipient"),
		Limit:     limit,
		Cursor:    q.Get("cursor"),
	}
	if raw := q.Get("since"); raw != "" {
		t, terr := time.Parse(time.RFC3339, raw)
		if terr != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "since must be an RFC3339 timestamp")
			return
		}
		filter.Since = t
	}
	if raw := q.Get("until"); raw != "" {
		t, terr := time.Parse(time.RFC3339, raw)
		if terr != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "until must be an RFC3339 timestamp")
			return
		}
		filter.Until = t
	}

	page, err := h.deps.Store.QueryMessages(scope, filter)
	if err != nil {
		// A bad cursor is the caller's error, not the server's, and it
		// is the only user-supplied value the query layer validates.
		if strings.Contains(err.Error(), "cursor") {
			writeError(w, http.StatusBadRequest, "invalid_request", "cursor is not valid")
			return
		}
		h.fail(w, "list_messages", err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *Handler) getMessage(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := h.scope(w, r)
	if !ok {
		return
	}
	msg, found, err := h.deps.Store.QueryMessage(scope, r.PathValue("id"))
	if err != nil {
		h.fail(w, "get_message", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, msg)
}

// attemptView is the wire shape of one provider attempt. Rendered here
// rather than serialising the storage row so a storage-schema change
// cannot silently alter the public API.
type attemptView struct {
	Attempt           int       `json:"attempt"`
	Provider          string    `json:"provider"`
	Result            string    `json:"result"`
	ErrorClass        string    `json:"error_class,omitempty"`
	ProviderMessageID string    `json:"provider_message_id,omitempty"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
}

func (h *Handler) getAttempts(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := h.scope(w, r)
	if !ok {
		return
	}
	attempts, found, err := h.deps.Store.QueryAttempts(scope, r.PathValue("id"))
	if err != nil {
		h.fail(w, "get_attempts", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	out := make([]attemptView, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, attemptView{
			Attempt: a.Attempt, Provider: a.Provider, Result: a.Result,
			ErrorClass: a.ErrorClass, ProviderMessageID: a.ProviderMessageID,
			StartedAt: a.StartedAt, FinishedAt: a.FinishedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempts": out})
}

// eventView is the wire shape of one delivery event.
type eventView struct {
	EventID           string            `json:"event_id"`
	Type              string            `json:"type"`
	Provider          string            `json:"provider,omitempty"`
	ProviderMessageID string            `json:"provider_message_id,omitempty"`
	OccurredAt        time.Time         `json:"occurred_at"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

func (h *Handler) getEvents(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := h.scope(w, r)
	if !ok {
		return
	}
	events, found, err := h.deps.Store.QueryEvents(scope, r.PathValue("id"))
	if err != nil {
		h.fail(w, "get_events", err)
		return
	}
	if !found {
		notFound(w)
		return
	}
	out := make([]eventView, 0, len(events))
	for _, e := range events {
		out = append(out, eventView{
			EventID: e.EventID, Type: e.Type, Provider: e.Provider,
			ProviderMessageID: e.ProviderMessageID, OccurredAt: e.OccurredAt,
			Metadata: e.Metadata,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

func (h *Handler) getUsage(w http.ResponseWriter, r *http.Request) {
	scope, _, ok := h.scope(w, r)
	if !ok {
		return
	}
	now := h.now().UTC()
	report, err := h.deps.Store.QueryUsage(scope, now.Format("2006-01-02"), now.Format("2006-01"))
	if err != nil {
		h.fail(w, "get_usage", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// fail logs the detail and returns a generic body: a storage error can
// carry SQL and column names, which is server internals, not an answer.
func (h *Handler) fail(w http.ResponseWriter, op string, err error) {
	if h.deps.Logger != nil {
		h.deps.Logger.Error("query_api_failed",
			slog.String("op", op), slog.String("error", err.Error()))
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "the query could not be completed")
}

// notFound is the answer for both a missing message and one owned by
// another project — see storage.QueryMessage for why they must match.
func notFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "no such message")
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// A read API's answers are per-credential and must not be cached by
	// anything between Posthorn and the caller.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, response.Error{Error: msg, Code: code})
}
