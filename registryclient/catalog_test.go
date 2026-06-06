package registryclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// catalogHandler is the canonical /v2/_catalog responder used across
// most tests. Optionally enforces a Bearer token.
func catalogHandler(repos []string, requireToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/_catalog" {
			http.NotFound(w, r)
			return
		}
		if requireToken != "" && r.Header.Get("Authorization") != "Bearer "+requireToken {
			w.Header().Set("Www-Authenticate", `Bearer realm="REPLACE_ME"`)
			http.Error(w, "auth required", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": repos})
	}
}

func TestCatalog_SubstringFilter(t *testing.T) {
	srv := httptest.NewServer(catalogHandler([]string{
		"library/alpine",
		"library/nginx",
		"mycompany/api",
	}, ""))
	defer srv.Close()

	c := &CatalogClient{Endpoint: srv.URL, HTTPClient: srv.Client()}

	cases := []struct {
		query string
		want  []string
	}{
		{"alpine", []string{"library/alpine"}},
		{"lib", []string{"library/alpine", "library/nginx"}},
		{"ALPINE", []string{"library/alpine"}}, // case-insensitive
		{"company", []string{"mycompany/api"}},
		{"", []string{"library/alpine", "library/nginx", "mycompany/api"}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			got, err := c.Catalog(context.Background(), tc.query, 0)
			if err != nil {
				t.Fatalf("Catalog(%q) : %v", tc.query, err)
			}
			if !sliceEq(got, tc.want) {
				t.Fatalf("Catalog(%q) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestCatalog_LimitCap(t *testing.T) {
	srv := httptest.NewServer(catalogHandler([]string{
		"a/1", "a/2", "a/3", "a/4", "a/5",
	}, ""))
	defer srv.Close()

	c := &CatalogClient{Endpoint: srv.URL, HTTPClient: srv.Client()}
	got, err := c.Catalog(context.Background(), "", 3)
	if err != nil {
		t.Fatalf("Catalog : %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 repos, got %d : %v", len(got), got)
	}
}

func TestCatalog_BearerRefresh(t *testing.T) {
	// Two servers : the registry that demands a Bearer token, and the
	// token endpoint that mints it. The registry's WWW-Authenticate
	// realm points back at the token server.
	const issuedToken = "tok-from-realm"
	var tokenHits int32

	var registryURL string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&tokenHits, 1)
		if r.URL.Query().Get("service") != "registry.svc" {
			http.Error(w, "missing service", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": issuedToken})
	}))
	defer tokenSrv.Close()

	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/_catalog" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+issuedToken {
			w.Header().Set("Www-Authenticate",
				fmt.Sprintf(`Bearer realm="%s",service="registry.svc",scope="registry:catalog:*"`, tokenSrv.URL))
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []string{"library/alpine"}})
	}))
	defer registrySrv.Close()
	registryURL = registrySrv.URL

	c := &CatalogClient{Endpoint: registryURL, HTTPClient: registrySrv.Client()}
	got, err := c.Catalog(context.Background(), "alpine", 0)
	if err != nil {
		t.Fatalf("Catalog : %v", err)
	}
	if !sliceEq(got, []string{"library/alpine"}) {
		t.Fatalf("expected library/alpine, got %v", got)
	}
	if atomic.LoadInt32(&tokenHits) != 1 {
		t.Fatalf("expected 1 token fetch, got %d", tokenHits)
	}
	if c.BearerToken != issuedToken {
		t.Fatalf("client BearerToken not refreshed : %q", c.BearerToken)
	}
}

func TestCatalog_BearerRefreshFailsWithoutChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 401 but no WWW-Authenticate header.
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &CatalogClient{Endpoint: srv.URL, HTTPClient: srv.Client()}
	if _, err := c.Catalog(context.Background(), "", 0); err == nil {
		t.Fatal("expected error on bare 401")
	}
}

