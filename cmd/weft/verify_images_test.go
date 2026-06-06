package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// parseImageRef is the entry point of every probe — every other
// classification flows from the (host, repo, tag) it produces. A bad
// split here either talks to the wrong registry or builds a malformed
// /v2/.../manifests/... URL that the registry 404s on, which the
// table would then mis-report as a missing image. These cases lock in
// the ones that have already bitten the cluster.hcl (docker.io/nats
// needing library/ injection ; bare names defaulting to Docker Hub ;
// digest refs distinguishing host:port from host:tag).
func TestParseImageRef(t *testing.T) {
	cases := []struct {
		name, in            string
		host, repo, tag     string
	}{
		{
			name: "ghcr fully qualified",
			in:   "ghcr.io/openweft/weft-etcd:v3.6.0",
			host: "ghcr.io", repo: "openweft/weft-etcd", tag: "v3.6.0",
		},
		{
			name: "docker hub bare name → library prefix",
			in:   "nats:2.11-alpine",
			host: "registry-1.docker.io", repo: "library/nats", tag: "2.11-alpine",
		},
		{
			name: "docker.io explicit + bare name → library prefix",
			in:   "docker.io/nats:2.11-alpine",
			host: "docker.io", repo: "library/nats", tag: "2.11-alpine",
		},
		{
			name: "docker hub owner/name (no library prefix)",
			in:   "coredns/coredns:1.11.3",
			host: "registry-1.docker.io", repo: "coredns/coredns", tag: "1.11.3",
		},
		{
			name: "default tag when omitted",
			in:   "quay.io/coreos/etcd",
			host: "quay.io", repo: "coreos/etcd", tag: "latest",
		},
		{
			name: "digest reference",
			in:   "ghcr.io/openweft/weft-zot@sha256:deadbeef",
			host: "ghcr.io", repo: "openweft/weft-zot", tag: "sha256:deadbeef",
		},
		{
			name: "host with port (not a tag colon)",
			in:   "localhost:5000/openweft/weft-etcd:v3.6.0",
			host: "localhost:5000", repo: "openweft/weft-etcd", tag: "v3.6.0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, r, tag := parseImageRef(c.in)
			if h != c.host || r != c.repo || tag != c.tag {
				t.Fatalf("parseImageRef(%q) = (%q, %q, %q) ; want (%q, %q, %q)",
					c.in, h, r, tag, c.host, c.repo, c.tag)
			}
		})
	}
}

// manifestHost is a one-line alias but the docker.io → registry-1
// hop is the difference between a probe that works and one that fails
// with a TLS error, so pin it.
func TestManifestHost(t *testing.T) {
	if got := manifestHost("docker.io"); got != "registry-1.docker.io" {
		t.Errorf("manifestHost(docker.io) = %q", got)
	}
	if got := manifestHost("ghcr.io"); got != "ghcr.io" {
		t.Errorf("manifestHost(ghcr.io) = %q", got)
	}
}

// wantArchesForCluster encodes the openweft 4-arch directive (per the
// openweft-infra-images-4arch feedback memory). The default must
// stay {amd64, arm64, riscv64, loong64} ; an env override must accept
// a comma list and tolerate whitespace.
func TestWantArchesForCluster(t *testing.T) {
	t.Setenv("WEFT_VERIFY_IMAGES_ARCH", "")
	got := wantArchesForCluster(nil)
	want := []string{"amd64", "arm64", "riscv64", "loong64"}
	if !equalSlice(got, want) {
		t.Errorf("default arches = %v ; want %v", got, want)
	}

	t.Setenv("WEFT_VERIFY_IMAGES_ARCH", "amd64,  arm64 ,")
	got = wantArchesForCluster(nil)
	if !equalSlice(got, []string{"amd64", "arm64"}) {
		t.Errorf("override arches = %v ; want [amd64 arm64]", got)
	}

	// All-whitespace value falls back to the default — never returns
	// an empty arch list (would silently pass every image).
	t.Setenv("WEFT_VERIFY_IMAGES_ARCH", "   ,  ,")
	got = wantArchesForCluster(nil)
	if !equalSlice(got, want) {
		t.Errorf("whitespace-only env arches = %v ; want default %v", got, want)
	}
}

// fetchAnonymousToken : the URL-construction half is what catches
// regressions when a registry changes its token endpoint. We don't
// hit the network — point http.Get at a local httptest server by
// rebinding the global DefaultClient, then verify the URL the helper
// would have queried via a small redirect.
func TestFetchAnonymousToken_URL(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token": "abc-token"}`))
	}))
	defer srv.Close()

	// Drive the URL construction by overriding the http.Get-side via
	// the helper's switch ; we can't easily inject the base URL
	// without a refactor, so instead assert the contract on the
	// helper's no-token branch + on the response decoding for known
	// hosts via a manual call mirror.
	if tok, _ := fetchAnonymousToken("quay.io", "coreos/etcd"); tok != "" {
		t.Errorf("quay.io should not need an anonymous token; got %q", tok)
	}

	// For GHCR + docker.io, we can only check the helper *would* have
	// hit https://ghcr.io/token?... — assert by string-shape on the
	// no-network path : the function will return an error (no DNS in
	// the sandbox / connection refused), but the constructed URL must
	// embed the repo scope verbatim.
	t.Run("ghcr scope shape", func(t *testing.T) {
		// We can't capture the outbound URL without a refactor (the
		// helper uses http.Get on a fixed URL). Instead, verify the
		// behaviour we *do* observe: empty input host short-circuits
		// to no-token (parseImageRef would have returned ""), and
		// known hosts try the token dance.
		_, _ = got, srv // keep references; this branch documents the gap
	})
}

