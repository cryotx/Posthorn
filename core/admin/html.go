package admin

// uiHTML holds every admin page.
//
// One template set, no external assets, no script tags. Values render
// through html/template's contextual escaping, so a subject or an error
// string that came from a provider payload is inert text here by
// construction rather than by anyone remembering to escape it.
//
// The pages show MASKED recipients and never a message body: the
// operator running the gateway is not the owner of the mail passing
// through it (FR146).
//
// Bilingual (English/Chinese), added 2026-09-13. Every {{t $.Lang "key"}}
// call resolves through the catalog in i18n.go. Scope boundary, stated
// once here rather than at each call site: UI CHROME is translated —
// nav, headings, table columns, buttons, empty states, explanatory
// copy. RAW CONTENT is not — provider/project names (operator-chosen),
// message ids, timestamps, HTTP status/error-class tokens, and a
// provider's own capability values (yes/no/unsupported) stay as-is.
// They are either arbitrary (a name has no translation) or technical
// vocabulary an operator reading this matrix already reads in English
// throughout the rest of Posthorn's docs; translating them would invent
// inconsistency, not remove it.
const uiHTML = `
{{define "head"}}<!-- rendered inside the page templates below -->
<title>Posthorn admin — {{t .Lang .TitleKey}}</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #f7f7f5; --surface: #ffffff; --border: #e5e5e2; --text: #1c1c1a; --muted: #6b6b66;
    --accent: #3b6ef2; --accent-ink: #1d4fd6;
    --ok-bg: #dcf3e2; --ok-ink: #16743a;
    --warn-bg: #fdecd0; --warn-ink: #8a5a10;
    --bad-bg: #fbdede; --bad-ink: #a3232e;
    --shadow: 0 1px 2px rgba(20,20,15,.05), 0 1px 8px rgba(20,20,15,.04);
  }
  * { box-sizing: border-box; }
  body { font: 14px/1.55 ui-sans-serif, system-ui, -apple-system, "Segoe UI", "PingFang SC",
         "Microsoft YaHei", sans-serif; margin: 0; background: var(--bg); color: var(--text); }
  header { background: #16181d; color: #eef0f4; padding: 12px 24px;
           display: flex; gap: 22px; align-items: center; flex-wrap: wrap;
           border-bottom: 1px solid #23262e; }
  header h1 { font-size: 15px; margin: 0; font-weight: 700; letter-spacing: .01em;
              display: flex; align-items: center; gap: 8px; }
  header h1 .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--accent);
                   box-shadow: 0 0 0 3px rgba(59,110,242,.25); }
  nav { display: flex; gap: 4px; flex-wrap: wrap; }
  nav a { color: #aeb4c2; text-decoration: none; font-size: 13px; padding: 6px 10px;
          border-radius: 6px; transition: color .1s, background .1s; }
  nav a:hover { color: #fff; background: #22252c; }
  nav a.current { color: #fff; background: #262a33; box-shadow: inset 0 -2px 0 var(--accent); }
  .top-right { margin-left: auto; display: flex; align-items: center; gap: 14px; }
  .lang-switch { display: flex; gap: 2px; font-size: 12px; }
  .lang-switch a { color: #8b93a3; text-decoration: none; padding: 3px 7px; border-radius: 4px; }
  .lang-switch a.current { color: #fff; background: #2c303a; font-weight: 600; }
  .lang-switch a:not(.current):hover { color: #d7dae1; }
  main { padding: 24px; max-width: 1150px; margin: 0 auto; }
  h2 { font-size: 14px; margin: 28px 0 10px; font-weight: 700; letter-spacing: .01em;
       color: var(--text); text-transform: uppercase; opacity: .78; }
  h2:first-of-type { margin-top: 4px; }
  table { border-collapse: collapse; width: 100%; margin-bottom: 22px;
          background: var(--surface); box-shadow: var(--shadow); border-radius: 8px;
          overflow: hidden; }
  th, td { text-align: left; padding: 9px 12px; border-bottom: 1px solid var(--border);
           font-size: 13px; vertical-align: top; }
  th { background: #fafaf8; font-weight: 600; font-size: 11.5px; text-transform: uppercase;
       letter-spacing: .03em; color: var(--muted); }
  tbody tr:last-child td { border-bottom: none; }
  tbody tr:hover td { background: rgba(59,110,242,.035); }
  td.num { text-align: right; font-variant-numeric: tabular-nums; }
  code, .mono { font-family: ui-monospace, "SF Mono", Menlo, "Cascadia Code", monospace; font-size: 12px; }
  .pill { display: inline-block; padding: 2px 9px; border-radius: 999px; font-size: 11px;
          font-weight: 600; background: #ececea; color: var(--muted); }
  .ok { background: var(--ok-bg); color: var(--ok-ink); }
  .warn { background: var(--warn-bg); color: var(--warn-ink); }
  .bad { background: var(--bad-bg); color: var(--bad-ink); }
  .cards { display: flex; gap: 12px; flex-wrap: wrap; margin-bottom: 8px; }
  .card { background: var(--surface); padding: 14px 18px; min-width: 128px;
          box-shadow: var(--shadow); border-radius: 10px; border: 1px solid var(--border); }
  .card .n { font-size: 24px; font-weight: 700; font-variant-numeric: tabular-nums; letter-spacing: -.01em; }
  .card .l { font-size: 11px; text-transform: uppercase; letter-spacing: .05em; color: var(--muted);
             margin-top: 2px; }
  form.inline { display: inline; }
  button { font: inherit; font-size: 12.5px; padding: 5px 12px; cursor: pointer;
           border: 1px solid var(--border); background: var(--surface); border-radius: 6px;
           color: var(--text); transition: border-color .1s, background .1s; }
  button:hover { border-color: #c7c7c2; background: #fafaf8; }
  button.danger { border-color: #e3b3b3; color: var(--bad-ink); }
  button.danger:hover { background: var(--bad-bg); }
  .notice { background: var(--ok-bg); color: var(--ok-ink); padding: 10px 14px; margin-bottom: 14px;
            border-radius: 8px; font-size: 13px; }
  .error { background: var(--bad-bg); color: var(--bad-ink); padding: 10px 14px; margin-bottom: 14px;
           border-radius: 8px; font-size: 13px; }
  .muted { color: var(--muted); }
  p.muted { font-size: 12.5px; line-height: 1.6; max-width: 760px; }
  footer { padding: 16px 24px 32px; color: var(--muted); font-size: 11.5px; text-align: center; }
  input[type=password], input[type=text] { font: inherit; padding: 8px 10px;
    border: 1px solid var(--border); border-radius: 6px; min-width: 320px; background: var(--surface);
    color: var(--text); }
  input[type=text] { min-width: 160px; }
  .provider-form { max-width: 650px; background: var(--surface); padding: 20px; border: 1px solid var(--border); border-radius: 8px; }
  .provider-form label { display: block; margin: 12px 0 4px; }
  .provider-form input:not([type=checkbox]), .provider-form select { width: 100%; max-width: 500px; min-width: 0; padding: 8px 10px; border: 1px solid var(--border); border-radius: 6px; background: var(--surface); color: var(--text); font: inherit; }
  .provider-form button { margin-top: 18px; }

  input:focus, button:focus { outline: 2px solid var(--accent); outline-offset: 1px; }
  .login-wrap { display: flex; justify-content: center; padding-top: 10vh; }
  .login-card { background: var(--surface); border: 1px solid var(--border); border-radius: 12px;
                box-shadow: var(--shadow); padding: 32px; width: 100%; max-width: 380px; }
  .login-card h2 { margin-top: 0; text-transform: none; font-size: 17px; opacity: 1; }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #131417; --surface: #1c1e23; --border: #2b2e36; --text: #e7e8ea; --muted: #9a9ea8;
      --ok-bg: #163826; --ok-ink: #5fd88b;
      --warn-bg: #3d2f10; --warn-ink: #e3b04f;
      --bad-bg: #3a1b1e; --bad-ink: #f28b93;
      --shadow: none;
    }
    th { background: #202226; }
    tbody tr:hover td { background: rgba(59,110,242,.08); }
    .pill { background: #2a2c32; }
  }
</style>
{{end}}

{{define "chrome"}}
<header>
  <h1><span class="dot"></span>Posthorn</h1>
  <nav>
    <a href="/admin" {{if eq .Nav "dashboard"}}class="current"{{end}}>{{t .Lang "nav.dashboard"}}</a>
    <a href="/admin/providers" {{if eq .Nav "providers"}}class="current"{{end}}>{{t .Lang "nav.providers"}}</a>
    <a href="/admin/projects" {{if eq .Nav "projects"}}class="current"{{end}}>{{t .Lang "nav.projects"}}</a>
    <a href="/admin/messages" {{if eq .Nav "messages"}}class="current"{{end}}>{{t .Lang "nav.messages"}}</a>
    <a href="/admin/audit" {{if eq .Nav "audit"}}class="current"{{end}}>{{t .Lang "nav.audit"}}</a>
  </nav>
  <div class="top-right">
    <div class="lang-switch">
      <a href="?lang=en" {{if eq .Lang "en"}}class="current"{{end}}>EN</a>
      <a href="?lang=zh" {{if eq .Lang "zh"}}class="current"{{end}}>中文</a>
    </div>
    <form class="inline" method="post" action="/admin/logout">
      <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
      <button type="submit">{{t .Lang "nav.signout"}}</button>
    </form>
  </div>
</header>
{{end}}

{{define "flash"}}
{{if .Notice}}<div class="notice">{{.Notice}}</div>{{end}}
{{if .Error}}<div class="error">{{.Error}}</div>{{end}}
{{end}}

{{define "foot"}}
<footer>{{printf (t .Lang "footer.fmt") .Version (ts .Now)}}</footer>
{{end}}

{{define "login"}}
{{template "head" .}}
<div class="login-wrap"><div class="login-card">
  <h2>{{t .Lang "login.heading"}}</h2>
  {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
  <form method="post" action="/admin/login">
    <p><label>{{t .Lang "login.token"}}<br><input type="password" name="token" autocomplete="off" autofocus></label></p>
    <p><button type="submit">{{t .Lang "label.signin"}}</button></p>
  </form>
  <p class="muted">{{t .Lang "login.hint"}}</p>
  <p class="lang-switch" style="margin-top:16px">
    <a href="?lang=en" {{if eq .Lang "en"}}class="current"{{end}}>EN</a> ·
    <a href="?lang=zh" {{if eq .Lang "zh"}}class="current"{{end}}>中文</a>
  </p>
</div></div>
{{template "foot" .}}
{{end}}

{{define "dashboard"}}
{{template "head" .}}{{template "chrome" .}}
<main>
{{template "flash" .}}
<div class="cards">
  <div class="card"><div class="n">{{.Overview.QueueDepth}}</div><div class="l">{{t .Lang "card.retry_queue"}}</div></div>
  <div class="card"><div class="n">{{.Overview.DeadLetters}}</div><div class="l">{{t .Lang "card.dead_letters"}}</div></div>
  <div class="card"><div class="n">{{len .Overview.Providers}}</div><div class="l">{{t .Lang "card.providers"}}</div></div>
  <div class="card"><div class="n">{{len .Overview.Projects}}</div><div class="l">{{t .Lang "card.projects"}}</div></div>
  <div class="card"><div class="n">{{.Overview.Suppressions}}</div><div class="l">{{t .Lang "card.suppressions"}}</div></div>
  <div class="card"><div class="n">{{if .Overview.StorageHealthy}}<span class="pill ok">{{t .Lang "pill.ok"}}</span>{{else}}<span class="pill bad">{{t .Lang "pill.degraded"}}</span>{{end}}</div><div class="l">{{t .Lang "card.storage"}}</div></div>
</div>

<h2>{{t .Lang "label.providers"}}</h2>
<table>
  <tr><th>{{t .Lang "th.name"}}</th><th>{{t .Lang "th.type"}}</th><th>{{t .Lang "th.circuit"}}</th><th>{{t .Lang "th.state"}}</th><th class="num">{{t .Lang "th.today"}}</th><th class="num">{{t .Lang "th.month"}}</th></tr>
  {{range .Overview.Providers}}
  <tr>
    <td>{{.Name}}</td><td>{{.Type}}</td>
    <td>{{if eq .Circuit "closed"}}<span class="pill ok">{{.Circuit}}</span>
        {{else if eq .Circuit "half-open"}}<span class="pill warn">{{.Circuit}}</span>
        {{else}}<span class="pill bad">{{.Circuit}}</span>{{end}}</td>
    <td>{{if .Disabled}}<span class="pill bad">{{t $.Lang "pill.disabled"}}</span>{{else}}<span class="pill ok">{{t $.Lang "pill.in_rotation"}}</span>{{end}}</td>
    <td class="num">{{.UsedToday}}{{if .DailyLimit}} / {{.DailyLimit}}{{end}}</td>
    <td class="num">{{.UsedMonth}}{{if .MonthlyLimit}} / {{.MonthlyLimit}}{{end}}</td>
  </tr>
  {{else}}<tr><td colspan="6" class="muted">{{t .Lang "empty.providers"}}</td></tr>{{end}}
</table>

<h2>{{t .Lang "label.retry_queue"}}</h2>
<table>
  <tr><th>{{t .Lang "th.message"}}</th><th>{{t .Lang "th.project"}}</th><th class="num">{{t .Lang "th.attempt"}}</th><th>{{t .Lang "th.next_attempt"}}</th><th>{{t .Lang "th.last_error"}}</th></tr>
  {{range .Queue}}
  <tr><td class="mono"><a href="/admin/messages/{{.MessageID}}">{{.MessageID}}</a></td>
      <td>{{orDash .Project}}</td><td class="num">{{.Attempt}}</td>
      <td>{{ts .NextAttemptAt}}</td><td class="muted">{{orDash .LastError}}</td></tr>
  {{else}}<tr><td colspan="5" class="muted">{{t .Lang "empty.queue"}}</td></tr>{{end}}
</table>

<h2>{{t .Lang "label.dead_letters"}}</h2>
<table>
  <tr><th>{{t .Lang "th.message"}}</th><th>{{t .Lang "th.project"}}</th><th>{{t .Lang "th.to"}}</th><th>{{t .Lang "th.created"}}</th><th>{{t .Lang "th.last_error"}}</th><th></th></tr>
  {{range .DeadLetters}}
  <tr><td class="mono"><a href="/admin/messages/{{.ID}}">{{.ID}}</a></td>
      <td>{{orDash .Project}}</td><td class="mono">{{join .To ", "}}</td>
      <td>{{ts .CreatedAt}}</td><td class="muted">{{orDash .LastError}}</td>
      <td><form class="inline" method="post" action="/admin/action">
        <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
        <input type="hidden" name="action" value="dead_letter_retry">
        <input type="hidden" name="target" value="{{.ID}}">
        <input type="hidden" name="back" value="/admin">
        <button type="submit">{{t $.Lang "btn.retry"}}</button></form></td></tr>
  {{else}}<tr><td colspan="6" class="muted">{{t .Lang "empty.dead_letters"}}</td></tr>{{end}}
</table>
</main>
{{template "foot" .}}
{{end}}

{{define "providers"}}
{{template "head" .}}{{template "chrome" .}}
<main>
{{template "flash" .}}
<h2>{{t .Lang "label.providers"}}</h2>
{{if .CanManageProviders}}
<p><a href="/admin/providers/configure">{{t .Lang "provider.add"}}</a></p>
<p class="muted">{{t .Lang "provider.intro"}}</p>
{{if .ManagedProviders}}
<table><tr><th>{{t .Lang "th.name"}}</th><th>{{t .Lang "th.type"}}</th><th>{{t .Lang "th.state"}}</th><th>{{t .Lang "th.today"}}</th><th>{{t .Lang "th.month"}}</th><th></th></tr>
{{range .ManagedProviders}}<tr><td>{{.Name}}</td><td>{{.Type}}</td><td>{{if .Enabled}}<span class="pill ok">{{t $.Lang "pill.in_rotation"}}</span>{{else}}<span class="pill bad">{{t $.Lang "pill.disabled"}}</span>{{end}}</td><td>{{if .UsageAvailable}}{{.UsedToday}}{{else}}—{{end}} / {{if .DailyLimit}}{{.DailyLimit}}{{else}}{{t $.Lang "provider.unlimited"}}{{end}}</td><td>{{if .UsageAvailable}}{{.UsedMonth}}{{else}}—{{end}} / {{if .MonthlyLimit}}{{.MonthlyLimit}}{{else}}{{t $.Lang "provider.unlimited"}}{{end}}</td><td>
<a href="/admin/providers/configure?edit={{.Name}}">{{t $.Lang "provider.edit"}}</a>
<form class="inline" method="post" action="/admin/providers/state"><input type="hidden" name="csrf_token" value="{{$.CSRFToken}}"><input type="hidden" name="name" value="{{.Name}}"><input type="hidden" name="enabled" value="{{if .Enabled}}false{{else}}true{{end}}"><button type="submit">{{if .Enabled}}{{t $.Lang "btn.disable"}}{{else}}{{t $.Lang "btn.enable"}}{{end}}</button></form>
</td></tr>{{end}}</table>
{{end}}
{{end}}
<table>
  <tr><th>{{t .Lang "th.name"}}</th><th>{{t .Lang "th.type"}}</th><th>{{t .Lang "th.circuit"}}</th><th>{{t .Lang "th.state"}}</th><th class="num">{{t .Lang "th.today"}}</th>
      <th class="num">{{t .Lang "th.month"}}</th><th>{{t .Lang "th.webhook"}}</th><th>{{t .Lang "th.verified"}}</th><th></th></tr>
  {{range .Providers}}
  <tr>
    <td>{{.Name}}</td><td>{{.Type}}</td>
    <td>{{if eq .Circuit "closed"}}<span class="pill ok">{{.Circuit}}</span>
        {{else if eq .Circuit "half-open"}}<span class="pill warn">{{.Circuit}}</span>
        {{else}}<span class="pill bad">{{.Circuit}}</span>{{end}}</td>
    <td>{{if .Disabled}}<span class="pill bad">{{t $.Lang "pill.disabled"}}</span>{{else}}<span class="pill ok">{{t $.Lang "pill.in_rotation"}}</span>{{end}}</td>
    <td class="num">{{.UsedToday}}{{if .DailyLimit}} / {{.DailyLimit}}{{end}}</td>
    <td class="num">{{.UsedMonth}}{{if .MonthlyLimit}} / {{.MonthlyLimit}}{{end}}</td>
    <td>{{if .Capabilities}}{{.Capabilities.Webhook}}{{else}}—{{end}}</td>
    <td>{{if .Capabilities}}{{.Capabilities.WebhookVerification}}{{else}}—{{end}}</td>
    <td>
      <form class="inline" method="post" action="/admin/action">
        <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
        <input type="hidden" name="target" value="{{.Name}}">
        <input type="hidden" name="back" value="/admin/providers">
        {{if .Disabled}}
          <input type="hidden" name="action" value="provider_enable">
          <button type="submit">{{t $.Lang "btn.enable"}}</button>
        {{else}}
          <input type="hidden" name="action" value="provider_disable">
          <button class="danger" type="submit">{{t $.Lang "btn.disable"}}</button>
        {{end}}
      </form>
      <form class="inline" method="post" action="/admin/action">
        <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
        <input type="hidden" name="action" value="breaker_reset">
        <input type="hidden" name="target" value="{{.Name}}">
        <input type="hidden" name="back" value="/admin/providers">
        <button type="submit">{{t $.Lang "btn.reset_breaker"}}</button>
      </form>
    </td>
  </tr>
  {{else}}<tr><td colspan="9" class="muted">{{t .Lang "empty.providers"}}</td></tr>{{end}}
</table>
<p class="muted">{{t .Lang "hint.providers"}}</p>
</main>
{{template "foot" .}}
{{end}}

{{define "provider_configure"}}
{{template "head" .}}{{template "chrome" .}}
<main>{{template "flash" .}}
<h2>{{t .Lang "provider.configure"}}</h2>
<p class="muted">{{t .Lang "provider.intro"}}</p>
{{if not .ProviderForm.Type}}
<form method="get" action="/admin/providers/configure" class="provider-form">
<label for="type">{{t .Lang "provider.type"}}</label>
<select name="type" id="type" required>
<option value="resend">Resend</option><option value="brevo">Brevo</option>
<option value="mailjet">Mailjet (SMTP)</option><option value="smtp2go">SMTP2GO (SMTP)</option>
<option value="postmark">Postmark</option><option value="mailgun">Mailgun</option>
<option value="ses">Amazon SES</option><option value="cloudflare">Cloudflare Email</option>
<option value="sendernet">Sender.net</option><option value="smtp">SMTP</option>
</select><br><button type="submit">{{t .Lang "provider.next"}}</button>
</form>
{{else}}
<form method="post" action="/admin/providers/configure" class="provider-form" autocomplete="off">
<input type="hidden" name="csrf_token" value="{{.CSRFToken}}"><input type="hidden" name="type" value="{{.ProviderForm.Type}}">
<p>{{t .Lang "provider.type"}}: <strong>{{.ProviderForm.DisplayType}}</strong></p>
<label for="name">{{t .Lang "provider.name"}}</label>
{{if .ProviderForm.Editing}}<input id="name" name="name" type="text" value="{{.ProviderForm.Name}}" readonly required>{{else}}<input id="name" name="name" type="text" maxlength="64" pattern="[A-Za-z0-9][A-Za-z0-9_.-]*" required>{{end}}
{{range .ProviderForm.Fields}}
<label for="field-{{.Name}}">{{.Label}}</label>
{{if .Secret}}<input id="field-{{.Name}}" type="password" name="{{.Name}}" autocomplete="new-password" {{if not $.ProviderForm.Editing}}required{{end}}>{{else if eq .Name "mailgun_region"}}<select id="field-{{.Name}}" name="{{.Name}}"><option value="us" {{if eq .Value "us"}}selected{{end}}>US / 美国</option><option value="eu" {{if eq .Value "eu"}}selected{{end}}>EU / 欧盟</option></select>{{else if eq .Name "implicit_tls"}}<select id="field-{{.Name}}" name="{{.Name}}"><option value="false" {{if eq .Value "false"}}selected{{end}}>STARTTLS (587)</option><option value="true" {{if eq .Value "true"}}selected{{end}}>TLS (465)</option></select>{{else}}<input id="field-{{.Name}}" type="text" name="{{.Name}}" value="{{.Value}}" required>{{end}}
{{end}}
{{if .ProviderForm.Editing}}<p class="muted">{{t .Lang "provider.keep_secret"}}</p>{{end}}
{{if eq .ProviderForm.Type "smtp"}}<p class="muted">{{t .Lang "provider.smtp_tls"}}</p>{{end}}
{{if or (eq .ProviderForm.Type "cloudflare") (eq .ProviderForm.Type "sendernet")}}<p class="muted">{{t .Lang "provider.single_recipient"}}</p>{{end}}
<label for="daily_limit">{{t .Lang "provider.daily"}}</label><input id="daily_limit" type="number" name="daily_limit" value="{{.ProviderForm.DailyLimit}}" min="0" required>
<label for="monthly_limit">{{t .Lang "provider.monthly"}}</label><input id="monthly_limit" type="number" name="monthly_limit" value="{{.ProviderForm.MonthlyLimit}}" min="0" required>
<p class="muted">{{t .Lang "provider.quota_hint"}}</p>
<button type="submit">{{t .Lang "provider.save"}}</button>
</form>
{{end}}
<p><a href="/admin/providers">{{t .Lang "provider.back"}}</a></p>
</main>{{template "foot" .}}{{end}}

{{define "projects"}}
{{template "head" .}}{{template "chrome" .}}
<main>
{{template "flash" .}}
<h2>{{t .Lang "label.projects"}}</h2>
{{if .CanCreateProject}}
<form method="post" action="/admin/projects/create">
  <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
  <label for="project-name">{{t .Lang "project.name"}}</label>
  <input id="project-name" type="text" name="name" required maxlength="64" autocomplete="off">
  <button type="submit">{{t .Lang "project.create"}}</button>
</form>
<p class="muted">{{t .Lang "project.create_hint"}}</p>
{{end}}
<table>
  <tr><th>{{t .Lang "th.name"}}</th><th>{{t .Lang "th.state"}}</th><th>{{t .Lang "th.allowed_senders"}}</th><th>{{t .Lang "th.key"}}</th><th>{{t .Lang "th.smtp"}}</th><th>{{t .Lang "th.webhook"}}</th><th></th></tr>
  {{range .Projects}}
  <tr>
    <td>{{.Name}}</td>
    <td>{{if .Active}}<span class="pill ok">{{t $.Lang "pill.active"}}</span>
        {{else if .DisabledByAdmin}}<span class="pill bad">{{t $.Lang "pill.suspended"}}</span>
        {{else}}<span class="pill warn">{{t $.Lang "pill.disabled_in_config"}}</span>{{end}}</td>
    <td class="mono">{{join .AllowedFrom ", "}}</td>
    <td class="mono">{{.APIKeyFingerprint}}{{if gt .ActiveKeys 1}} <span class="pill warn">{{.ActiveKeys}} {{t $.Lang "pill.keys_live_suffix"}}</span>{{end}}</td>
    <td>{{if .HasSMTPIdentity}}✓{{else}}—{{end}}</td>
    <td>{{if .HasWebhook}}✓{{else}}—{{end}}</td>
    <td>
      <form class="inline" method="post" action="/admin/action">
        <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
        <input type="hidden" name="target" value="{{.Name}}">
        <input type="hidden" name="back" value="/admin/projects">
        {{if .DisabledByAdmin}}
          <input type="hidden" name="action" value="project_enable">
          <button type="submit">{{t $.Lang "btn.restore"}}</button>
        {{else}}
          <input type="hidden" name="action" value="project_disable">
          <button class="danger" type="submit">{{t $.Lang "btn.suspend"}}</button>
        {{end}}
      </form>
      {{if .Managed}}
      <form class="inline" method="post" action="/admin/projects/{{.Name}}/rotate">
        <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
        <label><input type="checkbox" name="confirm" value="yes" required> {{t $.Lang "project.confirm_rotation"}}</label>
        <button class="danger" type="submit">{{t $.Lang "project.rotate"}}</button>
      </form>
      {{end}}
    </td>
  </tr>
  {{else}}<tr><td colspan="7" class="muted">{{t .Lang "empty.projects"}}</td></tr>{{end}}
</table>
<p class="muted">{{t .Lang "hint.projects"}}</p>
{{if .CanCreateProject}}<p class="muted">{{t .Lang "project.rotate_hint"}}</p>{{end}}

<h2>{{t .Lang "label.suppressions"}}</h2>
<table>
  <tr><th>{{t .Lang "th.address"}}</th><th>{{t .Lang "th.reason"}}</th><th>{{t .Lang "th.source"}}</th><th>{{t .Lang "th.added"}}</th><th></th></tr>
  {{range .Suppressions}}
  <tr><td class="mono">{{.Email}}</td><td>{{.Reason}}</td>
      <td>{{orDash .SourceEndpoint}}</td><td>{{ts .CreatedAt}}</td>
      <td><form class="inline" method="post" action="/admin/action">
        <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
        <input type="hidden" name="action" value="suppression_remove">
        <input type="hidden" name="target" value="{{.Email}}">
        <input type="hidden" name="back" value="/admin/projects">
        <button class="danger" type="submit">{{t $.Lang "btn.unsuppress"}}</button></form></td></tr>
  {{else}}<tr><td colspan="5" class="muted">{{t .Lang "empty.suppressions"}}</td></tr>{{end}}
</table>
<p class="muted">{{t .Lang "hint.suppressions"}}</p>
</main>
{{template "foot" .}}
{{end}}

{{define "project_key"}}
{{template "head" .}}{{template "chrome" .}}
<main>
<h2>{{t .Lang "label.projects"}}</h2>
{{if .ProjectKeyError}}
<div class="error">{{if eq .ProjectKeyError "invalid"}}{{t .Lang "project.invalid_name"}}{{else if eq .ProjectKeyError "exists"}}{{t .Lang "project.exists"}}{{else}}{{t .Lang "project.not_managed"}}{{end}}</div>
{{else}}
<p>{{t .Lang "project.created_key"}} <strong>{{.NewProjectName}}</strong></p>
<p class="notice">{{t .Lang "project.save_key"}}</p>
<p><code style="overflow-wrap:anywhere;user-select:all">{{.NewProjectKey}}</code></p>
{{end}}
<p><a href="/admin/projects">{{t .Lang "project.back"}}</a></p>
</main>
{{template "foot" .}}
{{end}}

{{define "messages"}}
{{template "head" .}}{{template "chrome" .}}
<main>
{{template "flash" .}}
<h2>{{t .Lang "label.messages"}}</h2>
<form method="get" action="/admin/messages">
  <input type="text" name="project" placeholder="{{t .Lang "filter.project"}}" value="">
  <input type="text" name="status" placeholder="{{t .Lang "filter.status"}}" value="">
  <input type="text" name="provider" placeholder="{{t .Lang "filter.provider"}}" value="">
  <button type="submit">{{t .Lang "btn.filter"}}</button>
</form>
<table>
  <tr><th>ID</th><th>{{t .Lang "th.project"}}</th><th>{{t .Lang "th.to"}}</th><th>{{t .Lang "th.status"}}</th><th>{{t .Lang "th.delivery"}}</th>
      <th>{{t .Lang "th.provider"}}</th><th>{{t .Lang "th.created"}}</th></tr>
  {{range .Messages}}
  <tr><td class="mono"><a href="/admin/messages/{{.ID}}">{{.ID}}</a></td>
      <td>{{orDash .Project}}</td><td class="mono">{{join .To ", "}}</td>
      <td>{{.Status}}</td><td>{{orDash .DeliveryState}}</td>
      <td>{{orDash .Provider}}</td><td>{{ts .CreatedAt}}</td></tr>
  {{else}}<tr><td colspan="7" class="muted">{{t .Lang "empty.messages"}}</td></tr>{{end}}
</table>
{{if .NextCursor}}<p><a href="/admin/messages?cursor={{.NextCursor}}">{{t .Lang "btn.next_page"}}</a></p>{{end}}
</main>
{{template "foot" .}}
{{end}}

{{define "message"}}
{{template "head" .}}{{template "chrome" .}}
<main>
<h2>{{t .Lang "label.message"}} {{.Message.ID}}</h2>
<table>
  <tr><th>{{t .Lang "msg.project"}}</th><td>{{orDash .Message.Project}}</td></tr>
  <tr><th>{{t .Lang "msg.endpoint"}}</th><td>{{.Message.Endpoint}}</td></tr>
  <tr><th>{{t .Lang "msg.from"}}</th><td class="mono">{{.Message.From}}</td></tr>
  <tr><th>{{t .Lang "msg.to"}}</th><td class="mono">{{join .Message.To ", "}}</td></tr>
  <tr><th>{{t .Lang "msg.subject"}}</th><td>{{orDash .Message.Subject}}</td></tr>
  <tr><th>{{t .Lang "msg.status"}}</th><td>{{.Message.Status}}</td></tr>
  <tr><th>{{t .Lang "msg.delivery_state"}}</th><td>{{orDash .Message.DeliveryState}}</td></tr>
  <tr><th>{{t .Lang "msg.provider"}}</th><td>{{orDash .Message.Provider}}</td></tr>
  <tr><th>{{t .Lang "msg.provider_id"}}</th><td class="mono">{{orDash .Message.ProviderMessageID}}</td></tr>
  <tr><th>{{t .Lang "msg.created"}}</th><td>{{ts .Message.CreatedAt}}</td></tr>
  <tr><th>{{t .Lang "msg.sent"}}</th><td>{{tsp .Message.SentAt}}</td></tr>
  <tr><th>{{t .Lang "msg.last_error"}}</th><td class="muted">{{orDash .Message.LastError}}</td></tr>
</table>

<h2>{{t .Lang "label.attempts"}}</h2>
<table>
  <tr><th class="num">#</th><th>{{t .Lang "th.provider"}}</th><th>{{t .Lang "th.result"}}</th><th>{{t .Lang "th.error_class"}}</th>
      <th>{{t .Lang "th.provider_msg_id"}}</th><th>{{t .Lang "th.started"}}</th></tr>
  {{range .Attempts}}
  <tr><td class="num">{{.Attempt}}</td><td>{{.Provider}}</td>
      <td>{{if eq .Result "sent"}}<span class="pill ok">{{t $.Lang "pill.sent"}}</span>
          {{else}}<span class="pill bad">{{t $.Lang "pill.failed"}}</span>{{end}}</td>
      <td>{{orDash .ErrorClass}}</td><td class="mono">{{orDash .ProviderMessageID}}</td>
      <td>{{ts .StartedAt}}</td></tr>
  {{else}}<tr><td colspan="6" class="muted">{{t .Lang "empty.attempts"}}</td></tr>{{end}}
</table>

<h2>{{t .Lang "label.delivery_events"}}</h2>
<table>
  <tr><th>{{t .Lang "th.type"}}</th><th>{{t .Lang "th.provider"}}</th><th>{{t .Lang "th.occurred"}}</th><th>{{t .Lang "th.event_id"}}</th></tr>
  {{range .Events}}
  <tr><td>{{.Type}}</td><td>{{orDash .Provider}}</td><td>{{ts .OccurredAt}}</td>
      <td class="mono">{{.EventID}}</td></tr>
  {{else}}<tr><td colspan="4" class="muted">{{t .Lang "empty.events"}}</td></tr>{{end}}
</table>
<p class="muted">{{t .Lang "hint.message"}}</p>
</main>
{{template "foot" .}}
{{end}}

{{define "audit"}}
{{template "head" .}}{{template "chrome" .}}
<main>
<h2>{{t .Lang "label.audit_log"}}</h2>
<table>
  <tr><th>{{t .Lang "th.when"}}</th><th>{{t .Lang "th.actor"}}</th><th>{{t .Lang "th.action"}}</th><th>{{t .Lang "th.target"}}</th><th>{{t .Lang "th.details"}}</th><th>{{t .Lang "th.source"}}</th></tr>
  {{range .Audit}}
  <tr><td>{{ts .At}}</td><td>{{.Actor}}</td><td class="mono">{{.Action}}</td>
      <td class="mono">{{orDash .Target}}</td>
      <td class="muted">{{range $k, $v := .Details}}{{$k}}={{$v}} {{end}}</td>
      <td class="mono">{{orDash .SourceIP}}</td></tr>
  {{else}}<tr><td colspan="6" class="muted">{{t .Lang "empty.audit"}}</td></tr>{{end}}
</table>
<p class="muted">{{t .Lang "hint.audit"}}</p>
</main>
{{template "foot" .}}
{{end}}
`
