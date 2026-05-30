// Package oci is a minimal OCI Distribution Spec v2 client: enough to pull a
// manifest and its blobs (with optional bearer/basic auth), and to push a
// manifest with its blobs.
package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/net/http2"
)

// Custom artifact media types for kernel/initrd/cmdline/modules/plan blobs.
const (
	MediaTypeConfig  = "application/vnd.cloud-boot.boot.config.v1+json"
	MediaTypeKernel  = "application/vnd.cloud-boot.kernel.v1"
	MediaTypeInitrd  = "application/vnd.cloud-boot.initrd.v1"
	MediaTypeCmdline = "application/vnd.cloud-boot.cmdline.v1"
	MediaTypeModules = "application/vnd.cloud-boot.modules.v1.cpio+gzip"
	// MediaTypeModloop carries a raw squashfs blob (Alpine's
	// modloop-virt or any distro equivalent). cloud-boot-init exposes
	// it as `modloop=<oci-blob-url>` on the target kernel's cmdline so
	// the init script can wget + loop-mount it. The squashfs stays in
	// the registry — no client-side download.
	MediaTypeModloop = "application/vnd.cloud-boot.modloop.v1+squashfs"
	// MediaTypeApkovl carries an Alpine apkovl (system overlay tar.gz).
	// cloud-boot-init injects `apkovl=<oci-blob-url>` into the target
	// kernel's cmdline so Alpine's init can wget + untar it over the
	// new root, then apk-installs the packages listed in
	// etc/apk/world. The overlay turns a barebones netboot kernel into
	// a fully-functional Alpine system at first boot.
	MediaTypeApkovl = "application/vnd.cloud-boot.apkovl.v1+tar.gz"
	// MediaTypeSquashfs carries Debian / Ubuntu live's
	// `filesystem.squashfs` (or any distro equivalent — Kali, Tails,
	// Mint use the same shape). cloud-boot-init exposes it as
	// `fetch=<oci-blob-url>` on the target kernel's cmdline so
	// live-boot's initrd (Debian's `live-boot` package) downloads it
	// at switch_root time and loop-mounts it as the read-only rootfs.
	// Squashfs stays in the registry — no client-side download.
	MediaTypeSquashfs = "application/vnd.cloud-boot.squashfs.v1"
	MediaTypePlan     = "application/vnd.cloud-boot.plan.v1+hcl"
)

// Ref describes a fully-parsed image reference.
type Ref struct {
	Scheme    string // http or https
	Host      string // host[:port]
	Repo      string // name/space
	Reference string // tag or digest
}

// ParseRef parses "registry.example.com/path/to/repo:tag" or "@digest".
// http:// or https:// can be explicit; otherwise https is assumed
// unless host is localhost or 127.0.0.1.
func ParseRef(s string) (*Ref, error) {
	scheme := "https"
	if i := strings.Index(s, "://"); i >= 0 {
		scheme = s[:i]
		s = s[i+3:]
	}
	slash := strings.Index(s, "/")
	if slash < 0 {
		return nil, fmt.Errorf("missing repository in %q", s)
	}
	host, rest := s[:slash], s[slash+1:]
	if strings.HasPrefix(host, "localhost") || strings.HasPrefix(host, "127.0.0.1") {
		if !strings.HasPrefix(s, "https") {
			scheme = "http"
		}
	}

	repo := rest
	ref := "latest"
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		repo, ref = rest[:i], rest[i+1:]
	} else if i := strings.LastIndex(rest, ":"); i >= 0 {
		repo, ref = rest[:i], rest[i+1:]
	}
	return &Ref{Scheme: scheme, Host: host, Repo: repo, Reference: ref}, nil
}

func (r *Ref) base() string { return fmt.Sprintf("%s://%s/v2/%s", r.Scheme, r.Host, r.Repo) }

// Client is an OCI v2 client.
type Client struct {
	HTTP     *http.Client
	Username string
	Password string

	// BearerToken, when non-empty, is sent as
	// `Authorization: Bearer <BearerToken>` on every request,
	// short-circuiting the Docker Registry V2 token negotiation.
	// Use case: the operator already has a token from an external
	// auth flow (e.g. Keystone application-credential exchange)
	// and wants the OCI client to reuse it across all registry
	// hosts the plan references. Empty = fall through to the
	// existing per-host fetchBearer / Basic auth paths.
	BearerToken string

	tokens map[string]string // host -> bearer token (cached)
}

