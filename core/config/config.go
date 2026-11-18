// Modified for the cryotx/posthorn distribution.

// Package config defines the Posthorn configuration schema and TOML loader.
//
// Configuration flows: TOML file → resolveEnvVars → toml.Unmarshal → Validate.
// The Config struct is the single source of truth for runtime behavior.
// Validation runs at load time (FR24) so operators get fast feedback
// rather than runtime surprises.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/craigmccaskill/posthorn/csrf"
	"github.com/craigmccaskill/posthorn/lifecycle"
	"github.com/craigmccaskill/posthorn/netguard"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/transport"
)

// Config is the top-level configuration object.
type Config struct {
	// Providers is the top-level named provider pool (FR93). Each entry
	// is one credential set for one upstream; endpoints and the SMTP
	// listener reference entries by name instead of repeating the
	// credentials in every block. Optional — a config that keeps the
	// v1.x inline [endpoints.transport] shape defines none.
	Providers []ProviderConfig `toml:"providers"`

	// Projects is the optional multi-tenant layer (FR107): several
	// business projects sharing one gateway, each with its own API key
	// and sender policy. Absent means project mode is off and every
	// ingress keeps its pre-project behavior (NFR42).
	Projects []ProjectConfig `toml:"projects"`

	// ProviderGroups are optional named provider lists (FR105) that
	// transport blocks and rules can reference by name instead of
	// repeating the list. Share the name space with Providers.
	ProviderGroups []ProviderGroupConfig `toml:"provider_groups"`

	Endpoints    []EndpointConfig    `toml:"endpoints"`
	Logging      LoggingConfig       `toml:"logging"`
	SMTPListener *SMTPListenerConfig `toml:"smtp_listener"`
	Storage      *StorageConfig      `toml:"storage"`
	Lifecycle    *LifecycleConfig    `toml:"lifecycle"`

	// Security holds the deployment-wide safety switches. Optional; the
	// zero value is the strict default in every case.
	Security SecurityConfig `toml:"security"`
}

// SecurityConfig is the optional top-level [security] block.
//
// One block rather than a flag on each URL: the question "may Posthorn
// call things inside this network?" is a property of the DEPLOYMENT, not
// of an individual webhook, and answering it per-URL invites answering
// it inconsistently.
type SecurityConfig struct {
	// AllowPrivateWebhookTargets permits Posthorn's outbound calls —
	// endpoint and project webhook_url, the webhook transport, and the
	// remote quota endpoint — to reach loopback and private-network
	// addresses (NFR45).
	//
	// Default false: a URL in a config file otherwise turns Posthorn
	// into a confused deputy against services on its own network, which
	// is the SSRF an operator cannot see happening.
	//
	// Set it to true for the ordinary self-hosted case where the
	// receiver IS the operator's own app on 127.0.0.1. It never
	// unblocks link-local (169.254.0.0/16, fe80::/10) — that is where
	// every cloud metadata service lives and no webhook receiver does.
	AllowPrivateWebhookTargets bool `toml:"allow_private_webhook_targets"`
}

// OutboundPolicy is the SSRF policy this config implies, for every
// component that makes an operator-configured outbound request.
func (c *Config) OutboundPolicy() netguard.Policy {
	return netguard.Policy{AllowPrivate: c.Security.AllowPrivateWebhookTargets}
}

// LifecycleConfig is the optional top-level [lifecycle] block (FR82,
// ADR-22). Presence enables the Postmark event-ingestion endpoint
// (`/events/postmark`) on the main HTTP listener. Requires [storage]
// (event correlation reads the submission log) — enforced in
// Config.Validate. Basic auth is mandatory and fail-closed: Posthorn's
// first inbound-from-a-third-party surface never runs open (NFR25).
type LifecycleConfig struct {
	// BasicAuthUsername/BasicAuthPassword protect the event endpoint.
	// Postmark supports setting both on the webhook URL natively. The
	// password falls under NFR3/NFR29: never logged.
	BasicAuthUsername string `toml:"basic_auth_username"`
	BasicAuthPassword string `toml:"basic_auth_password"`

	// Providers configures per-dialect webhook authentication (FR120),
	// keyed by provider name: [lifecycle.providers.resend].
	Providers map[string]ProviderWebhookConfig `toml:"providers"`
}

// ProviderWebhookConfig is one provider dialect's inbound webhook
// authentication.
//
// What belongs here differs per provider because their published
// schemes differ, and Posthorn implements what each one actually offers
// rather than a uniform pretence:
//
//   - resend signs with the Standard Webhooks / Svix scheme, so it
//     takes signing_secret.
//   - postmark and brevo do NOT sign their payloads, so they take
//     allowed_ips and lean on the endpoint's mandatory basic auth.
//     Setting signing_secret for either is a parse error rather than a
//     no-op — a defense an operator believes is active but isn't is
//     worse than no defense.
type ProviderWebhookConfig struct {
	// SigningSecret is the provider-issued webhook secret. Never
	// logged (NFR29).
	SigningSecret string `toml:"signing_secret"`

	// AllowedIPs restricts which source addresses may post, for
	// providers that publish a fixed egress set instead of signing.
	// Accepts addresses and CIDRs. Empty means no source restriction.
	AllowedIPs []string `toml:"allowed_ips"`

	// TrustedProxies are the CIDRs of reverse proxies in front of
	// Posthorn, so the source check can read X-Forwarded-For. Without
	// these, only the direct peer address is trusted — trusting the
	// header unconditionally would make allowed_ips forgeable.
	TrustedProxies []string `toml:"trusted_proxies"`

	// Tolerance overrides the signature timestamp window. Zero means
	// the provider's own default (5 minutes for Svix).
	Tolerance Duration `toml:"tolerance"`
}

// Validate runs structural checks on one provider webhook block.
func (p *ProviderWebhookConfig) Validate() error {
	if p.Tolerance.Std() < 0 {
		return fmt.Errorf("tolerance: must be non-negative, got %v", p.Tolerance.Std())
	}
	return nil
}

// Validate runs structural checks on the lifecycle block.
func (l *LifecycleConfig) Validate() error {
	if l.BasicAuthUsername == "" || l.BasicAuthPassword == "" {
		return errors.New("basic_auth_username and basic_auth_password are required (the event endpoint never runs unauthenticated)")
	}
	return nil
}

// StorageConfig is the optional top-level [storage] block (FR76,
// ADR-20/21). Nil after parse means every storage-dependent v2.0
// behavior is disabled and the pipeline is byte-identical to v1.x
// (NFR25: presence-activates, never default-on).
type StorageConfig struct {
	// Path is the SQLite file location. Required unless InMemory.
	Path string `toml:"path"`

	// InMemory runs a private in-memory database — tests and dry
	// evaluation only; nothing survives a restart.
	InMemory bool `toml:"in_memory"`

	// Retention bounds how long completed submission rows live (FR79).
	// Default 30d. Suppressions are exempt by design (ADR-23).
	Retention Duration `toml:"retention"`

	// MaxSize caps the database file size (FR79) so Posthorn can never
	// fill the disk on its own. Parsed like max_body_size ("1GB",
	// "256MB"). Default 1GB.
	MaxSize string `toml:"max_size"`
}

// Storage defaults applied by EffectiveRetention/EffectiveMaxSize.
const (
	DefaultStorageRetention = 30 * 24 * time.Hour
	DefaultStorageMaxSize   = "1GB"
)

// EffectiveRetention returns the configured retention or the 30d default.
func (s *StorageConfig) EffectiveRetention() time.Duration {
	if s == nil || s.Retention.Std() <= 0 {
		return DefaultStorageRetention
	}
	return s.Retention.Std()
}

// EffectiveMaxSize returns the configured cap string or the 1GB default.
func (s *StorageConfig) EffectiveMaxSize() string {
	if s == nil || s.MaxSize == "" {
		return DefaultStorageMaxSize
	}
	return s.MaxSize
}

// Validate runs structural checks on the storage block.
func (s *StorageConfig) Validate() error {
	if s.Path == "" && !s.InMemory {
		return errors.New("path is required (or set in_memory = true for tests)")
	}
	if s.Path != "" && s.InMemory {
		return errors.New("path and in_memory are mutually exclusive")
	}
	if s.Retention.Std() < 0 {
		return errors.New("retention must be positive")
	}
	return nil
}

