package schedulingrule

import (
	"fmt"
	"strings"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

// respawnFlags is the shared flag set both `create` and `update`
// register for the SchedulingRule.RespawnPolicy block. Kept here (one
// file, one source of truth) so the two subcommands can't drift apart.
//
// Flag names follow the proto field names and the HCL block keys ;
// keeping them aligned means a future `weft scheduling-rule create
// --from cfg.hcl` can map either source onto the same internal struct.
type respawnFlags struct {
	enabled bool
	clear   bool // --no-respawn ; used by `update` only

	grace       time.Duration
	maxRestarts int32
	window      time.Duration
	backoff     string // "" | "constant" | "exponential"
	initial     time.Duration

	livenessHTTPPath string
	livenessHTTPPort int32
	livenessTCPPort  int32
	probePeriod      time.Duration
	probeTimeout     time.Duration
	probeFailures    int32
	probeSuccesses   int32
	probeInitDelay   time.Duration
}

// register adds the respawn flag set to cmd. isUpdate controls whether
// `--no-respawn` is exposed (only useful on update — create starts
// without a policy, so omitting `--respawn-enabled` is equivalent).
func (r *respawnFlags) register(cmd *cobra.Command, isUpdate bool) {
	cmd.Flags().BoolVar(&r.enabled, "respawn-enabled", false, "Enable the respawn policy (deaths trigger restart)")
	if isUpdate {
		cmd.Flags().BoolVar(&r.clear, "no-respawn", false, "Clear the respawn policy entirely")
	}
	cmd.Flags().DurationVar(&r.grace, "respawn-grace-period", 0, "Wait this long after a death signal before respawning (anti-flap)")
	cmd.Flags().Int32Var(&r.maxRestarts, "respawn-max-restarts", 0, "Cap on respawns within --respawn-window (anti-thrash)")
	cmd.Flags().DurationVar(&r.window, "respawn-window", 0, "Sliding window for --respawn-max-restarts")
	cmd.Flags().StringVar(&r.backoff, "respawn-backoff", "", "Backoff between retries : \"constant\" | \"exponential\"")
	cmd.Flags().DurationVar(&r.initial, "respawn-initial-delay", 0, "Initial backoff before the first retry ; doubles each retry under exponential")

	cmd.Flags().StringVar(&r.livenessHTTPPath, "liveness-http-path", "", "HTTP path the liveness probe checks ; non-empty enables HTTP probing")
	cmd.Flags().Int32Var(&r.livenessHTTPPort, "liveness-http-port", 0, "HTTP port the liveness probe checks ; required with --liveness-http-path")
	cmd.Flags().Int32Var(&r.livenessTCPPort, "liveness-tcp-port", 0, "TCP port the liveness probe checks ; non-zero enables TCP probing")
	cmd.Flags().DurationVar(&r.probePeriod, "probe-period", 0, "Interval between liveness probe attempts (default 1s)")
	cmd.Flags().DurationVar(&r.probeTimeout, "probe-timeout", 0, "Per-attempt deadline for liveness probes (default 1s)")
	cmd.Flags().Int32Var(&r.probeFailures, "probe-failure-threshold", 0, "Consecutive failures before declaring unhealthy (default 3)")
	cmd.Flags().Int32Var(&r.probeSuccesses, "probe-success-threshold", 0, "Consecutive successes before declaring healthy (default 1)")
	cmd.Flags().DurationVar(&r.probeInitDelay, "probe-initial-delay", 0, "Wait this long after VM boot before the first probe attempt")
}

// build assembles the proto RespawnPolicy from the parsed flags.
// Returns (nil, nil) when no respawn flag was set at all — equivalent
// to "no policy" so the registry stays unmodified by the request.
// clearRespawn is true iff --no-respawn was passed on `update`.
func (r *respawnFlags) build() (*weftv1.RespawnPolicy, bool, error) {
	if r.clear {
		return nil, true, nil
	}
	// "Nothing was set" detection : every numeric is zero, every string
	// empty, enabled flag is the cobra default. Avoids creating an
	// empty-but-non-nil policy that'd erase upstream state.
	touched := r.enabled ||
		r.grace != 0 ||
		r.maxRestarts != 0 ||
		r.window != 0 ||
		r.backoff != "" ||
		r.initial != 0 ||
		r.livenessHTTPPath != "" ||
		r.livenessHTTPPort != 0 ||
		r.livenessTCPPort != 0 ||
		r.probePeriod != 0 ||
		r.probeTimeout != 0 ||
		r.probeFailures != 0 ||
		r.probeSuccesses != 0 ||
		r.probeInitDelay != 0
	if !touched {
		return nil, false, nil
	}

	if r.backoff != "" && r.backoff != "constant" && r.backoff != "exponential" {
		return nil, false, fmt.Errorf("--respawn-backoff must be \"constant\" or \"exponential\" (got %q)", r.backoff)
	}

	policy := &weftv1.RespawnPolicy{
		Enabled:        r.enabled,
		GracePeriodMs:  r.grace.Milliseconds(),
		MaxRestarts:    r.maxRestarts,
		WindowMs:       r.window.Milliseconds(),
		Backoff:        r.backoff,
		InitialDelayMs: r.initial.Milliseconds(),
	}

	probe, err := r.buildLivenessProbe()
	if err != nil {
		return nil, false, err
	}
	if probe != nil {
		policy.Liveness = probe
	}

	return policy, false, nil
}

func (r *respawnFlags) buildLivenessProbe() (*weftv1.HealthProbe, error) {
	hasHTTP := r.livenessHTTPPath != "" || r.livenessHTTPPort != 0
	hasTCP := r.livenessTCPPort != 0
	if hasHTTP && hasTCP {
		return nil, fmt.Errorf("specify either --liveness-http-* OR --liveness-tcp-port, not both")
	}
	probeTouched := hasHTTP || hasTCP ||
		r.probePeriod != 0 ||
		r.probeTimeout != 0 ||
		r.probeFailures != 0 ||
		r.probeSuccesses != 0 ||
		r.probeInitDelay != 0
	if !probeTouched {
		return nil, nil
	}
	p := &weftv1.HealthProbe{
		InitialDelayMs:   r.probeInitDelay.Milliseconds(),
		PeriodMs:         r.probePeriod.Milliseconds(),
		TimeoutMs:        r.probeTimeout.Milliseconds(),
		FailureThreshold: r.probeFailures,
		SuccessThreshold: r.probeSuccesses,
	}
	switch {
	case hasHTTP:
		if r.livenessHTTPPort == 0 {
			return nil, fmt.Errorf("--liveness-http-port is required when --liveness-http-path is set")
		}
		p.Type = weftv1.HealthProbe_HTTP
		p.HttpPath = r.livenessHTTPPath
		p.HttpPort = r.livenessHTTPPort
	case hasTCP:
		p.Type = weftv1.HealthProbe_TCP
		p.TcpPort = r.livenessTCPPort
	default:
		return nil, fmt.Errorf("liveness probe flags set but neither --liveness-http-* nor --liveness-tcp-port chose a kind")
	}
	return p, nil
}

// summarise renders a one-line human description of the policy for
// CLI feedback ("created … respawn=enabled grace=5s ..."). Empty
// string when policy is nil.
func summariseRespawn(p *weftv1.RespawnPolicy) string {
	if p == nil {
		return ""
	}
	var parts []string
	if p.Enabled {
		parts = append(parts, "enabled")
	} else {
		parts = append(parts, "disabled")
	}
	if p.GracePeriodMs > 0 {
		parts = append(parts, fmt.Sprintf("grace=%s", time.Duration(p.GracePeriodMs)*time.Millisecond))
	}
	if p.MaxRestarts > 0 {
		parts = append(parts, fmt.Sprintf("max=%d/%s", p.MaxRestarts, time.Duration(p.WindowMs)*time.Millisecond))
	}
	if p.Backoff != "" {
		parts = append(parts, fmt.Sprintf("backoff=%s", p.Backoff))
	}
	if p.GetLiveness() != nil {
		l := p.GetLiveness()
		switch l.Type {
		case weftv1.HealthProbe_HTTP:
			parts = append(parts, fmt.Sprintf("liveness=http://%s:%d", strings.TrimPrefix(l.HttpPath, "/"), l.HttpPort))
		case weftv1.HealthProbe_TCP:
			parts = append(parts, fmt.Sprintf("liveness=tcp/%d", l.TcpPort))
		}
	}
	return "respawn=" + strings.Join(parts, " ")
}
