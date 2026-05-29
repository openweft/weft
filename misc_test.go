//go:build darwin

package weft

// misc_test.go covers small utility paths missing coverage in the
// per-package test files: ref.go sanitize helpers, storage.Path,
// eventbus Close, dispatch NetworkOn/VolumeOn/ImageOn, acl
// RequireAdmin, scheduler ScheduleVMGroup, and timings round-trip.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	drivers "github.com/openweft/weft-drivers"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── ref.go ──────────────────────────────────────────────────────

func TestSanitizeRef_RoundTrip(t *testing.T) {
	in := "ghcr.io/foo/bar:v1.2"
	san := sanitizeRef(in)
	if san == "" {
		t.Errorf("sanitizeRef returned empty for %q", in)
	}
	got := unsanitizeRef(san)
	if got != in {
		t.Errorf("round-trip: %q → %q → %q", in, san, got)
	}
}

// ── storage.go Path & FileStorage error paths ───────────────────

func TestFileStorage_Path(t *testing.T) {
	s := NewFileStorage("/tmp/abc.hcl")
	if s.Path() != "/tmp/abc.hcl" {
		t.Errorf("Path() = %q", s.Path())
	}
}

func TestFileStorage_LoadMissingFile(t *testing.T) {
	tmp := t.TempDir()
	s := NewFileStorage(filepath.Join(tmp, "does-not-exist.hcl"))
	b, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if b != nil {
		t.Errorf("missing file should yield nil blob, got %q", b)
	}
}

