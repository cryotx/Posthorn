package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Admin authentication (FR143, FR146).
//
// The admin surface is a SEPARATE credential from every project key, on
// purpose and enforced at startup: a project key is handed to an
// application team and lives in their deployment, and reusing one here
// would mean any tenant could read every tenant's data. Config cannot
// set the admin token at all — it comes from POSTHORN_ADMIN_TOKEN — so
// it does not end up in the file operators paste into issues.
//
// Two ways in, deliberately different:
//
//	API — Authorization: Bearer <token>. For scripts, one request at a
//	      time, no browser involved and so no CSRF surface.
//	UI  — a login form exchanges the token for a SESSION COOKIE. The
//	      token itself never reaches the page, is never stored in
//	      localStorage, and cannot be read back by script: the cookie
//	      is HttpOnly, SameSite=Strict, and Secure over TLS.
//
// The session id is not derived from the token, so a stolen cookie
// cannot be turned back into the credential, and revoking sessions
// (a restart) does not require changing the token.

const (
	// AdminTokenEnv is the only place the admin credential comes from.
	AdminTokenEnv = "POSTHORN_ADMIN_TOKEN"

	// MinTokenLength is the shortest admin token accepted. The admin
	// surface can disable a provider and read every project's delivery
	// history, so a guessable token is not an inconvenience.
	MinTokenLength = 24

	// SessionCookie is the browser session's name. The __Host- prefix
	// is deliberate: browsers refuse to accept such a cookie unless it
	// is Secure, host-only and path=/, which means a plain-HTTP origin
	// (or a subdomain) cannot set one on the admin's behalf.
	SessionCookie = "__Host-posthorn_admin"

	// InsecureSessionCookie is used when the admin surface is served
	// over plain HTTP, which is the DEFAULT deployment: 127.0.0.1 with
	// the operator's own reverse proxy in front. The __Host- prefix
	// would make the cookie unusable there, so the name changes with
	// the guarantee rather than silently dropping the prefix's meaning.
	InsecureSessionCookie = "posthorn_admin"

	// SessionTTL bounds an idle admin session.
	SessionTTL = 8 * time.Hour

	// loginFailureBudget/Interval bound password guessing per source.
	loginFailureBudget   = 5
	loginFailureInterval = time.Minute
)

// ErrNoToken means the admin surface is not configured.
var ErrNoToken = errors.New("admin: " + AdminTokenEnv + " is not set")

// Authenticator holds the admin credential and the live sessions.
type Authenticator struct {
	// digest is the SHA-256 of the admin token. The token itself is
	// hashed at construction and not retained, so a heap dump or a
	// panic trace cannot yield it (NFR44).
	digest [sha256.Size]byte

	// secure reports whether sessions may claim the Secure attribute.
	secure bool

	mu       sync.Mutex
	sessions map[string]*session
	failures map[string]*failureCount
	now      func() time.Time
}

type session struct {
	expires   time.Time
	csrfToken string
}

type failureCount struct {
	count int
	until time.Time
}

// NewAuthenticator builds an authenticator for token.
//
// secure marks the deployment as TLS-terminated AT POSTHORN. It is not
// inferred from the request, because a request arriving on plain HTTP
// from a TLS-terminating proxy would then permanently downgrade the
// cookie for everyone.
func NewAuthenticator(token string, secure bool) (*Authenticator, error) {
	if strings.TrimSpace(token) == "" {
		return nil, ErrNoToken
	}
	if len(token) < MinTokenLength {
		return nil, errors.New("admin: " + AdminTokenEnv + " must be at least 24 characters — " +
			"this credential can read every project's delivery history")
	}
	return &Authenticator{
		digest:   sha256.Sum256([]byte(token)),
		secure:   secure,
		sessions: map[string]*session{},
		failures: map[string]*failureCount{},
	}, nil
}

func (a *Authenticator) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// MatchesToken reports whether candidate is the admin token. Constant
// time, so a timing difference cannot be used to guess it byte by byte.
func (a *Authenticator) MatchesToken(candidate string) bool {
	got := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(got[:], a.digest[:]) == 1
}

// TokenDigest exposes the credential's digest so startup can prove the
// admin token is not also a project key. A digest is safe to compare
// and useless to steal.
func (a *Authenticator) TokenDigest() [sha256.Size]byte { return a.digest }

// ErrLockedOut is returned when a source has failed too many logins.
var ErrLockedOut = errors.New("admin: too many failed logins from this address")

// Login exchanges a token for a session id, or reports why not.
//
// Failures are counted per source address: the admin surface is one
// credential with no rate limit of its own, so unbounded guessing is
// the obvious attack.
func (a *Authenticator) Login(sourceIP, token string) (sessionID string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.clock()

	if f, ok := a.failures[sourceIP]; ok {
		if now.Before(f.until) && f.count >= loginFailureBudget {
			return "", ErrLockedOut
		}
		if !now.Before(f.until) {
			delete(a.failures, sourceIP)
		}
	}

	if !a.MatchesToken(token) {
		f := a.failures[sourceIP]
		if f == nil {
			f = &failureCount{}
			a.failures[sourceIP] = f
		}
		f.count++
		f.until = now.Add(loginFailureInterval)
		return "", errors.New("admin: invalid token")
	}
	delete(a.failures, sourceIP)

	id, err := randomID()
	if err != nil {
		return "", err
	}
	csrfToken, err := randomID()
	if err != nil {
		return "", err
	}
	a.pruneLocked(now)
	a.sessions[id] = &session{expires: now.Add(SessionTTL), csrfToken: csrfToken}
	return id, nil
}

