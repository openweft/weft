package infra

// configfile.go materialises the plan's `config_file { path,
// template }` into a host-side directory the deployer exposes as
// a read-only virtio-fs share (tag "cfg"). The guest's ncl-init
// (or the OCI image's entrypoint) is then responsible for moving
// the file into place at its intended in-guest path — for now
// that's an operator-image concern, not vzd's.
//
// Token substitution is intentionally a no-op for this slice:
// `$REPLICA`, `$DC`, `$PRIVATE_IP`, `$PEERS` stay as literals in
// the rendered template because the deployer doesn't know
// per-replica context yet (it creates one VM per service, not
// three). When the deployer learns to fan out plans across DCs,
// substitution lands alongside the replica-index propagation.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// TemplateContext is the substitution payload for config-file
// templates. Per-replica fields drive `$REPLICA / $DC /
// $PRIVATE_IP / $PEERS / $PEER_DC` ; deploy-time secrets
// ($ADMIN_BCRYPT_HASH, $VZD_CLIENT_SECRET, $BASE_DOMAIN, …) are
// intentionally NOT in scope here — they pass through as
// literals so operator-side tooling (envsubst, CI templating)
// handles them.
type TemplateContext struct {
	Replica   int      // 1 for the single-replica deployer; 1..N for replica fan-out
	DC        string   // "dc1", "dc2", … — the AZ this replica is placed in
	PrivateIP string   // network.static_ip[Replica-1] when set
	Peers     []string // peer endpoints — comma-joined into $PEERS
	PeerDC    string   // sibling DC for cross-DC mirror configs (zot)
}

// SingleReplicaContext returns the substitution context for the
// one-replica path: replica=1, DC="dc1", PrivateIP from the first
// static_ip entry (or empty), no peers. Equivalent to
// `BuildReplicaContext(p, 1)` when ReplicaCount() == 1 — kept as
// a named convenience for callers that don't care about the
// distinction.
func SingleReplicaContext(p *Plan) TemplateContext {
	return BuildReplicaContext(p, 1)
}

// BuildReplicaContext returns the substitution context for the
// i-th replica (1-indexed). DC defaults to `dc<i>` ; PrivateIP
// is `network.static_ip[i-1]` when in bounds (and empty
// otherwise) ; PeerDC is the next sibling DC for cross-DC mirror
// configs ; Peers lists every static_ip *except* this replica's,
// formatted as `nats-dc1=...`-style tokens via plan-specific
// templates (today the renderer just comma-joins them).
//
// Out-of-bounds replica indexes (< 1 or > ReplicaCount()) clamp
// to sensible defaults rather than erroring — the caller is
// usually a loop where the index is already validated.
func BuildReplicaContext(p *Plan, replica int) TemplateContext {
	return BuildReplicaContextWithHost(p, replica, "")
}

// BuildReplicaContextWithHost is the scheduler-aware variant :
// when the deployer has picked an actual `Host` for this replica
// (via `weft.ScheduleVMGroup`), pass its `host.AZ` as
// `azOverride` to use it as `$DC`. That replaces the synthetic
// `dc<i>` label with the operator-declared AZ name from the
// Host registry, so rendered configs see the real cluster
// topology (e.g. `dc = "us-east-1a"` instead of `dc = "dc1"`).
//
// `azOverride = ""` falls back to the synthetic `dc<i>` label —
// the behaviour the single-host deployer has always used.
func BuildReplicaContextWithHost(p *Plan, replica int, azOverride string) TemplateContext {
	if replica < 1 {
		replica = 1
	}
	dc := fmt.Sprintf("dc%d", replica)
	if azOverride != "" {
		dc = azOverride
	}
	ctx := TemplateContext{
		Replica: replica,
		DC:      dc,
	}
	if p == nil || p.Network == nil {
		return ctx
	}
	ips := p.Network.StaticIP
	if len(ips) >= replica {
		ctx.PrivateIP = ips[replica-1]
	}
	// Peers + PeerDC only populated for multi-replica plans —
	// a count=1 plan deploys exactly this one VM, so the
	// "other static_ips" the operator declared aren't actually
	// reachable peers (yet) and putting them in $PEERS would
	// produce broken cluster configs.
	count := p.ReplicaCount()
	if count <= 1 {
		return ctx
	}
	peers := make([]string, 0, len(ips))
	for i, ip := range ips {
		if i == replica-1 {
			continue
		}
		peers = append(peers, ip)
	}
	ctx.Peers = peers
	// PeerDC: next sibling DC in the rotation (replica+1, mod
	// ReplicaCount). Mirror configs typically pair DC1↔DC2,
	// DC2↔DC3, DC3↔DC1 — the simplest stable rotation.
	next := (replica % count) + 1
	ctx.PeerDC = fmt.Sprintf("dc%d", next)
	return ctx
}

