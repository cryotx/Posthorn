// Package project implements Posthorn's lightweight multi-tenant layer:
// several business projects sharing one mail gateway.
//
// A Project is an identity, a credential, and a sending policy — nothing
// more. There is no user system, no registration, and no billing. Projects
// can come from configuration or be created by an administrator.
//
// The whole layer is opt-in. Without [[projects]] blocks the registry is
// empty, no ingress consults it, and Posthorn behaves exactly as it did
// before (NFR42).
package project

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// MinAPIKeyLength is the shortest credential a project may carry. A
// project key authorises sending under that project's identity, so a
// guessable one is an identity-takeover primitive, not an inconvenience.
const MinAPIKeyLength = 24

// Project is one business tenant of the gateway.
//
// The API key is NOT stored. Only its SHA-256 digest is retained, so a
// heap dump, a panic trace, or a future admin surface cannot yield the
// plaintext (NFR44). The plaintext exists only inside Build, long enough
// to be hashed.
type Project struct {
	// Name is the operator-chosen identifier. It reaches logs, metric
	// labels and event payloads, so it is operator content by
	// construction (NFR24).
	Name string

	// Enabled gates authentication. A disabled project's key stops
	// working immediately without the operator having to delete it —
	// the difference between suspending a tenant and losing its config.
	Enabled bool

	// DefaultFrom is used when a request names no sender. Validated
	// against AllowedFromDomains at config-parse time, so it can never
	// be a back door around the policy below.
	DefaultFrom string

	// fixedFrom restricts admin-managed projects to the configured mailbox.
	fixedFrom bool

	// AllowedFromDomains is the anti-spoofing control and the reason
	// this package exists. Required and non-empty: a project that may
	// send as any domain can impersonate every other project on the
	// gateway, so there is deliberately no "unset means allow all".
	// Entries are exact ("example.com") or a suffix wildcard
	// ("*.example.com").
	AllowedFromDomains []string

	// DailyLimit and MonthlyLimit are reserved for a future project-level
	// budget. They are RECORDED but NOT ENFORCED in this release, and
	// they are deliberately not wired to the provider quota machinery:
	// a project budget bounds one tenant's own volume, a provider quota
	// bounds an upstream account's allowance, and conflating them would
	// make "who ran out" unanswerable (ADR-35).
	DailyLimit   int64
	MonthlyLimit int64

	// WebhookURL and WebhookSecret receive this project's normalized
	// delivery events. The secret falls under NFR3/NFR29: never logged.
	WebhookURL    string
	WebhookSecret string

	// keyDigest is the SHA-256 of the PRIMARY API key — the first one
	// configured. Never the key itself.
	keyMu     sync.RWMutex
	keyDigest [sha256.Size]byte

	// keyDigests holds every currently-valid key's digest, primary
	// first. More than one exists during a ROTATION (FR141): the
	// operator adds the new key, restarts, moves callers across, then
	// removes the old one — and every caller keeps working throughout,
	// because both keys authenticate for as long as both are in the
	// config. Admin-managed projects instead rotate immediately at runtime.
	keyDigests [][sha256.Size]byte

	// smtpUsername is the SMTP AUTH identity that maps to this project,
	// when the operator configured one. Empty means this project has no
	// SMTP identity.
	smtpUsername string
	smtpDigest   [sha256.Size]byte
	hasSMTP      bool

	// adminDisabled is the operator's kill switch (FR144): suspend a
	// tenant mid-incident without an edit-and-restart.
	//
	// It can only SUBTRACT. A project the config disabled cannot be
	// enabled by clearing this, so the config file remains the source
	// of truth for who may send and this is purely a stop button.
	adminDisabled atomic.Bool
}

// SetAdminDisabled suspends or restores a project at runtime.
func (p *Project) SetAdminDisabled(disabled bool) { p.adminDisabled.Store(disabled) }

// AdminDisabled reports whether an operator has suspended this project.
func (p *Project) AdminDisabled() bool { return p.adminDisabled.Load() }

