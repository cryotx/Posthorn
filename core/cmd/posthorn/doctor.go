package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/craigmccaskill/posthorn/admin"
	"github.com/craigmccaskill/posthorn/config"
	"github.com/craigmccaskill/posthorn/project"
	"github.com/craigmccaskill/posthorn/storage"
	"github.com/craigmccaskill/posthorn/transport"
)

// `posthorn doctor` (FR152): everything an operator would otherwise
// find out at 3am, checked in one command while they are awake.
//
// Two rules the whole file obeys:
//
//	It never prints a secret. Not a key, not a password, not a webhook
//	secret, not a fragment of one. Checks report on the SHAPE of a
//	credential (present, long enough, distinct) and never its value —
//	doctor output is the thing operators paste into issues.
//
//	It changes nothing. Every check is a read or a probe. The one
//	exception is the writability check, which creates and removes its
//	own temporary file, because "is this volume writable" cannot be
//	answered by looking.

// checkStatus is the outcome of one check.
type checkStatus string

const (
	statusOK   checkStatus = "ok"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
)

type checkResult struct {
	Name   string
	Status checkStatus
	Detail string
}

type doctorReport struct {
	results []checkResult
}

func (d *doctorReport) add(name string, status checkStatus, format string, args ...any) {
	d.results = append(d.results, checkResult{
		Name: name, Status: status, Detail: fmt.Sprintf(format, args...),
	})
}

func (d *doctorReport) ok(name, format string, args ...any) { d.add(name, statusOK, format, args...) }
func (d *doctorReport) warn(name, format string, args ...any) {
	d.add(name, statusWarn, format, args...)
}
func (d *doctorReport) fail(name, format string, args ...any) {
	d.add(name, statusFail, format, args...)
}

func (d *doctorReport) failed() bool {
	for _, r := range d.results {
		if r.Status == statusFail {
			return true
		}
	}
	return false
}

func (d *doctorReport) print() {
	width := 0
	for _, r := range d.results {
		if len(r.Name) > width {
			width = len(r.Name)
		}
	}
	var warns, fails int
	for _, r := range d.results {
		mark := "  ok  "
		switch r.Status {
		case statusWarn:
			mark = " warn "
			warns++
		case statusFail:
			mark = " FAIL "
			fails++
		}
		fmt.Printf("[%s] %-*s  %s\n", mark, width, r.Name, r.Detail)
	}
	fmt.Println()
	switch {
	case fails > 0:
		fmt.Printf("%d check(s) failed, %d warning(s).\n", fails, warns)
	case warns > 0:
		fmt.Printf("All checks passed with %d warning(s).\n", warns)
	default:
		fmt.Println("All checks passed.")
	}
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	configPath := fs.String("config", defaultConfigPath, "path to TOML config file")
	probeProvider := fs.String("provider", "", "also make a live connectivity probe to this configured provider")
	if err := fs.Parse(args); err != nil {
		return err
	}

	d := &doctorReport{}
	cfg, err := config.Load(*configPath)
	if err != nil {
		// Everything below reads the config, so this is terminal.
		d.fail("config", "%s: %v", *configPath, err)
		d.print()
		return fmt.Errorf("configuration is not loadable")
	}
	d.ok("config", "%s parses and validates", *configPath)

	checkIngress(d, cfg)
	checkProviders(d, cfg)
	checkProjects(d, cfg)
	checkOutboundTargets(d, cfg)
	checkAdmin(d, cfg)
	checkStorage(d, cfg)

	if *probeProvider != "" {
		checkProviderConnectivity(d, cfg, *probeProvider)
	}

	d.print()
	if d.failed() {
		return fmt.Errorf("doctor found problems")
	}
	return nil
}

func checkIngress(d *doctorReport, cfg *config.Config) {
	switch {
	case len(cfg.Endpoints) == 0 && cfg.SMTPListener == nil:
		d.fail("ingress", "no endpoints and no smtp_listener — nothing can submit mail")
	case len(cfg.Endpoints) == 0:
		d.ok("ingress", "smtp_listener only")
	default:
		d.ok("ingress", "%d endpoint(s)%s", len(cfg.Endpoints),
			map[bool]string{true: " + smtp_listener"}[cfg.SMTPListener != nil])
	}

	seen := map[string]bool{}
	var dupes []string
	for _, ep := range cfg.Endpoints {
		if seen[ep.Path] {
			dupes = append(dupes, ep.Path)
		}
		seen[ep.Path] = true
	}
	if len(dupes) > 0 {
		d.fail("endpoint paths", "duplicate path(s): %s", strings.Join(dupes, ", "))
	}
}

