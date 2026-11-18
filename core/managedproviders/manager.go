// Package managedproviders manages administrator-configured outbound providers.
package managedproviders

import (
	"context"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/craigmccaskill/posthorn/netguard"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

type Input struct {
	Name, Type               string
	DailyLimit, MonthlyLimit int
	Enabled                  bool
	Settings                 map[string]string
}

type View = Input

type Store interface {
	ListManagedProviders() ([]storage.ManagedProvider, error)
	SaveManagedProvider(storage.ManagedProvider) error
}

type Manager struct {
	mu        sync.RWMutex
	store     Store
	pool      *transport.Pool
	records   map[string]storage.ManagedProvider
	providers map[string]*transport.Provider
	version   uint64
}

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// New loads and validates all persisted entries before ingress starts.
func New(store Store, pool *transport.Pool) (*Manager, error) {
	if store == nil {
		return nil, fmt.Errorf("managed providers require storage")
	}
	records, err := store.ListManagedProviders()
	if err != nil {
		return nil, err
	}
	m := &Manager{store: store, pool: pool, records: map[string]storage.ManagedProvider{}, providers: map[string]*transport.Provider{}}
	for _, r := range records {
		if _, exists := m.records[r.Name]; exists {
			return nil, fmt.Errorf("duplicate managed provider %q", r.Name)
		}
		if _, exists := pool.Get(r.Name); exists {
			return nil, fmt.Errorf("managed provider %q conflicts with configured provider", r.Name)
		}
		p, err := build(r)
		if err != nil {
			return nil, fmt.Errorf("managed provider %q: %w", r.Name, err)
		}
		m.records[r.Name] = r
		m.providers[r.Name] = p
	}
	return m, nil
}

var secrets = map[string]bool{"api_key": true, "api_token": true, "password": true, "username": true, "secret_access_key": true, "access_key_id": true}
var fields = map[string][]string{
	"smtp":   {"host", "port", "username", "password", "implicit_tls", "require_tls"},
	"resend": {"api_key"}, "brevo": {"api_key"}, "postmark": {"api_key"},
	"mailgun": {"api_key", "domain", "mailgun_region"}, "ses": {"access_key_id", "secret_access_key", "region"},
	"cloudflare": {"account_id", "api_token"}, "sendernet": {"api_token"},
}

func build(r storage.ManagedProvider) (*transport.Provider, error) {
	if !validName.MatchString(r.Name) {
		return nil, fmt.Errorf("名称须以字母或数字开头，且只能包含字母、数字、点、下划线或连字符（最多 64 字符）")
	}
	allowed, ok := fields[r.Type]
	if !ok {
		return nil, fmt.Errorf("不支持的服务商类型")
	}
	if r.DailyLimit < 0 || r.MonthlyLimit < 0 {
		return nil, fmt.Errorf("额度不能为负数")
	}
	a, err := mail.ParseAddress(r.Sender)
	if err != nil || a.Address != r.Sender || strings.ContainsAny(r.Sender, "\r\n") {
		return nil, fmt.Errorf("请填写有效的发件邮箱")
	}
	permitted := map[string]bool{}
	for _, k := range allowed {
		permitted[k] = true
	}
	settings := map[string]any{}
	for k, v := range r.Settings {
		if !permitted[k] {
			return nil, fmt.Errorf("不支持的配置字段 %q", k)
		}
		if len(v) > 4096 || strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("配置字段无效")
		}
		settings[k] = v
	}
	if r.Type == "mailgun" {
		switch defaultString(r.Settings["mailgun_region"], "us") {
		case "us":
			settings["base_url"] = "https://api.mailgun.net"
		case "eu":
			settings["base_url"] = "https://api.eu.mailgun.net"
		default:
			return nil, fmt.Errorf("Mailgun 区域必须为 us 或 eu")
		}
		delete(settings, "mailgun_region")
	}
	if r.Type == "smtp" {
		host := r.Settings["host"]
		if host == "" || strings.ContainsAny(host, "/?#@ ") {
			return nil, fmt.Errorf("SMTP 主机名无效")
		}
		u := url.URL{Scheme: "https", Host: net.JoinHostPort(host, "443")}
		if err := (netguard.Policy{}).ValidateURL(u.String()); err != nil {
			return nil, fmt.Errorf("SMTP 主机地址不可使用: %w", err)
		}
		n, err := strconv.Atoi(r.Settings["port"])
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("SMTP 端口须为 1 至 65535")
		}
		settings["port"] = n
		implicit, err := strconv.ParseBool(defaultString(r.Settings["implicit_tls"], "false"))
		if err != nil {
			return nil, fmt.Errorf("SMTP TLS 设置无效")
		}
		settings["implicit_tls"] = implicit
		settings["require_tls"] = !implicit
		if !implicit && r.Settings["require_tls"] == "false" {
			return nil, fmt.Errorf("SMTP 必须启用 TLS")
		}
	}
	reg, ok := transport.Lookup(r.Type)
	if !ok {
		return nil, fmt.Errorf("不支持的服务商类型")
	}
	if err := reg.Validate(settings); err != nil {
		return nil, err
	}
	t, err := reg.Build(settings)
	if err != nil {
		return nil, err
	}
	if smtp, ok := t.(*transport.SMTPOutTransport); ok {
		smtp.RestrictToPublicDestinations()
	}
	return &transport.Provider{Name: r.Name, Type: r.Type, Weight: 1, Transport: senderTransport{t, r.Sender}, Breaker: transport.NewBreaker(transport.BreakerConfig{}), Quota: transport.Quota{Daily: int64(r.DailyLimit), Monthly: int64(r.MonthlyLimit)}}, nil
}
func defaultString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