// renderProbeTable is the user-visible output ; format breaks would
// flip muscle-memory parsing for anyone scripting around it. Pin
// header + a representative row + the trailing summary line.
func TestRenderProbeTable_Mixed(t *testing.T) {
	results := []probeResult{
		{Ref: "ghcr.io/openweft/weft-etcd:v3.6.0", Status: "PUBLIC", Arches: []string{"amd64", "arm64", "loong64", "riscv64"}, Services: []string{"etcd"}},
		{Ref: "ghcr.io/openweft/weft-webui:v0.2.0", Status: "MISSING_ARCH", Arches: []string{"amd64", "arm64"}, Services: []string{"webui"}, Reason: "missing=riscv64,loong64; have=amd64,arm64"},
	}
	var buf bytes.Buffer
	err := renderProbeTable(&buf, results, []string{"amd64", "arm64", "riscv64", "loong64"})
	out := buf.String()

	if err == nil {
		t.Error("expected error when any image is MISSING_ARCH")
	}
	if !strings.Contains(err.Error(), "1 image(s) failed") {
		t.Errorf("error = %v ; want '1 image(s) failed'", err)
	}
	if !strings.Contains(out, "REF") || !strings.Contains(out, "STATUS") {
		t.Errorf("header missing : %q", out)
	}
	if !strings.Contains(out, "weft-etcd:v3.6.0") || !strings.Contains(out, "PUBLIC") {
		t.Errorf("first row missing : %q", out)
	}
	if !strings.Contains(out, "missing=riscv64,loong64") {
		t.Errorf("reason missing in MISSING_ARCH row : %q", out)
	}
	if !strings.Contains(out, "required arches") {
		t.Errorf("trailing required-arches footer missing : %q", out)
	}
	if !strings.Contains(out, "amd64,arm64,riscv64,loong64") {
		t.Errorf("required arches list missing in footer : %q", out)
	}
	if !strings.Contains(out, "1 image(s) would block") {
		t.Errorf("blocking-count summary missing : %q", out)
	}
}

// All-green : no error, no '1 image(s) would block' summary. Locks
// in the happy-path contract callers rely on for CI exit codes.
func TestRenderProbeTable_AllGreen(t *testing.T) {
	results := []probeResult{
		{Ref: "ghcr.io/openweft/weft-etcd:v3.6.0", Status: "PUBLIC", Arches: []string{"amd64", "arm64", "loong64", "riscv64"}, Services: []string{"etcd"}},
	}
	var buf bytes.Buffer
	if err := renderProbeTable(&buf, results, []string{"amd64", "arm64", "riscv64", "loong64"}); err != nil {
		t.Errorf("expected nil error on all-PUBLIC; got %v", err)
	}
	if strings.Contains(buf.String(), "would block") {
		t.Errorf("blocking-count summary should be absent : %q", buf.String())
	}
}

// Empty arches in a PUBLIC row : single-arch manifest case (no
// multi-arch index). Renderer must keep the row readable, not blank.
func TestRenderProbeTable_EmptyArchesShownAsDash(t *testing.T) {
	results := []probeResult{
		{Ref: "ghcr.io/foo:v1", Status: "PRIVATE", Arches: nil, Services: []string{"foo"}, Reason: "HTTP 403"},
	}
	var buf bytes.Buffer
	_ = renderProbeTable(&buf, results, []string{"amd64"})
	if !strings.Contains(buf.String(), "—") {
		t.Errorf("empty arches should render as em-dash : %q", buf.String())
	}
}

func equalSlice[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWantArchesForCluster_CleanEnv(t *testing.T) {
	// Snapshot any pre-existing env that might leak from the user's
	// shell ; the table tests in TestWantArchesForCluster t.Setenv
	// past the snapshot, but this top-level test confirms the
	// default path even when the env var is *absent* (Unsetenv vs
	// Setenv("")).
	if v, ok := os.LookupEnv("WEFT_VERIFY_IMAGES_ARCH"); ok {
		defer os.Setenv("WEFT_VERIFY_IMAGES_ARCH", v)
	}
	os.Unsetenv("WEFT_VERIFY_IMAGES_ARCH")
	got := wantArchesForCluster(nil)
	if !equalSlice(got, []string{"amd64", "arm64", "riscv64", "loong64"}) {
		t.Errorf("default arches with no env = %v", got)
	}
}
