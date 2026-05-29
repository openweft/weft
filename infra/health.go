package infra

// health.go implements the readiness probe the deployer uses to
// wait for an infra service to come up after StartVM. Plans
// declare their probe in the HCL `health { ... }` block ; the
// deployer turns it into a wait-loop with a configurable
// timeout.
//
// Two probe types are eventually supported :
//
//   type = "http"   GET the URL ; 2xx response = healthy. The
//                   URL is host-side : the plan author writes
//                   `http://$VM_IP:port/path` and `$VM_IP` is
//                   substituted with the guest's network IP at
//                   poll time. (127.0.0.1 from the host = the
//                   host, not the guest — that's why a guest-
//                   side URL needs the substitution.)
//
//   type = "exec"   not implemented yet ; needs ExecInVM
//                   plumbing. Plans that declare it surface a
//                   "not supported" error at deploy time so
//                   the operator notices instead of the
//                   bootstrap silently skipping the probe.
//
// Token convention parallels the config-file renderer in
// configfile.go : a single regex with word boundaries, only
// the supported tokens substituted, everything else passes
// through unchanged.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// vmIPToken matches `$VM_IP` with a trailing word boundary so
// it doesn't bleed into `$VM_IP_v4` or similar.
var vmIPToken = regexp.MustCompile(`\$VM_IP\b`)

// HealthURL renders the plan's health.cmd for HTTP probes,
// substituting `$VM_IP` with the supplied guest IP. Returns an
// error when the plan declares a non-HTTP probe (today only
// http is implemented) or when the URL is empty.
//
// `vmIP` is the host-side network address of the VM — what the
// deployer learned via `Adapter.IP(vmName)` after the VM
// finished booting. Empty `vmIP` is allowed (the substitution
// leaves the URL with a blank host, which surfaces a connection
// error during polling — useful for tests that don't fan out
// to a live VM).
func HealthURL(p *Plan, vmIP string) (string, error) {
	if p == nil || p.Health == nil {
		return "", fmt.Errorf("plan has no health block")
	}
	if p.Health.Type != "http" {
		return "", fmt.Errorf("health.type = %q is not supported (only http today)", p.Health.Type)
	}
	if p.Health.Cmd == "" {
		return "", fmt.Errorf("health.cmd is empty")
	}
	return vmIPToken.ReplaceAllString(p.Health.Cmd, vmIP), nil
}

// HealthPeriod returns the parsed poll interval from
// `health.period` (default 5s). Negative / unparseable values
// fall back to the default so the deployer never blocks on a
// nonsensical period.
func HealthPeriod(p *Plan) time.Duration {
	const dflt = 5 * time.Second
	if p == nil || p.Health == nil || p.Health.Period == "" {
		return dflt
	}
	d, err := time.ParseDuration(p.Health.Period)
	if err != nil || d <= 0 {
		return dflt
	}
	return d
}

// WaitHealthy polls `url` with HTTP GETs until a 2xx response
// arrives, the context cancels, or the deadline passes. Each
// poll has a 5s I/O timeout — long enough for a service that's
// still warming up, short enough that a misconfigured URL
// doesn't stall the loop.
//
// The first successful response wins ; non-2xx responses,
// connection-refused, and DNS errors all roll into the retry
// loop. Final error reports the last underlying issue so the
// operator sees *why* the probe never went green.
func WaitHealthy(ctx context.Context, url string, timeout, period time.Duration) error {
	if url == "" {
		return errors.New("WaitHealthy: empty URL")
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if period <= 0 {
		period = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 5 * time.Second}

	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("WaitHealthy: context cancelled (last err: %v)", lastErr)
		}
		ok, err := probeOnce(ctx, client, url)
		if ok {
			return nil
		}
		if err != nil {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("no 2xx response")
			}
			return fmt.Errorf("WaitHealthy(%s): %s elapsed without success (last: %w)", url, timeout, lastErr)
		}
		// Sleep until next probe or context-cancel.
		select {
		case <-ctx.Done():
			return fmt.Errorf("WaitHealthy: context cancelled (last err: %v)", lastErr)
		case <-time.After(period):
		}
	}
}

// probeOnce returns (true, nil) on a 2xx, (false, err) on any
// failure. The body is drained + discarded — we only care
// about the status. Connection / DNS errors come through `err` ;
// non-2xx statuses come through `err = fmt.Errorf("status %d")`.
func probeOnce(ctx context.Context, client *http.Client, url string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, nil
	}
	return false, fmt.Errorf("status %d %s", resp.StatusCode, strings.TrimSpace(resp.Status))
}
