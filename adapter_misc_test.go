//go:build darwin

package weft

// adapter_misc_test.go covers the misc small adapter methods that
// don't fit the registry-wrappers test file: image-cache helpers
// (ListCachedImages + dirSize + imageCacheNameAndFormat +
// NewCachedImage accessors), VMDir family, VMExists family, OCI
// glue, plus Name/Available.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdapter_NameAvailable(t *testing.T) {
	a := newAdapterForRegistries(t)
	if a.Name() != "vz" {
		t.Errorf("Name() = %q, want vz", a.Name())
	}
	if !a.Available() {
		t.Errorf("Available() = false, want true on darwin build")
	}
}

func TestAdapter_VMDir_AccessorsDelegate(t *testing.T) {
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("test-proj")

	// VMDirFor + VMDirIn both go through vmDirIn(project, name).
	for _, dir := range []string{
		a.VMDirFor(p.UUID, "vm-a"),
		a.VMDirIn(p.UUID, "vm-a"),
	} {
		if !strings.Contains(dir, p.UUID) {
			t.Errorf("dir %q should contain project UUID %q", dir, p.UUID)
		}
		if !strings.HasSuffix(dir, "vm-a") {
			t.Errorf("dir %q should end with vm name", dir)
		}
	}

	// VMDir(name) without project searches every project and
	// falls back to <vmsDir>/<defaultProjectUUID>/<name>.
	dir := a.VMDir("vm-a")
	if !strings.HasSuffix(dir, "vm-a") {
		t.Errorf("VMDir = %q", dir)
	}

	// DiskPath returns <vmDir>/disk.img.
	disk := a.DiskPath("vm-a")
	if !strings.HasSuffix(disk, "/disk.img") {
		t.Errorf("DiskPath = %q", disk)
	}
}

func TestAdapter_VMExists_NotPresent(t *testing.T) {
	a := newAdapterForRegistries(t)
	if a.VMExists("never-created") {
		t.Errorf("VMExists on unknown should be false")
	}
	if a.VMExistsIn("p", "never-created") {
		t.Errorf("VMExistsIn on unknown should be false")
	}
}

func TestAdapter_VMExistsIn_DirCreated(t *testing.T) {
	a := newAdapterForRegistries(t)
	p, _, _ := a.CreateProject("proj1")
	dir := a.VMDirIn(p.UUID, "vm-x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !a.VMExistsIn(p.UUID, "vm-x") {
		t.Errorf("VMExistsIn should be true after mkdir")
	}
	if !a.VMExists("vm-x") {
		t.Errorf("VMExists should be true after mkdir")
	}
}

// ── CachedImage value-type accessors ────────────────────────────

func TestCachedImage_Accessors(t *testing.T) {
	ci := NewCachedImage("http://example.com/img", "img.raw", "raw", 12345)
	if ci.URL() != "http://example.com/img" {
		t.Errorf("URL() = %q", ci.URL())
	}
	if ci.Name() != "img.raw" {
		t.Errorf("Name() = %q", ci.Name())
	}
	if ci.Format() != "raw" {
		t.Errorf("Format() = %q", ci.Format())
	}
	if ci.SizeBytes() != 12345 {
		t.Errorf("SizeBytes() = %d", ci.SizeBytes())
	}
}

