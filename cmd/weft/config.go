package main

// config.go loads an optional HCL config file that lets operators
// configure weft settings without long --flag lists. Per
// [[weft-hcl-config]]:
//
//   * --config <path> overrides the discovery order.
//   * default discovery: /etc/weft/weft.hcl, then ~/.config/weft/weft.hcl.
//   * CLI flags always win over the file (operator emergency knob).
//
// Schema (HCL):
//
//   socket               = "~/.weft/weft.sock"
//   ssh_socket           = "~/.weft/weft-ssh.sock"
//   ssh_authorized_keys  = "~/.weft/authorized_keys"
//   config_dir           = ".mock/hcl"
//
//   oidc {
//     issuer    = "https://dex.internal.example.com"
//     client_id = "weft"
//   }
//
// Storage / etcd / other blocks land here as they're implemented;
// the decoder is strict so a typo errors out instead of silently
// drifting (HCL's `Remain` is not used).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2/hclsimple"
)

// fileConfig is the decoded shape of weft.hcl. Pointers (*string)
// distinguish "not present in HCL" from "present and empty",
// which matters for the flag-overlay precedence rule: only
// non-nil HCL values pre-fill flag defaults.
type fileConfig struct {
	Socket             *string                 `hcl:"socket,optional"`
	SSHSocket          *string                 `hcl:"ssh_socket,optional"`
	SSHAuthorizedKeys  *string                 `hcl:"ssh_authorized_keys,optional"`
	ConfigDir          *string                 `hcl:"config_dir,optional"`
	OIDC               *oidcBlock              `hcl:"oidc,block"`
	Storage            *storageBlock           `hcl:"storage,block"`
	EventBus           *eventBusBlock          `hcl:"event_bus,block"`
	NATSAuthorization  *natsAuthorizationBlock `hcl:"nats_authorization,block"`
	Proxy              *proxyBlock             `hcl:"proxy,block"`
}

// proxyBlock mirrors the `proxy { ... }` HCL block — the operator-
// preferred way to enable the reverse-proxy plane (Caddy supervisor
// + etcd watcher). Mirror of cmd/weft/proxy.go's proxyOpts + the
// --proxy-* CLI flags; precedence is CLI > env > HCL > default
// (same rule as every other fileConfig field).
//
// Minimum viable opt-in:
//
//	proxy {
//	  enabled      = true
//	  caddy_binary = "/usr/local/bin/weft-proxy"
//	}
//
// Shared cert storage across hosts (recommended for 3-DC clusters
// — avoids each host hammering Let's Encrypt rate-limit on its own
// first reload):
//
//	proxy {
//	  enabled      = true
//	  caddy_binary = "/usr/local/bin/weft-proxy"
//	  state_dir    = "/var/lib/weft-agent/proxy"
//
//	  storage {
//	    endpoints = [
//	      "http://10.0.0.11:2379",
//	      "http://10.0.0.12:2379",
//	      "http://10.0.0.13:2379",
//	    ]
//	  }
//	}
type proxyBlock struct {
	Enabled     *bool              `hcl:"enabled,optional"`
	CaddyBinary *string            `hcl:"caddy_binary,optional"`
	StateDir    *string            `hcl:"state_dir,optional"`
	KeyPrefix   *string            `hcl:"key_prefix,optional"`
	Storage     *proxyStorageBlock `hcl:"storage,block"`
}

// proxyStorageBlock mirrors the proxy { storage { } } sub-block.
// Today only the etcd backend has knobs worth surfacing; the field
// shape leaves room for future backends (redis, S3) without breaking
// existing configs.
//
// When `endpoints` is set, weft-agent emits a Caddy storage block
// selecting the darkweak etcd adapter (compiled into weft-proxy).
// When empty, Caddy uses filesystem storage rooted under
// proxy.state_dir — fine for single-host dev, suboptimal in HA.
type proxyStorageBlock struct {
	Endpoints []string `hcl:"endpoints,optional"`
}

// oidcBlock mirrors the oidc { } HCL block. Empty issuer means
// "OIDC disabled" — weft stays in dev mode. The validator
// construction in weft/auth.go handles the empty case explicitly.
type oidcBlock struct {
	Issuer            string `hcl:"issuer,optional"`
	ClientID          string `hcl:"client_id,optional"`
	SkipClientIDCheck bool   `hcl:"skip_client_id_check,optional"`
}

// storageBlock mirrors the storage { } HCL block. Decides which
// Storage backend the registry layer (projects, users, networks,
// volumes, …) goes through. See pkg/openweft/weft/storage.go for
// the interface and the three implementations.
//
//   backend = "file" (default)
//     Atomic-rename on local disk under <vmsDir>/.<name>.hcl.
//     Dev / single-host. No external dependency.
//
//   backend = "etcd"
//     3-DC etcd cluster per [[etcd-control-plane]]. Production.
//     Requires the `etcd { ... }` sub-block. Currently returns
//     ErrEtcdNotWired at runtime — the etcd client integration
//     is the next concrete step. Wire-up shape is committed so
//     operators can prepare configs ahead of time.
type storageBlock struct {
	Backend string     `hcl:"backend,optional"` // "file" | "etcd"
	Etcd    *etcdBlock `hcl:"etcd,block"`
}

