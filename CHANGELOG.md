# Changelog

<!-- Modified for the cryotx/posthorn distribution. -->

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added — v1.0 completion round (ADR-47 … ADR-57)

The final feature round. After this the architecture is frozen: one binary, SQLite, single instance, single writer.

**Providers**

- **Native Cloudflare Email and Brevo adapters (FR135).** Both written against the vendors' current published references on 2026-09-08, not from memory. Brevo earns a native adapter with a reliable per-message id its webhooks carry back — the join the attempt trail is built on, which SMTP cannot offer. Cloudflare earns one with per-recipient delivery status returned **synchronously**: a permanent bounce is known at hand-off, hours before a callback would arrive, and Cloudflare sends none. It returns no message id and the adapter reports none rather than inventing one (ADR-48).
- **Brevo delivery-event adapter.** Implements the authentication Brevo actually publishes — source-IP allowlist plus the endpoint's mandatory basic auth — and **refuses** a `signing_secret`, because Brevo signs nothing and a verification an operator believes in but nobody performs is worse than none. No IP preset ships: Brevo's ranges live in a help-centre article Posthorn cannot re-verify at build time.
- **Provider capability registry (FR135, ADR-47).** Every provider declares send / webhook / webhook-verification / remote-quota separately, as exactly `yes`, `no` or `unsupported` — `no` is a Posthorn gap, `unsupported` is a provider limitation, and every `unsupported` carries its reason. The matrix is tested against both the transport registry and the adapter registry, so a row claiming a webhook with no adapter mounted fails the build.

**API and admin**

- **Project-scoped read API (FR136–FR140):** `GET /v1/messages`, `/v1/messages/{id}`, `/{id}/attempts`, `/{id}/events`, `/v1/usage`. Isolation is structural (ADR-50): every query takes a `Scope` with unexported fields, so reading across projects requires calling `AdminScope()` by name. The scope comes from the authenticated credential — a `project` query parameter is not read. A foreign message answers 404 identically to a missing one, so the endpoint is not an id oracle. Message bodies appear in no query in this surface.
- **Admin surface at `/admin` (FR143–FR147):** dashboard, providers, projects, messages and audit pages, plus the same data as JSON. Separate credential from `POSTHORN_ADMIN_TOKEN` — config cannot set it, and startup **refuses** when it collides with any project key or SMTP password. Session cookie is HttpOnly, SameSite=Strict, `__Host-` prefixed and Secure over TLS; login failures are throttled per source, and the correct token is refused while locked out.
- **Five audited operations:** disable/enable a provider, reset a breaker, suspend/restore a project, retry a dead letter, remove a suppression. Overrides can only SUBTRACT (ADR-51) — clearing one restores what the config says — and they persist across restarts.
- **API key rotation with no downtime (FR141, ADR-52).** A project may list several `api_keys`; all authenticate, so callers migrate during the overlap. Deliberately no endpoint that mints a key. Posthorn notices at startup that a key SET changed and writes the audit entry, comparing a digest of digests.

**Operations**

- **`/readyz` (FR149, ADR-55),** separate from `/healthz` and wired to different signals: storage writable, an ingress configured, workers started. A provider being down deliberately does **not** make Posthorn unready.
- **`posthorn backup` / `posthorn restore` (FR150, FR151, ADR-56).** `VACUUM INTO`, not a file copy — the naive backup loses whatever is in the `-wal`. Runs on a live database; the snapshot is verified immediately and needs no sidecars. Restore is offline, refuses a target in use, renames the old database aside and clears stale sidecars.
- **`posthorn doctor` (FR152).** Config, ingress, providers, capability coverage, which providers publish no webhook signature, projects, unfinished key rotations, SSRF policy and live webhook-target resolution, admin credential distinctness, storage writability proven by writing, schema version, queue and dead-letter depth. Optional `--provider` builds a transport from real credentials and sends nothing. It never prints a credential.
- **`posthorn probe` (FR154)** replaces config-validation as the container healthcheck, which passed happily while the listener was dead. Ships as a subcommand because the image is distroless.
- **`examples/production.toml`** with every secret as an env placeholder, and a documented metrics reference with the full label vocabulary.

### Security — v1.0

- **SSRF guard on every operator-supplied outbound URL (NFR45, ADR-49).** Endpoint and project `webhook_url`, the remote quota endpoint and the webhook transport are checked twice: at parse time, and at **connect time** on the address the resolver returned — which is the half that holds, because a public-looking hostname can resolve to loopback. Redirect hops are re-checked. Default deny, with `[security] allow_private_webhook_targets` as the opt-in for a receiver on the operator's own host. It never unblocks link-local, where every cloud metadata service lives.
- **Secrets are write-only everywhere (FR142, ADR-54).** No surface reads a credential to display it. The admin project view renders a fingerprint of the stored digest — Posthorn does not hold the plaintext to mask.
- **Recipients masked and bodies absent across the admin surface (ADR-54).** `u***@example.com`; message content appears nowhere. The operator running the gateway is not the owner of the mail passing through it.
- **Audit log in SQLite (FR147, ADR-53),** written before each action takes effect and aborting the action if it cannot be written. Exempt from retention pruning. No column for a secret, and the unsuppress entry records a masked address.
- **Metrics cardinality audit (NFR24, FR153).** Every exported Recorder method is called reflectively with a sentinel; the emitted label vocabulary must be exactly the sixteen documented names, and no caller value may reach a metric name.
- **Open-relay coverage (FR155)** as automated tests rather than an assurance. Two gaps closed: `smtp.New` would construct a listener with no sender allowlist, and `auth_required = "none"` with `allowed_senders = ["*"]` was accepted — each half legitimate alone, together an open relay.
- **Migration fails closed.** A database written by a newer Posthorn refuses to open. That test surfaced a real gap: `Open` proved a database READABLE, not writable, since an already-current database runs no migration. Open now performs a canary write and refuses to return a store it cannot write to.

### Changed — v1.0

