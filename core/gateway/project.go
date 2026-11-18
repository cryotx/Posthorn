package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/craigmccaskill/posthorn/idempotency"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/response"
	"github.com/craigmccaskill/posthorn/storage"
)

// Project-mode request handling (FR107-FR113).
//
// This file holds everything that only applies once [[projects]] are
// configured. The pre-project paths in handler.go are untouched, which
// is how NFR42 stays true: an operator with no projects runs the same
// code they ran before, not a variant of it with the project branch
// evaluating to false.

// fromField is the api-mode per-request sender (FR109). It is honoured
// ONLY under project authentication, where it is bounded by the
// project's allowed_from_domains — see ADR-33 for why that revision of
// ADR-11 is safe when the plain api-key case is not.
const fromField = "from"

// projectScope is the idempotency namespace for a project. Keys are
// scoped per project so two tenants generating the same UUID cannot
// collide, and so one tenant can never observe another's response.
func projectScope(name string) string { return "project:" + name }

// fingerprint hashes the request payload that an idempotency key
// stands for. Two requests with the same key must carry the same
// payload; a different one is a caller bug, not a replay.
func fingerprint(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// canonicalPayload renders the parts of a request that define what
// message it asks for, in a stable order.
//
// Deliberately built from the PARSED values rather than the raw body:
// a caller that re-serialises its JSON with different key ordering or
// whitespace is asking for the same message and should get a replay,
// not a conflict. What must differ to count as a different request is
// the field values, the resolved sender, and the resolved recipients —
// exactly what is hashed here.
//
// The honeypot field is excluded for the same reason it is excluded
// from templates: it is Posthorn's own plumbing, not the caller's
// payload.
func canonicalPayload(form url.Values, honeypot, from string, to []string) string {
	keys := make([]string, 0, len(form))
	for k := range form {
		if honeypot != "" && k == honeypot {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("from=")
	b.WriteString(from)
	b.WriteString("\nto=")
	b.WriteString(strings.Join(append([]string(nil), to...), ","))
	for _, k := range keys {
		vals := append([]string(nil), form[k]...)
		sort.Strings(vals)
		b.WriteString("\n")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(strings.Join(vals, "\x00"))
	}
	return b.String()
}

// authenticateProject resolves the request's Bearer token to a project.
//
// Returns (nil, false) when project mode is off or the token is not a
// project key, so the caller can fall through to the endpoint's own
// api_keys — which is what lets an operator adopt projects one endpoint
// at a time.
func (h *Handler) authenticateProject(r *http.Request) (*project.Project, *project.AuthError) {
	if h.projects.Len() == 0 {
		return nil, nil
	}
	token, ok := bearerToken(r)
	if !ok {
		return nil, nil
	}
	p, err := h.projects.Authenticate(token)
	if err == nil {
		return p, nil
	}
	var ae *project.AuthError
	if asAuthError(err, &ae) {
		if ae.Reason == "disabled" {
			// A valid credential for a suspended project must not fall
			// through to the endpoint keys — that would make disabling
			// a project depend on it not sharing a key by accident.
			return nil, ae
		}
	}
	return nil, nil
}

func asAuthError(err error, target **project.AuthError) bool {
	if ae, ok := err.(*project.AuthError); ok {
		*target = ae
		return true
	}
	return false
}

// resolveProjectFrom applies the project's sender policy (FR109).
//
// requested is the caller's `from` field (empty when absent). The
// fallback is the endpoint's configured `from`, which must itself pass
// the allowlist — an endpoint is shared infrastructure and must not be
// a way for one project to send as another's domain.
func (h *Handler) resolveProjectFrom(p *project.Project, requested string) (string, error) {
	return p.ResolveFrom(strings.TrimSpace(requested), h.cfg.From)
}

// idemOutcome is what the project-scoped idempotency guard decided.
type idemOutcome int

const (
	idemProceed idemOutcome = iota
	idemHandled             // a response has already been written
)

// idemGuard owns one request's idempotency reservation.
type idemGuard struct {
	handler *Handler
	key     string
	scope   string
	active  bool
}

// beginIdempotent reserves (project, key) atomically before any send
// (FR113).
//
// The reservation is taken in the database rather than in a map: two
// concurrent requests carrying the same key must not both find "no
// entry" and both send a verification code. The loser of the race gets
// 409, not a second email.
//
// Without storage there is nothing to reserve in, so the request
// proceeds unguarded — the same NFR27 posture the rest of the pipeline
// takes, and it is logged so the operator can see it.
func (h *Handler) beginIdempotent(w http.ResponseWriter, logger *slog.Logger, proj *project.Project, key, messageID string, body []byte, start time.Time) (*idemGuard, idemOutcome) {
	guard := &idemGuard{handler: h, key: key, scope: projectScope(proj.Name)}
	if h.storageGate == nil || !h.storageGate.Healthy() {
		logger.Warn("idempotency_unguarded",
			slog.String("reason", "storage unavailable"),
			slog.String("effect", "a concurrent retry of this key could send twice"))
		return guard, idemProceed
	}

	rec, err := h.storageGate.Store().ReserveIdempotent(
		key, guard.scope, fingerprint(body), messageID, time.Now(), idempotency.DefaultTTL)
	if err != nil {
		h.storageGate.ReportError(err)
		return guard, idemProceed
	}

	switch rec.State {
	case storage.IdemReserved:
		guard.active = true
		return guard, idemProceed

	case storage.IdemReplay:
		logger.Info("idempotent_replay",
			slog.Int("replayed_status", rec.Status),
			slog.String("message_id", rec.MessageID),
			slog.Int64("latency_ms", time.Since(start).Milliseconds()),
		)
		h.recorder.IdempotentReplay(h.cfg.Path)
		replayCachedResponse(w, &idempotency.Response{
			Status: rec.Status, Body: rec.Body, ContentType: rec.ContentType,
		})
		return guard, idemHandled

	case storage.IdemConflict:
		// Same key, different payload. Replaying the stored response
		// would answer a question the caller did not ask; sending would
		// defeat the key. Both are worse than saying so.
		logger.Warn("idempotent_payload_conflict",
			slog.Int64("latency_ms", time.Since(start).Milliseconds()))
		response.WriteJSON(w, http.StatusConflict, response.Error{
			Error: "this Idempotency-Key was already used with a different request payload",
			Code:  "idempotency_key_reused",
		})
		return guard, idemHandled

	default: // storage.IdemInFlight
		logger.Info("idempotent_conflict",
			slog.Int64("latency_ms", time.Since(start).Milliseconds()))
		response.WriteJSON(w, http.StatusConflict, response.Error{
			Error: "a request with this Idempotency-Key is already in flight",
			Code:  "idempotency_in_flight",
		})
		return guard, idemHandled
	}
}

// complete records the response so an identical retry replays it.
func (g *idemGuard) complete(messageID string, rw *recordingResponseWriter) {
	if !g.active || g.handler.storageGate == nil || !g.handler.storageGate.Healthy() {
		return
	}
	// Only terminally-handled outcomes are replayable. Caching a 5xx
	// would turn a transient failure into a permanent one for the whole
	// TTL — the caller's retry is exactly what should reach a provider.
	if rw.status < 200 || rw.status >= 400 {
		if err := g.handler.storageGate.Store().ReleaseIdempotent(g.key, g.scope); err != nil {
			g.handler.storageGate.ReportError(err)
		}
		return
	}
	err := g.handler.storageGate.Store().CompleteIdempotent(
		g.key, g.scope, messageID, rw.status, rw.Header().Get("Content-Type"),
		rw.body.Bytes(), time.Now(), idempotency.DefaultTTL)
	if err != nil {
		g.handler.storageGate.ReportError(err)
	}
}

// release hands the key back when nothing was recorded, so a crash or
// an early return does not hold it for the full TTL.
func (g *idemGuard) release() {
	if !g.active || g.handler.storageGate == nil || !g.handler.storageGate.Healthy() {
		return
	}
	if err := g.handler.storageGate.Store().ReleaseIdempotent(g.key, g.scope); err != nil {
		g.handler.storageGate.ReportError(err)
	}
}

// bearerToken extracts the Authorization: Bearer credential.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

// recordProjectOutcome fans one send outcome out to the project's
// metrics and durable usage counters (FR115, FR116). A nil project
// (non-project mode) makes every call a no-op, so the pre-project path
// pays nothing.
func (h *Handler) recordProjectOutcome(p *project.Project, status, provider string, latency time.Duration) {
	if p == nil {
		return
	}
	h.recorder.ProjectSend(p.Name, status)
	h.recorder.ProjectSendDuration(p.Name, latency)
	if provider != "" {
		h.recorder.ProjectProvider(p.Name, provider)
	}
	// Outcomes that never held a quota reservation still belong in the
	// usage counters. The ones that did are recorded by the
	// reservation's commit, so they must not be double-counted here.
	switch status {
	case "sent", "queued", "failed":
		// Already counted by settleQuota.
	default:
		h.projectUsage.Record(p.Name, false)
	}
}

// projectMessageID surfaces the Posthorn message id under its documented
// name for project callers.
//
// It is the SAME value as submission_id, not a second identifier: one
// logical message has one id, stable across every provider attempt
// (ADR-34). The field exists so a business system integrating against
// the project API can use the name the events and the attempt trail
// use, without Posthorn minting a redundant id to go with it.
func projectMessageID(p *project.Project, submissionID string) string {
	if p == nil {
		return ""
	}
	return submissionID
}

// projectName is the nil-safe name accessor used at call sites that
// only need the label.
func projectName(p *project.Project) string {
	if p == nil {
		return ""
	}
	return p.Name
}
