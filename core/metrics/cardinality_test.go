package metrics

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// The v1.0 metrics cardinality audit (NFR24, FR153).
//
// Prometheus cardinality is a memory bill that arrives later: one label
// carrying a message id turns a single time series into one per
// message, and the series never go away. The values that would do it
// are exactly the ones flowing through Posthorn — message ids,
// recipient addresses, subjects, provider error strings, idempotency
// keys.
//
// The defence is structural rather than a rule anyone has to remember:
// the Recorder's methods take only operator-configured names and closed
// enums, so a request-scoped value has no parameter to travel through.
// This file audits that the structure still holds, by calling EVERY
// exported Recorder method with a sentinel and inspecting the label
// space that comes out.

// forbiddenLabels are label names that would each be unbounded. Any of
// these appearing is a cardinality bug regardless of what it holds.
var forbiddenLabels = []string{
	"message_id", "submission_id", "recipient", "to", "from", "email",
	"address", "subject", "body", "error", "error_message", "detail",
	"idempotency_key", "key", "ip", "client_ip", "user_agent", "url",
}

// allowedLabels is the complete label vocabulary v1.0 emits. A new
// label name is a deliberate decision, so this list failing is the
// signal to make it — not to widen the list reflexively.
var allowedLabels = map[string]bool{
	"endpoint":    true, // operator-configured path
	"transport":   true, // registered transport type
	"provider":    true, // operator-chosen pool name
	"router":      true, // operator-configured route name
	"project":     true, // operator-chosen project name
	"window":      true, // "day" | "month"
	"status":      true, // closed enum
	"outcome":     true, // closed enum
	"result":      true, // closed enum
	"reason":      true, // closed enum
	"kind":        true, // closed enum
	"check":       true, // named spam check
	"error_class": true, // ErrorClass enum
	"state":       true, // circuit state enum
	"event":       true, // unified delivery event type
	"type":        true, // unified event type
	"le":          true, // histogram bucket bound, from a fixed bucket list
}

var labelPairRE = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="([^"]*)"`)

// exerciseEveryRecorderMethod calls every exported method on Recorder
// with sentinel arguments, so the audit covers the whole surface rather
// than the handful someone remembered.
func exerciseEveryRecorderMethod(t *testing.T, sentinel string) string {
	t.Helper()
	reg := New()
	r := NewRecorder(reg)

	rv := reflect.ValueOf(r)
	rt := rv.Type()
	called := 0
	for i := 0; i < rt.NumMethod(); i++ {
		m := rt.Method(i)
		mt := m.Type
		args := make([]reflect.Value, 0, mt.NumIn()-1)
		usable := true
		for j := 1; j < mt.NumIn(); j++ {
			switch mt.In(j) {
			case reflect.TypeOf(""):
				args = append(args, reflect.ValueOf(sentinel))
			case reflect.TypeOf(int(0)):
				args = append(args, reflect.ValueOf(1))
			case reflect.TypeOf(int64(0)):
				args = append(args, reflect.ValueOf(int64(1)))
			case reflect.TypeOf(float64(0)):
				args = append(args, reflect.ValueOf(float64(1)))
			case reflect.TypeOf(true):
				args = append(args, reflect.ValueOf(true))
			case reflect.TypeOf(time.Duration(0)):
				args = append(args, reflect.ValueOf(time.Second))
			default:
				usable = false
			}
		}
		if !usable {
			t.Logf("skipping %s: takes a type this audit does not synthesise (%v)", m.Name, mt)
			continue
		}
		rv.Method(i).Call(args)
		called++
	}
	if called < 30 {
		t.Fatalf("only exercised %d recorder methods; the audit is not covering the surface", called)
	}
	return emit(t, reg)
}

// TestCardinality_LabelNamesAreBounded is the audit proper: whatever
// values flow in, the label NAME space is the fixed vocabulary above.
func TestCardinality_LabelNamesAreBounded(t *testing.T) {
	out := exerciseEveryRecorderMethod(t, "SENTINEL")

	seen := map[string]bool{}
	for _, match := range labelPairRE.FindAllStringSubmatch(out, -1) {
		seen[match[1]] = true
	}
	if len(seen) == 0 {
		t.Fatal("no labels emitted; the audit is not reading the exposition")
	}

	var unexpected []string
	for name := range seen {
		if !allowedLabels[name] {
			unexpected = append(unexpected, name)
		}
		for _, forbidden := range forbiddenLabels {
			if name == forbidden {
				t.Errorf("label %q is unbounded by construction and must never be a metric label", name)
			}
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Errorf("label name(s) %v are not in the audited vocabulary — "+
			"adding one is a deliberate cardinality decision, not a reflex",
			unexpected)
	}
}

// TestCardinality_NoMethodAcceptsRequestContent: the Recorder API is
// the boundary. A method that took a message id or a recipient would
// let a caller do the wrong thing correctly, so the parameter names are
// audited rather than trusted.
func TestCardinality_NoMethodAcceptsRequestContent(t *testing.T) {
	// Method PARAMETER names are not available through reflection, so
	// this audits the observable consequence instead: a sentinel passed
	// to every string parameter must appear only in label positions
	// whose names are in the bounded vocabulary above.
	out := exerciseEveryRecorderMethod(t, "SENTINEL-VALUE")
	for _, match := range labelPairRE.FindAllStringSubmatch(out, -1) {
		name, value := match[1], match[2]
		if value != "SENTINEL-VALUE" {
			continue
		}
		if !allowedLabels[name] {
			t.Errorf("a caller-supplied value reached label %q, which is not in the audited vocabulary", name)
		}
	}
}

// A metric NAME must not carry the value either: posthorn_sent_total is
// bounded, posthorn_sent_msg_abc123_total is not.
func TestCardinality_MetricNamesCarryNoValues(t *testing.T) {
	out := exerciseEveryRecorderMethod(t, "SENTINEL")
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		if strings.Contains(name, "SENTINEL") {
			t.Errorf("a caller-supplied value reached a metric NAME: %q", name)
		}
		if !strings.HasPrefix(name, "posthorn_") {
			t.Errorf("metric %q is outside the posthorn_ namespace", name)
		}
	}
}

// Every series must be a value a human can alert on: an address in the
// exposition means somebody's recipient list is now in Prometheus.
func TestCardinality_NoAddressShapedValues(t *testing.T) {
	reg := New()
	r := NewRecorder(reg)
	// The realistic shapes: endpoints, providers, projects, enums.
	r.Submitted("/api/contact")
	r.Sent("/api/contact", "postmark", time.Second)
	r.Failed("/api/contact", "postmark", "transient")
	r.ProviderAttempt("postmark-prod", "sent", time.Second)
	r.ProjectSend("acme", "sent")
	r.ProjectDelivery("acme", "message.delivered")
	r.ProviderWebhook("resend", "verified")

	out := emit(t, reg)
	if strings.Contains(out, "@") {
		t.Errorf("an address-shaped value is in the exposition:\n%s", out)
	}
}
