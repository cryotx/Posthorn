package admin

import (
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// The admin UI chooses a language from the query, its preference cookie,
// or the highest-weight supported language in Accept-Language. English
// is the default and supplies the fallback for missing translations.

type uiLang string

const (
	langEN uiLang = "en"
	langZH uiLang = "zh"
	langJA uiLang = "ja"
	langKO uiLang = "ko"
	langES uiLang = "es"
)

const langCookieName = "posthorn_admin_lang"

var uiLanguages = []struct {
	Code  uiLang
	Label string
}{
	{langZH, "简体中文"},
	{langEN, "English"},
	{langJA, "日本語"},
	{langKO, "한국어"},
	{langES, "Español"},
}

// resolveLang reports whether the request chose a language explicitly,
// so the handler can persist it in a cookie.
func resolveLang(r *http.Request) (uiLang, bool) {
	if q := uiLang(r.URL.Query().Get("lang")); catalog[q] != nil {
		return q, true
	}
	if c, err := r.Cookie(langCookieName); err == nil && catalog[uiLang(c.Value)] != nil {
		return uiLang(c.Value), false
	}
	preferred, bestWeight := langEN, 0.0
	for _, entry := range strings.Split(strings.Join(r.Header.Values("Accept-Language"), ","), ",") {
		tag, params, err := mime.ParseMediaType(entry)
		if err != nil {
			continue
		}
		base, _, _ := strings.Cut(tag, "-")
		lang := uiLang(base)
		if catalog[lang] == nil {
			continue
		}
		weight := 1.0
		if q, ok := params["q"]; ok {
			weight, err = strconv.ParseFloat(q, 64)
			if err != nil {
				continue
			}
		}
		if weight > bestWeight && weight <= 1 {
			preferred, bestWeight = lang, weight
		}
	}
	return preferred, false
}

type languageLink struct {
	Code  uiLang
	Label string
	Href  string
}

func languageLinks(r *http.Request) []languageLink {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return nil
	}
	query := r.URL.Query()
	links := make([]languageLink, 0, len(uiLanguages))
	for _, language := range uiLanguages {
		query.Set("lang", string(language.Code))
		links = append(links, languageLink{language.Code, language.Label, "?" + query.Encode()})
	}
	return links
}

// lang resolves the request's language and, if it came from an
// explicit ?lang= switch, persists it — a year is generous for a
// preference an operator sets once and forgets about.
func (h *Handler) lang(w http.ResponseWriter, r *http.Request) uiLang {
	lang, fromQuery := resolveLang(r)
	if fromQuery {
		http.SetCookie(w, &http.Cookie{
			Name: langCookieName, Value: string(lang), Path: "/admin",
			MaxAge: 365 * 24 * 60 * 60, SameSite: http.SameSiteStrictMode,
		})
	}
	return lang
}

// translate looks up one string, exposed to templates under the short
// name "t" (see ui.go's FuncMap) — html.go call sites read
// `{{t $.Lang "key"}}`. Named translate rather than t here because a
// package-level `t` collides with every test function's `t *testing.T`
// parameter, shadowing the function inside its own test file.
func translate(lang uiLang, key string) string {
	if s, ok := catalog[lang][key]; ok {
		return s
	}
	if s, ok := catalog[langEN][key]; ok {
		return s // half-translated beats broken
	}
	return "[" + key + "]" // deliberately ugly — see file comment
}

// localizedMessage translates a UI message while retaining literal text
// supplied through existing notice and error query parameters.
func localizedMessage(lang uiLang, message string) string {
	if strings.HasPrefix(message, "flash.") && catalog[langEN][message] != "" {
		return translate(lang, message)
	}
	return message
}

