// Package admin serves the operator's read-and-limited-ops surface
// (FR143-FR147): a dashboard, a JSON API behind the same credential,
// and an audited set of incident controls.
//
// Three rules shape everything here, and each one is enforced in code
// rather than by convention:
//
//	Separate credential. POSTHORN_ADMIN_TOKEN, never a project API key,
//	and startup refuses to run if the two collide (see VerifyDistinct).
//
//	Generated project keys are shown only in the immediate authenticated
//	creation or rotation response. Existing credentials cannot be read
//	back; the project list displays only a digest fingerprint.
//
//	Ops are narrow and audited. Four actions exist: disable/enable a
//	provider, reset a breaker, suspend/restore a project, retry a dead
//	letter, and remove a suppression. Each writes an audit row BEFORE it
//	takes effect, and an action whose audit row cannot be written does
//	not happen.
package admin

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/craigmccaskill/posthorn/managedproviders"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// Deps are the admin surface's collaborators. Everything is read
// through these; the package holds no state of its own beyond sessions.
type Deps struct {
	// Auth is the admin credential and session store. Required.
	Auth *Authenticator

	// Store answers every query. Required.
	Store *storage.Store

	// Gate reports storage health. Optional.
	Gate *storage.Gate

	// Pool is the provider pool, for state and the ops actions.
	Pool *transport.Pool

	// Projects is the tenant registry.
	Projects *project.Registry

	// ProjectKeys manages credentials created from the admin surface.
	ProjectKeys ProjectKeyManager

	// ProviderManager manages providers configured through the admin UI.
	ProviderManager ProviderManager

	// Quota reports provider usage. Optional.
	Quota transport.QuotaSource

	// Logger records admin actions alongside the audit rows. Secrets
	// never reach it.
	Logger *slog.Logger

	// Version is the running binary's version, shown on the dashboard.
	Version string

	// Now is injectable for tests.
	Now func() time.Time
}

// ProjectKeyManager creates and rotates managed project credentials.
// A returned credential must only be shown in the immediate response.
type ProjectKeyManager interface {
	Create(name string) (string, error)
	Rotate(name string) (string, error)
	IsManaged(name string) bool
}

// ProviderManager controls the managed provider configuration.
type ProviderManager interface {
	List() ([]managedproviders.View, error)
	Save(managedproviders.Input) error
	SetEnabled(name string, enabled bool) error
}

// Handler serves /admin and /admin/api.
type Handler struct {
	deps Deps
	mux  *http.ServeMux
}

