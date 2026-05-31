package cluster

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

// ptr is a tiny helper for the *T fields in AgentConfigBlock. The pointer
// gymnastics are unavoidable because hcl's "missing vs zero" distinction
// flows through *string / *bool everywhere in cmd/weft/config.go.
func ptr[T any](v T) *T { return &v }

// fullBlock exercises every field RenderHCL knows about — the round-trip
// guard against a typo dropping one of the *Block sub-fields silently.
func fullBlock() AgentConfigBlock {
	return AgentConfigBlock{
		Socket:            ptr("/var/run/weft/weft.sock"),
		SSHSocket:         ptr("/var/run/weft/weft-ssh.sock"),
		SSHAuthorizedKeys: ptr("/etc/weft/authorized_keys"),
		ConfigDir:         ptr("/var/lib/weft/hcl"),
		MetricsListen:     ptr(":9101"),
		OIDC: &AgentOIDCBlock{
			Issuer:            "https://dex.internal.example.com",
			ClientID:          "weft",
			SkipClientIDCheck: true,
		},
		Storage: &AgentStorageBlock{
			Backend: "etcd",
			Etcd: &AgentEtcdBlock{
				Endpoints: []string{"http://10.0.0.11:2379", "http://10.0.0.12:2379", "http://10.0.0.13:2379"},
				Username:  "weft",
				Password:  "s3cret",
				KeyPrefix: "weft/",
			},
		},
		EventBus: &AgentEventBusBlock{
			Backend: "nats",
			NATS: &AgentNATSBlock{
				URL:             "nats://10.0.0.11:4222",
				CredentialsFile: "/etc/weft/nats.creds",
				Name:            "weft-agent",
				SubjectPrefix:   "weft",
			},
		},
		NATSAuthorization: &AgentNATSAuthzBlock{
			Path:        "/etc/weft/nats-authz.conf",
			AdminPubkey: "UABCDEF",
		},
		Proxy: &AgentProxyBlock{
			Enabled:     ptr(true),
			CaddyBinary: ptr("/usr/local/bin/weft-proxy"),
			StateDir:    ptr("/var/lib/weft-agent/proxy"),
			KeyPrefix:   ptr("caddy/"),
			Storage: &AgentProxyStorageBlock{
				Endpoints: []string{"http://10.0.0.11:2379", "http://10.0.0.12:2379", "http://10.0.0.13:2379"},
			},
		},
	}
}

// TestRenderHCL_Roundtrip writes the rendered HCL to a temp file, decodes it
// back with hclsimple (same decoder cmd/weft/config.go uses), and compares
// the result. This is the wire-level contract that the in-cluster
// AgentConfigBlock and cmd/weft's fileConfig must keep aligned.
func TestRenderHCL_Roundtrip(t *testing.T) {
	src := fullBlock()
	rendered := src.RenderHCL()

	dir := t.TempDir()
	p := filepath.Join(dir, "weft.hcl")
	if err := os.WriteFile(p, []byte(rendered), 0o600); err != nil {
		t.Fatalf("write tmp hcl: %v", err)
	}

	var got AgentConfigBlock
	if err := hclsimple.DecodeFile(p, nil, &got); err != nil {
		t.Fatalf("decode rendered hcl: %v\n---\n%s", err, rendered)
	}
	if !reflect.DeepEqual(src, got) {
		t.Errorf("roundtrip mismatch.\nwant: %+v\ngot:  %+v\nrendered:\n%s", src, got, rendered)
	}
}