// etcdBlock mirrors the storage { etcd { ... } } sub-block. Used
// only when Backend = "etcd". Matches the shape of EtcdConfig
// in pkg/openweft/weft/storage.go.
type etcdBlock struct {
	Endpoints []string `hcl:"endpoints"`
	Username  string   `hcl:"username,optional"`
	Password  string   `hcl:"password,optional"`
	KeyPrefix string   `hcl:"key_prefix,optional"`
}

// eventBusBlock mirrors the `event_bus { ... }` HCL block. Per
// [[weft-event-bus-nats]] the two backends are:
//
//   backend = "local" (default)
//     In-process LocalEventBus. No external dep at runtime.
//
//   backend = "nats"
//     Connect to a NATS cluster (per [[infra-in-micro-vms]] this
//     is itself a weft-managed micro-VM in production). Requires
//     the `nats { url = "..." }` sub-block.
type eventBusBlock struct {
	Backend string    `hcl:"backend,optional"` // "local" | "nats"
	NATS    *natsBlock `hcl:"nats,block"`
}

// natsBlock mirrors the event_bus { nats { ... } } sub-block.
// Maps onto weft.NATSConfig.
type natsBlock struct {
	URL             string `hcl:"url"`
	CredentialsFile string `hcl:"credentials_file,optional"`
	Name            string `hcl:"name,optional"`
	SubjectPrefix   string `hcl:"subject_prefix,optional"`
}

// natsAuthorizationBlock mirrors the `nats_authorization { ... }`
// HCL block. Turns on auto-render of the NATS authorization
// block (per [[weft-tenant-event-access]] Phase-5 follow-up):
// weft re-renders the per-project nkey allow-list on every
// mutation that affects it and writes the file atomically. Omit
// the block for operator-driven setups — `weft admin nats-authz`
// stays callable.
//
//   path         path to write (mode 0600). Tilde-expansion supported.
//   admin_pubkey optional NATS user-NKey public key ("U…") of weft
//                itself, granted full pub/sub on weft.>. Leave empty
//                in dev / single-host where weft publishes anonymously.
type natsAuthorizationBlock struct {
	Path        string `hcl:"path"`
	AdminPubkey string `hcl:"admin_pubkey,optional"`
}

// loadFileConfig discovers a weft.hcl file and returns its
// decoded form (or zero value when none is present). Search
// order:
//
//   1. explicit --config <path> (if non-empty)
//   2. /etc/weft/weft.hcl
//   3. $HOME/.config/weft/weft.hcl
//
// Missing-file at the default locations is not an error: the zero
// value just means "use flag defaults". An explicit --config that
// points at a missing file IS an error (operator typo).
func loadFileConfig(explicit string) (fileConfig, string, error) {
	var paths []string
	if explicit != "" {
		paths = []string{explicit}
	} else {
		home, _ := os.UserHomeDir()
		paths = []string{
			"/etc/weft/weft.hcl",
			filepath.Join(home, ".config", "weft", "weft.hcl"),
		}
	}
	for i, p := range paths {
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) && explicit == "" {
				continue // try the next default location
			}
			if explicit != "" && i == 0 {
				return fileConfig{}, "", fmt.Errorf("stat config %s: %w", p, err)
			}
			continue
		}
		var c fileConfig
		if err := hclsimple.DecodeFile(p, nil, &c); err != nil {
			return fileConfig{}, "", fmt.Errorf("decode config %s: %w", p, err)
		}
		return c, p, nil
	}
	return fileConfig{}, "", nil
}

// expandHome rewrites a leading "~/" to the user's home dir. HCL
// authors expect tildes to work in path values; the os/exec
// pipeline does not expand them.
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") && p != "~" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	return filepath.Join(home, p[2:])
}

