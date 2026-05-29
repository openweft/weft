package main

// users.go implements the five User RPCs on top of the Adapter's
// user-registry wrappers. ACL discipline:
//
//   * ListUsers      — platform-admin (or dev). Enumeration of
//     accounts leaks team structure; non-admin callers can't see
//     the full set.
//   * GetUser        — self OR platform-admin. Letting a caller
//     fetch their own record (`Me` is the shortcut for "get
//     myself by implicit UUID") is part of the OIDC sub-self-
//     introspection pattern dex consumers expect.
//   * Me             — every authenticated caller; no UUID
//     needed.
//   * SetUserDisplayName — self OR platform-admin. Same
//     reasoning as GetUser.
//   * DeleteUser     — platform-admin (or dev) only. Cascading
//     across registries is deferred — for now, the operator is
//     expected to clean up project ACLs that referenced the
//     deleted UUID.
//
// Per [[vzd-event-bus]] every mutation Publishes a `user.*`
// event (created in RegisterUser, renamed in SetUserDisplayName,
// deleted here) that bus subscribers can react to.

import (
	"context"

	"github.com/openweft/weft"
	vzdv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// userPersister returns a weft.UserPersister closure that the
// auth interceptors invoke after every successful (non-dev)
// token validation. Fire-and-forget by design: a registry write
// failure must not block the actual RPC, and the next request
// retries the persist naturally.
func userPersister(a weft.VZAdapter) weft.UserPersister {
	return func(c *weft.Caller) {
		if c == nil || c.Dev {
			return
		}
		_, _, _ = a.RegisterUser(c)
	}
}

func toUserInfo(u weft.User) *vzdv1.UserInfo {
	return &vzdv1.UserInfo{
		Uuid:             u.UUID,
		OidcSubject:      u.Subject,
		OidcIssuer:       u.Issuer,
		Email:            u.Email,
		DisplayName:      u.DisplayName,
		Groups:           u.Groups,
		CreatedAtUnixNs:  u.CreatedAt.UnixNano(),
		LastSeenAtUnixNs: u.LastSeenAt.UnixNano(),
	}
}

func (s *vzdServer) ListUsers(ctx context.Context, _ *vzdv1.ListUsersRequest) (*vzdv1.ListUsersResponse, error) {
	if err := weft.RequireAdmin(ctx, "list users"); err != nil {
		return nil, err
	}
	users := s.adp.Users()
	out := make([]*vzdv1.UserInfo, len(users))
	for i, u := range users {
		out[i] = toUserInfo(u)
	}
	return &vzdv1.ListUsersResponse{Users: out}, nil
}

// authUserSelfOrAdmin gates the per-user reads/writes. Returns
// the resolved User on success. The auth callsite passed to
// RequireAdmin is used only when the caller isn't `self`, so the
// error message tells the operator exactly which check failed.
func (s *vzdServer) authUserSelfOrAdmin(ctx context.Context, uuid, op string) (weft.User, error) {
	if uuid == "" {
		return weft.User{}, status.Error(codes.InvalidArgument, "uuid is required")
	}
	u, ok := s.adp.UserByUUID(uuid)
	if !ok {
		// Same existence-hiding as the volume / network handlers.
		return weft.User{}, status.Errorf(codes.PermissionDenied, "no access to user %s", uuid)
	}
	// Self check: compare the user's (issuer, subject) against the
	// authenticated caller in ctx.
	caller, _ := weft.CallerFrom(ctx)
	if caller != nil && caller.Subject == u.Subject && caller.Issuer == u.Issuer {
		return u, nil
	}
	if err := weft.RequireAdmin(ctx, op); err != nil {
		return weft.User{}, err
	}
	return u, nil
}

func (s *vzdServer) GetUser(ctx context.Context, req *vzdv1.GetUserRequest) (*vzdv1.GetUserResponse, error) {
	u, err := s.authUserSelfOrAdmin(ctx, req.Uuid, "get user")
	if err != nil {
		return nil, err
	}
	return &vzdv1.GetUserResponse{User: toUserInfo(u)}, nil
}

// Me returns the caller's own user record. Auto-registers on
// first sight so a fresh OIDC subject doesn't need a manual
// provisioning step — the auth interceptor already validated
// the token, so the caller is trusted-as-claimed.
func (s *vzdServer) Me(ctx context.Context, _ *vzdv1.MeRequest) (*vzdv1.MeResponse, error) {
	caller, _ := weft.CallerFrom(ctx)
	if caller == nil {
		return nil, status.Error(codes.Unauthenticated, "no caller in context")
	}
	u, _, err := s.adp.RegisterUser(caller)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "register user: %v", err)
	}
	return &vzdv1.MeResponse{User: toUserInfo(u)}, nil
}

func (s *vzdServer) SetUserDisplayName(ctx context.Context, req *vzdv1.SetUserDisplayNameRequest) (*vzdv1.SetUserDisplayNameResponse, error) {
	if _, err := s.authUserSelfOrAdmin(ctx, req.Uuid, "set user display name"); err != nil {
		return nil, err
	}
	if err := s.adp.SetUserDisplayName(req.Uuid, req.DisplayName); err != nil {
		return nil, status.Errorf(codes.Internal, "set display name: %v", err)
	}
	u, _ := s.adp.UserByUUID(req.Uuid)
	return &vzdv1.SetUserDisplayNameResponse{User: toUserInfo(u)}, nil
}

func (s *vzdServer) DeleteUser(ctx context.Context, req *vzdv1.DeleteUserRequest) (*vzdv1.DeleteUserResponse, error) {
	if req.Uuid == "" {
		return nil, status.Error(codes.InvalidArgument, "uuid is required")
	}
	if err := weft.RequireAdmin(ctx, "delete user"); err != nil {
		return nil, err
	}
	if err := s.adp.DeleteUser(req.Uuid); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "delete user: %v", err)
	}
	logger.Printf("DeleteUser uuid=%s", req.Uuid)
	return &vzdv1.DeleteUserResponse{}, nil
}