// Active reports whether this project may authenticate right now: both
// the configured state and the operator's kill switch must allow it.
func (p *Project) Active() bool { return p.Enabled && !p.adminDisabled.Load() }

// KeyFingerprint identifies WHICH credential a project uses, without
// being reversible into it (FR142).
//
// Derived from the key's SHA-256 digest, not the key: the plaintext is
// gone by the time anything can ask. Twelve hex characters is enough to
// see that a rotation happened and that two projects hold different
// keys, and far too little to confirm a guess — an attacker who could
// test candidates against it would need the digest function anyway, at
// which point they would just compare full digests.
func (p *Project) KeyFingerprint() string {
	p.keyMu.RLock()
	defer p.keyMu.RUnlock()
	return hex.EncodeToString(p.keyDigest[:6])
}

// HasSMTPIdentity reports whether this project can authenticate over
// the SMTP listener.
func (p *Project) HasSMTPIdentity() bool { return p.hasSMTP }

// KeySetDigest is a digest over the project's whole credential set, so
// a restart can tell that the operator rotated a key without ever
// holding one. Digest of digests: it can confirm "this changed" and
// nothing else.
func (p *Project) KeySetDigest() string {
	p.keyMu.RLock()
	defer p.keyMu.RUnlock()
	h := sha256.New()
	for _, d := range p.keyDigests {
		h.Write(d[:])
	}
	if p.hasSMTP {
		h.Write(p.smtpDigest[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// KeyCount reports how many keys currently authenticate for this
// project. More than one means a rotation is in progress, which is
// worth showing an operator: an overlap left open forever is an old
// credential nobody remembered to remove.
func (p *Project) KeyCount() int { p.keyMu.RLock(); defer p.keyMu.RUnlock(); return len(p.keyDigests) }

// Config is the parsed shape one [[projects]] block produces. The
// config package converts its own struct into this, so the project
// package stays free of TOML concerns and can be tested directly.
type Config struct {
	Name               string
	Enabled            bool
	APIKey             string
	APIKeys            []string
	DefaultFrom        string
	AllowedFromDomains []string
	DailyLimit         int64
	MonthlyLimit       int64
	WebhookURL         string
	WebhookSecret      string
	SMTPUsername       string
	SMTPPassword       string
}

// Registry is the process-wide set of projects. Administrator-created
// projects can be added and rotated while requests are being served.
type Registry struct {
	mu       sync.RWMutex
	managed  map[string]bool
	byName   map[string]*Project
	byKey    map[[sha256.Size]byte]*Project
	bySMTP   map[string]*Project
	ordered  []*Project
	enabled  int
	hasSMTPs bool
}

// ErrNoProjects is returned by Authenticate on an empty registry, so a
// caller can tell "project mode is off" from "bad credential".
var ErrNoProjects = errors.New("project: no projects configured")

// Build constructs a registry, validating every project. Returns an
// error naming the first problem; the API key plaintext never leaves
// this function.
func Build(cfgs []Config) (*Registry, error) {
	r := &Registry{
		managed: make(map[string]bool),
		byName:  make(map[string]*Project, len(cfgs)),
		byKey:   make(map[[sha256.Size]byte]*Project, len(cfgs)),
		bySMTP:  make(map[string]*Project, len(cfgs)),
	}
	for i, c := range cfgs {
		p, err := build(c)
		if err != nil {
			return nil, fmt.Errorf("projects[%d] (%s): %w", i, c.Name, err)
		}
		if _, dup := r.byName[p.Name]; dup {
			return nil, fmt.Errorf("projects[%d]: duplicate project name %q", i, p.Name)
		}
		for _, digest := range p.keyDigests {
			if other, dup := r.byKey[digest]; dup {
				// Two projects sharing a key makes the identity of an
				// authenticated request ambiguous — and would silently
				// pick whichever the map happened to hold.
				return nil, fmt.Errorf("projects[%d] (%s): an api key is identical to one used by project %q", i, p.Name, other.Name)
			}
		}
		r.byName[p.Name] = p
		for _, digest := range p.keyDigests {
			r.byKey[digest] = p
		}
		if p.hasSMTP {
			if other, dup := r.bySMTP[p.smtpUsername]; dup {
				return nil, fmt.Errorf("projects[%d] (%s): smtp_username %q is already used by project %q",
					i, p.Name, p.smtpUsername, other.Name)
			}
			r.bySMTP[p.smtpUsername] = p
			r.hasSMTPs = true
		}
		r.ordered = append(r.ordered, p)
		if p.Enabled {
			r.enabled++
		}
	}
	return r, nil
}

// collectKeys returns the digests of every key this project accepts,
// primary first, rejecting an empty set, a short key, or a duplicate.
//
// A duplicate inside one project is refused rather than deduplicated:
// it almost always means a half-finished rotation where the operator
// pasted the same value twice and believes two keys are live.
func collectKeys(c Config) ([][sha256.Size]byte, error) {
	raw := make([]string, 0, 1+len(c.APIKeys))
	if c.APIKey != "" {
		raw = append(raw, c.APIKey)
	}
	raw = append(raw, c.APIKeys...)
	if len(raw) == 0 {
		return nil, errors.New("api_key: required (set api_key, or api_keys during a rotation)")
	}
	seen := make(map[[sha256.Size]byte]bool, len(raw))
	out := make([][sha256.Size]byte, 0, len(raw))
	for i, k := range raw {
		if len(k) < MinAPIKeyLength {
			return nil, fmt.Errorf("api key %d: must be at least %d characters "+
				"(a project key authorises sending as this project; a guessable one is an identity takeover)",
				i+1, MinAPIKeyLength)
		}
		digest := sha256.Sum256([]byte(k))
		if seen[digest] {
			return nil, fmt.Errorf("api key %d: is the same value as an earlier key for this project — "+
				"during a rotation both entries must be different keys", i+1)
		}
		seen[digest] = true
		out = append(out, digest)
	}
	return out, nil
}

func build(c Config) (*Project, error) {
	if c.Name == "" {
		return nil, errors.New("name is required")
	}
	if strings.TrimSpace(c.Name) != c.Name {
		return nil, fmt.Errorf("name %q: leading/trailing whitespace", c.Name)
	}
	keys, err := collectKeys(c)
	if err != nil {
		return nil, err
	}
	if len(c.AllowedFromDomains) == 0 {
		return nil, errors.New("allowed_from_domains: required and non-empty — a project that may send as any domain can impersonate every other project on this gateway, so there is no allow-all default")
	}

	p := &Project{
		Name:               c.Name,
		Enabled:            c.Enabled,
		DefaultFrom:        c.DefaultFrom,
		AllowedFromDomains: make([]string, 0, len(c.AllowedFromDomains)),
		DailyLimit:         c.DailyLimit,
		MonthlyLimit:       c.MonthlyLimit,
		WebhookURL:         c.WebhookURL,
		WebhookSecret:      c.WebhookSecret,
		keyDigest:          keys[0],
		keyDigests:         keys,
	}
	for i, d := range c.AllowedFromDomains {
		norm, err := normalizeDomainPattern(d)
		if err != nil {
			return nil, fmt.Errorf("allowed_from_domains[%d]: %w", i, err)
		}
		p.AllowedFromDomains = append(p.AllowedFromDomains, norm)
	}

	// A default_from outside the project's own allowlist would be a
	// back door around the very policy this block exists to enforce.
	if p.DefaultFrom != "" {
		if _, err := p.checkFrom(p.DefaultFrom); err != nil {
			return nil, fmt.Errorf("default_from: %w", err)
		}
	}

	if c.SMTPUsername != "" || c.SMTPPassword != "" {
		if c.SMTPUsername == "" || c.SMTPPassword == "" {
			return nil, errors.New("smtp_username and smtp_password must be set together")
		}
		if len(c.SMTPPassword) < MinAPIKeyLength {
			return nil, fmt.Errorf("smtp_password: must be at least %d characters", MinAPIKeyLength)
		}
		p.smtpUsername = c.SMTPUsername
		p.smtpDigest = sha256.Sum256([]byte(c.SMTPPassword))
		p.hasSMTP = true
	}

	if c.WebhookURL != "" && len(c.WebhookSecret) < 16 {
		return nil, errors.New("webhook_secret: at least 16 characters required when webhook_url is set (it is the only thing proving an event came from Posthorn)")
	}
	return p, nil
}

// normalizeDomainPattern lowercases and validates one allowlist entry.
func normalizeDomainPattern(d string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(d))
	if v == "" {
		return "", errors.New("empty domain")
	}
	if strings.ContainsAny(v, "@ \t") {
		return "", fmt.Errorf("%q is a domain, not an address", d)
	}
	bare := strings.TrimPrefix(v, "*.")
	if bare == "" || strings.Contains(bare, "*") {
		return "", fmt.Errorf("%q: only a leading \"*.\" wildcard is supported", d)
	}
	if !strings.Contains(bare, ".") {
		return "", fmt.Errorf("%q: not a domain name", d)
	}
	return v, nil
}

// Len reports how many projects are configured. Zero means project mode
// is off and every ingress keeps its pre-project behavior.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.ordered)
}

// Enabled reports how many projects are currently enabled.
func (r *Registry) Enabled() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enabled
}

// HasSMTPIdentities reports whether any project carries SMTP credentials.
func (r *Registry) HasSMTPIdentities() bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.hasSMTPs
}