// NewClient builds a client with sensible defaults. Auth is read from env
// REGISTRY_USERNAME / REGISTRY_PASSWORD if not set explicitly.
//
// HTTP/2 behaviour:
//   - https:// targets negotiate HTTP/2 via ALPN automatically (Go's
//     default Transport already does this when h2 is offered by the
//     peer). No code change required for HTTPS registries.
//   - http:// (plaintext) targets default to HTTP/1.1. Set the env
//     var REGISTRY_HTTP2_CLEARTEXT=1 to opt into HTTP/2 over plaintext
//     (h2c, RFC 7540 §3.4). This is what local cloud-boot test
//     registries on 127.0.0.1:5000 typically run on; Docker
//     Distribution v3.0+ accepts h2c connections.
//
// The h2c transport keeps a tiny manual dialer that just dials TCP
// and returns the conn unwrapped — http2 then runs its own framing
// directly on the TCP stream (no TLS, no upgrade dance).
func NewClient() *Client {
	httpClient := &http.Client{Timeout: 60 * time.Second}
	if os.Getenv("REGISTRY_HTTP2_CLEARTEXT") == "1" {
		httpClient.Transport = &http2.Transport{
			AllowHTTP: true,
			// http2 normally only speaks to https endpoints; this
			// dialer lets it use the plaintext TCP socket directly
			// for http:// URLs.
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		}
	}
	return &Client{
		HTTP:     httpClient,
		Username: os.Getenv("REGISTRY_USERNAME"),
		Password: os.Getenv("REGISTRY_PASSWORD"),
		tokens:   map[string]string{},
	}
}

var manifestAccept = strings.Join([]string{
	ocispec.MediaTypeImageManifest,
	ocispec.MediaTypeImageIndex,
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
}, ", ")

// PullManifest fetches and decodes the image manifest at ref. It will NOT
// dereference an index; use PullManifestForPlatform for multi-arch repos.
func (c *Client) PullManifest(ref *Ref) (*ocispec.Manifest, []byte, error) {
	raw, _, err := c.fetchManifestRaw(ref)
	if err != nil {
		return nil, nil, err
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, raw, nil
}

// PullManifestForPlatform fetches the manifest at ref; if it is an index it
// picks the manifest matching (osName, arch) and refetches it.
func (c *Client) PullManifestForPlatform(ref *Ref, osName, arch string) (*ocispec.Manifest, []byte, error) {
	raw, mediaType, err := c.fetchManifestRaw(ref)
	if err != nil {
		return nil, nil, err
	}
	if isIndex(raw, mediaType) {
		var idx ocispec.Index
		if err := json.Unmarshal(raw, &idx); err != nil {
			return nil, nil, fmt.Errorf("decode index: %w", err)
		}
		var chosen *ocispec.Descriptor
		for i := range idx.Manifests {
			d := &idx.Manifests[i]
			if d.Platform == nil {
				continue
			}
			if d.Platform.OS == osName && d.Platform.Architecture == arch {
				chosen = d
				break
			}
		}
		if chosen == nil {
			return nil, nil, fmt.Errorf("no manifest in index for %s/%s", osName, arch)
		}
		sub := *ref
		sub.Reference = chosen.Digest.String()
		return c.PullManifest(&sub)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, fmt.Errorf("decode manifest: %w", err)
	}
	return &m, raw, nil
}

// DescribeManifest returns a descriptor (mediaType, digest, size) for the
// manifest at ref — used to assemble an index from already-pushed manifests.
func (c *Client) DescribeManifest(ref *Ref) (ocispec.Descriptor, error) {
	raw, mediaType, err := c.fetchManifestRaw(ref)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if mediaType == "" {
		// best effort: peek inside
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(raw, &probe)
		mediaType = probe.MediaType
		if mediaType == "" {
			mediaType = ocispec.MediaTypeImageManifest
		}
	}
	return ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    digest.FromBytes(raw),
		Size:      int64(len(raw)),
	}, nil
}

