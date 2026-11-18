// Modified for the cryotx/posthorn distribution.

package metrics

import "time"

// LatencyBuckets is the histogram bucket set Posthorn uses for send
// latency. Covers sub-millisecond to 10s in a power-of-roughly-5 spacing
// that maps cleanly to the operator-meaningful regions (idempotent
// replay → ~ms; happy-path Postmark → ~300ms; retry → ~1s; near-timeout
// → ~5s; hard-timeout boundary → 10s).
var LatencyBuckets = []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 10}

// Recorder is the typed entry point Posthorn's gateway and cmd/posthorn
// call to record metrics. Each method names the event semantically; the
// concrete metric storage (counters/histograms) is hidden so future
// metrics additions don't require gateway-side code changes.
//
// A nil *Recorder is a no-op — gateway code can call Recorder methods
// without an enabling-condition check; tests that don't care about
// metrics pass nil.
type Recorder struct {
	submitted        *Counter
	sent             *Counter
	failed           *Counter
	rateLimited      *Counter
	authFailed       *Counter
	spamBlocked      *Counter
	checkFailedOpen  *Counter
	validationFailed *Counter
	idempotentReplay *Counter
	sendLatency      *Histogram

	// v2.0 storage surface (FR78, FR80).
	queued          *Counter
	queueSent       *Counter
	queueRetried    *Counter
	queueDeadLetter *Counter
	storageHealthy  *Gauge
	retryQueueDepth *Gauge

	// v2.0 lifecycle + suppression surface (FR82-FR86). Label values
	// are fixed enums / endpoint paths only (NFR30).
	suppressed        *Counter
	lifecycleEvents   *Counter
	lifecycleDropped  *Counter
	lifecycleForwards *Counter

	// v2.1 multi-provider routing surface (FR97). The provider label is
	// the operator-chosen [[providers]] name and the outcome label is
	// the ErrorClass enum plus "sent" — both operator/enum content, so
	// NFR24 holds as it does everywhere else in this file.
	providerAttempts *Counter
	providerLatency  *Histogram
	providerSkipped  *Counter
	circuitState     *Gauge
	failovers        *Counter
	routeExhausted   *Counter

	// v2.2 quota + cost surface (FR103). provider and window are
	// operator-configured / closed-enum values, so NFR24 holds.
	quotaExhausted   *Counter
	quotaRemaining   *Gauge
	quotaLimit       *Gauge
	routeQuotaOut    *Counter
	providerCostGaug *Gauge

	// v2.3 multi-tenant surface (FR116). Every label is a project
	// name, a provider name, or a closed enum — never a recipient,
	// subject, address or message id, which would be unbounded
	// cardinality AND submitter content (NFR24, NFR47).
	projectSend     *Counter
	projectDelivery *Counter
	projectProvider *Counter
	projectDuration *Histogram

	// v2.4 webhook + project-quota surface (FR124, FR125). All label
	// values are provider names, project names, or closed enums.
	providerWebhook     *Counter
	projectQuotaLimit   *Gauge
	projectQuotaLeft    *Gauge
	projectQuotaBlocked *Counter
}

// Provider webhook outcomes. Closed, low-cardinality enum — an error
// string as a label would be unbounded cardinality AND could carry
// payload content.
const (
	WebhookAccepted         = "accepted"
	WebhookInvalidSignature = "invalid_signature"
	WebhookDuplicate        = "duplicate"
	WebhookInvalidPayload   = "invalid_payload"
	WebhookUnknownMessage   = "unknown_message"
	WebhookInternalError    = "internal_error"
)

