package weft

// nats_config.go renders the NATS server's `authorization` block
// from the current project registry. This is Phase 3 of
// [[weft-tenant-event-access]]: per-pubkey subject permissions on
// the running NATS server that finally enforce what Phase 2's NKey
// material was set up for.
//
// Output is NATS conf format (a comment-friendly superset of
// JSON that nats-server reads natively), not HashiCorp HCL —
// nats-server doesn't parse HCL. The format keeps the
// human-readable shape the user prefers (`key: value`, `#`
// comments) without lying about which language the file is in.
//
// Wire model:
//
//   - One `user` entry per project: nkey = project pubkey,
//     subscribe = weft.events.project.<uuid>.events.>, publish
//     = weft.events.project.<uuid>.app.> (Phase 4: tenants can
//     emit their own app-level events on a sibling namespace
//     while the operator-events tree stays read-only for them).
//
//   - One `user` entry for the platform itself ("weft-admin"):
//     full subscribe + publish on `weft.>` so weft's own server-side
//     consumers + publishers keep working. The admin nkey lives
//     on the Adapter (loaded at startup, separate from any user
//     project; future work: rotate via dex).
//
//   - default_permissions denies everything; users get explicit
//     allow lists only.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// NATSAuthorizationOptions controls what the renderer emits.
// AdminPubkey is the NATS user-NKey public key (`U...`) that
// weft itself uses to open its outbound bus connection — when
// empty, the renderer skips the admin block and only emits
// per-project users (single-host dev where weft publishes
// anonymously through `no_auth_user`).
type NATSAuthorizationOptions struct {
	AdminPubkey string
}

// RenderNATSAuthorization returns the `authorization { ... }`
// block to splice into nats.conf. The block is deterministic:
// projects are sorted by UUID so a registry mutation that adds a
// project yields a diff localised to the new entry. Comments
// document the wire model so an operator who opens nats.conf by
// hand can reason about who's allowed to talk to whom.
//
// Projects without a NATSUserSeed are skipped (they have nothing
// to authorize yet; the seed materialises on first
// RegisterMicroVM for the project). The caller decides whether
// that's an error — usually it isn't, since a freshly-created
// project with no VMs needs no NATS access.
func (a *Adapter) RenderNATSAuthorization(opts NATSAuthorizationOptions) (string, error) {
	if a == nil || a.projects == nil {
		return "", fmt.Errorf("weft: projects registry not initialised")
	}
	projects := a.projects.list()
	// Stable order: list() sorts by display name, but we want UUID
	// ordering for the rendered block because the UUID is what the
	// permission line keys on.
	sort.SliceStable(projects, func(i, j int) bool {
		return projects[i].UUID < projects[j].UUID
	})

	var b strings.Builder
	b.WriteString("# weft-rendered NATS authorization block.\n")
	b.WriteString("# Auto-generated from weft's project registry — do not edit by hand;\n")
	b.WriteString("# weft re-renders on every project create/delete and signals nats-server\n")
	b.WriteString("# to reload. See [[weft-tenant-event-access]] Phase 3.\n")
	b.WriteString("\n")
	b.WriteString("authorization {\n")
	b.WriteString("  default_permissions = {\n")
	b.WriteString("    publish:   { deny: [\">\"] }\n")
	b.WriteString("    subscribe: { deny: [\">\"] }\n")
	b.WriteString("  }\n")
	b.WriteString("  users = [\n")
	if opts.AdminPubkey != "" {
		b.WriteString("    # weft itself — publishes platform events, reads operator subscriptions.\n")
		fmt.Fprintf(&b, "    { nkey: %q, permissions: {\n", opts.AdminPubkey)
		b.WriteString("      publish:   { allow: [\"weft.>\"] }\n")
		b.WriteString("      subscribe: { allow: [\"weft.>\"] }\n")
		b.WriteString("    } },\n")
	}
	for _, p := range projects {
		if p.NATSUserSeed == "" {
			continue
		}
		pub, err := publicKeyFromSeed(p.NATSUserSeed)
		if err != nil {
			return "", fmt.Errorf("project %s: %w", p.UUID, err)
		}
		fmt.Fprintf(&b, "    # project %q (%s) — tenant: subscribe on its own event mirror, publish on its app namespace.\n", p.Name, p.UUID)
		fmt.Fprintf(&b, "    { nkey: %q, permissions: {\n", pub)
		fmt.Fprintf(&b, "      subscribe: { allow: [%q] }\n", projectSubscribeSubject(p.UUID))
		fmt.Fprintf(&b, "      publish:   { allow: [%q] }\n", projectAppPublishSubject(p.UUID))
		b.WriteString("    } },\n")
	}
	b.WriteString("  ]\n")
	b.WriteString("}\n")
	return b.String(), nil
}

// SetNATSAuthorizationFile turns on auto-render. When `path` is
// non-empty, the rendered `authorization { … }` block is written
// (atomic tmp+rename, mode 0600) after every mutation that
// changes its output: a new project seed, a project delete. The
// `adminPubkey` (optional) is rendered as the platform's own NKey
// user. Empty path disables the hook — operators can still drive
// the renderer manually via `weft admin nats-authz`.
//
// Not thread-safe with concurrent calls; settle this at startup
// before mutations begin.
func (a *Adapter) SetNATSAuthorizationFile(path, adminPubkey string) {
	a.natsAuthzPath = path
	a.natsAuthzAdminPubkey = adminPubkey
}

// autoRenderNATSAuthorization re-renders the authorization block
// and writes it to the configured path atomically. No-op when
// the path is unset (auto-render disabled). Errors are returned
// to the caller so the wrapping mutation can decide whether to
// roll back — but in practice the existing callers ignore the
// error: a registry change has already succeeded and we don't
// want to undo it just because the nats.conf file failed to
// update. The operator can re-run `weft admin nats-authz` to
// recover.
func (a *Adapter) autoRenderNATSAuthorization() error {
	if a == nil || a.natsAuthzPath == "" {
		return nil
	}
	conf, err := a.RenderNATSAuthorization(NATSAuthorizationOptions{
		AdminPubkey: a.natsAuthzAdminPubkey,
	})
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	dir := filepath.Dir(a.natsAuthzPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp := a.natsAuthzPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(conf), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, a.natsAuthzPath); err != nil {
		return fmt.Errorf("rename %s: %w", a.natsAuthzPath, err)
	}
	return nil
}

// projectSubscribeSubject returns the wildcard subject the
// project's tenant user is allowed to subscribe to. Centralised
// here so a future subject-scheme tweak only needs one edit.
func projectSubscribeSubject(projectUUID string) string {
	return "weft.events.project." + projectUUID + ".events.>"
}

// projectAppPublishSubject returns the wildcard subject the
// project's tenant user is allowed to publish to — the
// app-namespace mirror of the event-namespace. Operators reading
// the operational stream don't see this by default; the
// dual-namespace layout (events./app.) is exactly the read-only-
// to-tenant + read-write-on-app contract from
// [[weft-tenant-event-access]] Phase 4.
func projectAppPublishSubject(projectUUID string) string {
	return "weft.events.project." + projectUUID + ".app.>"
}
