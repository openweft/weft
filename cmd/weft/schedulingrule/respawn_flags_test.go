package schedulingrule

import (
	"testing"
	"time"

	weftv1 "github.com/openweft/weft-proto"
	"github.com/spf13/cobra"
)

func parseRespawnArgs(t *testing.T, args []string, isUpdate bool) (*weftv1.RespawnPolicy, bool, error) {
	t.Helper()
	cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
	var rf respawnFlags
	rf.register(cmd, isUpdate)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("flag parse: %v", err)
	}
	return rf.build()
}

func TestRespawnFlags_NoFlagsLeavesPolicyNil(t *testing.T) {
	p, clear, err := parseRespawnArgs(t, nil, false)
	if err != nil || p != nil || clear {
		t.Errorf("got p=%v clear=%v err=%v ; want nil/false/nil", p, clear, err)
	}
}

func TestRespawnFlags_EnabledOnlyGivesMinimalPolicy(t *testing.T) {
	p, _, err := parseRespawnArgs(t, []string{"--respawn-enabled"}, false)
	if err != nil || p == nil || !p.Enabled {
		t.Errorf("got p=%v err=%v ; want enabled minimal policy", p, err)
	}
}

func TestRespawnFlags_GraceAndBackoff(t *testing.T) {
	p, _, err := parseRespawnArgs(t, []string{
		"--respawn-enabled",
		"--respawn-grace-period", "5s",
		"--respawn-max-restarts", "3",
		"--respawn-window", "10m",
		"--respawn-backoff", "exponential",
		"--respawn-initial-delay", "500ms",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.GracePeriodMs != 5000 {
		t.Errorf("grace=%d ; want 5000", p.GracePeriodMs)
	}
	if p.MaxRestarts != 3 || p.WindowMs != 600000 {
		t.Errorf("max=%d window=%d ; want 3/600000", p.MaxRestarts, p.WindowMs)
	}
	if p.Backoff != "exponential" || p.InitialDelayMs != 500 {
		t.Errorf("backoff=%s initial=%d ; want exponential/500", p.Backoff, p.InitialDelayMs)
	}
}

func TestRespawnFlags_LivenessHTTPProbe(t *testing.T) {
	p, _, err := parseRespawnArgs(t, []string{
		"--respawn-enabled",
		"--liveness-http-path", "/api/healthz",
		"--liveness-http-port", "8080",
		"--probe-period", "2s",
		"--probe-failure-threshold", "5",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	l := p.GetLiveness()
	if l == nil || l.Type != weftv1.HealthProbe_HTTP {
		t.Fatalf("liveness=%v ; want HTTP", l)
	}
	if l.HttpPath != "/api/healthz" || l.HttpPort != 8080 {
		t.Errorf("got path=%q port=%d", l.HttpPath, l.HttpPort)
	}
	if l.PeriodMs != 2000 || l.FailureThreshold != 5 {
		t.Errorf("got period=%d failures=%d", l.PeriodMs, l.FailureThreshold)
	}
}

func TestRespawnFlags_LivenessTCPProbe(t *testing.T) {
	p, _, err := parseRespawnArgs(t, []string{
		"--respawn-enabled",
		"--liveness-tcp-port", "5432",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	l := p.GetLiveness()
	if l == nil || l.Type != weftv1.HealthProbe_TCP || l.TcpPort != 5432 {
		t.Errorf("got %+v ; want TCP port 5432", l)
	}
}

func TestRespawnFlags_HTTPAndTCPMutuallyExclusive(t *testing.T) {
	_, _, err := parseRespawnArgs(t, []string{
		"--liveness-http-path", "/x",
		"--liveness-http-port", "80",
		"--liveness-tcp-port", "443",
	}, false)
	if err == nil {
		t.Error("HTTP+TCP should be rejected ; got nil err")
	}
}

func TestRespawnFlags_HTTPPathNeedsPort(t *testing.T) {
	_, _, err := parseRespawnArgs(t, []string{
		"--liveness-http-path", "/x",
	}, false)
	if err == nil {
		t.Error("HTTP path without port should error")
	}
}

func TestRespawnFlags_InvalidBackoffRejected(t *testing.T) {
	_, _, err := parseRespawnArgs(t, []string{
		"--respawn-enabled",
		"--respawn-backoff", "linear",
	}, false)
	if err == nil {
		t.Error("invalid backoff should be rejected")
	}
}

func TestRespawnFlags_NoRespawnSetsClear(t *testing.T) {
	p, clear, err := parseRespawnArgs(t, []string{"--no-respawn"}, true)
	if err != nil || p != nil || !clear {
		t.Errorf("got p=%v clear=%v err=%v ; want nil/true/nil", p, clear, err)
	}
}

func TestSummariseRespawn_RendersHumanLine(t *testing.T) {
	p := &weftv1.RespawnPolicy{
		Enabled: true, GracePeriodMs: int64(2 * time.Second / time.Millisecond),
		MaxRestarts: 3, WindowMs: int64(time.Minute / time.Millisecond),
		Backoff: "exponential",
		Liveness: &weftv1.HealthProbe{
			Type: weftv1.HealthProbe_HTTP, HttpPort: 8080, HttpPath: "/api/healthz",
		},
	}
	got := summariseRespawn(p)
	if got == "" {
		t.Fatal("got empty summary")
	}
	for _, want := range []string{"enabled", "grace=2s", "max=3/1m", "backoff=exponential", "liveness=http://"} {
		if !contains(got, want) {
			t.Errorf("summary %q missing %q", got, want)
		}
	}
}

func TestSummariseRespawn_NilGivesEmpty(t *testing.T) {
	if got := summariseRespawn(nil); got != "" {
		t.Errorf("nil → %q ; want \"\"", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
