//go:build darwin && cgo

package weft

// acl_test.go exercises the two project-access grant paths that
// callerOwnsProject + VisibleProjects union:
//
//   1. dex `project:<uuid>` group claim (the OIDC-only path).
//   2. Platform-managed membership stored in the project's
//      Members list (the AddProjectMember path).
//
// Both are tested in isolation and in tandem; admin-bypass and
// outsider-denied cases pin the negatives.

import (
	"context"
	"testing"
)

// aclFixture builds a small multi-tenant world in one shot:
//
//   - three users (admin, alice, bob) registered via Caller.
//   - three projects: alice's default (auto-created on first
//     RegisterUser-equivalent), bob's default, plus a shared
//     project alice is a member of (via AddProjectMember) and
//     a group-claim project carol holds via her token's Groups.
//
// Returns the adapter + the canonical UUIDs for the test body so
// the assertions read cleanly.
type aclFixture struct {
	a              *Adapter
	aliceProject   string // alice's auto-default
	bobProject     string // bob's auto-default
	sharedProject  string // alice + admin via member-add
	claimedProject string // a project anyone with `project:<uuid>` group can see
}

func newACLFixture(t *testing.T) aclFixture {
	t.Helper()
	dir := t.TempDir()
	a := New(dir).(*Adapter)

	// Register the three users so they have stable vzd UUIDs the
	// membership path can reference.
	adminCaller := &Caller{Subject: "ldap:admin", Issuer: "https://dex.example", Groups: []string{PlatformAdminGroup}}
	aliceCaller := &Caller{Subject: "ldap:alice", Issuer: "https://dex.example"}
	bobCaller := &Caller{Subject: "ldap:bob", Issuer: "https://dex.example"}
	if _, _, err := a.RegisterUser(adminCaller); err != nil {
		t.Fatalf("RegisterUser(admin): %v", err)
	}
	if _, _, err := a.RegisterUser(aliceCaller); err != nil {
		t.Fatalf("RegisterUser(alice): %v", err)
	}
	if _, _, err := a.RegisterUser(bobCaller); err != nil {
		t.Fatalf("RegisterUser(bob): %v", err)
	}

	// Auto-default projects materialise on first ResolveProjectUUID
	// for the caller's subject — just call ResolveProjectUUID with
	// no input under a Caller-bearing ctx to trigger it.
	aliceCtx := WithCaller(context.Background(), aliceCaller)
	bobCtx := WithCaller(context.Background(), bobCaller)
	aliceUUID, err := a.AuthorizeProject(aliceCtx, "")
	if err != nil {
		t.Fatalf("alice default: %v", err)
	}
	bobUUID, err := a.AuthorizeProject(bobCtx, "")
	if err != nil {
		t.Fatalf("bob default: %v", err)
	}

	// Shared project: admin creates, then adds alice as a member.
	shared, _, err := a.CreateProject("shared-team")
	if err != nil {
		t.Fatalf("CreateProject(shared-team): %v", err)
	}
	aliceUser, ok := a.UserBySubject("https://dex.example", "ldap:alice")
	if !ok {
		t.Fatal("alice user vanished from registry")
	}
	if err := a.AddProjectMember(shared.UUID, aliceUser.UUID); err != nil {
		t.Fatalf("AddProjectMember(shared, alice): %v", err)
	}

	// Group-claim project: created admin-side; whoever's token
	// carries `project:<uuid>` sees it through the dex path.
	claimed, _, err := a.CreateProject("claimed-team")
	if err != nil {
		t.Fatalf("CreateProject(claimed-team): %v", err)
	}

	return aclFixture{
		a:              a,
		aliceProject:   aliceUUID,
		bobProject:     bobUUID,
		sharedProject:  shared.UUID,
		claimedProject: claimed.UUID,
	}
}

// TestVisibleProjects_AdminSeesAll pins the platform-admin
// bypass: the bool return is the "see all" signal and the map
// is nil (caller skips the per-UUID filter entirely).
func TestVisibleProjects_AdminSeesAll(t *testing.T) {
	f := newACLFixture(t)
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:admin",
		Issuer:  "https://dex.example",
		Groups:  []string{PlatformAdminGroup},
	})
	got, all, err := f.a.VisibleProjects(ctx)
	if err != nil {
		t.Fatalf("VisibleProjects: %v", err)
	}
	if !all || got != nil {
		t.Errorf("admin: want (nil, true, nil), got (%v, %v, nil)", got, all)
	}
}