func TestCatalog_PaginationLinkHeader(t *testing.T) {
	// Two-page catalogue : page 1 returns a/1, a/2 plus
	// `Link: </v2/_catalog?last=a/2>; rel="next"`.
	// Page 2 returns b/1, b/2, no Link.
	var hits int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/_catalog", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		last := r.URL.Query().Get("last")
		w.Header().Set("Content-Type", "application/json")
		if last == "" {
			w.Header().Set("Link", `</v2/_catalog?last=a%2F2>; rel="next"`)
			_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []string{"a/1", "a/2"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"repositories": []string{"b/1", "b/2"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := &CatalogClient{Endpoint: srv.URL, HTTPClient: srv.Client()}
	got, err := c.Catalog(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("Catalog : %v", err)
	}
	if !sliceEq(got, []string{"a/1", "a/2", "b/1", "b/2"}) {
		t.Fatalf("expected 4 repos across 2 pages, got %v", got)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("expected 2 page fetches, got %d", hits)
	}
}

func TestCatalog_InsecureSkipVerify(t *testing.T) {
	// httptest.NewTLSServer uses a self-signed cert that fails default
	// verification ; Insecure=true on the client should let it through.
	srv := httptest.NewTLSServer(catalogHandler([]string{"library/alpine"}, ""))
	defer srv.Close()

	// Strip scheme so the client picks https via the Insecure path
	// (Insecure controls TLS verify ; we keep the scheme explicit
	// because Insecure-implies-http only kicks in when the scheme is
	// absent AND the test server is on https).
	c := &CatalogClient{
		Endpoint: srv.URL,
		Insecure: true,
		// Use a transport with InsecureSkipVerify so we don't depend
		// on the httptest.Client which already trusts the test cert.
		HTTPClient: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test
			},
			Timeout: 5 * time.Second,
		},
	}
	got, err := c.Catalog(context.Background(), "alpine", 0)
	if err != nil {
		t.Fatalf("Catalog : %v", err)
	}
	if !sliceEq(got, []string{"library/alpine"}) {
		t.Fatalf("got %v", got)
	}

	// And the negative case : default httpClient w/o Insecure should
	// fail verification on the self-signed cert.
	cBad := &CatalogClient{Endpoint: srv.URL}
	if _, err := cBad.Catalog(context.Background(), "", 0); err == nil {
		t.Fatal("expected TLS verification error without Insecure")
	}
}

func TestCatalog_PerRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sleep just past DefaultPerRequestTimeout, but stay within
		// DefaultTotalTimeout so the failure surfaces as a per-request
		// deadline rather than the outer total budget.
		time.Sleep(DefaultPerRequestTimeout + 2*time.Second)
		_, _ = w.Write([]byte(`{"repositories":[]}`))
	}))
	defer srv.Close()

	c := &CatalogClient{
		Endpoint: srv.URL,
		HTTPClient: &http.Client{
			Transport: http.DefaultTransport,
			// No client timeout : we exercise the context deadline path.
		},
	}
	start := time.Now()
	_, err := c.Catalog(context.Background(), "", 0)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "context deadline") &&
		!strings.Contains(err.Error(), "deadline exceeded") &&
		!strings.Contains(err.Error(), "Client.Timeout") {
		t.Fatalf("expected deadline error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > DefaultTotalTimeout+time.Second {
		t.Fatalf("Catalog took %s, expected < %s", elapsed, DefaultTotalTimeout)
	}
}

func TestCatalog_BadStatusSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "ratelimited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c := &CatalogClient{Endpoint: srv.URL, HTTPClient: srv.Client()}
	_, err := c.Catalog(context.Background(), "", 0)
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected 429 in error, got %v", err)
	}
}

func TestCatalog_MissingEndpoint(t *testing.T) {
	c := &CatalogClient{}
	if _, err := c.Catalog(context.Background(), "", 0); err == nil {
		t.Fatal("expected error on missing endpoint")
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://auth/token",service="ghcr.io",scope="repository:foo:pull"`)
	if scheme != "Bearer" {
		t.Fatalf("scheme = %q", scheme)
	}
	if params["realm"] != "https://auth/token" {
		t.Fatalf("realm = %q", params["realm"])
	}
	if params["service"] != "ghcr.io" {
		t.Fatalf("service = %q", params["service"])
	}
	if params["scope"] != "repository:foo:pull" {
		t.Fatalf("scope = %q", params["scope"])
	}
}

func TestNextLink(t *testing.T) {
	c := &CatalogClient{}
	got := c.nextLink(`</v2/_catalog?last=foo>; rel="next"`, "http://reg/v2/_catalog?n=2")
	want := "http://reg/v2/_catalog?last=foo"
	if got != want {
		t.Fatalf("nextLink = %q, want %q", got, want)
	}
	// Multiple links, pick rel=next.
	got = c.nextLink(`</prev>; rel="prev", </v2/_catalog?last=bar>; rel="next"`, "http://reg/v2/_catalog")
	if !strings.HasSuffix(got, "last=bar") {
		t.Fatalf("multi-link rel=next : %q", got)
	}
	// Absent header.
	if c.nextLink("", "http://reg/v2/_catalog") != "" {
		t.Fatal("expected empty next on empty header")
	}
}

func TestBaseURL_InsecureSchemeFallback(t *testing.T) {
	cases := []struct {
		endpoint string
		insecure bool
		want     string
	}{
		{"ghcr.io", false, "https://ghcr.io"},
		{"ghcr.io", true, "http://ghcr.io"},
		{"https://ghcr.io", true, "https://ghcr.io"},
		{"http://localhost:5000", false, "http://localhost:5000"},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			c := &CatalogClient{Endpoint: tc.endpoint, Insecure: tc.insecure}
			u, err := c.baseURL()
			if err != nil {
				t.Fatalf("baseURL : %v", err)
			}
			if u.String() != tc.want {
				t.Fatalf("got %q, want %q", u.String(), tc.want)
			}
		})
	}
}

func sliceEq(a, b []string) bool {
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
