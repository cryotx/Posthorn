---
# Modified for the cryotx/posthorn distribution.
title: "Architecture: Posthorn v1.0"
status: locked
created: 2026-04-27
synced_from_obsidian: 2026-04-27
---

# Architecture: Posthorn v1.0

This document describes the structure, lifecycle, and component design for the v1.0 implementation. Every architectural decision derives from a requirement in [the PRD](./02-prd.md), which in turn derives from a commitment in [the project brief](./01-project-brief.md). Nothing here introduces new behavior.

This is the implementation blueprint. Open it before writing code; reference it during code review.

> **2026-05-15 amendment:** The Caddy v2 adapter module was cut from v1.0 pre-tag. Posthorn is now a single Go module at `github.com/craigmccaskill/posthorn` with one deployment shape (standalone behind any reverse proxy). ADR-6 and ADR-7 are retired in-place at the bottom of this doc. The product reasoning for the cut is in the brief's status log.

## Module overview

Posthorn is a single Go module at `github.com/craigmccaskill/posthorn`, rooted at `core/` in the repo. It provides `cmd/posthorn` (the binary) plus the importable HTTP form handler, transport implementations, and supporting libraries.

```
                       ┌──────────────────────────────────┐
HTTP request ─────────▶│   posthorn binary (cmd/posthorn) │
                       │     │                            │
                       │     ▼                            │
                       │   core/gateway handler           │
                       │     │                            │
                       └─────┼────────────────────────────┘
                             │
                             ▼
                       ┌──────────────┐
                       │ Postmark API │
                       └──────────────┘
```

In production the binary sits behind a reverse proxy of the operator's choice (Caddy, nginx, Traefik, Cloudflare) which handles TLS termination and request routing. Posthorn is a plain HTTP service on `:8080`.

## File layout

```
posthorn/
├── README.md
├── LICENSE                       # Apache-2.0
├── CONTRIBUTING.md
├── SECURITY.md
├── CODE_OF_CONDUCT.md
├── CHANGELOG.md
├── .github/
│   ├── workflows/
│   │   ├── ci.yml                # go vet + go test -race
│   │   └── release.yml           # tag → multi-arch Docker image to GHCR
│   ├── PULL_REQUEST_TEMPLATE.md
│   └── ISSUE_TEMPLATE/
├── docs/
│   ├── manual-test.md            # end-to-end test procedure
│   └── release-checklist.md      # tag-day procedure
├── examples/
│   └── multi-provider.toml       # v2.1: annotated provider pool + routing config
├── deploy/
│   ├── quadlet/                  # Podman Quadlet unit
│   └── compose/                  # v2.1: Docker Compose deployment (loopback-bound)
├── site/                         # Astro + Starlight source for posthorn.dev
├── spec/
│   ├── 01-project-brief.md
│   ├── 02-prd.md
│   └── 03-architecture.md
└── core/
    ├── go.mod                    # github.com/craigmccaskill/posthorn
    ├── go.sum
    ├── Dockerfile                # multi-stage; produces slim posthorn image
    ├── cmd/
    │   └── posthorn/
    │       └── main.go           # CLI entry: serve, validate
    ├── config/
    │   ├── config.go             # Config struct, TOML parser, env resolution
    │   └── config_test.go
    ├── gateway/
    │   ├── handler.go            # gateway.Handler (http.Handler implementor)
    │   ├── pipeline.go           # ordered pipeline: spam → validate → render → send
    │   └── handler_test.go
    ├── project/
    │   ├── project.go            # v2.3: Project, Registry, key digests, sender policy (FR107-FR109)
    │   ├── context.go            # v2.3: the ingress-agnostic project context (FR111)
    │   └── project_test.go
    ├── transport/
    │   ├── transport.go          # Transport interface, Message, ErrorClass, TransportError
    │   ├── retry.go              # SendWithRetry: FR19-22 policy, shared by both ingresses (#59)
    │   ├── postmark.go           # Postmark HTTP API implementation
    │   ├── resend.go             # v1.0 block C: Resend HTTP API (FR47)
    │   ├── mailgun.go            # v1.0 block C: Mailgun HTTP API (FR48)
    │   ├── awssigv4.go           # v1.0 block C: AWS SigV4 signing primitive (ADR-14)
    │   ├── ses.go                # v1.0 block C: AWS SES via SigV4 (FR49)
    │   ├── smtpout.go            # v1.0 block C: outbound SMTP transport (FR50, ADR-17)
    │   ├── provider.go           # v2.1: named provider Pool (FR93, ADR-26)
    │   ├── breaker.go            # v2.1: per-provider circuit breaker (FR96, ADR-28)
    │   ├── router.go             # v2.1: Router (a Transport over a pool), strategies, rules (FR94/FR95)
    │   ├── quota.go              # v2.2: Quota, QuotaSource, LocalQuota, UsageStore (FR99/FR100, ADR-29/30)
    │   ├── cost.go               # v2.2: fixed-point Cost (FR101, ADR-31)
    │   ├── mailkind.go           # v2.2: MailKind enum (FR104, ADR-32)
    │   ├── transport_test.go
    │   ├── provider_test.go
    │   ├── breaker_test.go
    │   ├── router_test.go
    │   ├── rules_test.go
    │   ├── quota_test.go
    │   ├── quota_routing_test.go
    │   ├── cost_test.go
    │   ├── mailkind_test.go
    │   ├── postmark_test.go
    │   ├── resend_test.go
    │   ├── mailgun_test.go
    │   ├── awssigv4_test.go
    │   ├── ses_test.go
    │   └── smtpout_test.go
    ├── spam/
    │   ├── spam.go               # Honeypot, Origin/Referer, body size
    │   └── spam_test.go
    ├── ratelimit/
    │   ├── ratelimit.go          # Token bucket, LRU-bounded
    │   ├── clientip.go           # X-Forwarded-For with trusted_proxies
    │   └── ratelimit_test.go
    ├── validate/
    │   ├── validate.go           # Required-fields, email format
    │   └── validate_test.go
    ├── template/
    │   ├── template.go           # Subject/body rendering, custom-fields passthrough
    │   └── template_test.go
    ├── response/
    │   ├── response.go           # JSON builder, content negotiation, redirects
    │   └── response_test.go
    ├── log/
    │   ├── log.go                # Structured logging helpers, submission ID
    │   └── log_test.go
    ├── idempotency/              # v1.0 block B: per-endpoint LRU cache + in-flight tracker
    │   ├── cache.go              # LRU + TTL + in-flight tracker (FR40-FR44, NFR20)
    │   └── cache_test.go
    ├── ingress/                  # v1.0 block D: Ingress interface (FR60, ADR-12)
    │   ├── ingress.go            # interface definition; HTTP gateway adapts to it
    │   └── ingress_test.go
    ├── smtp/                     # v1.0 block D: inbound SMTP listener (FR62-FR68)
    │   ├── listener.go           # TCP accept loop, STARTTLS upgrade, session state
    │   ├── session.go            # per-connection SMTP state machine
    │   ├── parse.go              # MIME → transport.Message (NFR22)
    │   ├── auth.go               # SMTP AUTH PLAIN/LOGIN + client-cert (FR63)
    │   ├── listener_test.go
    │   ├── session_test.go
    │   ├── parse_test.go
    │   └── auth_test.go
    ├── csrf/                     # v1.0 block C: HMAC-signed CSRF tokens (FR57, ADR-16)
    │   ├── csrf.go               # Issue/Verify helpers
    │   └── csrf_test.go
    └── metrics/                  # v1.0 block C: hand-rolled Prometheus exposition (FR55, ADR-15)
        ├── metrics.go            # counters, histograms, exposition writer
        ├── healthz.go            # /healthz handler (FR54)
        └── metrics_test.go
```

Sub-packages are organized one-per-concern. The original `caddy-formward` design used a flat package; Posthorn's broader scope (standalone binary, multi-ingress) justifies the package boundaries.

`core/idempotency/` lands in v1.0 feature block B (Epic 8 Story 8.4). `core/ingress/`, `core/smtp/`, `core/csrf/`, and `core/metrics/` land in feature blocks C and D (Epics 9-11). API-mode authentication is implemented in `core/gateway/` rather than a separate `core/auth/` package — auth state is per-request and tightly coupled to per-endpoint config, so the package boundary doesn't earn its keep.

## Lifecycle

```
1. main.go: parse CLI flags (subcommand: serve | validate)
2. config.Load(path): read TOML, resolve ${env.VAR}, validate schema
3. For each endpoint in config:
     a. Construct transport (Postmark client with API key)
     b. Compile templates (subject, body)
     c. Construct rate limiter
     d. Construct core.Handler with all of the above
4. Build http.ServeMux mapping each endpoint path to its core.Handler
5. http.Server.ListenAndServe()
6. On SIGTERM/SIGINT:
     a. Stop accepting new connections (server.Shutdown)
     b. Drain in-flight requests up to 10s per-request timeout
     c. Exit 0
   On second signal: forced exit
```

## Request flow

The `core/http.Handler` processes each request through an ordered pipeline. Order is intentional — cheaper checks first, header-only checks before body-parsing checks, security checks before processing.

```
┌──────────────────────────────────────────────────────────────┐
│ 1. body size cap        → http.MaxBytesReader wraps r.Body   │  413
│ 2. method check         → POST only                          │  405
│ 3. content-type check   → form-encoded only                  │  400
│ 4. origin/referer check → fail-closed if allowed_origins set │  403
│ 5. rate limit check     → token bucket, proxy-aware IP       │  429
│ 6. parse form           → r.ParseForm() reads body           │  413/400
│ 7. honeypot check       → silent 200 if field non-empty      │  200 (silent)
│ 8. required fields      → all listed fields present + non-empty │ 422
│ 9. email format         → submitter email field syntactic    │  422
│ 10. generate submission ID (UUIDv4), log "submission_received"│
│ 11. render subject template                                  │
│ 12. render body template + custom-fields passthrough         │
│ 13. transport.Send() with retry policy (FR19-22)             │
│ 14. log outcome, write response (JSON or redirect)           │  200/502
└──────────────────────────────────────────────────────────────┘
```

Ordering rationale carries over from the prior architecture (cheaper-first, header-before-body, security-first). Identical in both deployment shapes.

## Component design

### Configuration model (`core/config`)

A single top-level `Config` struct holds the parsed configuration, loaded from the TOML file.

```go
type Config struct {
    Endpoints []EndpointConfig `toml:"endpoints"`
    Logging   LoggingConfig    `toml:"logging"`
}

type EndpointConfig struct {
    Path                 string             `toml:"path"`
    To                   []string           `toml:"to"`
    From                 string             `toml:"from"`
    Transport            TransportConfig    `toml:"transport"`
    RateLimit            *RateLimitConfig   `toml:"rate_limit"`
    TrustedProxies       []string           `toml:"trusted_proxies"`
    Honeypot             string             `toml:"honeypot"`
    AllowedOrigins       []string           `toml:"allowed_origins"`
    MaxBodySize          string             `toml:"max_body_size"` // "32KB", "1MB"
    Required             []string           `toml:"required"`
    EmailField           string             `toml:"email_field"`
    Subject              string             `toml:"subject"`
    Body                 string             `toml:"body"`
    LogFailedSubmissions *bool              `toml:"log_failed_submissions"`
    RedirectSuccess      string             `toml:"redirect_success"`
    RedirectError        string             `toml:"redirect_error"`
}

type TransportConfig struct {
    Type     string                 `toml:"type"`     // "postmark"
    Settings map[string]any         `toml:"settings"` // transport-specific (api_key for postmark)
}
```

`*bool` for `LogFailedSubmissions` allows distinguishing unset (default true) from explicitly false (see ADR-4).

Env-var resolution (`${env.VAR}`) runs as a post-parse pass over all string fields recursively. Missing env vars are config-validation errors, not runtime errors.

### Transport interface (`core/transport`)

The transport layer is one interface with one implementation in v1.0. The interface is intentionally narrow so future transports (Resend, Mailgun, SMTP outbound, SMTP-ingress shared backend) can implement it without changes.

```go
type Message struct {
    From     string
    To       []string
    ReplyTo  string
    Subject  string
    BodyText string
    // BodyHTML added in v2.0 (FR71-FR75, ADR-19); optional Fields and
    // Attachments added in v2.0 (ADR-24, ADR-25).
}

type Transport interface {
    Send(ctx context.Context, msg Message) error
}

type ErrorClass int

const (
    ErrUnknown      ErrorClass = iota
    ErrTransient                // network/5xx, retry once after 1s
    ErrRateLimited              // 429, retry once after 5s
    ErrTerminal                 // 4xx (non-429), no retry
)

type TransportError struct {
    Class   ErrorClass
    Status  int
    Cause   error
    Message string
}
```

#### Multi-provider routing (v2.1, ADR-26/27/28)

The routing layer adds no concept above `Transport`. `transport.Router` *is* a `Transport`:

```go
// Built once per process from the top-level [[providers]] blocks.
// A pool entry is shared by every route that names it, so its circuit
// breaker is process-wide (ADR-28).
type Provider struct {
    Name      string        // operator-chosen pool key; reaches labels (NFR24)
    Type      string        // registered transport type
    Weight    int           // relative share under strategy = "weighted"
    Transport Transport     // the built client
    Breaker   *Breaker      // closed / open / half-open
}

type Route struct {
    Match     *RuleMatch    // nil = the default route
    Strategy  Strategy      // failover | round_robin | weighted
    Providers []*Provider
}

type Router struct {
    Name    string          // endpoint path or "smtp_listener"; label only
    Default *Route
    Rules   []*Route        // first match wins
    Hooks   RouterHooks     // metrics seam, storage.Hooks pattern
}

func (r *Router) Send(ctx context.Context, msg Message) (SendResult, error)
```

