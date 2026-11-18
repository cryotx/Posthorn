package transport

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Strategy selects which provider in a route gets the first attempt.
// Failover order after that first pick is always the route's declared
// order minus the already-tried providers — a strategy decides where a
// message starts, never whether it may move on.
type Strategy string

const (
	// StrategyFailover always starts at the first declared provider and
	// walks down the list. The default: an operator who lists a primary
	// and a backup means exactly that.
	StrategyFailover Strategy = "failover"

	// StrategyRoundRobin spreads first attempts evenly across the route,
	// ignoring weights.
	StrategyRoundRobin Strategy = "round_robin"

	// StrategyWeighted spreads first attempts in proportion to each
	// provider's weight.
	StrategyWeighted Strategy = "weighted"

	// StrategyFreeFirst tries every zero-cost provider before any paid
	// one, each tier in declared order (FR102). Quota exhaustion is
	// what moves traffic off the free tier: an exhausted provider is
	// skipped at send time, so the order needs no quota awareness of
	// its own.
	StrategyFreeFirst Strategy = "free_first"

	// StrategyCheapest orders every provider by price ascending, ties
	// broken by declared order (FR102).
	StrategyCheapest Strategy = "cheapest"
)

// ParseStrategy validates a config string. Empty means the default.
func ParseStrategy(s string) (Strategy, error) {
	switch Strategy(s) {
	case "":
		return StrategyFailover, nil
	case StrategyFailover, StrategyRoundRobin, StrategyWeighted, StrategyFreeFirst, StrategyCheapest:
		return Strategy(s), nil
	}
	return "", fmt.Errorf("unknown strategy %q (known: failover, round_robin, weighted, free_first, cheapest)", s)
}

// RuleMatch is the predicate half of a routing rule. A nil *RuleMatch
// matches everything (the default route). All populated criteria must
// match — criteria AND together, values within one criterion OR.
type RuleMatch struct {
	// RecipientDomains are patterns matched against the domain of every
	// recipient: an exact "gmail.com" or a "*.edu" suffix wildcard.
	//
	// The rule matches only when EVERY recipient matches at least one
	// pattern. "Any recipient" would let one gmail.com address drag a
	// whole multi-recipient message onto the bulk provider, and
	// splitting the message per-domain would be fan-out — deliberately
	// out of scope, since two provider message-IDs for one submission
	// breaks the submission log's one-row-one-send shape.
	RecipientDomains []string

	// Fields matches operator-named submitted fields against
	// operator-listed values. The rule matches when, for every key, the
	// message carries at least one of the listed values.
	//
	// Submitter data selects among operator-written alternatives here;
	// it never names a provider. A submitter can steer themselves into
	// a route the operator already sanctioned and nowhere else.
	Fields map[string][]string

	// Kinds matches the message's MailKind (FR104) — the criterion that
	// makes "verification goes to the fast provider, notifications go
	// to the free pool" a one-line rule. Values are validated against
	// the closed MailKind set at config-parse time.
	Kinds []MailKind
}

// Matches reports whether msg satisfies every populated criterion.
func (m *RuleMatch) Matches(msg Message) bool {
	if m == nil {
		return true
	}
	if len(m.RecipientDomains) > 0 && !m.matchesRecipients(msg.To) {
		return false
	}
	if len(m.Fields) > 0 && !m.matchesFields(msg.Fields) {
		return false
	}
	if len(m.Kinds) > 0 && !m.matchesKind(msg.Kind) {
		return false
	}
	return true
}