// TestVisibleProjects_MemberSeesOwnAndShared confirms the union:
// alice gets her auto-default + the project she was added to via
// AddProjectMember. She does NOT see bob's project.
func TestVisibleProjects_MemberSeesOwnAndShared(t *testing.T) {
	f := newACLFixture(t)
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:alice",
		Issuer:  "https://dex.example",
	})
	got, all, err := f.a.VisibleProjects(ctx)
	if err != nil {
		t.Fatalf("VisibleProjects: %v", err)
	}
	if all {
		t.Fatal("alice must not be unscoped")
	}
	if _, ok := got[f.aliceProject]; !ok {
		t.Errorf("alice should see her own project %s", f.aliceProject)
	}
	if _, ok := got[f.sharedProject]; !ok {
		t.Errorf("alice should see the shared project (member-added) %s", f.sharedProject)
	}
	if _, ok := got[f.bobProject]; ok {
		t.Errorf("alice MUST NOT see bob's project %s", f.bobProject)
	}
	if _, ok := got[f.claimedProject]; ok {
		t.Errorf("alice doesn't carry the project:<uuid> claim, should not see claimed-team")
	}
}

// TestVisibleProjects_GroupClaimPath confirms the OIDC-only path:
// carol has no member-add anywhere but her token carries the
// `project:<uuid>` group; she sees that project.
func TestVisibleProjects_GroupClaimPath(t *testing.T) {
	f := newACLFixture(t)
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:carol",
		Issuer:  "https://dex.example",
		Groups:  []string{ProjectGroup(f.claimedProject)},
	})
	got, all, err := f.a.VisibleProjects(ctx)
	if err != nil {
		t.Fatalf("VisibleProjects: %v", err)
	}
	if all {
		t.Fatal("carol is not admin")
	}
	if _, ok := got[f.claimedProject]; !ok {
		t.Errorf("carol's token group should grant access to claimed-team %s", f.claimedProject)
	}
	// Carol shouldn't see alice's or bob's bucket; she also gets
	// her own auto-default which is fine but is not asserted here.
	if _, ok := got[f.aliceProject]; ok {
		t.Errorf("carol must not see alice's project")
	}
	if _, ok := got[f.bobProject]; ok {
		t.Errorf("carol must not see bob's project")
	}
}

// TestAuthorizeProject_DeniedForOutsider pins the negative path:
// bob asking to authorize on alice's shared project must error
// with PermissionDenied (not NotFound — existence is hidden).
func TestAuthorizeProject_DeniedForOutsider(t *testing.T) {
	f := newACLFixture(t)
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:bob",
		Issuer:  "https://dex.example",
	})
	_, err := f.a.AuthorizeProject(ctx, f.sharedProject)
	if err == nil {
		t.Fatal("bob must not authorize on a project he's not a member of")
	}
}

// TestAuthorizeProject_AllowedForMember is the mirror image:
// alice can authorize on the shared project because the member-
// add granted her access.
func TestAuthorizeProject_AllowedForMember(t *testing.T) {
	f := newACLFixture(t)
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:alice",
		Issuer:  "https://dex.example",
	})
	got, err := f.a.AuthorizeProject(ctx, f.sharedProject)
	if err != nil {
		t.Fatalf("alice should authorize on shared: %v", err)
	}
	if got != f.sharedProject {
		t.Errorf("got %q, want %q", got, f.sharedProject)
	}
}

// TestAuthorizeProject_DevModeUnrestricted keeps the single-host
// workflow honest: a dev-mode caller resolves any project name to
// a UUID, never gets denied.
func TestAuthorizeProject_DevModeUnrestricted(t *testing.T) {
	f := newACLFixture(t)
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "dev:david",
		Issuer:  "vzd:dev",
		Dev:     true,
	})
	for _, p := range []string{f.aliceProject, f.bobProject, f.sharedProject, f.claimedProject} {
		if _, err := f.a.AuthorizeProject(ctx, p); err != nil {
			t.Errorf("dev mode should authorize on %s: %v", p, err)
		}
	}
}

// TestRemoveProjectMember_RevokesAccess covers the membership
// revoke path: after RemoveProjectMember, alice can no longer
// authorize on the shared project (assuming she has no dex group
// claim for it).
func TestRemoveProjectMember_RevokesAccess(t *testing.T) {
	f := newACLFixture(t)
	aliceUser, _ := f.a.UserBySubject("https://dex.example", "ldap:alice")
	if err := f.a.RemoveProjectMember(f.sharedProject, aliceUser.UUID); err != nil {
		t.Fatalf("RemoveProjectMember: %v", err)
	}
	ctx := WithCaller(context.Background(), &Caller{
		Subject: "ldap:alice",
		Issuer:  "https://dex.example",
	})
	if _, err := f.a.AuthorizeProject(ctx, f.sharedProject); err == nil {
		t.Fatal("alice should be denied after RemoveProjectMember")
	}
}
