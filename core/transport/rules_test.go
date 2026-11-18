package transport

import (
	"context"
	"testing"
)

// --- domain matching ---------------------------------------------------

func TestRuleMatch_RecipientDomains(t *testing.T) {
	for _, tc := range []struct {
		name     string
		patterns []string
		to       []string
		want     bool
	}{
		{"exact match", []string{"gmail.com"}, []string{"a@gmail.com"}, true},
		{"exact mismatch", []string{"gmail.com"}, []string{"a@outlook.com"}, false},
		{"case-insensitive address", []string{"gmail.com"}, []string{"A@GMail.CoM"}, true},
		{"wildcard one label", []string{"*.edu"}, []string{"a@mit.edu"}, true},
		{"wildcard nested label", []string{"*.edu"}, []string{"a@cs.mit.edu"}, true},
		{"wildcard matches the bare suffix", []string{"*.edu"}, []string{"a@edu"}, true},
		{"wildcard does not match a different tld", []string{"*.edu"}, []string{"a@mit.com"}, false},
		{"wildcard is not a substring match", []string{"*.example.com"}, []string{"a@notexample.com"}, false},
		{"any of several patterns", []string{"gmail.com", "*.edu"}, []string{"a@mit.edu"}, true},
		{"trailing dot tolerated", []string{"gmail.com"}, []string{"a@gmail.com."}, true},
		{"display-name form", []string{"gmail.com"}, []string{"Alice <a@gmail.com>"}, true},
		{"no at-sign", []string{"gmail.com"}, []string{"not-an-address"}, false},
		{"empty domain", []string{"gmail.com"}, []string{"a@"}, false},
		{"no recipients", []string{"gmail.com"}, nil, false},

		// Every recipient must match. "Any" would let one gmail address
		// drag a whole multi-recipient message onto the bulk provider,
		// and splitting per-domain would be fan-out.
		{"all recipients match", []string{"gmail.com"}, []string{"a@gmail.com", "b@gmail.com"}, true},
		{"one recipient off-pattern", []string{"gmail.com"}, []string{"a@gmail.com", "b@outlook.com"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &RuleMatch{RecipientDomains: tc.patterns}
			if got := m.Matches(Message{To: tc.to}); got != tc.want {
				t.Errorf("Matches(%v) = %v, want %v", tc.to, got, tc.want)
			}
		})
	}
}

// --- field matching ----------------------------------------------------

