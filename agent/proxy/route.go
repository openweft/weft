package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Route is one host → backend mapping plus its TLS posture. We keep the
// surface deliberately narrow — the goal is to expose the 80% of Caddy
// that matches weft's needs (TLS-terminating reverse proxy with
// auto-HTTPS) without baking the whole Caddyfile vocabulary into the HCL.
//
// Anything Caddy can do that doesn't fit here is reachable via the future
// `route { caddy_raw = "<caddy json fragment>" }` escape hatch — not yet
// implemented; first the basic shape needs to settle.
type Route struct {
	// Host is the SNI/Host header value Caddy matches against. Wildcards
	// follow Caddy's matcher syntax (`*.example.com`, `*.internal`).
	Host string `json:"host"`

	// Backends is the list of upstreams Caddy round-robins across. Each
	// is a "host:port" string — Caddy resolves DNS at request time.
	Backends []string `json:"backends"`

	// TLS controls cert acquisition.
	//   - "auto"     — Caddy mints a cert via ACME on demand (default).
	//   - "off"      — HTTP-only (port 80), no automatic redirect to 443.
	//   - "internal" — Caddy's self-signed CA, useful for *.internal
	//                  hostnames where ACME can't reach the validation
	//                  endpoint.
	TLS string `json:"tls,omitempty"`

	// Match is an optional path prefix filter. Empty = match all paths.
	Match string `json:"match,omitempty"`

	// Headers is an optional set of request-header rewrites Caddy
	// applies before forwarding. Map order is irrelevant — we sort it
	// at JSON-render time so the rendered Caddy config is stable across
	// agent restarts (otherwise Caddy reloads pointlessly on every
	// agent restart even when nothing changed).
	Headers map[string]string `json:"headers,omitempty"`
}

// Routes is the ordered slice of Route values an agent is responsible for.
// Order is preserved end-to-end (etcd → JSON → Caddy) because Caddy
// evaluates routes in declaration order — operators occasionally rely on
// that for specificity (`*.staging.example.com` before `*.example.com`).
type Routes []Route

// renderCaddyConfig converts a Routes slice into the JSON Caddy expects on
// the admin /load endpoint. Delegates to renderCaddyConfigWith with no
// storage override — keeps the simpler signature for the bootstrap path
// and existing tests.
func (rs Routes) renderCaddyConfig(adminSocket string) ([]byte, error) {
	return rs.renderCaddyConfigWith(adminSocket, nil)
}

// renderCaddyConfigWith is the parameterised renderer used by the supervisor
// when the operator opts into a shared certificate store (etcd). `storage`
// becomes the top-level `storage` field Caddy reads at startup; nil means
// filesystem default (Caddy's $XDG_DATA_HOME/caddy).
//
// The shape we emit is the minimal route table Caddy accepts:
//
//	{
//	  "admin": { "listen": "unix//.../caddy-admin.sock" },
//	  "apps": {
//	    "http": {
//	      "servers": {
//	        "weft": {
//	          "listen": [":80", ":443"],
//	          "routes": [ …per Route… ],
//	          "automatic_https": { "disable": false }
//	        }
//	      }
//	    },
//	    "tls": { "automation": { "policies": [ …per non-default TLS… ] } }
//	  }
//	}
//
// We render the `tls` block only when at least one route opts out of
// auto-ACME — keeping the config blob small when nothing exotic is in play.
func (rs Routes) renderCaddyConfigWith(adminSocket string, storage map[string]any) ([]byte, error) {
	type caddyRoute struct {
		Match  []map[string]any `json:"match,omitempty"`
		Handle []map[string]any `json:"handle"`
	}
	type caddyServer struct {
		Listen          []string     `json:"listen"`
		Routes          []caddyRoute `json:"routes"`
		AutomaticHTTPS  map[string]any `json:"automatic_https,omitempty"`
	}
	type caddyAdmin struct {
		Listen string `json:"listen"`
	}
	type caddyHTTP struct {
		Servers map[string]caddyServer `json:"servers"`
	}
	type caddyTLSPolicy struct {
		Subjects []string         `json:"subjects,omitempty"`
		Issuers  []map[string]any `json:"issuers,omitempty"`
	}
	type caddyTLS struct {
		Automation map[string]any `json:"automation,omitempty"`
	}
	type caddyApps struct {
		HTTP caddyHTTP `json:"http"`
		TLS  *caddyTLS `json:"tls,omitempty"`
	}
	type caddyConfig struct {
		Admin   caddyAdmin     `json:"admin"`
		Storage map[string]any `json:"storage,omitempty"`
		Apps    caddyApps      `json:"apps"`
	}

	var routes []caddyRoute
	var policies []caddyTLSPolicy
	for _, r := range rs {
		if r.Host == "" || len(r.Backends) == 0 {
			return nil, fmt.Errorf("route requires Host + Backends (got %+v)", r)
		}
		match := []map[string]any{{"host": []string{r.Host}}}
		if r.Match != "" {
			match[0]["path"] = []string{r.Match}
		}
		// reverse_proxy handler — Caddy expects an "upstreams" array
		// with {dial: "host:port"} entries.
		upstreams := make([]map[string]any, 0, len(r.Backends))
		for _, b := range r.Backends {
			upstreams = append(upstreams, map[string]any{"dial": b})
		}
		handlers := []map[string]any{}
		// Optional header_request handler — runs before reverse_proxy.
		if len(r.Headers) > 0 {
			set := make(map[string][]string, len(r.Headers))
			keys := make([]string, 0, len(r.Headers))
			for k := range r.Headers {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				set[k] = []string{r.Headers[k]}
			}
			handlers = append(handlers, map[string]any{
				"handler": "headers",
				"request": map[string]any{"set": set},
			})
		}
		handlers = append(handlers, map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": upstreams,
		})
		routes = append(routes, caddyRoute{Match: match, Handle: handlers})

		switch r.TLS {
		case "", "auto":
			// default ACME — no explicit policy needed.
		case "off":
			// route stays HTTP-only; auto-HTTPS sees the
			// "disable" flag below.
		case "internal":
			policies = append(policies, caddyTLSPolicy{
				Subjects: []string{r.Host},
				Issuers:  []map[string]any{{"module": "internal"}},
			})
		default:
			return nil, fmt.Errorf("route %q: unknown TLS mode %q (want auto|off|internal)", r.Host, r.TLS)
		}
	}

	cfg := caddyConfig{
		Admin:   caddyAdmin{Listen: adminSocket},
		Storage: storage,
		Apps: caddyApps{
			HTTP: caddyHTTP{
				Servers: map[string]caddyServer{
					"weft": {
						Listen: []string{":80", ":443"},
						Routes: routes,
						// Disable Caddy's "redirect HTTP→HTTPS" only when
						// at least one route opts out of TLS; otherwise
						// the default "on" matches weft's expectations.
						AutomaticHTTPS: map[string]any{"disable": anyTLSOff(rs)},
					},
				},
			},
		},
	}
	if len(policies) > 0 {
		cfg.Apps.TLS = &caddyTLS{Automation: map[string]any{"policies": policies}}
	}
	return json.Marshal(cfg)
}

// anyTLSOff returns true when at least one route has TLS="off". Used to
// flip Caddy's global automatic-https.disable, since per-route opt-out
// in the route-level matcher is messier than a server-level toggle.
func anyTLSOff(rs Routes) bool {
	for _, r := range rs {
		if r.TLS == "off" {
			return true
		}
	}
	return false
}
