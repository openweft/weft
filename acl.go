package weft

// acl.go is the policy layer that gates RPC handlers by Caller
// group membership. Sits on top of [[vzd-uuid-keyed-resources]]
// and the OIDC layer in auth.go.
//
// Policy (Phase 2 of multitenancy):
//
//   1. Dev mode (Caller.Dev == true): every operation allowed.
//      Single-host workflows must not require token plumbing.
//
//   2. Group `platform-admin`: every operation allowed.
//      Equivalent of root for the cloud platform.
//
//   3. Per-project group `project:<uuid>`: grants full access to
//      one project's resources (its VMs, registries, …). UUID-
//      keyed per [[vzd-uuid-keyed-resources]] so renaming a
//      project never invalidates an ACL grant.
//
//   4. Per-user default project: every authenticated Caller has
//      implicit access to a project whose name is their `sub`
//      claim. vzd auto-creates this project on first sight.
//
// The policy itself is dirt-simple by design. Sophisticated
// schemes (role-based, attribute-based) can layer on top once the
// product needs them — but the spine must stay readable so
// auditing "who can do what" is one grep away.

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PlatformAdminGroup is the well-known group whose members get
// unconditional access to every project. Equivalent of root.
const PlatformAdminGroup = "platform-admin"

// ProjectGroupPrefix is prepended to a project UUID to form the
// per-project group name. A token with `project:<uuid>` in its
// `groups` claim is authorised on that project (and only that
// project, plus its own default project).
const ProjectGroupPrefix = "project:"

// ProjectGroup returns the group name granting access to the
// project identified by `uuid`. Used both at config-time (when
// provisioning dex groups) and at request-time (when checking
// membership).
func ProjectGroup(uuid string) string {
	return ProjectGroupPrefix + uuid
}

// AuthorizeProject verifies that the Caller in ctx can act on the
// project resolved from `nameOrUUID`. Returns the canonical UUID
// of the project (handy because most handler paths need it
// anyway) or a gRPC status error tagged PermissionDenied /
// Unauthenticated.
//
// Resolution order matches ResolveProjectUUID: empty input →
// caller's default project; UUID-shaped input → returned as-is;
// display-name input → looked up (or auto-created) in the
// registry. Auto-create is gated: only the caller's own default
// project name is allowed to auto-create. Anything else must
// already exist.
func (a *Adapter) AuthorizeProject(ctx context.Context, nameOrUUID string) (string, error) {
	caller, _ := CallerFrom(ctx)
	// No caller in ctx ⇒ either tests bypassed the interceptor,
	// or there's a misconfiguration. Refuse, with an explicit
	// hint so the operator knows what to wire.
	if caller == nil {
		return "", status.Error(codes.Unauthenticated, "no caller in context (interceptor not wired?)")
	}
	// Dev mode: every project is allowed; auto-create like the
	// existing free-form path.
	if caller.Dev {
		return a.ResolveProjectUUID(nameOrUUID), nil
	}
	// Platform admins skip every check, but we still resolve to
	// a UUID so downstream code has one.
	if caller.HasGroup(PlatformAdminGroup) {
		return a.ResolveProjectUUID(nameOrUUID), nil
	}
	// Resolve the project without auto-creating arbitrary names.
	// Strategy:
	//   * empty → caller's default project (always allowed; the
	//     name is the caller's sub).
	//   * UUID-shaped → look up; not found is Permission Denied
	//     (not NotFound: don't leak existence of other projects).
	//   * display name == caller's default name → resolve via the
	//     registry, auto-create if absent.
	//   * any other display name → must already exist AND caller
	//     must hold the per-project group.
	if nameOrUUID == "" {
		// Default project name is the caller's subject; auto-create
		// is fine because it's the caller's own bucket.
		p, _, err := a.projects.getOrCreate(caller.Subject)
		if err != nil {
			return "", status.Errorf(codes.Internal, "default project: %v", err)
		}
		return p.UUID, nil
	}
	if isUUID(nameOrUUID) {
		_, ok := a.ProjectByUUID(nameOrUUID)
		if !ok || !a.callerOwnsProject(caller, nameOrUUID) {
			return "", status.Errorf(codes.PermissionDenied, "no access to project %s", nameOrUUID)
		}
		return nameOrUUID, nil
	}
	// Display name path.
	if nameOrUUID == caller.Subject {
		p, _, err := a.projects.getOrCreate(caller.Subject)
		if err != nil {
			return "", status.Errorf(codes.Internal, "default project: %v", err)
		}
		return p.UUID, nil
	}
	p, ok := a.ProjectByName(nameOrUUID)
	if !ok {
		return "", status.Errorf(codes.PermissionDenied, "no access to project %q", nameOrUUID)
	}
	if !a.callerOwnsProject(caller, p.UUID) {
		return "", status.Errorf(codes.PermissionDenied, "no access to project %q", nameOrUUID)
	}
	return p.UUID, nil
}

