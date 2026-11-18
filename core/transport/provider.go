package transport

import (
	"fmt"
	"sort"
	"sync/atomic"
)

// Provider is one named, built entry in the routing pool: a concrete
// Transport plus the operator-facing identity and the health state that
// routing decisions read.
//
// A Provider is shared. Every endpoint (and the SMTP listener) that
// names the same pool entry gets the same *Provider, so its circuit
// breaker observes the provider's health across the whole process
// rather than per endpoint — which is the point of having a pool at
// all. Nothing on this struct is mutated after Pool construction; the
// only mutable state lives behind Breaker's own mutex.
type Provider struct {
	// Name is the operator-chosen pool key ("postmark-prod"). It is the
	// value that reaches logs and metric labels, so it is operator
	// content by construction (NFR24).
	Name string

	// Type is the registered transport type ("postmark", "ses", ...).
	Type string

	// Weight is the relative share for the weighted strategy. Always
	// >= 1 after Pool construction; ignored by failover and round_robin.
	Weight int

	// Transport is the built client. Never nil.
	Transport Transport

	// Breaker guards this provider's share of the traffic.
	Breaker *Breaker

	// Quota is this provider's send budget (FR99). The zero value is
	// unlimited, which is what every pre-v2.2 config produces.
	Quota Quota

	// Cost is the price per 1000 messages as fixed-point integer
	// millionths (FR101). Zero means free, which is what the
	// free_first strategy partitions on. Never a float — see Cost.
	Cost Cost

	// adminDisabled is the operator's kill switch (FR144), reachable
	// during an incident without an edit-and-restart.
	//
	// It can only ever SUBTRACT from what the config permits: a
	// provider absent from the config cannot be summoned by clearing a
	// flag, and clearing this restores exactly the configured
	// behaviour. So design principle 4 holds — the config file is still
	// the source of truth for what is permitted — while an operator can
	// still take a misbehaving upstream out of rotation immediately.
	adminDisabled atomic.Bool
}

// SetAdminDisabled takes the provider out of, or back into, rotation.
func (p *Provider) SetAdminDisabled(disabled bool) { p.adminDisabled.Store(disabled) }

// AdminDisabled reports whether an operator has disabled this provider.
func (p *Provider) AdminDisabled() bool { return p.adminDisabled.Load() }

// Pool is the process-wide set of named providers, built once at
// startup from the top-level [[providers]] blocks and shared by every
// router that references them.
//
// The pool is immutable after construction — config files are the
// source of truth and there is no runtime mutation surface (design
// principle 4).
type Pool struct {
	byName map[string]*Provider
}

// NewPool builds a pool from already-constructed providers, rejecting
// duplicate and empty names. Providers arrive built because construction
// needs the registry, which the config layer already drives.
func NewPool(providers []*Provider) (*Pool, error) {
	p := &Pool{byName: make(map[string]*Provider, len(providers))}
	for _, prov := range providers {
		if prov == nil {
			return nil, fmt.Errorf("providers: nil entry")
		}
		if prov.Name == "" {
			return nil, fmt.Errorf("providers: name is required")
		}
		if prov.Transport == nil {
			return nil, fmt.Errorf("providers[%s]: transport is required", prov.Name)
		}
		if _, dup := p.byName[prov.Name]; dup {
			return nil, fmt.Errorf("providers: duplicate name %q", prov.Name)
		}
		if prov.Weight <= 0 {
			prov.Weight = 1
		}
		if prov.Breaker == nil {
			prov.Breaker = NewBreaker(BreakerConfig{})
		}
		p.byName[prov.Name] = prov
	}
	return p, nil
}

// Get returns the named provider, or (nil, false).
func (p *Pool) Get(name string) (*Provider, bool) {
	if p == nil {
		return nil, false
	}
	prov, ok := p.byName[name]
	return prov, ok
}

// Resolve maps a list of pool names to providers, preserving order.
// Returns a descriptive error naming the first unknown entry — this is
// the message an operator sees for a typo'd provider reference, so it
// lists what is actually defined.
func (p *Pool) Resolve(names []string) ([]*Provider, error) {
	out := make([]*Provider, 0, len(names))
	for _, n := range names {
		prov, ok := p.Get(n)
		if !ok {
			return nil, fmt.Errorf("unknown provider %q (defined: %v)", n, p.Names())
		}
		out = append(out, prov)
	}
	return out, nil
}

// Names returns the defined provider names, sorted for stable errors.
func (p *Pool) Names() []string {
	if p == nil {
		return nil
	}
	out := make([]string, 0, len(p.byName))
	for n := range p.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// All returns every provider, ordered by name. Used for startup logging
// and the /healthz provider report.
func (p *Pool) All() []*Provider {
	if p == nil {
		return nil
	}
	out := make([]*Provider, 0, len(p.byName))
	for _, n := range p.Names() {
		out = append(out, p.byName[n])
	}
	return out
}

// Len reports how many providers are in the pool.
func (p *Pool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.byName)
}

// States returns a name → circuit-state snapshot. Read-only; does not
// consume half-open probe slots.
func (p *Pool) States() map[string]CircuitState {
	if p == nil {
		return nil
	}
	out := make(map[string]CircuitState, len(p.byName))
	for n, prov := range p.byName {
		out[n] = prov.Breaker.State()
	}
	return out
}
