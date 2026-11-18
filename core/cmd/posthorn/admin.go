package main

import (
	"log/slog"
	"os"
	"time"

	"github.com/craigmccaskill/posthorn/admin"
	"github.com/craigmccaskill/posthorn/managedproviders"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// Admin surface wiring (FR143-FR147).
//
// The admin surface exists only when POSTHORN_ADMIN_TOKEN is set. Not
// configured means not mounted — an admin route that answers when
// nobody enabled it is a door standing open, and "no token configured"
// must never degrade into "no authentication required".

// buildAdmin returns the admin handler, or nil when the deployment has
// not enabled it. An error means the operator TRIED to enable it and
// the configuration is unusable, which must stop startup rather than
// quietly serve nothing.
func buildAdmin(gate *storage.Gate, pool *transport.Pool, projects *project.Registry,
	projectKeys *project.Manager, providerManager *managedproviders.Manager, quota transport.QuotaSource, logger *slog.Logger, secure bool) (*admin.Handler, error) {
	token := os.Getenv(admin.AdminTokenEnv)
	if token == "" {
		return nil, nil
	}
	if gate == nil {
		// The admin surface reads the submission log, the retry queue
		// and the audit log, and its actions are only meaningful if
		// they can be recorded. Without storage there is nothing to
		// show and nowhere to write the audit row.
		return nil, errAdminNeedsStorage
	}
	auth, err := admin.NewAuthenticator(token, secure)
	if err != nil {
		return nil, err
	}
	if err := admin.VerifyDistinct(auth, projects); err != nil {
		return nil, err
	}
	deps := admin.Deps{
		Auth: auth, Store: gate.Store(), Gate: gate, Pool: pool,
		Projects: projects, Quota: quota, Logger: logger, Version: version,
	}
	if projectKeys != nil {
		deps.ProjectKeys = projectKeys
	}
	if providerManager != nil {
		deps.ProviderManager = providerManager
	}
	return admin.New(deps), nil
}

// errAdminNeedsStorage is its own value so the message stays in one place.
var errAdminNeedsStorage = adminError("POSTHORN_ADMIN_TOKEN is set but there is no [storage] block — " +
	"the admin surface reads the submission log and writes an audit row for every action, and can do neither without storage")

type adminError string

func (e adminError) Error() string { return string(e) }

// restoreAdminOverrides re-applies persisted disables at startup.
//
// An override names something the config may no longer define — an
// operator can disable a provider and then remove it from the config.
// A stale override is reported and skipped rather than treated as an
// error: it describes something that is already not running.
func restoreAdminOverrides(store *storage.Store, pool *transport.Pool,
	projects *project.Registry, logger *slog.Logger) {
	overrides, err := store.ListOverrides()
	if err != nil {
		logger.Error("admin_overrides_restore_failed",
			slog.String("error", err.Error()),
			slog.String("effect", "providers or projects an operator disabled may be back in rotation"))
		return
	}
	var restored, stale []string
	for _, o := range overrides {
		switch o.Kind {
		case storage.OverrideProvider:
			if pool == nil {
				continue
			}
			if p, ok := pool.Get(o.Name); ok {
				p.SetAdminDisabled(true)
				restored = append(restored, "provider:"+o.Name)
			} else {
				stale = append(stale, "provider:"+o.Name)
			}
		case storage.OverrideProject:
			if projects == nil {
				continue
			}
			if p, ok := projects.ByName(o.Name); ok {
				p.SetAdminDisabled(true)
				restored = append(restored, "project:"+o.Name)
			} else {
				stale = append(stale, "project:"+o.Name)
			}
		}
	}
	if len(restored) > 0 {
		logger.Warn("admin_overrides_restored",
			slog.Any("disabled", restored),
			slog.String("action", "re-enable from the admin surface when the incident is over"))
	}
	if len(stale) > 0 {
		logger.Info("admin_overrides_stale",
			slog.Any("entries", stale),
			slog.String("reason", "named in an override but not present in the current config"))
	}
}

// recordKeyRotations notices that a project's credential set changed
// since the last start and writes the audit row for it (FR147).
//
// Rotation is a CONFIG edit — add the new key, restart, move callers
// over, remove the old one — so there is no runtime mutation surface to
// audit. What Posthorn can honestly record is the moment a changed set
// took effect, which is this.
func recordKeyRotations(store *storage.Store, projects *project.Registry, logger *slog.Logger) {
	now := time.Now()
	for _, p := range projects.All() {
		changed, err := store.RecordKeySet(p.Name, p.KeySetDigest(), now)
		if err != nil {
			logger.Error("project_key_state_failed",
				slog.String("project", p.Name), slog.String("error", err.Error()))
			continue
		}
		if !changed {
			continue
		}
		if err := store.RecordAudit(storage.AuditEntry{
			At: now, Actor: storage.ActorCLI, Action: storage.AuditAPIKeyRotated,
			Target: p.Name,
			Details: map[string]string{
				"fingerprint": p.KeyFingerprint(),
				"source":      "config",
			},
		}); err != nil {
			logger.Error("audit_write_failed",
				slog.String("action", storage.AuditAPIKeyRotated),
				slog.String("error", err.Error()))
			continue
		}
		// Say what actually changed rather than guessing at the phase:
		// during an overlap the old key still works, and after the
		// overlap closes it does not. The key COUNT is what tells an
		// operator which of the two they are looking at.
		effect := "any credential not in the current set no longer authenticates"
		if p.KeyCount() > 1 {
			effect = "a rotation is in progress: every key in the current set authenticates, " +
				"so callers can migrate before the retired key is removed"
		}
		logger.Warn("project_credentials_changed",
			slog.String("project", p.Name),
			slog.String("fingerprint", p.KeyFingerprint()),
			slog.Int("active_keys", p.KeyCount()),
			slog.String("effect", effect))
	}
}