// SMTPListenerConfig is the top-level [smtp_listener] block (FR62).
// When non-nil after parse, cmd/posthorn starts an SMTP ingress
// alongside the HTTP one. Lives in this package to avoid a config →
// smtp circular dependency; the smtp package converts it to its
// internal ListenerConfig shape.
type SMTPListenerConfig struct {
	Listen string `toml:"listen"`

	// RequireTLS forces STARTTLS upgrade before AUTH / MAIL / RCPT.
	// A *bool (per ADR-4 precedent) so we can distinguish unset (default
	// true — TLS-required is the safe production posture documented in
	// marketing) from explicit `require_tls = false` (operator opted into
	// plaintext, only sensible for local development).
	RequireTLS *bool `toml:"require_tls"`

	TLSCert                 string          `toml:"tls_cert"`
	TLSKey                  string          `toml:"tls_key"`
	ClientCertCA            string          `toml:"client_cert_ca"`
	AuthRequired            string          `toml:"auth_required"`
	SMTPUsers               []SMTPUser      `toml:"smtp_users"`
	AllowedSenders          []string        `toml:"allowed_senders"`
	AllowedRecipients       []string        `toml:"allowed_recipients"`
	MaxRecipientsPerSession int             `toml:"max_recipients_per_session"`
	MaxMessageSize          string          `toml:"max_message_size"`
	IdleTimeout             Duration        `toml:"idle_timeout"`
	Transport               TransportConfig `toml:"transport"`

	// TrustedNetwork acknowledges that an auth_required = "none"
	// listener bound to a non-loopback/non-private address is reachable
	// only from a trusted network (Docker bridge with no port exposure,
	// VPN, firewall). Without it, that combination is a parse error —
	// an unauthenticated listener on a public bind is an open relay
	// gated only by the sender allowlist (#41).
	TrustedNetwork bool `toml:"trusted_network"`

	// MaxConnections caps concurrent SMTP connections across all
	// clients; MaxConnectionsPerIP caps them per remote IP (#50).
	// 0 = defaults (100 / 16). Excess connections get 421 and close.
	MaxConnections      int `toml:"max_connections"`
	MaxConnectionsPerIP int `toml:"max_connections_per_ip"`

	// MailKind labels everything this listener accepts (FR104). SMTP
	// carries no per-message hint Posthorn could trust, so the kind is
	// a property of the listener.
	MailKind string `toml:"mail_kind"`
}

// SMTPUser is a single AUTH PLAIN credential pair.
type SMTPUser struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// Auth mode values for EndpointConfig.Auth. Empty / unset is equivalent to
// AuthForm (FR45 — v1.0 configs unchanged).
const (
	AuthForm   = "form"
	AuthAPIKey = "api-key"
)

// Body format values for EndpointConfig.BodyFormat. Empty / unset is
// equivalent to BodyFormatText (FR71 — v1.x configs unchanged).
const (
	BodyFormatText = "text"
	BodyFormatHTML = "html"
)

// EndpointConfig configures one ingress endpoint. Multiple endpoints in one
// Config are independent — no shared rate-limit budget, no cross-endpoint
// state (FR2).
//
// Two modes:
//   - Form mode (default; v1.0 behavior): browser POSTs form-encoded bodies,
//     defended by honeypot / Origin / rate limit / max-body-size.
//   - API-key mode (v1.1): server-to-server callers POST JSON bodies with
//     Authorization: Bearer <key>; browser defenses do not apply (FR31, FR32).
type EndpointConfig struct {
	Path              string           `toml:"path"`
	To                []string         `toml:"to"`
	From              string           `toml:"from"`
	Transport         TransportConfig  `toml:"transport"`
	RateLimit         *RateLimitConfig `toml:"rate_limit"`
	TrustedProxies    []string         `toml:"trusted_proxies"`
	Honeypot          string           `toml:"honeypot"`
	AllowedOrigins    []string         `toml:"allowed_origins"`
	MaxBodySize       string           `toml:"max_body_size"` // e.g. "32KB"; parsed at handler-construction time
	Required          []string         `toml:"required"`
	EmailField        string           `toml:"email_field"`
	ReplyToEmailField string           `toml:"reply_to_email_field"`
	Subject           string           `toml:"subject"`
	Body              string           `toml:"body"`

	// v2.0 block E: HTML body (FR71, FR72, ADR-19). BodyFormat selects
	// how Body renders: "text" (default; v1.x behavior) or "html", which
	// renders through html/template with contextual auto-escaping —
	// submitter values are inert markup by construction. TextBody is an
	// optional explicit plain-text template for the multipart fallback
	// part; when unset, the text part is auto-derived from the rendered
	// HTML. TextBody without BodyFormat = "html" is a parse error.
	BodyFormat string `toml:"body_format"`
	TextBody   string `toml:"text_body"`

	LogFailedSubmissions *bool  `toml:"log_failed_submissions"`
	RedirectSuccess      string `toml:"redirect_success"`
	RedirectError        string `toml:"redirect_error"`

	// v1.1: API mode. Auth selects the endpoint shape; empty defaults to
	// AuthForm preserving v1.0 behavior (FR31, FR45). APIKeys is the list
	// of valid Bearer tokens when Auth is AuthAPIKey (FR33). Multiple keys
	// support rotation. IdempotencyCacheSize sets the per-endpoint cache
	// capacity (FR42); zero or unset means use the package default.
	Auth                 string   `toml:"auth"`
	APIKeys              []string `toml:"api_keys"`
	IdempotencyCacheSize int      `toml:"idempotency_cache_size"`

	// MailKind labels what this endpoint sends (FR104): verification,
	// password_reset, notification, important or marketing. Empty means
	// unspecified. Routing rules can match on it; transports ignore it.
	// api-mode callers may override it per request against the same
	// closed enum.
	MailKind string `toml:"mail_kind"`

	// v1.0 block C: dry-run mode (FR56). When true, the handler runs the
	// full pipeline up to but not including transport.Send and returns
	// 200 with a JSON body containing the prepared transport.Message.
	// Operators use this to debug template rendering and recipient
	// resolution without sending mail.
	DryRun bool `toml:"dry_run"`

	// v1.0 block C: GDPR-shaped IP-stripping option (FR59). When true,
	// the resolved client IP is omitted from all log lines for this
	// endpoint. Rate-limit keying is unaffected — IP is still computed
	// internally; it just doesn't reach the log surface.
	StripClientIP bool `toml:"strip_client_ip"`

	// v1.0 block C: CSRF (FR57, ADR-16). When CSRFSecret is non-empty,
	// the handler requires a `_csrf_token` form field on every form-mode
	// submission and verifies its HMAC-SHA256 signature against
	// CSRFSecret. Tokens older than CSRFTokenTTL (default 1h) are
	// rejected with 403. Form-mode only — api-mode endpoints reject
	// csrf_secret at parse time.
	CSRFSecret   string   `toml:"csrf_secret"`
	CSRFTokenTTL Duration `toml:"csrf_token_ttl"`

	// Reputation: optional StopForumSpam lookup on the submitter email/IP
	// (form-mode only). Content-agnostic — targets repeat form-spam
	// identities. See ReputationConfig.
	Reputation *ReputationConfig `toml:"reputation"`

	// ProofOfBrowser: optional JS-gated submit token (form-mode only,
	// #45/ADR-18). Blocks bots that POST directly without executing the
	// page JavaScript that fetches the token. See ProofOfBrowserConfig.
	ProofOfBrowser *ProofOfBrowserConfig `toml:"proof_of_browser"`

	// Captcha: optional Cloudflare Turnstile verification (form-mode only,
	// #33). The escalation tier — stops bots that render JS. See
	// CaptchaConfig.
	Captcha *CaptchaConfig `toml:"captcha"`

	// v2.0 block E: file attachments (FR90, ADR-25). Opt-in: without
	// this block, form-mode file parts are dropped exactly as v1.x and
	// api-mode attachments are rejected. See AttachmentsConfig.
	Attachments *AttachmentsConfig `toml:"attachments"`

	// projectMode is set by Config.Validate when [[projects]] exist. It
	// is not a TOML field: an endpoint cannot opt itself into project
	// authentication, because that is a property of the deployment.
	projectMode bool

	// v2.0 block E: lifecycle callbacks (FR84). When WebhookURL is set,
	// normalized delivery/bounce/complaint events for mail sent through
	// this endpoint are POSTed there, signed with HMAC-SHA256 of the
	// body under WebhookSecret (X-Posthorn-Signature: sha256=<hex>).
	// Requires the top-level [lifecycle] block — a webhook_url with no
	// event ingestion would never fire, which is exactly the
	// looks-configured-but-isn't footgun ADR-10 exists to prevent.
	// WebhookSecret falls under NFR3/NFR29: never logged.
	WebhookURL    string `toml:"webhook_url"`
	WebhookSecret string `toml:"webhook_secret"`
}

// AttachmentsConfig configures opt-in file attachments
// (`[endpoints.attachments]`, FR90/FR91, ADR-25). AllowedTypes is
// deliberately required with no default: any default list is a silent
// security decision made on the operator's behalf. Enforcement runs
// against the sniffed content type of the actual bytes — the
// client-declared type is attacker-controlled and ignored for
// authorization.
type AttachmentsConfig struct {
	// AllowedTypes is the accepted content-type allowlist. Exact
	// ("application/pdf") or family wildcard ("image/*"). Matched
	// against the sniffed type, so only sniffable types are usable —
	// text-family formats (CSV etc.) sniff as text/plain.
	AllowedTypes []string `toml:"allowed_types"`

	// MaxCount bounds files per submission. Default 5.
	MaxCount int `toml:"max_count"`

	// MaxTotalSize bounds the combined decoded size ("10MB" default).
	MaxTotalSize string `toml:"max_total_size"`
}

