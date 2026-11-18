package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// `posthorn probe` — the container healthcheck (FR154).
//
// The previous healthcheck re-validated the config file, which answers
// a question nobody was asking: the config was already valid when the
// process started, and a container whose HTTP listener has died still
// passes it. So the healthcheck reported healthy for exactly the
// failure it existed to catch.
//
// This probes the running process over HTTP instead. It ships as a
// subcommand because the production image is distroless — there is no
// curl, no wget and no shell in it, which is the point of distroless
// and not something to give up for a healthcheck.

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8080/readyz", "readiness URL to probe")
	timeout := fs.Duration("timeout", 3*time.Second, "probe timeout")
	quiet := fs.Bool("quiet", false, "print nothing; report only via the exit code")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client := &http.Client{
		Timeout: *timeout,
		// A readiness probe follows no redirects: a 3xx from this
		// endpoint means something is in front of it that should not be.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Get(*url)
	if err != nil {
		return fmt.Errorf("probe %s: %w", *url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))

	if resp.StatusCode != http.StatusOK {
		// Print the failing check so `docker inspect` shows WHICH
		// dependency is not ready, not just that something is not.
		if !*quiet {
			fmt.Fprintf(os.Stderr, "not ready (%d): %s\n", resp.StatusCode, summarise(body))
		}
		return fmt.Errorf("not ready: status %d", resp.StatusCode)
	}
	if !*quiet {
		fmt.Println("ready")
	}
	return nil
}

// summarise renders the readiness body compactly, falling back to the
// raw bytes if it is not the shape we expect.
func summarise(body []byte) string {
	var parsed struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Checks == nil {
		return string(body)
	}
	out := parsed.Status
	for name, detail := range parsed.Checks {
		if detail != "ok" && detail != "none required" && detail != "not configured" {
			out += fmt.Sprintf(" [%s: %s]", name, detail)
		}
	}
	return out
}
