// Package registryclient dials OCI Distribution Spec endpoints from the
// weft control plane. The first user is `SearchRegistryRemote`, which
// queries `/v2/_catalog` on an enrolled RegistryRemote and filters the
// returned repository list against a free-form query.
//
// The package is pure-Go (CGO=0) and avoids the full oras-go pull machinery
// — it only needs the catalogue + token refresh subset of the OCI
// Distribution Spec :
//
//   - GET <endpoint>/v2/_catalog?n=<count>&last=<marker>
//     returns {"repositories": [...]}, optionally with a `Link:
//     <url>; rel="next"` header for pagination (RFC5988).
//   - 401 Unauthorized + `WWW-Authenticate: Bearer realm=...` triggers a
//     one-shot token fetch against the advertised realm, after which the
//     client retries the original request once.
//
// The catalogue endpoint is gated by registry policy on most public
// registries (ghcr.io for example refuses to enumerate private repos
// without admin scope). When the upstream rejects the call the dialer
// surfaces the HTTP status verbatim so the operator can react.
package registryclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Default knobs ; exported so callers can tune per-call when needed.
const (
	// DefaultPerRequestTimeout caps any single HTTP roundtrip — catalogue
	// fetch, token fetch, follow-up page fetch.
	DefaultPerRequestTimeout = 10 * time.Second
	// DefaultTotalTimeout caps the whole pagination traversal so a
	// pathological registry can't pin a request goroutine indefinitely.
	DefaultTotalTimeout = 30 * time.Second
	// DefaultPageSize is the `n=` parameter on the first `/v2/_catalog`
	// fetch. Registries are free to ignore or clamp it.
	DefaultPageSize = 100
	// MaxPages bounds the pagination loop independently of the per-page
	// size advertised by the registry.
	MaxPages = 50
)

// CatalogClient queries the OCI Distribution `/v2/_catalog` endpoint
// of one registry. Zero value is not usable — Endpoint is required.
//
// Endpoint may be a bare host ("ghcr.io"), a scheme+host
// ("https://ghcr.io"), or include a port. When the scheme is omitted
// the client defaults to https unless Insecure is set, in which case
// it falls back to http.
//
// Insecure controls both the URL scheme fallback above AND TLS
// verification when the resolved URL is https — `Insecure=true` lifts
// hostname/CA checks. This mirrors the same flag stored on
// `weft.RegistryRemote`.
//
// BearerToken, when non-empty, is sent as `Authorization: Bearer
// <token>` on every request. If the registry replies 401 with a
// WWW-Authenticate Bearer challenge the client transparently fetches
// a token at the advertised realm and retries once. Setting
// BearerToken up front skips the first 401 round-trip on registries
// that don't accept anonymous catalogue access.
//
// HTTPClient is the transport. Nil = a defensive default with the
// per-request timeout + InsecureSkipVerify wired from Insecure. Tests
// pass an httptest.Server client here to mock the wire.
type CatalogClient struct {
	Endpoint    string
	Insecure    bool
	BearerToken string
	HTTPClient  *http.Client
}

// Catalog returns the repositories on the registry whose names contain
// `query` (case-insensitive substring match). An empty query returns the
// raw paginated list, capped at `limit`. limit <= 0 falls back to
// DefaultPageSize * MaxPages.
//
// Pagination follows the `Link: <url>; rel="next"` header until the
// registry stops sending one, the limit is reached, or MaxPages /
// DefaultTotalTimeout fire.
func (c *CatalogClient) Catalog(ctx context.Context, query string, limit int) ([]string, error) {
	if c.Endpoint == "" {
		return nil, errors.New("registryclient: endpoint is required")
	}
	if limit <= 0 {
		limit = DefaultPageSize * MaxPages
	}
	totalCtx, cancel := context.WithTimeout(ctx, DefaultTotalTimeout)
	defer cancel()

	httpClient := c.httpClient()

	first, err := c.firstURL()
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, DefaultPageSize)
	seen := make(map[string]struct{})
	q := strings.ToLower(query)

	next := first
	pages := 0
	for next != "" && pages < MaxPages {
		page, link, err := c.fetchPage(totalCtx, httpClient, next)
		if err != nil {
			return nil, err
		}
		for _, repo := range page {
			if _, dup := seen[repo]; dup {
				continue
			}
			seen[repo] = struct{}{}
			if q == "" || strings.Contains(strings.ToLower(repo), q) {
				out = append(out, repo)
				if len(out) >= limit {
					return out, nil
				}
			}
		}
		next = link
		pages++
	}
	return out, nil
}