// New builds the admin handler.
func New(d Deps) *Handler {
	h := &Handler{deps: d, mux: http.NewServeMux()}

	// UI. Every page is a GET; the two POSTs are the login form and the
	// action form, both CSRF-checked.
	h.mux.HandleFunc("GET /admin", h.pageDashboard)
	h.mux.HandleFunc("GET /admin/", h.pageDashboard)
	h.mux.HandleFunc("GET /admin/login", h.pageLogin)
	h.mux.HandleFunc("POST /admin/login", h.doLogin)
	h.mux.HandleFunc("POST /admin/logout", h.doLogout)
	h.mux.HandleFunc("GET /admin/providers", h.pageProviders)
	h.mux.HandleFunc("GET /admin/providers/configure", h.pageProviderConfigure)
	h.mux.HandleFunc("POST /admin/providers/configure", h.doProviderConfigure)
	h.mux.HandleFunc("POST /admin/providers/state", h.doManagedProviderState)
	h.mux.HandleFunc("GET /admin/projects", h.pageProjects)
	h.mux.HandleFunc("GET /admin/messages", h.pageMessages)
	h.mux.HandleFunc("GET /admin/messages/{id}", h.pageMessage)
	h.mux.HandleFunc("GET /admin/audit", h.pageAudit)
	h.mux.HandleFunc("POST /admin/action", h.doAction)
	h.mux.HandleFunc("POST /admin/projects/create", h.doProjectKey)
	h.mux.HandleFunc("POST /admin/projects/{name}/rotate", h.doProjectKey)

	// JSON API, same credential.
	h.mux.HandleFunc("GET /admin/api/overview", h.apiOverview)
	h.mux.HandleFunc("GET /admin/api/providers", h.apiProviders)
	h.mux.HandleFunc("GET /admin/api/projects", h.apiProjects)
	h.mux.HandleFunc("GET /admin/api/messages", h.apiMessages)
	h.mux.HandleFunc("GET /admin/api/messages/{id}", h.apiMessage)
	h.mux.HandleFunc("GET /admin/api/messages/{id}/attempts", h.apiAttempts)
	h.mux.HandleFunc("GET /admin/api/messages/{id}/events", h.apiEvents)
	h.mux.HandleFunc("GET /admin/api/queue", h.apiQueue)
	h.mux.HandleFunc("GET /admin/api/dead-letters", h.apiDeadLetters)
	h.mux.HandleFunc("GET /admin/api/suppressions", h.apiSuppressions)
	h.mux.HandleFunc("GET /admin/api/audit", h.apiAudit)
	h.mux.HandleFunc("GET /admin/api/usage", h.apiUsage)

	// Ops. All POST, all audited, all CSRF-checked for session auth.
	h.mux.HandleFunc("POST /admin/api/providers/{name}/disable", h.opProviderDisable)
	h.mux.HandleFunc("POST /admin/api/providers/{name}/enable", h.opProviderEnable)
	h.mux.HandleFunc("POST /admin/api/providers/{name}/reset-breaker", h.opBreakerReset)
	h.mux.HandleFunc("POST /admin/api/projects/{name}/disable", h.opProjectDisable)
	h.mux.HandleFunc("POST /admin/api/projects/{name}/enable", h.opProjectEnable)
	h.mux.HandleFunc("POST /admin/api/dead-letters/{id}/retry", h.opDeadLetterRetry)
	h.mux.HandleFunc("POST /admin/api/suppressions/{email}/remove", h.opSuppressionRemove)

	return h
}

// Routes returns the patterns the composition root mounts.
//
// Two, not thirty: the handler owns the whole /admin subtree and routes
// within it. Mounting each leaf on the outer mux would mean two places
// that have to agree about what exists, and an admin route is not a
// thing to be wrong about.
func Routes() []string { return []string{"/admin", "/admin/"} }

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The admin surface is never cacheable and never framed: it shows
	// per-operator data and its forms perform state changes.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) now() time.Time {
	if h.deps.Now != nil {
		return h.deps.Now()
	}
	return time.Now()
}

// require authenticates a request, answering 401 when it fails.
//
// The returned authKind matters to the caller only for CSRF; every
// route is otherwise identical in what it permits, because there is one
// admin credential and no roles (complex RBAC is an explicit non-goal).
func (h *Handler) require(w http.ResponseWriter, r *http.Request) (authKind, string, bool) {
	kind, csrfToken := h.deps.Auth.authenticate(r)
	if kind == authNone {
		writeError(w, http.StatusUnauthorized, "unauthorized", "admin authentication required")
		return authNone, "", false
	}
	return kind, csrfToken, true
}

// requirePage is require for HTML routes: a browser gets a redirect to
// the login form rather than a JSON 401 it cannot act on.
func (h *Handler) requirePage(w http.ResponseWriter, r *http.Request) (string, bool) {
	kind, csrfToken := h.deps.Auth.authenticate(r)
	if kind == authNone {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return "", false
	}
	return csrfToken, true
}

// sourceIP is the request's peer address, for audit rows and login
// throttling.
//
// The PEER address only — no X-Forwarded-For. Posthorn's admin surface
// binds to 127.0.0.1 by default with the operator's own proxy in front,
// and trusting a header here would let a caller forge the address in
// the audit log, which is the one place it needs to be true.
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}

// fail logs the detail and returns a generic body: a storage error can
// carry SQL and column names, which is server internals, not an answer.
func (h *Handler) fail(w http.ResponseWriter, op string, err error) {
	if h.deps.Logger != nil {
		h.deps.Logger.Error("admin_request_failed",
			slog.String("op", op), slog.String("error", err.Error()))
	}
	writeError(w, http.StatusInternalServerError, "internal_error", "the request could not be completed")
}