// Attachment defaults.
const (
	DefaultAttachmentMaxCount  = 5
	DefaultAttachmentTotalSize = "10MB"
)

// EffectiveMaxCount returns MaxCount or the default.
func (a *AttachmentsConfig) EffectiveMaxCount() int {
	if a == nil || a.MaxCount <= 0 {
		return DefaultAttachmentMaxCount
	}
	return a.MaxCount
}

// EffectiveMaxTotalSize returns MaxTotalSize or the default.
func (a *AttachmentsConfig) EffectiveMaxTotalSize() string {
	if a == nil || a.MaxTotalSize == "" {
		return DefaultAttachmentTotalSize
	}
	return a.MaxTotalSize
}

// Validate runs structural checks on the attachments block (ADR-25
// fail-closed rules).
func (a *AttachmentsConfig) Validate() error {
	if len(a.AllowedTypes) == 0 {
		return errors.New("allowed_types: required — attachments are fail-closed, name the types you accept (e.g. [\"application/pdf\", \"image/*\"])")
	}
	for i, t := range a.AllowedTypes {
		parts := strings.Split(t, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" || t != strings.ToLower(t) {
			return fmt.Errorf("allowed_types[%d]: %q is not a lowercase type/subtype or type/* pattern", i, t)
		}
	}
	if a.MaxCount < 0 {
		return errors.New("max_count must be positive")
	}
	return nil
}

// CaptchaConfig configures the optional captcha check
// (`[endpoints.captcha]`). Form-mode only.
type CaptchaConfig struct {
	// Provider is the captcha service. Only "turnstile" in this release.
	Provider string `toml:"provider"`

	// SecretKey is the provider secret used for server-side verification.
	// Never sent to the client.
	SecretKey string `toml:"secret_key"`

	// OnProviderError selects behavior when the provider can't be reached:
	// "closed" (default) rejects the submission; "open" allows it. A
	// captcha that fails open is weak, so closed is the default; fail-open
	// events increment posthorn_check_failed_open_total{check="captcha"}.
	OnProviderError string `toml:"on_provider_error"`

	// Timeout per verification. Default 3s.
	Timeout Duration `toml:"timeout"`

	// BaseURL overrides the siteverify endpoint (testing / self-hosted).
	BaseURL string `toml:"base_url"`
}

// ProofOfBrowserConfig configures the proof-of-browser check
// (`[endpoints.proof_of_browser]`). Its presence enables the check.
// Posthorn serves a challenge token from a GET on the endpoint; the
// operator embeds a small script that fetches it and injects it as the
// `_pob_token` field before submit. Form-mode only (ADR-18).
type ProofOfBrowserConfig struct {
	// Secret is the HMAC key for the challenge token. Optional: when
	// empty, Posthorn generates a random one at startup (fine for a
	// single replica). Set it explicitly for multi-replica deployments so
	// every replica verifies the same tokens; ≥16 bytes when set.
	Secret string `toml:"secret"`

	// TTL is the token lifetime. Default 30m.
	TTL Duration `toml:"ttl"`

	// MinAge, when set, rejects a token submitted sooner than this after
	// it was issued — a time-trap for bots that fetch-then-submit
	// instantly. Must be less than the effective TTL.
	MinAge Duration `toml:"min_age"`
}

// ReputationConfig configures the optional reputation check
// (`[endpoints.reputation]`). Form-mode only.
type ReputationConfig struct {
	// Provider is the reputation source. Only "stopforumspam" in v1.
	Provider string `toml:"provider"`

	// BaseURL overrides the provider endpoint. Empty uses StopForumSpam's
	// public API; set it to a compatible mirror or a privacy proxy.
	BaseURL string `toml:"base_url"`

	// Check lists which fields to look up: "email", "ip", or both.
	// Non-empty; unknown entries are a parse error.
	Check []string `toml:"check"`

	// Confidence is the block threshold (0–100). A looked-up field that
	// appears in the database with confidence ≥ this blocks the
	// submission. Default 90.
	Confidence float64 `toml:"confidence"`

	// FailOpen: on provider error/timeout, allow (true, default) or block
	// (false). Fail-open keeps a provider outage from blocking real mail.
	FailOpen *bool `toml:"fail_open"`

	// Timeout per lookup. Default 2s.
	Timeout Duration `toml:"timeout"`

	// CacheSize / CacheTTL bound the in-memory result cache. Defaults
	// 10000 / 1h.
	CacheSize int      `toml:"cache_size"`
	CacheTTL  Duration `toml:"cache_ttl"`
}

// validate checks the reputation block. Called only for form-mode
// endpoints (api-mode rejects the whole block above).
func (r *ReputationConfig) validate() error {
	if r.Provider != "stopforumspam" {
		return fmt.Errorf("provider: only \"stopforumspam\" is supported, got %q", r.Provider)
	}
	if len(r.Check) == 0 {
		return errors.New("check: list at least one of \"email\", \"ip\"")
	}
	for _, c := range r.Check {
		if c != "email" && c != "ip" {
			return fmt.Errorf("check: unknown value %q (want \"email\" or \"ip\")", c)
		}
	}
	if r.Confidence < 0 || r.Confidence > 100 {
		return fmt.Errorf("confidence: must be 0–100, got %v", r.Confidence)
	}
	if r.Timeout.Std() < 0 {
		return fmt.Errorf("timeout: must be non-negative, got %v", r.Timeout.Std())
	}
	if r.CacheSize < 0 {
		return fmt.Errorf("cache_size: must be non-negative, got %d", r.CacheSize)
	}
	if r.CacheTTL.Std() < 0 {
		return fmt.Errorf("cache_ttl: must be non-negative, got %v", r.CacheTTL.Std())
	}
	return nil
}

// validate checks the proof_of_browser block. Called only for form-mode
// endpoints (api-mode rejects the whole block above).
func (p *ProofOfBrowserConfig) validate() error {
	if p.Secret != "" {
		if err := csrf.ValidateSecret([]byte(p.Secret)); err != nil {
			return err
		}
	}
	if p.TTL.Std() < 0 {
		return fmt.Errorf("ttl: must be non-negative, got %v", p.TTL.Std())
	}
	if p.MinAge.Std() < 0 {
		return fmt.Errorf("min_age: must be non-negative, got %v", p.MinAge.Std())
	}
	// Resolve the effective TTL for the min_age < ttl check (default 30m).
	ttl := p.TTL.Std()
	if ttl == 0 {
		ttl = 30 * time.Minute
	}
	if p.MinAge.Std() >= ttl {
		return fmt.Errorf("min_age (%v) must be less than ttl (%v)", p.MinAge.Std(), ttl)
	}
	return nil
}

// validate checks the captcha block. Called only for form-mode endpoints.
func (c *CaptchaConfig) validate() error {
	if c.Provider != "turnstile" {
		return fmt.Errorf("provider: only \"turnstile\" is supported, got %q", c.Provider)
	}
	if c.SecretKey == "" {
		return errors.New("secret_key: required")
	}
	switch c.OnProviderError {
	case "", "closed", "open":
		// ok; empty means closed
	default:
		return fmt.Errorf("on_provider_error: must be \"closed\" or \"open\", got %q", c.OnProviderError)
	}
	if c.Timeout.Std() < 0 {
		return fmt.Errorf("timeout: must be non-negative, got %v", c.Timeout.Std())
	}
	return nil
}

// ProviderConfig is one entry in the top-level [[providers]] pool
// (FR93). It is the same (type, settings) pair the inline transport
// block has always carried, plus the identity and health knobs that
// only make sense once several providers coexist.
//
// Pool entries are built once per process and shared by every route
// that names them, so two endpoints pointing at "postmark-prod" share
// one HTTP client and — more importantly — one circuit breaker. Health
// is a property of the provider, not of the endpoint that noticed.
type ProviderConfig struct {
	// Name is the pool key referenced from [endpoints.transport] and
	// [smtp_listener.transport]. Required and unique. It reaches logs
	// and metric labels, so it must stay operator content (NFR24).
	Name string `toml:"name"`

	// Type is the registered transport type: postmark, resend, mailgun,
	// ses, smtp, webhook.
	Type string `toml:"type"`

	// Settings is the transport-specific configuration, validated by
	// the registered transport's own validator.
	Settings map[string]any `toml:"settings"`

	// Weight is the relative share under strategy = "weighted".
	// Unset or 0 means 1. Ignored by the other strategies.
	Weight int `toml:"weight"`

	// CircuitBreaker overrides this provider's health thresholds.
	// Omitted means defaults (5 consecutive health-class failures, 30s
	// cooldown, 1 half-open probe).
	CircuitBreaker *CircuitBreakerConfig `toml:"circuit_breaker"`

	// Quota is this provider's send budget (FR99). Omitted means
	// unlimited.
	Quota *QuotaConfig `toml:"quota"`

	// QuotaSource selects how usage is counted. Only "local" is
	// implemented; "remote" is a named, reserved value that parse
	// rejects rather than silently ignores.
	QuotaSource string `toml:"quota_source"`

	// CostPer1000 is the price per 1000 messages, used by the
	// free_first and cheapest strategies (FR101). Fixed-point; see
	// Cost. Zero (the default) means free.
	CostPer1000 Cost `toml:"cost_per_1000"`
}