func TestFileStorage_LoadSaveCycle(t *testing.T) {
	tmp := t.TempDir()
	s := NewFileStorage(filepath.Join(tmp, "blob.hcl"))
	want := []byte("hello world")
	if err := s.Save(context.Background(), want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFileStorage_CancelledContext(t *testing.T) {
	tmp := t.TempDir()
	s := NewFileStorage(filepath.Join(tmp, "x.hcl"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Load(ctx); err == nil {
		t.Errorf("Load with cancelled ctx should error")
	}
	if err := s.Save(ctx, []byte("x")); err == nil {
		t.Errorf("Save with cancelled ctx should error")
	}
}

func TestFileStorage_SaveErrorsBadDir(t *testing.T) {
	// Parent doesn't exist → WriteFile fails.
	s := NewFileStorage("/this/parent/does/not/exist/blob.hcl")
	if err := s.Save(context.Background(), []byte("x")); err == nil {
		t.Errorf("save to bad path should error")
	}
}

func TestFileStorage_SaveRenameError(t *testing.T) {
	tmp := t.TempDir()
	// Target path is an existing NON-EMPTY directory → rename of the
	// tmp file over it fails (the os.Rename commit branch).
	target := filepath.Join(tmp, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewFileStorage(target)
	if err := s.Save(context.Background(), []byte("blob")); err == nil {
		t.Errorf("rename over a non-empty dir should error")
	}
}

func TestFileStorage_LoadReadError(t *testing.T) {
	tmp := t.TempDir()
	// Point at a directory → os.ReadFile fails with a non-NotExist
	// error, which Load propagates.
	dir := filepath.Join(tmp, "adir")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := NewFileStorage(dir)
	if _, err := s.Load(context.Background()); err == nil {
		t.Errorf("reading a directory as a file should error")
	}
}

func TestMemStorage_CancelledContext(t *testing.T) {
	s := NewMemStorage()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Load(ctx); err == nil {
		t.Errorf("MemStorage.Load with cancelled ctx should error")
	}
	if err := s.Save(ctx, []byte("x")); err == nil {
		t.Errorf("MemStorage.Save with cancelled ctx should error")
	}
}

func TestNewMemStorageWith_Seed(t *testing.T) {
	seed := []byte("seeded")
	s := NewMemStorageWith(seed)
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "seeded" {
		t.Errorf("seed not loaded: %q", got)
	}
}

func TestNewFileStorageInDir_PathShape(t *testing.T) {
	dir := "/tmp/vms"
	s := NewFileStorageInDir(dir, "projects")
	want := filepath.Join(dir, ".projects.hcl")
	if s.Path() != want {
		t.Errorf("Path() = %q, want %q", s.Path(), want)
	}
}

// ── eventbus.go Close ───────────────────────────────────────────

func TestLocalEventBus_Close(t *testing.T) {
	b := NewLocalEventBus()
	if err := b.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Publish after Close is a no-op.
	b.Publish(PlatformEvent{Kind: "x"})
	// Close on nil receiver is safe.
	var nilBus *LocalEventBus
	if err := nilBus.Close(); err != nil {
		t.Errorf("nil Close should be nil, got %v", err)
	}
}

// ── dispatch.go NetworkOn/VolumeOn/ImageOn ──────────────────────

func TestAdapter_NetworkVolumeImageOn(t *testing.T) {
	a := newAdapterForRegistries(t)
	hostUUID := a.localHostUUID()

	// Local handle should resolve all four driver types.
	if _, err := a.NetworkOn(hostUUID); err != nil {
		t.Errorf("NetworkOn: %v", err)
	}
	if _, err := a.VolumeOn(hostUUID); err != nil {
		t.Errorf("VolumeOn: %v", err)
	}
	if _, err := a.ImageOn(hostUUID); err != nil {
		t.Errorf("ImageOn: %v", err)
	}

	// Unknown host: each errors.
	if _, err := a.NetworkOn("nope"); err == nil {
		t.Errorf("NetworkOn unknown should error")
	}
	if _, err := a.VolumeOn("nope"); err == nil {
		t.Errorf("VolumeOn unknown should error")
	}
	if _, err := a.ImageOn("nope"); err == nil {
		t.Errorf("ImageOn unknown should error")
	}
}

// localHypervisor's error path: a bare adapter has no host UUID
// file so the helper fails with a clear message.
func TestLocalHypervisor_NoUUID(t *testing.T) {
	a := &Adapter{stateDir: "/var/empty/does-not-exist", bus: NewEventBus()}
	if _, err := a.localHypervisor(); err == nil {
		t.Errorf("bare adapter should fail localHypervisor")
	}
}

// ── acl.go RequireAdmin ────────────────────────────────────────

func TestRequireAdmin_NoCaller(t *testing.T) {
	ctx := context.Background()
	err := RequireAdmin(ctx, "delete-project")
	if err == nil {
		t.Fatal("no caller should error")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.Unauthenticated {
		t.Errorf("want Unauthenticated, got %v", err)
	}
}

func TestRequireAdmin_DevAllowed(t *testing.T) {
	ctx := WithCaller(context.Background(), &Caller{Dev: true})
	if err := RequireAdmin(ctx, "op"); err != nil {
		t.Errorf("dev should be allowed: %v", err)
	}
}

func TestRequireAdmin_GroupAllowed(t *testing.T) {
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:admin",
		Issuer:  "https://dex",
		Groups:  []string{PlatformAdminGroup},
	})
	if err := RequireAdmin(ctx, "op"); err != nil {
		t.Errorf("admin group should be allowed: %v", err)
	}
}

func TestRequireAdmin_Denied(t *testing.T) {
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:joe",
		Issuer:  "https://dex",
	})
	err := RequireAdmin(ctx, "delete-project")
	if err == nil {
		t.Fatal("non-admin should be denied")
	}
	if st, ok := status.FromError(err); !ok || st.Code() != codes.PermissionDenied {
		t.Errorf("want PermissionDenied, got %v", err)
	}
	if !strings.Contains(err.Error(), "delete-project") {
		t.Errorf("error should mention op: %v", err)
	}
}

// AuthorizeProject paths missing coverage: nil context, UUID-shaped
// input from a regular caller (must not auto-create / hide
// existence).
func TestAuthorizeProject_NoCaller(t *testing.T) {
	a := newAdapterForRegistries(t)
	if _, err := a.AuthorizeProject(context.Background(), ""); err == nil {
		t.Fatal("no caller should error")
	}
}

// AuthorizeProject UUID path: caller asks for an existing project
// they own → returns the UUID; for one they don't own → error.
func TestAuthorizeProject_UUIDOwnedAndDenied(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Register a project the caller doesn't own.
	p, _, _ := a.CreateProject("private-team")
	denied := WithCaller(context.Background(), &Caller{
		Subject: "ldap:outsider",
		Issuer:  "https://dex",
	})
	if _, err := a.AuthorizeProject(denied, p.UUID); err == nil {
		t.Fatal("outsider should be denied")
	}

	// Now grant via group claim.
	allowed := WithCaller(context.Background(), &Caller{
		Subject: "ldap:owner",
		Issuer:  "https://dex",
		Groups:  []string{ProjectGroup(p.UUID)},
	})
	got, err := a.AuthorizeProject(allowed, p.UUID)
	if err != nil {
		t.Fatalf("group-claim owner should be allowed: %v", err)
	}
	if got != p.UUID {
		t.Errorf("got %q, want %q", got, p.UUID)
	}
}

// AuthorizeProject: unknown UUID → permission denied (not NotFound).
func TestAuthorizeProject_UnknownUUID(t *testing.T) {
	a := newAdapterForRegistries(t)
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:user",
		Issuer:  "https://dex",
	})
	if _, err := a.AuthorizeProject(ctx, "f47ac10b-58cc-4372-a567-0e02b2c3d479"); err == nil {
		t.Fatal("unknown UUID should be denied")
	}
}

