package transport

import (
	"context"
	"strings"
	"testing"
)

func TestParseMailKind(t *testing.T) {
	for _, s := range []string{"", "verification", "password_reset", "notification", "important", "marketing"} {
		got, err := ParseMailKind(s)
		if err != nil {
			t.Errorf("ParseMailKind(%q): %v", s, err)
		}
		if string(got) != s {
			t.Errorf("ParseMailKind(%q) = %q", s, got)
		}
	}
	if got, _ := ParseMailKind("  verification  "); got != KindVerification {
		t.Errorf("surrounding space not trimmed: %q", got)
	}
}

func TestParseMailKind_RejectsUnknown(t *testing.T) {
	for _, s := range []string{"newsletter", "Verification", "urgent", "transactional"} {
		_, err := ParseMailKind(s)
		if err == nil {
			t.Errorf("ParseMailKind(%q) = nil error, want one", s)
			continue
		}
		if !strings.Contains(err.Error(), "verification") {
			t.Errorf("error %q should list the known kinds", err)
		}
	}
}

func TestMailKind_String(t *testing.T) {
	if got := KindUnspecified.String(); got != "unspecified" {
		t.Errorf("KindUnspecified.String() = %q, want unspecified", got)
	}
	if got := KindPasswordReset.String(); got != "password_reset" {
		t.Errorf("KindPasswordReset.String() = %q", got)
	}
}

func TestKnownMailKinds_Sorted(t *testing.T) {
	got := KnownMailKinds()
	want := []string{"important", "marketing", "notification", "password_reset", "verification"}
	if len(got) != len(want) {
		t.Fatalf("KnownMailKinds() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("KnownMailKinds()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// --- routing on kind ---------------------------------------------------

func TestRuleMatch_Kinds(t *testing.T) {
	m := &RuleMatch{Kinds: []MailKind{KindVerification, KindPasswordReset}}
	for _, tc := range []struct {
		kind MailKind
		want bool
	}{
		{KindVerification, true},
		{KindPasswordReset, true},
		{KindNotification, false},
		{KindUnspecified, false},
	} {
		if got := m.Matches(Message{Kind: tc.kind}); got != tc.want {
			t.Errorf("Matches(kind=%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

func TestRuleMatch_KindAndsWithOtherCriteria(t *testing.T) {
	m := &RuleMatch{
		Kinds:            []MailKind{KindImportant},
		RecipientDomains: []string{"example.com"},
	}
	if !m.Matches(Message{Kind: KindImportant, To: []string{"a@example.com"}}) {
		t.Error("both criteria satisfied but Matches = false")
	}
	if m.Matches(Message{Kind: KindImportant, To: []string{"a@other.com"}}) {
		t.Error("kind matched but domain did not; Matches should be false")
	}
	if m.Matches(Message{Kind: KindNotification, To: []string{"a@example.com"}}) {
		t.Error("domain matched but kind did not; Matches should be false")
	}
}

// The routing shape the feature exists for: transactional mail to the
// fast provider, routine notifications to the free pool.
func TestRouter_RoutesByMailKind(t *testing.T) {
	fastT, freeT, defaultT := succeeding("fast"), succeeding("free"), succeeding("def")

	r := mustRouter(t, "/api/send",
		mustRoute(t, nil, StrategyFailover, testProvider("default", defaultT)),
		mustRoute(t, &RuleMatch{Kinds: []MailKind{KindVerification, KindPasswordReset}},
			StrategyFailover, testProvider("fast", fastT)),
		mustRoute(t, &RuleMatch{Kinds: []MailKind{KindNotification, KindMarketing}},
			StrategyFailover, testProvider("free", freeT)),
	)

	for _, tc := range []struct {
		kind MailKind
		want string
	}{
		{KindVerification, "fast"},
		{KindPasswordReset, "fast"},
		{KindNotification, "free"},
		{KindMarketing, "free"},
		{KindImportant, "default"},
		{KindUnspecified, "default"},
	} {
		res, err := r.Send(context.Background(), Message{To: []string{"a@example.com"}, Kind: tc.kind})
		if err != nil {
			t.Fatalf("send kind=%q: %v", tc.kind, err)
		}
		if res.Provider != tc.want {
			t.Errorf("kind %q routed to %q, want %q", tc.kind, res.Provider, tc.want)
		}
	}
}

func TestRouter_ExplainReportsKind(t *testing.T) {
	r := mustRouter(t, "/x",
		mustRoute(t, nil, StrategyFailover, testProvider("d", succeeding("d"))),
		mustRoute(t, &RuleMatch{Kinds: []MailKind{KindMarketing}},
			StrategyFailover, testProvider("bulk", succeeding("b"))),
	)

	ex := r.Explain(Message{To: []string{"a@example.com"}, Kind: KindMarketing})
	if ex.RuleIndex != 0 || ex.Kind != KindMarketing {
		t.Errorf("Explain = %+v, want rule 0 and kind marketing", ex)
	}
	ex = r.Explain(Message{To: []string{"a@example.com"}})
	if ex.RuleIndex != -1 || ex.Kind != KindUnspecified {
		t.Errorf("Explain = %+v, want the default route and an unspecified kind", ex)
	}
}

// A rule that mentions no kind must keep matching messages that carry
// one — otherwise adding mail_kind to an endpoint would silently
// re-route everything (NFR32).
func TestRuleMatch_KindIsIgnoredWhenUnmentioned(t *testing.T) {
	m := &RuleMatch{RecipientDomains: []string{"example.com"}}
	for _, k := range []MailKind{KindUnspecified, KindVerification, KindMarketing} {
		if !m.Matches(Message{To: []string{"a@example.com"}, Kind: k}) {
			t.Errorf("a domain-only rule stopped matching for kind %q", k)
		}
	}
}