func TestRuleMatch_Fields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		match  map[string][]string
		fields map[string][]string
		want   bool
	}{
		{"single value hit", map[string][]string{"plan": {"enterprise"}},
			map[string][]string{"plan": {"enterprise"}}, true},
		{"one of several values", map[string][]string{"plan": {"pro", "enterprise"}},
			map[string][]string{"plan": {"enterprise"}}, true},
		{"value miss", map[string][]string{"plan": {"enterprise"}},
			map[string][]string{"plan": {"free"}}, false},
		{"missing key", map[string][]string{"plan": {"enterprise"}},
			map[string][]string{"tier": {"enterprise"}}, false},
		{"nil fields (SMTP ingress)", map[string][]string{"plan": {"enterprise"}}, nil, false},
		{"keys AND together", map[string][]string{"plan": {"pro"}, "region": {"eu"}},
			map[string][]string{"plan": {"pro"}, "region": {"eu"}}, true},
		{"one key of several misses", map[string][]string{"plan": {"pro"}, "region": {"eu"}},
			map[string][]string{"plan": {"pro"}, "region": {"us"}}, false},
		{"multi-valued field, one hit", map[string][]string{"tag": {"urgent"}},
			map[string][]string{"tag": {"newsletter", "urgent"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &RuleMatch{Fields: tc.match}
			if got := m.Matches(Message{Fields: tc.fields}); got != tc.want {
				t.Errorf("Matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRuleMatch_CriteriaAndTogether(t *testing.T) {
	m := &RuleMatch{
		RecipientDomains: []string{"example.com"},
		Fields:           map[string][]string{"plan": {"enterprise"}},
	}
	both := Message{To: []string{"a@example.com"}, Fields: map[string][]string{"plan": {"enterprise"}}}
	if !m.Matches(both) {
		t.Error("Matches = false when both criteria hold")
	}
	domainOnly := Message{To: []string{"a@example.com"}, Fields: map[string][]string{"plan": {"free"}}}
	if m.Matches(domainOnly) {
		t.Error("Matches = true with only the domain criterion satisfied")
	}
	fieldOnly := Message{To: []string{"a@other.com"}, Fields: map[string][]string{"plan": {"enterprise"}}}
	if m.Matches(fieldOnly) {
		t.Error("Matches = true with only the field criterion satisfied")
	}
}

func TestRuleMatch_NilMatchesEverything(t *testing.T) {
	var m *RuleMatch
	if !m.Matches(Message{}) {
		t.Error("a nil RuleMatch (the default route) should match everything")
	}
}

// --- routing through the rules -----------------------------------------

func TestRouter_FirstMatchingRuleWins(t *testing.T) {
	bulk, transactional, fallback := succeeding("bulk"), succeeding("tx"), succeeding("fb")
	r := mustRouter(t, "/api/mail",
		mustRoute(t, nil, StrategyFailover, testProvider("fallback", fallback)),
		mustRoute(t, &RuleMatch{RecipientDomains: []string{"*.edu"}}, StrategyFailover,
			testProvider("bulk", bulk)),
		mustRoute(t, &RuleMatch{RecipientDomains: []string{"mit.edu"}}, StrategyFailover,
			testProvider("transactional", transactional)),
	)

	// mit.edu satisfies both rules; the earlier one wins.
	res, err := r.Send(context.Background(), Message{To: []string{"a@mit.edu"}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Provider != "bulk" {
		t.Errorf("Provider = %q, want bulk (first matching rule)", res.Provider)
	}
	if len(transactional.Sent) != 0 {
		t.Error("a later rule fired for a message an earlier rule matched")
	}
}

func TestRouter_FallsThroughToDefaultRoute(t *testing.T) {
	ruleP, defaultP := succeeding("rule"), succeeding("default")
	r := mustRouter(t, "/api/mail",
		mustRoute(t, nil, StrategyFailover, testProvider("default", defaultP)),
		mustRoute(t, &RuleMatch{RecipientDomains: []string{"gmail.com"}}, StrategyFailover,
			testProvider("rule", ruleP)),
	)

	res, err := r.Send(context.Background(), Message{To: []string{"ops@example.com"}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Provider != "default" {
		t.Errorf("Provider = %q, want default", res.Provider)
	}
	if len(ruleP.Sent) != 0 {
		t.Error("the rule route fired for a non-matching message")
	}
}

func TestRouter_RuleRouteFailsOverWithinItsOwnList(t *testing.T) {
	down := failing(&TransportError{Class: ErrTransient, Message: "down"})
	up, defaultP := succeeding("up"), succeeding("default")
	r := mustRouter(t, "/api/mail",
		mustRoute(t, nil, StrategyFailover, testProvider("default", defaultP)),
		mustRoute(t, &RuleMatch{RecipientDomains: []string{"gmail.com"}}, StrategyFailover,
			testProvider("bulk-a", down), testProvider("bulk-b", up)),
	)

	res, err := r.Send(context.Background(), Message{To: []string{"a@gmail.com"}})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Provider != "bulk-b" {
		t.Errorf("Provider = %q, want bulk-b", res.Provider)
	}
	// Failover stays inside the matched route: falling back to the
	// default list would silently undo the operator's routing decision.
	if len(defaultP.Sent) != 0 {
		t.Error("a rule route fell back to the default route's providers")
	}
}

// Submitter-supplied field values pick between routes the operator
// wrote. They never name a provider, so the reachable set is exactly
// what the config sanctions.
func TestRouter_FieldRuleRoutesWithinOperatorAllowlist(t *testing.T) {
	priority, standard := succeeding("prio"), succeeding("std")
	r := mustRouter(t, "/api/mail",
		mustRoute(t, nil, StrategyFailover, testProvider("standard", standard)),
		mustRoute(t, &RuleMatch{Fields: map[string][]string{"plan": {"enterprise"}}},
			StrategyFailover, testProvider("priority", priority)),
	)

	res, _ := r.Send(context.Background(), Message{
		To:     []string{"a@example.com"},
		Fields: map[string][]string{"plan": {"enterprise"}},
	})
	if res.Provider != "priority" {
		t.Errorf("Provider = %q, want priority", res.Provider)
	}

	// An unrecognised value cannot conjure a route; it falls through.
	res, _ = r.Send(context.Background(), Message{
		To:     []string{"a@example.com"},
		Fields: map[string][]string{"plan": {"../../admin", "priority"}},
	})
	if res.Provider != "standard" {
		t.Errorf("Provider = %q, want standard — an unlisted value must fall through", res.Provider)
	}
}

// --- Explain -----------------------------------------------------------

func TestRouter_ExplainReportsMatchedRule(t *testing.T) {
	r := mustRouter(t, "/api/mail",
		mustRoute(t, nil, StrategyWeighted,
			testProvider("d1", succeeding("d1")), testProvider("d2", succeeding("d2"))),
		mustRoute(t, &RuleMatch{RecipientDomains: []string{"gmail.com"}}, StrategyRoundRobin,
			testProvider("bulk", succeeding("bulk"))),
	)

	ex := r.Explain(Message{To: []string{"a@gmail.com"}})
	if ex.RuleIndex != 0 {
		t.Errorf("RuleIndex = %d, want 0", ex.RuleIndex)
	}
	if ex.Strategy != string(StrategyRoundRobin) {
		t.Errorf("Strategy = %q, want round_robin", ex.Strategy)
	}
	if len(ex.Providers) != 1 || ex.Providers[0] != "bulk" {
		t.Errorf("Providers = %v, want [bulk]", ex.Providers)
	}

	ex = r.Explain(Message{To: []string{"a@example.com"}})
	if ex.RuleIndex != -1 {
		t.Errorf("RuleIndex = %d, want -1 for the default route", ex.RuleIndex)
	}
	if len(ex.Providers) != 2 || ex.Providers[0] != "d1" || ex.Providers[1] != "d2" {
		t.Errorf("Providers = %v, want [d1 d2] in declared order", ex.Providers)
	}
}

// Explain is the dry-run path: a debugging request must not advance the
// rotation that decides where real traffic goes.
func TestRouter_ExplainDoesNotAdvanceRotation(t *testing.T) {
	a, b := succeeding("a"), succeeding("b")
	r := mustRouter(t, "/x", mustRoute(t, nil, StrategyRoundRobin,
		testProvider("a", a), testProvider("b", b)))

	for i := 0; i < 10; i++ {
		r.Explain(simpleMsg())
	}
	for i := 0; i < 4; i++ {
		if _, err := r.Send(context.Background(), simpleMsg()); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.Sent) != 2 || len(b.Sent) != 2 {
		t.Errorf("split = a:%d b:%d, want 2 and 2 — Explain moved the rotation", len(a.Sent), len(b.Sent))
	}
}