// Validate runs structural checks on one pool entry and dispatches
// settings validation to the registered transport.
func (p *ProviderConfig) Validate() error {
	if p.Name == "" {
		return errors.New("name is required (it is how endpoints reference this provider)")
	}
	if strings.TrimSpace(p.Name) != p.Name {
		return fmt.Errorf("name %q: leading/trailing whitespace", p.Name)
	}
	if p.Type == "" {
		return errors.New("type is required (e.g., \"postmark\")")
	}
	reg, ok := transport.Lookup(p.Type)
	if !ok {
		return transport.UnknownTypeError(p.Type)
	}
	if p.Weight < 0 {
		return fmt.Errorf("weight: must be non-negative, got %d", p.Weight)
	}
	if p.CircuitBreaker != nil {
		if err := p.CircuitBreaker.Validate(); err != nil {
			return fmt.Errorf("circuit_breaker: %w", err)
		}
	}
	if p.Quota != nil {
		if err := p.Quota.Validate(); err != nil {
			return fmt.Errorf("quota: %w", err)
		}
	}
	switch p.QuotaSource {
	case "", QuotaSourceLocal:
		// ok
	case QuotaSourceRemote:
		return errors.New("quota_source = \"remote\" is reserved but not implemented — Posthorn cannot read this provider's usage API yet; use \"local\" (the default), which counts Posthorn's own successful sends")
	default:
		return fmt.Errorf("quota_source: must be %q (only implemented source), got %q", QuotaSourceLocal, p.QuotaSource)
	}
	if p.CostPer1000.V < 0 {
		return fmt.Errorf("cost_per_1000: must be non-negative, got %s", p.CostPer1000.V)
	}
	return reg.Validate(p.Settings)
}

// CircuitBreakerConfig tunes one provider's breaker
// ([providers.circuit_breaker], FR96).
type CircuitBreakerConfig struct {
	// FailureThreshold is the count of consecutive health-class
	// failures (transient / rate-limited) that opens the circuit.
	// Terminal errors never count — they describe the message, not the
	// provider. 0 means the default (5).
	FailureThreshold int `toml:"failure_threshold"`

	// Cooldown is how long an open circuit stays out of rotation before
	// admitting a probe. 0 means the default (30s).
	Cooldown Duration `toml:"cooldown"`

	// HalfOpenProbes is how many concurrent probes the half-open state
	// admits. 0 means the default (1).
	HalfOpenProbes int `toml:"half_open_probes"`

	// Disabled turns the breaker off: this provider is never skipped,
	// however it is behaving. For operators who prefer paying the
	// timeout to ever passing a provider over.
	Disabled bool `toml:"disabled"`
}

// Validate runs structural checks on the breaker block.
func (c *CircuitBreakerConfig) Validate() error {
	if c.FailureThreshold < 0 {
		return fmt.Errorf("failure_threshold: must be non-negative, got %d", c.FailureThreshold)
	}
	if c.Cooldown.Std() < 0 {
		return fmt.Errorf("cooldown: must be non-negative, got %v", c.Cooldown.Std())
	}
	if c.HalfOpenProbes < 0 {
		return fmt.Errorf("half_open_probes: must be non-negative, got %d", c.HalfOpenProbes)
	}
	return nil
}

// Cost wraps transport.Cost with TOML decoding, following the Duration
// precedent in this file. Accepts a decimal string (exact), an integer
// (whole currency units) or a float (quantised to six decimal places).
//
// A string is the recommended form and the only one that is exact by
// construction: `cost_per_1000 = "0.10"`.
type Cost struct{ V transport.Cost }

// UnmarshalTOML implements toml.Unmarshaler.
func (c *Cost) UnmarshalTOML(v any) error {
	parsed, err := transport.ParseCost(v)
	if err != nil {
		return err
	}
	c.V = parsed
	return nil
}

// QuotaConfig is one provider's send budget (`[providers.quota]`,
// FR99). Both fields default to 0, which means unlimited — so a config
// that never mentions quota behaves exactly as it did before (NFR32).
type QuotaConfig struct {
	// DailyLimit bounds successful sends per UTC calendar day.
	DailyLimit int64 `toml:"daily_limit"`

	// MonthlyLimit bounds successful sends per UTC calendar month.
	MonthlyLimit int64 `toml:"monthly_limit"`
}

// Validate runs structural checks on the quota block.
func (q *QuotaConfig) Validate() error {
	if q.DailyLimit < 0 {
		return fmt.Errorf("daily_limit: must be non-negative (0 means unlimited), got %d", q.DailyLimit)
	}
	if q.MonthlyLimit < 0 {
		return fmt.Errorf("monthly_limit: must be non-negative (0 means unlimited), got %d", q.MonthlyLimit)
	}
	if q.DailyLimit > 0 && q.MonthlyLimit > 0 && q.DailyLimit > q.MonthlyLimit {
		return fmt.Errorf("daily_limit (%d) exceeds monthly_limit (%d) — the daily budget could never be spent in full",
			q.DailyLimit, q.MonthlyLimit)
	}
	return nil
}

// Quota source values for ProviderConfig.QuotaSource.
const (
	// QuotaSourceLocal counts Posthorn's own successful sends. The only
	// implemented source.
	QuotaSourceLocal = "local"

	// QuotaSourceRemote would read the provider's own usage API. Named
	// so the config surface is settled, and deliberately rejected at
	// parse time until one exists — a silently-ignored quota_source
	// would be exactly the looks-configured-but-isn't footgun ADR-10
	// exists to prevent.
	QuotaSourceRemote = "remote"
)

// ProjectConfig is one [[projects]] block (FR107) — a business tenant
// of the gateway: an identity, a credential, and a sender policy.
//
// The API key is read from here exactly once, at startup, and retained
// only as a SHA-256 digest (NFR44). Use ${env.VAR} so the plaintext
// never lives in the config file either.
type ProjectConfig struct {
	// Name identifies the project in logs, metric labels and events.
	Name string `toml:"name"`

	// Enabled gates authentication. A *bool (ADR-4 precedent) so unset
	// means enabled — an operator who writes a project intends it to
	// work — while `enabled = false` suspends it without deleting it.
	Enabled *bool `toml:"enabled"`

	// APIKey authenticates HTTP callers as this project via
	// `Authorization: Bearer <key>`. Never logged (NFR3).
	APIKey string `toml:"api_key"`

	// APIKeys holds ADDITIONAL keys that authenticate as this project,
	// which is how a key is rotated with no downtime (FR141):
	//
	//	1. add the new key here, restart — both keys now work;
	//	2. move callers to the new key at their own pace;
	//	3. remove the old key from api_key (promoting the new one),
	//	   restart — the old key stops working.
	//
	// There is deliberately no runtime rotation endpoint: keys live in
	// the config file, and an admin surface that could mint one would
	// be a way to grant sending rights without a config change anybody
	// reviews. Every key must meet the same length floor, and no two
	// projects may share one.
	APIKeys []string `toml:"api_keys"`

	// DefaultFrom is the sender used when a request names none. Must
	// itself satisfy AllowedFromDomains.
	DefaultFrom string `toml:"default_from"`

	// AllowedFromDomains is the anti-spoofing allowlist. Required and
	// non-empty — see the project package for why there is no
	// allow-all default.
	AllowedFromDomains []string `toml:"allowed_from_domains"`

	// DailyLimit and MonthlyLimit are RESERVED: recorded, surfaced in
	// usage counters, and deliberately not enforced in this release.
	// They are a different dimension from [providers.quota] — one
	// bounds a tenant, the other an upstream account (ADR-35).
	DailyLimit   int64 `toml:"daily_limit"`
	MonthlyLimit int64 `toml:"monthly_limit"`

	// WebhookURL receives this project's normalized delivery events,
	// signed with WebhookSecret. The secret is never logged (NFR29).
	WebhookURL    string `toml:"webhook_url"`
	WebhookSecret string `toml:"webhook_secret"`

	// SMTPUsername and SMTPPassword give this project an SMTP AUTH
	// identity on the shared listener, so an SMTP submission produces
	// the same project context an HTTP request does (FR111).
	SMTPUsername string `toml:"smtp_username"`
	SMTPPassword string `toml:"smtp_password"`
}