// Names returns the configured project names, sorted for stable logs.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.ordered))
	for _, p := range r.ordered {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// All returns every project in declared order.
func (r *Registry) All() []*Project {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]*Project(nil), r.ordered...)
}

// ByName returns a project by name, or (nil, false).
func (r *Registry) ByName(name string) (*Project, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byName[name]
	return p, ok
}

// AuthError distinguishes the ways authentication can fail, so the
// caller can log a reason without ever echoing the credential.
type AuthError struct {
	// Reason is a fixed enum: "unknown_key" or "disabled".
	Reason string

	// Project is set only for "disabled" — the credential was valid, so
	// naming the project is safe and is what an operator needs.
	Project string
}

func (e *AuthError) Error() string {
	if e.Project != "" {
		return "project " + e.Project + ": " + e.Reason
	}
	return "project auth: " + e.Reason
}

// HasCredentialDigest reports whether any project's API key or SMTP
// password has this digest.
//
// Exists so startup can prove the ADMIN token is not also a project
// credential without either side ever handling the other's plaintext.
// Reusing one would mean a tenant's application key could read every
// tenant's delivery history.
func (r *Registry) HasCredentialDigest(digest [sha256.Size]byte) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.ordered {
		p.keyMu.RLock()
		for _, known := range p.keyDigests {
			if subtle.ConstantTimeCompare(digest[:], known[:]) == 1 {
				p.keyMu.RUnlock()
				return true
			}
		}
		p.keyMu.RUnlock()
		if p.hasSMTP && subtle.ConstantTimeCompare(digest[:], p.smtpDigest[:]) == 1 {
			return true
		}
	}
	return false
}