// Catalog keys are grouped by template.
var catalog = map[uiLang]map[string]string{
	langJA: japaneseCatalog,
	langKO: koreanCatalog,
	langES: spanishCatalog,
	langEN: {
		"label.language":               "Language",
		"title.admin":                  "Posthorn admin",
		"field.from":                   "Verified sender address",
		"field.api_key":                "API key",
		"field.domain":                 "Mailgun domain",
		"field.mailgun_region":         "Mailgun region",
		"field.region":                 "AWS region",
		"field.access_key_id":          "AWS access key ID",
		"field.secret_access_key":      "AWS secret access key",
		"field.account_id":             "Cloudflare account ID",
		"field.api_token":              "API token",
		"field.host":                   "SMTP host",
		"field.port":                   "SMTP port",
		"field.username":               "SMTP username",
		"field.password":               "SMTP password",
		"field.implicit_tls":           "TLS connection",
		"field.region_us":              "United States",
		"field.region_eu":              "European Union",
		"flash.bad_request":            "Invalid request.",
		"flash.invalid_token":          "Invalid admin token.",
		"flash.too_many_attempts":      "Too many sign-in attempts. Try again later.",
		"flash.invalid_filter":         "Invalid filter.",
		"flash.action_failed":          "Action failed.",
		"flash.done":                   "Action completed.",
		"flash.provider_save_failed":   "Unable to save the provider. Check its name, sender, credentials and limits.",
		"flash.provider_saved":         "Provider saved.",
		"flash.provider_update_failed": "Unable to update the provider.",
		"flash.provider_state_updated": "Provider state updated.",

		// --- chrome: nav, sign-out, language switch ---
		"project.name":             "Project name",
		"project.create":           "Create project and key",
		"project.create_hint":      "Use a unique name for each application. Its key is shown only once.",
		"project.rotate":           "Replace key",
		"project.confirm_rotation": "I understand the old key stops working immediately",
		"project.rotate_hint":      "Replacing a key immediately disables the old key. Update the application with the new key.",
		"project.created_key":      "API key for project:",
		"project.save_key":         "Copy and save this key now. It cannot be displayed again.",
		"project.back":             "Back to projects",
		"project.invalid_name":     "Invalid project name. Use letters, numbers, hyphens or underscores (up to 64 characters).",
		"project.exists":           "A project with that name already exists.",
		"project.not_managed":      "This project cannot be managed here.",
		"nav.dashboard":            "Dashboard",
		"nav.providers":            "Providers",
		"nav.projects":             "Projects",
		"nav.messages":             "Messages",
		"nav.audit":                "Audit",
		"nav.signout":              "Sign out",

		// --- page titles / section headings (shared across nav, <title>, <h2>) ---
		"label.dashboard":       "Dashboard",
		"label.providers":       "Providers",
		"label.projects":        "Projects",
		"label.messages":        "Messages",
		"label.message":         "Message",
		"label.audit_log":       "Audit log",
		"label.retry_queue":     "Retry queue",
		"label.dead_letters":    "Dead letters",
		"label.suppressions":    "Suppressions",
		"label.attempts":        "Attempts",
		"label.delivery_events": "Delivery events",
		"label.signin":          "Sign in",

		// --- login page ---
		"login.heading": "Posthorn admin",
		"login.token":   "Admin token",
		"login.hint": "The token comes from POSTHORN_ADMIN_TOKEN. It is exchanged for a " +
			"session cookie and is never stored in the browser.",

		// --- dashboard cards ---
		"card.retry_queue":  "Retry queue",
		"card.dead_letters": "Dead letters",
		"card.providers":    "Providers",
		"card.projects":     "Projects",
		"card.suppressions": "Suppressions",
		"card.storage":      "Storage",

		// --- status pills (from fixed, already-branched comparisons — see html.go) ---
		"pill.ok":                 "ok",
		"pill.degraded":           "degraded",
		"pill.disabled":           "disabled",
		"pill.in_rotation":        "in rotation",
		"pill.active":             "active",
		"pill.suspended":          "suspended",
		"pill.disabled_in_config": "disabled in config",
		"pill.sent":               "sent",
		"pill.failed":             "failed",
		"pill.keys_live_suffix":   "keys live", // rendered as "{N} {suffix}"

		// --- table headers ---
		"th.name":            "Name",
		"th.type":            "Type",
		"th.circuit":         "Circuit",
		"th.state":           "State",
		"th.today":           "Today",
		"th.month":           "Month",
		"th.webhook":         "Webhook",
		"th.verified":        "Verified",
		"th.message":         "Message",
		"th.project":         "Project",
		"th.attempt":         "Attempt",
		"th.next_attempt":    "Next attempt",
		"th.last_error":      "Last error",
		"th.to":              "To",
		"th.created":         "Created",
		"th.allowed_senders": "Allowed senders",
		"th.key":             "Key",
		"th.smtp":            "SMTP",
		"th.status":          "Status",
		"th.delivery":        "Delivery",
		"th.provider":        "Provider",
		"th.address":         "Address",
		"th.reason":          "Reason",
		"th.source":          "Source",
		"th.added":           "Added",
		"th.result":          "Result",
		"th.error_class":     "Error class",
		"th.provider_msg_id": "Provider message id",
		"th.started":         "Started",
		"th.occurred":        "Occurred",
		"th.event_id":        "Event id",
		"th.when":            "When",
		"th.actor":           "Actor",
		"th.action":          "Action",
		"th.target":          "Target",
		"th.details":         "Details",

		// --- buttons ---
		"btn.filter":        "Filter",
		"btn.retry":         "Retry",
		"btn.enable":        "Enable",
		"btn.disable":       "Disable",
		"btn.reset_breaker": "Reset breaker",
		"btn.restore":       "Restore",
		"btn.suspend":       "Suspend",
		"btn.unsuppress":    "Unsuppress",
		"btn.next_page":     "Next page →",

		// --- filter form placeholders ---
		"filter.project":  "project",
		"filter.status":   "status",
		"filter.provider": "provider",

		// --- empty states ---
		"empty.providers":    "No providers in the pool.",
		"empty.queue":        "Queue is empty.",
		"empty.dead_letters": "No dead letters.",
		"empty.projects":     "No projects configured.",
		"empty.suppressions": "No suppressions.",
		"empty.messages":     "No messages.",
		"empty.attempts":     "No recorded attempts.",
		"empty.events":       "No delivery events.",
		"empty.audit":        "Nothing recorded yet.",

		// --- explanatory notes (muted paragraphs) ---
		"provider.add":              "Add provider",
		"provider.edit":             "Edit",
		"provider.configure":        "Configure provider",
		"provider.type":             "Provider type",
		"provider.name":             "Configuration name (letters, numbers, dots, underscores, or hyphens; up to 64 characters)",
		"provider.next":             "Next",
		"provider.save":             "Save configuration",
		"provider.back":             "Back to providers",
		"provider.unlimited":        "unlimited",
		"provider.daily":            "Daily limit",
		"provider.monthly":          "Monthly limit",
		"provider.keep_secret":      "Leave credential fields blank to keep the existing values.",
		"provider.quota_hint":       "Limits count sends from this service only; 0 means unlimited. Check your provider’s plan limits. Counters reset on UTC calendar boundaries.",
		"provider.single_recipient": "This provider supports one recipient per message.",
		"provider.smtp_tls":         "SMTP always uses TLS. Select the connection type required by your provider.",
		"provider.intro":            "Use a sender address verified with this provider. New providers start disabled. In failover routes, Posthorn tries enabled providers in name order before those in the configuration file. Each provider uses its own sender address. Permanent rejection does not trigger fallback.",
		"hint.providers":            "Enable or disable configured providers here. Provider credentials are never shown.",
		"hint.projects": "The key column shows a fingerprint. API keys cannot be recovered after creation. " +
			"Configured projects may have multiple keys during a rotation.",
		"hint.suppressions": "Removing a suppression is audited. A bounce or complaint suppression exists " +
			"to protect the provider account — remove one only when the address is known good.",
		"hint.message": "Message content is never shown. Recipients are masked.",
		"hint.audit":   "Audit rows are never pruned by the retention sweep, and carry no credentials.",

		// --- message detail row labels ---
		"msg.project":        "Project",
		"msg.endpoint":       "Endpoint",
		"msg.from":           "From",
		"msg.to":             "To",
		"msg.subject":        "Subject",
		"msg.status":         "Status",
		"msg.delivery_state": "Delivery state",
		"msg.provider":       "Provider",
		"msg.provider_id":    "Provider message id",
		"msg.created":        "Created",
		"msg.sent":           "Sent",
		"msg.last_error":     "Last error",

		// --- footer (printf-style: %s version, %s timestamp) ---
		"footer.fmt": "Posthorn %s · %s · recipients are masked and message bodies are never shown",
	},
	langZH: {
		"label.language":               "语言",
		"title.admin":                  "Posthorn 管理后台",
		"field.from":                   "已验证的发件地址",
		"field.api_key":                "API 密钥",
		"field.domain":                 "Mailgun 发信域名",
		"field.mailgun_region":         "Mailgun 服务区域",
		"field.region":                 "AWS 区域",
		"field.access_key_id":          "AWS 访问密钥 ID",
		"field.secret_access_key":      "AWS 秘密访问密钥",
		"field.account_id":             "Cloudflare 账户 ID",
		"field.api_token":              "API 令牌",
		"field.host":                   "SMTP 服务器地址",
		"field.port":                   "SMTP 端口",
		"field.username":               "SMTP 用户名",
		"field.password":               "SMTP 密码",
		"field.implicit_tls":           "TLS 连接方式",
		"field.region_us":              "美国",
		"field.region_eu":              "欧盟",
		"flash.bad_request":            "请求无效。",
		"flash.invalid_token":          "管理员令牌无效。",
		"flash.too_many_attempts":      "登录尝试过多，请稍后再试。",
		"flash.invalid_filter":         "筛选条件无效。",
		"flash.action_failed":          "操作失败。",
		"flash.done":                   "操作已完成。",
		"flash.provider_save_failed":   "无法保存服务商。请检查名称、发件地址、凭据和配额。",
		"flash.provider_saved":         "服务商已保存。",
		"flash.provider_update_failed": "无法更新服务商。",
		"flash.provider_state_updated": "服务商状态已更新。",

		"project.name":             "项目名称",
		"project.create":           "创建项目并生成密钥",
		"project.create_hint":      "每个应用使用独立的项目名称。密钥仅显示一次。",
		"project.rotate":           "更换密钥",
		"project.confirm_rotation": "我了解旧密钥将立即失效",
		"project.rotate_hint":      "更换密钥会立即使旧密钥失效，请及时更新应用配置。",
		"project.created_key":      "项目的 API 密钥：",
		"project.save_key":         "请立即复制并妥善保存此密钥，离开后无法再次查看。",
		"project.back":             "返回项目列表",
		"project.invalid_name":     "项目名称无效，请使用字母、数字、连字符或下划线，最多 64 个字符。",
		"project.exists":           "项目名称已存在。",
		"project.not_managed":      "此项目无法在此处管理。",
		"nav.dashboard":            "仪表盘",
		"nav.providers":            "服务商",
		"nav.projects":             "项目",
		"nav.messages":             "消息",
		"nav.audit":                "审计",
		"nav.signout":              "退出登录",

		"label.dashboard":       "仪表盘",
		"label.providers":       "服务商",
		"label.projects":        "项目",
		"label.messages":        "消息",
		"label.message":         "消息详情",
		"label.audit_log":       "审计日志",
		"label.retry_queue":     "重试队列",
		"label.dead_letters":    "死信",
		"label.suppressions":    "抑制名单",
		"label.attempts":        "发送尝试",
		"label.delivery_events": "投递事件",
		"label.signin":          "登录",

		"login.heading": "Posthorn 管理后台",
		"login.token":   "管理员 Token",
		"login.hint":    "Token 来自服务器上的 POSTHORN_ADMIN_TOKEN。登录后会换成会话 Cookie，Token 本身不会保存在浏览器里。",

		"card.retry_queue":  "重试队列",
		"card.dead_letters": "死信",
		"card.providers":    "服务商",
		"card.projects":     "项目",
		"card.suppressions": "抑制名单",
		"card.storage":      "存储",

		"pill.ok":                 "正常",
		"pill.degraded":           "异常",
		"pill.disabled":           "已禁用",
		"pill.in_rotation":        "使用中",
		"pill.active":             "生效中",
		"pill.suspended":          "已暂停",
		"pill.disabled_in_config": "配置中已禁用",
		"pill.sent":               "已发送",
		"pill.failed":             "失败",
		"pill.keys_live_suffix":   "个密钥同时生效",

		"th.name":            "名称",
		"th.type":            "类型",
		"th.circuit":         "熔断器",
		"th.state":           "状态",
		"th.today":           "今日",
		"th.month":           "本月",
		"th.webhook":         "Webhook",
		"th.verified":        "已验证",
		"th.message":         "消息",
		"th.project":         "项目",
		"th.attempt":         "尝试次数",
		"th.next_attempt":    "下次尝试",
		"th.last_error":      "最近错误",
		"th.to":              "收件人",
		"th.created":         "创建时间",
		"th.allowed_senders": "允许发件人",
		"th.key":             "密钥",
		"th.smtp":            "SMTP",
		"th.status":          "状态",
		"th.delivery":        "投递状态",
		"th.provider":        "服务商",
		"th.address":         "地址",
		"th.reason":          "原因",
		"th.source":          "来源",
		"th.added":           "添加时间",
		"th.result":          "结果",
		"th.error_class":     "错误类别",
		"th.provider_msg_id": "服务商消息 ID",
		"th.started":         "开始时间",
		"th.occurred":        "发生时间",
		"th.event_id":        "事件 ID",
		"th.when":            "时间",
		"th.actor":           "操作者",
		"th.action":          "操作",
		"th.target":          "目标",
		"th.details":         "详情",

		"btn.filter":        "筛选",
		"btn.retry":         "重试",
		"btn.enable":        "启用",
		"btn.disable":       "禁用",
		"btn.reset_breaker": "重置熔断器",
		"btn.restore":       "恢复",
		"btn.suspend":       "暂停",
		"btn.unsuppress":    "移除抑制",
		"btn.next_page":     "下一页 →",

		"filter.project":  "项目",
		"filter.status":   "状态",
		"filter.provider": "服务商",

		"empty.providers":    "服务商池为空。",
		"empty.queue":        "队列为空。",
		"empty.dead_letters": "没有死信。",
		"empty.projects":     "未配置任何项目。",
		"empty.suppressions": "没有抑制记录。",
		"empty.messages":     "没有消息记录。",
		"empty.attempts":     "没有尝试记录。",
		"empty.events":       "没有投递事件。",
		"empty.audit":        "暂无记录。",

		"provider.add":              "添加服务商",
		"provider.edit":             "编辑",
		"provider.configure":        "配置服务商",
		"provider.type":             "服务商类型",
		"provider.name":             "配置名称（字母或数字开头，可使用点、下划线和连字符，最多 64 位）",
		"provider.next":             "下一步",
		"provider.save":             "保存配置",
		"provider.back":             "返回服务商列表",
		"provider.unlimited":        "不限",
		"provider.daily":            "每日上限",
		"provider.monthly":          "每月上限",
		"provider.keep_secret":      "留空凭据可保留已保存的值。",
		"provider.quota_hint":       "仅统计本服务发送的邮件，0 表示不限。请核对服务商账号的实际额度；按 UTC 日历重置。",
		"provider.single_recipient": "该服务商每封邮件只支持一个收件人。",
		"provider.smtp_tls":         "SMTP 始终使用加密连接，请按服务商的要求选择连接方式。",
		"provider.intro":            "请填写在该服务商验证过的发件地址。新服务商保存后默认停用。在故障转移路由中，系统按名称顺序优先尝试后台启用的服务商，之后再尝试配置文件中的服务商。每个服务商使用自己的发件地址。永久性拒绝不会自动切换备用通道。",
		"hint.providers":            "可以在这里启用或停用已配置的服务商。服务商凭据不会显示。",
		"hint.projects":             "密钥列显示的是指纹。密钥生成后无法再次查看；配置文件中的项目在轮换期间可能有多个密钥。",
		"hint.suppressions": "移除一条抑制记录会被记入审计日志。退信或投诉类的抑制存在是为了保护服务商账号——" +
			"只有确认这个地址没问题时才应该移除。",
		"hint.message": "消息正文永远不会显示，收件人地址会被打码。",
		"hint.audit":   "审计记录不受数据保留清理策略影响、永久保存，且不包含任何凭据。",

		"msg.project":        "项目",
		"msg.endpoint":       "端点",
		"msg.from":           "发件人",
		"msg.to":             "收件人",
		"msg.subject":        "主题",
		"msg.status":         "状态",
		"msg.delivery_state": "投递状态",
		"msg.provider":       "服务商",
		"msg.provider_id":    "服务商消息 ID",
		"msg.created":        "创建时间",
		"msg.sent":           "发送时间",
		"msg.last_error":     "最近错误",

		"footer.fmt": "Posthorn %s · %s · 收件人已打码，正文永不显示",
	},
}
