//go:build darwin && cgo

package weft

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderNATSAuthorization_EmptyRegistry confirms the renderer
// works on a fresh adapter — no projects, no admin key. The output
// is still a syntactically valid `authorization { ... }` block
// (default_deny + empty users list) so an operator can splice it
// into nats.conf even before the first project exists.
func TestRenderNATSAuthorization_EmptyRegistry(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)

	out, err := a.RenderNATSAuthorization(NATSAuthorizationOptions{})
	if err != nil {
		t.Fatalf("RenderNATSAuthorization: %v", err)
	}
	if !strings.Contains(out, "authorization {") {
		t.Errorf("missing authorization opening brace:\n%s", out)
	}
	if !strings.Contains(out, "default_permissions") {
		t.Errorf("missing default_permissions block:\n%s", out)
	}
	if !strings.Contains(out, "users = [") {
		t.Errorf("missing users list:\n%s", out)
	}
}

// TestRenderNATSAuthorization_ProjectGetsItsSubject is the
// load-bearing positive: a project with a minted NKey shows up
// with a `subscribe` allow-list keyed on its UUID and a publish
// deny-all. The pubkey is the one derived from the stored seed.
func TestRenderNATSAuthorization_ProjectGetsItsSubject(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)

	p, _, err := a.CreateProject("alpha")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	seed, err := a.projects.ensureNATSUserSeed(p.UUID)
	if err != nil {
		t.Fatalf("ensureNATSUserSeed: %v", err)
	}
	pub, err := publicKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("publicKeyFromSeed: %v", err)
	}

	out, err := a.RenderNATSAuthorization(NATSAuthorizationOptions{})
	if err != nil {
		t.Fatalf("RenderNATSAuthorization: %v", err)
	}
	wantSub := "vzd.events.project." + p.UUID + ".events.>"
	wantPub := "vzd.events.project." + p.UUID + ".app.>"
	if !strings.Contains(out, wantSub) {
		t.Errorf("output missing per-project subscribe subject %q:\n%s", wantSub, out)
	}
	if !strings.Contains(out, wantPub) {
		t.Errorf("output missing per-project app-publish subject %q:\n%s", wantPub, out)
	}
	if !strings.Contains(out, pub) {
		t.Errorf("output missing pubkey %q:\n%s", pub, out)
	}
}

// TestRenderNATSAuthorization_AppNamespaceIsolated pins the
// Phase-4 invariant: the tenant's publish allow-list is exactly
// the app subject (not the events subject) so a workload that
// somehow gets hold of its own creds still can't forge platform
// events back onto the operator stream.
func TestRenderNATSAuthorization_AppNamespaceIsolated(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)

	p, _, err := a.CreateProject("isolated")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := a.projects.ensureNATSUserSeed(p.UUID); err != nil {
		t.Fatalf("ensureNATSUserSeed: %v", err)
	}

	out, err := a.RenderNATSAuthorization(NATSAuthorizationOptions{})
	if err != nil {
		t.Fatalf("RenderNATSAuthorization: %v", err)
	}
	// The events subject must appear only in the subscribe context,
	// never as a publish allow-list entry — we look for the exact
	// shape an attacker would need to forge platform events.
	forbidden := "publish:   { allow: [\"vzd.events.project." + p.UUID + ".events.>\"] }"
	if strings.Contains(out, forbidden) {
		t.Errorf("tenant must not get publish on the events subject:\n%s", out)
	}
}

// TestRenderNATSAuthorization_AdminUserEmitted confirms the
// AdminPubkey path: when set, an extra user block shows up at the
// top of the list with full pub/sub on vzd.> so vzd's own server-
// side consumers + publishers keep working under Phase-3 enforcement.
func TestRenderNATSAuthorization_AdminUserEmitted(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)

	const adminPub = "UABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	out, err := a.RenderNATSAuthorization(NATSAuthorizationOptions{AdminPubkey: adminPub})
	if err != nil {
		t.Fatalf("RenderNATSAuthorization: %v", err)
	}
	if !strings.Contains(out, adminPub) {
		t.Errorf("admin pubkey absent from output:\n%s", out)
	}
	if !strings.Contains(out, `publish:   { allow: ["vzd.>"] }`) {
		t.Errorf("admin pub-allow missing:\n%s", out)
	}
}

// TestRenderNATSAuthorization_SkipsUnkeyedProjects pins the silent-
// skip behaviour: a project with no NATSUserSeed is omitted from
// the block (no VMs registered yet → nothing to authorise). The
// renderer must NOT error or generate a placeholder line.
func TestRenderNATSAuthorization_SkipsUnkeyedProjects(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)

	p, _, err := a.CreateProject("no-vms-yet")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	out, err := a.RenderNATSAuthorization(NATSAuthorizationOptions{})
	if err != nil {
		t.Fatalf("RenderNATSAuthorization: %v", err)
	}
	if strings.Contains(out, p.UUID) {
		t.Errorf("unkeyed project %s should be skipped:\n%s", p.UUID, out)
	}
}

