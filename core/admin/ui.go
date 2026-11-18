package admin

import (
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/craigmccaskill/posthorn/managedproviders"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
)

// The admin UI (FR145, FR146).
//
// Server-rendered HTML with no JavaScript and no external assets. Three
// reasons, in order of weight:
//
//  1. There is no script to inject INTO. Every value goes through
//     html/template's contextual escaping (ADR-19's guarantee applied to
//     the admin surface), and the page loads nothing from a CDN, so a
//     stored value from a webhook payload cannot become code.
//  2. The session cookie is HttpOnly, which only means something if the
//     page does not need to read it.
//  3. It works over an SSH tunnel to 127.0.0.1, which is how this is
//     meant to be reached.
//
// A Content-Security-Policy is set anyway, as the belt to that
// braces: no script of any origin, inline included.

const contentSecurityPolicy = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

type managedProviderView struct {
	managedproviders.View
	UsedToday, UsedMonth int64
	UsageAvailable       bool
}

// pageData is the shape every template renders against.
type pageData struct {
	// TitleKey is a catalog key (see i18n.go), not literal text — the
	// <title> tag resolves it via {{t .Lang .TitleKey}}. Every page
	// handler sets this instead of a hardcoded English string.
	TitleKey string
	Nav      string // which nav item is current
	// Lang is resolved once, in render, from the request — handlers
	// never set this field themselves.
	Lang      uiLang
	CSRFToken string
	Version   string
	Now       time.Time

	Overview           *overview
	Providers          []providerView
	ManagedProviders   []managedProviderView
	ProviderForm       *providerForm
	CanManageProviders bool
	Projects           []projectView
	Messages           []maskedMessage
	Message            *maskedMessage
	Attempts           []storage.Attempt
	Events             []storage.DeliveryEventRow
	Audit              []storage.AuditEntry
	Queue              []storage.PendingRetry
	DeadLetters        []maskedMessage
	Suppressions       []suppressionView
	NextCursor         string
	Error              string
	Notice             string
	CanCreateProject   bool
	NewProjectName     string
	NewProjectKey      string
	ProjectKeyError    string
}

var uiTemplates = template.Must(template.New("admin").Funcs(template.FuncMap{
	"join": strings.Join,
	"ts": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04:05Z")
	},
	"tsp": func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "—"
		}
		return t.UTC().Format("2006-01-02 15:04:05Z")
	},
	"orDash": func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	},
	// t is the translation lookup (i18n.go). Every static string in
	// the templates goes through it; raw operator/provider content
	// (names, ids, timestamps, error text) does not — see i18n.go's
	// file comment for that scope boundary.
	"t": translate,
}).Parse(uiHTML))

// render writes one named template with the standard headers. It is
// the ONE place that resolves the request's language (i18n.go) — every
// page handler passes the request through and never touches Lang
// itself, so the resolution logic and its cookie side effect live in
// exactly one spot.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, data pageData) {
	data.Version = h.deps.Version
	data.Now = h.now().UTC()
	data.Lang = h.lang(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	if err := uiTemplates.ExecuteTemplate(w, name, data); err != nil && h.deps.Logger != nil {
		h.deps.Logger.Error("admin_render_failed", "template", name, "error", err.Error())
	}
}