// applyFileConfigDefaults overlays the file's values onto the
// caller-supplied destinations *only when the corresponding HCL
// attribute was set*. This preserves the precedence rule: the
// caller has already initialised these with flag defaults (or
// real flag values when the operator passed them on the command
// line), so the file only fills gaps.
//
// The OIDCConfig is returned separately because it doesn't have
// a flag counterpart yet — the only way to set it today is the
// HCL file.
func applyFileConfigDefaults(c fileConfig, dst *fileConfigTargets) {
	if c.Socket != nil {
		dst.socket = expandHome(*c.Socket)
	}
	if c.SSHSocket != nil {
		dst.sshSocket = expandHome(*c.SSHSocket)
	}
	if c.SSHAuthorizedKeys != nil {
		dst.sshAuthorizedKeys = expandHome(*c.SSHAuthorizedKeys)
	}
	if c.ConfigDir != nil {
		dst.configDir = expandHome(*c.ConfigDir)
	}
	if c.OIDC != nil {
		dst.oidcIssuer = c.OIDC.Issuer
		dst.oidcClientID = c.OIDC.ClientID
		dst.oidcSkipClientIDCheck = c.OIDC.SkipClientIDCheck
	}
	if c.Storage != nil {
		if c.Storage.Backend != "" {
			dst.storageBackend = c.Storage.Backend
		}
		if c.Storage.Etcd != nil {
			dst.etcdEndpoints = append([]string(nil), c.Storage.Etcd.Endpoints...)
			dst.etcdUsername = c.Storage.Etcd.Username
			dst.etcdPassword = c.Storage.Etcd.Password
			dst.etcdKeyPrefix = c.Storage.Etcd.KeyPrefix
		}
	}
	if c.EventBus != nil {
		if c.EventBus.Backend != "" {
			dst.eventBusBackend = c.EventBus.Backend
		}
		if c.EventBus.NATS != nil {
			dst.natsURL = c.EventBus.NATS.URL
			dst.natsCredentialsFile = expandHome(c.EventBus.NATS.CredentialsFile)
			dst.natsName = c.EventBus.NATS.Name
			dst.natsSubjectPrefix = c.EventBus.NATS.SubjectPrefix
		}
	}
	if c.NATSAuthorization != nil {
		dst.natsAuthzPath = expandHome(c.NATSAuthorization.Path)
		dst.natsAuthzAdminPubkey = c.NATSAuthorization.AdminPubkey
	}
	if c.Proxy != nil {
		// Each field is overlaid only when the HCL attribute was
		// set — pointers distinguish "not present" from "present
		// and zero" so an explicit `enabled = false` still flips
		// an env-driven default off, but a missing line doesn't.
		if c.Proxy.Enabled != nil {
			dst.proxyEnabled = *c.Proxy.Enabled
		}
		if c.Proxy.StateDir != nil {
			dst.proxyStateDir = expandHome(*c.Proxy.StateDir)
		}
		if c.Proxy.CaddyBinary != nil {
			dst.proxyCaddyBinary = expandHome(*c.Proxy.CaddyBinary)
		}
		if c.Proxy.KeyPrefix != nil {
			dst.proxyKeyPrefix = *c.Proxy.KeyPrefix
		}
		if c.Proxy.Storage != nil && len(c.Proxy.Storage.Endpoints) > 0 {
			dst.proxyStorageEndpoints = append([]string(nil), c.Proxy.Storage.Endpoints...)
		}
	}
}

// fileConfigTargets bundles the destinations applyFileConfigDefaults
// writes into. Keeping them in a struct avoids the 7-pointer
// function signature that would otherwise pollute main.go.
type fileConfigTargets struct {
	socket                string
	sshSocket             string
	sshAuthorizedKeys     string
	tcpListen             string // dev-mode plain-TCP gRPC listener (cross-host bring-up); empty = disabled
	configDir             string
	oidcIssuer            string
	oidcClientID          string
	oidcSkipClientIDCheck bool

	// Storage backend selection — see storageBlock in this file
	// and the Storage interface in pkg/openweft/weft/storage.go.
	storageBackend string   // "" → "file" (default); "etcd" for prod
	etcdEndpoints  []string // only consulted when storageBackend == "etcd"
	etcdUsername   string
	etcdPassword   string
	etcdKeyPrefix  string

	// Event-bus backend selection — see eventBusBlock in this
	// file and the EventBus interface in pkg/openweft/weft/
	// eventbus.go.
	eventBusBackend     string // "" → "local" (default); "nats" for prod
	natsURL             string // only consulted when eventBusBackend == "nats"
	natsCredentialsFile string
	natsName            string
	natsSubjectPrefix   string

	// NATS authorization auto-render. Empty path disables; see
	// natsAuthorizationBlock + Adapter.SetNATSAuthorizationFile.
	natsAuthzPath        string
	natsAuthzAdminPubkey string

	// Consul-style role flags. The default no-flag mode is
	// single-host all-in-one (server-in-process + local driver
	// dispatch). --server pins to control-plane-only ; --client
	// to per-host-only. Today both are recorded but not
	// strictly enforced — full client-only mode waits on the
	// per-host gRPC ControlPlane stub (see weft/agent).
	serverMode      bool
	clientMode      bool
	controlPlaneURL string

	// Reverse-proxy plane (Caddy supervised subprocess + etcd
	// Watcher). Opt-in via --proxy ; off by default so the agent
	// stays a single-process daemon for operators that don't need
	// L7 ingress. See proxy.go (bootProxy) and agent/proxy/.
	//
	// Only honoured by the all-in-one boot path (run()) ; the
	// --client per-host runtime reaches etcd via the control-plane
	// gRPC bridge, which is a separate concern (an etcd-over-gRPC
	// shim is the cleanest fix and lands in a follow-up).
	proxyEnabled          bool
	proxyStateDir         string
	proxyCaddyBinary      string
	proxyKeyPrefix        string
	proxyStorageEndpoints []string // empty → filesystem cert storage ; non-empty → shared etcd via darkweak adapter
}
