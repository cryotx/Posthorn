package managedproviders

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

type memoryStore struct {
	records map[string]storage.ManagedProvider
}

func (s *memoryStore) ListManagedProviders() ([]storage.ManagedProvider, error) {
	var out []storage.ManagedProvider
	for _, r := range s.records {
		out = append(out, r)
	}
	return out, nil
}
func (s *memoryStore) SaveManagedProvider(r storage.ManagedProvider) error {
	s.records[r.Name] = r
	return nil
}

type testSender struct {
	mu      sync.Mutex
	calls   int
	from    string
	failure transport.ErrorClass
}

func (s *testSender) Send(_ context.Context, msg transport.Message) (transport.SendResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.from = msg.From
	if s.failure != 0 {
		return transport.SendResult{}, &transport.TransportError{Class: s.failure}
	}
	return transport.SendResult{MessageID: "sent"}, nil
}
func setup(t *testing.T) (*Manager, *memoryStore, *testSender, transport.Transport) {
	t.Helper()
	fb := &testSender{}
	p := &transport.Provider{Name: "qq", Type: "smtp", Transport: fb}
	pool, err := transport.NewPool([]*transport.Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	s := &memoryStore{records: map[string]storage.ManagedProvider{}}
	m, err := New(s, pool)
	if err != nil {
		t.Fatal(err)
	}
	r, err := transport.NewRoute(nil, transport.StrategyFailover, []*transport.Provider{p})
	if err != nil {
		t.Fatal(err)
	}
	base, err := transport.NewRouter("send", r, nil)
	if err != nil {
		t.Fatal(err)
	}
	base.Quota = transport.NewLocalQuota(nil, nil)
	return m, s, fb, m.Wrap(base)
}
func TestManageProvider(t *testing.T) {
	m, s, _, _ := setup(t)
	in := Input{Name: "resend1", Type: "resend", Enabled: true, DailyLimit: 3, Settings: map[string]string{"from": "verified@example.com", "api_key": "super-secret"}}
	if err := m.Save(in); err != nil {
		t.Fatal(err)
	}
	list, _ := m.List()
	if list[0].Enabled {
		t.Fatal("new provider enabled")
	}
	if strings.Contains(list[0].Settings["api_key"], "secret") {
		t.Fatal("secret returned")
	}
	in.Settings["api_key"] = ""
	in.Enabled = false
	if err := m.Save(in); err != nil {
		t.Fatal(err)
	}
	if s.records[in.Name].Settings["api_key"] != "super-secret" {
		t.Fatal("secret lost on update")
	}
	if err := m.SetEnabled(in.Name, true); err != nil {
		t.Fatal(err)
	}
	restored, err := New(s, m.pool)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := restored.List()
	if len(items) != 1 || !items[0].Enabled {
		t.Fatal("enabled state not restored")
	}
	if err := m.Save(Input{Name: "qq", Type: "resend"}); err == nil {
		t.Fatal("configured name collision accepted")
	}
}
func TestRoutingAndSender(t *testing.T) {
	m, _, fallback, wrapped := setup(t)
	if err := m.Save(Input{Name: "resend1", Type: "resend", DailyLimit: 1, Settings: map[string]string{"from": "verified@example.com", "api_key": "key"}}); err != nil {
		t.Fatal(err)
	}
	test := &testSender{}
	m.providers["resend1"].Transport = senderTransport{test, "verified@example.com"}
	if err := m.SetEnabled("resend1", true); err != nil {
		t.Fatal(err)
	}
	msg := transport.Message{From: "attacker@example.com"}
	result, err := wrapped.Send(context.Background(), msg)
	if err != nil || result.Provider != "resend1" || test.from != "verified@example.com" {
		t.Fatalf("managed send result=%+v err=%v from=%q", result, err, test.from)
	}
	result, err = wrapped.Send(context.Background(), msg)
	if err != nil || result.Provider != "qq" || fallback.calls != 1 || fallback.from != "attacker@example.com" {
		t.Fatalf("quota fallback result=%+v err=%v", result, err)
	}
}
func TestFailureClasses(t *testing.T) {
	for _, tc := range []struct {
		class    transport.ErrorClass
		fallback bool
	}{{transport.ErrTerminal, false}, {transport.ErrTransient, true}, {transport.ErrRateLimited, true}} {
		t.Run(tc.class.String(), func(t *testing.T) {
			m, _, fb, w := setup(t)
			if err := m.Save(Input{Name: "resend1", Type: "resend", Settings: map[string]string{"from": "a@example.com", "api_key": "key"}}); err != nil {
				t.Fatal(err)
			}
			m.providers["resend1"].Transport = &testSender{failure: tc.class}
			if err := m.SetEnabled("resend1", true); err != nil {
				t.Fatal(err)
			}
			_, err := w.Send(context.Background(), transport.Message{})
			if (fb.calls > 0) != tc.fallback {
				t.Fatalf("fallback calls=%d err=%v", fb.calls, err)
			}
		})
	}
}
func TestSMTPRejectsUnsafeTarget(t *testing.T) {
	m, _, _, _ := setup(t)
	for _, host := range []string{"127.0.0.1", "localhost", "169.254.169.254", "10.0.0.1"} {
		err := m.Save(Input{Name: "smtp", Type: "smtp", Settings: map[string]string{"from": "a@example.com", "host": host, "port": "587", "username": "u", "password": "p"}})
		if err == nil {
			t.Errorf("accepted %s", host)
		}
	}
}

func TestQuotaConcurrent(t *testing.T) {
	m, _, fallback, wrapped := setup(t)
	if err := m.Save(Input{Name: "resend1", Type: "resend", DailyLimit: 1, Settings: map[string]string{"from": "verified@example.com", "api_key": "key"}}); err != nil {
		t.Fatal(err)
	}
	managed := &testSender{}
	m.providers["resend1"].Transport = managed
	if err := m.SetEnabled("resend1", true); err != nil {
		t.Fatal(err)
	}
	const count = 24
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := wrapped.Send(context.Background(), transport.Message{}); err != nil {
				t.Errorf("send: %v", err)
			}
		}()
	}
	wg.Wait()
	if managed.calls != 1 || fallback.calls != count-1 {
		t.Fatalf("managed=%d fallback=%d", managed.calls, fallback.calls)
	}
}

