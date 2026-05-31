// agent_config.go declares the `agent_config { ... }` block operators put in
// cluster.hcl — both at the cluster level (defaults applied to every host) and
// per-host (overrides). It also renders the merged result back into HCL text
// that weft up pushes to /etc/weft/weft.hcl on each host, so operators don't
// have to hand-edit the file on every node.
//
// IMPORTANT — duplication note
//
// The struct hierarchy below intentionally mirrors fileConfig in
// cmd/weft/config.go, because the HCL we produce here is decoded by exactly
// that loader on the host. The two shapes must stay in sync ; the
// agent_config_test.go round-trip (RenderHCL → hclsimple.Decode → equal)
// guards the wire-level contract. Re-declaring rather than importing avoids
// a dependency from the `cluster` package into `cmd/weft` (the binary), and
// lets the cluster planner stay pure-schema.
package cluster

import (
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// AgentConfigBlock mirrors cmd/weft/config.go's fileConfig. Pointers
// distinguish "not present in HCL" from "present and zero" so per-host
// overlays only replace cluster-level defaults when the host explicitly sets
// the field (same precedence rule as in cmd/weft/config.go).
type AgentConfigBlock struct {
	Socket            *string                  `hcl:"socket,optional"`
	SSHSocket         *string                  `hcl:"ssh_socket,optional"`
	SSHAuthorizedKeys *string                  `hcl:"ssh_authorized_keys,optional"`
	ConfigDir         *string                  `hcl:"config_dir,optional"`
	MetricsListen     *string                  `hcl:"metrics_listen,optional"`
	OIDC              *AgentOIDCBlock          `hcl:"oidc,block"`
	Storage           *AgentStorageBlock       `hcl:"storage,block"`
	EventBus          *AgentEventBusBlock      `hcl:"event_bus,block"`
	NATSAuthorization *AgentNATSAuthzBlock     `hcl:"nats_authorization,block"`
	Proxy             *AgentProxyBlock         `hcl:"proxy,block"`
}

// AgentOIDCBlock mirrors oidcBlock.
type AgentOIDCBlock struct {
	Issuer            string `hcl:"issuer,optional"`
	ClientID          string `hcl:"client_id,optional"`
	SkipClientIDCheck bool   `hcl:"skip_client_id_check,optional"`
}

// AgentStorageBlock mirrors storageBlock.
type AgentStorageBlock struct {
	Backend string          `hcl:"backend,optional"`
	Etcd    *AgentEtcdBlock `hcl:"etcd,block"`
}

// AgentEtcdBlock mirrors etcdBlock.
type AgentEtcdBlock struct {
	Endpoints []string `hcl:"endpoints"`
	Username  string   `hcl:"username,optional"`
	Password  string   `hcl:"password,optional"`
	KeyPrefix string   `hcl:"key_prefix,optional"`
}

// AgentEventBusBlock mirrors eventBusBlock.
type AgentEventBusBlock struct {
	Backend string          `hcl:"backend,optional"`
	NATS    *AgentNATSBlock `hcl:"nats,block"`
}

// AgentNATSBlock mirrors natsBlock.
type AgentNATSBlock struct {
	URL             string `hcl:"url"`
	CredentialsFile string `hcl:"credentials_file,optional"`
	Name            string `hcl:"name,optional"`
	SubjectPrefix   string `hcl:"subject_prefix,optional"`
}

// AgentNATSAuthzBlock mirrors natsAuthorizationBlock.
type AgentNATSAuthzBlock struct {
	Path        string `hcl:"path"`
	AdminPubkey string `hcl:"admin_pubkey,optional"`
}

// AgentProxyBlock mirrors proxyBlock.
type AgentProxyBlock struct {
	Enabled     *bool                   `hcl:"enabled,optional"`
	CaddyBinary *string                 `hcl:"caddy_binary,optional"`
	StateDir    *string                 `hcl:"state_dir,optional"`
	KeyPrefix   *string                 `hcl:"key_prefix,optional"`
	Storage     *AgentProxyStorageBlock `hcl:"storage,block"`
}

// AgentProxyStorageBlock mirrors proxyStorageBlock.
type AgentProxyStorageBlock struct {
	Endpoints []string `hcl:"endpoints,optional"`
}

// AgentConfigFor returns the effective agent config to push to host h: the
// cluster-level defaults overlaid with the host's own overrides. Per-field
// overlay rule — a non-nil pointer / non-zero scalar on the host replaces the
// cluster default ; absent values fall through. Returns a zero block when
// neither side declares any agent_config (caller can decide to skip the push).
func (c *Cluster) AgentConfigFor(h *Host) AgentConfigBlock {
	var base AgentConfigBlock
	if c.AgentConfig != nil {
		base = *c.AgentConfig
	}
	if h == nil || h.AgentConfig == nil {
		return base
	}
	o := h.AgentConfig
	if o.Socket != nil {
		base.Socket = o.Socket
	}
	if o.SSHSocket != nil {
		base.SSHSocket = o.SSHSocket
	}
	if o.SSHAuthorizedKeys != nil {
		base.SSHAuthorizedKeys = o.SSHAuthorizedKeys
	}
	if o.ConfigDir != nil {
		base.ConfigDir = o.ConfigDir
	}
	if o.MetricsListen != nil {
		base.MetricsListen = o.MetricsListen
	}
	if o.OIDC != nil {
		base.OIDC = o.OIDC
	}
	if o.Storage != nil {
		base.Storage = o.Storage
	}
	if o.EventBus != nil {
		base.EventBus = o.EventBus
	}
	if o.NATSAuthorization != nil {
		base.NATSAuthorization = o.NATSAuthorization
	}
	if o.Proxy != nil {
		base.Proxy = o.Proxy
	}
	return base
}

// IsEmpty reports whether the block has nothing to render. weft up uses this
// to skip the PushAgentConfig action when the operator didn't declare any
// agent_config at all — otherwise we'd push an empty file and clobber the
// skeleton cloud-init dropped.
func (a AgentConfigBlock) IsEmpty() bool {
	return a.Socket == nil &&
		a.SSHSocket == nil &&
		a.SSHAuthorizedKeys == nil &&
		a.ConfigDir == nil &&
		a.MetricsListen == nil &&
		a.OIDC == nil &&
		a.Storage == nil &&
		a.EventBus == nil &&
		a.NATSAuthorization == nil &&
		a.Proxy == nil
}

// RenderHCL serialises the block back to the HCL text shape weft.hcl's
// loader (cmd/weft/config.go) accepts. Uses hclwrite so escaping +
// quoting are handled by the HashiCorp library, not by fmt.Sprintf.
func (a AgentConfigBlock) RenderHCL() string {
	f := hclwrite.NewEmptyFile()
	body := f.Body()

	if a.Socket != nil {
		body.SetAttributeValue("socket", cty.StringVal(*a.Socket))
	}
	if a.SSHSocket != nil {
		body.SetAttributeValue("ssh_socket", cty.StringVal(*a.SSHSocket))
	}
	if a.SSHAuthorizedKeys != nil {
		body.SetAttributeValue("ssh_authorized_keys", cty.StringVal(*a.SSHAuthorizedKeys))
	}
	if a.ConfigDir != nil {
		body.SetAttributeValue("config_dir", cty.StringVal(*a.ConfigDir))
	}
	if a.MetricsListen != nil {
		body.SetAttributeValue("metrics_listen", cty.StringVal(*a.MetricsListen))
	}

	if a.OIDC != nil {
		b := body.AppendNewBlock("oidc", nil).Body()
		if a.OIDC.Issuer != "" {
			b.SetAttributeValue("issuer", cty.StringVal(a.OIDC.Issuer))
		}
		if a.OIDC.ClientID != "" {
			b.SetAttributeValue("client_id", cty.StringVal(a.OIDC.ClientID))
		}
		if a.OIDC.SkipClientIDCheck {
			b.SetAttributeValue("skip_client_id_check", cty.BoolVal(true))
		}
	}

	if a.Storage != nil {
		b := body.AppendNewBlock("storage", nil).Body()
		if a.Storage.Backend != "" {
			b.SetAttributeValue("backend", cty.StringVal(a.Storage.Backend))
		}
		if a.Storage.Etcd != nil {
			eb := b.AppendNewBlock("etcd", nil).Body()
			// `endpoints` is required by the etcdBlock schema (no
			// optional tag), so always emit it — even when empty,
			// since a missing required attribute would fail decoding.
			eb.SetAttributeValue("endpoints", stringListVal(a.Storage.Etcd.Endpoints))
			if a.Storage.Etcd.Username != "" {
				eb.SetAttributeValue("username", cty.StringVal(a.Storage.Etcd.Username))
			}
			if a.Storage.Etcd.Password != "" {
				eb.SetAttributeValue("password", cty.StringVal(a.Storage.Etcd.Password))
			}
			if a.Storage.Etcd.KeyPrefix != "" {
				eb.SetAttributeValue("key_prefix", cty.StringVal(a.Storage.Etcd.KeyPrefix))
			}
		}
	}

	if a.EventBus != nil {
		b := body.AppendNewBlock("event_bus", nil).Body()
		if a.EventBus.Backend != "" {
			b.SetAttributeValue("backend", cty.StringVal(a.EventBus.Backend))
		}
		if a.EventBus.NATS != nil {
			nb := b.AppendNewBlock("nats", nil).Body()
			// `url` is required by natsBlock — always emit even if empty.
			nb.SetAttributeValue("url", cty.StringVal(a.EventBus.NATS.URL))
			if a.EventBus.NATS.CredentialsFile != "" {
				nb.SetAttributeValue("credentials_file", cty.StringVal(a.EventBus.NATS.CredentialsFile))
			}
			if a.EventBus.NATS.Name != "" {
				nb.SetAttributeValue("name", cty.StringVal(a.EventBus.NATS.Name))
			}
			if a.EventBus.NATS.SubjectPrefix != "" {
				nb.SetAttributeValue("subject_prefix", cty.StringVal(a.EventBus.NATS.SubjectPrefix))
			}
		}
	}

	if a.NATSAuthorization != nil {
		b := body.AppendNewBlock("nats_authorization", nil).Body()
		// `path` is required by natsAuthorizationBlock — always emit.
		b.SetAttributeValue("path", cty.StringVal(a.NATSAuthorization.Path))
		if a.NATSAuthorization.AdminPubkey != "" {
			b.SetAttributeValue("admin_pubkey", cty.StringVal(a.NATSAuthorization.AdminPubkey))
		}
	}

	if a.Proxy != nil {
		b := body.AppendNewBlock("proxy", nil).Body()
		if a.Proxy.Enabled != nil {
			b.SetAttributeValue("enabled", cty.BoolVal(*a.Proxy.Enabled))
		}
		if a.Proxy.CaddyBinary != nil {
			b.SetAttributeValue("caddy_binary", cty.StringVal(*a.Proxy.CaddyBinary))
		}
		if a.Proxy.StateDir != nil {
			b.SetAttributeValue("state_dir", cty.StringVal(*a.Proxy.StateDir))
		}
		if a.Proxy.KeyPrefix != nil {
			b.SetAttributeValue("key_prefix", cty.StringVal(*a.Proxy.KeyPrefix))
		}
		if a.Proxy.Storage != nil {
			sb := b.AppendNewBlock("storage", nil).Body()
			sb.SetAttributeValue("endpoints", stringListVal(a.Proxy.Storage.Endpoints))
		}
	}

	return string(f.Bytes())
}

// stringListVal converts a Go []string to a cty list — emitting an empty
// tuple value when the slice is nil/empty, since cty.ListVal panics on
// zero-length input.
func stringListVal(xs []string) cty.Value {
	if len(xs) == 0 {
		return cty.ListValEmpty(cty.String)
	}
	out := make([]cty.Value, len(xs))
	for i, x := range xs {
		out[i] = cty.StringVal(x)
	}
	return cty.ListVal(out)
}