// TestRenderNATSAuthorization_DeterministicOrder pins the output's
// stable ordering by UUID, so a registry change that doesn't
// reorder projects produces a localised diff that an operator can
// review.
func TestRenderNATSAuthorization_DeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)

	// Create projects out of UUID-sorted order to make sure the
	// renderer re-sorts. Names are alphabetical so list() returns
	// them in name-order; the renderer overrides to UUID-order.
	for _, n := range []string{"zeta", "alpha", "mu"} {
		p, _, err := a.CreateProject(n)
		if err != nil {
			t.Fatalf("CreateProject(%s): %v", n, err)
		}
		if _, err := a.projects.ensureNATSUserSeed(p.UUID); err != nil {
			t.Fatalf("ensureNATSUserSeed(%s): %v", p.UUID, err)
		}
	}

	out1, err := a.RenderNATSAuthorization(NATSAuthorizationOptions{})
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	out2, err := a.RenderNATSAuthorization(NATSAuthorizationOptions{})
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if out1 != out2 {
		t.Errorf("renderer is non-deterministic across calls")
	}

	// Collect UUIDs in lexical order and confirm they appear in that
	// same order in the output.
	uuids := make([]string, 0)
	for _, p := range a.projects.list() {
		uuids = append(uuids, p.UUID)
	}
	sortedUUIDs := append([]string(nil), uuids...)
	for i := 1; i < len(sortedUUIDs); i++ {
		for j := i; j > 0 && sortedUUIDs[j-1] > sortedUUIDs[j]; j-- {
			sortedUUIDs[j-1], sortedUUIDs[j] = sortedUUIDs[j], sortedUUIDs[j-1]
		}
	}
	prevIdx := -1
	for _, u := range sortedUUIDs {
		idx := strings.Index(out1, u)
		if idx < 0 {
			t.Errorf("project %s missing from render", u)
			continue
		}
		if idx < prevIdx {
			t.Errorf("project %s appears at offset %d but earlier-sorted UUID was at %d — UUID ordering violated", u, idx, prevIdx)
		}
		prevIdx = idx
	}
}

// TestAutoRenderNATSAuthorization_NoOpWhenUnset confirms the
// helper is silent when no path is configured: callers wire it
// into every relevant mutation, and operator-driven setups (no
// auto-write) must not error or touch the filesystem.
func TestAutoRenderNATSAuthorization_NoOpWhenUnset(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)
	if _, _, err := a.CreateProject("p"); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if err := a.autoRenderNATSAuthorization(); err != nil {
		t.Errorf("expected no-op, got error: %v", err)
	}
}

// TestAutoRenderNATSAuthorization_WritesAfterSeedMint pins the
// happy path: configure the file, mint a seed, call the helper
// (or trigger it via a mutation), file lands at the right path
// with mode 0600 and contains the project's subject.
func TestAutoRenderNATSAuthorization_WritesAfterSeedMint(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)

	authzPath := filepath.Join(t.TempDir(), "subdir", "authorization.conf")
	a.SetNATSAuthorizationFile(authzPath, "")

	p, _, err := a.CreateProject("alpha")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := a.projects.ensureNATSUserSeed(p.UUID); err != nil {
		t.Fatalf("ensureNATSUserSeed: %v", err)
	}
	if err := a.autoRenderNATSAuthorization(); err != nil {
		t.Fatalf("autoRender: %v", err)
	}

	info, err := os.Stat(authzPath)
	if err != nil {
		t.Fatalf("stat %s: %v", authzPath, err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("auth file mode = %o, want 0600", mode)
	}
	body, err := os.ReadFile(authzPath)
	if err != nil {
		t.Fatalf("read %s: %v", authzPath, err)
	}
	wantSubj := "vzd.events.project." + p.UUID + ".events.>"
	if !strings.Contains(string(body), wantSubj) {
		t.Errorf("rendered file missing subject %q:\n%s", wantSubj, body)
	}
}

// TestAutoRenderNATSAuthorization_DropsDeletedProject pins the
// DeleteProject hook: after the project is gone the rendered file
// must no longer carry its UUID. Catches "stale entry left over"
// regressions where the hook forgets to re-fire on delete.
func TestAutoRenderNATSAuthorization_DropsDeletedProject(t *testing.T) {
	dir := t.TempDir()
	a := New(dir).(*Adapter)

	authzPath := filepath.Join(t.TempDir(), "authorization.conf")
	a.SetNATSAuthorizationFile(authzPath, "")

	p, _, err := a.CreateProject("doomed")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := a.projects.ensureNATSUserSeed(p.UUID); err != nil {
		t.Fatalf("ensureNATSUserSeed: %v", err)
	}
	// Initial render (no mutation has fired the hook yet — call
	// it explicitly to seed the file with the project present).
	if err := a.autoRenderNATSAuthorization(); err != nil {
		t.Fatalf("initial autoRender: %v", err)
	}
	body, _ := os.ReadFile(authzPath)
	if !strings.Contains(string(body), p.UUID) {
		t.Fatalf("pre-delete file is missing the project UUID:\n%s", body)
	}

	// DeleteProject should re-fire the hook and drop the entry.
	if err := a.DeleteProject(p.UUID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	body, err = os.ReadFile(authzPath)
	if err != nil {
		t.Fatalf("read after delete: %v", err)
	}
	if strings.Contains(string(body), p.UUID) {
		t.Errorf("deleted project %s still present in rendered file:\n%s", p.UUID, body)
	}
}
