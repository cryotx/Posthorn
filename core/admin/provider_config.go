package admin

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/craigmccaskill/posthorn/managedproviders"
)

type providerField struct {
	Name, Label, Value string
	Secret             bool
}
type providerForm struct {
	Name, Type, DisplayType  string
	DailyLimit, MonthlyLimit int
	Editing                  bool
	Fields                   []providerField
}

// Only these fields are ever rendered or accepted. In particular, credentials
// are password inputs with no value, even if a manager accidentally returns one.
func providerFields(typ string) []providerField {
	fields := []providerField{{Name: "from", Label: "Verified sender address / 已验证发件地址"}}
	add := func(name, label string, secret bool) {
		fields = append(fields, providerField{Name: name, Label: label, Secret: secret})
	}
	switch typ {
	case "resend", "brevo", "postmark":
		add("api_key", "API key / API 密钥", true)
	case "mailgun":
		add("domain", "Mailgun domain / 发信域名", false)
		add("mailgun_region", "Mailgun region / 服务区域", false)
		add("api_key", "API key / API 密钥", true)
	case "ses":
		add("region", "AWS region / 区域", false)
		add("access_key_id", "AWS access key ID", true)
		add("secret_access_key", "AWS secret access key", true)
	case "cloudflare":
		add("account_id", "Cloudflare account ID / 账户 ID", false)
		add("api_token", "API token / API 令牌", true)
	case "sendernet":
		add("api_token", "API token / API 令牌", true)
	case "smtp":
		add("host", "SMTP host / 服务器地址", false)
		add("port", "SMTP port / 端口", false)
		add("username", "SMTP username / 用户名", true)
		add("password", "SMTP password / 密码", true)
		add("implicit_tls", "TLS connection / 加密连接方式", false)
	default:
		return nil
	}
	return fields
}

func (h *Handler) pageProviderConfigure(w http.ResponseWriter, r *http.Request) {
	csrf, ok := h.requirePage(w, r)
	if !ok {
		return
	}
	if h.deps.ProviderManager == nil {
		http.NotFound(w, r)
		return
	}
	typ := r.URL.Query().Get("type")
	edit := r.URL.Query().Get("edit")
	form := &providerForm{Type: typ, DisplayType: typ}
	if edit != "" {
		providers, err := h.deps.ProviderManager.List()
		if err != nil {
			h.fail(w, "managed_provider_list", err)
			return
		}
		found := false
		for _, p := range providers {
			if p.Name == edit {
				found = true
				form.Name = p.Name
				form.Type = p.Type
				form.DisplayType = p.Type
				form.Editing = true
				form.DailyLimit = p.DailyLimit
				form.MonthlyLimit = p.MonthlyLimit
				form.Fields = providerFields(p.Type)
				for i := range form.Fields {
					if !form.Fields[i].Secret {
						form.Fields[i].Value = p.Settings[form.Fields[i].Name]
						if p.Type == "mailgun" && form.Fields[i].Name == "mailgun_region" && form.Fields[i].Value == "" {
							form.Fields[i].Value = "us"
						}
						if p.Type == "smtp" && form.Fields[i].Name == "implicit_tls" && form.Fields[i].Value == "" {
							form.Fields[i].Value = "false"
						}
					}
				}
				break
			}
		}
		if !found {
			http.NotFound(w, r)
			return
		}
	} else if typ != "" {
		switch typ {
		case "mailjet":
			form.Type = "smtp"
			form.DailyLimit = 200
			form.MonthlyLimit = 6000
		case "smtp2go":
			form.Type = "smtp"
			form.DailyLimit = 200
			form.MonthlyLimit = 1000
		case "resend":
			form.DailyLimit = 100
			form.MonthlyLimit = 3000
		case "brevo":
			form.DailyLimit = 300
		}
		form.Fields = providerFields(form.Type)
		for i := range form.Fields {
			f := &form.Fields[i]
			if form.Type == "mailgun" && f.Name == "mailgun_region" {
				f.Value = "us"
			}
			if form.Type == "smtp" {
				switch f.Name {
				case "port":
					f.Value = "587"
				case "implicit_tls":
					f.Value = "false"
				case "host":
					if typ == "mailjet" {
						f.Value = "in-v3.mailjet.com"
					}
					if typ == "smtp2go" {
						f.Value = "mail.smtp2go.com"
					}
				}
			}
		}
	}
	if typ != "" && form.Fields == nil {
		http.NotFound(w, r)
		return
	}
	h.render(w, r, "provider_configure", pageData{TitleKey: "label.providers", Nav: "providers", CSRFToken: csrf, ProviderForm: form, Error: r.URL.Query().Get("error")})
}

func (h *Handler) doProviderConfigure(w http.ResponseWriter, r *http.Request) {
	kind, csrf, ok := h.require(w, r)
	if !ok {
		return
	}
	if h.deps.ProviderManager == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := checkCSRF(kind, csrf, r); err != nil {
		writeError(w, http.StatusForbidden, "csrf_failed", "invalid CSRF token")
		return
	}
	name, typ := strings.TrimSpace(r.PostFormValue("name")), r.PostFormValue("type")
	fields := providerFields(typ)
	if fields == nil {
		http.Error(w, "invalid provider", http.StatusBadRequest)
		return
	}
	daily, e1 := strconv.Atoi(r.PostFormValue("daily_limit"))
	monthly, e2 := strconv.Atoi(r.PostFormValue("monthly_limit"))
	if e1 != nil || e2 != nil || daily < 0 || monthly < 0 {
		http.Error(w, "invalid limits", http.StatusBadRequest)
		return
	}
	input := managedproviders.Input{Name: name, Type: typ, DailyLimit: daily, MonthlyLimit: monthly, Settings: map[string]string{}}
	for _, f := range fields {
		input.Settings[f.Name] = r.PostFormValue(f.Name)
	}
	// Preserve current enabled state. New entries start disabled.
	providers, err := h.deps.ProviderManager.List()
	if err != nil {
		h.fail(w, "managed_provider_list", err)
		return
	}
	for _, p := range providers {
		if p.Name == name {
			input.Enabled = p.Enabled
			break
		}
	}
	err = h.audited(r, "provider.configuration_saved", name, nil, func() error { return h.deps.ProviderManager.Save(input) })
	if err != nil {
		http.Redirect(w, r, "/admin/providers/configure?error="+url.QueryEscape("保存失败，请检查配置名称、发件地址、凭据和额度。 / Save failed; check the fields."), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/providers?notice="+url.QueryEscape("服务商配置已保存 / Provider saved"), http.StatusSeeOther)
}

func (h *Handler) doManagedProviderState(w http.ResponseWriter, r *http.Request) {
	kind, csrf, ok := h.require(w, r)
	if !ok {
		return
	}
	if h.deps.ProviderManager == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := checkCSRF(kind, csrf, r); err != nil {
		writeError(w, http.StatusForbidden, "csrf_failed", "invalid CSRF token")
		return
	}
	name := r.PostFormValue("name")
	enabled := r.PostFormValue("enabled") == "true"
	action := "provider.disabled"
	if enabled {
		action = "provider.enabled"
	}
	if err := h.audited(r, action, name, nil, func() error { return h.deps.ProviderManager.SetEnabled(name, enabled) }); err != nil {
		http.Redirect(w, r, "/admin/providers?error="+url.QueryEscape("无法更改服务商状态 / Unable to update provider"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/providers?notice="+url.QueryEscape("服务商状态已更新 / Provider state updated"), http.StatusSeeOther)
}