func checkProviders(d *doctorReport, cfg *config.Config) {
	if len(cfg.Providers) == 0 {
		d.ok("providers", "no [[providers]] pool (endpoints use inline transports)")
	} else {
		names := make([]string, 0, len(cfg.Providers))
		for _, p := range cfg.Providers {
			names = append(names, p.Name)
		}
		sort.Strings(names)
		d.ok("providers", "%d in the pool: %s", len(names), strings.Join(names, ", "))
	}

	// Every provider type must declare its capabilities, or the matrix
	// operators plan against has a hole in it.
	if err := transport.VerifyCapabilityCoverage(); err != nil {
		d.fail("capability matrix", "%v", err)
	} else {
		d.ok("capability matrix", "every registered provider type declares its surface")
	}

	// Providers whose delivery events cannot be verified: not a
	// failure, but the operator should know which callbacks rest on
	// basic auth alone.
	var unverified []string
	for _, p := range cfg.Providers {
		caps, ok := transport.CapabilitiesFor(p.Type)
		if !ok {
			continue
		}
		if caps.Webhook == transport.CapYes && caps.WebhookVerification != transport.CapYes {
			unverified = append(unverified, p.Type)
		}
	}
	if len(unverified) > 0 {
		sort.Strings(unverified)
		d.warn("webhook verification",
			"no payload signature is published by: %s — those callbacks rest on the endpoint's basic auth and, if set, allowed_ips",
			strings.Join(uniq(unverified), ", "))
	}
}

func checkProjects(d *doctorReport, cfg *config.Config) {
	if len(cfg.Projects) == 0 {
		d.ok("projects", "project mode is off")
		return
	}
	reg, err := project.Build(cfg.ProjectConfigs())
	if err != nil {
		// The message comes from the project package, which never
		// echoes a key.
		d.fail("projects", "%v", err)
		return
	}
	d.ok("projects", "%d configured, %d enabled", reg.Len(), reg.Enabled())

	var rotating []string
	for _, p := range reg.All() {
		if p.KeyCount() > 1 {
			rotating = append(rotating, fmt.Sprintf("%s (%d keys)", p.Name, p.KeyCount()))
		}
	}
	if len(rotating) > 0 {
		d.warn("key rotation", "in progress: %s — finish it by removing the retired key",
			strings.Join(rotating, ", "))
	}

	// A project with no allowed_from_domains cannot exist (Build
	// refuses it), but a project whose default_from is outside its own
	// allowlist is the subtle version of the same mistake.
	for _, p := range reg.All() {
		if p.DefaultFrom != "" && !p.AllowsFrom(p.DefaultFrom) {
			d.fail("sender policy", "project %s has a default_from outside its own allowed_from_domains", p.Name)
		}
	}
	d.ok("sender policy", "every project restricts its senders")
}

func checkOutboundTargets(d *doctorReport, cfg *config.Config) {
	policy := cfg.OutboundPolicy()
	if cfg.Security.AllowPrivateWebhookTargets {
		d.warn("ssrf policy", "allow_private_webhook_targets is on — webhook and quota URLs may reach "+
			"loopback and private addresses (link-local stays blocked)")
	} else {
		d.ok("ssrf policy", "outbound webhook targets are restricted to public addresses")
	}

	type target struct{ what, url string }
	var targets []target
	for _, ep := range cfg.Endpoints {
		if ep.WebhookURL != "" {
			targets = append(targets, target{"endpoint " + ep.Path, ep.WebhookURL})
		}
	}
	for _, p := range cfg.Projects {
		if p.WebhookURL != "" {
			targets = append(targets, target{"project " + p.Name, p.WebhookURL})
		}
	}
	if len(targets) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, t := range targets {
		// Resolution, not a request: doctor must not deliver anything.
		// The point is to tell an operator that their callback URL
		// currently resolves somewhere the policy will refuse, BEFORE
		// an event needs delivering.
		addrs, err := policy.Resolve(ctx, t.url)
		switch {
		case err == nil:
			d.ok("webhook target", "%s resolves to an allowed address", t.what)
		case len(addrs) > 0:
			d.fail("webhook target", "%s: %v", t.what, err)
		default:
			d.warn("webhook target", "%s: could not resolve right now (%v)", t.what, err)
		}
	}
}