func TestEditDoesNotChangeEnabled(t *testing.T) {
	m, _, _, _ := setup(t)
	input := Input{Name: "app", Type: "brevo", Settings: map[string]string{"from": "a@example.com", "api_key": "key"}}
	if err := m.Save(input); err != nil {
		t.Fatal(err)
	}
	if err := m.SetEnabled("app", true); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(input); err != nil {
		t.Fatal(err)
	}
	list, _ := m.List()
	if !list[0].Enabled {
		t.Fatal("edit changed enabled state")
	}
}

func TestExplainShowsEnabledProviders(t *testing.T) {
	m, _, _, w := setup(t)
	explain := w.(interface {
		Explain(transport.Message) transport.RouteExplanation
	})
	if err := m.Save(Input{Name: "resend1", Type: "resend", Settings: map[string]string{"from": "a@example.com", "api_key": "key"}}); err != nil {
		t.Fatal(err)
	}
	if got := explain.Explain(transport.Message{}).Providers; len(got) != 1 || got[0] != "qq" {
		t.Fatalf("disabled route = %v", got)
	}
	if err := m.SetEnabled("resend1", true); err != nil {
		t.Fatal(err)
	}
	if got := explain.Explain(transport.Message{}).Providers; len(got) != 2 || got[0] != "resend1" || got[1] != "qq" {
		t.Fatalf("enabled route = %v", got)
	}
}

func TestSupportedProviderFields(t *testing.T) {
	cases := []struct {
		typ    string
		fields map[string]string
	}{
		{"smtp", map[string]string{"host": "smtp.example.com", "port": "587", "username": "smtp-user", "password": "smtp-password", "implicit_tls": "false", "require_tls": "true"}},
		{"resend", map[string]string{"api_key": "secret"}},
		{"brevo", map[string]string{"api_key": "secret"}},
		{"postmark", map[string]string{"api_key": "secret"}},
		{"mailgun", map[string]string{"api_key": "secret", "domain": "example.com", "mailgun_region": "eu"}},
		{"ses", map[string]string{"access_key_id": "id", "secret_access_key": "secret", "region": "us-east-1"}},
		{"cloudflare", map[string]string{"account_id": "account", "api_token": "secret"}},
		{"sendernet", map[string]string{"api_token": "secret"}},
	}
	for _, tc := range cases {
		t.Run(tc.typ, func(t *testing.T) {
			m, s, _, _ := setup(t)
			settings := map[string]string{"from": "sender@example.com"}
			for k, v := range tc.fields {
				settings[k] = v
			}
			if err := m.Save(Input{Name: "platform", Type: tc.typ, DailyLimit: 100, MonthlyLimit: 1000, Settings: settings}); err != nil {
				t.Fatal(err)
			}
			stored := s.records["platform"]
			if stored.Sender != "sender@example.com" || stored.DailyLimit != 100 || stored.MonthlyLimit != 1000 {
				t.Fatalf("stored=%+v", stored)
			}
			for k, v := range tc.fields {
				if stored.Settings[k] != v {
					t.Errorf("%s=%q, want %q", k, stored.Settings[k], v)
				}
			}
			list, _ := m.List()
			for k, v := range tc.fields {
				if secrets[k] {
					if _, ok := list[0].Settings[k]; ok {
						t.Errorf("secret %s was returned", k)
					}
				} else if list[0].Settings[k] != v {
					t.Errorf("view %s=%q", k, list[0].Settings[k])
				}
			}
		})
	}
}

func TestMailgunRegionRestrictsEndpoint(t *testing.T) {
	for _, tc := range []struct{ region, url string }{{"", "https://api.mailgun.net"}, {"us", "https://api.mailgun.net"}, {"eu", "https://api.eu.mailgun.net"}} {
		t.Run(defaultString(tc.region, "default"), func(t *testing.T) {
			m, _, _, _ := setup(t)
			in := Input{Name: "mailgun", Type: "mailgun", Settings: map[string]string{"from": "sender@example.com", "domain": "example.com", "api_key": "secret", "mailgun_region": tc.region}}
			if err := m.Save(in); err != nil {
				t.Fatal(err)
			}
			actual := m.providers["mailgun"].Transport.(senderTransport).base.(*transport.MailgunTransport).BaseURL
			if actual != tc.url {
				t.Fatalf("base URL=%q want %q", actual, tc.url)
			}
			views, _ := m.List()
			if views[0].Settings["mailgun_region"] != defaultString(tc.region, "us") {
				t.Fatalf("unexpected view: %+v", views[0].Settings)
			}
		})
	}
	m, _, _, _ := setup(t)
	for _, bad := range []map[string]string{{"mailgun_region": "invalid"}, {"base_url": "https://attacker.example"}} {
		settings := map[string]string{"from": "sender@example.com", "domain": "example.com", "api_key": "secret"}
		for k, v := range bad {
			settings[k] = v
		}
		if err := m.Save(Input{Name: "mailgun", Type: "mailgun", Settings: settings}); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}