// configTokenRe matches the supported `$TOKEN` names with a
// trailing word boundary so `$DC` doesn't accidentally substitute
// inside `$DCFOO` or `$DC_extra`. Out-of-scope tokens
// (`$BASE_DOMAIN`, secrets) are not in this list — they pass
// through unchanged.
var configTokenRe = regexp.MustCompile(`\$(REPLICA|DC|PRIVATE_IP|PEERS|PEER_DC)\b`)

// RenderTemplate substitutes the supported tokens in `tmpl`
// using `ctx`. Unknown / out-of-scope tokens (operator secrets,
// $BASE_DOMAIN) survive verbatim so downstream tooling can fill
// them in. Designed to be a no-op when none of the supported
// tokens appear, so plans without templating cost nothing.
func RenderTemplate(tmpl string, ctx TemplateContext) string {
	return configTokenRe.ReplaceAllStringFunc(tmpl, func(match string) string {
		switch match[1:] { // strip the leading "$"
		case "REPLICA":
			return strconv.Itoa(ctx.Replica)
		case "DC":
			return ctx.DC
		case "PRIVATE_IP":
			return ctx.PrivateIP
		case "PEERS":
			return strings.Join(ctx.Peers, ",")
		case "PEER_DC":
			return ctx.PeerDC
		}
		return match
	})
}

// MaterialiseConfigFile writes the plan's config_file template
// (rendered with `ctx`) into a host-side directory and returns
// that directory path (suitable as the Path of a MicroVMShare).
// Returns ("", nil) when the plan has no config_file block —
// callers skip the share-append when the returned path is empty.
//
// The scratch sub-directory is keyed on the VM name so multi-
// replica deploys don't clash : the count=1 path lands at
// `<scratchRoot>/<service>/`, the count>1 path at
// `<scratchRoot>/<service>-dc<i>/` per replica.
//
// The file inside the directory is named after the basename of
// the plan's `config_file.path` (e.g. `/etc/nats/nats.conf` →
// `nats.conf`). A degenerate path ("/", ".", or empty) falls
// back to the literal "config".
//
// The directory is created with mode 0700 + the file 0600 —
// templates routinely carry inline secrets (bcrypt hashes for
// dex's static admin, NATS credentials seeds) so the materialised
// copy must not be world-readable on the host.
func MaterialiseConfigFile(p *Plan, scratchRoot string, ctx TemplateContext) (string, error) {
	if p == nil || p.ConfigFile == nil {
		return "", nil
	}
	// Per-replica scratch subdir keyed on the VM name. Single-
	// replica path stays at <root>/<service> (no -dc suffix) so
	// operators reading scratch don't see noisy names for the
	// common case.
	subdir := p.Service
	if p.ReplicaCount() > 1 {
		subdir = fmt.Sprintf("%s-dc%d", p.Service, ctx.Replica)
	}
	dir := filepath.Join(scratchRoot, subdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	name := filepath.Base(p.ConfigFile.Path)
	if name == "" || name == "." || name == "/" {
		name = "config"
	}
	target := filepath.Join(dir, name)
	tmp := target + ".tmp"
	rendered := RenderTemplate(p.ConfigFile.Template, ctx)
	if err := os.WriteFile(tmp, []byte(rendered), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", fmt.Errorf("rename %s -> %s: %w", tmp, target, err)
	}
	return dir, nil
}
