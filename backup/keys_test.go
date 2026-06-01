package backup

import (
	"strings"
	"testing"
)

// TestSnapshotKey_Format pins the canonical "<p>/<v>/<s>.qcow2"
// shape so a future refactor that tweaks the separator or
// extension fails loudly.
func TestSnapshotKey_Format(t *testing.T) {
	got := SnapshotKey("proj-1", "vol-2", "snap-3")
	want := "proj-1/vol-2/snap-3.qcow2"
	if got != want {
		t.Errorf("SnapshotKey = %q, want %q", got, want)
	}
}

// TestSnapshotKey_EmptyInputs covers the defensive zero-return
// branch — callers like the snapshot CLI rely on "" → no upload.
func TestSnapshotKey_EmptyInputs(t *testing.T) {
	cases := []struct{ p, v, s string }{
		{"", "v", "s"},
		{"p", "", "s"},
		{"p", "v", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := SnapshotKey(c.p, c.v, c.s); got != "" {
			t.Errorf("SnapshotKey(%q,%q,%q) = %q, want empty",
				c.p, c.v, c.s, got)
		}
	}
}

// TestParseSnapshotKey_HappyPath asserts the round-trip of
// SnapshotKey ↔ ParseSnapshotKey is stable.
func TestParseSnapshotKey_HappyPath(t *testing.T) {
	parts, err := ParseSnapshotKey("p/v/s.qcow2")
	if err != nil {
		t.Fatalf("ParseSnapshotKey error: %v", err)
	}
	if parts.ProjectUUID != "p" || parts.VolumeUUID != "v" || parts.SnapshotUUID != "s" {
		t.Errorf("ParseSnapshotKey parts = %+v", parts)
	}
}

// TestParseSnapshotKey_RejectsBadShape covers each error branch
// — wrong suffix, wrong segment count, empty middle segment.
func TestParseSnapshotKey_RejectsBadShape(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"", "suffix"},
		{"p/v/s.bin", "suffix"},
		{"p/v.qcow2", "segments"},
		{"p/v/s/extra.qcow2", "segments"},
		{"p//s.qcow2", "empty segment"},
	}
	for _, c := range cases {
		_, err := ParseSnapshotKey(c.key)
		if err == nil {
			t.Errorf("ParseSnapshotKey(%q) returned nil error, want %q", c.key, c.want)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseSnapshotKey(%q) error = %v, want substring %q", c.key, err, c.want)
		}
	}
}

// TestParseSnapshotKey_PreservesUUIDStrings ensures we don't try to
// validate UUID syntax — the registry-side identifiers are opaque
// and may differ from RFC 4122.
func TestParseSnapshotKey_PreservesUUIDStrings(t *testing.T) {
	parts, err := ParseSnapshotKey("foo-bar-baz/quux_volume/snap.42.qcow2")
	if err != nil {
		t.Fatalf("ParseSnapshotKey error: %v", err)
	}
	if parts.ProjectUUID != "foo-bar-baz" {
		t.Errorf("ProjectUUID = %q", parts.ProjectUUID)
	}
	if parts.VolumeUUID != "quux_volume" {
		t.Errorf("VolumeUUID = %q", parts.VolumeUUID)
	}
	if parts.SnapshotUUID != "snap.42" {
		t.Errorf("SnapshotUUID = %q", parts.SnapshotUUID)
	}
}
