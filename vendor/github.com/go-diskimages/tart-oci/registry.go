package tartoci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Registry is a minimal OCI distribution (registry v2) client scoped to one
// repository. It performs the anonymous Bearer-token flow used by ghcr.io and
// Docker Hub lazily, on the first 401 response.
type Registry struct {
	ref     Reference
	baseURL string
	client  *http.Client
	user    string
	pass    string
	token   string
}

// Option configures a Registry.
type Option func(*Registry)

// WithHTTPClient sets the HTTP client (e.g. one pointed at an httptest server,
// or with a custom timeout/transport).
func WithHTTPClient(c *http.Client) Option { return func(r *Registry) { r.client = c } }

// WithBaseURL overrides the registry base URL (scheme://host). It is primarily
// useful for tests; in production the base URL is derived from the reference.
func WithBaseURL(u string) Option {
	return func(r *Registry) { r.baseURL = strings.TrimRight(u, "/") }
}

// WithBasicAuth supplies credentials sent to the token endpoint (for private
// repositories).
func WithBasicAuth(user, pass string) Option {
	return func(r *Registry) { r.user, r.pass = user, pass }
}

// NewRegistry returns a Registry for ref.
func NewRegistry(ref Reference, opts ...Option) *Registry {
	r := &Registry{ref: ref, client: http.DefaultClient, baseURL: defaultBaseURL(ref.Registry)}
	for _, o := range opts {
		o(r)
	}
	return r
}

func defaultBaseURL(host string) string {
	scheme := "https"
	if isLocalHost(host) {
		scheme = "http"
	}
	return scheme + "://" + host
}

func isLocalHost(host string) bool {
	h := host
	if strings.HasPrefix(h, "[") { // [::1]:port
		if end := strings.IndexByte(h, ']'); end >= 0 {
			h = h[1:end]
		}
	} else if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// get issues an authenticated GET, retrying once after a 401 to obtain a token.
// A non-200 final response is returned as an error. The caller owns resp.Body.
func (r *Registry) get(ctx context.Context, path, accept string) (*http.Response, error) {
	resp, err := r.roundtrip(ctx, path, accept)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		challenge := resp.Header.Get("WWW-Authenticate")
		_ = resp.Body.Close()
		if err := r.authenticate(ctx, challenge); err != nil {
			return nil, err
		}
		if resp, err = r.roundtrip(ctx, path, accept); err != nil {
			return nil, err
		}
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("tart-oci: GET %s: unexpected status %d: %s",
			path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return resp, nil
}

func (r *Registry) roundtrip(ctx context.Context, path, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("tart-oci: build request: %w", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tart-oci: GET %s: %w", path, err)
	}
	return resp, nil
}

// authenticate parses a Bearer WWW-Authenticate challenge, fetches a token from
// its realm, and stores it for subsequent requests.
func (r *Registry) authenticate(ctx context.Context, challenge string) error {
	const bearer = "Bearer "
	if !strings.HasPrefix(challenge, bearer) {
		return fmt.Errorf("tart-oci: unsupported auth challenge %q", challenge)
	}
	params := parseChallenge(challenge[len(bearer):])
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("tart-oci: auth challenge is missing a realm: %q", challenge)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm, nil)
	if err != nil {
		return fmt.Errorf("tart-oci: bad token realm %q: %w", realm, err)
	}
	q := req.URL.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	if s := params["scope"]; s != "" {
		q.Set("scope", s)
	}
	req.URL.RawQuery = q.Encode()
	if r.user != "" || r.pass != "" {
		req.SetBasicAuth(r.user, r.pass)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("tart-oci: fetch token: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tart-oci: token endpoint returned status %d", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return fmt.Errorf("tart-oci: decode token: %w", err)
	}
	r.token = tok.Token
	if r.token == "" {
		r.token = tok.AccessToken
	}
	if r.token == "" {
		return fmt.Errorf("tart-oci: token endpoint returned an empty token")
	}
	return nil
}

// parseChallenge parses a comma-separated list of key="value" pairs from a
// WWW-Authenticate header value.
func parseChallenge(s string) map[string]string {
	out := map[string]string{}
	for _, part := range splitParams(s) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		out[key] = val
	}
	return out
}

// splitParams splits on commas that are not inside a quoted string.
func splitParams(s string) []string {
	var parts []string
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			b.WriteByte(c)
		case c == ',' && !inQuote:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteByte(c)
		}
	}
	if b.Len() > 0 {
		parts = append(parts, b.String())
	}
	return parts
}

// Manifest fetches and parses the image manifest addressed by the reference. If
// the reference resolves to an image index, the first listed manifest is
// fetched and returned.
func (r *Registry) Manifest(ctx context.Context) (*Manifest, error) {
	accept := strings.Join([]string{MediaTypeOCIManifest, MediaTypeOCIIndex, MediaTypeDockerManifest}, ", ")
	return r.manifestAt(ctx, r.ref.manifestRef(), accept)
}

func (r *Registry) manifestAt(ctx context.Context, ref, accept string) (*Manifest, error) {
	resp, err := r.get(ctx, "/v2/"+r.ref.Repository+"/manifests/"+ref, accept)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("tart-oci: read manifest: %w", err)
	}

	// An image index has a non-empty "manifests" array; resolve it to the first
	// image manifest.
	var idx index
	if json.Unmarshal(data, &idx) == nil && len(idx.Manifests) > 0 {
		return r.manifestAt(ctx, idx.Manifests[0].Digest, accept)
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("tart-oci: parse manifest: %w", err)
	}
	if len(m.Layers) == 0 {
		return nil, fmt.Errorf("tart-oci: manifest %s has no layers", ref)
	}
	return &m, nil
}

// blob opens a blob body for reading. The caller must close it.
func (r *Registry) blob(ctx context.Context, digest string) (io.ReadCloser, error) {
	resp, err := r.get(ctx, "/v2/"+r.ref.Repository+"/blobs/"+digest, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}