// EffectiveEnabled resolves the *bool: unset means enabled.
func (p *ProjectConfig) EffectiveEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}

// ProjectConfigs converts the parsed blocks into the project package's
// input shape. Structural validation lives there, next to the digests.
func (c *Config) ProjectConfigs() []project.Config {
	if len(c.Projects) == 0 {
		return nil
	}
	out := make([]project.Config, 0, len(c.Projects))
	for _, p := range c.Projects {
		out = append(out, project.Config{
			Name:               p.Name,
			Enabled:            p.EffectiveEnabled(),
			APIKey:             p.APIKey,
			APIKeys:            p.APIKeys,
			DefaultFrom:        p.DefaultFrom,
			AllowedFromDomains: p.AllowedFromDomains,
			DailyLimit:         p.DailyLimit,
			MonthlyLimit:       p.MonthlyLimit,
			WebhookURL:         p.WebhookURL,
			WebhookSecret:      p.WebhookSecret,
			SMTPUsername:       p.SMTPUsername,
			SMTPPassword:       p.SMTPPassword,
		})
	}
	return out
}

// ProviderGroupConfig is a named, reusable provider list
// (`[[provider_groups]]`, FR105). It exists so the same list — "the
// free pool", "the premium pool" — can be referenced from several
// endpoints and rules without being retyped, which is where a
// half-applied edit would otherwise creep in.
//
// A group resolves to exactly the same []*Provider and Strategy an
// inline list produces, so the router never learns that groups exist.
type ProviderGroupConfig struct {
	// Name is the group key. Must be unique AND must not collide with
	// a provider name — one namespace, so `group = "x"` and
	// `providers = ["x"]` can never mean two different things.
	Name string `toml:"name"`

	// Providers is the pool names this group stands for, in priority
	// order.
	Providers []string `toml:"providers"`

	// Strategy is the group's default strategy. A referencing block may
	// override it.
	Strategy string `toml:"strategy"`
}

// Validate runs structural checks on one group.
func (g *ProviderGroupConfig) Validate() error {
	if g.Name == "" {
		return errors.New("name is required")
	}
	if len(g.Providers) == 0 {
		return errors.New("providers: at least one provider name required")
	}
	if _, err := transport.ParseStrategy(g.Strategy); err != nil {
		return fmt.Errorf("strategy: %w", err)
	}
	seen := map[string]bool{}
	for i, n := range g.Providers {
		if n == "" {
			return fmt.Errorf("providers[%d]: empty provider name", i)
		}
		if seen[n] {
			return fmt.Errorf("providers[%d]: duplicate provider %q", i, n)
		}
		seen[n] = true
	}
	return nil
}

// RouteRuleConfig is one conditional route inside a transport block
// ([[endpoints.transport.rules]], FR95). Rules are evaluated in
// declared order and the first match wins; a message matching no rule
// takes the block's top-level providers list.
//
// Populated criteria AND together; values within one criterion OR.
type RouteRuleConfig struct {
	// MatchRecipientDomain matches the domain of EVERY recipient
	// against these patterns — exact ("gmail.com") or suffix wildcard
	// ("*.edu"). Requiring all recipients to match keeps one submission
	// on one provider; per-recipient splitting would be fan-out, which
	// is deliberately not in scope.
	MatchRecipientDomain []string `toml:"match_recipient_domain"`

	// MatchField matches submitted fields against operator-listed
	// values. Submitter data chooses between routes the operator wrote;
	// it never names a provider.
	MatchField map[string][]string `toml:"match_field"`

	// MatchKind matches the message's mail kind (FR104) — one of
	// verification, password_reset, notification, important,
	// marketing. This is the criterion that expresses "verification
	// goes to the fast provider, notifications go to the free pool".
	MatchKind []string `toml:"match_kind"`

	// Strategy and Providers describe the route taken on a match. An
	// empty Strategy means "failover". Group names a
	// [[provider_groups]] entry instead of listing providers inline;
	// the two are mutually exclusive.
	Strategy  string   `toml:"strategy"`
	Providers []string `toml:"providers"`
	Group     string   `toml:"group"`
}

// Validate runs structural checks on one rule. Name resolution against
// the pool happens later, in Config.Validate, which is the only place
// that can see both the rule and the [[providers]] list.
func (r *RouteRuleConfig) Validate() error {
	if len(r.MatchRecipientDomain) == 0 && len(r.MatchField) == 0 && len(r.MatchKind) == 0 {
		return errors.New("a rule needs at least one of match_recipient_domain, match_field or match_kind (a rule matching everything would shadow the default route)")
	}
	if len(r.Providers) > 0 && r.Group != "" {
		return errors.New("providers and group are mutually exclusive — a route is one list, name it once")
	}
	if len(r.Providers) == 0 && r.Group == "" {
		return errors.New("providers: at least one provider name required (or reference a group)")
	}
	for i, k := range r.MatchKind {
		if _, err := transport.ParseMailKind(k); err != nil {
			return fmt.Errorf("match_kind[%d]: %w", i, err)
		}
		if k == "" {
			return fmt.Errorf("match_kind[%d]: empty kind — omit match_kind instead", i)
		}
	}
	if _, err := transport.ParseStrategy(r.Strategy); err != nil {
		return fmt.Errorf("strategy: %w", err)
	}
	for i, d := range r.MatchRecipientDomain {
		if err := validateDomainPattern(d); err != nil {
			return fmt.Errorf("match_recipient_domain[%d]: %w", i, err)
		}
	}
	for field, vals := range r.MatchField {
		if field == "" {
			return errors.New("match_field: empty field name")
		}
		if len(vals) == 0 {
			return fmt.Errorf("match_field[%s]: at least one value required", field)
		}
	}
	return nil
}

// validateDomainPattern rejects patterns that would silently never
// match. Domains are compared lowercase, so an uppercase pattern is a
// config bug rather than a case-insensitive convenience.
func validateDomainPattern(p string) error {
	if p == "" {
		return errors.New("empty pattern")
	}
	if p != strings.ToLower(p) {
		return fmt.Errorf("%q must be lowercase (domains are compared lowercase)", p)
	}
	if strings.ContainsAny(p, "@ \t") {
		return fmt.Errorf("%q is a domain pattern, not an address", p)
	}
	bare := strings.TrimPrefix(p, "*.")
	if bare == "" || strings.Contains(bare, "*") {
		return fmt.Errorf("%q: only a leading \"*.\" wildcard is supported", p)
	}
	return nil
}

// TransportConfig is the polymorphic transport block, in either of two
// shapes:
//
//	# inline (v1.x, unchanged): one provider, credentials right here
//	[endpoints.transport]
//	type = "postmark"
//	[endpoints.transport.settings]
//	api_key = "..."
//
//	# pooled (FR94): several providers by name, with a strategy
//	[endpoints.transport]
//	strategy  = "failover"
//	providers = ["postmark-prod", "ses-backup"]
//
// The two are mutually exclusive at parse time, in the ADR-10 spirit:
// a block carrying both a type and a providers list would leave the
// operator guessing which one is live.
type TransportConfig struct {
	Type     string         `toml:"type"`
	Settings map[string]any `toml:"settings"`

	// Providers names pool entries, in priority order for the failover
	// strategy. Non-empty selects the pooled shape.
	Providers []string `toml:"providers"`

	// Strategy picks the first provider for each message: "failover"
	// (default), "round_robin", or "weighted". Failover order after
	// that first pick is always the declared order.
	Strategy string `toml:"strategy"`

	// Rules are optional conditional routes evaluated before the
	// default providers list.
	Rules []RouteRuleConfig `toml:"rules"`

	// Group names a [[provider_groups]] entry as the default route,
	// instead of listing providers inline. Mutually exclusive with
	// Providers.
	Group string `toml:"group"`
}

// Pooled reports whether this block uses the multi-provider shape.
func (t *TransportConfig) Pooled() bool {
	return len(t.Providers) > 0 || len(t.Rules) > 0 || t.Group != ""
}

// Label is the static transport identity for logs and metrics. Inline
// blocks report their type exactly as v1.x did; pooled blocks report
// "pool", because which provider ran is only known per-send and is
// reported from the send result instead.
func (t *TransportConfig) Label() string {
	if t.Pooled() {
		return "pool"
	}
	return t.Type
}

// EffectiveStrategy reports the strategy that will actually be used —
// the configured one, or "failover" by default. Empty for an inline
// block, which has nothing to route between.
func (t *TransportConfig) EffectiveStrategy() string {
	if !t.Pooled() {
		return ""
	}
	if t.Strategy == "" {
		return string(transport.StrategyFailover)
	}
	return t.Strategy
}