// NewRecorder constructs a Recorder backed by counters and histograms
// registered with reg. Call once per Posthorn process.
func NewRecorder(reg *Registry) *Recorder {
	r := &Recorder{
		submitted: NewCounter(
			"posthorn_submissions_received_total",
			"Submissions accepted past the body-parse stage, before transport.Send.",
			[]string{"endpoint"},
		),
		sent: NewCounter(
			"posthorn_submissions_sent_total",
			"Submissions successfully delivered to the upstream transport.",
			[]string{"endpoint", "transport"},
		),
		failed: NewCounter(
			"posthorn_submissions_failed_total",
			"Submissions that reached transport.Send but failed terminally (no retry, 502 to client).",
			[]string{"endpoint", "transport", "error_class"},
		),
		rateLimited: NewCounter(
			"posthorn_rate_limited_total",
			"Submissions rejected at the rate-limit gate (HTTP 429).",
			[]string{"endpoint"},
		),
		authFailed: NewCounter(
			"posthorn_auth_failed_total",
			"API-mode submissions rejected with HTTP 401 (missing or invalid Bearer token).",
			[]string{"endpoint"},
		),
		spamBlocked: NewCounter(
			"posthorn_spam_blocked_total",
			"Form-mode submissions rejected by spam defenses (honeypot/origin silent-200; csrf/reputation 403).",
			[]string{"endpoint", "kind"},
		),
		checkFailedOpen: NewCounter(
			"posthorn_check_failed_open_total",
			"Spam checks whose provider errored and were allowed through (fail-open). A rising rate is a silent-bypass window. check is \"reputation\" or \"captcha\".",
			[]string{"endpoint", "check"},
		),
		validationFailed: NewCounter(
			"posthorn_validation_failed_total",
			"Submissions rejected with HTTP 422 for missing required fields or malformed email.",
			[]string{"endpoint"},
		),
		idempotentReplay: NewCounter(
			"posthorn_idempotent_replay_total",
			"API-mode requests served from the idempotency cache without re-sending.",
			[]string{"endpoint"},
		),
		sendLatency: NewHistogram(
			"posthorn_send_latency_seconds",
			"Wall-clock latency of transport.Send, including any retries.",
			LatencyBuckets,
			[]string{"endpoint", "transport"},
		),
		queued: NewCounter(
			"posthorn_submissions_queued_total",
			"Submissions whose inline retries exhausted transiently and entered the background retry queue (202 to api-mode callers).",
			[]string{"endpoint", "transport"},
		),
		queueSent: NewCounter(
			"posthorn_queue_sent_total",
			"Queued submissions delivered by a background retry.",
			[]string{"endpoint"},
		),
		queueRetried: NewCounter(
			"posthorn_queue_retries_total",
			"Background retry attempts that failed transiently and rescheduled.",
			[]string{"endpoint"},
		),
		queueDeadLetter: NewCounter(
			"posthorn_queue_dead_lettered_total",
			"Queued submissions abandoned as failed (terminal error or attempts exhausted).",
			[]string{"endpoint"},
		),
		storageHealthy: NewGauge(
			"posthorn_storage_healthy",
			"1 when the storage canary write succeeds, 0 while degraded (mail still flows synchronously; nothing persists).",
			nil,
		),
		retryQueueDepth: NewGauge(
			"posthorn_retry_queue_depth",
			"Submissions currently waiting in (or claimed from) the retry queue.",
			nil,
		),
		suppressed: NewCounter(
			"posthorn_suppressed_recipients_total",
			"Recipients removed from sends by the suppression list (FR86).",
			[]string{"endpoint"},
		),
		lifecycleEvents: NewCounter(
			"posthorn_lifecycle_events_total",
			"Provider lifecycle events accepted and normalized. event is the fixed normalized enum.",
			[]string{"event"},
		),
		lifecycleDropped: NewCounter(
			"posthorn_lifecycle_dropped_total",
			"Inbound lifecycle posts dropped without processing. reason is \"unmatched\" (no submission for the message ID) or \"malformed\".",
			[]string{"reason"},
		),
		providerAttempts: NewCounter(
			"posthorn_provider_attempts_total",
			"Send attempts per pool provider. outcome is \"sent\", \"transient\", \"rate_limited\", \"terminal\", or \"unknown\".",
			[]string{"provider", "outcome"},
		),
		providerLatency: NewHistogram(
			"posthorn_provider_latency_seconds",
			"Wall-clock latency of one provider attempt, successful or not.",
			LatencyBuckets,
			[]string{"provider"},
		),
		providerSkipped: NewCounter(
			"posthorn_provider_skipped_total",
			"Attempts skipped because the provider's circuit breaker was open.",
			[]string{"provider"},
		),
		circuitState: NewGauge(
			"posthorn_provider_circuit_state",
			"Circuit-breaker state per provider: 0 = closed, 1 = half-open, 2 = open.",
			[]string{"provider"},
		),
		failovers: NewCounter(
			"posthorn_route_failover_total",
			"Messages delivered by a provider other than the route's first pick.",
			[]string{"router"},
		),
		routeExhausted: NewCounter(
			"posthorn_route_exhausted_total",
			"Messages where every provider in the route failed or was skipped.",
			[]string{"router"},
		),
		quotaExhausted: NewCounter(
			"posthorn_provider_quota_exhausted_total",
			"Attempts skipped because the provider had spent a configured send budget. window is \"daily\" or \"monthly\".",
			[]string{"provider", "window"},
		),
		quotaRemaining: NewGauge(
			"posthorn_provider_quota_remaining",
			"Sends left in the current window for providers with a configured limit. Absent for unlimited providers.",
			[]string{"provider", "window"},
		),
		quotaLimit: NewGauge(
			"posthorn_provider_quota_limit",
			"The configured send limit per window. Pairs with posthorn_provider_quota_remaining for a used/limit ratio.",
			[]string{"provider", "window"},
		),
		routeQuotaOut: NewCounter(
			"posthorn_route_quota_exhausted_total",
			"Messages that failed because every provider in the route was out of budget (no provider was unhealthy).",
			[]string{"router"},
		),
		providerCostGaug: NewGauge(
			"posthorn_provider_cost_per_1000_micros",
			"Configured price per 1000 messages, in millionths of a currency unit. Static; join against attempts for spend.",
			[]string{"provider"},
		),
		projectSend: NewCounter(
			"posthorn_project_send_total",
			"Send attempts per project. status is \"sent\", \"queued\", \"failed\", \"suppressed\", \"sender_rejected\" or \"rejected\".",
			[]string{"project", "status"},
		),
		projectDelivery: NewCounter(
			"posthorn_project_delivery_total",
			"Normalized delivery events per project, by unified event type.",
			[]string{"project", "event"},
		),
		projectProvider: NewCounter(
			"posthorn_project_provider_total",
			"Successful sends per project and delivering provider.",
			[]string{"project", "provider"},
		),
		projectDuration: NewHistogram(
			"posthorn_project_send_duration_seconds",
			"Wall-clock latency of a project's send, from request to terminal outcome.",
			LatencyBuckets,
			[]string{"project"},
		),
		providerWebhook: NewCounter(
			"posthorn_provider_webhook_total",
			"Inbound provider webhook requests by outcome: accepted, invalid_signature, duplicate, invalid_payload, unknown_message, internal_error.",
			[]string{"provider", "result"},
		),
		projectQuotaLimit: NewGauge(
			"posthorn_project_quota_limit",
			"The configured send limit per project and window. Absent for projects with no limit.",
			[]string{"project", "window"},
		),
		projectQuotaLeft: NewGauge(
			"posthorn_project_quota_remaining",
			"Sends left in the current window for projects with a configured limit.",
			[]string{"project", "window"},
		),
		projectQuotaBlocked: NewCounter(
			"posthorn_project_quota_exhausted_total",
			"Requests refused because the project had spent its own send budget. window is \"daily\" or \"monthly\".",
			[]string{"project", "window"},
		),
		lifecycleForwards: NewCounter(
			"posthorn_lifecycle_forwards_total",
			"Callback forwarding outcomes per originating endpoint. outcome is \"sent\", \"queued\", or \"dropped\".",
			[]string{"endpoint", "outcome"},
		),
	}
	reg.Register(r.submitted)
	reg.Register(r.sent)
	reg.Register(r.failed)
	reg.Register(r.rateLimited)
	reg.Register(r.authFailed)
	reg.Register(r.spamBlocked)
	reg.Register(r.checkFailedOpen)
	reg.Register(r.validationFailed)
	reg.Register(r.idempotentReplay)
	reg.Register(r.sendLatency)
	reg.Register(r.queued)
	reg.Register(r.queueSent)
	reg.Register(r.queueRetried)
	reg.Register(r.queueDeadLetter)
	reg.Register(r.storageHealthy)
	reg.Register(r.retryQueueDepth)
	reg.Register(r.suppressed)
	reg.Register(r.lifecycleEvents)
	reg.Register(r.lifecycleDropped)
	reg.Register(r.lifecycleForwards)
	reg.Register(r.providerAttempts)
	reg.Register(r.providerLatency)
	reg.Register(r.providerSkipped)
	reg.Register(r.circuitState)
	reg.Register(r.failovers)
	reg.Register(r.routeExhausted)
	reg.Register(r.quotaExhausted)
	reg.Register(r.quotaRemaining)
	reg.Register(r.quotaLimit)
	reg.Register(r.routeQuotaOut)
	reg.Register(r.providerCostGaug)
	reg.Register(r.projectSend)
	reg.Register(r.projectDelivery)
	reg.Register(r.projectProvider)
	reg.Register(r.projectDuration)
	reg.Register(r.providerWebhook)
	reg.Register(r.projectQuotaLimit)
	reg.Register(r.projectQuotaLeft)
	reg.Register(r.projectQuotaBlocked)
	return r
}