type senderTransport struct {
	base transport.Transport
	from string
}

func (t senderTransport) Send(ctx context.Context, msg transport.Message) (transport.SendResult, error) {
	msg.From = t.from
	return t.base.Send(ctx, msg)
}

// List never returns credentials.
func (m *Manager) List() ([]View, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.records))
	for n := range m.records {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]View, 0, len(names))
	for _, n := range names {
		r := m.records[n]
		settings := map[string]string{"from": r.Sender}
		if r.Type == "mailgun" {
			settings["mailgun_region"] = defaultString(r.Settings["mailgun_region"], "us")
		}
		for k, v := range r.Settings {
			if !secrets[k] {
				settings[k] = v
			}
		}
		out = append(out, View{Name: r.Name, Type: r.Type, DailyLimit: r.DailyLimit, MonthlyLimit: r.MonthlyLimit, Enabled: r.Enabled, Settings: settings})
	}
	return out, nil
}

// Save creates an initially disabled provider or edits an existing one.
// Empty secret fields retain existing values on edit.
func (m *Manager) Save(in Input) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, exists := m.records[in.Name]
	if _, conflict := m.pool.Get(in.Name); conflict {
		return fmt.Errorf("名称与已有服务商重复")
	}
	if exists && old.Type != in.Type {
		return fmt.Errorf("不能更改现有服务商类型")
	}
	r := storage.ManagedProvider{Name: in.Name, Type: in.Type, Sender: in.Settings["from"], DailyLimit: in.DailyLimit, MonthlyLimit: in.MonthlyLimit, Enabled: in.Enabled, Settings: map[string]string{}}
	if exists {
		r.Enabled = old.Enabled
	} else {
		r.Enabled = false
	}
	for k, v := range in.Settings {
		if k == "from" {
			continue
		}
		if secrets[k] && v == "" && exists {
			v = old.Settings[k]
		}
		if v != "" {
			r.Settings[k] = v
		}
	}
	if exists {
		for k, v := range old.Settings {
			if secrets[k] && r.Settings[k] == "" {
				r.Settings[k] = v
			}
		}
	}
	p, err := build(r)
	if err != nil {
		return err
	}
	if err = m.store.SaveManagedProvider(r); err != nil {
		return fmt.Errorf("保存服务商失败: %w", err)
	}
	m.records[r.Name] = r
	m.providers[r.Name] = p
	m.version++
	return nil
}
func (m *Manager) SetEnabled(name string, enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[name]
	if !ok {
		return fmt.Errorf("服务商不存在")
	}
	r.Enabled = enabled
	if err := m.store.SaveManagedProvider(r); err != nil {
		return fmt.Errorf("保存服务商失败: %w", err)
	}
	m.records[name] = r
	m.version++
	return nil
}

// Wrap incorporates enabled managed providers into each route, ahead of the
// configured route's providers. It retains routing rules, hooks and quotas.
func (m *Manager) Wrap(base transport.Transport) transport.Transport {
	if _, ok := base.(*transport.Router); !ok {
		return base
	}
	return &wrapped{m: m, base: base.(*transport.Router)}
}

type wrapped struct {
	m       *Manager
	base    *transport.Router
	mu      sync.Mutex
	version uint64
	router  *transport.Router
}

func (w *wrapped) snapshot() *transport.Router {
	w.mu.Lock()
	w.m.mu.RLock()
	if w.router == nil || w.version != w.m.version {
		names := make([]string, 0, len(w.m.records))
		for n, r := range w.m.records {
			if r.Enabled {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		providers := make([]*transport.Provider, 0, len(names))
		for _, n := range names {
			providers = append(providers, w.m.providers[n])
		}
		makeRoute := func(r *transport.Route) *transport.Route {
			all := append(append([]*transport.Provider{}, providers...), r.Providers...)
			out, _ := transport.NewRoute(r.Match, r.Strategy, all)
			return out
		}
		def := makeRoute(w.base.Default)
		rules := make([]*transport.Route, 0, len(w.base.Rules))
		for _, r := range w.base.Rules {
			rules = append(rules, makeRoute(r))
		}
		router, _ := transport.NewRouter(w.base.Name, def, rules)
		router.Quota = w.base.Quota
		router.Hooks = w.base.Hooks
		router.Now = w.base.Now
		w.router = router
		w.version = w.m.version
	}
	router := w.router
	w.m.mu.RUnlock()
	w.mu.Unlock()
	return router
}

func (w *wrapped) Send(ctx context.Context, msg transport.Message) (transport.SendResult, error) {
	return w.snapshot().Send(ctx, msg)
}

// Explain reports the currently effective route without sending a message.
func (w *wrapped) Explain(msg transport.Message) transport.RouteExplanation {
	return w.snapshot().Explain(msg)
}