// ProviderRefs returns every pool name this block references, default
// route first, in declared order with duplicates kept — callers that
// need uniqueness de-duplicate themselves.
func (t *TransportConfig) ProviderRefs() []string {
	if !t.Pooled() {
		return nil
	}
	refs := append([]string(nil), t.Providers...)
	for _, r := range t.Rules {
		refs = append(refs, r.Providers...)
	}
	return refs
}

// GroupRefs returns every [[provider_groups]] name this block
// references, default route first.
func (t *TransportConfig) GroupRefs() []string {
	if !t.Pooled() {
		return nil
	}
	var refs []string
	if t.Group != "" {
		refs = append(refs, t.Group)
	}
	for _, r := range t.Rules {
		if r.Group != "" {
			refs = append(refs, r.Group)
		}
	}
	return refs
}

// RateLimitConfig is the per-endpoint token-bucket configuration.
type RateLimitConfig struct {
	Count    int      `toml:"count"`
	Interval Duration `toml:"interval"`
}

// LoggingConfig is the global logging configuration.
type LoggingConfig struct {
	Level  string `toml:"level"`  // debug | info | warn | error (default info)
	Format string `toml:"format"` // json (only json supported in v1.0)
}

// Duration wraps time.Duration with TOML support. BurntSushi/toml does not
// natively unmarshal time.Duration, so we provide a TextUnmarshaler.
type Duration time.Duration

// UnmarshalText parses a duration string like "1m", "30s", "1h30m".
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Load reads a TOML config file from path, resolves ${env.VAR} placeholders,
// parses the TOML, and runs validation. Returns the validated Config or an
// error describing the first problem encountered.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	resolved, err := resolveEnvVars(raw)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	var cfg Config
	md, err := toml.Decode(string(resolved), &cfg)
	if err != nil {
		return nil, fmt.Errorf("config: parse TOML: %w", err)
	}

	// Reject unknown top-level keys and unknown struct fields. Silent
	// acceptance of typos (e.g., writing `starttls = true` when the
	// real field is `require_tls`) is a high-cost foot-gun: the parse
	// succeeds, the runtime behavior doesn't match the intent, and the
	// operator only finds out when something downstream fails to work.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("config: unknown field(s) — likely a typo or stale config: %s", strings.Join(keys, ", "))
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	return &cfg, nil
}

// envVarPattern matches ${env.VARNAME} placeholders. Variable names must
// be UPPER_SNAKE_CASE per POSIX env-var conventions; this also reduces
// false matches in body templates and other user-controlled strings.
var envVarPattern = regexp.MustCompile(`\$\{env\.([A-Z_][A-Z0-9_]*)\}`)

