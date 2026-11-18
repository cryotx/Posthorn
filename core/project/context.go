package project

import (
	"context"
)

// Context is what an authenticated ingress attaches to a request, and
// the only place downstream code may learn which project it is serving.
//
// HTTP and SMTP produce the same value from the same registry, so
// routing, logging, metrics, idempotency scope and events see one
// identity model rather than two (FR111).
type Context struct {
	// Project is the authenticated tenant. Never nil in a Context that
	// reached a handler.
	Project *Project
}

type ctxKey struct{}

// WithContext attaches pc to ctx.
func WithContext(ctx context.Context, pc *Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, pc)
}

// FromContext returns the attached project context, or (nil, false) in
// non-project mode.
func FromContext(ctx context.Context) (*Context, bool) {
	pc, ok := ctx.Value(ctxKey{}).(*Context)
	return pc, ok && pc != nil && pc.Project != nil
}

// NameFromContext returns the authenticated project's name, or "" when
// project mode is off. Safe to use as a metric label and a log field —
// it is operator-configured text (NFR24).
func NameFromContext(ctx context.Context) string {
	if pc, ok := FromContext(ctx); ok {
		return pc.Project.Name
	}
	return ""
}