func (m *RuleMatch) matchesKind(kind MailKind) bool {
	for _, k := range m.Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func (m *RuleMatch) matchesRecipients(to []string) bool {
	if len(to) == 0 {
		return false
	}
	for _, addr := range to {
		if !matchesAnyDomain(addr, m.RecipientDomains) {
			return false
		}
	}
	return true
}

func (m *RuleMatch) matchesFields(fields map[string][]string) bool {
	for key, wanted := range m.Fields {
		got, ok := fields[key]
		if !ok {
			return false
		}
		if !intersects(got, wanted) {
			return false
		}
	}
	return true
}

// matchesAnyDomain reports whether addr's domain satisfies any pattern.
// Comparison is case-insensitive; patterns are lowercased at config
// validation time.
func matchesAnyDomain(addr string, patterns []string) bool {
	at := strings.LastIndex(addr, "@")
	if at < 0 || at == len(addr)-1 {
		return false
	}
	domain := strings.ToLower(strings.TrimSuffix(addr[at+1:], "."))
	// A display-name form ("Name <a@b.com>") leaves a trailing angle.
	domain = strings.TrimSuffix(domain, ">")
	for _, p := range patterns {
		if suffix, ok := strings.CutPrefix(p, "*."); ok {
			// "*.edu" covers foo.edu and a.foo.edu, not "edu" itself.
			if strings.HasSuffix(domain, "."+suffix) || domain == suffix {
				return true
			}
			continue
		}
		if domain == p {
			return true
		}
	}
	return false
}

func intersects(got, wanted []string) bool {
	for _, g := range got {
		for _, w := range wanted {
			if g == w {
				return true
			}
		}
	}
	return false
}

// Route is one provider list plus the strategy that orders it. The
// default route has a nil Match; rule routes carry theirs.
type Route struct {
	Match     *RuleMatch
	Strategy  Strategy
	Providers []*Provider

	// mu guards the smooth-weighted-round-robin counters. One route's
	// rotation is independent of every other route's.
	mu      sync.Mutex
	current []int

	// costOrder is the attempt order for the cost strategies. Prices
	// are immutable after config load, so it is computed once at
	// construction rather than sorted per message.
	costOrder []*Provider
}

// NewRoute builds a route, defaulting the strategy and initialising the
// rotation counters.
func NewRoute(match *RuleMatch, strategy Strategy, providers []*Provider) (*Route, error) {
	if len(providers) == 0 {
		return nil, fmt.Errorf("route: at least one provider required")
	}
	s, err := ParseStrategy(string(strategy))
	if err != nil {
		return nil, err
	}
	route := &Route{
		Match:     match,
		Strategy:  s,
		Providers: providers,
		current:   make([]int, len(providers)),
	}
	if s == StrategyFreeFirst || s == StrategyCheapest {
		route.costOrder = orderByCost(providers, s == StrategyFreeFirst)
	}
	return route, nil
}

// orderByCost builds the static attempt order for the cost strategies.
//
// freeFirst partitions into zero-cost and paid, preserving declared
// order inside each tier; otherwise providers sort by price ascending
// with declared order as the tie-break. Both are stable, so the same
// config always produces the same order — an operator reading the TOML
// can predict where a message goes without running it.
func orderByCost(providers []*Provider, freeFirst bool) []*Provider {
	idx := make(map[*Provider]int, len(providers))
	for i, p := range providers {
		idx[p] = i
	}
	out := append([]*Provider(nil), providers...)
	sort.SliceStable(out, func(a, b int) bool {
		pa, pb := out[a], out[b]
		if freeFirst {
			fa, fb := pa.Cost.Free(), pb.Cost.Free()
			if fa != fb {
				return fa
			}
			return idx[pa] < idx[pb]
		}
		if pa.Cost != pb.Cost {
			return pa.Cost < pb.Cost
		}
		return idx[pa] < idx[pb]
	})
	return out
}

// order returns the full attempt order for one message: the strategy's
// pick first, then the remaining providers in declared order. Every
// call advances the rotation exactly once, so a message that never
// leaves its first provider still counts as that provider's turn.
func (r *Route) order() []*Provider {
	if len(r.Providers) == 1 {
		return r.Providers
	}

	switch r.Strategy {
	case StrategyFreeFirst, StrategyCheapest:
		// Deterministic and precomputed; nothing to rotate.
		return r.costOrder
	}

	first := 0
	switch r.Strategy {
	case StrategyRoundRobin:
		first = r.advance(func(int) int { return 1 })
	case StrategyWeighted:
		first = r.advance(func(i int) int { return r.Providers[i].Weight })
	default: // StrategyFailover
		first = 0
	}

	out := make([]*Provider, 0, len(r.Providers))
	out = append(out, r.Providers[first])
	for i, p := range r.Providers {
		if i != first {
			out = append(out, p)
		}
	}
	return out
}

// advance runs one step of smooth weighted round-robin (the nginx
// algorithm) and returns the winning index. Chosen over weighted-random
// because it is deterministic — the distribution is exact over each
// full cycle instead of exact only in expectation, which makes both the
// tests and an operator's traffic split readable.
func (r *Route) advance(weight func(int) int) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	total := 0
	best := 0
	for i := range r.Providers {
		w := weight(i)
		total += w
		r.current[i] += w
		if r.current[i] > r.current[best] {
			best = i
		}
	}
	r.current[best] -= total
	return best
}

