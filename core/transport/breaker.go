package transport

import (
	"sync"
	"time"
)

// CircuitState is the health state of one provider in a routing pool.
//
// The breaker exists because a multi-provider gateway that keeps dialling
// a dead provider spends its whole request budget discovering the same
// outage on every submission. Taking a failing provider out of rotation
// for a cooldown converts "every message pays the timeout" into "one
// probe per cooldown pays it".
type CircuitState int

const (
	// CircuitClosed is the healthy state: sends go through normally.
	CircuitClosed CircuitState = iota

	// CircuitOpen means the provider tripped its failure threshold and is
	// out of rotation until the cooldown expires.
	CircuitOpen

	// CircuitHalfOpen means the cooldown has expired and a bounded number
	// of probe sends are allowed through. A probe success closes the
	// circuit; a probe failure re-opens it for another cooldown.
	CircuitHalfOpen
)

// String returns a stable label for logs and metrics (NFR24-safe: the
// value space is a closed enum, never submitter content).
func (s CircuitState) String() string {
	switch s {
	case CircuitOpen:
		return "open"
	case CircuitHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// Circuit-breaker defaults. Deliberately forgiving: the breaker is a
// latency optimisation for a gateway whose first doctrine is "mail
// first" (NFR27), not an availability gate. Five consecutive
// health-class failures is well past noise; thirty seconds is short
// enough that a blip self-heals inside one provider incident.
const (
	DefaultBreakerFailureThreshold = 5
	DefaultBreakerCooldown         = 30 * time.Second
	DefaultBreakerHalfOpenProbes   = 1
)

// BreakerConfig tunes one provider's circuit breaker. The zero value is
// valid and means "all defaults".
type BreakerConfig struct {
	// FailureThreshold is the number of consecutive health-class
	// failures that opens the circuit. Zero means the default.
	FailureThreshold int

	// Cooldown is how long the circuit stays open before admitting
	// probes. Zero means the default.
	Cooldown time.Duration

	// HalfOpenProbes is how many concurrent probe sends are admitted in
	// the half-open state. Zero means the default.
	HalfOpenProbes int

	// Disabled turns the breaker off entirely: every send is admitted
	// and failures are recorded for metrics only. For operators who
	// would rather pay the timeout than ever skip a provider.
	Disabled bool
}

// Breaker is one provider's circuit breaker. Safe for concurrent use —
// a pooled provider is shared by every endpoint that names it, so its
// health state is process-wide by construction.
//
// Only *health-class* failures (ErrTransient, ErrRateLimited) count
// toward the threshold. An ErrTerminal is a statement about the
// message ("that From domain isn't verified"), not about the provider,
// and penalising a healthy provider for a malformed message would take
// the whole pool down one bad submission at a time.
type Breaker struct {
	cfg BreakerConfig

	// now is injected for tests. Nil means time.Now.
	now func() time.Time

	mu           sync.Mutex
	state        CircuitState
	consecutive  int
	openedAt     time.Time
	probesInUse  int
	onTransition func(CircuitState)
}

// NewBreaker builds a breaker from cfg, applying defaults for zero fields.
func NewBreaker(cfg BreakerConfig) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = DefaultBreakerFailureThreshold
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = DefaultBreakerCooldown
	}
	if cfg.HalfOpenProbes <= 0 {
		cfg.HalfOpenProbes = DefaultBreakerHalfOpenProbes
	}
	return &Breaker{cfg: cfg, state: CircuitClosed}
}

// OnTransition registers a callback fired on every state change. Used to
// wire the circuit-state gauge without importing metrics here (the
// storage.Hooks pattern). Not safe to call concurrently with Send paths;
// call it once at construction.
func (b *Breaker) OnTransition(fn func(CircuitState)) { b.onTransition = fn }

func (b *Breaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// Allow reports whether a send may be attempted, transitioning
// open → half-open when the cooldown has expired. When it returns true
// for a half-open probe, the caller MUST report the outcome via Success
// or Failure, or the probe slot leaks.
func (b *Breaker) Allow() bool {
	if b.cfg.Disabled {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case CircuitClosed:
		return true
	case CircuitOpen:
		if b.clock().Sub(b.openedAt) < b.cfg.Cooldown {
			return false
		}
		b.transition(CircuitHalfOpen)
		b.probesInUse = 1
		return true
	default: // CircuitHalfOpen
		if b.probesInUse >= b.cfg.HalfOpenProbes {
			return false
		}
		b.probesInUse++
		return true
	}
}

// Success records a delivered message, closing the circuit and clearing
// the consecutive-failure count.
func (b *Breaker) Success() {
	if b.cfg.Disabled {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	b.probesInUse = 0
	if b.state != CircuitClosed {
		b.transition(CircuitClosed)
	}
}

// Failure records a failed attempt. Only health-class errors (transient,
// rate-limited) move the breaker; terminal errors reset nothing and trip
// nothing — see the type comment.
func (b *Breaker) Failure(err error) {
	if b.cfg.Disabled || !IsRetryable(err) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == CircuitHalfOpen {
		// A probe failed: straight back to open for another cooldown.
		b.probesInUse = 0
		b.openedAt = b.clock()
		b.transition(CircuitOpen)
		return
	}

	b.consecutive++
	if b.state == CircuitClosed && b.consecutive >= b.cfg.FailureThreshold {
		b.openedAt = b.clock()
		b.transition(CircuitOpen)
	}
}

// State returns the current state, applying the cooldown expiry without
// consuming a probe slot. Read-only: for logs, /healthz and the gauge.
func (b *Breaker) State() CircuitState {
	if b.cfg.Disabled {
		return CircuitClosed
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == CircuitOpen && b.clock().Sub(b.openedAt) >= b.cfg.Cooldown {
		return CircuitHalfOpen
	}
	return b.state
}

// Reset forces the breaker closed and clears its failure count (FR144).
//
// For the case where an operator KNOWS the upstream is healthy again —
// they just fixed the credential, or the provider posted an all-clear —
// and would otherwise wait out a cooldown for information they already
// have. It cannot make a broken provider work: the next failure trips
// the breaker again, which is why this is safe to expose.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consecutive = 0
	b.probesInUse = 0
	b.openedAt = time.Time{}
	b.transition(CircuitClosed)
}

// transition sets the state and fires the callback. Caller holds b.mu;
// the callback runs under the lock, so it must not re-enter the breaker
// (the metrics gauge setter doesn't).
func (b *Breaker) transition(to CircuitState) {
	if b.state == to {
		return
	}
	b.state = to
	if to == CircuitClosed {
		b.consecutive = 0
	}
	if b.onTransition != nil {
		b.onTransition(to)
	}
}