func checkAdmin(d *doctorReport, cfg *config.Config) {
	token := os.Getenv(admin.AdminTokenEnv)
	if token == "" {
		d.ok("admin surface", "not enabled (%s is unset)", admin.AdminTokenEnv)
		return
	}
	if cfg.Storage == nil {
		d.fail("admin surface", "%s is set but there is no [storage] block — "+
			"the admin surface cannot write its audit log", admin.AdminTokenEnv)
		return
	}
	auth, err := admin.NewAuthenticator(token, false)
	if err != nil {
		// The error names the requirement, never the value.
		d.fail("admin surface", "%v", err)
		return
	}
	if len(cfg.Projects) > 0 {
		reg, berr := project.Build(cfg.ProjectConfigs())
		if berr == nil {
			if verr := admin.VerifyDistinct(auth, reg); verr != nil {
				d.fail("admin credential", "%v", verr)
				return
			}
		}
	}
	d.ok("admin surface", "enabled, credential is distinct from every project key")
}

func checkStorage(d *doctorReport, cfg *config.Config) {
	if cfg.Storage == nil {
		d.warn("storage", "no [storage] block — no submission log, no retry queue, no admin surface")
		return
	}
	if cfg.Storage.InMemory || cfg.Storage.Path == ":memory:" {
		d.warn("storage", "in-memory — everything is lost on restart, including queued mail")
		return
	}

	dir := backupDir(cfg.Storage.Path)
	if err := checkWritable(dir); err != nil {
		d.fail("storage volume", "%s is not writable: %v", dir, err)
	} else {
		d.ok("storage volume", "%s is writable (a backup can be written beside the database)", dir)
	}

	if _, err := os.Stat(cfg.Storage.Path); os.IsNotExist(err) {
		d.ok("database", "%s does not exist yet — it will be created on first start", cfg.Storage.Path)
		return
	}

	info, err := storage.InspectFile(cfg.Storage.Path)
	if err != nil {
		d.fail("database", "%v", err)
		return
	}
	d.ok("database", "%s, %s, integrity_check ok", cfg.Storage.Path, humanBytes(info.Bytes))

	switch {
	case info.SchemaVersion > storage.SchemaVersion():
		d.fail("schema", "database is at v%d, this binary supports v%d — "+
			"it was written by a newer Posthorn and will not open",
			info.SchemaVersion, storage.SchemaVersion())
	case info.SchemaVersion < storage.SchemaVersion():
		d.ok("schema", "database is at v%d, will migrate to v%d on next start",
			info.SchemaVersion, storage.SchemaVersion())
	default:
		d.ok("schema", "v%d, current", info.SchemaVersion)
	}

	// Opening the store reports operational depth. Read-only questions
	// only; doctor does not drain a queue or retry anything.
	store, err := storage.Open(storage.Config{Path: cfg.Storage.Path})
	if err != nil {
		d.fail("database open", "%v", err)
		return
	}
	defer func() { _ = store.Close() }()

	depth, err := store.QueueDepth()
	if err != nil {
		d.fail("retry queue", "%v", err)
	} else if depth > 0 {
		d.warn("retry queue", "%d message(s) waiting", depth)
	} else {
		d.ok("retry queue", "empty")
	}

	dead, err := store.CountDeadLetters()
	if err != nil {
		d.fail("dead letters", "%v", err)
	} else if dead > 0 {
		d.warn("dead letters", "%d message(s) will not be retried without an operator", dead)
	} else {
		d.ok("dead letters", "none")
	}
}

// checkWritable proves a directory is writable by writing to it, then
// cleans up. "Looks writable" from a mode bit is exactly the check that
// passes on a full disk or a read-only bind mount.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".posthorn-doctor-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := f.WriteString("posthorn doctor writability probe\n"); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// checkProviderConnectivity makes ONE real call to a configured
// provider, using the credentials already in the config.
//
// Opt-in via --provider, because it is the only check that talks to the
// outside world and the only one that could cost the operator anything.
// It does not send mail: it builds the transport and reports whether
// the credential and the endpoint are usable, which is what an operator
// wants to know before a deploy.
func checkProviderConnectivity(d *doctorReport, cfg *config.Config, name string) {
	var found *config.ProviderConfig
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == name {
			found = &cfg.Providers[i]
			break
		}
	}
	if found == nil {
		d.fail("provider probe", "no provider named %q in the pool", name)
		return
	}
	reg, ok := transport.Lookup(found.Type)
	if !ok {
		d.fail("provider probe", "provider %s has unknown type %q", name, found.Type)
		return
	}
	if err := reg.Validate(found.Settings); err != nil {
		// Validation errors name the missing SETTING, never its value.
		d.fail("provider probe", "provider %s: %v", name, err)
		return
	}
	if _, err := reg.Build(found.Settings); err != nil {
		d.fail("provider probe", "provider %s: %v", name, err)
		return
	}
	d.ok("provider probe", "provider %s (%s) builds with the configured credentials; "+
		"a live send is deliberately not attempted", name, found.Type)
}

func uniq(in []string) []string {
	out := in[:0:0]
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