- Storage schema `user_version` 5 → 6: `audit_log`, `admin_overrides`, `project_key_state`.
- SQLite `temp_store = MEMORY`, so a large query does not depend on a writable `/tmp` under the read-only production root.
- Compose deployment finalised: bounded storage, tmpfs for scratch, memory and pid limits, bounded log rotation, `POSTHORN_ADMIN_TOKEN` passthrough, and a healthcheck that probes the running process.
- **Provider config reloads by restart (ADR-57).** Hot reload evaluated and declined: its failure modes are all silent (a send routed to a replaced transport, a breaker whose history is discarded, a quota counter reset by an edit), and the alternative costs one sub-second restart with a durable queue behind it.


### Fixed — RC1 production hardening (ADR-44/45/46)

An audit round ahead of the first release candidate. No new capability; correctness gaps found by inspection and fault injection, plus semantics that needed stating rather than implying.

- **Delivery state is now projected per recipient (FR128, ADR-44).** A submission may carry several recipients, and providers disagree about what an event covers — Postmark's are recipient-scoped (one event per recipient, confirmed against its docs), Resend's are message-scoped (one event per `email_id` carrying the whole `to` array). Writing either into one submission-level column meant **one recipient's bounce silently overwrote another's delivery**. State now lives in `recipient_delivery`, with the submission carrying an aggregate: `pending` / `delivered` / `partial` / `failed`. Suppression is scoped the same way — a recipient-scoped bounce no longer suppresses every recipient of a multi-recipient message.
- **The state machine is explicit transition rules, not a linear rank (FR130, ADR-44).** `delivery_state_rank` assumed the lifecycle is totally ordered. It is not: a `deferred` webhook delayed past a `delivered` one carried a later timestamp and would have **un-delivered the message**. Now a terminal outcome lands however late it arrives, a transient never overrides a terminal, and terminal outcomes escalate rather than de-escalate — a delivery event arriving after a complaint does not undo it. The rules are exported as `storage.Supersedes` so each disorder case is asserted directly.
- **Postmark replay identity uses its own trace id (FR129).** `X-PM-Webhook-Trace-Id` is stable across retries of one event, so it is now the dedup key when present, with the derived content hash as fallback. The header is a dedup input only, never an authenticity claim, and is read only after authentication.
- **A crash no longer stalls the caller for two minutes (FR132, ADR-45).** A process killed mid-send left a `pending` idempotency reservation, and the caller's retry got `409 in_flight` for the full pending window while the retry queue was about to deliver. Recovery now settles those reservations with the `202 queued` answer the dead request would have returned. The message id is stored at reserve time to make that join possible.
- **A crash no longer refunds a project's spent budget (FR133).** Usage counters for projects **with a limit** are written through at the send rather than buffered — a buffered counter handed the tenant extra allowance on restart.
- **The storage health metric no longer lags `/healthz` (FR134).** A write failure degraded `/healthz` instantly while `posthorn_storage_healthy` stayed at 1 until the next canary probe — and the metric is the one operators alert on.
- **Malformed-but-signed webhook payloads are counted correctly.** A `Normalize` failure now records `invalid_payload` rather than going uncounted, and a payload with no provider message id is a payload defect rather than an unknown message.

### Added — RC1