`Send` picks a route (first matching rule, else the default), orders its
providers by the strategy, and walks them: skip an open circuit, attempt,
stop at the first success. Failover is health-class only (ADR-27). All
circuits open returns `ErrTransient` so the ADR-21 queue holds the
message (ADR-28).

Strategy ordering uses smooth weighted round-robin (the nginx algorithm)
rather than weighted-random: the split is exact over each full cycle
instead of exact only in expectation, which makes both the tests and an
operator's traffic split readable. `failover` skips the rotation entirely
and always starts at the first declared provider.

`SendResult.Provider` and `TransportError.Provider` carry the pool entry
that ran, so `submission_sent` logs it, `posthorn_submissions_sent_total`
labels by it, and `Store.MarkSentVia` writes it back over the pre-send
row's `"pool"` placeholder.

#### Quota and cost (v2.2, ADR-29/30/31)

The router gained one gate and two orderings; nothing else changed.

```go
// Checked BEFORE the breaker, and never touching it: a spent budget is
// not a health event (ADR-29).
type QuotaSource interface {
    Check(p *Provider, now time.Time) QuotaVerdict
    Record(p *Provider, now time.Time)   // successful sends only
    Snapshot(name string, now time.Time) (Usage, bool)
}

// LocalQuota is quota_source = "local". Counters live in memory for the
// read path and write through to a UsageStore (implemented by
// storage.Store) so a restart does not hand out a fresh allowance.
// Rollover is a key comparison, not a timer (ADR-30).
type Quota struct{ Daily, Monthly int64 }   // 0 = unlimited
type Cost int64                             // millionths per 1000 (ADR-31)
```

Send-loop order per candidate: quota check → breaker check → attempt.
All candidates skipped for quota and none unhealthy yields
`ErrQuotaExhausted`; any open circuit in the mix outranks it and yields
`ErrTransient`, because "something is failing" is both more urgent and
the shorter wait.

`quota_source = "remote"` is a reserved config value that parse
*rejects* rather than ignores. The interface above is the seam a future
provider-usage-API implementation would satisfy; nothing beyond the
local source is built, deliberately, since the shape of those APIs is
not yet known.

An endpoint that keeps the v1.x inline `type` + `settings` block is
**not** wrapped in a Router — it gets the bare client, no breaker, no
rotation, and every label and log field is byte-identical to v1.x.

The interface and its support types are unchanged from the prior `caddy-formward` design (see brief Status log; the existing `transport.go` and `transport_postmark.go` migrate into `core/transport/` with no semantic changes — only import path updates).

### Postmark transport (`core/transport/postmark.go`)

Unchanged in behavior from the prior design:

