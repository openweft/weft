// Smoke-test the testutil helper itself. The bulk of its surface
// is exercised through every cmd/weft/* sub-package's tests, but
// because Go runs coverage per-package those cross-package calls
// don't contribute to testutil's own profile. This file fills that
// gap with a tour of the override hooks + zero-value defaults.
package testutil

import (
	"context"
	"errors"
	"testing"

	vzdv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft-client"
	"google.golang.org/grpc"
)

// dial connects to the server's unix socket via the same path
// production weft uses. Returns the typed client + a deferred
// cleanup function.
func dial(t *testing.T, s *Server) (vzdv1.WeftAgentClient, func()) {
	t.Helper()
	c, conn, err := vzclient.Client(s.Socket())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return c, func() { _ = conn.Close() }
}

// TestServer_DefaultsReturnZeroValueResponses pins the "no Fn set"
// path for every RPC — we don't need to assert payload details,
// just that the call lands and returns nil errors.
func TestServer_DefaultsReturnZeroValueResponses(t *testing.T) {
	s := NewServer(t)
	c, cleanup := dial(t, s)
	defer cleanup()
	ctx := context.Background()

	// Unary RPCs — each returns the zero-value response from the
	// default implementation. Errors here would mean the stub is
	// broken, not that the test is wrong.
	if _, err := c.ListVMs(ctx, &vzdv1.ListVMsRequest{}); err != nil {
		t.Errorf("ListVMs: %v", err)
	}
	if _, err := c.VMStatus(ctx, &vzdv1.VMStatusRequest{Name: "x"}); err != nil {
		t.Errorf("VMStatus: %v", err)
	}
	if _, err := c.StartVM(ctx, &vzdv1.StartVMRequest{}); err != nil {
		t.Errorf("StartVM: %v", err)
	}
	if _, err := c.StopVM(ctx, &vzdv1.StopVMRequest{}); err != nil {
		t.Errorf("StopVM: %v", err)
	}
	if _, err := c.CreateVM(ctx, &vzdv1.CreateVMRequest{}); err != nil {
		t.Errorf("CreateVM: %v", err)
	}
	if _, err := c.DeleteVM(ctx, &vzdv1.DeleteVMRequest{}); err != nil {
		t.Errorf("DeleteVM: %v", err)
	}
	if _, err := c.ProvisionVM(ctx, &vzdv1.ProvisionVMRequest{}); err != nil {
		t.Errorf("ProvisionVM: %v", err)
	}
	if _, err := c.DeprovisionVM(ctx, &vzdv1.DeprovisionVMRequest{}); err != nil {
		t.Errorf("DeprovisionVM: %v", err)
	}
	if _, err := c.PullImages(ctx, &vzdv1.PullImagesRequest{}); err != nil {
		t.Errorf("PullImages: %v", err)
	}
	if _, err := c.PullImage(ctx, &vzdv1.PullImageRequest{}); err != nil {
		t.Errorf("PullImage: %v", err)
	}
	if _, err := c.PatchImage(ctx, &vzdv1.PatchImageRequest{}); err != nil {
		t.Errorf("PatchImage: %v", err)
	}
	if _, err := c.ListImages(ctx, &vzdv1.ListImagesRequest{}); err != nil {
		t.Errorf("ListImages: %v", err)
	}
	if _, err := c.CleanImages(ctx, &vzdv1.CleanImagesRequest{}); err != nil {
		t.Errorf("CleanImages: %v", err)
	}
	if _, err := c.WaitVM(ctx, &vzdv1.WaitVMRequest{}); err != nil {
		t.Errorf("WaitVM: %v", err)
	}
	if _, err := c.RegisterMicroVM(ctx, &vzdv1.RegisterMicroVMRequest{}); err != nil {
		t.Errorf("RegisterMicroVM: %v", err)
	}
	if _, err := c.VMTimings(ctx, &vzdv1.VMTimingsRequest{}); err != nil {
		t.Errorf("VMTimings: %v", err)
	}
	if _, err := c.VMLogs(ctx, &vzdv1.VMLogsRequest{}); err != nil {
		t.Errorf("VMLogs: %v", err)
	}
	if _, err := c.ListProjects(ctx, &vzdv1.ListProjectsRequest{}); err != nil {
		t.Errorf("ListProjects: %v", err)
	}
	if _, err := c.CreateProject(ctx, &vzdv1.CreateProjectRequest{Name: "p"}); err != nil {
		t.Errorf("CreateProject: %v", err)
	}
	if _, err := c.RenameProject(ctx, &vzdv1.RenameProjectRequest{Uuid: "u", NewName: "n"}); err != nil {
		t.Errorf("RenameProject: %v", err)
	}
	if _, err := c.DeleteProject(ctx, &vzdv1.DeleteProjectRequest{}); err != nil {
		t.Errorf("DeleteProject: %v", err)
	}
	if _, err := c.AddProjectMember(ctx, &vzdv1.AddProjectMemberRequest{}); err != nil {
		t.Errorf("AddProjectMember: %v", err)
	}
	if _, err := c.RemoveProjectMember(ctx, &vzdv1.RemoveProjectMemberRequest{}); err != nil {
		t.Errorf("RemoveProjectMember: %v", err)
	}
	if _, err := c.ListProjectMembers(ctx, &vzdv1.ListProjectMembersRequest{}); err != nil {
		t.Errorf("ListProjectMembers: %v", err)
	}
	if _, err := c.ListUsers(ctx, &vzdv1.ListUsersRequest{}); err != nil {
		t.Errorf("ListUsers: %v", err)
	}
	if _, err := c.GetUser(ctx, &vzdv1.GetUserRequest{Uuid: "u"}); err != nil {
		t.Errorf("GetUser: %v", err)
	}
	if _, err := c.Me(ctx, &vzdv1.MeRequest{}); err != nil {
		t.Errorf("Me: %v", err)
	}
	if _, err := c.SetUserDisplayName(ctx, &vzdv1.SetUserDisplayNameRequest{Uuid: "u", DisplayName: "n"}); err != nil {
		t.Errorf("SetUserDisplayName: %v", err)
	}
	if _, err := c.DeleteUser(ctx, &vzdv1.DeleteUserRequest{}); err != nil {
		t.Errorf("DeleteUser: %v", err)
	}
	if _, err := c.ListNetworks(ctx, &vzdv1.ListNetworksRequest{}); err != nil {
		t.Errorf("ListNetworks: %v", err)
	}
	if _, err := c.CreateNetwork(ctx, &vzdv1.CreateNetworkRequest{Name: "n", Cidr: "10/8", Type: "nat"}); err != nil {
		t.Errorf("CreateNetwork: %v", err)
	}
	if _, err := c.RenameNetwork(ctx, &vzdv1.RenameNetworkRequest{Uuid: "u", NewName: "n"}); err != nil {
		t.Errorf("RenameNetwork: %v", err)
	}
	if _, err := c.SetNetworkDNS(ctx, &vzdv1.SetNetworkDNSRequest{Uuid: "u"}); err != nil {
		t.Errorf("SetNetworkDNS: %v", err)
	}
	if _, err := c.DeleteNetwork(ctx, &vzdv1.DeleteNetworkRequest{}); err != nil {
		t.Errorf("DeleteNetwork: %v", err)
	}
	if _, err := c.SetNetworkDefaultSecurityGroups(ctx, &vzdv1.SetNetworkDefaultSecurityGroupsRequest{Uuid: "u"}); err != nil {
		t.Errorf("SetNetworkDefaultSecurityGroups: %v", err)
	}
	if _, err := c.ListSecurityGroups(ctx, &vzdv1.ListSecurityGroupsRequest{}); err != nil {
		t.Errorf("ListSecurityGroups: %v", err)
	}
	if _, err := c.CreateSecurityGroup(ctx, &vzdv1.CreateSecurityGroupRequest{Name: "n"}); err != nil {
		t.Errorf("CreateSecurityGroup: %v", err)
	}
	if _, err := c.RenameSecurityGroup(ctx, &vzdv1.RenameSecurityGroupRequest{Uuid: "u", NewName: "n"}); err != nil {
		t.Errorf("RenameSecurityGroup: %v", err)
	}
	if _, err := c.SetSecurityGroupDescription(ctx, &vzdv1.SetSecurityGroupDescriptionRequest{Uuid: "u"}); err != nil {
		t.Errorf("SetSecurityGroupDescription: %v", err)
	}
	if _, err := c.SetSecurityGroupRules(ctx, &vzdv1.SetSecurityGroupRulesRequest{Uuid: "u"}); err != nil {
		t.Errorf("SetSecurityGroupRules: %v", err)
	}
	if _, err := c.DeleteSecurityGroup(ctx, &vzdv1.DeleteSecurityGroupRequest{}); err != nil {
		t.Errorf("DeleteSecurityGroup: %v", err)
	}
	if _, err := c.ListVolumes(ctx, &vzdv1.ListVolumesRequest{}); err != nil {
		t.Errorf("ListVolumes: %v", err)
	}
	if _, err := c.CreateVolume(ctx, &vzdv1.CreateVolumeRequest{Name: "n", SizeGib: 1, Format: "raw"}); err != nil {
		t.Errorf("CreateVolume: %v", err)
	}
	if _, err := c.RenameVolume(ctx, &vzdv1.RenameVolumeRequest{Uuid: "u", NewName: "n"}); err != nil {
		t.Errorf("RenameVolume: %v", err)
	}
	if _, err := c.ResizeVolume(ctx, &vzdv1.ResizeVolumeRequest{Uuid: "u", NewSizeGib: 2}); err != nil {
		t.Errorf("ResizeVolume: %v", err)
	}
	if _, err := c.AttachVolume(ctx, &vzdv1.AttachVolumeRequest{Uuid: "u", VmUuid: "vm"}); err != nil {
		t.Errorf("AttachVolume: %v", err)
	}
	if _, err := c.DetachVolume(ctx, &vzdv1.DetachVolumeRequest{}); err != nil {
		t.Errorf("DetachVolume: %v", err)
	}
	if _, err := c.DeleteVolume(ctx, &vzdv1.DeleteVolumeRequest{}); err != nil {
		t.Errorf("DeleteVolume: %v", err)
	}
	if _, err := c.RenderNATSAuthorization(ctx, &vzdv1.RenderNATSAuthorizationRequest{}); err != nil {
		t.Errorf("RenderNATSAuthorization: %v", err)
	}
	if _, err := c.RegisterHost(ctx, &vzdv1.RegisterHostRequest{Hostname: "h"}); err != nil {
		t.Errorf("RegisterHost: %v", err)
	}
	if _, err := c.ListHosts(ctx, &vzdv1.ListHostsRequest{}); err != nil {
		t.Errorf("ListHosts: %v", err)
	}
	if _, err := c.GetHost(ctx, &vzdv1.GetHostRequest{Uuid: "u"}); err != nil {
		t.Errorf("GetHost: %v", err)
	}
	if _, err := c.HeartbeatHost(ctx, &vzdv1.HeartbeatHostRequest{}); err != nil {
		t.Errorf("HeartbeatHost: %v", err)
	}
	if _, err := c.SetHostState(ctx, &vzdv1.SetHostStateRequest{}); err != nil {
		t.Errorf("SetHostState: %v", err)
	}
	if _, err := c.SetHostLabels(ctx, &vzdv1.SetHostLabelsRequest{}); err != nil {
		t.Errorf("SetHostLabels: %v", err)
	}
	if _, err := c.DeleteHost(ctx, &vzdv1.DeleteHostRequest{}); err != nil {
		t.Errorf("DeleteHost: %v", err)
	}

	// Server-streaming WatchEvents — default impl returns nil
	// (no events, clean EOF).
	stream, err := c.WatchEvents(ctx, &vzdv1.WatchEventsRequest{})
	if err != nil {
		t.Fatalf("WatchEvents: %v", err)
	}
	if _, err := stream.Recv(); err == nil {
		t.Error("expected EOF on empty stream")
	}
}