- **Delivery semantics are documented as AT-LEAST-ONCE (NFR51, ADR-46).** Reproduced directly: a provider accepted a message while Posthorn was killed, recovery queued it, and the retry sent it again. `Idempotency-Key` deduplicates the *client*; it cannot see what a provider did with a request that timed out. The docs now carry the caller-side convention that neutralises it — keep the same business token across retries of one logical message, so a duplicate is two copies of one working code rather than two competing codes.
- **Migration warning for legacy endpoint `api_keys` (FR131).** With `[[projects]]` configured, endpoints that still carry their own keys are named at startup with the consequence stated (those keys bypass the project sender policy). No key is printed. The compatibility itself is retained.
- **New docs page: [Production operations](https://posthorn.dev/deployment/operations/)** — delivery semantics, SQLite durability pragmas and their rationale, the online backup/restore procedure, crash behaviour table, and the measured capacity envelope.
- Storage schema `user_version` 4 → 5: `recipient_delivery`. `submissions.delivery_state` changes meaning to the aggregate and is cleared on migration; the events it derived from are untouched and the projection rebuilds from the next event.

### Security — provider webhook verification (v2.4, ADR-40)

`ProviderWebhookAdapter.Verify` no longer returns nil unconditionally. Each adapter now implements the scheme its provider actually publishes, confirmed against the vendors' live documentation on 2026-09-08 rather than written from memory.

- **Resend (FR120a).** Full Standard Webhooks / Svix verification: `svix-id` / `svix-timestamp` / `svix-signature` headers (`webhook-*` aliases accepted), signed content `{id}.{timestamp}.{raw body}`, HMAC-SHA256 under the base64-decoded secret with its `whsec_` prefix stripped, base64 signatures in a space-delimited `v1,<sig>` list, unix-second timestamps rejected beyond 5 minutes, constant-time comparison. Verification uses the **raw request bytes** — never a decode/re-encode round trip, which would reorder keys and break every signature.
- **Postmark (FR120b).** Postmark does not sign webhook payloads; its documentation says so and recommends basic auth plus IP allowlisting. So the adapter implements exactly that: the endpoint's mandatory basic auth, an optional source-IP allowlist against Postmark's published egress (offered as `PostmarkWebhookIPs`), and payload-shape validation — with `X-Forwarded-For` honoured only from configured `trusted_proxies`. A `signing_secret` configured for Postmark is now a **parse error**, because accepting it would leave an operator believing payloads were verified when they cannot be. The residual risk is documented rather than implied: anyone holding the basic-auth credential can forge Postmark events.
- **Nothing runs on an unverified request (FR121).** No normalize, no delivery event, no message-state change, no project webhook — and the caller gets a `401` instead of the previous silent `200`, so a misconfigured secret is visible in the provider's dashboard rather than hidden forever. Diagnostics carry the provider, a closed-enum reason and a request id; never the secret, the signature, the Authorization header or payload content (NFR48).
- **A dialect is only mounted when configured.** `[lifecycle.providers.<name>]` both supplies the credentials and enables the route — an unconfigured provider has no open door.
- **Replay collapse (FR122).** A redelivered provider event produces no second delivery event and no second outbound project webhook. Resend supplies the key directly (`svix-id`, stable across retries per the spec); Postmark, which mints none, gets one derived from the event's identifying content. Enforced by a partial UNIQUE index in SQLite — no separate cache system.
- New metric `posthorn_provider_webhook_total{provider,result}` with a closed result enum (`accepted`, `invalid_signature`, `duplicate`, `invalid_payload`, `unknown_message`, `internal_error`).

### Added — project quota enforcement (v2.4, ADR-41/42)

`[[projects]].daily_limit` and `monthly_limit` were recorded but inert. They are now enforced.

- **Refuses, does not reroute (FR125).** A project out of budget gets `429` with a stable `project_quota_exhausted` code naming the exhausted window; SMTP gets `452 4.7.1`, a *temporary* failure because the allowance returns on a calendar boundary. No other provider is tried, no circuit breaker is touched, nothing is queued — this is deliberately the opposite of a provider quota running out, and a caller can always tell which limit it hit. Queueing would be actively wrong: holding a verification code until midnight is worse than answering now.
- **Strict under concurrency (FR126).** With a limit of 100 and 99 spent, a hundred simultaneous requests admit exactly one — the allowance is reserved under the same lock that reads it, mirroring ADR-36. A failed send releases its reservation, so a broken provider never eats a tenant's budget. Strictness is per process; cross-instance remains soft under NFR26 and is documented, not implied (NFR50).
- **A replay costs nothing (NFR49, ADR-42).** The order is authenticate → sender policy → idempotency reserve/replay → project quota reserve → send → commit/release, and it is load-bearing: a retry of an already-sent request replays the recorded response without reaching the quota, so a retry storm cannot drain a tenant's daily budget without sending anything. Dry runs and fully-suppressed sends cost nothing either.
- Counters persist and roll over per day and month with no scheduled job, and are restored at startup so a restart does not hand out a fresh allowance.
- New metrics `posthorn_project_quota_limit{project,window}`, `posthorn_project_quota_remaining{project,window}`, `posthorn_project_quota_exhausted_total{project,window}`. Projects with no limit are absent from the gauges rather than showing a fabricated ceiling.

### Added — message delivery state (v2.4, ADR-43)

`submissions.delivery_state` projects the latest delivery outcome from the event log, so "what happened to this message" is one indexed read. Maintained by **event time**, so a late stale callback cannot move it backwards; at equal timestamps a terminal negative outranks a delivery. Events remain the record of fact — nothing is removed or rewritten to maintain the projection.

### Changed

- Storage schema `user_version` 3 → 4: `delivery_events.provider_event_key` with a partial UNIQUE index, and `submissions.delivery_state` / `delivery_state_at` / `delivery_state_rank`. Existing databases migrate in place.
- `Store.Prune` is now the whole retention pass (submissions, idempotency, attempts, events, usage) rather than half of it.
- `lifecycle.RegisterAdapter` takes a builder rather than a stateless instance, since adapters now carry their authentication configuration.

### Added — multi-project gateway (v2.3 work, ADR-33…39)

One Posthorn now serves several business projects. Entirely opt-in: with no `[[projects]]` blocks every path, response body, status code and metric label is what v2.2 produced (NFR42).

- **Projects (FR107).** Top-level `[[projects]]` with `name`, `enabled`, `api_key`, `default_from` and a required non-empty `allowed_from_domains`. API keys are hashed at startup and the plaintext is never retained — not in logs, metrics, errors, or the database.
- **Project API-key authentication (FR108).** `Authorization: Bearer <project-key>` on api-mode endpoints establishes the tenant identity that logging, metrics, routing, idempotency and events all key off. A non-project token falls through to the endpoint's own `api_keys` so projects can be adopted one endpoint at a time; a valid key for a **disabled** project is refused outright. `api_keys` becomes optional once projects exist.
- **Sender policy (FR109, P0).** A project cannot send as another project's domain. A request `from` must satisfy `allowed_from_domains`; absent, `default_from`; absent that, the endpoint's `from` — which is checked too. `default_from` is validated at parse time. Rejection is `403 sender_not_allowed`. A `project` field in a request body is ignored: identity comes from the credential only.
- **Project-scoped idempotency (FR110, FR113).** `Idempotency-Key` scoped to `(project, key)` and reserved **atomically in SQLite** before any send. An identical retry replays; the same key with a different payload returns `409 idempotency_key_reused`; a concurrent duplicate returns `409 idempotency_in_flight`. 100 simultaneous requests with one key produce exactly one email. Fingerprints are over the canonical payload, so re-serialised JSON replays rather than falsely conflicting. Failed sends release the key; only 2xx/3xx outcomes are recorded.
- **SMTP project identity (FR111).** `smtp_username` / `smtp_password` per project resolve through the same registry, so an SMTP submission carries the same project context an HTTP request does — including the sender policy, applied after MIME parsing. The listener's own `smtp_users` are unchanged.
- **Message identity and attempt trail (FR112).** One Posthorn message id per logical message, stable across provider failover, surfaced as `message_id` on project responses (the same value as `submission_id` — one message, one id). Every provider attempt is persisted: attempt number, provider, timings, result, error class, and that provider's own message id. No bodies, recipients or subjects.
- **Unified delivery events (FR114, FR117).** A provider-agnostic event shape (`event_id`, `message_id`, `project`, `provider`, `provider_message_id`, `type`, `timestamp`, `metadata`) with a closed type set — `message.accepted` / `sent` / `delivered` / `deferred` / `bounced` / `complained` / `failed` / `other`. New `ProviderWebhookAdapter` interface (`Verify` / `Parse` / `Normalize`) with a registry; Postmark and Resend ship, mounted at `/events/<provider>/unified`. Events are stored keyed by `event_id`, so a provider webhook retry cannot double-count.
- **Project webhooks (FR118).** Per-project `webhook_url` / `webhook_secret`. HMAC-SHA256 signed, with `event_id` and `timestamp` **inside the signed body** (and mirrored as `X-Posthorn-Event-Id` / `X-Posthorn-Timestamp`) so a receiver can deduplicate and reject replays. Delivery reuses the existing forward queue's retry, backoff and dead-letter rather than a second task system; a redelivery carries the same event id.
- **Project usage and metrics (FR115, FR116).** Durable per-project success/failure counters per day and month, period-keyed so they roll over with no scheduled job. New metrics `posthorn_project_send_total{project,status}`, `posthorn_project_delivery_total{project,event}`, `posthorn_project_provider_total{project,provider}`, `posthorn_project_send_duration_seconds{project}`.
- **Storage schema v3.** Project ownership and message kind on `submissions`; rebuilt `idempotency` (scope + fingerprint + state + message id); new `message_attempts`, `delivery_events` and `project_usage`. Existing databases migrate in place.
- New docs page [Projects](https://posthorn.dev/features/projects/) and `examples/projects.toml`.

### Changed — quota concurrency and remote quota

- **Quota is now strict within a process (NFR43, ADR-36).** The v2.2 audit found the `Check`-then-`Record` shape was **soft**: with `daily_limit = 100` and 99 spent, all 100 concurrent senders passed the check and all 100 sent. The new optional `Reserver` interface takes the allowance under the same lock that reads it — the same scenario now admits exactly one. Failed sends hand the reservation back, so "only successful sends count" still holds on disk. Cross-instance strictness remains out of scope under NFR26 and is documented rather than implied.
- **`RemoteQuota` (FR119, ADR-37).** The v2.2 `QuotaSource` interface was audited against a remote implementation and needed **no change**. Remote readings are advisory reconciliation: they may raise local usage, never lower it, and never persist. A generic `HTTPUsageFetcher` ships as the reference; no vendor adapter does, deliberately — each needs its live API read rather than being written from memory.
- **ADR-11 revised for project mode only (ADR-33).** Per-request `from` is now allowed under project authentication, bounded by the project's own domain allowlist. Plain api-key endpoints are unchanged: per-request `from` stays forbidden there.

### Added — quota-aware + cost-aware routing (v2.2 work, ADR-29/30/31/32)

The gateway learns what each provider costs and how much of it is left. Additive on top of the multi-provider work below: a config with no quota, cost, `mail_kind` or group fields keeps byte-identical behavior, labels and status codes (NFR36).

- **Provider send budgets (FR99).** Optional `[providers.quota]` with `daily_limit` / `monthly_limit`; zero or absent means unlimited. Only **successful** sends count — a provider that failed did not spend your allowance. An exhausted provider stops being a routing candidate; when every provider in a route is out of budget the send fails with its own `quota_exhausted` class rather than a generic transport error.
- **Usage survives restarts and rolls over without a cron (FR100).** Counters are written through to the existing SQLite storage (schema v2, `provider_usage`) and restored at startup. Rollover is a property of the key, not of a timer: rows are keyed by period (`2026-09-07`, `2026-09`), so a new day is a new row and the old count is never read again — a Posthorn that was stopped or crashed across midnight computes the right window on its next send. Without `[storage]`, counters are in-memory only and a restart resets them (documented, not silent).
- **Fixed-point costs (FR101).** `cost_per_1000` accepts an exact decimal string (`"0.10"`, recommended), an integer, or a float quantised deterministically at parse time. Money is compared as `int64` millionths and never as floating point, so a routing decision cannot change between runs on a rounding difference.
- **Two cost-aware strategies (FR102).** `free_first` (every zero-cost provider before any paid one) and `cheapest` (ascending price, ties on declared order). Both are precomputed and stable; both still fail over on health-class errors. Neither needs quota awareness — an exhausted provider is skipped at send time, so `free_first` walks onto the paid tier by itself.
- **Mail kinds (FR104).** Messages carry an optional label from a closed set — `verification`, `password_reset`, `notification`, `important`, `marketing` — set per endpoint (`mail_kind`) or per SMTP listener, overridable per request in api-mode against the same set (unknown values are a 422). Routing rules match on it with `match_kind`, and dry-run reports it. A routing input only: transports ignore it and it never reaches a header.
- **Provider groups (FR105).** Optional `[[provider_groups]]` name a provider list plus a default strategy once; transport blocks and rules reference it with `group = "name"`. A config-level alias that resolves to the same route an inline list produces — the router is unchanged. Groups share one name space with providers.
- **Quota observability (FR103).** `posthorn_provider_quota_exhausted_total{provider,window}`, `posthorn_provider_quota_remaining{provider,window}`, `posthorn_provider_quota_limit{provider,window}`, `posthorn_route_quota_exhausted_total{router}`, `posthorn_provider_cost_per_1000_micros{provider}`. Quota skips are a **separate series** from circuit skips. Unlimited providers are absent from the quota gauges rather than showing a fabricated ceiling.
- **`quota_source` (FR106).** Per-provider; only `"local"` is implemented. `"remote"` is reserved and **rejected at parse time** rather than silently ignored — the interface is designed so a provider-usage-API implementation can be added without touching the router.
- New docs page [Quota & cost routing](https://posthorn.dev/features/quota-and-cost/) and `examples/quota-cost-routing.toml`.

### Changed — quota-aware routing

- **Quota exhaustion is not a health failure (NFR37).** It is checked *before* the circuit breaker and never touches it, so a provider whose free tier runs out every afternoon keeps a closed circuit all month.
- **`transport.IsDeferrable` joins `IsRetryable`.** A quota-blocked message must not be retried inline (no backoff outlives a daily budget) but must not be dead-lettered either (it will send after midnight). The FR78 queue and the retry worker's dead-letter check now ask `IsDeferrable`; the inline FR19-22 policy still asks `IsRetryable`. Behavior for every pre-existing error class is unchanged.
- **New HTTP status: 503** when a quota-exhausted message cannot be queued because no `[storage]` is configured. Terminal delivery failures keep their 502. Only the new quota path produces 503, so no pre-quota deployment sees a changed status code.
- `Message.Kind`, `SendResult`/`TransportError` unchanged otherwise; `ErrorClass` gained `ErrQuotaExhausted`.
- Storage schema `user_version` 1 → 2. Existing databases migrate in place on open.

### Added — multi-provider gateway (v2.1 work, ADR-26/27/28)

Posthorn goes from *one endpoint, one provider* to *one endpoint, one route over a pool of providers*. All of it is additive: a config that keeps the inline `[endpoints.transport]` shape gets byte-identical behavior, metric labels and log fields.

- **Named provider pool (FR93).** Top-level `[[providers]]` blocks declare each upstream once — `name`, `type`, `settings`, optional `weight` — and endpoints reference them by name instead of repeating credentials in every block. Pool entries are built once per process and shared, so one upstream has one client and one health state however many endpoints send through it. A pool entry nobody references is a parse error (it is almost always a half-applied rename).
- **Routing strategies (FR94).** `[endpoints.transport]` and `[smtp_listener.transport]` accept `providers = [...]` plus `strategy`: `failover` (default, ordered), `round_robin`, or `weighted` — the last using smooth weighted round-robin, so a 3:1 split is exactly 30/10 over 40 messages rather than 30/10 on average. Failover order after the first pick is always the declared order. The inline shape and the pooled shape are mutually exclusive at parse time.
- **Routing rules (FR95).** Ordered `[[endpoints.transport.rules]]` route by `match_recipient_domain` (exact or `*.suffix`) and/or `match_field` (submitted fields against operator-listed values); first match wins, everything else takes the default `providers` list. Domain rules require *every* recipient to match, so a submission is never split across providers. Submitter data selects among routes you wrote; it can never name a provider.
- **Per-provider circuit breakers (FR96).** `[providers.circuit_breaker]` with `failure_threshold` / `cooldown` / `half_open_probes` (defaults 5 / 30s / 1, `disabled` to opt out). Only transient and rate-limited failures count — a terminal 4xx describes the message, not the provider, and if it counted, one bad `From` domain would drain the whole pool one submission at a time. When every provider in a route is unavailable the send fails **transiently**, so the retry queue holds the message instead of dead-lettering it.
- **Routing observability (FR97).** New metrics `posthorn_provider_attempts_total{provider,outcome}`, `posthorn_provider_latency_seconds{provider}`, `posthorn_provider_skipped_total{provider}`, `posthorn_provider_circuit_state{provider}` (published for every provider at startup), `posthorn_route_failover_total{router}`, `posthorn_route_exhausted_total{router}`. `posthorn_submissions_sent_total` is now labelled with the provider that actually delivered; `submission_sent` and `smtp_submission_sent` carry a `provider` field; the submission log records the delivering provider; `/healthz` reports each provider's circuit state (still 200 — restarting does not fix an upstream outage).
- **Dry-run route explanation (FR98).** `dry_run = true` on a pooled endpoint returns a `route` block naming the matched rule index, the strategy and the route's providers, without sending and without advancing any rotation.
- **Deployment + example.** `examples/multi-provider.toml` (annotated pool, weighted split, rules, SMTP listener, storage) and `deploy/compose/` (loopback-bound Docker Compose stack; bring your own reverse proxy). New docs pages: [Provider pool](https://posthorn.dev/configuration/providers/) and [Routing & failover](https://posthorn.dev/features/routing/).

### Changed — multi-provider gateway

- **ADR-13 revised.** "One SMTP listener has one outbound transport" now reads "hands each message to one `Transport`" — which may be a pool. Per-*tenant* SMTP routing (different senders resolving to different identities, DKIM keys and suppression scopes) is unchanged and still out of scope.
- `transport.SendResult` and `transport.TransportError` gained a `Provider` field, set by the router and empty for inline transports.
- A terminal transport error now names the failing provider in its message: `transport: ... (class=terminal provider=postmark-prod status=422)`.

---

The v2.0 work — the stateful shift, recut 2026-08-02 against the integration-seam USP (unsubscribe injection and fan-out moved out behind demand gates; see the roadmap). Everything below is opt-in: a config without the new blocks behaves identically to v1.2.

### Added

- **HTML email bodies ([#5](https://github.com/craigmccaskill/posthorn/issues/5), ADR-19).** Per-endpoint `body_format = "html"`: the body template renders through `html/template` with contextual auto-escaping, so submitter input is inert markup by construction. Plain-text part auto-derived from the rendered HTML (or supply `text_body`); multipart delivery on all five transports; existing file-based HTML templates work directly via `body = "templates/x.html"`. The SMTP listener now accepts HTML mail — `text/html` maps through, and HTML-only messages (previously rejected `554`) are accepted with a derived text part.
- **Storage spine ([#21](https://github.com/craigmccaskill/posthorn/issues/21), [#22](https://github.com/craigmccaskill/posthorn/issues/22), ADR-20/21).** Optional `[storage]` block: SQLite (pure-Go driver; static builds unchanged) submission log, sync-first retry queue — 200 still means sent; api-mode callers get `202 {"status":"queued"}` where v1.x dropped mail after inline retries, and queued sends survive restarts with exponential backoff. Durable idempotency (byte-identical replays across restarts). Retention pruning, a hard `max_size` cap so Posthorn can never fill its own disk, and a storage canary in `/healthz` + `posthorn_storage_healthy`; storage failure degrades to v1.x behavior — mail never blocks on the disk.
- **Lifecycle events + suppression ([#8](https://github.com/craigmccaskill/posthorn/issues/8), [#4](https://github.com/craigmccaskill/posthorn/issues/4), ADR-22/23).** `[lifecycle]` enables a basic-auth-protected Postmark event sink; events normalize to a provider-agnostic shape and forward, HMAC-signed, to per-endpoint `webhook_url`s with retry. Hard bounces and spam complaints auto-suppress globally; suppressed sends answer `200 {"status":"suppressed"}` / SMTP `250` (terminally handled — never retried). Managed via the new `posthorn suppressions list|add|remove` CLI; no HTTP admin API.
- **Webhook transport ([#23](https://github.com/craigmccaskill/posthorn/issues/23), ADR-24).** `type = "webhook"` delivers the structured submission (id, envelope, bodies, raw fields) as a signed JSON POST — same retry classification and queueing as mail.
- **File attachments ([#24](https://github.com/craigmccaskill/posthorn/issues/24), ADR-25).** Opt-in `[endpoints.attachments]` with a required `allowed_types` allowlist enforced on the sniffed bytes, never the declared type. Form-mode multipart files and api-mode base64 `attachments`; carried by all five mail transports.
- New metrics: queue counters/gauges, lifecycle event/forward counters, `posthorn_suppressed_recipients_total`.

### Fixed

- Multipart form **fields** were silently dropped in v1.x — `multipart/form-data` submissions never delivered their values to validation, spam checks, or templates. Form parsing is now content-type-aware.

### Security

- Doctrine, spec-level (NFR25): security-relevant features are opt-in, never default-on. The lifecycle event endpoint cannot run unauthenticated; attachments require an explicit type allowlist; storage requires an explicit block.

## [1.2.0] — 2026-07-04

Spam-protection minor. Adds three content-agnostic checks that target automated contact-form spam — the kind that fills valid-looking fields, varies its wording and language, and reuses throwaway sender addresses, so there's no content signature to match. All are opt-in, form-mode only, and backwards-compatible; existing configs are unaffected.

### Added

- **Reputation check ([#44](https://github.com/craigmccaskill/posthorn/issues/44)).** Optional `[endpoints.reputation]` — a StopForumSpam lookup on the submitter email and/or IP before send, blocking known form-spam identities regardless of message content or language. Bounded LRU result cache; fails open by default so a provider outage never blocks real mail (fail-open events increment `posthorn_check_failed_open_total{check="reputation"}`). Sends email+IP to a third party — opt-in.
- **Proof-of-browser check ([#45](https://github.com/craigmccaskill/posthorn/issues/45), ADR-18).** Optional `[endpoints.proof_of_browser]` — Posthorn serves a challenge token from a `GET` on the endpoint that only page JavaScript can fetch and inject as `_pob_token`, blocking bots that POST the form directly. An optional `min_age` time-trap rejects impossibly-fast submissions. No third party; works on static sites.
- **Captcha check ([#33](https://github.com/craigmccaskill/posthorn/issues/33)).** Optional `[endpoints.captcha]` — Cloudflare Turnstile verification, the escalation tier for bots that render a real browser. Byte-identical silent 200 on a rejected token; fails closed by default on a provider outage.
- New `posthorn_check_failed_open_total{endpoint,check}` metric and `spam_blocked` kinds `reputation` / `proof_of_browser` / `captcha`.

### Changed

- The three spam checks (origin, honeypot, CSRF) were refactored into a two-phase pipeline ([#60](https://github.com/craigmccaskill/posthorn/issues/60)): header checks pre-parse, form checks post-parse, each producing a `spam.Verdict` the handler renders through one mapping. No behavior change — the extraction is what let the three new checks land as small additions rather than more inline blocks.

### Fixed

- The SMTP ingress was constructed with a nil metrics recorder, so `posthorn_submissions_sent_total{endpoint="smtp_listener"}` and its failure counter never emitted. The registry and recorder are now shared with the HTTP mux, and the real send latency is recorded (was hardcoded 0). ([#57](https://github.com/craigmccaskill/posthorn/issues/57))
- `posthorn validate` now builds the SMTP ingress, so a listener config with a semantic error (e.g. `require_tls` with no `tls_cert`) is caught at validate time instead of failing at boot. ([#58](https://github.com/craigmccaskill/posthorn/issues/58))

### Recipes

- Added Authentik and Mastodon SMTP-listener recipes to the docs site.

## [1.1.0] — 2026-07-04

A security-hardening and operational-maturity release, from a full audit of the shipped v1.0. One breaking change (below) that closes an open-relay footgun; everything else is additive or a fix.

### Breaking

- **`auth_required = "none"` on a public bind is now refused at config-parse time ([#41](https://github.com/craigmccaskill/posthorn/issues/41)).** An SMTP listener with `auth_required = "none"` whose `listen` address is not verifiably loopback/private (e.g. `:2525`, `0.0.0.0:2525`, a public IP, or an unresolvable hostname) is a startup error. Set `trusted_network = true` to assert the listener is reachable only from a trusted network — a Docker bridge with no `ports:` exposure, a VPN, or a firewall. **Migration:** deployments built from the pre-existing Ghost / Gitea / internal-relay recipes used `auth_required = "none"` with `listen = ":2525"` and must add `trusted_network = true` (the updated recipes include it). This closes the open-relay footgun where the sender allowlist was the only gate on an unauthenticated public listener.

### Added

- **SMTP listener brute-force and resource hardening ([#50](https://github.com/craigmccaskill/posthorn/issues/50)).** Per-remote-IP AUTH-failure budget (10 failures/minute; further attempts get `421` and the connection closes; successful auths never consume budget). Global (`max_connections`, default 100) and per-IP (`max_connections_per_ip`, default 16) concurrent-connection caps; over-cap connections get a `421` before any protocol work.
- CSRF rejections now increment `posthorn_spam_blocked_total{kind="csrf"}` — previously they were log-only and invisible on `/metrics`.
- `spam_blocked` (honeypot and origin) and `csrf_rejected` log lines now carry `client_ip` for operator forensics, matching `rate_limited`; omitted when `strip_client_ip = true` (FR59).

### Fixed

- An `[smtp_listener]`-only config (no `[[endpoints]]`) is now accepted — the config validator required at least one HTTP endpoint, breaking the documented Ghost and Gitea recipes and every SMTP-listener-only deployment. `posthorn validate` output now names the listener so a listener-only config doesn't report a bare "0 endpoint(s)". ([#37](https://github.com/craigmccaskill/posthorn/issues/37))
- SMTP-listener sends now honor the FR19-22 retry policy — a transient provider error is retried once (as the HTTP ingress already did) instead of returning an immediate `451`. Previously a provider blip could drop a transactional email on the SMTP path that the HTTP path would have retried. The retry policy is now shared between both ingresses. ([#59](https://github.com/craigmccaskill/posthorn/issues/59))
- The HTTP server now sets `ReadTimeout` (15s) and `WriteTimeout` (30s, chosen to exceed `ReadTimeout` + the handler's 10s request bound so a legitimate slow upload followed by a provider retry isn't truncated after the mail was sent) plus an explicit 64 KB `MaxHeaderBytes`, so slow-body senders and slow-reading clients can't hold connections indefinitely. ([#42](https://github.com/craigmccaskill/posthorn/issues/42))
- Raw stdlib parse errors are no longer echoed to clients on malformed bodies; both parse paths return a generic 400 and log the detail server-side as `body_parse_failed`. Deliberate API-shape messages (e.g. "nested objects are not supported") remain client-visible. ([#42](https://github.com/craigmccaskill/posthorn/issues/42))

### Security & supply chain

- CI now runs a `gofmt` gate, `govulncheck`, and `golangci-lint` on every push and pull request. ([#38](https://github.com/craigmccaskill/posthorn/issues/38))
- Docker base images are pinned by digest, and Dependabot keeps the base images, Go modules, and GitHub Actions current. ([#39](https://github.com/craigmccaskill/posthorn/issues/39))
- Release images now ship SLSA provenance and SBOM attestations, are signed with cosign (keyless via GitHub OIDC), and are gated on a Trivy vulnerability scan. ([#56](https://github.com/craigmccaskill/posthorn/issues/56))

### Development

- New hermetic provider-testing harness (`core/providertest`): ingress→egress end-to-end tests for all five transports and the SMTP listener, run on every PR with no credentials. A `make test-live` runner and a scheduled, protected workflow validate against real provider APIs (against non-delivering targets; SES via OIDC, no stored key). See [#76](https://github.com/craigmccaskill/posthorn/issues/76). Posthorn deliberately does not test deliverability/DKIM — the provider owns signing and reputation.

## [1.0.0] — 2026-05-26

Initial public release. The v1.0 spec is in [`spec/`](./spec/). Four feature blocks ship in a single release: form-mode HTTP ingress, API-mode HTTP ingress, multi-transport + operational maturity, and an SMTP listener. Originally sequenced as v1.0 → v1.1 → v1.2 → v1.3 themed releases; consolidated into v1.0 before tag.

### Block A — HTTP form ingress

- HTTP form ingress with multiple independent endpoints per config (FR1, FR2)
- Postmark HTTP API transport with bespoke ~80-line client (FR3, FR4, ADR-1)
- Honeypot field, Origin/Referer fail-closed check, max body size (1 MB safe default; configurable), token-bucket rate limit with LRU eviction at 10K IPs (FR5–FR9, NFR4, NFR6)
- Required-field and email-format validation returning structured 422 (FR10, FR11)
- Go `text/template` rendering for subject and body with custom-fields passthrough block (FR12, FR13)
- JSON responses, content negotiation, `redirect_success` / `redirect_error` (FR14–FR16)
- Per-request UUIDv4 `submission_id` in the 200 JSON body on both real-success and silent-honeypot paths — byte-identical body shape so a bot inspecting the response cannot distinguish honeypot rejection from success (FR5, NFR5)
- Retry policy: one retry on transient/5xx (1s), one retry on 429 (5s), no retry on 4xx config errors, 10s hard request timeout (FR19–FR22)
- Structured JSON logging with UUIDv4 submission IDs propagated through every log line; `submission_sent` carries `transport_message_id` so operators jump straight from Posthorn logs to the provider's UI (FR17, FR18, NFR7, NFR8)
- Standalone binary `cmd/posthorn` with `serve` and `validate` subcommands, SIGTERM/SIGINT graceful shutdown (FR24–FR26)
- Multi-stage Dockerfile producing a distroless static image for `linux/amd64` and `linux/arm64`, published to `ghcr.io/craigmccaskill/posthorn` on tag push (NFR12, NFR13)
- GitHub Actions CI: `go vet` and `go test -race -count=1`
- Public documentation site at [posthorn.dev](https://posthorn.dev) (Astro + Starlight)
- Strict TOML field validation — unknown / misspelled config keys (e.g., `starttls` when the real field is `require_tls`) fail at parse time with a clear error naming the offending key, rather than being silently ignored and producing wrong runtime behavior.

### Block B — API mode

- API-mode endpoints via per-endpoint `auth = "api-key"` config; default `auth = "form"` preserves form-mode behavior (FR31, FR45)
- API-key authentication via `Authorization: Bearer <key>` with constant-time comparison (`crypto/subtle.ConstantTimeCompare`); multiple keys per endpoint for rotation (FR33, FR34, NFR19)
- Per-API-key rate limiting on API-mode endpoints — workers sharing egress IPs (Cloudflare Workers, etc.) get independent buckets (FR35)
- JSON content type on API-mode endpoints (`application/json`); flat-object body shape with primitive type coercion to template variables; nested objects rejected with 400 (FR36, FR37, FR38, FR39)
- Idempotency keys via standard `Idempotency-Key` header; in-memory per-endpoint LRU cache, 24-hour TTL, byte-identical response replay; HTTP 409 on concurrent in-flight collisions (FR40–FR44, NFR20)
- New `core/idempotency/` package; new `idempotency_cache_size` endpoint config (default 10000)
- Per-request `to_override` in api-mode JSON bodies — string or array of email addresses replaces the endpoint's `to` list for the request. Each address validated as syntactic email; empty array or any invalid address returns 422. `from` is intentionally not overridable to prevent spoofing via leaked keys (FR46, ADR-11)
- API keys configured via `${env.VAR}` are subject to NFR3's "never appear in log output" invariant; tests verify with sentinel-key assertions (NFR21)
- Per-IP brute-force defense for failed auth: 10 failed attempts from one IP within ~1 minute trip a bucket and subsequent failures return 429 (instead of 401) until the budget refills. Successful auths never consume the budget, so legitimate callers are unaffected. New `auth_rate_limited` log event records the lockout for forensic follow-up.

### Block C — Multi-transport + operational maturity

- **Resend** HTTP API transport — bespoke client (~150 LOC); Bearer auth; JSON body; ErrorClass mapping (FR47)
- **Mailgun** HTTP API transport — bespoke client (~180 LOC); HTTP Basic auth; multipart/form-data body via `mime/multipart.Writer`; US + EU regions (FR48)
- **AWS SES** transport — bespoke SigV4 implementation (~230 LOC) shared in `core/transport/awssigv4.go` for any future AWS-signed transport (S3, SNS) + SESv2 `SendEmail` request shape (~265 LOC) (FR49, ADR-14)
- **Outbound SMTP** transport — stdlib `net/smtp.PlainAuth` + STARTTLS; per-Send connect/AUTH/MAIL/RCPT/DATA/QUIT cycle; 30s per-Send timeout (FR50, ADR-17)
- Transport registry pattern — new file in `core/transport/` calls `Register()` in `init()`; config + cmd/posthorn dispatch through `Lookup()`. Adding a sixth transport requires zero edits to config or main (FR4, FR51, FR52, FR53)
- `/healthz` endpoint returning `200 OK` with body `ok`; auth-free, fixed path (FR54)
- `/metrics` endpoint with hand-rolled Prometheus text exposition (FR55, ADR-15) — counters for submissions received / sent / failed (with `error_class`), rate-limit hits, auth failures, spam blocks, idempotent replays, validation failures; latency histogram with operator-meaningful buckets. Submitter content never enters the label space (NFR24)
- New `core/metrics/` package: `Registry`, `Counter`, `Histogram`, exposition writer; nil-safe `Recorder` for gateway integration
- Dry-run mode via per-endpoint `dry_run = true` — runs full pipeline up to but not including `transport.Send`, returns 200 with `{"status":"dry_run","submission_id":"...","prepared_message":{...}}` (FR56)
- CSRF tokens via HMAC-SHA256 over Unix timestamp — operator-issued at form-render time using `csrf_secret`; Posthorn verifies on submit; form-mode only (api-mode rejects `csrf_secret` at parse time) (FR57, ADR-16)
- New `core/csrf/` package with `Issue` / `Verify` helpers; `csrf_secret` and `csrf_token_ttl` endpoint config; `_csrf_token` reserved form field name
- Named `trusted_proxies` presets — `cloudflare` shipped in full; `aws-elb`, `gcp-lb`, `azure-front-door` reserved as empty slots awaiting maintained ranges. Mix presets and explicit CIDRs in one list (FR58)
- `strip_client_ip` endpoint option — omits the resolved client IP from log lines (rate-limited, etc.) for GDPR-conscious deployments; rate-limit keying unaffected (FR59)

### Block D — SMTP ingress

- New `core/ingress/` package: minimal `Ingress` interface (Start/Stop/Name); `HTTPIngress` wraps `http.Server` for the v1.0 lifecycle (FR60, FR61, ADR-12)
- New `core/smtp/` package: TCP listener accepting SMTP from internal clients; full state machine for EHLO/STARTTLS/AUTH/MAIL/RCPT/DATA/QUIT/RSET/NOOP (FR62)
- SMTP AUTH PLAIN with constant-time password compare; client-cert auth as alternative (`auth_required = "either"`) (FR63)
- `auth_required = "none"` mode for internal-network deployments where network access already implies trust (Docker bridge, loopback). Disables the AUTH state entirely; the sender allowlist (`allowed_senders`) and recipient cap remain in force as the only ingress gates. Pairs with `require_tls = false` to skip the TLS cert requirement.
- STARTTLS required by default (`require_tls = true`); rejected before AUTH/MAIL when plaintext (FR67)
- Sender allowlist (`allowed_senders`, exact-or-`*@domain` syntax) — required, non-empty (FR64)
- Recipient cap OR allowlist (default cap 10 per session) preventing RCPT bombing (FR65)
- Message size cap (`max_message_size`, default 1MB) — DATA exceeding returns `552 5.3.4` (FR66)
- MIME → `transport.Message` conversion (FR68): recipients come from SMTP envelope (`RCPT TO`), never from MIME `To`/`Cc`/`Bcc` headers — a malicious DATA blob with smuggled `Bcc:` cannot add recipients (NFR22)
- Per-session structured logging with `session_id` UUID; password never logged on auth failures (NFR23)
- `cmd/posthorn` starts both HTTP and SMTP ingresses in their own goroutines when `[smtp_listener]` is configured; SIGTERM drains both with a 15s deadline

### Removed (before tag)

- The Caddy v2 adapter module (previously planned as a secondary deployment shape under FR27–FR30 / NFR10 / ADR-6 / ADR-7) was cut before tagging v1.0.0. The single-shape standalone-behind-any-reverse-proxy story keeps the product thesis cleaner and avoids ongoing per-feature carve-outs. Caddy users keep first-class support as a reverse proxy — see [posthorn.dev/deployment/reverse-proxy](https://posthorn.dev/deployment/reverse-proxy/).
- Batch send API (originally listed as a v1.1 feature) was dropped 2026-05-16 — see [spec/01-project-brief.md](spec/01-project-brief.md) §"Deliberately not on the roadmap" for the reasoning. Reconsidered if a concrete operator workload surfaces with the need.

### Documentation

In addition to the per-feature pages, the site ships five recipes (contact form, newsletter signup, multi-form site, monitoring alerts, transactional email from a Cloudflare Worker, internal SMTP relay for Docker Compose), a [Design principles](https://posthorn.dev/design-principles/) page, a [Reading the logs](https://posthorn.dev/deployment/reading-logs/) ops guide, and a [Deploying API mode safely](https://posthorn.dev/deployment/api-mode-deployment/) guide covering loopback bind, Cloudflare Tunnel + Cloudflare Access service tokens, IP allowlist, and mTLS deployment shapes for API-mode endpoints.

### Dependencies

Three external Go dependencies in the entire module: `github.com/BurntSushi/toml`, `github.com/google/uuid`, `github.com/hashicorp/golang-lru/v2`. Every transport (Postmark, Resend, Mailgun, SES, outbound-SMTP) is bespoke — no vendor SDK in transport code per [ADR-1](spec/03-architecture.md).
