package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	weft "github.com/openweft/weft"
	telem "github.com/openweft/weft/telemetry"
)

// loadStateFromDir reads the on-disk blob the CLI wrote so tests
// can assert against the same path the agent would read at
// startup. Mirrors the production path the CLI takes.
func loadStateFromDir(t *testing.T, dir string) telem.State {
	t.Helper()
	store := telem.NewBlobStore(weft.NewFileStorage(weft.PathInDir(dir, "telemetry")))
	st, err := store.LoadState(context.Background())
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return st
}

func TestEnable_FlipsFlagAndMintsIdentity(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	if err := runEnable(ioDiscard{}, &stderr, dir, "https://aggregator.example/telemetry"); err != nil {
		t.Fatalf("runEnable: %v", err)
	}
	st := loadStateFromDir(t, dir)
	if !st.Enabled {
		t.Errorf("Enabled = false after enable")
	}
	if st.Endpoint != "https://aggregator.example/telemetry" {
		t.Errorf("Endpoint = %q, want set", st.Endpoint)
	}
	if st.ClusterUUID == "" || len(st.ClusterUUID) != 32 {
		t.Errorf("ClusterUUID = %q, want 32-hex", st.ClusterUUID)
	}
	if st.InstallDate == "" {
		t.Errorf("InstallDate empty")
	}
	banner := stderr.String()
	for _, want := range []string{
		"ENABLED",
		"docs/operations/telemetry.md",
		"NEVER sent",
		"anonymous",
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner missing %q ; got:\n%s", want, banner)
		}
	}
}

func TestEnable_PreservesIdentityOnReEnable(t *testing.T) {
	dir := t.TempDir()
	if err := runEnable(ioDiscard{}, ioDiscard{}, dir, "https://x/"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	st1 := loadStateFromDir(t, dir)
	if err := runDisable(ioDiscard{}, dir); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := runEnable(ioDiscard{}, ioDiscard{}, dir, ""); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	st2 := loadStateFromDir(t, dir)
	if st1.ClusterUUID != st2.ClusterUUID || st1.InstallDate != st2.InstallDate {
		t.Errorf("identity drifted across disable+enable cycle: %q/%q vs %q/%q",
			st1.ClusterUUID, st1.InstallDate, st2.ClusterUUID, st2.InstallDate)
	}
	if !st2.Enabled {
		t.Errorf("Enabled = false after re-enable")
	}
	// Endpoint must persist when --endpoint not passed.
	if st2.Endpoint != st1.Endpoint {
		t.Errorf("Endpoint lost on re-enable : %q vs %q", st2.Endpoint, st1.Endpoint)
	}
}

func TestDisable_FlipsFlagOff(t *testing.T) {
	dir := t.TempDir()
	if err := runEnable(ioDiscard{}, ioDiscard{}, dir, "https://x/"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := runDisable(ioDiscard{}, dir); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got := loadStateFromDir(t, dir); got.Enabled {
		t.Errorf("Enabled = true after disable")
	}
}

func TestStatus_RendersExpectedFields(t *testing.T) {
	dir := t.TempDir()
	if err := runEnable(ioDiscard{}, ioDiscard{}, dir, "https://aggregator.example/telemetry"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	var out bytes.Buffer
	if err := runStatus(&out, dir); err != nil {
		t.Fatalf("status: %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"state       : enabled",
		"endpoint    : https://aggregator.example/telemetry",
		"anonymous_id: ",
		"last_sent_at: never",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("status missing %q ; got:\n%s", want, s)
		}
	}
}

func TestStatus_DefaultIsDisabled(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runStatus(&out, dir); err != nil {
		t.Fatalf("status: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "state       : disabled") {
		t.Errorf("default status not 'disabled' ; got:\n%s", s)
	}
	if !strings.Contains(s, "<unset") {
		t.Errorf("default endpoint not flagged 'unset' ; got:\n%s", s)
	}
}

func TestPreview_RequiresEnabled(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	err := runPreview(&out, dir, false)
	if err == nil {
		t.Fatalf("preview when disabled : want error, got nil")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("error = %v, want mention 'disabled'", err)
	}
}

func TestPreview_ForceSamplePrintsPayload(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runPreview(&out, dir, true); err != nil {
		t.Fatalf("preview --force-sample: %v", err)
	}
	var p telem.Payload
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatalf("preview output not JSON: %v\n%s", err, out.String())
	}
	if p.AnonymousID == "" {
		t.Errorf("payload.AnonymousID empty")
	}
	if p.Version != telem.AgentVersion {
		t.Errorf("Version = %q, want %q", p.Version, telem.AgentVersion)
	}
}

func TestPreview_EnabledPrintsRealIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := runEnable(ioDiscard{}, ioDiscard{}, dir, ""); err != nil {
		t.Fatalf("enable: %v", err)
	}
	var out1, out2 bytes.Buffer
	if err := runPreview(&out1, dir, false); err != nil {
		t.Fatalf("preview #1: %v", err)
	}
	if err := runPreview(&out2, dir, false); err != nil {
		t.Fatalf("preview #2: %v", err)
	}
	// Decode and compare anonymous_id only — uptime / now can
	// drift between the two calls, which is fine.
	var p1, p2 telem.Payload
	if err := json.Unmarshal(out1.Bytes(), &p1); err != nil {
		t.Fatalf("decode #1: %v", err)
	}
	if err := json.Unmarshal(out2.Bytes(), &p2); err != nil {
		t.Fatalf("decode #2: %v", err)
	}
	if p1.AnonymousID == "" || p1.AnonymousID != p2.AnonymousID {
		t.Errorf("AnonymousID not stable across preview calls : %q vs %q", p1.AnonymousID, p2.AnonymousID)
	}
}

func TestCommand_Structure(t *testing.T) {
	sock := ""
	c := Command(&sock, &sock, &sock)
	if c.Use != "telemetry" {
		t.Errorf("Use = %q, want telemetry", c.Use)
	}
	wantSubs := map[string]bool{
		"enable":  false,
		"disable": false,
		"status":  false,
		"preview": false,
	}
	for _, sub := range c.Commands() {
		wantSubs[sub.Use] = true
	}
	for name, ok := range wantSubs {
		if !ok {
			t.Errorf("subcommand %q missing", name)
		}
	}
}

// ioDiscard is the tiny io.Writer stub the tests use to mute the
// stderr banner when they're only asserting on persistence.
type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