// RouterHooks are optional observation points, mirroring storage.Hooks:
// the router stays free of a metrics import and the cmd layer decides
// what to count. Nil funcs are skipped. Every value passed is
// operator-configured (provider names, a closed outcome enum, the
// router's own name), never submitter content — NFR24 holds by
// construction.
type RouterHooks struct {
	// OnAttempt fires once per provider attempt. outcome is "sent" or a
	// TransportError class name ("transient", "rate_limited",
	// "terminal", "unknown").
	OnAttempt func(provider, outcome string, latency time.Duration)

	// OnSkipped fires when a provider is passed over because its
	// circuit is open.
	OnSkipped func(provider string)

	// OnQuotaBlocked fires when a provider is passed over because a
	// configured send budget is spent. window is "daily" or "monthly".
	// Distinct from OnSkipped on purpose: an operator triaging a
	// gateway needs to tell "my provider is broken" apart from "I am
	// out of allowance", and the two want different responses.
	OnQuotaBlocked func(provider, window string)

	// OnFailover fires once per message that was delivered by a
	// provider other than the one the strategy picked first.
	OnFailover func(router string)

	// OnExhausted fires when every candidate in the route failed or was
	// skipped and the message could not be delivered.
	OnExhausted func(router string)

	// OnRouteQuotaExhausted fires when the route failed *specifically*
	// because every provider was out of budget and none was unhealthy.
	OnRouteQuotaExhausted func(router string)

	// OnAttemptRecord fires once per completed provider attempt with
	// the full per-attempt detail (FR112). Separate from OnAttempt,
	// which stays the narrow metrics hook: this one carries the
	// message id and provider message id a durable audit trail needs,
	// and the metrics path must not grow those (NFR24).
	OnAttemptRecord func(AttemptRecord)
}

// AttemptRecord is one provider attempt, for the durable per-message
// trail (FR112). Deliberately carries no message content: it answers
// "who took this and what happened", which needs no body, recipients
// or subject.
type AttemptRecord struct {
	// MessageID is the Posthorn message id — stable across every
	// attempt for one logical message, which is the whole point.
	MessageID string

	// Attempt is 1-based within this Send call.
	Attempt int

	Provider   string
	StartedAt  time.Time
	FinishedAt time.Time

	// Result is "sent" or "failed".
	Result string

	// ErrorClass is the ErrorClass.String() value on failure, empty on
	// success.
	ErrorClass string

	// ProviderMessageID is the upstream's own id, when it returned one.
	ProviderMessageID string
}