// Authenticate resolves a bearer token to a project.
//
// The token is hashed and looked up by digest, then confirmed with a
// constant-time comparison of the full digest. Neither the token nor
// the digest is ever returned, logged, or included in an error.
func (r *Registry) Authenticate(token string) (*Project, error) {
	if r == nil {
		return nil, ErrNoProjects
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.byKey) == 0 {
		return nil, ErrNoProjects
	}
	digest := sha256.Sum256([]byte(token))
	p, ok := r.byKey[digest]
	if !ok {
		return nil, &AuthError{Reason: "unknown_key"}
	}
	// Belt and braces: confirm the full digest matched one of the
	// project's keys, not just the map's bucket, and do it in constant
	// time. Every key is compared, without an early exit, so the number
	// of comparisons does not depend on WHICH key was presented.
	var matched int
	for _, known := range p.keyDigests {
		matched |= subtle.ConstantTimeCompare(digest[:], known[:])
	}
	if matched != 1 {
		return nil, &AuthError{Reason: "unknown_key"}
	}
	if !p.Active() {
		// One reason for both: from the caller's side a suspended
		// project and a disabled one are the same answer, and which
		// mechanism produced it is the operator's business.
		return nil, &AuthError{Reason: "disabled", Project: p.Name}
	}
	return p, nil
}

