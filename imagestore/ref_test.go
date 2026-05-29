//go:build darwin

package imagestore

import (
	"strings"
	"testing"
)

func TestSanitizeRef_KnownValues(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ghcr.io/foo:latest", "ghcr.io_foo__latest"},
		{"image@sha256:abc", "image___sha256__abc"},
		{"simple", "simple"},
		{"host:5000/org/name:tag", "host__5000_org_name__tag"},
	}
	for _, tc := range cases {
		if got := SanitizeRef(tc.in); got != tc.want {
			t.Errorf("SanitizeRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeRef_NoSpecialCharsInResult(t *testing.T) {
	refs := []string{
		"ghcr.io/cirruslabs/ubuntu:latest",
		"registry.example.com/myrepo/image@sha256:abc123",
		"host:5000/org/name:tag",
	}
	for _, ref := range refs {
		got := SanitizeRef(ref)
		if strings.ContainsAny(got, "@:/") {
			t.Errorf("SanitizeRef(%q) = %q still contains special chars", ref, got)
		}
	}
}

func TestSanitizeRef_Roundtrip(t *testing.T) {
	// Roundtrip is only guaranteed for refs that contain no literal underscores,
	// since '_' is the encoding for '/'.
	refs := []string{
		"ghcr.io/cirruslabs/ubuntu:latest",
		"registry.example.com/myrepo/image@sha256:abc123",
		"host:5000/org/name:tag",
		"simple",
	}
	for _, ref := range refs {
		if got := UnsanitizeRef(SanitizeRef(ref)); got != ref {
			t.Errorf("roundtrip(%q) = %q", ref, got)
		}
	}
}