// Submitted records a submission that passed body parse + validation
// and is about to enter the transport. Called once per request before
// transport.Send.
func (r *Recorder) Submitted(endpoint string) {
	if r == nil {
		return
	}
	r.submitted.Inc(endpoint)
}

// Sent records a successful upstream delivery and observes the latency.
func (r *Recorder) Sent(endpoint, transport string, latency time.Duration) {
	if r == nil {
		return
	}
	r.sent.Inc(endpoint, transport)
	r.sendLatency.Observe(latency.Seconds(), endpoint, transport)
}

// Failed records a terminal transport failure (no retry, 502 to client).
// errorClass is the ErrorClass.String() value ("transient" / "rate_limited"
// / "terminal" / "unknown").
func (r *Recorder) Failed(endpoint, transport, errorClass string) {
	if r == nil {
		return
	}
	r.failed.Inc(endpoint, transport, errorClass)
}

// RateLimited records a 429 response at the rate-limit gate.
func (r *Recorder) RateLimited(endpoint string) {
	if r == nil {
		return
	}
	r.rateLimited.Inc(endpoint)
}

// AuthFailed records a 401 response from the API-mode auth check.
func (r *Recorder) AuthFailed(endpoint string) {
	if r == nil {
		return
	}
	r.authFailed.Inc(endpoint)
}