// TestImageCacheNameAndFormat covers the OCI-layout + raw + qcow2
// + img + unknown branches, plus the directory-walk error path.
func TestImageCacheNameAndFormat(t *testing.T) {
	tmp := t.TempDir()

	// Empty directory → ("", "unknown")
	emptyDir := filepath.Join(tmp, "empty")
	if err := os.MkdirAll(emptyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	name, format := imageCacheNameAndFormat(emptyDir)
	if name != "" || format != "unknown" {
		t.Errorf("empty dir: got (%q,%q)", name, format)
	}

	// OCI layout (index.json present).
	ociDir := filepath.Join(tmp, "oci")
	_ = os.MkdirAll(ociDir, 0o700)
	_ = os.WriteFile(filepath.Join(ociDir, "index.json"), []byte("{}"), 0o600)
	name, format = imageCacheNameAndFormat(ociDir)
	if name != "(oci layout)" || format != "oci" {
		t.Errorf("oci: got (%q,%q)", name, format)
	}

	// HTTP raw download.
	rawDir := filepath.Join(tmp, "raw")
	_ = os.MkdirAll(rawDir, 0o700)
	_ = os.WriteFile(filepath.Join(rawDir, "ubuntu.raw"), []byte("x"), 0o600)
	name, format = imageCacheNameAndFormat(rawDir)
	if name != "ubuntu.raw" || format != "raw" {
		t.Errorf("raw: got (%q,%q)", name, format)
	}

	// HTTP qcow2.
	qcowDir := filepath.Join(tmp, "qcow2")
	_ = os.MkdirAll(qcowDir, 0o700)
	_ = os.WriteFile(filepath.Join(qcowDir, "img.qcow2"), []byte("x"), 0o600)
	name, format = imageCacheNameAndFormat(qcowDir)
	if format != "qcow2" {
		t.Errorf("qcow2: format=%q", format)
	}

	// HTTP img.
	imgDir := filepath.Join(tmp, "img")
	_ = os.MkdirAll(imgDir, 0o700)
	_ = os.WriteFile(filepath.Join(imgDir, "cloud.img"), []byte("x"), 0o600)
	name, format = imageCacheNameAndFormat(imgDir)
	if format != "img" {
		t.Errorf("img: format=%q", format)
	}

	// Unknown extension.
	unkDir := filepath.Join(tmp, "unknown")
	_ = os.MkdirAll(unkDir, 0o700)
	_ = os.WriteFile(filepath.Join(unkDir, "weird.zzz"), []byte("x"), 0o600)
	name, format = imageCacheNameAndFormat(unkDir)
	if format != "unknown" {
		t.Errorf("unknown: format=%q", format)
	}

	// Non-existent directory: ReadDir errors → ("", "unknown").
	missing := filepath.Join(tmp, "nope")
	name, format = imageCacheNameAndFormat(missing)
	if name != "" || format != "unknown" {
		t.Errorf("missing: got (%q,%q)", name, format)
	}
}

// TestDirSize sums file sizes recursively.
func TestDirSize(t *testing.T) {
	tmp := t.TempDir()
	// Empty dir = 0.
	if got := dirSize(tmp); got != 0 {
		t.Errorf("empty dir size = %d, want 0", got)
	}
	// Some files.
	_ = os.WriteFile(filepath.Join(tmp, "a"), []byte("hello"), 0o600)
	sub := filepath.Join(tmp, "sub")
	_ = os.MkdirAll(sub, 0o700)
	_ = os.WriteFile(filepath.Join(sub, "b"), []byte("world!"), 0o600)
	if got := dirSize(tmp); got != int64(len("hello")+len("world!")) {
		t.Errorf("dirSize = %d, want %d", got, len("hello")+len("world!"))
	}
	// Non-existent dir: WalkDir errors out gracefully → 0.
	if got := dirSize(filepath.Join(tmp, "missing")); got != 0 {
		t.Errorf("missing dir size = %d, want 0", got)
	}
}

// TestListCachedImages exercises the directory walk + value
// extraction. Empty cache dir is allowed (returns nil).
func TestListCachedImages(t *testing.T) {
	a := newAdapterForRegistries(t)

	// Cache dir doesn't exist yet → returns (nil, nil).
	imgs, err := a.ListCachedImages()
	if err != nil {
		t.Fatalf("empty cache: %v", err)
	}
	if len(imgs) != 0 {
		t.Errorf("empty cache should be empty, got %d", len(imgs))
	}

	// Populate one cache entry.
	cacheDir := a.cacheDir()
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(cacheDir, "my_image")
	_ = os.MkdirAll(entry, 0o700)
	_ = os.WriteFile(filepath.Join(entry, "img.raw"), []byte("x"), 0o600)

	imgs, err = a.ListCachedImages()
	if err != nil {
		t.Fatalf("ListCachedImages: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("expected 1 image, got %d", len(imgs))
	}
	if imgs[0].Name() != "img.raw" {
		t.Errorf("Name = %q", imgs[0].Name())
	}
}

// TestImageInCache_DeleteOCI exercises wrappers via the
// imagestore — for an absent image the methods should not panic.
func TestAdapter_OCIWrappers_AbsentImage(t *testing.T) {
	a := newAdapterForRegistries(t)
	if a.ImageInCache("ghcr.io/nope:tag") {
		t.Errorf("ImageInCache on absent should be false")
	}
	// ListOCI on empty cache returns empty or error - both acceptable.
	_, _ = a.ListOCI()

	// DeleteOCI on missing: an error is acceptable; we just exercise.
	_ = a.DeleteOCI("nope:tag")
}

// TestPull_NoImages confirms the early-return for an empty list.
func TestAdapter_Pull_EmptyList(t *testing.T) {
	a := newAdapterForRegistries(t)
	if err := a.Pull(context.Background(), nil, 0); err != nil {
		t.Errorf("Pull empty list: %v", err)
	}
}

// TestPullWithOutput_BogusRef exercises the wrapper with a
// definitely-unreachable image; the result may be an error but
// we just want to exercise the code path.
func TestAdapter_PullWithOutput_BogusRef(t *testing.T) {
	a := newAdapterForRegistries(t)
	// We use a context that's already cancelled to short-circuit
	// any network calls cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = a.PullWithOutput(ctx, "ghcr.io/openweft/this-does-not-exist-and-never-will:tag", io.Discard)
}

// TestCachedImagePath_AbsentImage returns an error.
func TestAdapter_CachedImagePath_AbsentImage(t *testing.T) {
	a := newAdapterForRegistries(t)
	if _, err := a.CachedImagePath("never-pulled"); err == nil {
		t.Errorf("CachedImagePath on missing should error")
	}
}

// ── GetOSFromCache: empty for absent image ──────────────────────

func TestAdapter_GetOSFromCache_AbsentImage(t *testing.T) {
	a := newAdapterForRegistries(t)
	// Empty result is fine — the helper short-circuits when the
	// metadata file isn't there.
	got := a.GetOSFromCache("absent")
	_ = got // value is "" or some default
}

// TestAdapter_LocalHostUUID exercises the public LocalHostUUID
// path (the persisted file is materialised by NewWithStorage).
func TestAdapter_LocalHostUUID_FromPersistedFile(t *testing.T) {
	a := newAdapterForRegistries(t)
	uuid := a.LocalHostUUID()
	if uuid == "" {
		t.Errorf("LocalHostUUID should be non-empty after NewWithStorage")
	}
}

// TestAdapter_LocalHostUUID_NilSafe ensures the nil-receiver guard
// returns the empty string instead of panicking.
func TestAdapter_LocalHostUUID_NilReceiver(t *testing.T) {
	var a *Adapter
	if got := a.LocalHostUUID(); got != "" {
		t.Errorf("nil adapter should return empty UUID, got %q", got)
	}
}

// TestAdapter_LocalHostUUID_MissingFile covers the read-error path:
// pointing the Adapter at a stateDir where the host-uuid file
// doesn't exist yet returns empty.
func TestAdapter_LocalHostUUID_MissingFile(t *testing.T) {
	tmp := t.TempDir()
	// Bare Adapter — bypass NewWithStorage so the file isn't written.
	a := &Adapter{stateDir: tmp}
	if got := a.LocalHostUUID(); got != "" {
		t.Errorf("missing file should return empty UUID, got %q", got)
	}
}