func (c *Client) fetchManifestRaw(ref *Ref) (raw []byte, contentType string, err error) {
	u := ref.base() + "/manifests/" + ref.Reference
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Accept", manifestAccept)
	resp, err := c.do(ref, req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", errStatus(resp)
	}
	contentType = resp.Header.Get("Content-Type")
	raw, err = io.ReadAll(resp.Body)
	return
}

func isIndex(raw []byte, contentType string) bool {
	switch contentType {
	case ocispec.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	}
	var probe struct {
		MediaType string                 `json:"mediaType"`
		Manifests []ocispec.Descriptor   `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return false
	}
	if probe.MediaType == ocispec.MediaTypeImageIndex ||
		probe.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		return true
	}
	return len(probe.Manifests) > 0
}

// PullBlob streams a blob into w and verifies the digest.
func (c *Client) PullBlob(ref *Ref, d digest.Digest, w io.Writer) (int64, error) {
	u := ref.base() + "/blobs/" + d.String()
	req, _ := http.NewRequest("GET", u, nil)
	resp, err := c.do(ref, req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, errStatus(resp)
	}
	h := sha256.New()
	tee := io.MultiWriter(w, h)
	n, err := io.Copy(tee, resp.Body)
	if err != nil {
		return n, err
	}
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != d.String() {
		return n, fmt.Errorf("digest mismatch: got %s want %s", got, d)
	}
	return n, nil
}

// PushBlob uploads a blob via POST(start) + PUT(complete) (monolithic upload).
func (c *Client) PushBlob(ref *Ref, data []byte) (digest.Digest, error) {
	d := digest.FromBytes(data)
	if exists, err := c.headBlob(ref, d); err == nil && exists {
		return d, nil
	}
	// 1) initiate
	req, _ := http.NewRequest("POST", ref.base()+"/blobs/uploads/", nil)
	resp, err := c.do(ref, req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		return "", errStatus(resp)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("upload location header missing")
	}
	// 2) complete with PUT ?digest=...
	put, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	if !put.IsAbs() {
		put = &url.URL{Scheme: ref.Scheme, Host: ref.Host, Path: put.Path, RawQuery: put.RawQuery}
	}
	q := put.Query()
	q.Set("digest", d.String())
	put.RawQuery = q.Encode()
	req, _ = http.NewRequest("PUT", put.String(), bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(data))
	resp, err = c.do(ref, req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		return "", errStatus(resp)
	}
	return d, nil
}

func (c *Client) headBlob(ref *Ref, d digest.Digest) (bool, error) {
	req, _ := http.NewRequest("HEAD", ref.base()+"/blobs/"+d.String(), nil)
	resp, err := c.do(ref, req)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	return resp.StatusCode == 200, nil
}

// PushManifest PUTs the manifest at the given reference.
func (c *Client) PushManifest(ref *Ref, m *ocispec.Manifest) (digest.Digest, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return c.putManifest(ref, raw, ocispec.MediaTypeImageManifest)
}

// PushIndex PUTs an image index (manifest list) at ref.
func (c *Client) PushIndex(ref *Ref, idx *ocispec.Index) (digest.Digest, error) {
	raw, err := json.Marshal(idx)
	if err != nil {
		return "", err
	}
	return c.putManifest(ref, raw, ocispec.MediaTypeImageIndex)
}

func (c *Client) putManifest(ref *Ref, raw []byte, mediaType string) (digest.Digest, error) {
	req, _ := http.NewRequest("PUT", ref.base()+"/manifests/"+ref.Reference, bytes.NewReader(raw))
	req.Header.Set("Content-Type", mediaType)
	req.ContentLength = int64(len(raw))
	resp, err := c.do(ref, req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		return "", errStatus(resp)
	}
	return digest.FromBytes(raw), nil
}

// do issues req against the resolved endpoints of ref.Host in RFC 2782
// order. Non-SRV hosts produce a single-element endpoint list and behave
// identically to the original single-shot client. Retriable failures
// (connection errors and 5xx/408/429 responses) make the next endpoint be
// tried; 4xx errors are returned immediately because retrying won't change
// the outcome.
//
// The Bearer-token cache is keyed by the *original* ref.Host (i.e. the SRV
// name), so authentication is shared across the replicas of one logical
// registry.
// resolveEndpoints is a package-level indirection over ResolveEndpoints so
// tests can inject a deterministic endpoint list without going to DNS.
var resolveEndpoints = ResolveEndpoints

func (c *Client) do(ref *Ref, req *http.Request) (*http.Response, error) {
	eps, err := resolveEndpoints(ref.Host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", ref.Host, err)
	}
	var last error
	for i, ep := range eps {
		if err := rewriteHost(req, ep.Host); err != nil {
			return nil, err
		}
		resp, err := c.doOnce(ref, req)
		if err == nil {
			if !shouldRetryStatus(resp.StatusCode) || i+1 == len(eps) {
				return resp, nil
			}
			last = fmt.Errorf("status %d from %s", resp.StatusCode, ep.Host)
			resp.Body.Close()
			continue
		}
		last = fmt.Errorf("%s: %w", ep.Host, err)
		if !isRetriableNetErr(err) || i+1 == len(eps) {
			return nil, last
		}
	}
	return nil, last
}

// doOnce sends req once against the host already written into req.URL,
// handling a single 401 challenge by negotiating a Bearer token.
func (c *Client) doOnce(ref *Ref, req *http.Request) (*http.Response, error) {
	c.applyAuth(ref, req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 401 {
		return resp, nil
	}
	chal := resp.Header.Get("Www-Authenticate")
	resp.Body.Close()
	if chal == "" {
		return nil, errors.New("401 with no Www-Authenticate")
	}
	tok, err := c.fetchBearer(chal)
	if err != nil {
		return nil, err
	}
	c.tokens[ref.Host] = tok
	r2 := req.Clone(req.Context())
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		r2.Body = body
	}
	c.applyAuth(ref, r2)
	return c.HTTP.Do(r2)
}

// rewriteHost swaps req.URL.Host (and req.Host) to the given endpoint and
// rewinds the request body if it was constructed with GetBody.
func rewriteHost(req *http.Request, host string) error {
	req.URL.Host = host
	req.Host = host
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return fmt.Errorf("rewind body: %w", err)
		}
		req.Body = body
	}
	return nil
}

func shouldRetryStatus(s int) bool {
	return s >= 500 || s == 408 || s == 429
}

func isRetriableNetErr(err error) bool {
	// Conservatively treat any transport-level error as retriable. The auth
	// challenge layer in doOnce never wraps a plain network error in another
	// type, so this is exhaustive in practice.
	return err != nil
}

func (c *Client) applyAuth(ref *Ref, req *http.Request) {
	// Operator-supplied bearer (e.g. from Keystone AC exchange)
	// wins over both the cached negotiated token and basic auth —
	// it's the authoritative credential for this VM session.
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
		return
	}
	if t, ok := c.tokens[ref.Host]; ok {
		req.Header.Set("Authorization", "Bearer "+t)
		return
	}
	if c.Username != "" || c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
}

// fetchBearer parses Bearer realm=...,service=...,scope=... and obtains a token.
func (c *Client) fetchBearer(chal string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(chal), "bearer ") {
		return "", fmt.Errorf("unsupported challenge: %s", chal)
	}
	params := map[string]string{}
	for _, p := range splitChallenge(chal[len("Bearer "):]) {
		kv := strings.SplitN(p, "=", 2)
		if len(kv) != 2 {
			continue
		}
		params[strings.TrimSpace(kv[0])] = strings.Trim(kv[1], `" `)
	}
	realm := params["realm"]
	if realm == "" {
		return "", errors.New("bearer challenge missing realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if s, ok := params["service"]; ok {
		q.Set("service", s)
	}
	if s, ok := params["scope"]; ok {
		q.Set("scope", s)
	}
	u.RawQuery = q.Encode()
	req, _ := http.NewRequest("GET", u.String(), nil)
	if c.Username != "" || c.Password != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errStatus(resp)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", err
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	return tok.AccessToken, nil
}

func splitChallenge(s string) []string {
	out := []string{}
	var cur strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '"':
			inQuote = !inQuote
			cur.WriteByte(ch)
		case ch == ',' && !inQuote:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if cur.Len() > 0 {
		out = append(out, strings.TrimSpace(cur.String()))
	}
	return out
}

func errStatus(r *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	return fmt.Errorf("%s %s: %s", r.Request.Method, r.Request.URL, strings.TrimSpace(string(body)))
}