// resolveEnvVars replaces ${env.VAR} placeholders with os.Getenv values.
// Reports all missing variables in a single error so operators don't play
// whack-a-mole on first run.
func resolveEnvVars(raw []byte) ([]byte, error) {
	var missing []string
	seen := map[string]bool{}
	out := envVarPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		sub := envVarPattern.FindSubmatch(match)
		name := string(sub[1])
		val, ok := os.LookupEnv(name)
		if !ok {
			if !seen[name] {
				missing = append(missing, name)
				seen[name] = true
			}
			return match // leave as-is; caller will surface the error
		}
		return []byte(val)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("env var(s) not set: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// Validate checks structural and semantic constraints on a parsed Config.
// Returns the first error; runs cheap checks before expensive ones.
func (c *Config) Validate() error {
	// An SMTP-listener-only deployment is a first-class shape (the
	// Ghost/Gitea recipes run exactly this); the HTTP mux still serves
	// /healthz and /metrics with zero endpoints. Require at least one
	// ingress of either kind.
	if len(c.Endpoints) == 0 && c.SMTPListener == nil {
		return errors.New("at least one ingress required: define [[endpoints]] or [smtp_listener]")
	}

	// The pool is validated first: every later block that references a
	// provider by name needs the name set to already exist, and a bad
	// credential block is a more useful first error than the reference
	// that happens to mention it.
	pool := map[string]bool{}
	for i, p := range c.Providers {
		if err := p.Validate(); err != nil {
			return fmt.Errorf("providers[%d] (%s): %w", i, p.Name, err)
		}
		if pool[p.Name] {
			return fmt.Errorf("providers[%d]: duplicate provider name %q", i, p.Name)
		}
		pool[p.Name] = true
	}

	// Projects are validated through the project package, which owns
	// the credential and sender-policy rules (and the digests, so the
	// plaintext key never travels further than this call).
	if len(c.Projects) > 0 {
		if _, err := project.Build(c.ProjectConfigs()); err != nil {
			return err
		}
		if c.Lifecycle == nil {
			for i, p := range c.Projects {
				if p.WebhookURL != "" {
					return fmt.Errorf("projects[%d] (%s): webhook_url requires a top-level [lifecycle] block — without event ingestion the callback would never fire", i, p.Name)
				}
			}
		}
		if c.Storage == nil {
			for i, p := range c.Projects {
				if p.WebhookURL != "" {
					return fmt.Errorf("projects[%d] (%s): webhook_url requires a [storage] block — undeliverable events must be queued, not dropped", i, p.Name)
				}
			}
		}
	}

	// Groups share the provider name space so `group = "x"` and
	// `providers = ["x"]` can never denote two different routes.
	groups := map[string]bool{}
	for i, g := range c.ProviderGroups {
		if err := g.Validate(); err != nil {
			return fmt.Errorf("provider_groups[%d] (%s): %w", i, g.Name, err)
		}
		if groups[g.Name] {
			return fmt.Errorf("provider_groups[%d]: duplicate group name %q", i, g.Name)
		}
		if pool[g.Name] {
			return fmt.Errorf("provider_groups[%d]: %q is already a provider name — groups and providers share one name space", i, g.Name)
		}
		for j, n := range g.Providers {
			if !pool[n] {
				return fmt.Errorf("provider_groups[%d] (%s): providers[%d]: unknown provider %q", i, g.Name, j, n)
			}
		}
		groups[g.Name] = true
	}

	seenPaths := map[string]bool{}
	for i, ep := range c.Endpoints {
		ep.projectMode = len(c.Projects) > 0
		if err := ep.Validate(); err != nil {
			return fmt.Errorf("endpoints[%d] (%s): %w", i, ep.Path, err)
		}
		if err := ep.Transport.ValidateRefs(pool, groups); err != nil {
			return fmt.Errorf("endpoints[%d] (%s): transport: %w", i, ep.Path, err)
		}
		if seenPaths[ep.Path] {
			return fmt.Errorf("duplicate endpoint path: %s", ep.Path)
		}
		seenPaths[ep.Path] = true
	}

	if c.Logging.Format != "" && c.Logging.Format != "json" {
		return fmt.Errorf("logging.format: only \"json\" supported in v1.0, got %q", c.Logging.Format)
	}
	switch c.Logging.Level {
	case "", "debug", "info", "warn", "error":
		// ok
	default:
		return fmt.Errorf("logging.level: must be one of debug|info|warn|error, got %q", c.Logging.Level)
	}

	if c.SMTPListener != nil {
		if err := c.SMTPListener.Validate(); err != nil {
			return fmt.Errorf("smtp_listener: %w", err)
		}
		if err := c.SMTPListener.Transport.ValidateRefs(pool, groups); err != nil {
			return fmt.Errorf("smtp_listener: transport: %w", err)
		}
	}

	// A pool entry nobody references is almost always a rename that
	// only got applied on one side. Cheap to catch here, expensive to
	// notice in production when the failover you configured never fires.
	if len(pool) > 0 {
		byGroup := map[string][]string{}
		for _, g := range c.ProviderGroups {
			byGroup[g.Name] = g.Providers
		}
		referenced := map[string]bool{}
		mark := func(t TransportConfig) {
			for _, n := range t.ProviderRefs() {
				referenced[n] = true
			}
			for _, g := range t.GroupRefs() {
				for _, n := range byGroup[g] {
					referenced[n] = true
				}
			}
		}
		for _, ep := range c.Endpoints {
			mark(ep.Transport)
		}
		if c.SMTPListener != nil {
			mark(c.SMTPListener.Transport)
		}
		var orphans []string
		for _, p := range c.Providers {
			if !referenced[p.Name] {
				orphans = append(orphans, p.Name)
			}
		}
		if len(orphans) > 0 {
			return fmt.Errorf("providers: %v defined but referenced by no endpoint or smtp_listener — a provider nobody routes to is dead config (usually a half-applied rename)", orphans)
		}
	}

	if c.Storage != nil {
		if err := c.Storage.Validate(); err != nil {
			return fmt.Errorf("storage: %w", err)
		}
	}

	// FR82: lifecycle rides the submission log; without storage there is
	// nothing to correlate events against.
	if c.Lifecycle != nil {
		if err := c.Lifecycle.Validate(); err != nil {
			return fmt.Errorf("lifecycle: %w", err)
		}
		for name, pw := range c.Lifecycle.Providers {
			if err := pw.Validate(); err != nil {
				return fmt.Errorf("lifecycle.providers.%s: %w", name, err)
			}
			// Build the adapter now so a bad secret, an unusable CIDR,
			// or a setting the provider does not support fails at
			// startup rather than silently accepting forged events.
			_, known, berr := lifecycle.BuildAdapter(name, lifecycle.ProviderAuthConfig{
				SigningSecret:  pw.SigningSecret,
				AllowedIPs:     pw.AllowedIPs,
				TrustedProxies: pw.TrustedProxies,
				Tolerance:      pw.Tolerance.Std(),
			})
			if !known {
				return fmt.Errorf("lifecycle.providers.%s: unknown provider dialect (known: %v)", name, lifecycle.AdapterNames())
			}
			if berr != nil {
				return fmt.Errorf("lifecycle.providers.%s: %w", name, berr)
			}
		}
		if c.Storage == nil {
			return errors.New("lifecycle: requires a [storage] block (event correlation reads the submission log)")
		}
	}
	for i, ep := range c.Endpoints {
		if ep.WebhookURL != "" && c.Lifecycle == nil {
			return fmt.Errorf("endpoints[%d] (%s): webhook_url requires a top-level [lifecycle] block — without event ingestion the callback would never fire", i, ep.Path)
		}
	}

	// NFR45: every URL an operator can point Posthorn AT is checked
	// against the outbound policy at parse time. This is the early,
	// legible half of the SSRF defense; netguard's dial-time check is
	// the half that actually holds, because a hostname's resolution can
	// change after the config loads.
	if err := c.validateOutboundTargets(); err != nil {
		return err
	}

	return nil
}

// validateOutboundTargets checks every operator-supplied destination.
func (c *Config) validateOutboundTargets() error {
	policy := c.OutboundPolicy()
	check := func(what, raw string) error {
		if raw == "" {
			return nil
		}
		if err := policy.ValidateURL(raw); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		return nil
	}

	for i, ep := range c.Endpoints {
		if err := check(fmt.Sprintf("endpoints[%d] (%s): webhook_url", i, ep.Path), ep.WebhookURL); err != nil {
			return err
		}
		if err := check(fmt.Sprintf("endpoints[%d] (%s): transport.settings.url", i, ep.Path),
			webhookTransportURL(ep.Transport)); err != nil {
			return err
		}
	}
	for i, p := range c.Projects {
		if err := check(fmt.Sprintf("projects[%d] (%s): webhook_url", i, p.Name), p.WebhookURL); err != nil {
			return err
		}
	}
	for i, p := range c.Providers {
		if err := check(fmt.Sprintf("providers[%d] (%s): settings.url", i, p.Name),
			webhookTransportURL(TransportConfig{Type: p.Type, Settings: p.Settings})); err != nil {
			return err
		}
	}
	if c.SMTPListener != nil {
		if err := check("smtp_listener: transport.settings.url",
			webhookTransportURL(c.SMTPListener.Transport)); err != nil {
			return err
		}
	}
	return nil
}

// webhookTransportURL returns the destination of a webhook-transport
// block, or "" for any other transport type. The webhook transport is
// the one that POSTs a submission to an address of the operator's
// choosing, which makes it an outbound target like a callback URL.
func webhookTransportURL(tc TransportConfig) string {
	if tc.Type != "webhook" {
		return ""
	}
	raw, _ := tc.Settings["url"].(string)
	return raw
}

// Validate runs structural checks on the SMTP listener block. The
// detailed semantic checks (auth/cert combinations) live in the smtp
// package's own Validate; here we just confirm required fields are
// present and the transport block is well-formed.
func (s *SMTPListenerConfig) Validate() error {
	if s.Listen == "" {
		return errors.New("listen is required (e.g., \":2525\")")
	}
	if _, err := transport.ParseMailKind(s.MailKind); err != nil {
		return fmt.Errorf("mail_kind: %w", err)
	}
	if len(s.AllowedSenders) == 0 {
		return errors.New("allowed_senders: at least one entry required")
	}
	if err := s.Transport.Validate(); err != nil {
		return fmt.Errorf("transport: %w", err)
	}
	if s.MaxRecipientsPerSession < 0 {
		return fmt.Errorf("max_recipients_per_session: must be non-negative, got %d", s.MaxRecipientsPerSession)
	}
	if s.IdleTimeout.Std() < 0 {
		return fmt.Errorf("idle_timeout: must be non-negative, got %v", s.IdleTimeout.Std())
	}
	if s.MaxConnections < 0 {
		return fmt.Errorf("max_connections: must be non-negative, got %d", s.MaxConnections)
	}
	if s.MaxConnectionsPerIP < 0 {
		return fmt.Errorf("max_connections_per_ip: must be non-negative, got %d", s.MaxConnectionsPerIP)
	}
	// #41: with auth_required = "none" the sender allowlist is the only
	// gate, so refuse a bind address we can't verify as private unless
	// the operator explicitly asserts the network is trusted. Fail-closed
	// at parse time per the ADR-10 footgun philosophy: better a startup
	// error with a fix in it than a silently open relay.
	if s.AuthRequired == "none" && !s.TrustedNetwork {
		if err := validatePrivateBind(s.Listen); err != nil {
			return fmt.Errorf("auth_required = \"none\": %w — bind to a loopback/private address, require auth, or set trusted_network = true if the listener is reachable only from a trusted network (Docker bridge with no port exposure, VPN, firewall)", err)
		}
	}
	return nil
}

// validatePrivateBind returns an error when the listen address is not
// verifiably private: empty host / unspecified IP (binds all
// interfaces), a public IP, or a hostname we can't classify. Loopback,
// RFC1918/ULA-private, and link-local addresses pass, as does the
// literal "localhost".
func validatePrivateBind(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen %q: %v", listen, err)
	}
	if host == "" {
		return fmt.Errorf("listen %q binds all interfaces", listen)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// A hostname we can't classify without resolving — fail closed.
		return fmt.Errorf("listen host %q is not an IP address; cannot verify it is private", host)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("listen %q binds all interfaces", listen)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return nil
	}
	return fmt.Errorf("listen address %q is public", host)
}

// EffectiveRequireTLS resolves the *bool RequireTLS to a concrete value.
// Unset (nil) defaults to true — STARTTLS-required is the documented
// production default; operators opt out by explicitly setting
// `require_tls = false` for local development.
func (s *SMTPListenerConfig) EffectiveRequireTLS() bool {
	if s.RequireTLS == nil {
		return true
	}
	return *s.RequireTLS
}

// Validate checks one endpoint's configuration.
func (e *EndpointConfig) Validate() error {
	if e.Path == "" {
		return errors.New("path is required")
	}
	if !strings.HasPrefix(e.Path, "/") {
		return fmt.Errorf("path must start with /: %q", e.Path)
	}
	if _, err := transport.ParseMailKind(e.MailKind); err != nil {
		return fmt.Errorf("mail_kind: %w", err)
	}

	if len(e.To) == 0 {
		return errors.New("to is required (one or more recipient addresses)")
	}
	for _, addr := range e.To {
		if _, err := mail.ParseAddress(addr); err != nil {
			return fmt.Errorf("to: invalid email %q: %w", addr, err)
		}
	}

	if e.From == "" {
		return errors.New("from is required")
	}
	if _, err := mail.ParseAddress(e.From); err != nil {
		return fmt.Errorf("from: invalid email %q: %w", e.From, err)
	}

	if e.Subject == "" {
		return errors.New("subject is required")
	}
	if e.Body == "" {
		return errors.New("body is required")
	}

	// FR71/FR72: body_format enum and the text_body pairing rule. An
	// explicit fallback template on a text endpoint is a config the
	// operator misunderstands — reject loudly rather than ignore.
	switch e.BodyFormat {
	case "", BodyFormatText, BodyFormatHTML:
		// valid
	default:
		return fmt.Errorf("body_format: must be %q or %q, got %q", BodyFormatText, BodyFormatHTML, e.BodyFormat)
	}
	if e.TextBody != "" && e.BodyFormat != BodyFormatHTML {
		return errors.New("text_body: only valid when body_format = \"html\" (text endpoints render body directly; there is no fallback part)")
	}

	// FR84: webhook callback pairing. The secret gate is ≥16 bytes —
	// HMAC with a short secret is decorative (proof_of_browser
	// precedent).
	if e.WebhookURL != "" {
		u, err := url.Parse(e.WebhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("webhook_url: must be an absolute http(s) URL, got %q", e.WebhookURL)
		}
		if len(e.WebhookSecret) < 16 {
			return errors.New("webhook_secret: required with webhook_url and must be at least 16 bytes")
		}
	}
	if e.WebhookSecret != "" && e.WebhookURL == "" {
		return errors.New("webhook_secret: only valid alongside webhook_url")
	}

	// FR90/FR91: attachments block (fail-closed).
	if e.Attachments != nil {
		if err := e.Attachments.Validate(); err != nil {
			return fmt.Errorf("attachments: %w", err)
		}
	}

	if err := e.Transport.Validate(); err != nil {
		return fmt.Errorf("transport: %w", err)
	}

	// FR31: resolve effective auth mode. Empty / unset defaults to form.
	auth := e.Auth
	if auth == "" {
		auth = AuthForm
	}
	switch auth {
	case AuthForm, AuthAPIKey:
		// valid
	default:
		return fmt.Errorf("auth: must be %q or %q, got %q", AuthForm, AuthAPIKey, e.Auth)
	}

	if auth == AuthAPIKey {
		// FR33: api-mode requires non-empty api_keys — unless projects
		// are configured, in which case project credentials are the
		// endpoint's authentication and listing endpoint keys is
		// optional (and, once migration is done, undesirable: an
		// endpoint key bypasses the project sender policy).
		if len(e.APIKeys) == 0 && !e.projectMode {
			return errors.New("api_keys: at least one key required when auth = \"api-key\" (or configure [[projects]], whose credentials authenticate api-mode endpoints)")
		}
		for i, k := range e.APIKeys {
			if strings.TrimSpace(k) == "" {
				return fmt.Errorf("api_keys[%d]: empty key", i)
			}
		}
		// FR32: api-mode rejects form-mode browser defenses at parse time
		// (ADR-10). Silent ignore would let an operator think they were
		// protected when they weren't.
		if e.Honeypot != "" {
			return errors.New("honeypot: not valid on auth=\"api-key\" endpoints (api-mode is authenticated; browser bot defenses do not apply)")
		}
		if e.AllowedOrigins != nil {
			return errors.New("allowed_origins: not valid on auth=\"api-key\" endpoints (api-mode is authenticated; browser CORS defenses do not apply)")
		}
		if e.RedirectSuccess != "" {
			return errors.New("redirect_success: not valid on auth=\"api-key\" endpoints (servers do not follow redirects in this flow)")
		}
		if e.RedirectError != "" {
			return errors.New("redirect_error: not valid on auth=\"api-key\" endpoints (servers do not follow redirects in this flow)")
		}
		if e.CSRFSecret != "" {
			return errors.New("csrf_secret: not valid on auth=\"api-key\" endpoints (api-mode callers are server-to-server; CSRF defense is form-mode only)")
		}
		if e.Reputation != nil {
			return errors.New("reputation: not valid on auth=\"api-key\" endpoints (form-mode only in v1.1)")
		}
		if e.ProofOfBrowser != nil {
			return errors.New("proof_of_browser: not valid on auth=\"api-key\" endpoints (server-to-server callers don't run a browser)")
		}
		if e.Captcha != nil {
			return errors.New("captcha: not valid on auth=\"api-key\" endpoints (server-to-server callers don't solve captchas)")
		}
		// FR42: idempotency_cache_size must be positive when set; zero
		// means "use the default" (handler-side resolution).
		if e.IdempotencyCacheSize < 0 {
			return fmt.Errorf("idempotency_cache_size: must be non-negative, got %d", e.IdempotencyCacheSize)
		}
	} else {
		// Form mode: api-mode-only fields must be unset. Catches the
		// "operator forgot to set auth = api-key" misconfiguration.
		if e.IdempotencyCacheSize != 0 {
			return errors.New("idempotency_cache_size: only valid on auth=\"api-key\" endpoints")
		}
		if len(e.APIKeys) > 0 {
			return errors.New("api_keys: must be unset unless auth = \"api-key\"")
		}
		// FR57: form-mode CSRF. When csrf_secret is set, validate it
		// passes the minimum-length check. csrf_token_ttl must be
		// positive when set; zero means "use the default" at handler-
		// construction time.
		if e.CSRFSecret != "" {
			if err := csrf.ValidateSecret([]byte(e.CSRFSecret)); err != nil {
				return err
			}
		}
		if e.CSRFTokenTTL.Std() < 0 {
			return fmt.Errorf("csrf_token_ttl: must be non-negative, got %v", e.CSRFTokenTTL.Std())
		}
		if e.Reputation != nil {
			if err := e.Reputation.validate(); err != nil {
				return fmt.Errorf("reputation: %w", err)
			}
		}
		if e.ProofOfBrowser != nil {
			if err := e.ProofOfBrowser.validate(); err != nil {
				return fmt.Errorf("proof_of_browser: %w", err)
			}
		}
		if e.Captcha != nil {
			if err := e.Captcha.validate(); err != nil {
				return fmt.Errorf("captcha: %w", err)
			}
		}
	}

	// NFR4: explicitly-empty allowed_origins is a misconfiguration.
	// BurntSushi/toml leaves the slice nil when the key is absent and
	// returns a non-nil empty slice when the operator wrote `allowed_origins = []`.
	// (Already rejected above for api-mode; this catches form-mode.)
	if e.AllowedOrigins != nil && len(e.AllowedOrigins) == 0 {
		return errors.New("allowed_origins is explicitly empty; either remove the key (to allow all origins) or list at least one origin")
	}

	if e.RateLimit != nil {
		if e.RateLimit.Count <= 0 {
			return fmt.Errorf("rate_limit.count must be positive, got %d", e.RateLimit.Count)
		}
		if e.RateLimit.Interval.Std() <= 0 {
			return fmt.Errorf("rate_limit.interval must be positive, got %v", e.RateLimit.Interval.Std())
		}
	}

	return nil
}

// Validate checks a transport block's own structure. Two shapes are
// accepted and they are mutually exclusive:
//
//   - inline: type + settings, dispatched to the transport registry's
//     validator exactly as v1.x did;
//   - pooled: providers + optional strategy + optional rules, whose
//     names are resolved later against the [[providers]] pool.
//
// Name resolution is deliberately NOT done here: this method is a
// method on the block, and only Config.Validate can see both the block
// and the pool. Config.Validate calls ValidateRefs for that half.
func (t *TransportConfig) Validate() error {
	inline := t.Type != "" || len(t.Settings) > 0
	pooled := t.Pooled()

	switch {
	case inline && pooled:
		return errors.New("type/settings and providers/rules are mutually exclusive — an inline transport and a provider pool in one block leaves it ambiguous which one is live; pick one")
	case !inline && !pooled:
		return errors.New("type is required (e.g., \"postmark\"), or reference the pool with providers = [\"name\", ...]")
	case inline:
		if t.Type == "" {
			return errors.New("type is required (e.g., \"postmark\") — settings were given with no type to interpret them")
		}
		if t.Strategy != "" {
			return errors.New("strategy applies to a provider pool; an inline transport has nothing to route between")
		}
		reg, ok := transport.Lookup(t.Type)
		if !ok {
			return transport.UnknownTypeError(t.Type)
		}
		return reg.Validate(t.Settings)
	}

	// Pooled.
	if len(t.Providers) > 0 && t.Group != "" {
		return errors.New("providers and group are mutually exclusive — a route is one list, name it once")
	}
	if len(t.Providers) == 0 && t.Group == "" {
		return errors.New("providers: at least one provider name required, or group = \"name\" (rules alone leave messages that match no rule with nowhere to go)")
	}
	if _, err := transport.ParseStrategy(t.Strategy); err != nil {
		return fmt.Errorf("strategy: %w", err)
	}
	seen := map[string]bool{}
	for i, name := range t.Providers {
		if name == "" {
			return fmt.Errorf("providers[%d]: empty provider name", i)
		}
		if seen[name] {
			return fmt.Errorf("providers[%d]: duplicate provider %q — listing a provider twice does not give it a second chance, use weight instead", i, name)
		}
		seen[name] = true
	}
	for i, rule := range t.Rules {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("rules[%d]: %w", i, err)
		}
	}
	return nil
}

// ValidateRefs resolves every provider name this block references
// against the defined pool. Split from Validate because only the
// top-level Config can supply the pool.
func (t *TransportConfig) ValidateRefs(pool, groups map[string]bool) error {
	if !t.Pooled() {
		return nil
	}
	if len(pool) == 0 {
		return errors.New("providers references the pool, but no [[providers]] blocks are defined")
	}
	known := make([]string, 0, len(pool))
	for n := range pool {
		known = append(known, n)
	}
	sort.Strings(known)
	for _, name := range t.ProviderRefs() {
		if !pool[name] {
			return fmt.Errorf("unknown provider %q (defined: %v)", name, known)
		}
	}
	knownGroups := make([]string, 0, len(groups))
	for n := range groups {
		knownGroups = append(knownGroups, n)
	}
	sort.Strings(knownGroups)
	for _, name := range t.GroupRefs() {
		if !groups[name] {
			return fmt.Errorf("unknown provider group %q (defined: %v)", name, knownGroups)
		}
	}
	return nil
}