// Logout drops a session.
func (a *Authenticator) Logout(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, sessionID)
}

// ValidSession reports whether a session id is live, and returns its
// CSRF token.
func (a *Authenticator) ValidSession(id string) (csrfToken string, ok bool) {
	if id == "" {
		return "", false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, found := a.sessions[id]
	if !found {
		return "", false
	}
	if a.clock().After(s.expires) {
		delete(a.sessions, id)
		return "", false
	}
	return s.csrfToken, true
}

// pruneLocked drops expired sessions. Caller holds a.mu.
func (a *Authenticator) pruneLocked(now time.Time) {
	for id, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, id)
		}
	}
}

// SessionCount reports live sessions, for tests and the doctor command.
func (a *Authenticator) SessionCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pruneLocked(a.clock())
	return len(a.sessions)
}

// cookieName is the session cookie's name for this deployment.
func (a *Authenticator) cookieName() string {
	if a.secure {
		return SessionCookie
	}
	return InsecureSessionCookie
}

// setSessionCookie writes the session cookie.
func (a *Authenticator) setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:  a.cookieName(),
		Value: id,
		Path:  "/",
		// HttpOnly: script cannot read the session, so an XSS in the
		// admin UI cannot exfiltrate it.
		HttpOnly: true,
		// Strict rather than Lax: every admin action is state-changing
		// or sensitive, and none of them should work from a link in
		// someone else's page.
		SameSite: http.SameSiteStrictMode,
		Secure:   a.secure,
		MaxAge:   int(SessionTTL / time.Second),
	})
}

// clearSessionCookie expires the cookie.
func (a *Authenticator) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: a.cookieName(), Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.secure, MaxAge: -1,
	})
}

// sessionID reads the session cookie.
func (a *Authenticator) sessionID(r *http.Request) string {
	c, err := r.Cookie(a.cookieName())
	if err != nil {
		return ""
	}
	return c.Value
}

// authKind distinguishes how a request authenticated, because the CSRF
// rules differ: a bearer token is not sent automatically by a browser,
// a cookie is.
type authKind int

const (
	authNone authKind = iota
	authBearer
	authSession
)

// authenticate resolves a request to an auth kind.
func (a *Authenticator) authenticate(r *http.Request) (authKind, string) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if a.MatchesToken(strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))) {
			return authBearer, ""
		}
		// A wrong bearer token is a failed attempt, not a fallthrough
		// to cookie auth: falling through would let a stale script's
		// bad header be masked by a browser session.
		return authNone, ""
	}
	if csrfToken, ok := a.ValidSession(a.sessionID(r)); ok {
		return authSession, csrfToken
	}
	return authNone, ""
}

// randomID returns 256 bits of hex-encoded randomness.
func randomID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("admin: could not generate a session id")
	}
	return hex.EncodeToString(b[:]), nil
}

// checkCSRF validates a state-changing request from a browser session.
//
// Bearer-authenticated requests skip it: CSRF exists because browsers
// attach cookies automatically, and nothing attaches an Authorization
// header automatically.
func checkCSRF(kind authKind, sessionCSRF string, r *http.Request) error {
	if kind != authSession {
		return nil
	}
	supplied := r.Header.Get("X-Posthorn-CSRF")
	if supplied == "" {
		supplied = r.PostFormValue("csrf_token")
	}
	if supplied == "" {
		return errors.New("admin: this action requires a CSRF token")
	}
	if subtle.ConstantTimeCompare([]byte(supplied), []byte(sessionCSRF)) != 1 {
		return errors.New("admin: CSRF token mismatch")
	}
	return nil
}

// VerifyDistinct refuses to start when the admin token is also a
// project credential (FR143).
//
// A project key is handed to an application team and lives in their
// deployment; the admin token can read every project's delivery history
// and take providers out of rotation. Sharing one collapses the two
// blast radii into one, and it is the kind of mistake that happens by
// copy-paste rather than by intent — which is why it is checked at
// startup instead of documented.
//
// Neither side handles the other's plaintext: the comparison is
// digest-to-digest.
func VerifyDistinct(a *Authenticator, projects interface {
	HasCredentialDigest([sha256.Size]byte) bool
}) error {
	if a == nil || projects == nil {
		return nil
	}
	if projects.HasCredentialDigest(a.TokenDigest()) {
		return errors.New("admin: " + AdminTokenEnv + " is also a project credential — " +
			"the admin surface must have its own token, or any project could read every project's data")
	}
	return nil
}
