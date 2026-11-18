# Posthorn

<!-- Modified for the cryotx/posthorn distribution. -->

[简体中文](README.md) · English · [日本語](README.ja.md) · [한국어](README.ko.md) · [Español](README.es.md)

[![CI](https://github.com/cryotx/Posthorn/actions/workflows/ci.yml/badge.svg)](https://github.com/cryotx/Posthorn/actions/workflows/ci.yml)
[![Docs](https://img.shields.io/badge/docs-posthorn.dev-d4a300)](https://posthorn.dev)
[![License: Apache-2.0](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](./LICENSE)
[![Go version](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](https://go.dev)

This edition is based on [craigmccaskill/posthorn](https://github.com/craigmccaskill/posthorn) and is distributed under the original Apache-2.0 license. It adds a multilingual admin dashboard, project API key management, and encrypted storage for provider credentials. The [Compose guide](deploy/compose/README.md) runs this edition from source. Links to posthorn.dev refer to the upstream project.

The admin dashboard supports Simplified Chinese, English, Japanese, Korean, and Spanish. Choose a language on the sign-in page or in the dashboard header.

**The unified outbound mail layer for self-hosted projects.** One gateway between every app you self-host — or every project you run — and the transactional mail providers you've already picked. Three ingress shapes (HTTP form, HTTP API, SMTP), six transports (Postmark, Resend, Mailgun, AWS SES, outbound-SMTP relay, webhook), and a provider pool that fails over, splits by weight, routes by rule, and spends your free tier before your paid one. Single Go binary, single TOML config.

**Real-world stacks:** [Hugo + Comentario](https://posthorn.dev/recipes/hugo-comentario/) · [Ghost](https://posthorn.dev/recipes/ghost/) · [Gitea](https://posthorn.dev/recipes/gitea/) · [Umami digest cron](https://posthorn.dev/recipes/umami/) · [Cloudflare Worker](https://posthorn.dev/recipes/cloudflare-worker/)

## Why

Nobody wants to run a mail server in 2026. Self-hosted operators use Postmark, Resend, Mailgun, or AWS SES because they're cheap, they handle deliverability properly, and someone else worries about SPF / DKIM / DMARC / bounces / sender reputation.

But every app you self-host has to integrate with that service **independently.** Your contact form. Your Ghost blog's admin emails. Your Gitea magic links. Your Mastodon notifications. The Cloudflare Worker that fires a password-reset email when someone clicks the link. Each one needs its own copy of the API key, its own integration code, its own quirks around retry and bounce handling. **The same outbound concern duplicated across your stack.**

And on cloud hosts that block outbound SMTP — DigitalOcean, AWS Lightsail, Linode, Vultr — the SMTP-only apps don't work at all without a workaround.

Posthorn is the bridge. One container, one config, one set of credentials. Your apps point at Posthorn. Posthorn talks to your provider.

| Where your app connects | What Posthorn does |
|---|---|
| HTTP form (contact forms, signups, alert webhooks) | Honeypot + Origin/Referer + rate limit + optional CSRF; templates the email; sends |
| HTTP API mode (workers, cron, payment handlers, internal services) | `Authorization: Bearer` auth; JSON body; idempotent retries; per-request `to_override` for transactional sends |
| SMTP listener (Ghost, Gitea, Mastodon, Matrix, NextCloud, Authentik, anything that emits SMTP) | AUTH PLAIN or client-cert; STARTTLS-required; sender + recipient allowlists; parses MIME; forwards via HTTP API transport |

All three ingresses converge on one `transport.Message` and one outbound route — a single provider, or a pool that fails over between several.

## What Posthorn is not

To save you a wrong turn:

| | What it does | Look at instead |
|---|---|---|
| **Not a mail server** | No mailbox storage, no IMAP/JMAP, no DKIM key management, no MX target | [Stalwart](https://stalw.art), [Mailcow](https://mailcow.email), [iRedMail](https://www.iredmail.org) |
| **Not its own outbound infrastructure** | Posthorn relays through a provider you chose; it doesn't run its own SMTP fleet or manage IP reputation | [Postal](https://postalserver.io), [Hyvor Relay](https://hyvor.com/relay) |
| **Not a marketing email platform** | No list management, no segmentation, no campaign dashboard | [Listmonk](https://listmonk.app) |
| **Not webmail / a mailbox UI** | No interface for reading mail | Roundcube, Snappymail (with a mail server) |

The wedge is **the integration layer** between your self-hosted apps and the transactional provider you've already picked.

## Documentation

**[posthorn.dev](https://posthorn.dev)** — getting started, configuration reference, deployment guides, feature deep-dives, security model, HTTP API reference, FAQ. Ten recipes covering contact forms, newsletter signups, multi-form sites, monitoring alerts, Cloudflare Workers, internal SMTP relay (Docker Compose), and full case studies for Hugo+Comentario, Ghost, Gitea, and self-hosted Umami digests.

For project history and the v1.0 spec, see [`spec/`](./spec/).

## Quick start (Docker)

Follow the [Docker Compose guide](deploy/compose/README.md) to build the local image, generate a client key, and preview an API request. The example binds to loopback and starts in dry-run mode. The same guide covers delivery with your own sender, admin access and database backups.

## API mode (server-to-server)

For Workers, cron jobs, internal services — anything that speaks JSON instead of forms:

```toml
[[endpoints]]
path = "/api/transactional"
to = ["fallback@yourdomain.com"]
from = "YourApp <noreply@yourdomain.com>"
auth = "api-key"
api_keys = ["${env.WORKER_KEY_PRIMARY}", "${env.WORKER_KEY_BACKUP}"]
required = ["subject_line", "message"]
subject = "{{.subject_line}}"
body = "{{.message}}"

[endpoints.transport]
type = "postmark"

[endpoints.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
```

```bash
curl -X POST https://posthorn.yourdomain.com/api/transactional \
  -H "Authorization: Bearer $WORKER_KEY_PRIMARY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: reset:user-123:$(date -u +%FT%H)" \
  --data '{
    "to_override": "alice@example.com",
    "subject_line": "Reset your password",
    "message": "Click here: https://app.example.com/reset/abc"
  }'
```

Calling from Python or JavaScript with no dependencies: [docs/calling-posthorn.md](./docs/calling-posthorn.md).

Full walkthrough: [posthorn.dev/recipes/cloudflare-worker](https://posthorn.dev/recipes/cloudflare-worker/).

## SMTP listener (Ghost / Gitea / Mastodon / Authentik)

For apps that speak SMTP natively and can't be reconfigured to call an HTTP API:

```toml
[smtp_listener]
listen          = ":2525"
require_tls     = true
tls_cert        = "/etc/posthorn/cert.pem"
tls_key         = "/etc/posthorn/key.pem"
auth_required   = "smtp-auth"
allowed_senders = ["*@yourdomain.com"]
max_recipients_per_session = 10
max_message_size = "1MB"

[[smtp_listener.smtp_users]]
username = "ghost"
password = "${env.GHOST_SMTP_PASSWORD}"

[smtp_listener.transport]
type = "postmark"

[smtp_listener.transport.settings]
api_key = "${env.POSTMARK_API_KEY}"
```

Point Ghost (or any app's SMTP config) at `posthorn.yourdomain.com:2525` with the username/password above. Posthorn parses the MIME, builds a `transport.Message`, forwards via Postmark.

Full doc: [posthorn.dev/features/smtp-ingress](https://posthorn.dev/features/smtp-ingress/).

## Picking a transport

| Transport | Best for | Auth | Body |
|---|---|---|---|
| **Postmark** | Transactional email, strong deliverability defaults | `X-Postmark-Server-Token` | JSON |
| **Resend** | Modern HTTP API, developer-friendly dashboard | `Authorization: Bearer` | JSON |
| **Mailgun** | Higher-volume transactional, US + EU regions | HTTP Basic | multipart/form-data |
| **AWS SES** | AWS-native deployments, cheapest at volume | AWS SigV4 (bespoke) | JSON |
| **Outbound SMTP** | Any STARTTLS-capable relay (Mailtrap, your Postfix smarthost, etc.) | AUTH PLAIN | SMTP DATA |
| **Webhook** | A non-mail sink you run — the structured submission as a signed JSON POST | HMAC signature | JSON |

Switching providers is a TOML edit — every transport implements the same `Transport` interface. See [posthorn.dev/configuration/transports](https://posthorn.dev/configuration/transports/) for per-provider config.

## Multi-provider routing

You don't have to pick just one. Declare your upstreams once as a named pool, then let an endpoint route across them:

```toml
[[providers]]
name = "postmark-prod"
type = "postmark"
[providers.settings]
api_key = "${env.POSTMARK_API_KEY}"

[[providers]]
name = "ses-backup"
type = "ses"
[providers.settings]
region            = "us-east-1"
access_key_id     = "${env.AWS_ACCESS_KEY_ID}"
secret_access_key = "${env.AWS_SECRET_ACCESS_KEY}"

[endpoints.transport]
strategy  = "failover"                       # or round_robin / weighted
providers = ["postmark-prod", "ses-backup"]

# Optional: send some mail somewhere else. First match wins.
[[endpoints.transport.rules]]
match_recipient_domain = ["gmail.com", "*.edu"]
providers              = ["mailgun-bulk", "ses-backup"]
```

What you get:

- **Failover on the errors that mean it.** Transient failures and 429s walk to the next provider. A terminal 4xx does not — that is the provider rejecting *this message*, and shopping it around the pool would spend N sender reputations on one bad submission.
- **A circuit breaker per provider,** shared process-wide, so a dead upstream is skipped instead of rediscovered on every send. Terminal errors never trip it.
- **Routing rules** on recipient domain or submitted field values. Submitter data picks between routes you wrote; it can never name a provider.
- **Per-provider observability** — `posthorn_provider_attempts_total`, `posthorn_provider_circuit_state`, `posthorn_route_failover_total`, a `provider` field on `submission_sent`, and circuit state on `/healthz`.
- **`dry_run = true`** answers "where would this message go?" without sending, since rules are the one thing you can't read off the TOML.

Annotated config: [`examples/multi-provider.toml`](./examples/multi-provider.toml). Full docs: [provider pool](https://posthorn.dev/configuration/providers/) and [routing & failover](https://posthorn.dev/features/routing/). Existing single-transport configs keep working unchanged.

## Free tier first, paid tier when it runs out

Most self-hosted operators are not on one provider by choice — they are on one provider's free tier, with a paid account behind it for the month that tier runs out. Posthorn manages that boundary:

```toml
[[providers]]
name = "resend-free"
type = "resend"
cost_per_1000 = 0
[providers.settings]
api_key = "${env.RESEND_API_KEY}"
[providers.quota]
daily_limit   = 100
monthly_limit = 3000

[[providers]]
name = "ses-paid"
type = "ses"
cost_per_1000 = "0.10"
[providers.settings]
region            = "us-east-1"
access_key_id     = "${env.AWS_ACCESS_KEY_ID}"
secret_access_key = "${env.AWS_SECRET_ACCESS_KEY}"

[endpoints.transport]
strategy  = "free_first"        # or "cheapest"
providers = ["resend-free", "ses-paid"]
```

Every message goes to Resend until the free allowance is gone, then to SES, then back to Resend when the window resets.

- **Only successful sends count.** A provider that failed didn't spend your allowance.
- **Rollover needs no cron.** Counters are keyed by period, so a new day is a new key — a Posthorn that was down at midnight still rolls over correctly on its next send.
- **Usage survives restarts** when `[storage]` is configured (in-memory only without it, and the docs say so).
- **Running out is not a failure.** Quota is checked *before* the circuit breaker and never trips it: a free tier that empties every afternoon keeps a closed circuit all month. When every provider is out of budget the message is queued until the window resets, not dropped.
- **Money is fixed-point,** never float — `cheapest` can't pick differently between runs on a rounding difference.
- **Route by what the mail is:** `mail_kind` (`verification`, `password_reset`, `notification`, `important`, `marketing`) with `match_kind` rules, so a password reset takes the premium provider and a digest takes the free pool.

This is for managing allowances you legitimately have. Posthorn has no multi-account rotation and nothing that helps evade a provider's limits or anti-abuse controls.

Annotated config: [`examples/quota-cost-routing.toml`](./examples/quota-cost-routing.toml). Full docs: [quota & cost routing](https://posthorn.dev/features/quota-and-cost/).

## One gateway, many projects

Run more than one thing? Give each project its own key and its own sender policy, and share the provider accounts, the retry queue and the deliverability work between them:

```toml
[[projects]]
name                 = "novel"
api_key              = "${env.NOVEL_MAIL_API_KEY}"
default_from         = "noreply@novel.example.com"
allowed_from_domains = ["novel.example.com"]
webhook_url          = "https://novel.example.com/hooks/mail"
webhook_secret       = "${env.NOVEL_WEBHOOK_SECRET}"

[[projects]]
name                 = "account"
api_key              = "${env.ACCOUNT_MAIL_API_KEY}"
default_from         = "account@example.com"
allowed_from_domains = ["example.com"]
```

```bash
curl -X POST https://posthorn.example.com/api/send   -H "Authorization: Bearer $NOVEL_MAIL_API_KEY"   -H "Idempotency-Key: verify:user-4821"   -H "Content-Type: application/json"   --data '{"to_override":"reader@example.net","subject_line":"Confirm your email","message":"..."}'
```

What that buys you:

- **No project can send as another's domain.** `allowed_from_domains` is required and enforced server-side on both HTTP and SMTP — there is no allow-all default, because one would let any tenant impersonate every other.
- **Idempotency that actually holds under concurrency.** Keys are scoped `(project, key)` and reserved atomically in SQLite: a hundred simultaneous requests carrying one key produce exactly one email. Same key with a different payload is a `409`, not a silent wrong replay.
- **One message id per logical email,** stable across provider failover, with a durable per-attempt trail — `resend timed out → ses delivered` under one id, with each provider's own id recorded beside it.
- **Delivery events your app can actually consume.** Postmark and Resend webhooks are *verified* — Resend by its real Svix signature over the raw body, Postmark by the basic auth plus IP allowlist it actually supports (it does not sign payloads, and Posthorn says so rather than faking a check) — then normalized to one shape (`message.delivered`, `message.bounced`, …) and forwarded, HMAC-signed, to each project's own callback. Redeliveries are collapsed. An unverified callback gets a 401 and changes nothing.
- **Per-project send budgets.** `daily_limit` / `monthly_limit` are enforced strictly: over-limit requests get `429 project_quota_exhausted` — refused, not rerouted to another provider and not queued. A retried request replays without spending a second unit.
- **API keys are hashed, never stored or logged.** Neither are SMTP passwords, webhook secrets, or message bodies.
- **SMTP too.** A project with `smtp_username` / `smtp_password` gets the same identity and the same sender policy through the SMTP door.

Annotated config: [`examples/projects.toml`](./examples/projects.toml). Full docs: [projects](https://posthorn.dev/features/projects/). No `[[projects]]` blocks means none of this exists and Posthorn behaves exactly as before.

## Production checklist

Before pointing real traffic at Posthorn:

1. **DNS** — SPF, DKIM, and DMARC records on your sending domain. Without these your mail goes to spam. See [posthorn.dev/security/dns](https://posthorn.dev/security/dns/).
2. **Reverse proxy** — Posthorn does not terminate TLS. Run it behind Caddy, nginx, or Traefik. See [posthorn.dev/deployment/reverse-proxy](https://posthorn.dev/deployment/reverse-proxy/).
3. **`allowed_origins`** (form-mode endpoints) — set this to lock submissions to your domain. Without it, anyone can POST to your endpoint.
4. **`rate_limit`** — set a tight bucket per endpoint (5/minute is a sensible default for a public contact form; API mode rate-limits per matched key).
5. **`trusted_proxies`** — if behind a reverse proxy, list its CIDR (or use the `cloudflare` named preset) so the rate limiter sees the real client IP.
6. **`/healthz` and `/metrics`** — auto-registered on the same listener. Wire your Docker healthcheck or Prometheus scrape to these.

The full operator checklist is on [posthorn.dev](https://posthorn.dev).

## What's in v1.0

| Block | Detail |
|---|---|
| **Form ingress** | Form-encoded + multipart bodies; honeypot, Origin/Referer fail-closed, rate limit, optional CSRF tokens |
| **API mode** | `auth = "api-key"` with Bearer tokens (constant-time compare); JSON content type; idempotency keys (24h, in-memory LRU); per-request `to_override` |
| **Transports** | Postmark, Resend, Mailgun, AWS SES (bespoke SigV4), outbound-SMTP relay |
| **SMTP listener** | TCP listener with AUTH PLAIN / client-cert, STARTTLS-required, sender + recipient allowlists, size cap, MIME → `transport.Message` |
| **Operations** | `/healthz`, `/metrics` (Prometheus exposition), dry-run mode, IP-stripping, named `trusted_proxies` presets (Cloudflare) |
| **Failure handling** | 1 retry on transient/5xx (1s), 1 retry on 429 (5s), 10s hard timeout |
| **Logging** | Structured JSON; UUIDv4 submission IDs and SMTP session IDs; `transport_message_id` in `submission_sent` |
| **Deployment** | Single Go binary, distroless Docker image built from the source in this repository |

Three external Go dependencies for the v1.0 surface — TOML parser, UUID library, LRU cache — plus a pure-Go SQLite driver for the optional v2 storage spine. Every transport is bespoke: no vendor SDK in transport code.

## Roadmap

**v2 — platform maturity.** SQLite submission log, retry queue across restarts, suppression list (auto on hard bounces), durable idempotency, lifecycle event callbacks via HMAC-signed webhook, file attachments, HTML body, webhook transport — and the multi-provider gateway above (pool, failover, weighted splits, routing rules, circuit breakers). Still ahead: RFC 8058 one-click unsubscribe, multiple outputs per endpoint (email + webhook + log fan-out), multi-tenant SMTP routing.

**v3 — speculative.** Proof-of-work spam challenge, PGP encryption. Depends on community traction.

Full trajectory: [posthorn.dev/roadmap](https://posthorn.dev/roadmap/).

## Build from source

Requires Go 1.25+.

```bash
git clone https://github.com/cryotx/Posthorn
cd Posthorn/core
go build -o /tmp/posthorn ./cmd/posthorn
/tmp/posthorn version
```

## Ecosystem

Posthorn ships no official client SDK; the HTTP API is the supported integration surface and needs no client library. Community-built clients and tools are listed in [docs/ecosystem.md](./docs/ecosystem.md) and are third-party, not endorsed by Posthorn.

## Contributing

The v1.0 specification is in [`spec/`](./spec/) (brief, PRD, architecture). The architecture doc at [`spec/03-architecture.md`](./spec/03-architecture.md) is the source of truth for design questions.

Security issues: see [SECURITY.md](./SECURITY.md) — do **not** open public issues for security disclosures.

## License

Apache-2.0. See [LICENSE](./LICENSE).