// SpamBlocked records a spam-check rejection. kind is "honeypot",
// "origin", "csrf", or "reputation" — an operator-facing enum, never
// submitter content (NFR24).
func (r *Recorder) SpamBlocked(endpoint, kind string) {
	if r == nil {
		return
	}
	r.spamBlocked.Inc(endpoint, kind)
}

// CheckFailedOpen records a spam check whose provider errored and was
// allowed through (fail-open) — a silent-bypass signal. check is the
// operator-facing check name ("reputation", "captcha"), never submitter
// content (NFR24).
func (r *Recorder) CheckFailedOpen(endpoint, check string) {
	if r == nil {
		return
	}
	r.checkFailedOpen.Inc(endpoint, check)
}

// ValidationFailed records a 422 response for required-fields or
// email-format failure.
func (r *Recorder) ValidationFailed(endpoint string) {
	if r == nil {
		return
	}
	r.validationFailed.Inc(endpoint)
}

// IdempotentReplay records a cache-hit replay (no transport send).
func (r *Recorder) IdempotentReplay(endpoint string) {
	if r == nil {
		return
	}
	r.idempotentReplay.Inc(endpoint)
}

// Queued records a submission entering the background retry queue after
// exhausting inline retries (FR78).
func (r *Recorder) Queued(endpoint, transport string) {
	if r == nil {
		return
	}
	r.queued.Inc(endpoint, transport)
}

// QueueSent records a background retry that delivered.
func (r *Recorder) QueueSent(endpoint string) {
	if r == nil {
		return
	}
	r.queueSent.Inc(endpoint)
}

