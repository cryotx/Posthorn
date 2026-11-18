package main

import (
	"sync/atomic"

	"github.com/craigmccaskill/posthorn/config"
)

// Worker readiness for /readyz (FR149).
//
// The background workers — the retry queue drainer, the storage
// maintenance loop, the lifecycle forwarder — start AFTER the HTTP
// listener is up. Between those two moments Posthorn will accept a
// submission and queue it, and nothing will drain the queue. That gap
// is short and usually harmless, but it is exactly what a readiness
// probe is for: hold traffic until the process can actually finish the
// work it accepts.
//
// Each worker marks itself started. A deployment that configures none
// of them (no [storage] block, so no queue to drain) is ready with none
// running, because there is nothing to wait for.
type workerReadiness struct {
	expected int32
	started  atomic.Int32
}

// newWorkerReadiness counts the workers this config will start.
func newWorkerReadiness(cfg *config.Config) *workerReadiness {
	w := &workerReadiness{}
	if cfg.Storage != nil {
		// The retry worker and the maintenance loop.
		w.expected += 2
	}
	if cfg.Lifecycle != nil {
		// The event forward queue.
		w.expected++
	}
	return w
}

// markStarted records one worker as running.
func (w *workerReadiness) markStarted() {
	if w == nil {
		return
	}
	w.started.Add(1)
}

// check reports readiness for the /readyz handler.
func (w *workerReadiness) check() (bool, string) {
	if w == nil || w.expected == 0 {
		return true, "none required"
	}
	started := w.started.Load()
	if started < w.expected {
		return false, "starting"
	}
	return true, "ok"
}