// fetchPage runs a single GET against `pageURL`, transparently handling
// a 401 + Bearer challenge by fetching a token at the advertised realm
// and retrying the request once. Returns the page contents + the
// `Link: rel="next"` URL (empty when absent).
func (c *CatalogClient) fetchPage(ctx context.Context, httpClient *http.Client, pageURL string) ([]string, string, error) {
	resp, body, err := c.do(ctx, httpClient, pageURL, c.BearerToken)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		token, terr := c.refreshBearer(ctx, httpClient, resp.Header.Get("Www-Authenticate"))
		if terr != nil {
			return nil, "", fmt.Errorf("registryclient: %w", terr)
		}
		c.BearerToken = token
		resp, body, err = c.do(ctx, httpClient, pageURL, token)
		if err != nil {
			return nil, "", err
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("registryclient: %s returned HTTP %d : %s", pageURL, resp.StatusCode, snippet(body))
	}
	var doc struct {
		Repositories []string `json:"repositories"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, "", fmt.Errorf("registryclient: decode catalogue response : %w", err)
	}
	return doc.Repositories, c.nextLink(resp.Header.Get("Link"), pageURL), nil
}

// do issues a single GET with optional bearer auth. The response body
// is fully drained + returned so callers can re-inspect it without
// leaking the connection.
func (c *CatalogClient) do(ctx context.Context, httpClient *http.Client, target, token string) (*http.Response, []byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, DefaultPerRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("registryclient: build request %s : %w", target, err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("registryclient: GET %s : %w", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("registryclient: read body from %s : %w", target, err)
	}
	return resp, body, nil
}

// refreshBearer parses a `Bearer realm="..."[ ,service="...",scope="..."]`
// WWW-Authenticate header, fetches a token at the advertised realm,
// and returns it. Empty token = error ; the caller is responsible for
// surfacing it as an auth failure.
func (c *CatalogClient) refreshBearer(ctx context.Context, httpClient *http.Client, challenge string) (string, error) {
	if challenge == "" {
		return "", errors.New("401 without WWW-Authenticate header")
	}
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "Bearer") {
		return "", fmt.Errorf("unsupported auth scheme %q", scheme)
	}
	realm := params["realm"]
	if realm == "" {
		return "", errors.New("bearer challenge missing realm")
	}
	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parse realm %q : %w", realm, err)
	}
	q := tokenURL.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	if scope := params["scope"]; scope != "" {
		q.Set("scope", scope)
	}
	tokenURL.RawQuery = q.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, DefaultPerRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build token request : %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET token %s : %w", tokenURL.Redacted(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token body : %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint HTTP %d : %s", resp.StatusCode, snippet(body))
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("decode token response : %w", err)
	}
	if tok.Token != "" {
		return tok.Token, nil
	}
	if tok.AccessToken != "" {
		return tok.AccessToken, nil
	}
	return "", errors.New("token response missing token/access_token")
}

// firstURL builds the first `/v2/_catalog` URL from Endpoint, honouring
// the Insecure flag for the scheme fallback.
func (c *CatalogClient) firstURL() (string, error) {
	base, err := c.baseURL()
	if err != nil {
		return "", err
	}
	u := base.JoinPath("/v2/_catalog")
	q := u.Query()
	q.Set("n", fmt.Sprintf("%d", DefaultPageSize))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// baseURL normalises Endpoint into a scheme+host URL.
func (c *CatalogClient) baseURL() (*url.URL, error) {
	ep := strings.TrimSpace(c.Endpoint)
	if ep == "" {
		return nil, errors.New("empty endpoint")
	}
	if !strings.Contains(ep, "://") {
		scheme := "https"
		if c.Insecure {
			scheme = "http"
		}
		ep = scheme + "://" + ep
	}
	u, err := url.Parse(ep)
	if err != nil {
		return nil, fmt.Errorf("parse endpoint %q : %w", c.Endpoint, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("endpoint %q : missing host", c.Endpoint)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

// nextLink extracts the `rel="next"` URL from a Link header, resolved
// against `pageURL` so registries can use relative paths (`/v2/_catalog?last=...`).
func (c *CatalogClient) nextLink(header, pageURL string) string {
	if header == "" {
		return ""
	}
	parts := strings.Split(header, ",")
	for _, part := range parts {
		segs := strings.Split(strings.TrimSpace(part), ";")
		if len(segs) < 2 {
			continue
		}
		raw := strings.TrimSpace(segs[0])
		raw = strings.TrimPrefix(raw, "<")
		raw = strings.TrimSuffix(raw, ">")
		isNext := false
		for _, attr := range segs[1:] {
			attr = strings.TrimSpace(attr)
			if attr == `rel="next"` || attr == "rel=next" {
				isNext = true
				break
			}
		}
		if !isNext {
			continue
		}
		base, err := url.Parse(pageURL)
		if err != nil {
			return raw
		}
		linkURL, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return base.ResolveReference(linkURL).String()
	}
	return ""
}

// httpClient returns the configured HTTPClient or a defensive default
// honouring Insecure for TLS verification.
func (c *CatalogClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if c.Insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // gated by RegistryRemote.Insecure
	}
	return &http.Client{
		Transport: transport,
		Timeout:   DefaultPerRequestTimeout,
	}
}

// parseChallenge parses a WWW-Authenticate value such as
// `Bearer realm="https://auth/token",service="registry",scope="repository:..."`.
// Returns the scheme + the param map.
func parseChallenge(challenge string) (string, map[string]string) {
	params := map[string]string{}
	parts := strings.SplitN(strings.TrimSpace(challenge), " ", 2)
	scheme := parts[0]
	if len(parts) < 2 {
		return scheme, params
	}
	// Naive parse : split on commas, then on `=`. Bearer realm values are
	// double-quoted so commas inside the URL are rare ; we accept the
	// trade-off for not pulling in a full RFC7235 parser.
	for _, kv := range strings.Split(parts[1], ",") {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[:eq]))
		val := strings.Trim(strings.TrimSpace(kv[eq+1:]), `"`)
		params[key] = val
	}
	return scheme, params
}

func snippet(b []byte) string {
	const max = 256
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
