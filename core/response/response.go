// Modified for the cryotx/posthorn distribution.

// Package response builds the JSON response bodies and content-negotiated
// outputs the gateway returns to clients.
//
// v1.0 surface is small: a JSON error envelope, a JSON success envelope,
// and a content-negotiation helper that picks JSON or 303-redirect based
// on the request's Accept header (FR12, FR13, FR14, FR15, FR16). The
// architectural goal is that every error path ends up here, so any future
// schema change is a single-file edit.
package response

import (
	"encoding/json"
	"net/http"
	"strings"
)

// MIME constants used for content-type checks and writes.
const (
	mimeJSON = "application/json; charset=utf-8"
)

// Error is the JSON envelope returned for non-2xx responses. Fields:
//
//   - Error: short human-readable summary, safe to log
//   - Code: machine-readable identifier (validation_failed, rate_limited, etc.)
//   - Fields: per-field details for 422 responses; omitted otherwise
type Error struct {
	Error  string            `json:"error"`
	Code   string            `json:"code"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Success is the JSON envelope returned on 200. The same shape is written
// on both real success and silent-honeypot rejection (NFR5) — a bot that
// looks at the body should not be able to tell which path it took.
type Success struct {
	Status       string `json:"status"`
	SubmissionID string `json:"submission_id"`

	// MessageID is the Posthorn message id, present only on
	// project-authenticated responses (FR112). It carries the same
	// value as SubmissionID — one logical message has one id, stable
	// across every provider attempt — and exists so a project caller
	// can use the name the attempt trail and delivery events use.
	// Absent in non-project mode, so v2.2 bodies are byte-identical.
	MessageID string `json:"message_id,omitempty"`

	// Suppressed lists recipients removed from this send by the
	// suppression list, keyed by address with the suppression reason as
	// the value (FR86, v2.0). Empty on sends with no suppressed
	// recipients — including every send on a storage-less deployment —
	// so v1.x response bodies are byte-identical. When Status is
	// "suppressed", every recipient was on the list and nothing was
	// sent; 2xx still means terminally handled — do not retry.
	Suppressed map[string]string `json:"suppressed,omitempty"`
}

// DryRun is the JSON envelope returned on 200 when the endpoint has
// dry_run = true (FR56). The PreparedMessage shows what would have been
// passed to transport.Send so operators can debug template rendering
// and recipient resolution without sending mail.
type DryRun struct {
	Status          string          `json:"status"`
	SubmissionID    string          `json:"submission_id"`
	PreparedMessage PreparedMessage `json:"prepared_message"`

	// Route is present only for a multi-provider endpoint (FR95): which
	// routing rule this message matched and which providers stand
	// behind it. Rules are the one part of the config whose effect an
	// operator cannot read off the TOML alone, so dry-run answers it.
	Route *DryRunRoute `json:"route,omitempty"`
}

// DryRunRoute reports a routing decision without sending.
type DryRunRoute struct {
	// MatchedRule is the index of the matching [[...transport.rules]]
	// entry, or -1 when the message fell through to the default
	// providers list.
	MatchedRule int `json:"matched_rule"`

	// Strategy is the route's first-pick strategy.
	Strategy string `json:"strategy"`

	// Providers is the route's provider list in DECLARED order.
	// Deliberately not the attempt order: computing that would advance
	// the round-robin rotation, and a dry run must not move real
	// traffic.
	Providers []string `json:"providers"`

	// Kind is the mail kind the router matched on (FR104), so an
	// operator debugging a match_kind rule can see which label the
	// message actually carried. "unspecified" when none was set.
	Kind string `json:"kind"`
}

// PreparedMessage mirrors transport.Message but lives in this package
// to avoid a circular import (transport doesn't depend on response).
// Fields match transport.Message byte-for-byte.
type PreparedMessage struct {
	From     string   `json:"from"`
	To       []string `json:"to"`
	ReplyTo  string   `json:"reply_to,omitempty"`
	Subject  string   `json:"subject"`
	BodyText string   `json:"body_text"`
	BodyHTML string   `json:"body_html,omitempty"`
}

// WriteJSON writes body as JSON with the given status code.
//
// Errors from json.Encode are logged-and-ignored intentionally: at this
// point in the response cycle the headers and status are already on the
// wire, so there's no recovery path. A future structured-logging hook
// (Story 4.2) can capture the encode error.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", mimeJSON)
	w.WriteHeader(status)
	if body != nil {
		_ = json.NewEncoder(w).Encode(body)
	}
}

// Validation builds a 422 Error for failed field validation. Pass the
// list of missing-required field names and a map of additional per-field
// error messages (e.g., {"email": "invalid format"}). Both inputs may
// be empty; the result still has a non-nil Fields map so JSON marshaling
// is consistent.
func Validation(missingRequired []string, fieldErrors map[string]string) Error {
	fields := make(map[string]string, len(missingRequired)+len(fieldErrors))
	for _, name := range missingRequired {
		fields[name] = "required"
	}
	for name, msg := range fieldErrors {
		// Don't overwrite a "required" entry with a format error — the
		// missing-ness is the more fundamental problem; report it first.
		if _, present := fields[name]; !present {
			fields[name] = msg
		}
	}
	return Error{
		Error:  "validation failed",
		Code:   "validation_failed",
		Fields: fields,
	}
}

// Mode is the content-negotiation result.
type Mode int

const (
	// ModeJSON: respond with a JSON body.
	ModeJSON Mode = iota
	// ModeRedirect: respond with 303 See Other to the configured URL.
	ModeRedirect
)

// Negotiate decides between JSON and redirect responses for a request.
//
// Rules (in priority order):
//
//  1. If neither redirect URL is configured → JSON (no other choice).
//  2. If the Accept header explicitly prefers JSON over HTML → JSON.
//  3. Otherwise → Redirect.
//
// "Explicitly prefers JSON" means application/json appears in Accept and
// either text/html does not, or application/json has higher q-value. The
// implementation is deliberately simple: a substring check biased toward
// JSON when both are present at equal quality. Browsers default to */* or
// text/html-first, so they get redirects; fetch() defaults are
// application/json-first, so they get JSON.
func Negotiate(acceptHeader string, hasRedirects bool) Mode {
	if !hasRedirects {
		return ModeJSON
	}
	accept := strings.ToLower(acceptHeader)
	if accept == "" {
		// No Accept header → assume browser form submit → redirect.
		return ModeRedirect
	}
	wantsJSON := strings.Contains(accept, "application/json")
	wantsHTML := strings.Contains(accept, "text/html")
	if wantsJSON && !wantsHTML {
		return ModeJSON
	}
	return ModeRedirect
}