func (h *Handler) pageLogin(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.deps.Auth.ValidSession(h.deps.Auth.sessionID(r)); ok {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	h.render(w, r, "login", pageData{TitleKey: "label.signin", Error: r.URL.Query().Get("error")})
}

func (h *Handler) doLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/login?error=bad+request", http.StatusSeeOther)
		return
	}
	token := r.PostFormValue("token")
	id, err := h.deps.Auth.Login(sourceIP(r), token)
	if err != nil {
		// The audit log records the ATTEMPT, not the token. A failed
		// login is exactly the event an operator wants to see.
		_ = h.deps.Store.RecordAudit(storage.AuditEntry{
			At: h.now(), Actor: storage.ActorAdmin, Action: storage.AuditAdminLoginFailed,
			SourceIP: sourceIP(r),
		})
		msg := "invalid+token"
		if err == ErrLockedOut {
			msg = "too+many+attempts"
		}
		http.Redirect(w, r, "/admin/login?error="+msg, http.StatusSeeOther)
		return
	}
	_ = h.deps.Store.RecordAudit(storage.AuditEntry{
		At: h.now(), Actor: storage.ActorAdmin, Action: storage.AuditAdminLogin,
		SourceIP: sourceIP(r),
	})
	h.deps.Auth.setSessionCookie(w, id)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (h *Handler) doLogout(w http.ResponseWriter, r *http.Request) {
	h.deps.Auth.Logout(h.deps.Auth.sessionID(r))
	h.deps.Auth.clearSessionCookie(w)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (h *Handler) pageDashboard(w http.ResponseWriter, r *http.Request) {
	csrfToken, ok := h.requirePage(w, r)
	if !ok {
		return
	}
	ov, err := h.overview()
	if err != nil {
		h.fail(w, "overview", err)
		return
	}
	queue, err := h.deps.Store.ListRetryQueue(20)
	if err != nil {
		h.fail(w, "queue", err)
		return
	}
	dead, err := h.deps.Store.ListDeadLetters(20)
	if err != nil {
		h.fail(w, "dead_letters", err)
		return
	}
	h.render(w, r, "dashboard", pageData{
		TitleKey: "label.dashboard", Nav: "dashboard", CSRFToken: csrfToken,
		Overview: &ov, Queue: queue, DeadLetters: maskMessages(dead),
		Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error"),
	})
}

func (h *Handler) pageProviders(w http.ResponseWriter, r *http.Request) {
	csrfToken, ok := h.requirePage(w, r)
	if !ok {
		return
	}
	var managed []managedProviderView
	if h.deps.ProviderManager != nil {
		entries, err := h.deps.ProviderManager.List()
		if err != nil {
			h.fail(w, "managed_providers_list", err)
			return
		}
		for _, p := range entries {
			v := managedProviderView{View: p}
			if h.deps.Quota != nil {
				usage, _ := h.deps.Quota.Snapshot(p.Name, h.now())
				v.UsedToday, v.UsedMonth, v.UsageAvailable = usage.UsedToday, usage.UsedMonth, true
			}
			managed = append(managed, v)
		}
	}
	h.render(w, r, "providers", pageData{
		ManagedProviders: managed, CanManageProviders: h.deps.ProviderManager != nil,
		TitleKey: "label.providers", Nav: "providers", CSRFToken: csrfToken,
		Providers: h.providerViews(),
		Notice:    r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error"),
	})
}

func (h *Handler) pageProjects(w http.ResponseWriter, r *http.Request) {
	csrfToken, ok := h.requirePage(w, r)
	if !ok {
		return
	}
	sups, err := h.deps.Store.ListSuppressions(50)
	if err != nil {
		h.fail(w, "suppressions", err)
		return
	}
	views := make([]suppressionView, 0, len(sups))
	for _, s := range sups {
		views = append(views, suppressionView{
			Email: s.Email, Reason: s.Reason,
			SourceEndpoint: s.SourceEndpoint, CreatedAt: s.CreatedAt,
		})
	}
	h.render(w, r, "projects", pageData{
		TitleKey: "label.projects", Nav: "projects", CSRFToken: csrfToken,
		Projects: h.projectViews(), Suppressions: views, CanCreateProject: h.deps.ProjectKeys != nil,
		Notice: r.URL.Query().Get("notice"), Error: r.URL.Query().Get("error"),
	})
}

func (h *Handler) pageMessages(w http.ResponseWriter, r *http.Request) {
	csrfToken, ok := h.requirePage(w, r)
	if !ok {
		return
	}
	page, err := h.listMessages(r)
	if err != nil {
		h.render(w, r, "messages", pageData{
			TitleKey: "label.messages", Nav: "messages", CSRFToken: csrfToken,
			Error: "invalid filter",
		})
		return
	}
	h.render(w, r, "messages", pageData{
		TitleKey: "label.messages", Nav: "messages", CSRFToken: csrfToken,
		Messages: maskMessages(page.Messages), NextCursor: page.NextCursor,
	})
}

func (h *Handler) pageMessage(w http.ResponseWriter, r *http.Request) {
	csrfToken, ok := h.requirePage(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	msg, found, err := h.deps.Store.QueryMessage(storage.AdminScope(), id)
	if err != nil {
		h.fail(w, "get_message", err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	attempts, _, err := h.deps.Store.QueryAttempts(storage.AdminScope(), id)
	if err != nil {
		h.fail(w, "get_attempts", err)
		return
	}
	events, _, err := h.deps.Store.QueryEvents(storage.AdminScope(), id)
	if err != nil {
		h.fail(w, "get_events", err)
		return
	}
	masked := maskMessage(msg)
	h.render(w, r, "message", pageData{
		TitleKey: "label.message", Nav: "messages", CSRFToken: csrfToken,
		Message: &masked, Attempts: attempts, Events: events,
	})
}

func (h *Handler) pageAudit(w http.ResponseWriter, r *http.Request) {
	csrfToken, ok := h.requirePage(w, r)
	if !ok {
		return
	}
	entries, err := h.deps.Store.ListAudit(storage.MaxPageSize)
	if err != nil {
		h.fail(w, "list_audit", err)
		return
	}
	h.render(w, r, "audit", pageData{
		TitleKey: "label.audit_log", Nav: "audit", CSRFToken: csrfToken, Audit: entries,
	})
}

// doAction is the UI's single form target. It translates a form post
// into the same audited op the JSON API performs, so there is exactly
// one implementation of each action.
func (h *Handler) doAction(w http.ResponseWriter, r *http.Request) {
	kind, csrfToken, ok := h.require(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin?error=bad+request", http.StatusSeeOther)
		return
	}
	if err := checkCSRF(kind, csrfToken, r); err != nil {
		writeError(w, http.StatusForbidden, "csrf_failed", err.Error())
		return
	}

	action := r.PostFormValue("action")
	target := r.PostFormValue("target")
	back := r.PostFormValue("back")
	if back == "" || !strings.HasPrefix(back, "/admin") {
		// Never redirect somewhere a form field chose: an open redirect
		// on an authenticated surface is a phishing primitive.
		back = "/admin"
	}

	var err error
	switch action {
	case "provider_disable":
		err = h.applyProviderDisable(r, target, true)
	case "provider_enable":
		err = h.applyProviderDisable(r, target, false)
	case "breaker_reset":
		err = h.applyBreakerReset(r, target)
	case "project_disable":
		err = h.applyProjectDisable(r, target, true)
	case "project_enable":
		err = h.applyProjectDisable(r, target, false)
	case "dead_letter_retry":
		err = h.applyDeadLetterRetry(r, target)
	case "suppression_remove":
		err = h.applySuppressionRemove(r, target)
	default:
		err = errNotFound
	}
	if err != nil {
		http.Redirect(w, r, back+"?error=action+failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, back+"?notice=done", http.StatusSeeOther)
}

// doProjectKey renders the generated credential directly in the POST response.
// It is never put in a redirect, cookie, log, or later GET response.
func (h *Handler) doProjectKey(w http.ResponseWriter, r *http.Request) {
	kind, csrfToken, ok := h.require(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := checkCSRF(kind, csrfToken, r); err != nil {
		writeError(w, http.StatusForbidden, "csrf_failed", err.Error())
		return
	}
	if h.deps.ProjectKeys == nil {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("name")
	action := "project.key_rotated"
	if name != "" && r.PostFormValue("confirm") != "yes" {
		http.Error(w, "rotation confirmation required", http.StatusBadRequest)
		return
	}
	if name == "" {
		name = r.PostFormValue("name")
		action = "project.created"
	}
	var key string
	err := h.audited(r, action, name, nil, func() (err error) {
		if action == "project.created" {
			key, err = h.deps.ProjectKeys.Create(name)
		} else {
			key, err = h.deps.ProjectKeys.Rotate(name)
		}
		return err
	})
	errorKey := ""
	switch {
	case errors.Is(err, project.ErrInvalidName):
		errorKey = "invalid"
	case errors.Is(err, project.ErrProjectExists):
		errorKey = "exists"
	case errors.Is(err, project.ErrProjectNotManaged):
		errorKey = "not_managed"
	case err != nil:
		h.fail(w, "project_key", err)
		return
	}
	h.render(w, r, "project_key", pageData{TitleKey: "label.projects", Nav: "projects", CSRFToken: csrfToken, NewProjectName: name, NewProjectKey: key, ProjectKeyError: errorKey})
}
