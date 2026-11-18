package admin

import (
	"errors"
	"net/http"
	"time"

	"github.com/craigmccaskill/posthorn/mask"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// --- read endpoints -----------------------------------------------------

// overview is the dashboard's data in one request.
type overview struct {
	Version        string         `json:"version"`
	StorageHealthy bool           `json:"storage_healthy"`
	QueueDepth     int            `json:"queue_depth"`
	DeadLetters    int            `json:"dead_letters"`
	Providers      []providerView `json:"providers"`
	Projects       []projectView  `json:"projects"`
	Suppressions   int            `json:"suppressions"`
	GeneratedAt    time.Time      `json:"generated_at"`
}

func (h *Handler) apiOverview(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	ov, err := h.overview()
	if err != nil {
		h.fail(w, "overview", err)
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

func (h *Handler) overview() (overview, error) {
	ov := overview{
		Version:     h.deps.Version,
		Providers:   h.providerViews(),
		Projects:    h.projectViews(),
		GeneratedAt: h.now().UTC(),
	}
	if h.deps.Gate != nil {
		ov.StorageHealthy = h.deps.Gate.Healthy()
	}
	depth, err := h.deps.Store.QueueDepth()
	if err != nil {
		return overview{}, err
	}
	ov.QueueDepth = depth
	dead, err := h.deps.Store.CountDeadLetters()
	if err != nil {
		return overview{}, err
	}
	ov.DeadLetters = dead
	sups, err := h.deps.Store.ListSuppressions(storage.MaxPageSize)
	if err != nil {
		return overview{}, err
	}
	ov.Suppressions = len(sups)
	return ov, nil
}

// providerView is one provider's operational state.
//
// Note what is NOT here: settings. A provider's config block holds its
// API key, and there is no code path in this package that reads it —
// the surface reports what a provider is DOING, not how it is
// authenticated.
type providerView struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Circuit      string `json:"circuit"`
	Disabled     bool   `json:"disabled_by_operator"`
	Weight       int    `json:"weight,omitempty"`
	CostPer1000  string `json:"cost_per_1000,omitempty"`
	DailyLimit   int64  `json:"daily_limit,omitempty"`
	MonthlyLimit int64  `json:"monthly_limit,omitempty"`
	UsedToday    int64  `json:"used_today"`
	UsedMonth    int64  `json:"used_month"`

	// Capabilities is the provider type's declared surface, so an
	// operator can see in one place whether delivery events for this
	// provider are ingested and whether they are verified.
	Capabilities *capabilityView `json:"capabilities,omitempty"`
}

type capabilityView struct {
	Webhook             string `json:"webhook"`
	WebhookVerification string `json:"webhook_verification"`
	RemoteQuota         string `json:"remote_quota"`
	Notes               string `json:"notes,omitempty"`
}

func (h *Handler) providerViews() []providerView {
	if h.deps.Pool == nil {
		return nil
	}
	now := h.now()
	out := make([]providerView, 0, h.deps.Pool.Len())
	for _, name := range h.deps.Pool.Names() {
		p, ok := h.deps.Pool.Get(name)
		if !ok {
			continue
		}
		v := providerView{
			Name: p.Name, Type: p.Type, Circuit: p.Breaker.State().String(),
			Disabled: p.AdminDisabled(), Weight: p.Weight,
			DailyLimit: p.Quota.Daily, MonthlyLimit: p.Quota.Monthly,
		}
		if p.Cost > 0 {
			v.CostPer1000 = p.Cost.String()
		}
		if h.deps.Quota != nil {
			if usage, found := h.deps.Quota.Snapshot(p.Name, now); found {
				v.UsedToday, v.UsedMonth = usage.UsedToday, usage.UsedMonth
			}
		}
		if caps, found := transport.CapabilitiesFor(p.Type); found {
			v.Capabilities = &capabilityView{
				Webhook:             string(caps.Webhook),
				WebhookVerification: string(caps.WebhookVerification),
				RemoteQuota:         string(caps.RemoteQuota),
				Notes:               caps.Notes,
			}
		}
		out = append(out, v)
	}
	return out
}

func (h *Handler) apiProviders(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": h.providerViews()})
}

// projectView is one tenant's state.
//
// APIKey is absent, not masked-from-the-plaintext: the registry never
// holds the plaintext to begin with. What is shown is a mask derived
// from the stored DIGEST, which identifies the credential without being
// reversible into it.
type projectView struct {
	Name              string   `json:"name"`
	Enabled           bool     `json:"enabled"`
	DisabledByAdmin   bool     `json:"disabled_by_operator"`
	Active            bool     `json:"active"`
	AllowedFrom       []string `json:"allowed_from_domains"`
	DefaultFrom       string   `json:"default_from,omitempty"`
	DailyLimit        int64    `json:"daily_limit,omitempty"`
	MonthlyLimit      int64    `json:"monthly_limit,omitempty"`
	APIKeyFingerprint string   `json:"api_key_fingerprint"`
	Managed           bool     `json:"managed"`

	// ActiveKeys is how many keys currently authenticate. Two means a
	// rotation is in progress — worth showing, because an overlap left
	// open forever is an old credential nobody removed.
	ActiveKeys      int  `json:"active_keys"`
	HasWebhook      bool `json:"has_webhook"`
	HasSMTPIdentity bool `json:"has_smtp_identity"`
}

func (h *Handler) projectViews() []projectView {
	if h.deps.Projects == nil {
		return nil
	}
	out := make([]projectView, 0, h.deps.Projects.Len())
	for _, p := range h.deps.Projects.All() {
		out = append(out, projectView{
			Name: p.Name, Managed: h.deps.ProjectKeys != nil && h.deps.ProjectKeys.IsManaged(p.Name), Enabled: p.Enabled, DisabledByAdmin: p.AdminDisabled(),
			Active: p.Active(), AllowedFrom: p.AllowedFromDomains,
			DefaultFrom: p.DefaultFrom, DailyLimit: p.DailyLimit,
			MonthlyLimit: p.MonthlyLimit,
			// A fingerprint of the digest: enough to tell two keys
			// apart across a rotation, never enough to reconstruct one.
			APIKeyFingerprint: p.KeyFingerprint(),
			ActiveKeys:        p.KeyCount(),
			HasWebhook:        p.WebhookURL != "",
			HasSMTPIdentity:   p.HasSMTPIdentity(),
		})
	}
	return out
}

func (h *Handler) apiProjects(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": h.projectViews()})
}

// maskedMessage is a message as the ADMIN sees it: the envelope with
// recipients masked, and no content at all.
//
// The operator running Posthorn is not the owner of the mail flowing
// through it. They need to see that a message to a gmail.com address
// bounced; they do not need the address, and an admin surface that
// lists them is a harvestable directory of every user of every tenant.
type maskedMessage struct {
	storage.MessageSummary
	To   []string `json:"to"`
	From string   `json:"from"`
}

func maskMessage(m storage.MessageSummary) maskedMessage {
	out := maskedMessage{MessageSummary: m, To: mask.Emails(m.To), From: mask.Email(m.From)}
	// Zero the embedded copies so the masked values are the only ones
	// the encoder can reach.
	out.MessageSummary.To = nil
	out.MessageSummary.From = ""
	return out
}

func maskMessages(in []storage.MessageSummary) []maskedMessage {
	out := make([]maskedMessage, 0, len(in))
	for _, m := range in {
		out = append(out, maskMessage(m))
	}
	return out
}

func (h *Handler) apiMessages(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	page, err := h.listMessages(r)
	if err != nil {
		if errors.Is(err, errBadRequest) {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		h.fail(w, "list_messages", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages":    maskMessages(page.Messages),
		"next_cursor": page.NextCursor,
	})
}

var errBadRequest = errors.New("invalid request")

func (h *Handler) listMessages(r *http.Request) (storage.MessagePage, error) {
	q := r.URL.Query()
	limit, err := storage.ParseLimit(q.Get("limit"))
	if err != nil {
		return storage.MessagePage{}, errors.Join(errBadRequest, err)
	}
	filter := storage.MessageFilter{
		Status: q.Get("status"), Provider: q.Get("provider"),
		MailKind: q.Get("mail_kind"), Limit: limit, Cursor: q.Get("cursor"),
	}
	// The admin scope crosses projects, so a project filter here is a
	// narrowing convenience rather than the security boundary.
	if name := q.Get("project"); name != "" {
		scope, serr := storage.ProjectScope(name)
		if serr != nil {
			return storage.MessagePage{}, errors.Join(errBadRequest, serr)
		}
		return h.deps.Store.QueryMessages(scope, filter)
	}
	return h.deps.Store.QueryMessages(storage.AdminScope(), filter)
}

func (h *Handler) apiMessage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	msg, found, err := h.deps.Store.QueryMessage(storage.AdminScope(), r.PathValue("id"))
	if err != nil {
		h.fail(w, "get_message", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no such message")
		return
	}
	writeJSON(w, http.StatusOK, maskMessage(msg))
}

func (h *Handler) apiAttempts(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	attempts, found, err := h.deps.Store.QueryAttempts(storage.AdminScope(), r.PathValue("id"))
	if err != nil {
		h.fail(w, "get_attempts", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no such message")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"attempts": attempts})
}

func (h *Handler) apiEvents(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	events, found, err := h.deps.Store.QueryEvents(storage.AdminScope(), r.PathValue("id"))
	if err != nil {
		h.fail(w, "get_events", err)
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "no such message")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (h *Handler) apiQueue(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	queue, err := h.deps.Store.ListRetryQueue(storage.MaxPageSize)
	if err != nil {
		h.fail(w, "list_queue", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": queue})
}

func (h *Handler) apiDeadLetters(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	dead, err := h.deps.Store.ListDeadLetters(storage.MaxPageSize)
	if err != nil {
		h.fail(w, "list_dead_letters", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dead_letters": maskMessages(dead)})
}

// suppressionView masks the address for the same reason message
// recipients are masked: this list is precisely a list of real people's
// addresses.
type suppressionView struct {
	Email          string    `json:"email"`
	Reason         string    `json:"reason"`
	SourceEndpoint string    `json:"source_endpoint,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

func (h *Handler) apiSuppressions(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	sups, err := h.deps.Store.ListSuppressions(storage.MaxPageSize)
	if err != nil {
		h.fail(w, "list_suppressions", err)
		return
	}
	out := make([]suppressionView, 0, len(sups))
	for _, s := range sups {
		out = append(out, suppressionView{
			Email: mask.Email(s.Email), Reason: s.Reason,
			SourceEndpoint: s.SourceEndpoint, CreatedAt: s.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"suppressions": out})
}

func (h *Handler) apiAudit(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	limit, err := storage.ParseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	entries, err := h.deps.Store.ListAudit(limit)
	if err != nil {
		h.fail(w, "list_audit", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": entries})
}

func (h *Handler) apiUsage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := h.require(w, r); !ok {
		return
	}
	now := h.now().UTC()
	day, month := now.Format("2006-01-02"), now.Format("2006-01")

	name := r.URL.Query().Get("project")
	if name != "" {
		scope, err := storage.ProjectScope(name)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		report, err := h.deps.Store.QueryUsage(scope, day, month)
		if err != nil {
			h.fail(w, "usage", err)
			return
		}
		writeJSON(w, http.StatusOK, report)
		return
	}

	// No project named: every project's usage, which is a question only
	// the operator can ask.
	reports := make([]storage.UsageReport, 0, h.deps.Projects.Len())
	for _, p := range h.deps.Projects.All() {
		scope, err := storage.ProjectScope(p.Name)
		if err != nil {
			continue
		}
		report, err := h.deps.Store.QueryUsage(scope, day, month)
		if err != nil {
			h.fail(w, "usage", err)
			return
		}
		reports = append(reports, report)
	}
	writeJSON(w, http.StatusOK, map[string]any{"usage": reports})
}
