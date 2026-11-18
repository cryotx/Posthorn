package transport

import (
	"html"
	"net/mail"
	"strings"
)

// Address helpers shared by the providers whose APIs take a structured
// {email, name} pair rather than an RFC 5322 header line.
//
// Splitting through net/mail rather than by hand is the NFR1 boundary
// for these providers: the parsed parts cross as separate JSON fields,
// so a display name containing a comma, a quote or a CRLF cannot smuggle
// itself into a header.

type parsedAddress struct {
	name    string
	address string
}

// splitAddress parses "Name <a@b.com>" or "a@b.com" into its parts.
func splitAddress(s string) (parsedAddress, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		return parsedAddress{}, err
	}
	return parsedAddress{name: addr.Name, address: addr.Address}, nil
}

// htmlEscapedParagraph wraps plain text as minimal, fully escaped HTML.
//
// For providers whose API REQUIRES an HTML part (Brevo rejects a send
// with neither htmlContent nor a templateId), a text-only Posthorn
// message still needs something in that slot. Escaping first and only
// then adding <p>/<br> means submitter content is inert markup by
// construction — the same guarantee ADR-19 gives the HTML renderer.
func htmlEscapedParagraph(text string) string {
	if text == "" {
		return ""
	}
	escaped := html.EscapeString(text)
	escaped = strings.ReplaceAll(escaped, "\r\n", "\n")
	var b strings.Builder
	b.WriteString("<p>")
	b.WriteString(strings.ReplaceAll(escaped, "\n", "<br>"))
	b.WriteString("</p>")
	return b.String()
}
