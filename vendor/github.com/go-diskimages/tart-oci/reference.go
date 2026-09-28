package tartoci

import (
	"fmt"
	"strings"
)

// Reference identifies a Tart image in an OCI registry, e.g.
// "ghcr.io/cirruslabs/macos-sequoia-base:latest".
type Reference struct {
	Registry   string // registry host[:port], e.g. "ghcr.io"
	Repository string // repository path, e.g. "cirruslabs/macos-sequoia-base"
	Tag        string // e.g. "latest"; empty when Digest is set
	Digest     string // e.g. "sha256:…"; empty when Tag is set
}

const digestPrefix = "sha256:"
const digestLen = len(digestPrefix) + 64 // sha256 hex

// ParseReference parses "registry/repository[:tag|@sha256:…]".
//
// A registry host is required and is recognised as the first path element when
// it contains "." or ":" or equals "localhost" (the same heuristic Docker and
// Tart use). When neither a tag nor a digest is given the tag defaults to
// "latest".
func ParseReference(s string) (Reference, error) {
	if s == "" {
		return Reference{}, fmt.Errorf("tart-oci: empty reference")
	}
	var ref Reference
	name := s

	// A digest (which itself contains ':') is split off first.
	if at := strings.LastIndex(name, "@"); at >= 0 {
		ref.Digest = name[at+1:]
		name = name[:at]
		if !strings.HasPrefix(ref.Digest, digestPrefix) || len(ref.Digest) != digestLen {
			return Reference{}, fmt.Errorf("tart-oci: invalid digest %q in reference %q", ref.Digest, s)
		}
	}

	slash := strings.IndexByte(name, '/')
	if slash < 0 {
		return Reference{}, fmt.Errorf("tart-oci: reference %q is missing a registry host", s)
	}
	host := name[:slash]
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		return Reference{}, fmt.Errorf("tart-oci: reference %q is missing a registry host", s)
	}
	ref.Registry = host
	rest := name[slash+1:]

	// A tag is the last ':' in the remaining path (the digest is already gone).
	if ref.Digest == "" {
		if colon := strings.LastIndexByte(rest, ':'); colon >= 0 {
			ref.Tag = rest[colon+1:]
			rest = rest[:colon]
			if ref.Tag == "" {
				return Reference{}, fmt.Errorf("tart-oci: reference %q has an empty tag", s)
			}
		} else {
			ref.Tag = "latest"
		}
	}
	if rest == "" {
		return Reference{}, fmt.Errorf("tart-oci: reference %q is missing a repository", s)
	}
	ref.Repository = rest
	return ref, nil
}

// manifestRef returns the tag or digest used to address the manifest.
func (r Reference) manifestRef() string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

// String rebuilds the canonical reference string.
func (r Reference) String() string {
	if r.Digest != "" {
		return r.Registry + "/" + r.Repository + "@" + r.Digest
	}
	return r.Registry + "/" + r.Repository + ":" + r.Tag
}