// Router is a Transport that fronts several providers.
//
// Making the router a Transport rather than a new concept above one is
// what keeps the multi-provider change additive: the gateway handler,
// the SMTP session, transport.SendWithRetry and the storage retry
// worker all keep calling Send on an interface and none of them learn
// that a pool exists.
//
// Composition with SendWithRetry is deliberate. The FR19-22 policy
// wraps the router, so a pool that exhausts every provider on transient
// errors gets one more full pass after the backoff, still inside the
// caller's hard timeout (FR22) — the retry policy did not have to grow
// a provider dimension to keep working.
type Router struct {
	// Name is the router's owner — an endpoint path or "smtp_listener".
	// Log and metric label only.
	Name string

	// Default is the route used when no rule matches. Required.
	Default *Route

	// Rules are evaluated in declared order; the first match wins.
	Rules []*Route

	// Quota gates every attempt on the provider's remaining budget.
	// Nil disables quota entirely, which is the shape of every config
	// that sets no limits.
	Quota QuotaSource

	// Now is injected for tests. Nil means time.Now.
	Now func() time.Time

	Hooks RouterHooks
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// NewRouter builds a router. def is required; rules may be empty.
func NewRouter(name string, def *Route, rules []*Route) (*Router, error) {
	if def == nil {
		return nil, fmt.Errorf("router %q: default route is required", name)
	}
	return &Router{Name: name, Default: def, Rules: rules}, nil
}

// Providers returns every provider reachable from this router, default
// route first, de-duplicated. Used for startup logging.
func (r *Router) Providers() []*Provider {
	seen := map[string]bool{}
	var out []*Provider
	add := func(route *Route) {
		for _, p := range route.Providers {
			if !seen[p.Name] {
				seen[p.Name] = true
				out = append(out, p)
			}
		}
	}
	add(r.Default)
	for _, rule := range r.Rules {
		add(rule)
	}
	return out
}

// Route returns the route a message would take. Exported for the
// dry-run path, which reports the decision without sending.
func (r *Router) Route(msg Message) *Route {
	for _, rule := range r.Rules {
		if rule.Match.Matches(msg) {
			return rule
		}
	}
	return r.Default
}

// RouteExplanation describes the route a message would take, for the
// dry-run response. Providers are in DECLARED order, not attempt order:
// computing the attempt order would advance the round-robin rotation,
// and a debugging request should not move production traffic. The
// strategy tells the operator how the first pick is chosen from this
// list.
type RouteExplanation struct {
	// RuleIndex is the matched rule's position, or -1 for the default
	// route.
	RuleIndex int

	Strategy  string
	Providers []string

	// Kind is the message's routing label, as the rules saw it.
	Kind MailKind
}

// Explain reports which route a message would take without sending it
// or perturbing any rotation state.
func (r *Router) Explain(msg Message) RouteExplanation {
	route := r.Default
	idx := -1
	for i, rule := range r.Rules {
		if rule.Match.Matches(msg) {
			route, idx = rule, i
			break
		}
	}
	names := make([]string, 0, len(route.Providers))
	for _, p := range route.Providers {
		names = append(names, p.Name)
	}
	return RouteExplanation{RuleIndex: idx, Strategy: string(route.Strategy), Providers: names, Kind: msg.Kind}
}

// Send implements Transport: pick a route, walk its providers in the
// strategy's order, and stop at the first success.
//
// Failover is health-class only. An ErrTerminal ends the walk
// immediately: the provider has said this *message* is unacceptable
// (unverified From domain, malformed address, payload too large), and
// offering the same message to every remaining provider would turn one
// rejection into N rejections against N reputations. Only ErrTransient
// and ErrRateLimited — the classes that mean "this provider, right
// now" — advance to the next candidate.
func (r *Router) Send(ctx context.Context, msg Message) (SendResult, error) {
	route := r.Route(msg)
	candidates := route.order()

	var lastErr error
	attempts := 0
	circuitSkipped := 0
	quotaSkipped := 0
	disabledSkipped := 0

	for _, p := range candidates {
		if ctx.Err() != nil {
			break
		}

		// Quota is checked BEFORE the breaker, and never touches it.
		// Spending the last of a free tier is a success, not a health
		// event; letting it trip a circuit would take a perfectly
		// healthy provider out of rotation for a cooldown it does not
		// need, and would make the metrics lie about why.
		//
		// reserveQuota takes the allowance up front where the source
		// supports it, so two concurrent sends cannot both observe the
		// last unit of budget (ADR-36). commit(false) hands it back.
		commit, allowed, window := r.reserveQuota(p)
		if !allowed {
			quotaSkipped++
			if r.Hooks.OnQuotaBlocked != nil {
				r.Hooks.OnQuotaBlocked(p.Name, window)
			}
			continue
		}

		// An admin-disabled provider is skipped the same way an open
		// circuit is: the route continues to the next candidate rather
		// than failing, which is what makes disabling one provider in a
		// failover pool a safe thing to do mid-incident.
		if p.AdminDisabled() {
			commit(false)
			disabledSkipped++
			if r.Hooks.OnSkipped != nil {
				r.Hooks.OnSkipped(p.Name)
			}
			continue
		}

		if !p.Breaker.Allow() {
			commit(false)
			circuitSkipped++
			if r.Hooks.OnSkipped != nil {
				r.Hooks.OnSkipped(p.Name)
			}
			continue
		}

		start := time.Now()
		res, err := p.Transport.Send(ctx, msg)
		finished := time.Now()
		latency := finished.Sub(start)
		attempts++

		if err == nil {
			p.Breaker.Success()
			// Only a delivered message spends allowance. Committing on
			// attempt would let a broken provider burn the operator's
			// paid budget by failing.
			commit(true)
			if r.Hooks.OnAttempt != nil {
				r.Hooks.OnAttempt(p.Name, "sent", latency)
			}
			r.recordAttempt(msg, attempts, p, start, finished, "sent", "", res.MessageID)
			if attempts > 1 && r.Hooks.OnFailover != nil {
				r.Hooks.OnFailover(r.Name)
			}
			res.Provider = p.Name
			return res, nil
		}

		commit(false)
		p.Breaker.Failure(err)
		if r.Hooks.OnAttempt != nil {
			r.Hooks.OnAttempt(p.Name, errorClass(err), latency)
		}
		r.recordAttempt(msg, attempts, p, start, finished, "failed", errorClass(err), "")
		lastErr = annotateProvider(err, p.Name)

		if !IsRetryable(err) {
			return SendResult{}, lastErr
		}
	}

	if r.Hooks.OnExhausted != nil {
		r.Hooks.OnExhausted(r.Name)
	}

	if lastErr != nil {
		return SendResult{}, lastErr
	}

	// Nothing was attempted. Which of the two reasons gets reported
	// matters, because they call for different operator responses:
	// an open circuit means something is broken and will likely clear
	// in a cooldown; a spent budget means everything is working and
	// the answer is to wait for the window or raise a limit.
	//
	// An open circuit wins the tie: "something is failing" is the more
	// urgent of the two, and it is also the shorter wait.
	if circuitSkipped > 0 {
		return SendResult{}, &TransportError{
			Class: ErrTransient,
			Message: fmt.Sprintf("no provider available in route (%d circuit(s) open, %d out of quota, %d disabled by an operator)",
				circuitSkipped, quotaSkipped, disabledSkipped),
		}
	}
	if quotaSkipped > 0 {
		if r.Hooks.OnRouteQuotaExhausted != nil {
			r.Hooks.OnRouteQuotaExhausted(r.Name)
		}
		return SendResult{}, &TransportError{
			Class: ErrQuotaExhausted,
			Message: fmt.Sprintf("every provider in route has spent its send quota (%d provider(s))",
				quotaSkipped),
		}
	}
	if disabledSkipped > 0 {
		// Named separately from an open circuit: this is a state an
		// operator CHOSE, and the fix is to re-enable rather than to
		// wait for a cooldown.
		return SendResult{}, &TransportError{
			Class: ErrTransient,
			Message: fmt.Sprintf("every provider in route is disabled by an operator (%d provider(s))",
				disabledSkipped),
		}
	}
	return SendResult{}, &TransportError{
		Class:   ErrTransient,
		Message: fmt.Sprintf("no provider available in route (%d candidate(s))", len(candidates)),
	}
}

// reserveQuota takes budget for one attempt. It returns a commit
// function the caller must always call — commit(true) keeps the unit,
// commit(false) returns it.
//
// A source implementing Reserver gets strict semantics: the allowance
// is taken before the send, so concurrent sends cannot both see the
// last unit. A source that only implements Check/Record keeps the
// advisory check-then-record shape, which is all a remote,
// eventually-consistent source can honestly offer.
func (r *Router) reserveQuota(p *Provider) (commit func(bool), allowed bool, window string) {
	noop := func(bool) {}
	if r.Quota == nil {
		return noop, true, ""
	}
	now := r.now()
	if res, ok := r.Quota.(Reserver); ok {
		return res.Reserve(p, now)
	}
	v := r.Quota.Check(p, now)
	if !v.Allowed {
		return noop, false, string(v.Window)
	}
	return func(kept bool) {
		if kept {
			r.Quota.Record(p, now)
		}
	}, true, ""
}

// recordAttempt fires the per-attempt trail hook, if wired.
func (r *Router) recordAttempt(msg Message, n int, p *Provider, start, finish time.Time, result, class, providerMsgID string) {
	if r.Hooks.OnAttemptRecord == nil || msg.SubmissionID == "" {
		return
	}
	r.Hooks.OnAttemptRecord(AttemptRecord{
		MessageID:         msg.SubmissionID,
		Attempt:           n,
		Provider:          p.Name,
		StartedAt:         start,
		FinishedAt:        finish,
		Result:            result,
		ErrorClass:        class,
		ProviderMessageID: providerMsgID,
	})
}

// errorClass names the class of err for the attempt hook.
func errorClass(err error) string {
	var te *TransportError
	if !errors.As(err, &te) {
		return ErrUnknown.String()
	}
	return te.Class.String()
}

// annotateProvider stamps the failing provider onto a TransportError so
// the operator-facing message says which pool entry rejected the send.
// A copy is returned; the original is left untouched.
func annotateProvider(err error, provider string) error {
	var te *TransportError
	if !errors.As(err, &te) {
		return err
	}
	if te.Provider == provider {
		return err
	}
	clone := *te
	clone.Provider = provider
	return &clone
}