// QueueRetried records a background retry that failed transiently and
// rescheduled.
func (r *Recorder) QueueRetried(endpoint string) {
	if r == nil {
		return
	}
	r.queueRetried.Inc(endpoint)
}

// QueueDeadLettered records a queued submission abandoned as failed.
func (r *Recorder) QueueDeadLettered(endpoint string) {
	if r == nil {
		return
	}
	r.queueDeadLetter.Inc(endpoint)
}

// SetStorageHealthy flips the FR80 canary gauge.
func (r *Recorder) SetStorageHealthy(healthy bool) {
	if r == nil {
		return
	}
	v := 0.0
	if healthy {
		v = 1
	}
	r.storageHealthy.Set(v)
}

// SetQueueDepth publishes the current retry-queue depth.
func (r *Recorder) SetQueueDepth(n int) {
	if r == nil {
		return
	}
	r.retryQueueDepth.Set(float64(n))
}

// Suppressed records recipients removed from a send by the suppression
// list (FR86). n is the number of removed recipients.
func (r *Recorder) Suppressed(endpoint string, n int) {
	if r == nil || n <= 0 {
		return
	}
	r.suppressed.Add(int64(n), endpoint)
}

// LifecycleEvent records one accepted, normalized provider event. kind
// must be a lifecycle.Event* enum value (NFR30).
func (r *Recorder) LifecycleEvent(kind string) {
	if r == nil {
		return
	}
	r.lifecycleEvents.Inc(kind)
}

// LifecycleDropped records an inbound event post dropped without
// processing. reason is "unmatched" or "malformed".
func (r *Recorder) LifecycleDropped(reason string) {
	if r == nil {
		return
	}
	r.lifecycleDropped.Inc(reason)
}

// LifecycleForward records a callback delivery outcome ("sent",
// "queued", "dropped").
func (r *Recorder) LifecycleForward(endpoint, outcome string) {
	if r == nil {
		return
	}
	r.lifecycleForwards.Inc(endpoint, outcome)
}

// --- v2.1 multi-provider routing (FR97) ---

// ProviderAttempt records one provider attempt and its latency.
// outcome is "sent" or an ErrorClass.String() value. Wired as
// transport.RouterHooks.OnAttempt.
func (r *Recorder) ProviderAttempt(provider, outcome string, latency time.Duration) {
	if r == nil {
		return
	}
	r.providerAttempts.Inc(provider, outcome)
	r.providerLatency.Observe(latency.Seconds(), provider)
}

// ProviderSkipped records a provider passed over on an open circuit.
func (r *Recorder) ProviderSkipped(provider string) {
	if r == nil {
		return
	}
	r.providerSkipped.Inc(provider)
}

// SetCircuitState publishes a provider's breaker state as a gauge.
// state is a transport.CircuitState.String() value ("closed",
// "half_open", "open") — passed as a string for the same reason
// errorClass is: this package stays free of a transport import, and
// every label the recorder accepts is already enum-shaped text.
// Wired as transport.Breaker.OnTransition; also called once per
// provider at startup so a healthy pool has a series before its first
// incident (an absent series and a closed circuit look identical to an
// alerting rule otherwise).
func (r *Recorder) SetCircuitState(provider, state string) {
	if r == nil {
		return
	}
	var v float64
	switch state {
	case "half_open":
		v = 1
	case "open":
		v = 2
	}
	r.circuitState.Set(v, provider)
}

// Failover records a message delivered by a provider other than the
// route's first pick — the signal that a pool actually earned its keep.
func (r *Recorder) Failover(router string) {
	if r == nil {
		return
	}
	r.failovers.Inc(router)
}

// RouteExhausted records a message no provider in the route could take.
func (r *Recorder) RouteExhausted(router string) {
	if r == nil {
		return
	}
	r.routeExhausted.Inc(router)
}

// --- v2.2 quota + cost (FR103) ---