// TestRenderHCL_ShapeSpotCheck pins the prose shape — surface-level so a
// future hclwrite version that re-formats slightly won't silently change
// what operators see in /etc/weft/weft.hcl.
func TestRenderHCL_ShapeSpotCheck(t *testing.T) {
	b := AgentConfigBlock{
		Socket: ptr("/var/run/weft/weft.sock"),
		Proxy: &AgentProxyBlock{
			Enabled:     ptr(true),
			CaddyBinary: ptr("/usr/local/bin/weft-proxy"),
			Storage: &AgentProxyStorageBlock{
				Endpoints: []string{"http://10.0.0.11:2379"},
			},
		},
	}
	out := b.RenderHCL()
	// hclwrite aligns `=` within a block (e.g. `enabled      = true`),
	// so spacing isn't exact — collapse runs of whitespace to one space
	// before substring checks.
	collapsed := strings.Join(strings.Fields(out), " ")
	for _, frag := range []string{
		`socket = "/var/run/weft/weft.sock"`,
		"proxy {",
		"enabled = true",
		`caddy_binary = "/usr/local/bin/weft-proxy"`,
		"storage {",
		`endpoints = ["http://10.0.0.11:2379"]`,
	} {
		if !strings.Contains(collapsed, frag) {
			t.Errorf("rendered HCL missing %q:\n%s", frag, out)
		}
	}
}

// TestAgentConfigFor_NoBlocks: when neither cluster nor host declares
// agent_config, the result is empty and IsEmpty reports true — so Build
// can skip the PushAgentConfig action entirely.
func TestAgentConfigFor_NoBlocks(t *testing.T) {
	c := &Cluster{Hosts: []Host{{ID: "h1"}}}
	got := c.AgentConfigFor(&c.Hosts[0])
	if !got.IsEmpty() {
		t.Errorf("AgentConfigFor with no blocks = %+v, want empty", got)
	}
}

// TestAgentConfigFor_HostOverridesClusterPerField: per-host non-nil fields
// replace the cluster default ; absent fields fall through.
func TestAgentConfigFor_HostOverridesClusterPerField(t *testing.T) {
	c := &Cluster{
		AgentConfig: &AgentConfigBlock{
			Socket:        ptr("/cluster/sock"),
			MetricsListen: ptr(":9101"),
			Proxy: &AgentProxyBlock{
				Enabled:     ptr(true),
				CaddyBinary: ptr("/cluster/caddy"),
			},
		},
		Hosts: []Host{{
			ID: "h1",
			AgentConfig: &AgentConfigBlock{
				Socket: ptr("/host/sock"), // override
				// MetricsListen absent → cluster default falls through.
				Proxy: &AgentProxyBlock{ // whole block replaces cluster's
					Enabled:     ptr(false),
					CaddyBinary: ptr("/host/caddy"),
				},
			},
		}},
	}
	got := c.AgentConfigFor(&c.Hosts[0])
	if got.Socket == nil || *got.Socket != "/host/sock" {
		t.Errorf("socket override lost: %v", got.Socket)
	}
	if got.MetricsListen == nil || *got.MetricsListen != ":9101" {
		t.Errorf("metrics_listen fall-through lost: %v", got.MetricsListen)
	}
	if got.Proxy == nil || got.Proxy.Enabled == nil || *got.Proxy.Enabled {
		t.Errorf("proxy.enabled override lost: %+v", got.Proxy)
	}
	if got.Proxy == nil || got.Proxy.CaddyBinary == nil || *got.Proxy.CaddyBinary != "/host/caddy" {
		t.Errorf("proxy.caddy_binary override lost: %+v", got.Proxy)
	}
}

// TestIsEmpty distinguishes a real config from the zero value — Build keys
// off this to skip the PushAgentConfig action when nothing's declared.
func TestIsEmpty(t *testing.T) {
	if !(AgentConfigBlock{}).IsEmpty() {
		t.Error("zero AgentConfigBlock should be IsEmpty()")
	}
	if (AgentConfigBlock{Socket: ptr("/x")}).IsEmpty() {
		t.Error("non-zero socket → !IsEmpty")
	}
	if (AgentConfigBlock{Proxy: &AgentProxyBlock{}}).IsEmpty() {
		t.Error("non-nil proxy block → !IsEmpty")
	}
}
