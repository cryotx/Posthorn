// Modified for the cryotx/posthorn distribution.

// Command posthorn is the standalone Posthorn email-gateway binary.
//
// Usage:
//
//	posthorn serve --config /path/to/config.toml [--listen :8080]
//	posthorn validate --config /path/to/config.toml
//	posthorn version
//	posthorn help
//
// Loads a TOML config (with ${env.VAR} placeholder resolution), constructs
// one gateway.Handler per configured endpoint, mounts them on an
// http.ServeMux, and serves until SIGTERM/SIGINT.
//
// The validate subcommand parses + validates the config without starting
// the listener, useful for CI and pre-deploy checks.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/craigmccaskill/posthorn/admin"
	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/gateway"
	"github.com/craigmccaskill/posthorn/ingress"
	"github.com/craigmccaskill/posthorn/lifecycle"
	"github.com/craigmccaskill/posthorn/managedproviders"
	"github.com/craigmccaskill/posthorn/metrics"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/queryapi"
	"github.com/craigmccaskill/posthorn/response"
	"github.com/craigmccaskill/posthorn/smtp"
	"github.com/craigmccaskill/posthorn/spam"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// version is the release version. Replaced at build time with -ldflags
// "-X main.version=v1.0.0" in the release workflow (Story 5.3).
var version = "v0.0.1-dev"