// QuotaExhausted records a provider passed over because a send budget
// is spent. window is "daily" or "monthly". Wired as
// transport.RouterHooks.OnQuotaBlocked.
//
// Deliberately a different series from ProviderSkipped: an operator
// triaging a gateway needs "my provider is broken" and "I am out of
// allowance" to be distinguishable at a glance, because the responses
// differ — one is an incident, the other is a purchasing decision.
func (r *Recorder) QuotaExhausted(provider, window string) {
	if r == nil {
		return
	}
	r.quotaExhausted.Inc(provider, window)
}

// SetQuotaRemaining publishes the sends left in one window. Only called
// for providers with a configured limit — an unlimited provider has no
// meaningful "remaining", and a fabricated large number would break any
// alert written against it.
func (r *Recorder) SetQuotaRemaining(provider, window string, remaining int64) {
	if r == nil {
		return
	}
	r.quotaRemaining.Set(float64(remaining), provider, window)
}

// SetQuotaLimit publishes a configured limit, so a dashboard can show
// used/limit without hard-coding the config.
func (r *Recorder) SetQuotaLimit(provider, window string, limit int64) {
	if r == nil {
		return
	}
	r.quotaLimit.Set(float64(limit), provider, window)
}

// RouteQuotaExhausted records a message no provider could take because
// every one of them was out of budget.
func (r *Recorder) RouteQuotaExhausted(router string) {
	if r == nil {
		return
	}
	r.routeQuotaOut.Inc(router)
}

// SetProviderCost publishes a provider's configured price per 1000
// messages in fixed-point millionths — the same integer the router
// compares, so a dashboard and the routing decision can never disagree
// through a rounding difference.
func (r *Recorder) SetProviderCost(provider string, micros int64) {
	if r == nil {
		return
	}
	r.providerCostGaug.Set(float64(micros), provider)
}

// --- v2.3 multi-tenant (FR116) ---

// ProjectSend records one project's send outcome. status is a closed
// enum; nothing derived from the request reaches this call.
func (r *Recorder) ProjectSend(name, status string) {
	if r == nil || name == "" {
		return
	}
	r.projectSend.Inc(name, status)
}

// ProjectSendDuration observes a project's end-to-end send latency.
func (r *Recorder) ProjectSendDuration(name string, d time.Duration) {
	if r == nil || name == "" {
		return
	}
	r.projectDuration.Observe(d.Seconds(), name)
}

// ProjectProvider records which provider delivered a project's message.
func (r *Recorder) ProjectProvider(name, provider string) {
	if r == nil || name == "" || provider == "" {
		return
	}
	r.projectProvider.Inc(name, provider)
}

// ProjectDelivery records a normalized delivery event for a project.
// event is one of the unified lifecycle types, never a provider's own
// dialect string.
func (r *Recorder) ProjectDelivery(name, event string) {
	if r == nil || name == "" {
		return
	}
	r.projectDelivery.Inc(name, event)
}

// --- v2.4 webhook + project quota (FR124, FR125) ---

// ProviderWebhook records one inbound provider webhook outcome. result
// must be one of the Webhook* constants: an error string as a label
// would be unbounded cardinality and could carry payload content.
func (r *Recorder) ProviderWebhook(provider, result string) {
	if r == nil || provider == "" {
		return
	}
	r.providerWebhook.Inc(provider, result)
}

// SetProjectQuotaLimit publishes a project's configured ceiling.
func (r *Recorder) SetProjectQuotaLimit(project, window string, limit int64) {
	if r == nil || project == "" {
		return
	}
	r.projectQuotaLimit.Set(float64(limit), project, window)
}

// SetProjectQuotaRemaining publishes what is left in the window. Only
// called for projects with a configured limit — an unlimited project
// has no meaningful remaining, and a fabricated number would break any
// alert written against it.
func (r *Recorder) SetProjectQuotaRemaining(project, window string, remaining int64) {
	if r == nil || project == "" {
		return
	}
	r.projectQuotaLeft.Set(float64(remaining), project, window)
}

// ProjectQuotaExhausted records a request refused on the project's own
// budget. Distinct from a provider quota skip: this one refuses the
// caller outright rather than moving to another provider.
func (r *Recorder) ProjectQuotaExhausted(project, window string) {
	if r == nil || project == "" {
		return
	}
	r.projectQuotaBlocked.Inc(project, window)
}