// AuthenticateSMTP resolves an SMTP AUTH username/password pair to a
// project, so the SMTP listener produces the same project context an
// HTTP request does (FR111). Both ingresses share this one code path
// rather than growing separate authorisation systems.
func (r *Registry) AuthenticateSMTP(username, password string) (*Project, error) {
	if r == nil {
		return nil, ErrNoProjects
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.bySMTP) == 0 {
		return nil, ErrNoProjects
	}
	p, ok := r.bySMTP[username]
	if !ok {
		return nil, &AuthError{Reason: "unknown_key"}
	}
	digest := sha256.Sum256([]byte(password))
	if subtle.ConstantTimeCompare(digest[:], p.smtpDigest[:]) != 1 {
		return nil, &AuthError{Reason: "unknown_key"}
	}
	// Both ingresses share this check, so suspending a project stops
	// its SMTP identity at the same instant as its HTTP key.
	if !p.Active() {
		return nil, &AuthError{Reason: "disabled", Project: p.Name}
	}
	return p, nil
}

// FromError is a rejected sender address. It carries no submitter
// content beyond the domain, which is what the operator needs to see.
type FromError struct {
	Domain  string
	Project string
	Allowed []string
}

func (e *FromError) Error() string {
	return fmt.Sprintf("sender domain %q is not in project %q's allowed_from_domains %v",
		e.Domain, e.Project, e.Allowed)
}

// ResolveFrom decides the sender address for one message (FR109).
//
//	requested == ""  → DefaultFrom, else fallback (the endpoint's own
//	                   `from`), which must ALSO satisfy the allowlist
//	requested != ""  → must satisfy the allowlist
//
// This is the control that stops one project sending as another. It is
// enforced server-side from the authenticated project only: a `project`
// field in a request body is never consulted (NFR45).
func (p *Project) ResolveFrom(requested, fallback string) (string, error) {
	if p.fixedFrom {
		if requested != "" && requested != p.DefaultFrom {
			return "", fmt.Errorf("project %q: sender must be the configured mailbox", p.Name)
		}
		return p.DefaultFrom, nil
	}
	candidate := requested
	if candidate == "" {
		candidate = p.DefaultFrom
	}
	if candidate == "" {
		candidate = fallback
	}
	if candidate == "" {
		return "", fmt.Errorf("project %q: no sender address — set default_from on the project or from on the endpoint", p.Name)
	}
	if _, err := p.checkFrom(candidate); err != nil {
		return "", err
	}
	return candidate, nil
}

// AllowsFrom reports whether addr satisfies this project's allowlist.
func (p *Project) AllowsFrom(addr string) bool {
	_, err := p.checkFrom(addr)
	return err == nil
}

// checkFrom parses addr and matches its domain against the allowlist.
func (p *Project) checkFrom(addr string) (string, error) {
	parsed, err := mail.ParseAddress(addr)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid email address: %w", addr, err)
	}
	at := strings.LastIndex(parsed.Address, "@")
	if at < 0 || at == len(parsed.Address)-1 {
		return "", fmt.Errorf("%q has no domain", addr)
	}
	domain := strings.ToLower(strings.TrimSuffix(parsed.Address[at+1:], "."))
	for _, pattern := range p.AllowedFromDomains {
		if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
			if domain == suffix || strings.HasSuffix(domain, "."+suffix) {
				return domain, nil
			}
			continue
		}
		if domain == pattern {
			return domain, nil
		}
	}
	return "", &FromError{Domain: domain, Project: p.Name, Allowed: p.AllowedFromDomains}
}
