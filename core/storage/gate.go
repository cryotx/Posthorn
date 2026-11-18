// Modified for the cryotx/posthorn distribution.

package storage

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Gate wraps a Store with the degrade switch (FR80, NFR27). Callers ask
// Healthy() before persisting; when the answer is false they skip
// storage entirely and the gateway behaves exactly like v1.x — mail
// keeps flowing, nothing persists, no 202s are offered.
//
// State transitions are driven two ways: the periodic canary probe
// (authoritative, both directions) and ReportError (fast downgrade when
// a live write fails between probes; only a probe brings health back).
type Gate struct {
	store   *Store
	logger  *slog.Logger
	healthy atomic.Bool

	// onHealth is fired on every health transition. Set once at wiring
	// time, before serving, so it needs no lock.
	onHealth func(bool)
}

// NewGate wraps store, starting healthy.
func NewGate(store *Store, logger *slog.Logger) *Gate {
	g := &Gate{store: store, logger: logger}
	g.healthy.Store(true)
	return g
}

// Store returns the underlying store. Callers must check Healthy first.
func (g *Gate) Store() *Store { return g.store }

// Healthy reports whether persistence is currently trusted.
func (g *Gate) Healthy() bool { return g.healthy.Load() }

// ReportError downgrades immediately after a live write failure. The
// canary probe owns recovery.
func (g *Gate) ReportError(err error) {
	if g.healthy.CompareAndSwap(true, false) {
		g.logger.Error("storage_degraded",
			slog.String("error", err.Error()),
			slog.String("effect", "mail flow continues synchronously; persistence and queueing paused"),
		)
		g.notifyHealth(false)
	}
}

// OnHealthChange registers a callback fired on every health transition,
// so the metric moves with /healthz instead of waiting for the next
// canary probe.
//
// Without it the two disagreed for up to a probe interval: /healthz
// said degraded the instant a write failed, while
// posthorn_storage_healthy still read 1 — and an operator alerting on
// the metric, which is the one you actually alert on, would not see the
// outage until the probe caught up.
func (g *Gate) OnHealthChange(fn func(bool)) { g.onHealth = fn }

func (g *Gate) notifyHealth(healthy bool) {
	if g.onHealth != nil {
		g.onHealth(healthy)
	}
}

func (g *Gate) setFromProbe(err error) {
	if err == nil {
		if g.healthy.CompareAndSwap(false, true) {
			g.logger.Info("storage_recovered")
			g.notifyHealth(true)
		}
		return
	}
	if g.healthy.CompareAndSwap(true, false) {
		g.logger.Error("storage_degraded",
			slog.String("error", err.Error()),
			slog.String("effect", "mail flow continues synchronously; persistence and queueing paused"),
		)
		g.notifyHealth(false)
	}
}

// MaintenanceHooks observe the maintenance loop for metrics wiring.
// Nil funcs are skipped.
type MaintenanceHooks struct {
	OnHealth func(healthy bool)
	OnDepth  func(depth int)
}

// RunMaintenance runs the canary probe (FR80) and retention pruning
// (FR79) until ctx is canceled. probeInterval defaults to 15s,
// pruneInterval to 1h. Pruning only runs while healthy.
func (g *Gate) RunMaintenance(ctx context.Context, probeInterval, pruneInterval, retention time.Duration, hooks MaintenanceHooks) {
	if probeInterval <= 0 {
		probeInterval = 15 * time.Second
	}
	if pruneInterval <= 0 {
		pruneInterval = time.Hour
	}
	probe := time.NewTicker(probeInterval)
	prune := time.NewTicker(pruneInterval)
	defer probe.Stop()
	defer prune.Stop()

	// Immediate first probe so /healthz is accurate at startup, not
	// after the first interval.
	g.probeOnce(hooks)

	for {
		select {
		case <-ctx.Done():
			return
		case <-probe.C:
			g.probeOnce(hooks)
		case <-prune.C:
			if !g.Healthy() || retention <= 0 {
				continue
			}
			n, err := g.store.Prune(time.Now().Add(-retention))
			if err != nil {
				g.logger.Error("storage_prune_failed", slog.String("error", err.Error()))
				continue
			}
			ni, err := g.store.PruneIdempotency(time.Now())
			if err != nil {
				g.logger.Error("storage_prune_failed", slog.String("error", err.Error()))
				continue
			}
			if n+ni > 0 {
				g.logger.Info("storage_pruned",
					slog.Int64("retention_rows", n),
					slog.Int64("idempotency_rows", ni))
			}
		}
	}
}

func (g *Gate) probeOnce(hooks MaintenanceHooks) {
	err := g.store.Probe(time.Now())
	g.setFromProbe(err)
	if hooks.OnHealth != nil {
		hooks.OnHealth(g.Healthy())
	}
	if hooks.OnDepth != nil && g.Healthy() {
		if depth, derr := g.store.QueueDepth(); derr == nil {
			hooks.OnDepth(depth)
		}
	}
}