const usage = `posthorn — the unified outbound mail layer for self-hosted projects.

Usage:
  posthorn serve         [--config <path>] [--listen <addr>]
  posthorn validate      [--config <path>]
  posthorn doctor        [--config <path>] [--provider <name>]
  posthorn probe         [--url <readyz-url>] [--timeout <d>] [--quiet]
  posthorn backup        [--config <path>] [--out <file>]
  posthorn restore       --from <file> [--to <db>] [--config <path>] [--yes]
  posthorn suppressions  <list | add <email> [--reason <r>] | remove <email>> [--config <path>]
  posthorn version
  posthorn help

Default config path:  /etc/posthorn/config.toml
Default listen addr:  :8080

Examples:
  posthorn serve --config ./posthorn.toml --listen :8080
  posthorn validate --config ./posthorn.toml
  posthorn doctor --config ./posthorn.toml
  posthorn backup --config ./posthorn.toml --out /backups/posthorn-2026-09-08.db
  posthorn restore --from /backups/posthorn-2026-09-08.db --config ./posthorn.toml --yes
  posthorn suppressions list --config ./posthorn.toml
  posthorn suppressions add bounced@example.com --config ./posthorn.toml
  posthorn suppressions remove bounced@example.com --config ./posthorn.toml
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "serve":
		if err := runServe(args); err != nil {
			fmt.Fprintln(os.Stderr, "posthorn:", err)
			os.Exit(1)
		}
	case "validate":
		if err := runValidate(args); err != nil {
			fmt.Fprintln(os.Stderr, "posthorn:", err)
			os.Exit(1)
		}
	case "probe":
		if err := runProbe(args); err != nil {
			fmt.Fprintln(os.Stderr, "posthorn:", err)
			os.Exit(1)
		}
	case "doctor":
		if err := runDoctor(args); err != nil {
			fmt.Fprintln(os.Stderr, "posthorn:", err)
			os.Exit(1)
		}
	case "backup":
		if err := runBackup(args); err != nil {
			fmt.Fprintln(os.Stderr, "posthorn:", err)
			os.Exit(1)
		}
	case "restore":
		if err := runRestore(args); err != nil {
			fmt.Fprintln(os.Stderr, "posthorn:", err)
			os.Exit(1)
		}
	case "suppressions":
		if err := runSuppressions(args); err != nil {
			fmt.Fprintln(os.Stderr, "posthorn:", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Println("posthorn", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "posthorn: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

// --- serve ---

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/posthorn/config.toml", "path to TOML config file")
	listen := fs.String("listen", ":8080", "TCP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := buildLogger(cfg.Logging)
	logger.Info("posthorn starting",
		slog.String("version", version),
		slog.String("listen", *listen),
		slog.String("config", *configPath),
		slog.Int("endpoints", len(cfg.Endpoints)),
	)

	// One metrics registry + recorder for the whole process, shared by the
	// HTTP mux (which also exposes /metrics) and the SMTP ingress (#57) so
	// SMTP submissions land in the same scrape as HTTP ones.
	metricsReg := metrics.New()
	recorder := metrics.NewRecorder(metricsReg)

	// v2.0: optional storage spine (FR76). Presence-activates (NFR25);
	// without [storage] the gate stays nil and every path is v1.x.
	var gate *storage.Gate
	if cfg.Storage != nil {
		maxSize, err := spam.ParseSize(cfg.Storage.EffectiveMaxSize())
		if err != nil {
			return fmt.Errorf("storage.max_size: %w", err)
		}
		store, err := storage.Open(storage.Config{
			Path:      cfg.Storage.Path,
			InMemory:  cfg.Storage.InMemory,
			Retention: cfg.Storage.EffectiveRetention(),
			MaxSize:   maxSize,
		})
		if err != nil {
			return fmt.Errorf("open storage: %w", err)
		}
		defer func() { _ = store.Close() }()
		gate = storage.NewGate(store, logger)
		recorder.SetStorageHealthy(true)
		// Move the metric with the gate, not with the probe: a write
		// failure degrades /healthz instantly, and the metric an
		// operator alerts on must not lag behind it.
		gate.OnHealthChange(recorder.SetStorageHealthy)

		// ADR-45: settle idempotency reservations orphaned by a crash.
		// Their submissions have just been recovered into the retry
		// queue, so the honest answer to the caller's retry is the 202
		// the dead request would have returned — not two minutes of
		// "already in flight" while the queue delivers.
		settled, rerr := store.RecoverPendingReservations(func(messageID string) (int, string, []byte) {
			body, merr := json.Marshal(response.Success{
				Status: "queued", SubmissionID: messageID, MessageID: messageID,
			})
			if merr != nil {
				return http.StatusAccepted, "application/json", []byte(`{"status":"queued"}`)
			}
			return http.StatusAccepted, "application/json", body
		})
		if rerr != nil {
			logger.Error("idempotency_recovery_failed", slog.String("error", rerr.Error()))
		} else if settled > 0 {
			logger.Warn("idempotency_reservations_recovered",
				slog.Int("count", settled),
				slog.String("effect", "these submissions were in flight at the last shutdown and are queued for retry; a caller retrying the same key now replays the queued response"))
		}
		logger.Info("storage_enabled",
			slog.String("path", cfg.Storage.Path),
			slog.Bool("in_memory", cfg.Storage.InMemory),
			slog.Duration("retention", cfg.Storage.EffectiveRetention()),
			slog.String("max_size", cfg.Storage.EffectiveMaxSize()),
		)
	}

	// NFR45: the outbound SSRF policy is installed BEFORE anything that
	// could make an operator-configured request is built. Its default
	// is the strict one, so the ordering protects against a future
	// call site added above this line as well.
	transport.SetOutboundPolicy(cfg.OutboundPolicy())
	if cfg.Security.AllowPrivateWebhookTargets {
		logger.Warn("private_webhook_targets_allowed",
			slog.String("effect", "webhook and quota URLs may reach loopback and private addresses; link-local (cloud metadata) stays blocked"))
	}

	// The provider pool is built once, before any ingress, so every
	// endpoint and the SMTP listener share one instance (and one
	// circuit breaker) per named provider.
	pool, err := buildPool(cfg.Providers, recorder)
	if err != nil {
		return err
	}
	if pool.Len() > 0 {
		logger.Info("provider_pool_built", slog.Any("providers", pool.Names()))
	}

	// Restore dashboard-managed providers even if the admin UI is later
	// disabled; their configured delivery routes must survive a restart.
	var providerManager *managedproviders.Manager
	var managedCount int
	if gate != nil {
		providerManager, err = managedproviders.New(gate.Store(), pool)
		if err != nil {
			return fmt.Errorf("restore managed providers: %w", err)
		}
		providers, err := providerManager.List()
		if err != nil {
			return fmt.Errorf("list managed providers: %w", err)
		}
		managedCount = len(providers)
	}

	// Quota counters are restored BEFORE any ingress starts, so the
	// first message after a restart is judged against the budget the
	// operator has actually spent — not against a fresh allowance.
	var quota transport.QuotaSource
	lq := buildQuota(cfg, gate)
	if lq == nil && gate != nil && (managedCount > 0 || os.Getenv(admin.AdminTokenEnv) != "") {
		lq = transport.NewLocalQuota(gate.Store(), gate.ReportError)
	}
	if lq != nil {
		if err := lq.Restore(time.Now()); err != nil {
			if managedCount > 0 {
				return fmt.Errorf("restore managed provider quota: %w", err)
			}
			// Degrade rather than refuse to start: mail first (NFR27).
			// Loud, because the operator is now over-permissive for
			// the rest of this period.
			logger.Error("quota_restore_failed",
				slog.String("error", err.Error()),
				slog.String("effect", "provider usage counters start from zero for this period"))
			if gate != nil {
				gate.ReportError(err)
			}
		}
		quota = lq
		publishQuotaGauges(pool, quota, recorder, time.Now())
		logger.Info("provider_quota_enabled",
			slog.String("source", config.QuotaSourceLocal),
			slog.Bool("persistent", gate != nil))
	}

	// FR107: the multi-tenant registry. Built before any ingress so
	// HTTP and SMTP share one identity model rather than each growing
	// its own (FR111). The API-key plaintext stops here: only digests
	// are retained (NFR44).
	projects, err := project.Build(cfg.ProjectConfigs())
	if err != nil {
		return err
	}
	// Projects created from the dashboard are persisted as key digests.
	// Restore them before attaching the registry to the gateway and before
	// restoring administrative suspension overrides.
	var projectKeys *project.Manager
	if gate != nil {
		for _, ep := range cfg.Endpoints {
			if ep.Auth != config.AuthAPIKey || ep.From == "" {
				continue
			}
			projectKeys, err = project.NewManager(projects, gate.Store(), ep.From)
			if err != nil {
				return fmt.Errorf("project key manager: %w", err)
			}
			if err := projectKeys.Restore(); err != nil {
				return fmt.Errorf("restore managed projects: %w", err)
			}
			break
		}
	}
	projectUsage := gateway.NewProjectUsage(gate)
	if projects.Len() > 0 {
		// FR125: a project whose allowance is already spent must not
		// get a fresh one because the process restarted.
		if rerr := projectUsage.Restore(projects.All(), time.Now()); rerr != nil {
			logger.Error("project_usage_restore_failed",
				slog.String("error", rerr.Error()),
				slog.String("effect", "project quota counters start from zero for this period"))
			if gate != nil {
				gate.ReportError(rerr)
			}
		}
		publishProjectQuotaGauges(projects, projectUsage, recorder, time.Now())

		// FR131: endpoint api_keys alongside projects is a migration
		// aid, not a destination. An endpoint key authenticates
		// WITHOUT a project identity, which means it bypasses the
		// project sender policy — the control that stops one tenant
		// sending as another. Say so at startup, name the endpoints,
		// and never print a key.
		var legacy []string
		for _, ep := range cfg.Endpoints {
			if ep.Auth == config.AuthAPIKey && len(ep.APIKeys) > 0 {
				legacy = append(legacy, ep.Path)
			}
		}
		if len(legacy) > 0 {
			sort.Strings(legacy)
			logger.Warn("legacy_endpoint_api_keys_in_use",
				slog.Any("endpoints", legacy),
				slog.String("effect", "these keys authenticate without a project identity and therefore bypass allowed_from_domains"),
				slog.String("action", "migrate callers to project api_keys, then remove api_keys from these endpoints"))
		}

		// FR144: a project an operator suspended stays suspended across
		// a restart. Without this, the kill switch would silently
		// release every time the process came back.
		if gate != nil {
			restoreAdminOverrides(gate.Store(), pool, projects, logger)
			// FR147: keys live in the config file, so a rotation is
			// noticed HERE rather than through a runtime mutation
			// surface. Digest-of-digests: enough to see the set
			// changed, never enough to reconstruct a key.
			recordKeyRotations(gate.Store(), projects, logger)
		}

		logger.Info("projects_enabled",
			slog.Any("projects", projects.Names()),
			slog.Int("enabled", projects.Enabled()),
			slog.Bool("smtp_identities", projects.HasSMTPIdentities()),
			slog.Bool("usage_persisted", gate != nil))
	}

	workersReady := newWorkerReadiness(cfg)
	mux, transports, err := buildMux(cfg, pool, quota, projects, projectUsage, logger, metricsReg, recorder, gate, workersReady, providerManager)
	if err != nil {
		return fmt.Errorf("build router: %w", err)
	}

	// FR143-FR147: the admin surface, mounted only when
	// POSTHORN_ADMIN_TOKEN is set. Not configured means not mounted —
	// "no token" must never degrade into "no authentication".
	//
	// secure=false: Posthorn does not terminate TLS on this listener.
	// The default deployment is 127.0.0.1 behind the operator's own
	// proxy, and claiming Secure on the session cookie there would make
	// it unusable rather than safer. Inferring it from the request
	// would be worse: one plain-HTTP request from a TLS-terminating
	// proxy would downgrade the cookie for everyone.
	adminHandler, aerr := buildAdmin(gate, pool, projects, projectKeys, providerManager, quota, logger, false)
	if aerr != nil {
		return fmt.Errorf("admin: %w", aerr)
	}
	if adminHandler != nil {
		for _, pattern := range admin.Routes() {
			mux.Handle(pattern, adminHandler)
		}
		logger.Info("admin_enabled",
			slog.String("path", "/admin"),
			slog.String("bind_warning", "the admin surface must not be exposed publicly — bind Posthorn to 127.0.0.1 and reach it through a tunnel or an authenticated proxy"))
	}

	// Server-level timeouts bound slow-body senders and slow-reading
	// clients so they can't hold a connection indefinitely (#42).
	// WriteTimeout's deadline is set when the request headers are read,
	// so it must cover the WHOLE remaining request: body read (bounded
	// by ReadTimeout) plus handler execution (bounded by the handler's
	// own 10s requestTimeout, which includes retry backoff). It must
	// therefore exceed ReadTimeout + requestTimeout, or a legitimate
	// slow upload followed by a provider retry would have its response
	// truncated after the mail was already sent — and form mode has no
	// idempotency, so the user's resubmit would double-send.
	server := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second, // > ReadTimeout (15s) + handler requestTimeout (10s)
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10, // 64 KB; explicit, was stdlib 1MB default
	}

	// HTTP ingress is the v1.0 form/api-mode listener. v1.0 block D
	// (SMTP ingress) appends a second ingress to this slice when the
	// operator configures [smtp_listener].
	ingresses := []ingress.Ingress{
		ingress.NewHTTPIngress(server, logger),
	}

	// v1.0 block D: optional SMTP listener (FR62). Built only when the
	// operator's TOML includes [smtp_listener].
	if cfg.SMTPListener != nil {
		smtpIng, smtpTransport, err := buildSMTPIngress(cfg.SMTPListener, pool, groupIndex(cfg), quota, gate, logger, recorder)
		if err != nil {
			return fmt.Errorf("build smtp_listener: %w", err)
		}
		if l, ok := smtpIng.(*smtp.Listener); ok {
			if gate != nil {
				l.AttachStorage(gate)
			}
			if projects.HasSMTPIdentities() {
				l.AttachProjects(projects, projectUsage)
			}
		}
		ingresses = append(ingresses, smtpIng)
		transports[smtpListenerEndpoint] = smtpTransport
		logger.Info("smtp_listener registered",
			slog.String("listen", cfg.SMTPListener.Listen),
			slog.String("transport", cfg.SMTPListener.Transport.Label()),
			slog.Any("providers", routedProviderNames(smtpTransport)),
			slog.String("strategy", cfg.SMTPListener.Transport.EffectiveStrategy()),
			slog.Int("smtp_users", len(cfg.SMTPListener.SMTPUsers)),
		)
	}

	// Background loops live for the life of the process and stop when
	// the ingresses shut down.
	bgCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()

	// Remaining-budget gauges refresh on a timer rather than per send:
	// the send path should not pay for a gauge write, and a scrape a
	// few seconds stale is fine for a number that moves one at a time.
	// The tick also re-publishes across a period rollover, so a
	// dashboard shows the reset without waiting for the next message.
	if quota != nil {
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-bgCtx.Done():
					return
				case now := <-t.C:
					publishQuotaGauges(pool, quota, recorder, now)
				}
			}
		}()
	}

	// FR115: project usage counters flush on a timer, so a busy
	// gateway does not pay a transaction per message for a counter
	// nobody reads in real time.
	if projects.Len() > 0 && gate != nil {
		go projectUsage.Run(bgCtx.Done(), 0)
		defer projectUsage.Flush()
	}
	if projects.Len() > 0 {
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-bgCtx.Done():
					return
				case now := <-t.C:
					publishProjectQuotaGauges(projects, projectUsage, recorder, now)
				}
			}
		}()
	}

	// v2.0: background retry worker + storage maintenance (FR78-FR80).
	// Both stop when the ingresses shut down.
	if gate != nil {
		ctx := bgCtx
		worker := &storage.Worker{
			Store:  gate.Store(),
			Send:   queuedSendFunc(transports),
			Logger: logger,
		}
		workersReady.markStarted()
		go worker.Run(ctx, storage.Hooks{
			OnSent:       recorder.QueueSent,
			OnRetryAgain: recorder.QueueRetried,
			OnDeadLetter: recorder.QueueDeadLettered,
		})
		workersReady.markStarted()
		go gate.RunMaintenance(ctx, 0, 0, cfg.Storage.EffectiveRetention(), storage.MaintenanceHooks{
			OnHealth: recorder.SetStorageHealthy,
			OnDepth:  recorder.SetQueueDepth,
		})

		// v2.0: lifecycle event ingestion + callback forwarding (FR82-FR85).
		if cfg.Lifecycle != nil {
			webhooks := map[string]lifecycle.Webhook{}
			for _, ep := range cfg.Endpoints {
				if ep.WebhookURL != "" {
					webhooks[ep.Path] = lifecycle.Webhook{URL: ep.WebhookURL, Secret: ep.WebhookSecret}
				}
			}
			// FR118: project callbacks share the endpoint forwarder's
			// queue, backoff and dead-letter rather than growing a
			// second delivery system. The scope key namespaces them
			// apart from endpoint paths.
			for _, p := range cfg.Projects {
				if p.WebhookURL != "" {
					webhooks["project:"+p.Name] = lifecycle.Webhook{URL: p.WebhookURL, Secret: p.WebhookSecret}
				}
			}
			fwd := &lifecycle.Forwarder{
				Webhooks: webhooks,
				Gate:     gate,
				Logger:   logger,
				OnResult: recorder.LifecycleForward,
				// Event forwarding calls operator-supplied URLs, so it
				// uses the guarded client rather than a plain one: the
				// check runs at connect time, on every retry from the
				// queue as well as the inline attempt (NFR45).
				Client: cfg.OutboundPolicy().Client(lifecycle.ForwardTimeout),
			}
			eventsHandler, err := lifecycle.NewHandler(
				cfg.Lifecycle.BasicAuthUsername, cfg.Lifecycle.BasicAuthPassword,
				gate, fwd, logger, recorder)
			if err != nil {
				return fmt.Errorf("build lifecycle handler: %w", err)
			}
			// The pre-adapter Postmark route is preserved byte-for-byte
			// so an existing deployment's configured webhook URL keeps
			// working unchanged.
			mux.Handle("/events/postmark", eventsHandler)
			// FR117/FR120: one route per provider dialect the operator
			// has CONFIGURED. Unconfigured dialects get no route at
			// all — mounting one that cannot verify anything would be
			// an unauthenticated door standing open.
			mounted := make([]string, 0, len(cfg.Lifecycle.Providers))
			for name, pw := range cfg.Lifecycle.Providers {
				adapter, known, aerr := lifecycle.BuildAdapter(name, lifecycle.ProviderAuthConfig{
					SigningSecret:  pw.SigningSecret,
					AllowedIPs:     pw.AllowedIPs,
					TrustedProxies: pw.TrustedProxies,
					Tolerance:      pw.Tolerance.Std(),
				})
				if !known || aerr != nil {
					return fmt.Errorf("lifecycle.providers.%s: %w", name, aerr)
				}
				mux.Handle("/events/"+name+"/unified", eventsHandler.WithAdapter(adapter))
				mounted = append(mounted, name)
			}
			sort.Strings(mounted)
			workersReady.markStarted()
			go fwd.RunQueue(ctx, 0)
			logger.Info("lifecycle_enabled",
				slog.Int("webhook_targets", len(webhooks)),
				slog.Any("verified_adapters", mounted))
		}
	}

	return runIngressesUntilSignal(ingresses, logger)
}

// smtpListenerEndpoint is the submission-log endpoint label for mail
// accepted by the SMTP listener (it has no HTTP path).
const smtpListenerEndpoint = "smtp_listener"

// queuedSendFunc resolves a queued submission's endpoint back to its
// transport. An endpoint that no longer exists in the config (edited
// between restarts) yields a terminal error, which dead-letters the
// row rather than looping forever.
func queuedSendFunc(transports map[string]transport.Transport) storage.SendFunc {
	return func(ctx context.Context, endpoint string, msg transport.Message) (transport.SendResult, error) {
		t, ok := transports[endpoint]
		if !ok {
			return transport.SendResult{}, &transport.TransportError{
				Class:   transport.ErrTerminal,
				Message: fmt.Sprintf("endpoint %q no longer configured", endpoint),
			}
		}
		return t.Send(ctx, msg)
	}
}

// buildSMTPIngress converts the config-package SMTPListenerConfig into
// the smtp-package ListenerConfig, constructs the outbound transport
// via the same registry the HTTP endpoints use, and returns the
// resulting smtp.Listener (which satisfies ingress.Ingress) plus the
// transport itself (the retry worker needs it to replay queued rows).
func buildSMTPIngress(c *config.SMTPListenerConfig, pool *transport.Pool, groups map[string]config.ProviderGroupConfig, quota transport.QuotaSource, gate *storage.Gate, logger *slog.Logger, recorder *metrics.Recorder) (ingress.Ingress, transport.Transport, error) {
	tp, err := buildRoutedTransport(smtpListenerEndpoint, c.Transport, pool, groups, quota, recorder, gate)
	if err != nil {
		return nil, nil, fmt.Errorf("transport: %w", err)
	}
	// Parse max_message_size (default 1MB if unset).
	rawSize := c.MaxMessageSize
	if rawSize == "" {
		rawSize = "1MB"
	}
	maxBody, err := spam.ParseSize(rawSize)
	if err != nil {
		return nil, nil, fmt.Errorf("max_message_size: %w", err)
	}
	listenerCfg := smtp.ListenerConfig{
		Listen:                  c.Listen,
		RequireTLS:              c.EffectiveRequireTLS(),
		TLSCert:                 c.TLSCert,
		TLSKey:                  c.TLSKey,
		ClientCertCA:            c.ClientCertCA,
		AuthRequired:            smtp.AuthMode(c.AuthRequired),
		AllowedSenders:          c.AllowedSenders,
		AllowedRecipients:       c.AllowedRecipients,
		MaxRecipientsPerSession: c.MaxRecipientsPerSession,
		MaxConnections:          c.MaxConnections,
		MaxConnectionsPerIP:     c.MaxConnectionsPerIP,
		MaxMessageSize:          rawSize,
		IdleTimeout:             c.IdleTimeout,
		MailKind:                c.MailKind,
		Transport:               c.Transport,
	}
	listenerCfg.SMTPUsers = make([]smtp.User, len(c.SMTPUsers))
	for i, u := range c.SMTPUsers {
		listenerCfg.SMTPUsers[i] = smtp.User{Username: u.Username, Password: u.Password}
	}
	if err := listenerCfg.Validate(); err != nil {
		return nil, nil, err
	}
	ing, err := smtp.New(listenerCfg, tp, maxBody, logger, recorder)
	if err != nil {
		return nil, nil, err
	}
	return ing, tp, nil
}

// runIngressesUntilSignal starts each ingress in its own goroutine,
// waits for SIGTERM/SIGINT, then drains in-flight work via Stop on
// each ingress with a 15s deadline (longer than the per-request 10s
// hard timeout from FR22 so in-flight retries can complete
// gracefully). A second signal forces immediate exit.
func runIngressesUntilSignal(ingresses []ingress.Ingress, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, len(ingresses))
	for _, ing := range ingresses {
		ing := ing
		go func() {
			err := ing.Start(ctx)
			if err != nil {
				errCh <- fmt.Errorf("%s ingress: %w", ing.Name(), err)
				return
			}
			errCh <- nil
		}()
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil {
			return err
		}
		// First ingress returned without error — unusual, but treat
		// as graceful end (other ingresses will follow).
	case sig := <-sigCh:
		logger.Info("shutdown signal received", slog.String("signal", sig.String()))
	}

	// Forced-exit watcher.
	go func() {
		sig := <-sigCh
		logger.Warn("second signal received, forcing exit", slog.String("signal", sig.String()))
		os.Exit(1)
	}()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	var firstErr error
	for _, ing := range ingresses {
		if err := ing.Stop(shutdownCtx); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("%s ingress graceful shutdown: %w", ing.Name(), err)
		}
	}
	logger.Info("posthorn stopped")
	return firstErr
}

// --- validate ---

func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/posthorn/config.toml", "path to TOML config file")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Load only validates structurally; we also try to construct a
	// transport and a gateway.Handler for each endpoint so template parse
	// errors and transport-key issues surface here too.
	transport.SetOutboundPolicy(cfg.OutboundPolicy())
	pool, err := buildPool(cfg.Providers, nil)
	if err != nil {
		return err
	}

	groups := groupIndex(cfg)
	for i, ep := range cfg.Endpoints {
		t, err := buildRoutedTransport(ep.Path, ep.Transport, pool, groups, nil, nil, nil)
		if err != nil {
			return fmt.Errorf("endpoints[%d] (%s): transport: %w", i, ep.Path, err)
		}
		if _, err := gateway.New(ep, t); err != nil {
			return fmt.Errorf("endpoints[%d] (%s): %w", i, ep.Path, err)
		}
	}

	// Build the SMTP ingress too (#58): its semantic checks (smtp_users
	// required, TLS cert/key readable, client-cert CA parseable) live in
	// buildSMTPIngress, not config.Load — so without this a listener-only
	// config that passes `validate` could still fail at `serve`.
	if cfg.SMTPListener != nil {
		if _, _, err := buildSMTPIngress(cfg.SMTPListener, pool, groups, nil, nil, buildLogger(cfg.Logging), nil); err != nil {
			return fmt.Errorf("smtp_listener: %w", err)
		}
	}

	summary := fmt.Sprintf("%d endpoint(s)", len(cfg.Endpoints))
	if pool.Len() > 0 {
		summary += fmt.Sprintf(", %d provider(s)", pool.Len())
	}
	if cfg.SMTPListener != nil {
		summary += " + smtp_listener"
	}
	fmt.Printf("config OK: %s\n", summary)
	return nil
}

// --- shared plumbing ---

// buildMux constructs an http.ServeMux mapping each configured endpoint
// path to its gateway.Handler. Endpoints share no state. Each handler
// gets the logger and the shared metrics Recorder so per-request
// submission_id propagation and operator observability work.
//
// The mux additionally registers `/healthz` (FR54) and `/metrics`
// (FR55) at fixed paths. Operators can firewall those paths at the
// reverse proxy if internal-only access is desired.
func buildMux(cfg *config.Config, pool *transport.Pool, quota transport.QuotaSource, projects *project.Registry, usage *gateway.ProjectUsage, logger *slog.Logger, metricsReg *metrics.Registry, recorder *metrics.Recorder, gate *storage.Gate, workersReady *workerReadiness, managed ...*managedproviders.Manager) (*http.ServeMux, map[string]transport.Transport, error) {
	groups := groupIndex(cfg)
	mux := http.NewServeMux()
	transports := make(map[string]transport.Transport, len(cfg.Endpoints))

	for i, ep := range cfg.Endpoints {
		t, err := buildRoutedTransport(ep.Path, ep.Transport, pool, groups, quota, recorder, gate)
		if err != nil {
			return nil, nil, fmt.Errorf("endpoints[%d] (%s): transport: %w", i, ep.Path, err)
		}
		// Managed providers augment pool-backed HTTP endpoints and the same
		// transport is retained for the retry worker below.
		if len(managed) > 0 && managed[0] != nil {
			t = managed[0].Wrap(t)
		}
		h, err := gateway.New(ep, t,
			gateway.WithLogger(logger),
			gateway.WithRecorder(recorder),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("endpoints[%d] (%s): %w", i, ep.Path, err)
		}
		if gate != nil {
			h.AttachStorage(gate)
		}
		// Keep the registry attached even when initially empty: projects may
		// be created by the admin after the listener has started.
		if projects != nil {
			h.AttachProjects(projects, usage)
		}
		transports[ep.Path] = t
		mux.Handle(ep.Path, h)
		logger.Info("endpoint registered",
			slog.String("path", ep.Path),
			slog.String("transport", ep.Transport.Label()),
			slog.Any("providers", routedProviderNames(t)),
			slog.String("strategy", ep.Transport.EffectiveStrategy()),
			slog.Int("recipients", len(ep.To)),
		)
	}

	// FR54: /healthz — always-on liveness probe. With storage configured
	// it also reports the canary state (FR80); always 200 because a
	// degraded disk doesn't stop mail flow (NFR27).
	var storageState metrics.StorageState
	if gate != nil {
		storageState = gate.Healthy
	}
	// FR97: with a pool configured, /healthz also reports each
	// provider's circuit state so an operator can see which upstream is
	// out of rotation without scraping /metrics.
	var providerState metrics.ProviderState
	if pool.Len() > 0 {
		providerState = func() map[string]string {
			out := make(map[string]string, pool.Len())
			for name, st := range pool.States() {
				out[name] = st.String()
			}
			return out
		}
	}
	// FR136-FR140: the project-scoped read API. Mounted only when both
	// projects and storage exist — without a project there is no scope
	// to answer in, and without storage there is nothing to answer
	// from. An unscoped read API would be a cross-tenant leak by
	// default, so its absence is the safe state.
	if projects.Len() > 0 && gate != nil {
		qapi := queryapi.New(queryapi.Deps{
			Store:    gate.Store(),
			Projects: projects,
			Logger:   logger,
		})
		for _, pattern := range queryapi.Routes() {
			mux.Handle(pattern, qapi)
		}
		logger.Info("query_api_enabled", slog.Any("routes", queryapi.Routes()))
	}

	mux.Handle("/healthz", metrics.HealthzHandlerWith(storageState, providerState))

	// FR149: /readyz answers a DIFFERENT question from /healthz —
	// "should traffic come to me", not "am I alive" — so it is wired to
	// different signals on purpose.
	//
	// A provider being down is deliberately absent from these checks.
	// That is the case the retry queue, the breaker and failover exist
	// to absorb; reporting unready would take Posthorn out of rotation
	// and turn mail that would have been queued into mail refused at
	// the door.
	readiness := []metrics.Readiness{
		{Name: "storage", Ready: func() (bool, string) {
			if gate == nil {
				// No [storage] block: v1.x synchronous operation is a
				// supported shape, and it is ready without a database.
				return true, "not configured"
			}
			if !gate.Healthy() {
				return false, "degraded — the write canary is failing"
			}
			return true, "ok"
		}},
		{Name: "router", Ready: func() (bool, string) {
			// Endpoints and their transports are built before this
			// handler is registered, so reaching /readyz at all means
			// routing is constructed. An SMTP-listener-only deployment
			// legitimately has zero HTTP endpoints.
			if len(transports) == 0 && cfg.SMTPListener == nil {
				return false, "no ingress is configured"
			}
			return true, "ok"
		}},
		{Name: "workers", Ready: workersReady.check},
	}
	mux.Handle("/readyz", metrics.ReadyzHandler(readiness...))
	// FR55: /metrics — Prometheus exposition. Same registry as the
	// Recorder above so all observations land in the scrape.
	mux.Handle("/metrics", metricsReg.Handler())

	return mux, transports, nil
}

// buildTransport constructs a transport from its config block. Dispatch
// is via the transport package's registry — each transport (postmark,
// resend, mailgun, ses, smtp-out) registers its builder at init.
// Adding a new transport requires no edits here.
//
// Only the inline (v1.x) shape goes through here. A pooled block is
// built by buildRoutedTransport, which needs the pool and the router's
// name and cannot be reduced to one settings map.
func buildTransport(cfg config.TransportConfig) (transport.Transport, error) {
	reg, ok := transport.Lookup(cfg.Type)
	if !ok {
		return nil, transport.UnknownTypeError(cfg.Type)
	}
	return reg.Build(cfg.Settings)
}

// buildPool constructs every [[providers]] entry once for the process.
// Sharing the built providers across endpoints is the whole point of a
// pool: one HTTP client and, critically, one circuit breaker per
// upstream, so an outage noticed on /api/contact is already known to
// /api/alerts.
//
// recorder may be nil (the validate path builds a pool without one).
func buildPool(cfgs []config.ProviderConfig, recorder *metrics.Recorder) (*transport.Pool, error) {
	providers := make([]*transport.Provider, 0, len(cfgs))
	for i, pc := range cfgs {
		t, err := buildTransport(config.TransportConfig{Type: pc.Type, Settings: pc.Settings})
		if err != nil {
			return nil, fmt.Errorf("providers[%d] (%s): %w", i, pc.Name, err)
		}
		bc := transport.BreakerConfig{}
		if pc.CircuitBreaker != nil {
			bc = transport.BreakerConfig{
				FailureThreshold: pc.CircuitBreaker.FailureThreshold,
				Cooldown:         pc.CircuitBreaker.Cooldown.Std(),
				HalfOpenProbes:   pc.CircuitBreaker.HalfOpenProbes,
				Disabled:         pc.CircuitBreaker.Disabled,
			}
		}
		breaker := transport.NewBreaker(bc)
		if recorder != nil {
			name := pc.Name
			breaker.OnTransition(func(st transport.CircuitState) {
				recorder.SetCircuitState(name, st.String())
			})
			// Publish the healthy series up front: to an alerting rule,
			// an absent series and a closed circuit look the same.
			recorder.SetCircuitState(name, transport.CircuitClosed.String())
		}
		var quota transport.Quota
		if pc.Quota != nil {
			quota = transport.Quota{Daily: pc.Quota.DailyLimit, Monthly: pc.Quota.MonthlyLimit}
		}
		if recorder != nil {
			// Publish the static facts up front so a dashboard has
			// series before the first send, and so an unlimited
			// provider is visibly absent from the quota gauges rather
			// than showing a made-up ceiling.
			recorder.SetProviderCost(pc.Name, int64(pc.CostPer1000.V))
			if quota.Daily > 0 {
				recorder.SetQuotaLimit(pc.Name, string(transport.WindowDaily), quota.Daily)
			}
			if quota.Monthly > 0 {
				recorder.SetQuotaLimit(pc.Name, string(transport.WindowMonthly), quota.Monthly)
			}
		}
		providers = append(providers, &transport.Provider{
			Name:      pc.Name,
			Type:      pc.Type,
			Weight:    pc.Weight,
			Transport: t,
			Breaker:   breaker,
			Quota:     quota,
			Cost:      pc.CostPer1000.V,
		})
	}
	return transport.NewPool(providers)
}

// buildQuota constructs the quota tracker for a pool.
//
// quota_source is validated at config-parse time and only "local" gets
// this far. gate may be nil (no [storage]), in which case counters live
// only in memory and a restart resets them — which the docs state
// plainly, because an operator relying on a monthly free tier needs to
// know that.
func buildQuota(cfg *config.Config, gate *storage.Gate) *transport.LocalQuota {
	if !quotaConfigured(cfg) {
		return nil
	}
	var store transport.UsageStore
	var onErr func(error)
	if gate != nil {
		store = gate.Store()
		onErr = gate.ReportError
	}
	return transport.NewLocalQuota(store, onErr)
}

// quotaConfigured reports whether any provider has a limit worth
// tracking. Without one the router gets a nil QuotaSource and the send
// path is exactly what it was before (NFR32).
func quotaConfigured(cfg *config.Config) bool {
	for _, p := range cfg.Providers {
		if p.Quota != nil && (p.Quota.DailyLimit > 0 || p.Quota.MonthlyLimit > 0) {
			return true
		}
	}
	return false
}

// publishQuotaGauges refreshes the remaining-budget gauges for every
// limited provider. Called after startup restore and on the maintenance
// tick, so a scrape sees fresh numbers without the send path paying for
// a gauge write per message.
func publishQuotaGauges(pool *transport.Pool, q transport.QuotaSource, rec *metrics.Recorder, now time.Time) {
	if pool == nil || q == nil || rec == nil {
		return
	}
	for _, p := range pool.All() {
		daily, hasDaily, monthly, hasMonthly := transport.Remaining(q, p, now)
		if hasDaily {
			rec.SetQuotaRemaining(p.Name, string(transport.WindowDaily), daily)
		}
		if hasMonthly {
			rec.SetQuotaRemaining(p.Name, string(transport.WindowMonthly), monthly)
		}
	}
}

// buildRoutedTransport turns one transport block into the Transport the
// ingress will call: the inline client for a v1.x block, or a
// transport.Router over pool entries for a pooled one.
//
// Returning a Transport either way is what keeps this change additive.
// Nothing downstream — gateway.Handler, smtp.Listener,
// transport.SendWithRetry, the storage retry worker — learns that a
// pool exists.
func buildRoutedTransport(name string, cfg config.TransportConfig, pool *transport.Pool,
	groups map[string]config.ProviderGroupConfig, quota transport.QuotaSource,
	recorder *metrics.Recorder, gate *storage.Gate) (transport.Transport, error) {
	if !cfg.Pooled() {
		return buildTransport(cfg)
	}

	def, err := buildRoute(nil, cfg.Group, cfg.Providers, cfg.Strategy, pool, groups)
	if err != nil {
		return nil, err
	}

	rules := make([]*transport.Route, 0, len(cfg.Rules))
	for i, rc := range cfg.Rules {
		kinds := make([]transport.MailKind, 0, len(rc.MatchKind))
		for _, k := range rc.MatchKind {
			kind, err := transport.ParseMailKind(k)
			if err != nil {
				return nil, fmt.Errorf("rules[%d]: match_kind: %w", i, err)
			}
			kinds = append(kinds, kind)
		}
		match := &transport.RuleMatch{
			RecipientDomains: rc.MatchRecipientDomain,
			Fields:           rc.MatchField,
			Kinds:            kinds,
		}
		route, err := buildRoute(match, rc.Group, rc.Providers, rc.Strategy, pool, groups)
		if err != nil {
			return nil, fmt.Errorf("rules[%d]: %w", i, err)
		}
		rules = append(rules, route)
	}

	router, err := transport.NewRouter(name, def, rules)
	if err != nil {
		return nil, err
	}
	router.Quota = quota
	if recorder != nil {
		router.Hooks = transport.RouterHooks{
			OnAttempt:             recorder.ProviderAttempt,
			OnSkipped:             recorder.ProviderSkipped,
			OnQuotaBlocked:        recorder.QuotaExhausted,
			OnFailover:            recorder.Failover,
			OnExhausted:           recorder.RouteExhausted,
			OnRouteQuotaExhausted: recorder.RouteQuotaExhausted,
		}
	}
	if gate != nil {
		// FR112: the durable per-message attempt trail. Best-effort by
		// construction — a storage failure must never stop mail, so a
		// missing trail row is reported and the send continues.
		router.Hooks.OnAttemptRecord = func(a transport.AttemptRecord) {
			if !gate.Healthy() {
				return
			}
			err := gate.Store().RecordAttempt(storage.Attempt{
				MessageID:         a.MessageID,
				Attempt:           a.Attempt,
				Provider:          a.Provider,
				StartedAt:         a.StartedAt,
				FinishedAt:        a.FinishedAt,
				Result:            a.Result,
				ErrorClass:        a.ErrorClass,
				ProviderMessageID: a.ProviderMessageID,
			})
			if err != nil {
				gate.ReportError(err)
			}
		}
	}
	return router, nil
}

// groupIndex maps group name → config, for route resolution.
func groupIndex(cfg *config.Config) map[string]config.ProviderGroupConfig {
	if len(cfg.ProviderGroups) == 0 {
		return nil
	}
	out := make(map[string]config.ProviderGroupConfig, len(cfg.ProviderGroups))
	for _, g := range cfg.ProviderGroups {
		out[g.Name] = g
	}
	return out
}

// buildRoute resolves one route's provider list, from a group name or
// an inline list. A group supplies its own strategy unless the
// referencing block states one — so "the free pool is free_first" is
// written once, and an endpoint can still deviate.
func buildRoute(match *transport.RuleMatch, group string, inline []string, strategy string,
	pool *transport.Pool, groups map[string]config.ProviderGroupConfig) (*transport.Route, error) {
	names := inline
	if group != "" {
		g, ok := groups[group]
		if !ok {
			return nil, fmt.Errorf("unknown provider group %q", group)
		}
		names = g.Providers
		if strategy == "" {
			strategy = g.Strategy
		}
	}
	provs, err := pool.Resolve(names)
	if err != nil {
		return nil, err
	}
	return transport.NewRoute(match, transport.Strategy(strategy), provs)
}

// publishProjectQuotaGauges refreshes the per-project budget gauges.
// Only projects with a configured limit get a series: an unlimited
// project has no meaningful remaining, and a fabricated ceiling would
// break any alert written against it.
func publishProjectQuotaGauges(projects *project.Registry, usage *gateway.ProjectUsage, rec *metrics.Recorder, now time.Time) {
	if projects == nil || usage == nil || rec == nil {
		return
	}
	for _, p := range projects.All() {
		daily, hasDaily, monthly, hasMonthly := usage.Remaining(p, now)
		if hasDaily {
			rec.SetProjectQuotaLimit(p.Name, gateway.WindowDaily, p.DailyLimit)
			rec.SetProjectQuotaRemaining(p.Name, gateway.WindowDaily, daily)
		}
		if hasMonthly {
			rec.SetProjectQuotaLimit(p.Name, gateway.WindowMonthly, p.MonthlyLimit)
			rec.SetProjectQuotaRemaining(p.Name, gateway.WindowMonthly, monthly)
		}
	}
}

// routedProviderNames reports the pool names behind a built transport,
// or nil for an inline one. Startup-log detail only.
func routedProviderNames(t transport.Transport) []string {
	router, ok := t.(*transport.Router)
	if !ok {
		return nil
	}
	var names []string
	for _, p := range router.Providers() {
		names = append(names, p.Name)
	}
	return names
}

// buildLogger returns a slog.Logger configured per the config's Logging
// section. v1.0 supports JSON format only (NFR7); level defaults to info.
func buildLogger(cfg config.LoggingConfig) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler)
}