// AuthorizeProject: caller asks for display name == subject →
// auto-creates own default project.
func TestAuthorizeProject_OwnSubjectName(t *testing.T) {
	a := newAdapterForRegistries(t)
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:carol",
		Issuer:  "https://dex",
	})
	got, err := a.AuthorizeProject(ctx, "ldap:carol")
	if err != nil {
		t.Fatalf("caller asking for own subject: %v", err)
	}
	if got == "" {
		t.Errorf("expected an auto-created project UUID")
	}
}

// ── scheduler.go ScheduleVMGroup ────────────────────────────────

func TestAdapter_ScheduleVMGroup_DefaultsFirstFit(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Add a second host so the group has room to spread (the
	// FirstFitScheduler's default behaviour will pick something).
	if _, err := a.RegisterHost(RegisterHostSpec{Hostname: "h2"}); err != nil {
		t.Fatal(err)
	}

	req := GroupScheduleRequest{
		ScheduleRequest: ScheduleRequest{
			Hypervisor: "apple-vz",
		},
		Replicas: 1,
	}
	hosts, err := a.ScheduleVMGroup(context.Background(), req)
	if err != nil {
		t.Fatalf("ScheduleVMGroup: %v", err)
	}
	if len(hosts) != 1 {
		t.Errorf("want 1 host, got %d", len(hosts))
	}
}

func TestAdapter_ScheduleVMGroup_DefaultsSchedulerWhenNil(t *testing.T) {
	// Setting scheduler to nil triggers the lazy fallback.
	a := newAdapterForRegistries(t)
	a.scheduler = nil
	req := GroupScheduleRequest{
		ScheduleRequest: ScheduleRequest{Hypervisor: "apple-vz"},
		Replicas:        1,
	}
	if _, err := a.ScheduleVMGroup(context.Background(), req); err != nil {
		t.Errorf("with nil scheduler should default + work: %v", err)
	}
}

// ── timings.go RecordEvent + ReadTimings ────────────────────────

func TestRecordEvent_ReadTimings_RoundTrip(t *testing.T) {
	vmDir := t.TempDir()

	// Empty dir → nil events, nil error.
	got, err := ReadTimings(vmDir)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if got != nil {
		t.Errorf("empty should be nil, got %v", got)
	}

	RecordEvent(vmDir, "stage1", map[string]string{"k": "v"})
	RecordEvent(vmDir, "stage2", nil)

	got, err = ReadTimings(vmDir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(got), got)
	}
	if got[0].Name != "stage1" || got[1].Name != "stage2" {
		t.Errorf("names: %+v", got)
	}
	if got[0].Meta["k"] != "v" {
		t.Errorf("meta: %+v", got[0].Meta)
	}
	if got[0].TsUnixNano == 0 {
		t.Errorf("TsUnixNano should be set")
	}
}

func TestRecordEvent_EmptyArgs_AreNoOp(t *testing.T) {
	tmp := t.TempDir()
	RecordEvent("", "stage", nil) // empty vmDir → no file.
	RecordEvent(tmp, "", nil)     // empty name → no file.
	if _, err := os.Stat(filepath.Join(tmp, "timings.jsonl")); !os.IsNotExist(err) {
		t.Errorf("file should not be created")
	}
}

func TestReadTimings_NonExistent(t *testing.T) {
	got, err := ReadTimings("/var/empty/does-not-exist")
	if err != nil {
		t.Errorf("missing file should not error: %v", err)
	}
	if got != nil {
		t.Errorf("missing should be nil events")
	}
}

func TestReadTimings_SkipsCorrupt(t *testing.T) {
	vmDir := t.TempDir()
	// Write one valid line + one corrupt line.
	path := filepath.Join(vmDir, "timings.jsonl")
	content := `{"name":"good","ts_unix_ns":1}` + "\n" +
		"this-is-not-json" + "\n" +
		"" + "\n" + // empty line
		`{"name":"good2","ts_unix_ns":2}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTimings(vmDir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 valid events, got %d: %+v", len(got), got)
	}
}

// ── eventbus Close-via-interface ────────────────────────────────

func TestEventBus_InterfaceClose(t *testing.T) {
	var b EventBus = NewLocalEventBus()
	if err := b.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Publish after Close: still safe.
	b.Publish(PlatformEvent{Kind: "x"})
}

// ── helper: drivers package symbol referenced so the import is
// kept honest even when no test body uses it directly above.
var _ drivers.HostInfo
