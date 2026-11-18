package admin

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/craigmccaskill/posthorn/mask"
	"github.com/craigmccaskill/posthorn/storage"
)

// The audited operations (FR144, FR147).
//
// Every privileged action lives here exactly once and is reached two
// ways — a JSON POST and the UI's form — so the API and the UI cannot
// drift into different behaviour, and neither can skip the audit.
//
// The order inside audited() is deliberate: the audit row is written
// BEFORE the change takes effect, and a failure to write it aborts the
// action. A crash between the two then leaves a recorded action that
// did not happen, which an operator can check and correct. The reverse
// ordering would leave a change nobody can trace, which they cannot.

var errNotFound = errors.New("not found")

// audited records the action and then applies it.
func (h *Handler) audited(r *http.Request, action, target string,
	details map[string]string, apply func() error) error {
	entry := storage.AuditEntry{
		At: h.now(), Actor: storage.ActorAdmin, Action: action,
		Target: target, Details: details, SourceIP: sourceIP(r),
	}
	if err := h.deps.Store.RecordAudit(entry); err != nil {
		return err
	}
	if err := apply(); err != nil {
		return err
	}
	if h.deps.Logger != nil {
		// Warn, not Info: a privileged change is the kind of line an
		// operator should see without turning the level up.
		h.deps.Logger.Warn("admin_action",
			slog.String("action", action), slog.String("target", target),
			slog.String("source_ip", sourceIP(r)))
	}
	return nil
}

func (h *Handler) applyProviderDisable(r *http.Request, name string, disabled bool) error {
	action, details := storage.AuditProviderEnabled, map[string]string(nil)
	if disabled {
		action = storage.AuditProviderDisabled
		if reason := r.FormValue("reason"); reason != "" {
			details = map[string]string{"reason": reason}
		}
	}
	return h.audited(r, action, name, details, func() error {
		if h.deps.Pool == nil {
			return errNotFound
		}
		p, ok := h.deps.Pool.Get(name)
		if !ok {
			return errNotFound
		}
		if disabled {
			if err := h.deps.Store.SetOverride(
				storage.OverrideProvider, name, r.FormValue("reason"), h.now()); err != nil {
				return err
			}
		} else if _, err := h.deps.Store.ClearOverride(storage.OverrideProvider, name); err != nil {
			return err
		}
		// Clearing the override restores the CONFIGURED state; it can
		// never summon a provider the config left out.
		p.SetAdminDisabled(disabled)
		return nil
	})
}

func (h *Handler) applyBreakerReset(r *http.Request, name string) error {
	return h.audited(r, storage.AuditBreakerReset, name, nil, func() error {
		if h.deps.Pool == nil {
			return errNotFound
		}
		p, ok := h.deps.Pool.Get(name)
		if !ok {
			return errNotFound
		}
		p.Breaker.Reset()
		return nil
	})
}

func (h *Handler) applyProjectDisable(r *http.Request, name string, disabled bool) error {
	action, details := storage.AuditProjectEnabled, map[string]string(nil)
	if disabled {
		action = storage.AuditProjectDisabled
		if reason := r.FormValue("reason"); reason != "" {
			details = map[string]string{"reason": reason}
		}
	}
	return h.audited(r, action, name, details, func() error {
		if h.deps.Projects == nil {
			return errNotFound
		}
		p, ok := h.deps.Projects.ByName(name)
		if !ok {
			return errNotFound
		}
		if disabled {
			if err := h.deps.Store.SetOverride(
				storage.OverrideProject, name, r.FormValue("reason"), h.now()); err != nil {
				return err
			}
		} else if _, err := h.deps.Store.ClearOverride(storage.OverrideProject, name); err != nil {
			return err
		}
		p.SetAdminDisabled(disabled)
		return nil
	})
}

func (h *Handler) applyDeadLetterRetry(r *http.Request, id string) error {
	return h.audited(r, storage.AuditDeadLetterRetry, id, nil, func() error {
		return h.deps.Store.RequeueDeadLetter(id, h.now())
	})
}

func (h *Handler) applySuppressionRemove(r *http.Request, email string) error {
	// The audit TARGET is masked: an audit log is read far more often
	// than the suppression list, and it should not become the place
	// every unsuppressed address is written in the clear.
	return h.audited(r, storage.AuditSuppressionRemoved, mask.Email(email), nil, func() error {
		n, err := h.deps.Store.RemoveSuppression(email)
		if err != nil {
			return err
		}
		if n == 0 {
			return errNotFound
		}
		return nil
	})
}

// --- JSON entry points --------------------------------------------------

// op wraps one action for the JSON API: authenticate, CSRF-check, run.
func (h *Handler) op(w http.ResponseWriter, r *http.Request, run func() error) {
	kind, csrfToken, ok := h.require(w, r)
	if !ok {
		return
	}
	// Parsing the form here so an action's `reason` can arrive either as
	// a query parameter or a form field, and so checkCSRF can read a
	// posted token.
	_ = r.ParseForm()
	if err := checkCSRF(kind, csrfToken, r); err != nil {
		writeError(w, http.StatusForbidden, "csrf_failed", err.Error())
		return
	}
	if err := run(); err != nil {
		switch {
		case errors.Is(err, errNotFound):
			writeError(w, http.StatusNotFound, "not_found", "no such target")
		case errors.Is(err, storage.ErrNotReplayable):
			writeError(w, http.StatusBadRequest, "action_failed", err.Error())
		default:
			h.fail(w, "admin_action", err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) opProviderDisable(w http.ResponseWriter, r *http.Request) {
	h.op(w, r, func() error { return h.applyProviderDisable(r, r.PathValue("name"), true) })
}

func (h *Handler) opProviderEnable(w http.ResponseWriter, r *http.Request) {
	h.op(w, r, func() error { return h.applyProviderDisable(r, r.PathValue("name"), false) })
}

func (h *Handler) opBreakerReset(w http.ResponseWriter, r *http.Request) {
	h.op(w, r, func() error { return h.applyBreakerReset(r, r.PathValue("name")) })
}

func (h *Handler) opProjectDisable(w http.ResponseWriter, r *http.Request) {
	h.op(w, r, func() error { return h.applyProjectDisable(r, r.PathValue("name"), true) })
}

func (h *Handler) opProjectEnable(w http.ResponseWriter, r *http.Request) {
	h.op(w, r, func() error { return h.applyProjectDisable(r, r.PathValue("name"), false) })
}

func (h *Handler) opDeadLetterRetry(w http.ResponseWriter, r *http.Request) {
	h.op(w, r, func() error { return h.applyDeadLetterRetry(r, r.PathValue("id")) })
}

func (h *Handler) opSuppressionRemove(w http.ResponseWriter, r *http.Request) {
	h.op(w, r, func() error { return h.applySuppressionRemove(r, r.PathValue("email")) })
}