// TestServer_OverridesDispatch pins the if-Fn-then-call branches.
// Sets an override on every RPC; calling each one must invoke the
// fn (signalled by a sentinel error we return).
func TestServer_OverridesDispatch(t *testing.T) {
	s := NewServer(t)
	c, cleanup := dial(t, s)
	defer cleanup()
	ctx := context.Background()
	want := errors.New("override-fired")

	// Wire one sentinel-returning override per RPC and confirm
	// the dispatch hit it. Using errors.Is keeps the assertion
	// resilient to gRPC's wrapping.
	expectErr := func(t *testing.T, label string, err error) {
		t.Helper()
		if err == nil || !contains(err.Error(), want.Error()) {
			t.Errorf("%s: expected sentinel error, got %v", label, err)
		}
	}

	s.ListVMsFn = func(context.Context, *vzdv1.ListVMsRequest) (*vzdv1.ListVMsResponse, error) {
		return nil, want
	}
	s.VMStatusFn = func(context.Context, *vzdv1.VMStatusRequest) (*vzdv1.VMStatusResponse, error) {
		return nil, want
	}
	s.StartVMFn = func(context.Context, *vzdv1.StartVMRequest) (*vzdv1.StartVMResponse, error) {
		return nil, want
	}
	s.StopVMFn = func(context.Context, *vzdv1.StopVMRequest) (*vzdv1.StopVMResponse, error) {
		return nil, want
	}
	s.CreateVMFn = func(context.Context, *vzdv1.CreateVMRequest) (*vzdv1.CreateVMResponse, error) {
		return nil, want
	}
	s.DeleteVMFn = func(context.Context, *vzdv1.DeleteVMRequest) (*vzdv1.DeleteVMResponse, error) {
		return nil, want
	}
	s.ProvisionVMFn = func(context.Context, *vzdv1.ProvisionVMRequest) (*vzdv1.ProvisionVMResponse, error) {
		return nil, want
	}
	s.DeprovisionVMFn = func(context.Context, *vzdv1.DeprovisionVMRequest) (*vzdv1.DeprovisionVMResponse, error) {
		return nil, want
	}
	s.PullImagesFn = func(context.Context, *vzdv1.PullImagesRequest) (*vzdv1.PullImagesResponse, error) {
		return nil, want
	}
	s.PullImageFn = func(context.Context, *vzdv1.PullImageRequest) (*vzdv1.PullImageResponse, error) {
		return nil, want
	}
	s.PatchImageFn = func(context.Context, *vzdv1.PatchImageRequest) (*vzdv1.PatchImageResponse, error) {
		return nil, want
	}
	s.ListImagesFn = func(context.Context, *vzdv1.ListImagesRequest) (*vzdv1.ListImagesResponse, error) {
		return nil, want
	}
	s.CleanImagesFn = func(context.Context, *vzdv1.CleanImagesRequest) (*vzdv1.CleanImagesResponse, error) {
		return nil, want
	}
	s.WaitVMFn = func(context.Context, *vzdv1.WaitVMRequest) (*vzdv1.WaitVMResponse, error) {
		return nil, want
	}
	s.RegisterMicroVMFn = func(context.Context, *vzdv1.RegisterMicroVMRequest) (*vzdv1.RegisterMicroVMResponse, error) {
		return nil, want
	}
	s.VMTimingsFn = func(context.Context, *vzdv1.VMTimingsRequest) (*vzdv1.VMTimingsResponse, error) {
		return nil, want
	}
	s.VMLogsFn = func(context.Context, *vzdv1.VMLogsRequest) (*vzdv1.VMLogsResponse, error) {
		return nil, want
	}
	s.ListProjectsFn = func(context.Context, *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
		return nil, want
	}
	s.CreateProjectFn = func(context.Context, *vzdv1.CreateProjectRequest) (*vzdv1.CreateProjectResponse, error) {
		return nil, want
	}
	s.RenameProjectFn = func(context.Context, *vzdv1.RenameProjectRequest) (*vzdv1.RenameProjectResponse, error) {
		return nil, want
	}
	s.DeleteProjectFn = func(context.Context, *vzdv1.DeleteProjectRequest) (*vzdv1.DeleteProjectResponse, error) {
		return nil, want
	}
	s.AddProjectMemberFn = func(context.Context, *vzdv1.AddProjectMemberRequest) (*vzdv1.AddProjectMemberResponse, error) {
		return nil, want
	}
	s.RemoveProjectMemberFn = func(context.Context, *vzdv1.RemoveProjectMemberRequest) (*vzdv1.RemoveProjectMemberResponse, error) {
		return nil, want
	}
	s.ListProjectMembersFn = func(context.Context, *vzdv1.ListProjectMembersRequest) (*vzdv1.ListProjectMembersResponse, error) {
		return nil, want
	}
	s.ListUsersFn = func(context.Context, *vzdv1.ListUsersRequest) (*vzdv1.ListUsersResponse, error) {
		return nil, want
	}
	s.GetUserFn = func(context.Context, *vzdv1.GetUserRequest) (*vzdv1.GetUserResponse, error) {
		return nil, want
	}
	s.MeFn = func(context.Context, *vzdv1.MeRequest) (*vzdv1.MeResponse, error) { return nil, want }
	s.SetUserDisplayNameFn = func(context.Context, *vzdv1.SetUserDisplayNameRequest) (*vzdv1.SetUserDisplayNameResponse, error) {
		return nil, want
	}
	s.DeleteUserFn = func(context.Context, *vzdv1.DeleteUserRequest) (*vzdv1.DeleteUserResponse, error) {
		return nil, want
	}
	s.ListNetworksFn = func(context.Context, *vzdv1.ListNetworksRequest) (*vzdv1.ListNetworksResponse, error) {
		return nil, want
	}
	s.CreateNetworkFn = func(context.Context, *vzdv1.CreateNetworkRequest) (*vzdv1.CreateNetworkResponse, error) {
		return nil, want
	}
	s.RenameNetworkFn = func(context.Context, *vzdv1.RenameNetworkRequest) (*vzdv1.RenameNetworkResponse, error) {
		return nil, want
	}
	s.SetNetworkDNSFn = func(context.Context, *vzdv1.SetNetworkDNSRequest) (*vzdv1.SetNetworkDNSResponse, error) {
		return nil, want
	}
	s.DeleteNetworkFn = func(context.Context, *vzdv1.DeleteNetworkRequest) (*vzdv1.DeleteNetworkResponse, error) {
		return nil, want
	}
	s.SetNetworkDefaultSecurityGroupsFn = func(context.Context, *vzdv1.SetNetworkDefaultSecurityGroupsRequest) (*vzdv1.SetNetworkDefaultSecurityGroupsResponse, error) {
		return nil, want
	}
	s.ListSecurityGroupsFn = func(context.Context, *vzdv1.ListSecurityGroupsRequest) (*vzdv1.ListSecurityGroupsResponse, error) {
		return nil, want
	}
	s.CreateSecurityGroupFn = func(context.Context, *vzdv1.CreateSecurityGroupRequest) (*vzdv1.CreateSecurityGroupResponse, error) {
		return nil, want
	}
	s.RenameSecurityGroupFn = func(context.Context, *vzdv1.RenameSecurityGroupRequest) (*vzdv1.RenameSecurityGroupResponse, error) {
		return nil, want
	}
	s.SetSecurityGroupDescriptionFn = func(context.Context, *vzdv1.SetSecurityGroupDescriptionRequest) (*vzdv1.SetSecurityGroupDescriptionResponse, error) {
		return nil, want
	}
	s.SetSecurityGroupRulesFn = func(context.Context, *vzdv1.SetSecurityGroupRulesRequest) (*vzdv1.SetSecurityGroupRulesResponse, error) {
		return nil, want
	}
	s.DeleteSecurityGroupFn = func(context.Context, *vzdv1.DeleteSecurityGroupRequest) (*vzdv1.DeleteSecurityGroupResponse, error) {
		return nil, want
	}
	s.ListVolumesFn = func(context.Context, *vzdv1.ListVolumesRequest) (*vzdv1.ListVolumesResponse, error) {
		return nil, want
	}
	s.CreateVolumeFn = func(context.Context, *vzdv1.CreateVolumeRequest) (*vzdv1.CreateVolumeResponse, error) {
		return nil, want
	}
	s.RenameVolumeFn = func(context.Context, *vzdv1.RenameVolumeRequest) (*vzdv1.RenameVolumeResponse, error) {
		return nil, want
	}
	s.ResizeVolumeFn = func(context.Context, *vzdv1.ResizeVolumeRequest) (*vzdv1.ResizeVolumeResponse, error) {
		return nil, want
	}
	s.AttachVolumeFn = func(context.Context, *vzdv1.AttachVolumeRequest) (*vzdv1.AttachVolumeResponse, error) {
		return nil, want
	}
	s.DetachVolumeFn = func(context.Context, *vzdv1.DetachVolumeRequest) (*vzdv1.DetachVolumeResponse, error) {
		return nil, want
	}
	s.DeleteVolumeFn = func(context.Context, *vzdv1.DeleteVolumeRequest) (*vzdv1.DeleteVolumeResponse, error) {
		return nil, want
	}
	s.RenderNATSAuthorizationFn = func(context.Context, *vzdv1.RenderNATSAuthorizationRequest) (*vzdv1.RenderNATSAuthorizationResponse, error) {
		return nil, want
	}
	s.RegisterHostFn = func(context.Context, *vzdv1.RegisterHostRequest) (*vzdv1.RegisterHostResponse, error) {
		return nil, want
	}
	s.ListHostsFn = func(context.Context, *vzdv1.ListHostsRequest) (*vzdv1.ListHostsResponse, error) {
		return nil, want
	}
	s.GetHostFn = func(context.Context, *vzdv1.GetHostRequest) (*vzdv1.GetHostResponse, error) {
		return nil, want
	}
	s.HeartbeatHostFn = func(context.Context, *vzdv1.HeartbeatHostRequest) (*vzdv1.HeartbeatHostResponse, error) {
		return nil, want
	}
	s.SetHostStateFn = func(context.Context, *vzdv1.SetHostStateRequest) (*vzdv1.SetHostStateResponse, error) {
		return nil, want
	}
	s.SetHostLabelsFn = func(context.Context, *vzdv1.SetHostLabelsRequest) (*vzdv1.SetHostLabelsResponse, error) {
		return nil, want
	}
	s.DeleteHostFn = func(context.Context, *vzdv1.DeleteHostRequest) (*vzdv1.DeleteHostResponse, error) {
		return nil, want
	}

	// Spot-check a few RPCs end-to-end. The override fires the
	// sentinel; the rest of the methods are exercised the same way
	// — invocation is enough to flip the `if Fn != nil` branch.
	_, err := c.ListVMs(ctx, &vzdv1.ListVMsRequest{})
	expectErr(t, "ListVMs", err)
	_, err = c.VMStatus(ctx, &vzdv1.VMStatusRequest{Name: "x"})
	expectErr(t, "VMStatus", err)
	_, err = c.StartVM(ctx, &vzdv1.StartVMRequest{})
	expectErr(t, "StartVM", err)
	_, err = c.StopVM(ctx, &vzdv1.StopVMRequest{})
	expectErr(t, "StopVM", err)
	_, err = c.CreateVM(ctx, &vzdv1.CreateVMRequest{})
	expectErr(t, "CreateVM", err)
	_, err = c.DeleteVM(ctx, &vzdv1.DeleteVMRequest{})
	expectErr(t, "DeleteVM", err)
	_, err = c.ProvisionVM(ctx, &vzdv1.ProvisionVMRequest{})
	expectErr(t, "ProvisionVM", err)
	_, err = c.DeprovisionVM(ctx, &vzdv1.DeprovisionVMRequest{})
	expectErr(t, "DeprovisionVM", err)
	_, err = c.PullImages(ctx, &vzdv1.PullImagesRequest{})
	expectErr(t, "PullImages", err)
	_, err = c.PullImage(ctx, &vzdv1.PullImageRequest{})
	expectErr(t, "PullImage", err)
	_, err = c.PatchImage(ctx, &vzdv1.PatchImageRequest{})
	expectErr(t, "PatchImage", err)
	_, err = c.ListImages(ctx, &vzdv1.ListImagesRequest{})
	expectErr(t, "ListImages", err)
	_, err = c.CleanImages(ctx, &vzdv1.CleanImagesRequest{})
	expectErr(t, "CleanImages", err)
	_, err = c.WaitVM(ctx, &vzdv1.WaitVMRequest{})
	expectErr(t, "WaitVM", err)
	_, err = c.RegisterMicroVM(ctx, &vzdv1.RegisterMicroVMRequest{})
	expectErr(t, "RegisterMicroVM", err)
	_, err = c.VMTimings(ctx, &vzdv1.VMTimingsRequest{})
	expectErr(t, "VMTimings", err)
	_, err = c.VMLogs(ctx, &vzdv1.VMLogsRequest{})
	expectErr(t, "VMLogs", err)
	_, err = c.ListProjects(ctx, &vzdv1.ListProjectsRequest{})
	expectErr(t, "ListProjects", err)
	_, err = c.CreateProject(ctx, &vzdv1.CreateProjectRequest{})
	expectErr(t, "CreateProject", err)
	_, err = c.RenameProject(ctx, &vzdv1.RenameProjectRequest{})
	expectErr(t, "RenameProject", err)
	_, err = c.DeleteProject(ctx, &vzdv1.DeleteProjectRequest{})
	expectErr(t, "DeleteProject", err)
	_, err = c.AddProjectMember(ctx, &vzdv1.AddProjectMemberRequest{})
	expectErr(t, "AddProjectMember", err)
	_, err = c.RemoveProjectMember(ctx, &vzdv1.RemoveProjectMemberRequest{})
	expectErr(t, "RemoveProjectMember", err)
	_, err = c.ListProjectMembers(ctx, &vzdv1.ListProjectMembersRequest{})
	expectErr(t, "ListProjectMembers", err)
	_, err = c.ListUsers(ctx, &vzdv1.ListUsersRequest{})
	expectErr(t, "ListUsers", err)
	_, err = c.GetUser(ctx, &vzdv1.GetUserRequest{})
	expectErr(t, "GetUser", err)
	_, err = c.Me(ctx, &vzdv1.MeRequest{})
	expectErr(t, "Me", err)
	_, err = c.SetUserDisplayName(ctx, &vzdv1.SetUserDisplayNameRequest{})
	expectErr(t, "SetUserDisplayName", err)
	_, err = c.DeleteUser(ctx, &vzdv1.DeleteUserRequest{})
	expectErr(t, "DeleteUser", err)
	_, err = c.ListNetworks(ctx, &vzdv1.ListNetworksRequest{})
	expectErr(t, "ListNetworks", err)
	_, err = c.CreateNetwork(ctx, &vzdv1.CreateNetworkRequest{})
	expectErr(t, "CreateNetwork", err)
	_, err = c.RenameNetwork(ctx, &vzdv1.RenameNetworkRequest{})
	expectErr(t, "RenameNetwork", err)
	_, err = c.SetNetworkDNS(ctx, &vzdv1.SetNetworkDNSRequest{})
	expectErr(t, "SetNetworkDNS", err)
	_, err = c.DeleteNetwork(ctx, &vzdv1.DeleteNetworkRequest{})
	expectErr(t, "DeleteNetwork", err)
	_, err = c.SetNetworkDefaultSecurityGroups(ctx, &vzdv1.SetNetworkDefaultSecurityGroupsRequest{})
	expectErr(t, "SetNetworkDefaultSecurityGroups", err)
	_, err = c.ListSecurityGroups(ctx, &vzdv1.ListSecurityGroupsRequest{})
	expectErr(t, "ListSecurityGroups", err)
	_, err = c.CreateSecurityGroup(ctx, &vzdv1.CreateSecurityGroupRequest{})
	expectErr(t, "CreateSecurityGroup", err)
	_, err = c.RenameSecurityGroup(ctx, &vzdv1.RenameSecurityGroupRequest{})
	expectErr(t, "RenameSecurityGroup", err)
	_, err = c.SetSecurityGroupDescription(ctx, &vzdv1.SetSecurityGroupDescriptionRequest{})
	expectErr(t, "SetSecurityGroupDescription", err)
	_, err = c.SetSecurityGroupRules(ctx, &vzdv1.SetSecurityGroupRulesRequest{})
	expectErr(t, "SetSecurityGroupRules", err)
	_, err = c.DeleteSecurityGroup(ctx, &vzdv1.DeleteSecurityGroupRequest{})
	expectErr(t, "DeleteSecurityGroup", err)
	_, err = c.ListVolumes(ctx, &vzdv1.ListVolumesRequest{})
	expectErr(t, "ListVolumes", err)
	_, err = c.CreateVolume(ctx, &vzdv1.CreateVolumeRequest{})
	expectErr(t, "CreateVolume", err)
	_, err = c.RenameVolume(ctx, &vzdv1.RenameVolumeRequest{})
	expectErr(t, "RenameVolume", err)
	_, err = c.ResizeVolume(ctx, &vzdv1.ResizeVolumeRequest{})
	expectErr(t, "ResizeVolume", err)
	_, err = c.AttachVolume(ctx, &vzdv1.AttachVolumeRequest{})
	expectErr(t, "AttachVolume", err)
	_, err = c.DetachVolume(ctx, &vzdv1.DetachVolumeRequest{})
	expectErr(t, "DetachVolume", err)
	_, err = c.DeleteVolume(ctx, &vzdv1.DeleteVolumeRequest{})
	expectErr(t, "DeleteVolume", err)
	_, err = c.RenderNATSAuthorization(ctx, &vzdv1.RenderNATSAuthorizationRequest{})
	expectErr(t, "RenderNATSAuthorization", err)
	_, err = c.RegisterHost(ctx, &vzdv1.RegisterHostRequest{Hostname: "h"})
	expectErr(t, "RegisterHost", err)
	_, err = c.ListHosts(ctx, &vzdv1.ListHostsRequest{})
	expectErr(t, "ListHosts", err)
	_, err = c.GetHost(ctx, &vzdv1.GetHostRequest{})
	expectErr(t, "GetHost", err)
	_, err = c.HeartbeatHost(ctx, &vzdv1.HeartbeatHostRequest{})
	expectErr(t, "HeartbeatHost", err)
	_, err = c.SetHostState(ctx, &vzdv1.SetHostStateRequest{})
	expectErr(t, "SetHostState", err)
	_, err = c.SetHostLabels(ctx, &vzdv1.SetHostLabelsRequest{})
	expectErr(t, "SetHostLabels", err)
	_, err = c.DeleteHost(ctx, &vzdv1.DeleteHostRequest{})
	expectErr(t, "DeleteHost", err)

	// Streaming override: return the sentinel so we cover the
	// "WatchEventsFn != nil" branch.
	s.WatchEventsFn = func(_ *vzdv1.WatchEventsRequest, _ grpc.ServerStreamingServer[vzdv1.PlatformEvent]) error {
		return want
	}
	stream, err := c.WatchEvents(ctx, &vzdv1.WatchEventsRequest{})
	if err != nil {
		t.Fatalf("WatchEvents init: %v", err)
	}
	if _, err := stream.Recv(); err == nil || !contains(err.Error(), want.Error()) {
		t.Errorf("WatchEvents override: got err=%v", err)
	}
}

// TestRandomSuffix is a tiny invariant: the helper must return a
// non-empty string. Just covers the line so the coverage tool is
// happy.
func TestRandomSuffix(t *testing.T) {
	if randomSuffix(t) == "" {
		t.Error("randomSuffix should never return empty")
	}
}

// contains is a tiny stdlib-free substring check — keeps the import
// set minimal and the test self-contained.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