- No third-party Postmark SDK (ADR-1)
- Headers passed as JSON struct fields (NFR1 enforcement)
- API key in `X-Postmark-Server-Token` header, never logged (NFR3)
- Status mapping: 200/202 → success; 429 → ErrRateLimited; 5xx/network → ErrTransient; 4xx (non-429) → ErrTerminal
- Package-level `*http.Client` with 5s per-request timeout (caller's context handles overall deadline)

### Gateway handler (`core/gateway`)

`core.Handler` is a plain `http.Handler` that wraps the per-endpoint pipeline:

```go
type Handler struct {
    cfg          EndpointConfig
    transport    Transport
    limiter      *ratelimit.Limiter
    subjectTpl   *template.Template
    bodyTpl      *template.Template
    namedFields  map[string]bool
    logger       *slog.Logger
}

func New(cfg EndpointConfig, opts ...Option) (*Handler, error) { ... }
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { ... }
```

Construction (`New`) does the equivalent of the prior `Provision` — resolves placeholders, compiles templates, builds the rate limiter, instantiates the transport. Errors at construction time are fatal config errors; the binary surfaces them via the `validate` subcommand's exit code.

### Spam protection (`core/spam`)

Three independent check functions, each pure (no logging, no response writing):

```go
type Result int
const (
    Pass Result = iota
    SilentReject    // honeypot — return 200 OK silently
    HardReject      // origin — return appropriate status
)

func CheckHoneypot(form url.Values, fieldName string) Result
func CheckOrigin(r *http.Request, allowed []string) (Result, string /* reason */)
// max-body-size is enforced via http.MaxBytesReader at the handler entry
```

The handler runs these in sequence and translates results to HTTP responses.

### Rate limiter (`core/ratelimit`)

Token-bucket limiter keyed by client IP. Implementation unchanged from the prior design (see ADR-3 — token bucket with LRU, not `golang.org/x/time/rate`):

```go
type Limiter struct {
    capacity     float64
    refillPerSec float64
    mu           sync.Mutex
    buckets      *lru.Cache[string, *bucket]
}

func (l *Limiter) Allow(clientIP string) bool { ... }
```

Default LRU capacity 10K IPs (NFR6). `clientip.go` extracts the keying IP from RemoteAddr or X-Forwarded-For per the trusted_proxies config.

### Validation (`core/validate`)

Pure functions:

```go
func RequiredFields(form url.Values, required []string) []string
func EmailFormat(value string) bool
```

No logging, no response writing — the handler uses returns to construct 422 responses.

### Templating (`core/template`)

Subject and body rendering with custom-fields passthrough. The "named fields" set (required + email field + honeypot + fields referenced in templates) is computed at config-load time via Go template `Tree.Root.Nodes` walk, cached in the Handler.

Custom-fields passthrough format unchanged from prior design:
```
[rendered body template output]

Additional fields:
  company: Acme Corp
  source: HN
```

### Response handling (`core/response`)

```go
type ErrorResponse struct {
    Error  string            `json:"error"`
    Code   string            `json:"code"`
    Fields map[string]string `json:"fields,omitempty"`
}

func WriteJSON(w http.ResponseWriter, status int, body any) error
func WriteRedirect(w http.ResponseWriter, r *http.Request, url string) error
func Negotiate(r *http.Request, hasRedirects bool) Mode
```

Negotiation logic: if no redirect URLs configured → JSON; else if `Accept: application/json` preferred → JSON; else redirect.

### Logging (`core/log`)

Wraps `log/slog` (stdlib) with structured-field helpers. The binary configures slog with a JSON handler at INFO by default.

```go
type Ctx struct {
    SubmissionID uuid.UUID
    Endpoint     string
    Transport    string
    StartTime    time.Time
}

func (l *Logger) Received(ctx Ctx, form url.Values)
func (l *Logger) Sent(ctx Ctx)
func (l *Logger) Retry(ctx Ctx, err error)
func (l *Logger) Failed(ctx Ctx, err error, payload url.Values)
func (l *Logger) SpamBlocked(ctx Ctx, reason string)
func (l *Logger) RateLimited(ctx Ctx, clientIP string)
func (l *Logger) ValidationFailed(ctx Ctx, fields []string)
```

Every log line includes `submission_id`, `endpoint`, `transport`, `latency_ms`. API keys never appear in any log path.

`Failed` includes the full payload only if `log_failed_submissions` is true; otherwise omits form values.

## Threat → defense → code mapping

This is the load-bearing artifact for security review. Every in-scope threat from [the project brief](./01-project-brief.md) §"Threat Model" maps to a concrete defense in concrete code with concrete tests.

| Threat | Defense | Code | Tests |
|---|---|---|---|
| Drive-by scraper bots | Honeypot field | `core/spam/spam.go::CheckHoneypot` | `core/spam/spam_test.go::TestHoneypot_*` |
| Direct-POST bots that skip the form page | Origin/Referer with fail-closed | `core/spam/spam.go::CheckOrigin` | `core/spam/spam_test.go::TestOrigin_*` |
| Basic targeted abuse | Token bucket rate limit, proxy-aware | `core/ratelimit/ratelimit.go::Allow`, `core/ratelimit/clientip.go` | `core/ratelimit/ratelimit_test.go` |
| Postmark quota burn | Rate limit + max body size | `core/ratelimit/` + `http.MaxBytesReader` in handler | as above + `core/gateway/handler_test.go::TestBodySizeLimit` |
| Email header injection | Header fields passed as JSON struct fields, never string-concat | `core/transport/postmark.go::Send` (struct → `json.Marshal`) | `core/transport/postmark_test.go::TestNoHeaderInjection_*` |
| API key theft from logs | Key set in HTTP header at construction time, never passed to logger | `core/transport/postmark.go::Send`, `core/log/*` | `core/log/log_test.go::TestNoAPIKeyInLogs` |
| **v1.1: Leaked API-mode key** (operator-side compromise) | Constant-time comparison; per-key rate limit (not per-IP); key never logged | `core/gateway/handler.go` (auth path), `core/ratelimit/` (key extraction) | `core/gateway/handler_test.go::TestAPIAuth_*`, `TestAPIAuth_KeyNotInLogs` |
| **v1.1: Replay attacks on API-mode endpoints** | Idempotency-Key cache returns byte-identical response on replay; in-flight collision returns 409 | `core/idempotency/cache.go`, `core/gateway/handler.go` (cache integration) | `core/idempotency/cache_test.go`, `core/gateway/handler_test.go::TestIdempotency_*` |
| **v1.1: Timing attack on API-key comparison** | `crypto/subtle.ConstantTimeCompare` for all key checks | `core/gateway/handler.go` (auth path) | `core/gateway/handler_test.go::TestAPIAuth_ConstantTimeCompare` (source-level assertion) |
| **v1.1: Mode misconfiguration** (operator sets form-mode defenses on API-mode endpoint, thinks they're protected) | Parse-time rejection with named-field error | `core/config/config.go::Validate` | `core/config/config_test.go::TestAPIMode_RejectsFormFields` |
| **v1.0 D: SMTP open relay** (attacker connects to listener and tries to send to arbitrary recipients) | Auth required after EHLO (530); sender allowlist (550); recipient allowlist OR cap (550); STARTTLS required by default (530) | `core/smtp/session.go`, `core/smtp/auth.go` | `core/smtp/session_test.go::TestUnauth*`, `TestSenderAllowlist*`, `TestRecipientCap*`, `TestSTARTTLSRequired*` |
| **v1.0 D: SMTP header injection via MIME** (attacker sends `Subject:` followed by `\r\nBcc:`) | MIME parsed by `net/mail`, not string-split; headers stored as a structured map; outbound Message construction uses struct fields | `core/smtp/parse.go` | `core/smtp/parse_test.go::TestNoHeaderInjection_MIME*` (NFR22) |
| **v1.0 D: SMTP credential leak** (attacker captures AUTH PLAIN bytes off the wire) | STARTTLS required before AUTH; plain-text AUTH path explicitly disabled | `core/smtp/session.go` | `core/smtp/session_test.go::TestAuthBlockedBeforeTLS` |
| **v1.1 D: unauthenticated public bind** (FR69, #41 — operator sets `auth_required = "none"` on a public listen address, exposing an open relay gated only by the sender allowlist) | Parse-time refusal of non-loopback/non-private binds under auth "none" unless `trusted_network = true` is set as an explicit trust assertion | `core/config/config.go::validatePrivateBind` | `core/config/config_test.go::TestValidate_SMTPAuthNone_*` |
| **v1.1 D: SMTP AUTH brute-force / connection exhaustion** (FR70, #50 — attacker guesses credentials or floods connections) | Per-IP AUTH-failure token bucket (421+close past budget); global + per-IP concurrent-connection caps (421 before protocol work) | `core/smtp/listener.go`, `core/smtp/session.go::authFailed` | `core/smtp/listener_test.go::TestSMTP_AuthBruteForce_*`, `TestSMTP_*ConnectionCap_421` |
| **v1.0 C: `/metrics` cardinality explosion** (attacker triggers metric labels containing recipient emails) | Labels carry only operator-configured names (endpoint path, transport type, error class); submitter values never appear as labels | `core/metrics/metrics.go` | `core/metrics/metrics_test.go::TestNoSubmitterCardinality` (NFR24) |
| **v1.0 C: `csrf_secret` leak via logs** | Secret loaded from `${env.VAR}`, never logged; same pattern as NFR3 for API keys | `core/csrf/csrf.go`, `core/log/*` | `core/csrf/csrf_test.go::TestSecretNotInLogs` |

For threats explicitly out of scope, this table also documents the *non*-defense:

| Out-of-scope threat | Disposition |
|---|---|
| SMTP-ingress threats (open relay, MX spoofing, RCPT bombing) | v1.3 SMTP ingress will add: AUTH PLAIN/LOGIN, RCPT recipient cap, sender allowlist, body size cap. Architecture must not foreclose this — see "Forward compatibility" below. |
| Botnet spam (many low-rate IPs) | No v1.0 defense; LRU eviction at 10K IPs gracefully degrades but does not protect. v3 captcha/PoW is the planned response. |
| DDoS / Layer 7 attacks | CDN's responsibility; gateway trusts upstream proxy/load balancer to absorb. Documented in README. |
| API key theft from misconfigured deployment | Operator concern; mitigated by `${env.VAR}` config + NFR3 (key never logged). Documented in README. |

## Concurrency and state

### Per-request state

Each `ServeHTTP` invocation operates on:
- The request and response writer (request-scoped, no shared mutation)
- A locally-allocated `log.Ctx` with a fresh UUID
- Locally-rendered subject and body strings
- A local `Message` passed to `Transport.Send`

No request-scoped state is shared across requests.

### Shared state

Each `core.Handler` holds three pieces of shared state, all populated at construction time and immutable thereafter:

| State | Concurrency strategy |
|---|---|
| `*template.Template` (subject and body) | Go's `template.Template` is safe for concurrent execution after parse; no extra locking needed. |
| `Transport` (Postmark client) | Wraps a single `*http.Client`. Standard library guarantees `*http.Client` is safe for concurrent use. The transport itself is stateless beyond the client and API key. |
| `*ratelimit.Limiter` | Mutex-guarded; one mutex per limiter, held only during the per-IP bucket update. Contention is bounded by request rate. |

No goroutines are spawned per request. The standalone `cmd/posthorn` runs `http.Server` which spawns goroutines per connection; the rate limiter and template are the only shared state across those goroutines.

## Dependencies

### Core (production)

| Dependency | Purpose | Justification |
|---|---|---|
| `github.com/BurntSushi/toml` | TOML config parsing | The standard Go TOML library; mature, single-purpose |
| `github.com/google/uuid` | Submission ID generation | Standard, single-purpose, ~200 LOC |
| `github.com/hashicorp/golang-lru/v2` | LRU cache for rate limiter | Battle-tested, type-parameterized in v2 |
| `modernc.org/sqlite` (v2.0+) | SQLite driver for `core/storage` | Pure Go preserves the `CGO_ENABLED=0` static multi-arch build; by far the project's largest dep, taken deliberately — full reasoning and named costs in ADR-20 |

### Core (test-only)

| Dependency | Purpose |
|---|---|
| Standard library `testing` | Test framework |
| Standard library `net/http/httptest` | Mock Postmark API server |

### Explicitly NOT pulled in

- Postmark SDKs (ADR-1)
- Validation libraries — stdlib `net/mail` suffices for v1.0
- Rate-limiting libraries (ADR-3)
- Templating engines beyond stdlib `text/template`
- HTTP frameworks (gin, echo, chi, etc.) — stdlib `net/http` is sufficient
- Logging libraries beyond stdlib `log/slog`
- Cobra/urfave/cli for CLI — flag stdlib package suffices for `serve`/`validate`

This list is conservative on purpose: every dependency is a v1.1+ liability.

## Test architecture

Test files are co-located with the source they test (`spam.go` ↔ `spam_test.go`).

### Mock Postmark server

Transport tests use `httptest.NewServer` with handler stubs. The Postmark transport accepts a `BaseURL` config value (default `https://api.postmarkapp.com`) so tests can point it at the mock. Production config never sets `BaseURL`.

### Header injection tests (NFR2)

Required test cases as a table (carries over from prior architecture; the payloads are unchanged):

```go
var injectionPayloads = []struct {
    name  string
    field string
    value string
}{
    {"crlf_bcc_in_from",     "from",     "attacker@evil.com\r\nBcc: target@victim.com"},
    {"crlf_bcc_in_subject",  "subject",  "Hello\r\nBcc: target@victim.com"},
    {"crlf_in_replyto",      "reply_to", "x@x.com\r\nBcc: target@victim.com"},
    {"unicode_crlf",         "subject",  "Hello\u000ABcc: target@victim.com"},
    {"smuggled_header",      "name",     "Jane\r\nX-Spoof: yes"},
}
```

For each payload: send a submission, capture the outgoing JSON to the mock Postmark server, assert (1) the injected CRLF sequence does not appear as a JSON-level header in the marshaled request and (2) the literal string is preserved as data within the field it was injected into.

### CI

Single Linux + Go 1.25 job. `go vet ./...` and `go test -race -count=1 -timeout=2m ./...` from the `core/` working directory.

## Build and distribution

### Standalone binary

```bash
go install github.com/craigmccaskill/posthorn/cmd/posthorn@latest
```

Produces a single static binary. No CGO. Single-file deployment.

### Standalone Docker image

Multi-stage:

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
ENV CGO_ENABLED=0 GOWORK=off
RUN go build -trimpath -ldflags="-s -w" -o /out/posthorn ./cmd/posthorn

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /out/posthorn /usr/local/bin/posthorn
USER nonroot
ENTRYPOINT ["/usr/local/bin/posthorn", "serve"]
CMD ["--config", "/etc/posthorn/config.toml"]
```

Multi-arch via `docker buildx` in the release workflow: `linux/amd64` and `linux/arm64`. Image at `ghcr.io/craigmccaskill/posthorn:v1.0.0` and `:latest`.

### Release artifacts

For each tagged release (v1.0.0+):
- GitHub Release with hand-written notes
- Multi-arch Docker image at `ghcr.io/craigmccaskill/posthorn`
- No pre-built binaries — `go install` and Docker are the supported install paths

## v1.0 implementation plan + forward compatibility

Sections below describe both the v1.0 implementation plan (feature blocks A through D, originally numbered v1.0/v1.1/v1.2/v1.3 before the 2026-05-16 rescope) and the genuinely forward-looking v2 / v3 commitments. The four v1.0 blocks are sequenced as Epics 1–11 inside a single v1.0 release; the version sub-headings preserve historical traceability and let each block be reasoned about independently. v2 and v3 remain future scope and carry forward-compatibility implications for v1.0 code.

These commitments serve the project's [Design principles](./01-project-brief.md#design-principles) in the brief — especially "Gateway, not infrastructure" (principle #1) and "Integration layer, not mail-receiving layer" (principle #2). When adding a new forward-compat commitment, check both the principles and the existing ADRs (below); architecture decisions that contradict the principles need spec discussion first.

### v1.1 (API mode) — scoped 2026-05-16

A second auth + ingress shape alongside the v1.0 form-mode pipeline. None of the additions break the existing form-mode path (FR45 backwards-compat invariant). Three coherent features: API-key auth (FR31–FR35), JSON content type (FR36–FR39), idempotency (FR40–FR44). Batch send was originally listed here and dropped 2026-05-16; see brief's "Deliberately not on the roadmap" section.

**Mode dispatch (FR31, FR32).** New `EndpointConfig.Auth` field with values `"form"` (default; v1.0 behavior) and `"api-key"`. The handler routes per-mode at the top of `ServeHTTP`. Mode is fixed per endpoint — there is no mixed shape. Mode-incompatible config fields (`honeypot`, `allowed_origins`, `redirect_success`, `redirect_error` on API-mode endpoints) are rejected at config-parse time with a named-field error message, not silently ignored at request time — see ADR-10.

**API-key authentication (FR33, FR34, NFR19, NFR21).** API-mode endpoints require non-empty `api_keys`. The handler parses `Authorization: Bearer <key>` and compares against the list using `crypto/subtle.ConstantTimeCompare`. Multiple keys are supported per endpoint to enable rotation. Failed auth returns HTTP 401 with no detail in the body — the operator's logs carry the structured failure. The matched key never appears in log output (parallel to NFR3 for Postmark tokens).

**API-mode rate limiting (FR35).** On API-mode endpoints, the existing `core/ratelimit/` token bucket keys on the matched API key value, not the client IP. Server-to-server callers commonly NAT through shared egress IPs; IP-keyed rate limiting would conflate independent callers. `trusted_proxies` / `X-Forwarded-For` logic does not apply to API-mode endpoints. The underlying token-bucket + LRU eviction implementation is unchanged.

**JSON ingress (FR36–FR39).** API-mode endpoints accept `application/json` bodies only. JSON is parsed into the same flat-keyed `map[string][]string` shape the form parser produces (single-valued keys become a one-element slice), so template rendering, validation, custom-fields passthrough, and the transport layer require no v1.1 changes. Non-JSON bodies on API-mode endpoints return HTTP 415, never silently accepted.

**Idempotency (FR40–FR44, NFR20).** New package `core/idempotency/` provides per-endpoint LRU caches with TTL eviction (24h default) and an in-flight tracker. The cache key is the raw `Idempotency-Key` header value; per-endpoint scoping (FR41) is achieved by giving each API-mode endpoint its own cache instance — no key prefixing, no cross-endpoint collision possible by construction. The cache stores the *complete* response (status code, body bytes, submission_id, transport_message_id), so replays are byte-identical to the original (NFR20). In-flight tracker is a separate `map[string]chan struct{}` guarded by a mutex; a duplicate key arriving before the original completes returns HTTP 409 Conflict — not blocking, see ADR-9. v2 swaps the storage backend for SQLite while keeping the package interface stable.

**Per-request recipient override (FR46, ADR-11).** API-mode request bodies may include a `to_override` field (string or array of strings) that replaces the endpoint's configured recipients for that one request. Each entry passes the same email-syntax validation as form-mode submissions (FR11); failures return 422 with field detail. `from` is intentionally not overridable — see ADR-11 for the safety-asymmetry reasoning. The handler inserts the override between validation and `transport.Message` construction; absent override falls back to `cfg.To` (no behavior change for callers that don't use it).

### v1.0 block C (multi-transport + operational maturity) — scoped 2026-05-16

Originally v1.2; folded into v1.0 alongside blocks B and D. Four new transports plus operational features.

**Transport registry (FR47–FR53, Story 9.1).** Today's `config.TransportConfig.Validate` switches hard-coded on `type`. The v1.0-C refactor introduces a registry pattern where each transport file (`resend.go`, `mailgun.go`, `ses.go`, `smtpout.go`) calls `transport.Register(type string, validator func(map[string]any) error, factory func(cfg map[string]any) (Transport, error))` in an `init()`. The Postmark transport adapts to the same registration pattern. Tests pass unchanged because the public `Transport` interface and `TransportError` contract don't move.

**Resend transport (FR47, Story 9.2).** Bespoke HTTP client (~150 LOC) against `https://api.resend.com/emails`. Bearer auth via `Authorization` header. Response shape includes a JSON `id` field which becomes `SendResult.MessageID`. Error mapping: 422 → `ErrTerminal`, 429 → `ErrRateLimited`, 5xx → `ErrTransient`. NFR1 enforcement via `json.Marshal` of a struct (parallel to Postmark).

**Mailgun transport (FR48, Story 9.3).** Bespoke HTTP client (~180 LOC). Auth via HTTP Basic `api:<key>`. Request body uses `mime/multipart.Writer` — never hand-craft boundary string concatenation (NFR1 lives in the multipart writer, which escapes the structured fields properly). Endpoint: `https://api.mailgun.net/v3/<domain>/messages`. Response `id` field → `SendResult.MessageID`.

**AWS SES transport (FR49, ADR-14, Story 9.4).** Bespoke SigV4 implementation in `core/transport/awssigv4.go` (~200 LOC), SES request shape in `ses.go` (~100 LOC). SigV4 signing is the canonical AWS signature-v4 algorithm: build the canonical request, hash, build the string-to-sign, derive the signing key via four rounds of HMAC-SHA256, sign. Tests round-trip against AWS's published canonical-request examples. SES request shape uses the SESv2 `SendEmail` endpoint (JSON body, returns a `MessageId`). **The ADR-14 tripwire is real:** if total LOC including tests exceeds 500, we stop and surface the deviation rather than ship bloated bespoke code.

**Outbound-SMTP transport (FR50, ADR-17, Story 9.5).** Stdlib `net/smtp.PlainAuth` + `net/smtp.NewClient` + `STARTTLS`. Required config: `host`, `port`, `username`, `password`, `require_tls` (default true). The transport is stateful per-Send (open connection, AUTH, STARTTLS, MAIL FROM, RCPT TO, DATA, QUIT, close) — no connection reuse in v1.0; each Send is a fresh TCP+TLS handshake. Reuse is a v2 optimization. Error classification: 421 / 450 / 451 (greylisting) → `ErrTransient`; 530 (auth) / 550-552 (relay rejection, size) → `ErrTerminal`; TLS handshake failure → `ErrTerminal` (config bug, not transient).

**`/healthz` and `/metrics` (FR54, FR55, ADR-15, Story 10.1).** New `core/metrics/` package. `/healthz` returns 200 + `ok`. `/metrics` writes Prometheus text exposition format manually — `# HELP`, `# TYPE`, then `<metric_name>{<labels>} <value>` lines. Counter primitive: `atomic.Int64` + label map. Histogram primitive: fixed buckets ([1ms, 5ms, 10ms, 50ms, 100ms, 500ms, 1s, 5s, 10s]) + `atomic.Int64` per bucket. The metrics handler walks the registered metrics and emits in alphabetical order for deterministic output. NFR24 enforcement: label values come from a known operator-controlled set (endpoint paths from config, transport types from registry, error classes from `ErrorClass.String()`) — no submitter-controlled strings ever enter the label space.

**Dry-run mode (FR56, Story 10.2).** New `EndpointConfig.DryRun bool` field. Handler short-circuits before `sendWithRetry`: skip transport, build the `transport.Message` as normal, write `200 OK` with JSON body `{"status":"dry_run","prepared_message":{...}}`. Idempotency cache treats dry-run replies as cacheable (the operator may legitimately replay a dry-run to inspect template state). Form-mode and api-mode both supported.

**CSRF tokens (FR57, ADR-16, Story 10.3).** New `core/csrf/` package exposing `Issue(secret []byte, ttl time.Duration) string` and `Verify(secret []byte, token string, maxAge time.Duration) error`. Token format: `<unix-timestamp>.<base64-hmac>` where `hmac = HMAC-SHA256(secret, timestamp)`. `Issue` is for operator-side embedding at form-render time; `Verify` runs in the handler when `csrf_secret` is configured. Form-mode only — api-mode endpoints reject `csrf_secret` at config parse (parallels FR32's mode-mutex). The handler integrates the check after honeypot and before required-fields validation: missing/malformed/expired → 403.

**`trusted_proxies` named presets (FR58, Story 10.4).** Embedded static map at package init time:

```go
var trustedProxiesPresets = map[string][]string{
    "cloudflare":        {"173.245.48.0/20", "103.21.244.0/22", /* full list */},
    "aws-elb":           {/* AWS published ELB CIDR ranges */},
    "gcp-lb":            {/* Google Cloud LB CIDRs */},
    "azure-front-door":  {/* Azure Front Door CIDRs */},
}
```

`ratelimit.ParsePrefixes` expands preset names at parse time; mixing presets with explicit CIDRs in one list is allowed. Updates to a provider's published ranges require a Posthorn release. We don't refresh dynamically — the brief's "no runtime mutation surface" principle applies.

**IP stripping (FR59, Story 10.5).** New `EndpointConfig.StripClientIP bool`. Handler check at each log site that includes `client_ip` — omit the field entirely when `strip_client_ip = true`. The IP is still computed (rate limiter needs it) but not persisted to log lines. Rate-limit keying is unaffected.

### v1.0 block D (SMTP ingress) — scoped 2026-05-16

Originally v1.3; folded into v1.0 alongside blocks B and C. The strategic feature that completes the gateway thesis — internal apps that speak SMTP get a relay that exists and works.

**`Ingress` interface (FR60, FR61, ADR-12, Stories 11.1, 11.2).** New `core/ingress/` package defining:

```go
// Ingress is "thing that produces a transport.Message and dispatches it."
// HTTP form/api-mode handler and SMTP listener both implement it.
type Ingress interface {
    // Lifecycle methods (Start/Stop) match cmd/posthorn's listener-loop
    // shape. Each ingress owns its own listener goroutine.
    Start(ctx context.Context) error
    Stop(ctx context.Context) error
}
```

The interface itself is intentionally minimal — Messages flow internally to each ingress and reach `transport.Send` via the ingress-specific path. The interface is for `cmd/posthorn`'s lifecycle management, not for cross-ingress code sharing. **HTTP-specific concepts (templates, content negotiation, redirects, idempotency keys, CSRF tokens) do not appear in the interface** — they're internal to the HTTP ingress.

The existing `gateway.Handler` adapts to satisfy `Ingress` by wrapping itself in a Start/Stop pair that runs an `http.Server`. No behavior change; the refactor is mechanical.

**SMTP listener architecture (FR62–FR67, Stories 11.3, 11.4, 11.6, 11.7).** New `core/smtp/` package with three concerns:

```
core/smtp/
  listener.go   — TCP accept loop, per-connection goroutine spawn
  session.go    — SMTP state machine (EHLO → STARTTLS → AUTH → MAIL → RCPT → DATA → QUIT)
  parse.go      — MIME → transport.Message
  auth.go       — SMTP AUTH PLAIN/LOGIN + client-cert verification
```

State machine states: `Greeted`, `TLSPending`, `TLSEstablished`, `Authed`, `InTransaction`, `InData`, `Quit`. Transitions are validated — sending RCPT before MAIL FROM is rejected with `503 5.5.1 Bad sequence of commands`. STARTTLS upgrade replaces the underlying `net.Conn` with `*tls.Conn` and resets EHLO state. AUTH PLAIN decodes `\x00<user>\x00<pass>` and constant-time compares (NFR3 + NFR21 carry over).

Per-session state lives on a `session` struct, garbage-collected when the connection closes. Idle timeout (default 60s) closes hanging connections. Max message size enforced by `io.LimitReader` on the DATA path.

**MIME ingest (FR68, NFR22, Story 11.5).** `core/smtp/parse.go` uses `net/mail.ReadMessage` to parse the DATA blob into headers + body. For multipart bodies, `mime/multipart.NewReader` walks parts; we extract `text/plain` (preferred) or reject HTML-only with `554 5.6.0 No plain-text part`. (v2.0 amends this per FR75: `text/html` maps to `BodyHTML` and HTML-only messages are accepted with an auto-derived text part; the 554 path is v1.x behavior.) Resulting `transport.Message`: From (header), To (RCPT TO list — overrides MIME `To:` per NFR22 to prevent header injection), Subject (header, validated for CRLF), BodyText.

Key NFR22 guarantee: **MIME `To:`/`Cc:`/`Bcc:` headers in the inbound message are ignored.** Recipients are taken exclusively from the SMTP transaction's RCPT TO commands, which we've already validated against the allowlist. This is the structural defense against `Subject: hi\r\nBcc: victim@target.com` style injection — the smuggled `Bcc:` header lands in the parsed headers map but never affects recipients.

**SMTP-specific defenses (FR64, FR65, FR66, FR67, Story 11.6).** Sender allowlist matching (exact email or `*@domain.com` wildcard). Recipient allowlist OR `max_recipients_per_session` (one must be set; both can coexist with allowlist applied first). Message size cap via `io.LimitReader`. STARTTLS enforcement: when `require_tls=true`, plain-text AUTH and MAIL commands are rejected with `530 5.7.0 Must issue STARTTLS first`.

**Binary integration (Story 11.7).** `cmd/posthorn/main.go` reads top-level `[smtp_listener]` config (parallel to `[[endpoints]]`); when present, constructs the SMTP ingress and adds it to a list of `[]Ingress` to start. Each ingress runs in its own goroutine. SIGTERM signals all, drains all (each ingress has its own drain timeout — 10s for HTTP, longer for SMTP since transactions may be mid-flight). Forced exit on second signal.

**Config shape:**

```toml
[smtp_listener]
listen = ":2525"
require_tls = true
tls_cert = "/etc/posthorn/cert.pem"
tls_key = "/etc/posthorn/key.pem"

# Auth: smtp-auth | client-cert | either
auth_required = "smtp-auth"
[[smtp_listener.smtp_users]]
username = "ghost"
password = "${env.GHOST_SMTP_PASSWORD}"
[[smtp_listener.smtp_users]]
username = "gitea"
password = "${env.GITEA_SMTP_PASSWORD}"

allowed_senders = ["noreply@yourdomain.com", "*@internal.yourdomain.com"]
allowed_recipients = ["*"]  # or list specific allowed RCPT TO patterns
max_recipients_per_session = 10
max_message_size = "1MB"
idle_timeout = "60s"

[smtp_listener.transport]
type = "postmark"

[smtp_listener.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
```

### v2.0 (recut 2026-08-02 — gateway reliability + seam features)

The stateful shift, scoped by the first-principles recut recorded in the brief's status log. Unsubscribe injection and fan-out moved out behind demand gates; what remains strengthens the integration seam. Storage is optional: without `[storage]`, every v1.x code path is byte-identical.

**New packages and touch points:**

- `core/storage/` — SQLite via modernc.org/sqlite (ADR-20). Tables: `submissions` (UUID, endpoint, transport, rendered message, raw fields, status enum `sent|queued|failed|suppressed`, timestamps, transport_message_id, last_error), `retry_queue` (submission FK, attempt, next_attempt_at), `idempotency` (key, endpoint, response blob, expires_at), `suppressions` (email, reason, source_endpoint, created_at; exempt from retention), plus a `schema_version` pragma with forward-only migrations. `max_size` maps to `max_page_count`; retention pruning and the canary probe run on jittered background tickers. Storage failure trips degrade-to-sync (FR80/NFR27): the gateway keeps sending, offers no 202s, and surfaces state via healthz/metrics/log.
- **Retry worker** — single background goroutine per process (NFR26 single-writer), claims due rows, replays through the ingress-agnostic `transport.SendWithRetry`, backs off exponentially, dead-letters after 5 attempts. At-least-once: the crash window between provider accept and status write can replay (NFR28); submission IDs are stable for receiver-side dedup.
- `core/lifecycle/` — Postmark event ingestion (`/events/postmark`, basic-auth fail-closed, rate-limited), normalization to the stable event shape, message-ID correlation against the submission log, HMAC-signed forwarding to per-endpoint `webhook_url` (transient forward failures ride the retry queue), and the auto-suppress hook (hard_bounce/spam_complaint → global suppression row).
- `core/transport/webhook.go` — sixth transport: signed JSON POST of envelope + rendered bodies + raw `Fields` (ADR-24). Standard FR19-22 classification; registry entry like the other five.
- **Attachments** — ingestion in the HTTP ingress only (form multipart + api-mode base64), sniffed-type enforcement against the fail-closed allowlist (ADR-25), `Message.Attachments []Attachment{Filename, ContentType, Data}`, per-provider structural mapping; SMTP-out wraps the existing body in `multipart/mixed`.
- **HTML body** — `html/template` render path (ADR-19), bespoke HTML-to-text fallback, `BodyHTML` on Message, `multipart/alternative` in SMTP-out, HTML-part mapping in the SMTP listener.

**New threat surface (extends the threat→defense mapping):**

| Threat | Defense |
|---|---|
| Forged lifecycle events (fake bounces to poison suppression) | Mandatory basic auth, fail-closed, constant-time compare; rate limit; unknown message IDs dropped |
| Callback forwarding as SSRF-ish primitive | `webhook_url` is operator-configured only, never submitter-influenced |
| Malicious attachment content | Sniffed-type allowlist (declared type ignored), count/size caps, opt-in per endpoint; delivery-side scanning remains the provider's job |
| Attachment filename header injection | Filenames cross only as structured API fields / MIME-encoded parameters; injection suite extended (FR92) |
| SQLite file as theft target (PII, payloads, suppression emails) | Operator-owned file permissions/backup posture, documented (NFR31); retention bounds exposure window |
| Disk exhaustion via queue/attachments | `max_size` page cap + retention + degrade-to-sync; Posthorn cannot fill the disk itself |

## Architectural decisions log

**ADR-1: No Postmark SDK.** A third-party SDK adds a dependency we'd have to track and update for every Postmark API change. The bespoke client is ~80 lines, has zero runtime overhead, and gives complete control over error classification. Reconsider in v1.1+ only if 3+ HTTP API transports duplicate enough code to warrant a shared SDK abstraction.

**ADR-2 (revised twice): Internal sub-packages within a single `core/` module.** The original `caddy-formward` design used a flat package (small Caddy-module surface). Posthorn briefly adopted a two-module workspace (`core/` + `caddy/`) when the Caddy adapter was in scope; that workspace was collapsed back to the single `core/` module on 2026-05-15 when the adapter was cut. The sub-package layout (`core/gateway`, `core/transport`, `core/spam`, etc.) is retained because it still earns its keep — `cmd/posthorn` consumes it as a library, and the v1.3 SMTP ingress needs a place to land that doesn't disturb HTTP-form code.

**ADR-3: Token bucket with LRU, not `golang.org/x/time/rate`.** `x/time/rate` is excellent but doesn't bound memory — every distinct key (IP) holds a `Limiter` forever. We need LRU eviction at 10K IPs (NFR6) which requires rolling our own. The implementation is ~30 lines and well-tested. Unchanged from prior design.

**ADR-4: `*bool` for `LogFailedSubmissions`.** A pointer lets us distinguish "operator omitted the field" (default true) from "operator explicitly set false." Pointer-bool is a common Go config pattern despite the awkwardness. Unchanged from prior design.

**ADR-5: Synchronous send, not async with queue.** v1.0 has no persistent storage, so an async queue would be lost on restart. The brief commits to "log on terminal failure" as the recovery mechanism, which works only if the failure is observed in the request log. v2 brings SQLite + async queue together. Unchanged from prior design.

**ADR-6 (retired 2026-05-15): Core has zero Caddy dependency.** Originally the load-bearing decision that made the standalone-plus-adapter architecture work. Trivially true after the Caddy adapter was cut; preserved as a numbered slot for historical traceability.

**ADR-7 (retired 2026-05-15): Standalone is the primary deployment shape; Caddy adapter is optional.** Originally framed the standalone Docker image as the headline distribution with the Caddy adapter as a secondary sibling. After the 2026-05-15 cut, the standalone is the *only* deployment shape; the primary/secondary distinction is gone. Preserved as a numbered slot for historical traceability.

**ADR-8: Per-endpoint in-memory LRU cache for idempotency, not a shared store.** The v1.1 idempotency cache is local-process state: one LRU cache instance per API-mode endpoint, configurable capacity, 24h TTL. We considered three alternatives — a single global cache with prefixed keys, an external Redis dependency, or SQLite-backed durability — and rejected all three. Global-with-prefixes shares the LRU cap across endpoints (one chatty endpoint can evict another's keys), violating the FR41 per-endpoint scoping guarantee by construction risk. Redis adds an operational dependency that contradicts the "single binary, single TOML config" thesis. SQLite-backed durability is the v2 design — exposing it in v1.1 means committing to schema migrations and corruption-handling pre-v2, which the brief explicitly defers. Per-endpoint LRU keeps the v1.1 contract simple, the failure modes obvious (cache evicts → cache miss → caller retries → fresh send), and leaves the v2 swap point at the package interface boundary.

**ADR-9: HTTP 409 on in-flight idempotency-key collision, not blocking.** When two requests arrive concurrently with the same `Idempotency-Key` and the first hasn't completed, the second returns HTTP 409 Conflict immediately rather than waiting for the first to finish. Alternatives considered: block-and-return-same-result (AWS / PayPal style) and reject-with-429-and-retry-after. Block-and-return adds a coordination primitive (per-key wait channel, fairness, timeout interaction with the 10s hard request limit per FR22) for marginal caller convenience; the only callers hitting this race in practice are workers retrying without backoff, which is a caller bug we want to surface rather than mask. 429 implies "try again later because of capacity," misleading when the real signal is "your retry is already in flight." 409 Conflict is the semantically correct status and matches Stripe's documented behavior. Decided during v1.1 FR drafting, 2026-05-16.

**ADR-10: Mode/defense mutex enforced at config-parse time, not request time.** When an operator configures `auth = "api-key"` together with `honeypot`, `allowed_origins`, or any `redirect_*` field, the config fails to parse with an error naming the offending field. Alternative considered: silently ignore the inapplicable fields at request time. Silent ignore creates a footgun — an operator configuring `honeypot = "_gotcha"` on an API-mode endpoint mentally checks "spam-protected" without realizing the field is dead, then thinks the endpoint is defended when it isn't. Parse-time rejection forces the operator to learn the mode model and acts as live documentation. This parallels NFR4's strict treatment of explicitly-empty `allowed_origins` — fail closed and loud, not silent. The narrow cost (operators flipping a single endpoint between modes have to also delete the inapplicable fields) is worth the safety. Decided during v1.1 FR drafting, 2026-05-16.

**ADR-11: Per-request `to_override` is in scope; per-request `from` is not.** API-mode JSON bodies may include a `to_override` field (string or array of strings) that replaces the endpoint's configured recipients for that request. `from` stays endpoint-configured and cannot be overridden per request. The asymmetry follows from where leaked-key blast radius lands:

- **Per-request `to`:** an attacker with a leaked API key can send email *from* the operator's verified domain *to* arbitrary addresses. The operator's domain reputation isn't directly damaged (recipients see legitimate-looking mail from a real domain; provider abuse systems catch volume-based spam patterns). Rate limit + idempotency + Postmark's own abuse detection cap the damage. The alternative — "one endpoint per recipient" — doesn't scale to user-shaped recipients (password resets, receipts, notifications all go to a different user each time). Per-request `to` is the only practical shape for the named v1.1 audience.

- **Per-request `from`:** an attacker with a leaked API key could spoof email *from* arbitrary identities (`from = "ceo@victim.com"`) through the operator's Postmark account. Postmark would reject the send if `from` doesn't match a verified sender signature, but a sophisticated attacker who knows what's verified could spoof within those constraints. Worse, even rejected sends consume rate-limit budget and pollute logs. Endpoint-configured `from` makes spoofing structurally impossible: the operator's TOML names the exact sender identity for each endpoint, and the JSON body cannot override it.

The compromise lets v1.1 actually serve its named audience (Workers doing transactional sends to end users) while preserving the "operator owns sender identity" invariant. Originally deferred under D5 during FR drafting; reversed (partially) the same day after the Cloudflare Worker recipe surfaced that the deferred shape blocked the named audience. Decided 2026-05-16.

**ADR-12: `Ingress` interface produces a complete `transport.Message`.** With SMTP listener landing as a second ingress shape, the existing HTTP handler is generalized behind a new `Ingress` interface in `core/ingress/`. The contract: an Ingress produces a complete, ready-to-send `transport.Message` (From, To, Subject, BodyText, ReplyTo all populated). HTTP form/api-mode ingress finishes template rendering before yielding the Message; SMTP listener parses MIME directly into one. **HTTP-specific concepts (templates, content negotiation, redirects, idempotency keys, CSRF tokens) do not cross the Ingress boundary** — they're handled inside the HTTP ingress and have no SMTP equivalent. The egress side (`Transport`, retry policy, structured logging) is ingress-agnostic and unchanged. Rationale: keeping the Ingress→Message→Transport pipeline narrow (Message is the only data type that crosses both boundaries) lets v2 add new ingress shapes (programmatic/SDK ingress, webhook-triggered ingress, etc.) without touching transport code. Decided 2026-05-16 during v1.3 spec.

**ADR-13 (revised 2026-09-07 by ADR-26): One SMTP listener hands each message to one Transport.** The revision is narrow: that single Transport may now be a `transport.Router` over a provider pool, so an operator gets failover and weighted splits on the SMTP path exactly as on the HTTP one. What ADR-13 actually forbade — per-RCPT *tenant* routing, where different recipients in one session resolve to different sender identities, DKIM keys and suppression scopes — is unchanged and still deferred. Routing rules match on recipient domain but require every recipient to match, so no session is ever split across providers. Original text follows.

**ADR-13: One SMTP listener has one outbound transport.** The SMTP listener config has a single `[smtp_listener.transport]` block — the same shape as `[endpoints.transport]` — and every accepted message routes through it. Multi-tenant routing-by-RCPT (RCPT TO `@brand-a.com` → Postmark account A; RCPT TO `@brand-b.com` → SES account B) is deferred to v2 alongside the per-tenant config isolation story. Rationale: v1.0 SMTP ingress's named use case is "internal app on a homelab speaks SMTP to a single relay" — Ghost admin login, Gitea notifications, a backup script. None of these need multi-tenant routing; they need "a relay that exists and works." Multi-tenant SMTP routing requires per-tenant `from` (sender identity per branded domain), DKIM signing keys per domain, per-tenant suppression — all of which is v2 platform shape. Adding multi-tenant routing in v1.0 would commit us to those follow-ons. Decided 2026-05-16 during v1.3 spec.

**ADR-14: AWS SES SigV4 implemented bespoke (noted exception to ADR-1).** SES transport requires AWS SigV4 request signing — the most complex auth scheme of any v1.0 transport. We implement SigV4 bespoke (`core/transport/awssigv4.go`) following AWS's published canonical-request specification, then SES on top (`core/transport/ses.go`). Decided 2026-05-16 during v1.2 spec; the original wording set a "stop and surface if total LOC > 500" tripwire. Tripwire fired at implementation time — final LOC: 229 (awssigv4.go) + 264 (ses.go) = 493 implementation, plus 220 + 445 = 665 tests, for 1,158 total.

After the tripwire fired the deviation was accepted (option (a)) for these reasons:

- The "total LOC including tests" tripwire was overly conservative. Test bulk in this codebase is uniformly 400-500 LOC per transport (Postmark, Resend, Mailgun all hit similar bulk) because of the table-driven header-injection suite, status-code-mapping suite, and registry-integration suite each transport gets. Tests scale with quality bar, not transport complexity.
- The honest binding constraint is implementation LOC (493) which is 2.5× ADR-1's "~200 lines suffices" heuristic but comfortably under its "1000+ → use SDK" line.
- Bringing in `github.com/aws/aws-sdk-go-v2/service/sesv2` trades the ~400 LOC of bespoke for a 30+ package transitive dependency tree — net negative against ADR-1's spirit.
- The SigV4 primitive is **reusable** for any future AWS-signed transport (S3-backed storage in v2, SNS lifecycle webhooks in v2). The 229 LOC of SigV4 is forward-value, not single-use cost.
- The implementation is well-tested: NFR1 header-injection table, NFR3 secret-not-in-headers, deterministic signature output across runs, key derivation matches AWS's published example hex value (`c4afb1cc...4a4b9` for the canonical inputs).

The tripwire as written was a useful forcing function — it made us look at the numbers before letting bespoke creep go unchallenged. Future AWS-shaped transports (S3, etc.) inherit the SigV4 primitive at near-zero cost; the 493-LOC budget covers the lifetime cost of all AWS-signed transports in the project, not just SES.

**ADR-15: Prometheus `/metrics` exposition is hand-rolled.** The standard `github.com/prometheus/client_golang` library would pull in `procfs`, `expvar` bridges, descriptor types — likely 30+ transitive packages. The text exposition format itself is stable, plaintext, and ~50 LOC to emit by hand for the v1.0 metrics surface (counters, histograms with fixed buckets, labeled by `endpoint`/`transport`/`error_class`). Parallel to ADR-1: bespoke when the surface is small. v2's broader observability story may justify the dep when histogram buckets become user-configurable or when push-based remote-write enters scope; not v1.0. Decided 2026-05-16 during v1.2 spec.

**ADR-16: CSRF tokens are HMAC-signed timestamps, issued by the operator at form-render time.** Posthorn does not issue cookies (it has no UI surface) and cannot issue tokens at form-load time (it doesn't render forms). The CSRF design instead: operator configures `csrf_secret` in TOML; operator's static-site builder or server-rendered template code computes `_csrf_token = <timestamp>.<HMAC-SHA256(csrf_secret, timestamp)>` at form-render time and embeds it as a hidden input; Posthorn verifies the HMAC and TTL on submit. The secret never crosses to the client. Alternatives rejected: (a) Posthorn-issued tokens via cookie — adds cookie state and a token-fetch round-trip, contradicts "stateless gateway" identity; (b) double-submit cookie pattern — same cookie issue; (c) no CSRF at all — leaves operators with only Origin/Referer + honeypot for defense, weaker than industry baseline. Operators using static-site generators (Hugo, Jekyll, Astro) can compute tokens at build time; SaaS operators with dynamic forms compute them in their own server-rendered template. Form-mode only; api-mode endpoints reject `csrf_secret` at parse time (no browser form rendering involved). Decided 2026-05-16 during v1.2 spec.

**ADR-17: Outbound-SMTP and inbound-SMTP both use stdlib `net/smtp` primitives.** Go's `net/smtp` is marked "frozen, no new features" but is functionally complete for the operations Posthorn needs (DIAL, AUTH PLAIN/LOGIN, STARTTLS, MAIL/RCPT/DATA, QUIT). Outbound-SMTP transport uses `smtp.PlainAuth` + `smtp.NewClient`. Inbound SMTP listener is hand-rolled (no Go stdlib SMTP server exists — `net/smtp` is client-only) but uses `crypto/tls` and `net` directly — same posture as `net/http` is for HTTP. Parallel to ADR-1 / ADR-15: stdlib-first when the surface is small enough. If we hit a real limitation (e.g., XCLIENT for proxy chains, PROXY protocol on the listener), reconsider with an ADR amendment and possibly adopt `github.com/emersion/go-smtp`. Decided 2026-05-16 during v1.2 and v1.3 spec.

**ADR-18: Proof-of-browser tokens are issued by a Posthorn-served challenge endpoint — a deliberate, scoped exception to ADR-16's no-round-trip stance.** The v1.1 proof-of-browser check (#45) needs a token obtainable only by executing page JavaScript, but an HMAC secret cannot ship in client JS. Design: when `[endpoints.proof_of_browser]` is enabled, Posthorn serves a challenge GET endpoint that issues short-lived HMAC-signed timestamp tokens (same token format as ADR-16 CSRF); a small operator-embedded inline script fetches the challenge and injects it as a hidden field before submit. Verification composes with the single-use consumed-token cache (#62) and a minimum-age floor, so harvest-and-replay and instant-POST both fail. The endpoint enforces the endpoint's `allowed_origins` for CORS and is covered by the per-IP rate limiter. Alternatives rejected: (a) operator-rendered seed — requires a server-side render step, which excludes the static-site audience this feature exists to serve (the same structural gap that makes ADR-16 CSRF unusable for them); (b) client-computed token from a public constant — no round-trip, but forgeable by anything that reads the OSS source, and not upgradeable later without breaking deployed forms; the ADR would have to document a check we already know Posthorn-aware bots walk through. Honest threat framing, recorded deliberately: a targeted script can fetch the challenge endpoint directly — the protection defeats bots that do not execute JS or fetch secondary resources (the observed form-blaster class), not adapted bots. It is the no-dependency ladder rung below Turnstile (#33), which remains the escalation for headless-browser adversaries. ADR-16 is unchanged for CSRF tokens: the round-trip exception applies only to proof-of-browser, where no operator-side render step exists to do the issuing. Decided 2026-07-03 during the v1.1 scope conversation.

**ADR-19: HTML bodies render through `html/template`, never `text/template`.** When `body_format = "html"` (FR71), the body template executes under Go's `html/template`, whose contextual auto-escaping makes every submitter value inert in every HTML context (element body, attribute, URL) by construction. This is NFR1's structural-defense pattern applied to HTML: unsafe is impossible, not documented away. The cost is deliberate: a caller cannot pass pre-rendered HTML through a template variable — it renders as literal text. If a real operator need for trusted passthrough materializes, the compatible extension is an explicit per-endpoint `trusted_fields` opt-in with its own ADR (ADR-11's asymmetry logic); it is not built speculatively. Alternative rejected: `text/template` plus documented escaping rules — one forgotten pipe is stored XSS in mail sent from the operator's own domain, and convention-based safety contradicts every other defense in the codebase. The plain-text fallback part is auto-derived from the rendered HTML by a bespoke ~100-LOC converter (no new deps), overridden by an explicit `text_body` template; an HTML-only opt-out ships only on concrete demand. Decided 2026-08-02.

**ADR-20: SQLite via modernc.org/sqlite — the least-bad storage dependency, taken with eyes open.** `core/storage` needs durable, crash-safe, queryable storage; principle 5's own rule (dependency when bespoke would be 1000+ LOC) squarely covers a storage engine. Driver choice: `mattn/go-sqlite3` is CGo and would end the `CGO_ENABLED=0` static multi-arch build story the release pipeline is built on; bbolt is one small readable dep but loses SQL and, critically, stock-`sqlite3`-CLI inspectability of the operator's own data file; a bespoke append-only store re-invents crash-safety and compaction — the exact 1000+ case. modernc.org/sqlite is real SQLite machine-translated to pure Go: the build story is untouched and the file is standard SQLite. Named costs, recorded honestly: it is by far the largest dependency this project has ever taken (a tree of modernc.org support modules against three tidy deps), it is effectively unauditable by reading (transpiled C), and it lands right after the #38/#39/#56 supply-chain hardening pass — version pinning and quarantine behind the `core/storage` interface are the mitigations. Disk posture: `storage.max_size` → SQLite `max_page_count` so Posthorn cannot fill its own disk (a 100%-full disk wedges even DELETE, since the journal needs bytes); the ballast-file pattern for shared-disk exhaustion is recorded here as the known answer if wedged-DB reports ever materialize, and deliberately not shipped. Decided 2026-08-02.

**ADR-21 (revises ADR-5): sync-first retry queue — 200 keeps meaning sent.** ADR-5 chose synchronous send because v1.0 had no storage; v2.0 adds the queue without changing what existing responses mean. The request path is untouched: inline send with FR19-22 retries. Only transient/rate-limited failures that exhaust inline retries enqueue for background retry (exponential backoff, 5 attempts, dead-letter `failed`); terminal failures still 502 immediately and never enqueue. The new state is additive: api-mode gets `202 {"status": "queued"}` exactly where v1.x dropped mail after giving up; the response contract generalizes to "2xx = terminally handled; the body names the outcome (`sent`, `queued`, `suppressed`)". Alternatives rejected: full-async (every request 202s — breaks 200-means-sent everywhere, wrecks form-mode UX and idempotent-replay semantics, drifts toward mail-infrastructure shape); crash-recovery-only queueing (leaves the actual transient-loss pain unsolved). Two subordinate decisions: queued delivery is at-least-once with a documented crash-window duplicate risk (NFR28) — the alternative, dropping ambiguous in-flight rows, silently re-introduces v1.x lossiness; and `log_failed_submissions = false` disables queueing for that endpoint — the operator's "don't persist submitter content on failure" declaration wins over the queue (the ADR-10 footgun rule: no defense an operator thinks is active but isn't). Storage failure degrades to v1.x behavior; mail flow never blocks on the disk (NFR27). Decided 2026-08-02.

**ADR-22: Lifecycle ingestion is Postmark-only, behind a provider-agnostic event shape.** Every provider speaks a different webhook dialect (Postmark: JSON + basic auth; Resend: Svix signing; Mailgun: bespoke signatures; SES: SNS with a subscription handshake — a second AWS integration). v2.0 builds one dialect, chosen by live-validation truth (Postmark is the operator-validated provider), and commits to the normalized event shape as the forward contract: `event` enum, `submission_id`, `endpoint`, `recipient`, `timestamp`, `provider`, with the raw payload nested as explicitly best-effort `provider_data`. Consumers coupling to `provider_data` are on their own; the normalized fields are the API. Other providers slot in on demand as new parsers behind the same shape. Two honest costs, both documented rather than hidden: the transport surface is no longer feature-flat (a per-provider capability matrix enters the docs), and lifecycle features require Posthorn to be reachable by provider webhooks — the deliberately-unreachable homelab deployment keeps everything else and manages suppression via CLI. Decided 2026-08-02.

**ADR-23: Suppression is reason-scoped, and its management surface is a CLI, not an API.** The "global vs per-endpoint" question dissolves under reasons: a hard bounce means the mailbox is dead everywhere and provider reputation is account-wide, so bounces and spam complaints suppress globally; unsubscribe-class reasons (not shipped in v2.0) would be per-endpoint consent withdrawal. Rows carry `(email, reason, source_endpoint, created_at)`, are exempt from retention (forgetting a bounce defeats the feature), and constitute the deliberate stateless-gateway → stateful-platform crossing named in the brief — bounded by storing only what protects the operator's provider account. Management: principle 4's letter is about config, and nothing in the DB overrides TOML, but an HTTP admin API is a new authenticated surface with no current need. `posthorn suppressions list|add|remove` operates on the storage file directly (also the GDPR-erasure path), and the file is stock SQLite — inspectable with standard tooling, the files-over-UIs ethos applied to state. The HTTP admin API is deferred; #51's internal ops bind is its natural future home. Decided 2026-08-02.

**ADR-24 (amends ADR-12): `Message` gains optional `Fields` — the boundary admits what was submitted, not just what the email looks like.** ADR-12's narrow waist was drawn when every sink was an email, making "canonical message" and "rendered email" the same thing. A structured sink (webhook transport, FR88) reveals they are not: consumers want `{"name": ..., "message": ...}`, and rendering destroys that structure before the boundary. Amendment: `Message.Fields map[string][]string`, populated by HTTP ingresses, nil from the SMTP listener (inbound mail has no form fields), ignored by all mail transports, and never interpolated into any header at any layer — NFR1 restated for it explicitly, with guardrail tests proving mail transports don't read it. *(Story 16.1 extension: `Message.SubmissionID` widens the boundary the same way — set by both ingresses and the retry worker, ignored by mail transports. The forcing fact is at-least-once queued delivery: a webhook replay without a stable ID gives receivers nothing to deduplicate on.)* Alternatives rejected: shipping the webhook transport on rendered-body-only (a weak feature that forces receivers to regex prose); delivering webhooks from the ingress side before rendering (keeps the boundary pure by duplicating retry classification, backoff, registry, metrics, and now the queue — more architecture to protect a boundary whose purpose was less architecture). Consequence acknowledged: raw fields now persist in queued submissions, consistent with the payloads-at-rest posture (NFR31). Decided 2026-08-02.

**ADR-25: Attachments are opt-in, fail-closed, and judged by their bytes.** No attachment behavior exists without `[endpoints.attachments]` (NFR25's doctrine: security-relevant surfaces are opt-in, never default-on). The block must name `allowed_types` explicitly — there is no default list, because any default is a silent security decision made on the operator's behalf (ADR-10 tradition). Enforcement sniffs the actual content bytes (stdlib detection); the client-declared MIME type is attacker-controlled and ignored for authorization. Ingress scope: form multipart and api-mode base64 — the two doors with named demand; SMTP-listener MIME extraction is demand-gated (no recipe app attaches files, and nested-multipart parsing is the codebase's riskiest surface). Sizing: `max_count`/`max_total_size` defaults (5 / 10MB) respect the strictest provider cap; queued attachments persist as blobs under `storage.max_size`, which is part of why that cap exists. Decided 2026-08-02.

**ADR-26 (2026-09-07, revises ADR-13): The multi-provider router is itself a `Transport`.** Posthorn v2.1 turns "one endpoint, one provider" into "one endpoint, one route over a named provider pool." The router is implemented as `transport.Router`, which satisfies `transport.Transport` and wraps N pool entries. Alternatives considered and rejected: (a) a new `Router` concept sitting *above* `Transport`, which would have required `gateway.Handler`, `smtp.session`, `transport.SendWithRetry` and `storage.Worker` all to learn about pools; (b) registering `router` as a transport *type* in the registry, which fails because `BuildFunc(settings map[string]any)` has no access to the named pool. Making the router a Transport keeps the change additive: every downstream caller keeps calling `Send` on an interface, and the FR19-22 retry policy composes by wrapping the router — a pool that exhausts on transient errors gets one more full pass after the backoff, still inside the FR22 hard timeout, with no provider dimension added to the retry code. Costs named honestly: a two-provider pool under `SendWithRetry` can make up to four upstream attempts per request (bounded by the 10s timeout), and `SendResult`/`TransportError` grew a `Provider` field so labels and logs can name the entry that actually ran.

**ADR-27 (2026-09-07): Failover is health-class only; terminal errors stop the walk.** `ErrTransient` and `ErrRateLimited` advance to the next provider — they mean "this provider, right now." `ErrTerminal` returns immediately. The rejected alternative (failover on any error) is the intuitive one, and it is wrong for mail: a terminal 4xx is the provider saying *this message* is unacceptable — unverified `From` domain, malformed address, payload over the cap — and offering the same message to every remaining provider converts one rejection into N rejections charged against N sender reputations. The cost of this choice is named: a provider-level terminal failure that is genuinely provider-specific (a revoked API key returning 401) does not fail over, and shows up as a terminal error naming that provider rather than as a silent degradation. That is the trade we want — a revoked key is an operator problem that should be loud, not one the pool papers over indefinitely.

**ADR-28 (2026-09-07): Circuit breakers are per-provider, process-wide, and trip on health-class failures only.** A pool entry is built once per process and shared by every route that names it, so its breaker observes the provider's health across all endpoints rather than per endpoint — an outage noticed on `/api/contact` is already known to `/api/alerts`. Defaults are deliberately forgiving (5 consecutive failures, 30s cooldown, 1 half-open probe): the breaker is a latency optimisation for a gateway whose first doctrine is "mail first" (NFR27), not an availability gate. Terminal errors never count toward the threshold — if they did, one endpoint with a bad `From` domain would take every provider in the pool out of rotation one submission at a time. When every provider in a route has an open circuit the router returns `ErrTransient`, not `ErrTerminal`, so the ADR-21 retry queue holds the message instead of dead-lettering it. `/healthz` reports circuit state but its status code stays 200: restarting the process does not fix an upstream outage, it only drops the mail the queue was holding. Operators alert on `posthorn_provider_circuit_state`.

**ADR-29 (2026-09-07): Quota exhaustion is its own error class, not a health failure.** A provider that has spent its daily allowance is working perfectly; it simply may not be used right now. Folding that into `ErrTransient` would have been one line, and would have been wrong in three ways: the circuit breaker would trip on a healthy provider and hold it out for a cooldown it does not need; `posthorn_provider_skipped_total` would conflate "my provider is broken" with "I am out of allowance", which call for an incident response and a purchasing decision respectively; and the inline FR19-22 retry would spend the request's remaining budget rediscovering that a *daily* limit has not reset in five seconds. So `ErrQuotaExhausted` is a fourth class, the breaker ignores it entirely, and the router checks quota *before* the breaker so an exhausted provider never even produces a health observation. The consequence that needed a decision: quota must not be retried inline but must still be queued, which `IsRetryable` alone could not express. `IsDeferrable` is the new predicate — "a later attempt at this unchanged message could succeed" — and it, not `IsRetryable`, is what the FR78 queue and the retry worker's dead-letter check now ask. HTTP status is 503 rather than 502 where no queue can hold the message: the caller should come back, not give up. No pre-quota config can reach that path, so no existing deployment's status codes change.

**ADR-30 (2026-09-07): Usage is persisted per (provider, window, period), and rollover is a property of the key, not of a scheduled job.** The obvious design — a `used` counter plus a `resets_at` timestamp, zeroed by a midnight task — fails in exactly the case that matters: the process is down at midnight, or the task misfires, and the counter is silently wrong for a whole period with no way to tell. Keying each row by its period string ("2026-09-07", "2026-09") makes a new day a *new row*, so the old count is simply never read again. Nothing has to run at any particular time; a process that was asleep across three midnights reads the correct row on its next query. The in-memory cache carries the period keys alongside the counts for the same reason, and resets a counter whenever the clock's key no longer matches. Period keys are UTC, which is wrong for every provider's own reset window in the same documented, predictable way — an operator sizing a limit leaves headroom for the offset rather than trusting a local-time guess that is right for one provider and wrong for the rest. Only successful sends are recorded: counting attempts would let a broken provider burn a paid allowance by failing. Writes are write-through and best-effort — a failed write costs one message's accounting and is reported through the storage gate, because NFR27 says mail comes before bookkeeping. Without `[storage]` the counters are in-memory only and a restart resets them; that is stated plainly in the docs rather than hidden, since an operator leaning on a monthly free tier needs to know it.

**ADR-31 (2026-09-07): Cost is fixed-point integer millionths, and cost strategies are static orderings.** Prices are parsed once into `int64` millionths of a currency unit and compared as integers forever after. Float comparison would make `cheapest` pick a different provider on different runs when two prices differ in the seventh decimal place, or when the same price reaches the config by two spellings — a routing decision an operator could not explain from reading the TOML. The string form (`cost_per_1000 = "0.10"`) never passes through a float at all; the float form is accepted because it is what an operator writes first, and is quantised deterministically at parse time. `free_first` and `cheapest` are precomputed at route construction (prices are immutable after load) and are stable: ties break on declared order, so the same config always produces the same order. Neither strategy is quota-aware, and deliberately so — an exhausted provider is skipped at send time by the quota gate, so `free_first` walks onto the paid tier for free, with no second mechanism to keep in sync. Posthorn never converts currencies; a pool priced in two currencies is an operator error we cannot detect, and the docs say to normalise first.

**ADR-32 (2026-09-07): Mail kind is a closed five-value enum, and provider groups are a config-layer alias.** `MailKind` (verification, password_reset, notification, important, marketing) is a routing input only — transports ignore it and, like `SubmissionID` and `Fields`, it may never reach a header (NFR1). It is closed rather than free-form because a free-form label is the first step toward the campaign taxonomy Posthorn will not build (design principle 3: that is Listmonk's lane); an operator needing a finer distinction already has `match_field`. The kind is endpoint-configured, overridable per request in api-mode against the same enum — the bounded-choice shape `match_field` already established, where submitter data selects among routes the operator wrote and can never name a provider. Form mode ignores a submitted kind (a browser form's purpose is a property of the endpoint), and the SMTP listener takes its kind from the listener config, because an inbound header is exactly the submitter-controlled value NFR1 exists to keep out of routing. Provider groups were evaluated against "does this break the router's simplicity" and shipped because they do not touch the router at all: a group resolves to the same `[]*Provider` and `Strategy` an inline list produces, so the change is confined to config resolution. Groups share one name space with providers, so `group = "x"` and `providers = ["x"]` can never denote two different routes.

**ADR-33 (2026-09-07, revises ADR-11 for project mode only): a project MAY set `from` per request, bounded by its own domain allowlist.** ADR-11 forbade per-request `from` because a leaked API key would enable irreversible spoofing of any sender. That reasoning is unchanged and still governs plain api-key endpoints. What changes under `[[projects]]` is the blast radius: a project key can only produce a `from` inside that project's `allowed_from_domains` — domains the project already owns — so a leaked key buys an attacker the ability to spoof the victim's own domain, which they could already do with the key by other means. The control that makes this safe is therefore mandatory: `allowed_from_domains` is required and non-empty, there is deliberately no allow-all default, and `default_from` is itself validated against the list at parse time so it cannot be a back door. The endpoint's own `from` is checked too when it is used as a fallback — an endpoint is shared infrastructure and must not become a way for one project to send as another's domain. On the SMTP path the same policy applies after MIME parsing, because the `From` there is submitter-controlled by definition. Rejection is a 403 with `sender_not_allowed`, counted as `posthorn_project_send_total{status="sender_rejected"}`.

**ADR-34 (2026-09-07): one logical message has ONE id, and it is the submission id.** The obvious move — mint a new `posthorn_message_id` alongside the existing submission UUID — was rejected. The submission id is already exactly what the requirement describes: generated once per request, carried into `Message.SubmissionID`, stable across every provider the router walks, and already the join key for the submission log and the retry queue. A second identifier for the same thing would be two names to keep in sync, two things to log, and a permanent question about which one a caller should use. So the submission id IS the Posthorn message id; project-authenticated responses additionally expose it as `message_id` (identical value, `omitempty` so no pre-project body changes), and the new attempt and event tables key on `message_id`. The provider's own id is separate and per-attempt: `message_attempts.provider_message_id` records which provider minted what, and the submission row's `transport_message_id` holds the one that actually delivered. That pair — one Posthorn id, many provider ids — is what makes "Resend timed out, SES delivered" a single readable story.

**ADR-35 (2026-09-07): project usage and provider quota are separate dimensions with separate tables.** They answer different questions — "how much has this tenant sent?" versus "how much of this upstream account's allowance is left?" — and an operator debugging a refused send needs to know which one ran out. Sharing the enforcement machinery would have made that ambiguous, and sharing a table would have made a project's counters move when an unrelated project spent the same provider's quota. So `project_usage` is a plain counter table with success/failure columns and no enforcement at all, and `[[projects]].daily_limit` / `monthly_limit` are RECORDED BUT NOT ENFORCED in this release — named so the config surface is settled, deliberately inert until there is a decision about what a project hitting its own budget should do (refuse? degrade to a cheaper provider? queue?). What the two dimensions DO share is the period-key mechanism from ADR-30, because "a new day is a new row" is the right shape for any period counter and copying it costs nothing.

**ADR-36 (2026-09-07, revises ADR-30's accounting): quota reservation is strict, via an optional `Reserver` interface.** The v2.2 audit asked directly: with `daily_limit = 100`, 99 used, and 100 concurrent sends, what happens? The answer was that all 100 passed `Check` before any called `Record` — a soft quota, and the arithmetic settling correctly afterwards did not make it strict. `Reserver` closes the gap by taking the unit under the same lock that reads it, with a `commit(kept bool)` the caller must always invoke: `commit(true)` keeps and persists, `commit(false)` hands it back, so FR99's "only successful sends count" stays true on disk. `LocalQuota` implements it and the router prefers it; a source that cannot honestly promise atomicity simply does not implement it and keeps the advisory `Check`/`Record` path. The interface is optional rather than mandatory precisely so a remote source is not forced to lie. Honest limit: strictness is per process. NFR26's one-instance-one-writer assumption already governs everything storage-backed, and two Posthorns sharing a provider account remain a soft quota by construction.

**ADR-37 (2026-09-07): remote quota is advisory reconciliation; local usage stays authoritative.** The v2.2 `QuotaSource` interface was audited against a remote implementation and needed no change — `Check`/`Record`/`Snapshot` are exactly what a remote source can answer, and `Reserve` being optional (ADR-36) is what lets one exist without pretending to atomicity. The relationship is deliberately asymmetric: Posthorn's own count of its own successful sends is exact and is what blocks; a provider's usage API lags, rounds, counts differently (messages versus recipients), and includes sends Posthorn never made. So reconciliation may only ever RAISE the local counter, never lower it. Lowering on a lagging read would refund allowance that is already spent, which is the exact failure quota exists to prevent; raising on a stale read costs at most a premature failover to the next provider, which is the cheap direction. Reconciliation never persists — those sends were either already recorded or were made by something else, and writing them would double-count on the next restore. Shipped: the `RemoteUsageFetcher` interface, the `RemoteQuota` reconciler, and a generic `HTTPUsageFetcher` reference implementation. Deliberately NOT shipped: any vendor-specific adapter. Resend, SES, Mailgun, Brevo and SMTP2GO each need their live API read carefully, and an adapter written from memory would feed confident, wrong numbers into a budget.

**ADR-38 (2026-09-07): idempotency reservation moves into the database, scoped to the identity that owns the key.** ADR-8/ADR-9's in-memory in-flight map was correct for its scope but cannot answer the two questions project callers need: it is keyed per endpoint (two tenants generating the same UUID collide), and it has no payload fingerprint (a key reused for a different message replays the wrong response). The project path therefore reserves in SQLite — `INSERT ... ON CONFLICT DO NOTHING` inside a transaction is the atom, so of N concurrent requests carrying one key exactly one sends and the rest read back what the winner wrote. Scope is `project:<name>`, fingerprint is a hash of the CANONICAL payload (sorted parsed fields plus the resolved sender and recipients) rather than the raw bytes, so a caller re-serialising its JSON gets a replay rather than a spurious conflict. A stale `pending` row from a crashed process is reclaimable after two minutes — comfortably longer than the request timeout plus every retry — so one crash does not make a key unusable for a day. Only 2xx/3xx outcomes are recorded as replayable: caching a 5xx would turn a transient failure into a permanent one for the whole TTL. The pre-project in-memory path is untouched, so a deployment with no projects behaves exactly as before (NFR42).

**ADR-39 (2026-09-07): provider webhook dialects normalize behind an adapter interface, and outbound events reuse the existing forwarder.** `ProviderWebhookAdapter` splits verify / parse / normalize so each can fail distinctly and be tested alone, and adapters are the only place a provider's field names may appear — everything downstream sees `DeliveryEvent`. Postmark and Resend ship; both `Verify` methods honestly return nil, because the ingestion endpoint's mandatory basic auth (ADR-22) is the gate and a signature check reimplemented from memory would look like protection without being it. Outbound, project callbacks reuse the endpoint forwarder's queue, backoff and dead-letter rather than growing a second delivery system; the scope key `project:<name>` namespaces them apart from endpoint paths, and a project's webhook wins over the endpoint's because the project is the business owner of the message. Replay protection is by `event_id` and `timestamp` INSIDE the signed body — headers carry them too for convenience, but a header alone is unauthenticated and could be rewritten. The body-only signing scheme is therefore unchanged from v2.0 and existing receivers keep verifying exactly as they do today. A queued redelivery re-derives the same event id from the stored payload, so a receiver's dedupe recognises a retry rather than a new event.

**ADR-40 (2026-09-08, supersedes ADR-39's "Verify honestly returns nil"): each adapter implements the scheme its provider actually publishes, and says so when there is none.** Both schemes were confirmed against the vendors' live documentation on 2026-09-08 rather than written from memory, because a signature check that is subtly wrong is worse than an absent one — it looks like protection.

*Resend* signs with the Standard Webhooks / Svix scheme: headers `svix-id` / `svix-timestamp` / `svix-signature` (the vendor-neutral `webhook-*` aliases are accepted too), signed content `{id}.{timestamp}.{raw body}`, HMAC-SHA256 under the base64-decoded secret with its `whsec_` prefix stripped, base64 signature, sent as a space-delimited list of `v1,<sig>` entries. Verification uses the raw request bytes — decoding and re-encoding the JSON would reorder keys and normalise whitespace and break every signature, which is the mistake the spec explicitly warns about. Unknown version prefixes are skipped rather than rejected so a key rotation can ship two signatures. The timestamp is unix seconds and is rejected beyond five minutes in either direction, matching what Svix enforces on the sending side; comparison is constant-time.

*Postmark does not sign its webhook payloads.* Its documentation states this plainly and recommends HTTP basic auth on the webhook URL plus IP allowlisting instead. So the Postmark adapter implements no signature check, and a `signing_secret` configured for it is a **parse error** rather than a no-op — an operator who sets one believes payloads are being verified, and silently accepting it would leave exactly that false confidence. What it does implement: the endpoint's mandatory basic auth (unchanged from ADR-22, and enforced before the adapter is reached), an optional source-IP allowlist against Postmark's published egress addresses, and payload-shape validation. `X-Forwarded-For` is consulted only when the direct peer is a configured trusted proxy — trusting it unconditionally would let anyone claim a Postmark address with a request header.

The security boundary is stated rather than papered over: **for Postmark, anyone who learns the basic-auth credential can forge events.** The IP allowlist narrows that but does not close it, because it authenticates the network path and not the payload, and IP allowlists are the operator's to maintain when Postmark changes egress. A signature would close it; Postmark does not offer one.

A dialect gets an HTTP route only when the operator has CONFIGURED it. Mounting a route that cannot verify anything would be an unauthenticated door standing open.

**ADR-41 (2026-09-08): project quota refuses; provider quota reroutes.** The two exhaustion cases look similar and call for opposite responses. A provider out of budget means *this upstream* cannot take the message — so the router tries the next one, and only fails when every provider is spent. A project out of budget means *this caller* has spent its own allowance — so no other provider is tried, no circuit is touched, nothing is queued, and the request is refused. Queueing would be actively wrong: holding a verification code for eight hours until midnight is worse for the user than telling the caller now. HTTP is `429` with a stable `project_quota_exhausted` code and the exhausted `window`, chosen over `403` because the condition is temporary and over `503` because nothing is unavailable. SMTP is `452 4.7.1` — a *temporary* failure, because the allowance returns on a calendar boundary and a sending MTA that queues and retries will succeed; a 5xx would tell it to give up permanently. Neither reply is shaped like a provider failure, so a caller can always tell "my own limit" from "the gateway's upstream is broken".

**ADR-42 (2026-09-08): idempotency is reserved before project quota, so a replay costs nothing.** The order is authenticate → sender policy → idempotency reserve/replay → project quota reserve → send → commit/release, and it is load-bearing rather than incidental. A replay returns the recorded response from the idempotency check and never reaches the quota reserve, so a caller retrying a request it already made does not spend a second unit of its allowance — which matters most for the case the feature exists for, a client that retries a verification-code send after a network timeout. Concurrent duplicates are bounded by the same property: the storage-level reservation (ADR-38) admits exactly one, and only that one reaches the quota. Reserving quota first would have been simpler and wrong: a retry storm would drain a tenant's daily budget without sending anything. Dry runs and fully-suppressed sends are also placed after the quota reserve point in the request flow, so neither spends allowance for a message that never leaves.

**ADR-43 (2026-09-08): message delivery state is a projection; events remain the record of fact.** `submissions.delivery_state` answers "what happened to this message" in one indexed read instead of a scan over the event log. It is maintained by event time, not arrival time, so an out-of-order callback cannot move the state backwards, with a rank breaking ties inside the same second — and at equal timestamps a terminal negative outranks a delivery, because a message that was delivered and then complained about is, operationally, a complaint. No event is ever deleted or rewritten to maintain the projection: if the two ever disagree, the events win and the column is rebuildable from them.

**ADR-44 (2026-09-08, supersedes ADR-43's single column and rank): delivery state is projected PER RECIPIENT, under explicit transition rules.** Two defects in the v2.4 projection, both found by audit rather than by a failure.

First, a submission may carry several recipients, and providers disagree about what an event covers. Postmark's documentation states each event "is specific to a single recipient — a message sent to several people produces several events", and its payload carries a `Recipient`. Resend's is the opposite: one event per `email_id` carrying the whole `to` array. Writing either into one submission-level column meant one recipient's bounce silently overwrote another's delivery. State is therefore projected into `recipient_delivery`, and the submission carries an aggregate — `pending` / `delivered` / `partial` / `failed`. `partial` is the case the single column could not express at all. Suppression follows the same scoping: a recipient-scoped bounce suppresses only the address it names, because suppressing every recipient of a multi-recipient message would block addresses that are perfectly deliverable.

Second, `delivery_state_rank` assumed the lifecycle is a linear order, and it is not. A `deferred` webhook delayed past a `delivered` one carries a later timestamp and would have un-delivered the message. The rank is replaced with explicit rules: a terminal event always lands on a non-terminal state whatever the timestamps say; a non-terminal never overrides a terminal; between two terminals **severity** wins and time only breaks ties, so outcomes escalate (`delivered` → `complained`) and never de-escalate (a delivery event arriving after a complaint does not undo it); between two non-terminals the later event wins. Duplicates are idempotent under every branch. The rules are exported as `storage.Supersedes` specifically so the disorder cases can be asserted one by one rather than inferred from a rank table.

**ADR-45 (2026-09-08): a crash settles its orphaned idempotency reservations with the queued answer.** A process killed mid-send leaves a `pending` reservation whose owner is gone. Before this, the caller's retry got `409 in_flight` for the full two-minute pending window — a dead zone in which a user asking for a new verification code is told "already in flight" while the retry queue is in fact about to deliver the first one. Recovery has better information than the dead request did: `recoverInFlight` has just moved the submission into the queue, so the honest answer to a retry is the `202 queued` the original request would have returned. The message id is therefore stored at RESERVE time, not only on completion, so recovery can make that join. Only reservations whose submission actually reached the queue are settled; one whose request died before the submission was persisted has nothing to promise and correctly stays pending until its window expires.

**ADR-46 (2026-09-08): Posthorn's external delivery semantics are AT-LEAST-ONCE, and no configuration makes them exactly-once.** This is stated as a decision because the alternative — implying exactly-once — would be a lie that operators build on.

The ambiguity is structural and lives outside Posthorn. A provider can accept a message and have its HTTP response lost in transit; Posthorn then sees a timeout, classifies it transient (correctly — it cannot distinguish "never arrived" from "arrived, reply lost"), and fails over or retries. The mail goes twice. This was reproduced directly in the RC1 audit: a provider that accepted a message while Posthorn was killed left the submission `sending`, recovery moved it to the retry queue, and the retry sent it again.

What each mechanism actually guarantees, and what none of them do:

- `Idempotency-Key` deduplicates the CLIENT. It stops a caller's own retries from producing several messages, and its reservation is atomic so concurrent duplicates collapse to one. It has no visibility into what the provider did with a request that timed out.
- The FR19-22 retry and ADR-27 failover deliberately prefer a possible duplicate to a possible loss. For transactional mail that is the right trade: a user who receives two verification codes is inconvenienced, one who receives none is blocked.
- The FR78 retry queue and crash recovery are at-least-once by construction (NFR28), and the crash window is exactly the ambiguous case above.

The consequence for callers, which belongs in the docs and not only here: for verification codes and password resets, keep the SAME business token across retries of one logical message. Then a duplicate delivery is two copies of one working code rather than two competing codes, and the ambiguity becomes invisible to the user. Distributed transactions or provider-side dedup APIs would be the only way further, and both are out of scope for a single-binary gateway — the honest fix is a caller-side convention, not machinery.

**ADR-47 (2026-09-08): the provider capability matrix has exactly three values, and `no` means something different from `unsupported`.** "Supports Resend" is three claims — can Posthorn SEND through it, does it ACCEPT that provider's delivery webhooks, and can it VERIFY those webhooks are genuine — and conflating them is how an operator ends up trusting a callback nobody authenticated. Each provider therefore declares each capability separately as `yes`, `no`, or `unsupported`, with no "partial" or "planned". `no` is a Posthorn gap an operator can wait out; `unsupported` is a provider limitation nothing will fix. Every `unsupported` carries a one-sentence reason, because a blank cell reads as a gap. The matrix is enforced against reality at build time: `cmd/posthorn` tests it against both the transport registry and the webhook adapter registry, so a row claiming a webhook with no adapter mounted, or a provider accepting a signing secret it cannot check, fails the build rather than the operator's expectations.

**ADR-48 (2026-09-08): a native provider adapter must beat the generic SMTP relay at something nameable.** Otherwise every vendor gets a bespoke client that is only more code to maintain and more surface to get wrong. The qualifying reasons are concrete: a better provider message id, a webhook, quota information, rate-limit information, synchronous delivery metadata, or an API-specific feature. Brevo qualifies on a reliable per-message id its webhooks carry back — the join the attempt trail and delivery projection are built on, which SMTP cannot offer because the relay chooses what to report at hand-off. Cloudflare is the borderline case and qualifies on synchronous per-recipient delivery status: its REST send reports a permanent bounce at hand-off, hours before a callback would, and it sends no callback at all. It returns no message id, and the adapter reports none rather than inventing one — a synthetic id in the attempt trail is a value no callback can ever join to.

**ADR-49 (2026-09-08): outbound destinations are checked at CONNECT time, not only at parse time.** Three config values — endpoint `webhook_url`, project `webhook_url`, the remote quota URL — plus the webhook transport turn "a string in a file" into "a request Posthorn makes from inside the operator's network". Parse-time validation of the URL is the legible half and catches the obvious cases; it is not the security boundary, because a hostname's resolution can change after the config loads. The `netguard` policy therefore also runs as a `net.Dialer.Control` on the address the resolver actually returned, which catches a public-looking name resolving to loopback and re-checks every redirect hop. The default is deny. `[security] allow_private_webhook_targets` is the documented opt-in for the ordinary self-hosted case where the receiver IS the operator's own app on 127.0.0.1 — scoped to loopback, private and CGNAT, and it never unblocks link-local, because no webhook receiver lives on 169.254.0.0/16 and every cloud metadata service does. Vendor transports are deliberately exempt: their destination is a fixed provider API, and `base_url` exists so tests can point them at a local server.

**ADR-50 (2026-09-08): query scope is a type, not a WHERE clause anyone has to remember.** Project A must never see project B, and "remember to filter" is not a mechanism. Every scoped storage query takes a `Scope` whose fields are unexported, so the only way to read across projects is to call `AdminScope()` by name — a missed filter becomes a compile error or a greppable call site rather than a silent leak. The scope comes from the authenticated credential and nothing else; a `project` query parameter is not read, because a client-supplied tenant name is not a fact. A message owned by another project answers 404 with the same body as one that does not exist, so the endpoint is not an oracle for guessing other tenants' ids. Sub-resources check message visibility FIRST, because `message_attempts` has no project column of its own and joining is exactly the filter that gets forgotten. Message CONTENT is absent from the surface rather than masked: `body_text` and `body_html` appear in no SELECT, so there is nothing to get wrong, and there is deliberately no body or subject search because full-text search would be a reason to keep content queryable.

**ADR-51 (2026-09-08): admin overrides can only SUBTRACT, and that is what lets them coexist with design principle 4.** Config files remain the source of truth, and there is still no runtime surface that can grant anything. An override DISABLES a provider or suspends a project; clearing one restores whatever the config says rather than forcing an enable, so a provider absent from the config cannot be summoned by clearing a flag and a project the config disabled cannot be enabled by an admin. What this buys is the thing an operator actually needs at 3am: a kill switch reachable without an edit, a restart and a deploy. Overrides persist in SQLite so a restart does not silently release something someone turned off during an incident, and a stale override naming something the config no longer defines is reported and skipped rather than treated as an error. A disabled provider is skipped like an open circuit — the route continues to the next candidate — but reports its own error when every candidate is disabled, because the fix is to re-enable rather than to wait out a cooldown.

**ADR-52 (2026-09-08): API key rotation is a config overlap, not an endpoint.** A project may list several keys, all of which authenticate. Rotating is add-the-new-key, restart, migrate callers, remove-the-old-key, restart — both keys work throughout the overlap, so there is no window in which a caller is broken. There is deliberately no runtime endpoint that mints a key: that would grant sending rights without a config change anybody reviews, and it is exactly the runtime mutation surface principle 4 exists to prevent. What Posthorn can honestly record is the moment a changed key SET took effect, so it compares a digest of digests at startup and writes the audit entry then — enough to confirm the set changed, never enough to reconstruct a key. The digest comparison at authentication walks the whole set without an early exit, so timing does not reveal which key was presented, and the admin surface shows the live key count so an overlap nobody finished is visible.

**ADR-53 (2026-09-08): every privileged action writes its audit row BEFORE it takes effect, and aborts if it cannot.** The ordering is the decision. A crash between the row and the change leaves a recorded action that did not happen — visible, checkable, correctable. The reverse leaves a change nobody can trace, which is not recoverable by any amount of care afterwards. Audit rows are exempt from retention pruning, because a trail that ages out is a trail an attacker only has to outwait, and the table has no column for a secret: entries describe WHAT changed, and the suppression-removal entry records a masked address, since the audit log is read far more often than the suppression list it describes.

**ADR-54 (2026-09-08): the admin surface shows masked recipients and never a message body.** The operator running the gateway is not the owner of the mail passing through it. They need to see that a message to a `gmail.com` address bounced; they do not need the address, and an admin surface listing them is a harvestable directory of every user of every tenant. Credentials are not shown at all — not masked on the way out, but absent, because Posthorn does not store them: only digests are retained, and the surface renders a short FINGERPRINT of the digest, enough to see that a key rotated and that two projects hold different keys. The UI is server-rendered with no JavaScript and no external assets under `default-src 'none'`: there is no script to inject into, and the HttpOnly session cookie means something precisely because the page never needs to read it.

**ADR-55 (2026-09-08): `/healthz` and `/readyz` answer different questions and are wired to different signals.** `/healthz` failing means restart me; `/readyz` failing means stop sending me traffic, but restarting will not help. Readiness covers storage being open and writable, an ingress being configured, and the background workers having started — that last one closes a real gap, because between the listener coming up and the queue drainer starting Posthorn will accept work nothing is draining. A PROVIDER being down deliberately does NOT make Posthorn unready: that is precisely what the retry queue, the circuit breaker and failover exist to absorb, and reporting unready would turn mail that would have been queued into mail refused at the door. A degraded disk fails readiness and keeps liveness at 200, because restarting does not fix a disk. Alert on `posthorn_provider_circuit_state` for upstream outages.

**ADR-56 (2026-09-08): backups are taken with `VACUUM INTO`, never by copying files.** The naive backup is wrong for a WAL database in two independent ways: recent writes live in the `-wal` file, so a copy of the `.db` alone silently loses them, and there is no instant at which `cp` sees a consistent set of all three files. `VACUUM INTO` walks the database under a read transaction and writes a fresh, fully-checkpointed single file that needs no sidecars — while the service keeps running, which is the requirement: an operator must not have to stop mail to get a backup, or know about WAL files to get a correct one. The snapshot is verified immediately, so a bad backup surfaces while someone is watching rather than during a restore, when nobody is. Restore is explicitly OFFLINE and refuses a target that looks in use, because replacing the file under a running process leaves it writing through a descriptor to a database nobody can see; it renames the existing database aside rather than deleting it, and clears the stale `-wal`/`-shm` that would otherwise be applied on top of the restored file.

**ADR-57 (2026-09-08): provider configuration reloads by RESTART; there is no hot reload.** Evaluated and declined for v1.0. Hot reload would have to swap a provider's transport, breaker, quota counters and any in-flight sends atomically, and the failure modes are all silent: a message routed to a provider that was replaced mid-send, a breaker whose history is discarded so a failing upstream gets a fresh three strikes, a quota counter reset by a config edit. The cost of the alternative is one restart. Posthorn is a single binary that restarts in well under a second, its retry queue is durable across restarts, in-flight HTTP requests drain on SIGTERM, and admin disables persist — so the operational gap a hot reload would close is already covered by the two things an operator actually reaches for during an incident: the admin kill switch for immediate action, and a restart for a config change. Revisit only if a deployment shape appears where a restart is genuinely unavailable, which single-instance is not.

If you find yourself wanting to deviate from any ADR, update this document with the new decision and rationale before changing code.

## Open architectural questions

These are implementation decisions deferred from the brief and PRD that affect code organization. Each has a recommended answer; final decision is made during the relevant story.

1. **Logging library: `log/slog` (stdlib) vs `zap`.** Recommendation: `slog` (zero deps, stdlib). Decided during Story 4.2.

2. **`trusted_proxies` syntax in v1.0.** Decision: CIDR-only. Named presets (`cloudflare`, etc.) are planned for v1.2 (see brief §"Post-MVP Vision"). Confirmed during Story 3.2.

3. **Body template — file path vs inline detection.** Recommendation: heuristic — if the value contains `{{` it's inline; otherwise it's a file path. Reject ambiguity at validation time. Decided during Story 2.4. **v2.0 extension (Story 13.1):** markup-shaped values (containing both `<` and `>`) are also inline — static HTML bodies have no template vars but their closing tags contain `/`, and no real filesystem path is markup-shaped.

4. **Response body for 502 terminal failures.** Recommendation: 502 response body says "Submission could not be delivered. Please try again later." with no detail. Detail is in the operator's logs. Avoiding leaking whether the failure was config (4xx from upstream) vs runtime (network) to a potential attacker. Decided during Story 4.1.

5. **JSON body type coercion in API mode (v1.1).** The form-mode pipeline produces `map[string][]string` and templates render strings. JSON request bodies can contain numbers, booleans, arrays, and nested objects. Recommendation: coerce primitive types to strings (`42` → `"42"`, `true` → `"true"`) using `fmt.Sprintf` and reject nested objects with HTTP 400 ("nested JSON objects are not supported in v1.1"). Top-level arrays of primitives serialize as comma-joined strings (parallel to form-mode multi-value fields). Decided during Story 8.3.

## Appendix: TOML grammar reference

For Story 2.1 implementation. Single-endpoint shape:

```toml
[[endpoints]]
path = "/api/contact"
to = ["craig@example.com"]
from = "Contact Form <noreply@example.com>"

trusted_proxies = ["10.0.0.0/8", "192.168.0.0/16"]
honeypot = "_gotcha"
allowed_origins = ["https://example.com"]
max_body_size = "32KB"

required = ["name", "email", "message"]
email_field = "email"     # default: "email"

subject = "Contact from {{.name}}"
body = "templates/contact.txt"   # file path; inline allowed if value contains "{{"

log_failed_submissions = true

redirect_success = "/thank-you"
redirect_error = "/contact?error=true"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
# base_url = "https://api.postmarkapp.com"  # test-only, undocumented

[endpoints.rate_limit]
count = 5
interval = "1m"

[logging]
level = "info"
format = "json"   # only json supported in v1.0
```

For multiple endpoints, repeat the `[[endpoints]]` block. Each subsequent `[endpoints.transport]`, `[endpoints.transport.settings]`, and `[endpoints.rate_limit]` table applies to the most recent `[[endpoints]]` entry.