// callerOwnsProject is true when the caller has access to the
// project via either of the two granting mechanisms:
//
//   1. dex `groups` claim carries `project:<uuid>` (the
//      OIDC-only path, original Phase-2 design).
//   2. The project's stored Members list contains the caller's
//      vzd user UUID (the platform-managed path, added later so
//      ops can grant access without round-tripping through dex
//      every time).
//
// Either is sufficient; both are commonly used in tandem.
func (a *Adapter) callerOwnsProject(c *Caller, uuid string) bool {
	if c == nil {
		return false
	}
	if c.HasGroup(ProjectGroup(uuid)) {
		return true
	}
	// Membership path: look up the caller's vzd user, see if
	// they're in the project's Members.
	if a.userReg == nil || a.projects == nil {
		return false
	}
	u, ok := a.userReg.lookupBySubject(c.Issuer, c.Subject)
	if !ok {
		return false
	}
	members, ok := a.projects.members(uuid)
	if !ok {
		return false
	}
	for _, m := range members {
		if m == u.UUID {
			return true
		}
	}
	return false
}

// VisibleProjects returns the set of project UUIDs the caller in
// ctx is permitted to enumerate. Used by ListVMs / ListProjects
// to scope responses. Dev-mode and platform-admin see every
// project; other callers see {default} ∪ {projects whose
// `project:<uuid>` group they belong to}.
func (a *Adapter) VisibleProjects(ctx context.Context) (map[string]struct{}, bool, error) {
	caller, _ := CallerFrom(ctx)
	if caller == nil {
		return nil, false, status.Error(codes.Unauthenticated, "no caller in context")
	}
	// Unlimited-scope shortcut: dev mode + platform-admin both
	// pass the empty-set + "see all" signal so callers can skip
	// the per-UUID filter entirely.
	if caller.Dev || caller.HasGroup(PlatformAdminGroup) {
		return nil, true, nil
	}
	out := make(map[string]struct{})
	// Caller's default project (auto-create on demand — costs
	// one Save the first time, then noop). Skipping this means a
	// brand-new user gets an empty ls; we'd rather they see
	// their own bucket so the next `ncl run` lands somewhere.
	defaultP, _, err := a.projects.getOrCreate(caller.Subject)
	if err == nil {
		out[defaultP.UUID] = struct{}{}
	}
	for _, g := range caller.Groups {
		if len(g) > len(ProjectGroupPrefix) && g[:len(ProjectGroupPrefix)] == ProjectGroupPrefix {
			out[g[len(ProjectGroupPrefix):]] = struct{}{}
		}
	}
	// Membership-based projects: any project whose Members list
	// contains the caller's vzd user UUID. Same union semantics as
	// the dex groups path above.
	if a.userReg != nil && a.projects != nil {
		if u, ok := a.userReg.lookupBySubject(caller.Issuer, caller.Subject); ok {
			for _, p := range a.projects.list() {
				for _, m := range p.Members {
					if m == u.UUID {
						out[p.UUID] = struct{}{}
						break
					}
				}
			}
		}
	}
	return out, false, nil
}

// RequireAdmin returns nil when the caller in ctx is a platform
// admin (or dev). Otherwise PermissionDenied. Use for operations
// that mutate global state (creating projects with arbitrary
// names, deleting projects belonging to others, …).
func RequireAdmin(ctx context.Context, op string) error {
	caller, _ := CallerFrom(ctx)
	if caller == nil {
		return status.Error(codes.Unauthenticated, "no caller in context")
	}
	if caller.Dev || caller.HasGroup(PlatformAdminGroup) {
		return nil
	}
	return status.Error(codes.PermissionDenied, fmt.Sprintf("%s requires platform-admin", op))
}
