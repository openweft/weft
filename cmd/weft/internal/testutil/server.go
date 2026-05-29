// Package testutil provides a tiny gRPC server harness for the
// cmd/weft sub-package tests.
//
// Each weft subcommand calls shared.Client(socketPath, …) which dials
// a unix socket. To exercise the RunE callbacks end-to-end the tests
// stand up a real grpc.Server on a t.TempDir-rooted unix socket,
// register an overridable WeftAgentServer impl, and point the
// command at that socket. No cgo, no real vzd needed.
//
// Tests are expected to mutate fields on the returned *Server to
// inject per-RPC behaviour, then dial via NewServer().Socket().
package testutil

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	vzdv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// Server bundles a running grpc.Server with the per-RPC overrides
// the weft sub-package tests want to inject. Every field is a func
// pointer; leaving one nil makes the server return the zero-value
// response for that RPC (or, for the streaming WatchEvents, close
// the stream immediately).
type Server struct {
	vzdv1.UnimplementedWeftAgentServer

	socket string
	server *grpc.Server

	// Per-RPC override hooks. Nil = zero-value response.
	ListVMsFn                         func(context.Context, *vzdv1.ListVMsRequest) (*vzdv1.ListVMsResponse, error)
	VMStatusFn                        func(context.Context, *vzdv1.VMStatusRequest) (*vzdv1.VMStatusResponse, error)
	StartVMFn                         func(context.Context, *vzdv1.StartVMRequest) (*vzdv1.StartVMResponse, error)
	StopVMFn                          func(context.Context, *vzdv1.StopVMRequest) (*vzdv1.StopVMResponse, error)
	CreateVMFn                        func(context.Context, *vzdv1.CreateVMRequest) (*vzdv1.CreateVMResponse, error)
	DeleteVMFn                        func(context.Context, *vzdv1.DeleteVMRequest) (*vzdv1.DeleteVMResponse, error)
	ProvisionVMFn                     func(context.Context, *vzdv1.ProvisionVMRequest) (*vzdv1.ProvisionVMResponse, error)
	DeprovisionVMFn                   func(context.Context, *vzdv1.DeprovisionVMRequest) (*vzdv1.DeprovisionVMResponse, error)
	PullImagesFn                      func(context.Context, *vzdv1.PullImagesRequest) (*vzdv1.PullImagesResponse, error)
	PullImageFn                       func(context.Context, *vzdv1.PullImageRequest) (*vzdv1.PullImageResponse, error)
	PatchImageFn                      func(context.Context, *vzdv1.PatchImageRequest) (*vzdv1.PatchImageResponse, error)
	ListImagesFn                      func(context.Context, *vzdv1.ListImagesRequest) (*vzdv1.ListImagesResponse, error)
	CleanImagesFn                     func(context.Context, *vzdv1.CleanImagesRequest) (*vzdv1.CleanImagesResponse, error)
	WaitVMFn                          func(context.Context, *vzdv1.WaitVMRequest) (*vzdv1.WaitVMResponse, error)
	RegisterMicroVMFn                 func(context.Context, *vzdv1.RegisterMicroVMRequest) (*vzdv1.RegisterMicroVMResponse, error)
	VMTimingsFn                       func(context.Context, *vzdv1.VMTimingsRequest) (*vzdv1.VMTimingsResponse, error)
	VMLogsFn                          func(context.Context, *vzdv1.VMLogsRequest) (*vzdv1.VMLogsResponse, error)
	ListProjectsFn                    func(context.Context, *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error)
	CreateProjectFn                   func(context.Context, *vzdv1.CreateProjectRequest) (*vzdv1.CreateProjectResponse, error)
	RenameProjectFn                   func(context.Context, *vzdv1.RenameProjectRequest) (*vzdv1.RenameProjectResponse, error)
	DeleteProjectFn                   func(context.Context, *vzdv1.DeleteProjectRequest) (*vzdv1.DeleteProjectResponse, error)
	AddProjectMemberFn                func(context.Context, *vzdv1.AddProjectMemberRequest) (*vzdv1.AddProjectMemberResponse, error)
	RemoveProjectMemberFn             func(context.Context, *vzdv1.RemoveProjectMemberRequest) (*vzdv1.RemoveProjectMemberResponse, error)
	ListProjectMembersFn              func(context.Context, *vzdv1.ListProjectMembersRequest) (*vzdv1.ListProjectMembersResponse, error)
	ListUsersFn                       func(context.Context, *vzdv1.ListUsersRequest) (*vzdv1.ListUsersResponse, error)
	GetUserFn                         func(context.Context, *vzdv1.GetUserRequest) (*vzdv1.GetUserResponse, error)
	MeFn                              func(context.Context, *vzdv1.MeRequest) (*vzdv1.MeResponse, error)
	SetUserDisplayNameFn              func(context.Context, *vzdv1.SetUserDisplayNameRequest) (*vzdv1.SetUserDisplayNameResponse, error)
	DeleteUserFn                      func(context.Context, *vzdv1.DeleteUserRequest) (*vzdv1.DeleteUserResponse, error)
	ListNetworksFn                    func(context.Context, *vzdv1.ListNetworksRequest) (*vzdv1.ListNetworksResponse, error)
	CreateNetworkFn                   func(context.Context, *vzdv1.CreateNetworkRequest) (*vzdv1.CreateNetworkResponse, error)
	RenameNetworkFn                   func(context.Context, *vzdv1.RenameNetworkRequest) (*vzdv1.RenameNetworkResponse, error)
	SetNetworkDNSFn                   func(context.Context, *vzdv1.SetNetworkDNSRequest) (*vzdv1.SetNetworkDNSResponse, error)
	DeleteNetworkFn                   func(context.Context, *vzdv1.DeleteNetworkRequest) (*vzdv1.DeleteNetworkResponse, error)
	SetNetworkDefaultSecurityGroupsFn func(context.Context, *vzdv1.SetNetworkDefaultSecurityGroupsRequest) (*vzdv1.SetNetworkDefaultSecurityGroupsResponse, error)
	ListSecurityGroupsFn              func(context.Context, *vzdv1.ListSecurityGroupsRequest) (*vzdv1.ListSecurityGroupsResponse, error)
	CreateSecurityGroupFn             func(context.Context, *vzdv1.CreateSecurityGroupRequest) (*vzdv1.CreateSecurityGroupResponse, error)
	RenameSecurityGroupFn             func(context.Context, *vzdv1.RenameSecurityGroupRequest) (*vzdv1.RenameSecurityGroupResponse, error)
	SetSecurityGroupDescriptionFn     func(context.Context, *vzdv1.SetSecurityGroupDescriptionRequest) (*vzdv1.SetSecurityGroupDescriptionResponse, error)
	SetSecurityGroupRulesFn           func(context.Context, *vzdv1.SetSecurityGroupRulesRequest) (*vzdv1.SetSecurityGroupRulesResponse, error)
	DeleteSecurityGroupFn             func(context.Context, *vzdv1.DeleteSecurityGroupRequest) (*vzdv1.DeleteSecurityGroupResponse, error)
	ListVolumesFn                     func(context.Context, *vzdv1.ListVolumesRequest) (*vzdv1.ListVolumesResponse, error)
	CreateVolumeFn                    func(context.Context, *vzdv1.CreateVolumeRequest) (*vzdv1.CreateVolumeResponse, error)
	RenameVolumeFn                    func(context.Context, *vzdv1.RenameVolumeRequest) (*vzdv1.RenameVolumeResponse, error)
	ResizeVolumeFn                    func(context.Context, *vzdv1.ResizeVolumeRequest) (*vzdv1.ResizeVolumeResponse, error)
	AttachVolumeFn                    func(context.Context, *vzdv1.AttachVolumeRequest) (*vzdv1.AttachVolumeResponse, error)
	DetachVolumeFn                    func(context.Context, *vzdv1.DetachVolumeRequest) (*vzdv1.DetachVolumeResponse, error)
	DeleteVolumeFn                    func(context.Context, *vzdv1.DeleteVolumeRequest) (*vzdv1.DeleteVolumeResponse, error)
	WatchEventsFn                     func(*vzdv1.WatchEventsRequest, grpc.ServerStreamingServer[vzdv1.PlatformEvent]) error
	RenderNATSAuthorizationFn         func(context.Context, *vzdv1.RenderNATSAuthorizationRequest) (*vzdv1.RenderNATSAuthorizationResponse, error)
	RegisterHostFn                    func(context.Context, *vzdv1.RegisterHostRequest) (*vzdv1.RegisterHostResponse, error)
	ListHostsFn                       func(context.Context, *vzdv1.ListHostsRequest) (*vzdv1.ListHostsResponse, error)
	GetHostFn                         func(context.Context, *vzdv1.GetHostRequest) (*vzdv1.GetHostResponse, error)
	HeartbeatHostFn                   func(context.Context, *vzdv1.HeartbeatHostRequest) (*vzdv1.HeartbeatHostResponse, error)
	SetHostStateFn                    func(context.Context, *vzdv1.SetHostStateRequest) (*vzdv1.SetHostStateResponse, error)
	SetHostLabelsFn                   func(context.Context, *vzdv1.SetHostLabelsRequest) (*vzdv1.SetHostLabelsResponse, error)
	DeleteHostFn                      func(context.Context, *vzdv1.DeleteHostRequest) (*vzdv1.DeleteHostResponse, error)
	ListFlavorsFn                     func(context.Context, *vzdv1.ListFlavorsRequest) (*vzdv1.ListFlavorsResponse, error)
	GetFlavorFn                       func(context.Context, *vzdv1.GetFlavorRequest) (*vzdv1.GetFlavorResponse, error)
	SetFlavorFn                       func(context.Context, *vzdv1.SetFlavorRequest) (*vzdv1.SetFlavorResponse, error)
	DeleteFlavorFn                    func(context.Context, *vzdv1.DeleteFlavorRequest) (*vzdv1.DeleteFlavorResponse, error)
	ListScriptsFn                     func(context.Context, *vzdv1.ListScriptsRequest) (*vzdv1.ListScriptsResponse, error)
	GetScriptFn                       func(context.Context, *vzdv1.GetScriptRequest) (*vzdv1.GetScriptResponse, error)
	SetScriptFn                       func(context.Context, *vzdv1.SetScriptRequest) (*vzdv1.SetScriptResponse, error)
	DeleteScriptFn                    func(context.Context, *vzdv1.DeleteScriptRequest) (*vzdv1.DeleteScriptResponse, error)
}

// NewServer stands up a grpc.Server on a unix socket and registers
// itself as the WeftAgentServer. The socket lives in t.TempDir so
// the OS reaps it when the test ends. Stop() is registered via
// t.Cleanup so callers only ever need NewServer.
func NewServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	// Keep the path short — unix socket paths are length-limited
	// (~104 chars on darwin); long t.TempDir() roots blow past that.
	// Place the sock file in /tmp instead.
	socket := filepath.Join("/tmp", "weft-test-"+randomSuffix(t)+".sock")
	_ = dir
	srv := grpc.NewServer()
	s := &Server{socket: socket, server: srv}
	vzdv1.RegisterWeftAgentServer(srv, s)
	lis, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen unix %s: %v", socket, err)
	}
	go func() {
		_ = srv.Serve(lis)
	}()
	t.Cleanup(func() {
		srv.GracefulStop()
	})
	// Spin up briefly so the listener is ready before the test
	// dials. grpc.WithBlock() on the client side does the heavy
	// lifting but a 5ms warm-up avoids spurious flakes on cold
	// machines.
	time.Sleep(5 * time.Millisecond)
	return s
}

// Socket returns the unix socket path the server listens on.
func (s *Server) Socket() string { return s.socket }

// --- RPC implementations: dispatch to the override hook if set,
//     otherwise return the zero-value response. Wins by being short. ---

func (s *Server) ListVMs(ctx context.Context, in *vzdv1.ListVMsRequest) (*vzdv1.ListVMsResponse, error) {
	if s.ListVMsFn != nil {
		return s.ListVMsFn(ctx, in)
	}
	return &vzdv1.ListVMsResponse{}, nil
}

func (s *Server) VMStatus(ctx context.Context, in *vzdv1.VMStatusRequest) (*vzdv1.VMStatusResponse, error) {
	if s.VMStatusFn != nil {
		return s.VMStatusFn(ctx, in)
	}
	return &vzdv1.VMStatusResponse{Vm: &vzdv1.VMInfo{Name: in.Name}}, nil
}

func (s *Server) StartVM(ctx context.Context, in *vzdv1.StartVMRequest) (*vzdv1.StartVMResponse, error) {
	if s.StartVMFn != nil {
		return s.StartVMFn(ctx, in)
	}
	return &vzdv1.StartVMResponse{}, nil
}

func (s *Server) StopVM(ctx context.Context, in *vzdv1.StopVMRequest) (*vzdv1.StopVMResponse, error) {
	if s.StopVMFn != nil {
		return s.StopVMFn(ctx, in)
	}
	return &vzdv1.StopVMResponse{}, nil
}

func (s *Server) CreateVM(ctx context.Context, in *vzdv1.CreateVMRequest) (*vzdv1.CreateVMResponse, error) {
	if s.CreateVMFn != nil {
		return s.CreateVMFn(ctx, in)
	}
	return &vzdv1.CreateVMResponse{}, nil
}

func (s *Server) DeleteVM(ctx context.Context, in *vzdv1.DeleteVMRequest) (*vzdv1.DeleteVMResponse, error) {
	if s.DeleteVMFn != nil {
		return s.DeleteVMFn(ctx, in)
	}
	return &vzdv1.DeleteVMResponse{}, nil
}

func (s *Server) ProvisionVM(ctx context.Context, in *vzdv1.ProvisionVMRequest) (*vzdv1.ProvisionVMResponse, error) {
	if s.ProvisionVMFn != nil {
		return s.ProvisionVMFn(ctx, in)
	}
	return &vzdv1.ProvisionVMResponse{}, nil
}

func (s *Server) DeprovisionVM(ctx context.Context, in *vzdv1.DeprovisionVMRequest) (*vzdv1.DeprovisionVMResponse, error) {
	if s.DeprovisionVMFn != nil {
		return s.DeprovisionVMFn(ctx, in)
	}
	return &vzdv1.DeprovisionVMResponse{}, nil
}

func (s *Server) PullImages(ctx context.Context, in *vzdv1.PullImagesRequest) (*vzdv1.PullImagesResponse, error) {
	if s.PullImagesFn != nil {
		return s.PullImagesFn(ctx, in)
	}
	return &vzdv1.PullImagesResponse{}, nil
}

func (s *Server) PullImage(ctx context.Context, in *vzdv1.PullImageRequest) (*vzdv1.PullImageResponse, error) {
	if s.PullImageFn != nil {
		return s.PullImageFn(ctx, in)
	}
	return &vzdv1.PullImageResponse{}, nil
}

func (s *Server) PatchImage(ctx context.Context, in *vzdv1.PatchImageRequest) (*vzdv1.PatchImageResponse, error) {
	if s.PatchImageFn != nil {
		return s.PatchImageFn(ctx, in)
	}
	return &vzdv1.PatchImageResponse{}, nil
}

func (s *Server) ListImages(ctx context.Context, in *vzdv1.ListImagesRequest) (*vzdv1.ListImagesResponse, error) {
	if s.ListImagesFn != nil {
		return s.ListImagesFn(ctx, in)
	}
	return &vzdv1.ListImagesResponse{}, nil
}

func (s *Server) CleanImages(ctx context.Context, in *vzdv1.CleanImagesRequest) (*vzdv1.CleanImagesResponse, error) {
	if s.CleanImagesFn != nil {
		return s.CleanImagesFn(ctx, in)
	}
	return &vzdv1.CleanImagesResponse{}, nil
}

func (s *Server) WaitVM(ctx context.Context, in *vzdv1.WaitVMRequest) (*vzdv1.WaitVMResponse, error) {
	if s.WaitVMFn != nil {
		return s.WaitVMFn(ctx, in)
	}
	return &vzdv1.WaitVMResponse{}, nil
}

func (s *Server) RegisterMicroVM(ctx context.Context, in *vzdv1.RegisterMicroVMRequest) (*vzdv1.RegisterMicroVMResponse, error) {
	if s.RegisterMicroVMFn != nil {
		return s.RegisterMicroVMFn(ctx, in)
	}
	return &vzdv1.RegisterMicroVMResponse{}, nil
}

func (s *Server) VMTimings(ctx context.Context, in *vzdv1.VMTimingsRequest) (*vzdv1.VMTimingsResponse, error) {
	if s.VMTimingsFn != nil {
		return s.VMTimingsFn(ctx, in)
	}
	return &vzdv1.VMTimingsResponse{}, nil
}

func (s *Server) VMLogs(ctx context.Context, in *vzdv1.VMLogsRequest) (*vzdv1.VMLogsResponse, error) {
	if s.VMLogsFn != nil {
		return s.VMLogsFn(ctx, in)
	}
	return &vzdv1.VMLogsResponse{}, nil
}

func (s *Server) ListProjects(ctx context.Context, in *vzdv1.ListProjectsRequest) (*vzdv1.ListProjectsResponse, error) {
	if s.ListProjectsFn != nil {
		return s.ListProjectsFn(ctx, in)
	}
	return &vzdv1.ListProjectsResponse{}, nil
}

func (s *Server) CreateProject(ctx context.Context, in *vzdv1.CreateProjectRequest) (*vzdv1.CreateProjectResponse, error) {
	if s.CreateProjectFn != nil {
		return s.CreateProjectFn(ctx, in)
	}
	return &vzdv1.CreateProjectResponse{Project: &vzdv1.ProjectInfo{Name: in.Name}, Created: true}, nil
}

func (s *Server) RenameProject(ctx context.Context, in *vzdv1.RenameProjectRequest) (*vzdv1.RenameProjectResponse, error) {
	if s.RenameProjectFn != nil {
		return s.RenameProjectFn(ctx, in)
	}
	return &vzdv1.RenameProjectResponse{Project: &vzdv1.ProjectInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
}

func (s *Server) DeleteProject(ctx context.Context, in *vzdv1.DeleteProjectRequest) (*vzdv1.DeleteProjectResponse, error) {
	if s.DeleteProjectFn != nil {
		return s.DeleteProjectFn(ctx, in)
	}
	return &vzdv1.DeleteProjectResponse{}, nil
}

func (s *Server) AddProjectMember(ctx context.Context, in *vzdv1.AddProjectMemberRequest) (*vzdv1.AddProjectMemberResponse, error) {
	if s.AddProjectMemberFn != nil {
		return s.AddProjectMemberFn(ctx, in)
	}
	return &vzdv1.AddProjectMemberResponse{}, nil
}

func (s *Server) RemoveProjectMember(ctx context.Context, in *vzdv1.RemoveProjectMemberRequest) (*vzdv1.RemoveProjectMemberResponse, error) {
	if s.RemoveProjectMemberFn != nil {
		return s.RemoveProjectMemberFn(ctx, in)
	}
	return &vzdv1.RemoveProjectMemberResponse{}, nil
}

func (s *Server) ListProjectMembers(ctx context.Context, in *vzdv1.ListProjectMembersRequest) (*vzdv1.ListProjectMembersResponse, error) {
	if s.ListProjectMembersFn != nil {
		return s.ListProjectMembersFn(ctx, in)
	}
	return &vzdv1.ListProjectMembersResponse{}, nil
}

func (s *Server) ListUsers(ctx context.Context, in *vzdv1.ListUsersRequest) (*vzdv1.ListUsersResponse, error) {
	if s.ListUsersFn != nil {
		return s.ListUsersFn(ctx, in)
	}
	return &vzdv1.ListUsersResponse{}, nil
}

func (s *Server) GetUser(ctx context.Context, in *vzdv1.GetUserRequest) (*vzdv1.GetUserResponse, error) {
	if s.GetUserFn != nil {
		return s.GetUserFn(ctx, in)
	}
	return &vzdv1.GetUserResponse{User: &vzdv1.UserInfo{Uuid: in.Uuid}}, nil
}

func (s *Server) Me(ctx context.Context, in *vzdv1.MeRequest) (*vzdv1.MeResponse, error) {
	if s.MeFn != nil {
		return s.MeFn(ctx, in)
	}
	return &vzdv1.MeResponse{User: &vzdv1.UserInfo{}}, nil
}

func (s *Server) SetUserDisplayName(ctx context.Context, in *vzdv1.SetUserDisplayNameRequest) (*vzdv1.SetUserDisplayNameResponse, error) {
	if s.SetUserDisplayNameFn != nil {
		return s.SetUserDisplayNameFn(ctx, in)
	}
	return &vzdv1.SetUserDisplayNameResponse{User: &vzdv1.UserInfo{Uuid: in.Uuid, DisplayName: in.DisplayName}}, nil
}

func (s *Server) DeleteUser(ctx context.Context, in *vzdv1.DeleteUserRequest) (*vzdv1.DeleteUserResponse, error) {
	if s.DeleteUserFn != nil {
		return s.DeleteUserFn(ctx, in)
	}
	return &vzdv1.DeleteUserResponse{}, nil
}

func (s *Server) ListNetworks(ctx context.Context, in *vzdv1.ListNetworksRequest) (*vzdv1.ListNetworksResponse, error) {
	if s.ListNetworksFn != nil {
		return s.ListNetworksFn(ctx, in)
	}
	return &vzdv1.ListNetworksResponse{}, nil
}

func (s *Server) CreateNetwork(ctx context.Context, in *vzdv1.CreateNetworkRequest) (*vzdv1.CreateNetworkResponse, error) {
	if s.CreateNetworkFn != nil {
		return s.CreateNetworkFn(ctx, in)
	}
	return &vzdv1.CreateNetworkResponse{Network: &vzdv1.NetworkInfo{Name: in.Name, Cidr: in.Cidr, Type: in.Type}}, nil
}

func (s *Server) RenameNetwork(ctx context.Context, in *vzdv1.RenameNetworkRequest) (*vzdv1.RenameNetworkResponse, error) {
	if s.RenameNetworkFn != nil {
		return s.RenameNetworkFn(ctx, in)
	}
	return &vzdv1.RenameNetworkResponse{Network: &vzdv1.NetworkInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
}

func (s *Server) SetNetworkDNS(ctx context.Context, in *vzdv1.SetNetworkDNSRequest) (*vzdv1.SetNetworkDNSResponse, error) {
	if s.SetNetworkDNSFn != nil {
		return s.SetNetworkDNSFn(ctx, in)
	}
	return &vzdv1.SetNetworkDNSResponse{Network: &vzdv1.NetworkInfo{Uuid: in.Uuid, DnsServers: in.DnsServers}}, nil
}

func (s *Server) DeleteNetwork(ctx context.Context, in *vzdv1.DeleteNetworkRequest) (*vzdv1.DeleteNetworkResponse, error) {
	if s.DeleteNetworkFn != nil {
		return s.DeleteNetworkFn(ctx, in)
	}
	return &vzdv1.DeleteNetworkResponse{}, nil
}

func (s *Server) SetNetworkDefaultSecurityGroups(ctx context.Context, in *vzdv1.SetNetworkDefaultSecurityGroupsRequest) (*vzdv1.SetNetworkDefaultSecurityGroupsResponse, error) {
	if s.SetNetworkDefaultSecurityGroupsFn != nil {
		return s.SetNetworkDefaultSecurityGroupsFn(ctx, in)
	}
	return &vzdv1.SetNetworkDefaultSecurityGroupsResponse{Network: &vzdv1.NetworkInfo{Uuid: in.Uuid, DefaultSecurityGroupUuids: in.SecurityGroupUuids}}, nil
}

func (s *Server) ListSecurityGroups(ctx context.Context, in *vzdv1.ListSecurityGroupsRequest) (*vzdv1.ListSecurityGroupsResponse, error) {
	if s.ListSecurityGroupsFn != nil {
		return s.ListSecurityGroupsFn(ctx, in)
	}
	return &vzdv1.ListSecurityGroupsResponse{}, nil
}

func (s *Server) CreateSecurityGroup(ctx context.Context, in *vzdv1.CreateSecurityGroupRequest) (*vzdv1.CreateSecurityGroupResponse, error) {
	if s.CreateSecurityGroupFn != nil {
		return s.CreateSecurityGroupFn(ctx, in)
	}
	return &vzdv1.CreateSecurityGroupResponse{Group: &vzdv1.SecurityGroupInfo{Name: in.Name, Description: in.Description, Rules: in.Rules}}, nil
}

func (s *Server) RenameSecurityGroup(ctx context.Context, in *vzdv1.RenameSecurityGroupRequest) (*vzdv1.RenameSecurityGroupResponse, error) {
	if s.RenameSecurityGroupFn != nil {
		return s.RenameSecurityGroupFn(ctx, in)
	}
	return &vzdv1.RenameSecurityGroupResponse{Group: &vzdv1.SecurityGroupInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
}

func (s *Server) SetSecurityGroupDescription(ctx context.Context, in *vzdv1.SetSecurityGroupDescriptionRequest) (*vzdv1.SetSecurityGroupDescriptionResponse, error) {
	if s.SetSecurityGroupDescriptionFn != nil {
		return s.SetSecurityGroupDescriptionFn(ctx, in)
	}
	return &vzdv1.SetSecurityGroupDescriptionResponse{Group: &vzdv1.SecurityGroupInfo{Uuid: in.Uuid, Description: in.Description}}, nil
}

func (s *Server) SetSecurityGroupRules(ctx context.Context, in *vzdv1.SetSecurityGroupRulesRequest) (*vzdv1.SetSecurityGroupRulesResponse, error) {
	if s.SetSecurityGroupRulesFn != nil {
		return s.SetSecurityGroupRulesFn(ctx, in)
	}
	return &vzdv1.SetSecurityGroupRulesResponse{Group: &vzdv1.SecurityGroupInfo{Uuid: in.Uuid, Rules: in.Rules}}, nil
}

func (s *Server) DeleteSecurityGroup(ctx context.Context, in *vzdv1.DeleteSecurityGroupRequest) (*vzdv1.DeleteSecurityGroupResponse, error) {
	if s.DeleteSecurityGroupFn != nil {
		return s.DeleteSecurityGroupFn(ctx, in)
	}
	return &vzdv1.DeleteSecurityGroupResponse{}, nil
}

func (s *Server) ListVolumes(ctx context.Context, in *vzdv1.ListVolumesRequest) (*vzdv1.ListVolumesResponse, error) {
	if s.ListVolumesFn != nil {
		return s.ListVolumesFn(ctx, in)
	}
	return &vzdv1.ListVolumesResponse{}, nil
}

func (s *Server) CreateVolume(ctx context.Context, in *vzdv1.CreateVolumeRequest) (*vzdv1.CreateVolumeResponse, error) {
	if s.CreateVolumeFn != nil {
		return s.CreateVolumeFn(ctx, in)
	}
	return &vzdv1.CreateVolumeResponse{Volume: &vzdv1.VolumeInfo{Name: in.Name, SizeGib: in.SizeGib, Format: in.Format}}, nil
}

func (s *Server) RenameVolume(ctx context.Context, in *vzdv1.RenameVolumeRequest) (*vzdv1.RenameVolumeResponse, error) {
	if s.RenameVolumeFn != nil {
		return s.RenameVolumeFn(ctx, in)
	}
	return &vzdv1.RenameVolumeResponse{Volume: &vzdv1.VolumeInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
}

func (s *Server) ResizeVolume(ctx context.Context, in *vzdv1.ResizeVolumeRequest) (*vzdv1.ResizeVolumeResponse, error) {
	if s.ResizeVolumeFn != nil {
		return s.ResizeVolumeFn(ctx, in)
	}
	return &vzdv1.ResizeVolumeResponse{Volume: &vzdv1.VolumeInfo{Uuid: in.Uuid, SizeGib: in.NewSizeGib}}, nil
}

func (s *Server) AttachVolume(ctx context.Context, in *vzdv1.AttachVolumeRequest) (*vzdv1.AttachVolumeResponse, error) {
	if s.AttachVolumeFn != nil {
		return s.AttachVolumeFn(ctx, in)
	}
	return &vzdv1.AttachVolumeResponse{Volume: &vzdv1.VolumeInfo{Uuid: in.Uuid, AttachedToUuid: in.VmUuid}}, nil
}

func (s *Server) DetachVolume(ctx context.Context, in *vzdv1.DetachVolumeRequest) (*vzdv1.DetachVolumeResponse, error) {
	if s.DetachVolumeFn != nil {
		return s.DetachVolumeFn(ctx, in)
	}
	return &vzdv1.DetachVolumeResponse{}, nil
}

func (s *Server) DeleteVolume(ctx context.Context, in *vzdv1.DeleteVolumeRequest) (*vzdv1.DeleteVolumeResponse, error) {
	if s.DeleteVolumeFn != nil {
		return s.DeleteVolumeFn(ctx, in)
	}
	return &vzdv1.DeleteVolumeResponse{}, nil
}

func (s *Server) WatchEvents(in *vzdv1.WatchEventsRequest, stream grpc.ServerStreamingServer[vzdv1.PlatformEvent]) error {
	if s.WatchEventsFn != nil {
		return s.WatchEventsFn(in, stream)
	}
	return nil
}

func (s *Server) RenderNATSAuthorization(ctx context.Context, in *vzdv1.RenderNATSAuthorizationRequest) (*vzdv1.RenderNATSAuthorizationResponse, error) {
	if s.RenderNATSAuthorizationFn != nil {
		return s.RenderNATSAuthorizationFn(ctx, in)
	}
	return &vzdv1.RenderNATSAuthorizationResponse{Config: []byte("authorization { /* test */ }")}, nil
}

func (s *Server) RegisterHost(ctx context.Context, in *vzdv1.RegisterHostRequest) (*vzdv1.RegisterHostResponse, error) {
	if s.RegisterHostFn != nil {
		return s.RegisterHostFn(ctx, in)
	}
	return &vzdv1.RegisterHostResponse{Host: &vzdv1.HostInfo{Uuid: "test-uuid", Hostname: in.Hostname, Az: in.Az, Rack: in.Rack}}, nil
}

func (s *Server) ListHosts(ctx context.Context, in *vzdv1.ListHostsRequest) (*vzdv1.ListHostsResponse, error) {
	if s.ListHostsFn != nil {
		return s.ListHostsFn(ctx, in)
	}
	return &vzdv1.ListHostsResponse{}, nil
}

func (s *Server) GetHost(ctx context.Context, in *vzdv1.GetHostRequest) (*vzdv1.GetHostResponse, error) {
	if s.GetHostFn != nil {
		return s.GetHostFn(ctx, in)
	}
	return &vzdv1.GetHostResponse{Host: &vzdv1.HostInfo{Uuid: in.Uuid, Hostname: in.Hostname}}, nil
}

func (s *Server) HeartbeatHost(ctx context.Context, in *vzdv1.HeartbeatHostRequest) (*vzdv1.HeartbeatHostResponse, error) {
	if s.HeartbeatHostFn != nil {
		return s.HeartbeatHostFn(ctx, in)
	}
	return &vzdv1.HeartbeatHostResponse{}, nil
}

func (s *Server) SetHostState(ctx context.Context, in *vzdv1.SetHostStateRequest) (*vzdv1.SetHostStateResponse, error) {
	if s.SetHostStateFn != nil {
		return s.SetHostStateFn(ctx, in)
	}
	return &vzdv1.SetHostStateResponse{}, nil
}

func (s *Server) SetHostLabels(ctx context.Context, in *vzdv1.SetHostLabelsRequest) (*vzdv1.SetHostLabelsResponse, error) {
	if s.SetHostLabelsFn != nil {
		return s.SetHostLabelsFn(ctx, in)
	}
	return &vzdv1.SetHostLabelsResponse{}, nil
}

func (s *Server) DeleteHost(ctx context.Context, in *vzdv1.DeleteHostRequest) (*vzdv1.DeleteHostResponse, error) {
	if s.DeleteHostFn != nil {
		return s.DeleteHostFn(ctx, in)
	}
	return &vzdv1.DeleteHostResponse{}, nil
}

func (s *Server) ListFlavors(ctx context.Context, in *vzdv1.ListFlavorsRequest) (*vzdv1.ListFlavorsResponse, error) {
	if s.ListFlavorsFn != nil {
		return s.ListFlavorsFn(ctx, in)
	}
	return &vzdv1.ListFlavorsResponse{}, nil
}

func (s *Server) GetFlavor(ctx context.Context, in *vzdv1.GetFlavorRequest) (*vzdv1.GetFlavorResponse, error) {
	if s.GetFlavorFn != nil {
		return s.GetFlavorFn(ctx, in)
	}
	return &vzdv1.GetFlavorResponse{Flavor: &vzdv1.Flavor{Name: in.Name}}, nil
}

func (s *Server) SetFlavor(ctx context.Context, in *vzdv1.SetFlavorRequest) (*vzdv1.SetFlavorResponse, error) {
	if s.SetFlavorFn != nil {
		return s.SetFlavorFn(ctx, in)
	}
	return &vzdv1.SetFlavorResponse{Flavor: in.Flavor}, nil
}

func (s *Server) DeleteFlavor(ctx context.Context, in *vzdv1.DeleteFlavorRequest) (*vzdv1.DeleteFlavorResponse, error) {
	if s.DeleteFlavorFn != nil {
		return s.DeleteFlavorFn(ctx, in)
	}
	return &vzdv1.DeleteFlavorResponse{Deleted: in.Name}, nil
}

func (s *Server) ListScripts(ctx context.Context, in *vzdv1.ListScriptsRequest) (*vzdv1.ListScriptsResponse, error) {
	if s.ListScriptsFn != nil {
		return s.ListScriptsFn(ctx, in)
	}
	return &vzdv1.ListScriptsResponse{}, nil
}

func (s *Server) GetScript(ctx context.Context, in *vzdv1.GetScriptRequest) (*vzdv1.GetScriptResponse, error) {
	if s.GetScriptFn != nil {
		return s.GetScriptFn(ctx, in)
	}
	return &vzdv1.GetScriptResponse{Script: &vzdv1.Script{Name: in.Name}}, nil
}

func (s *Server) SetScript(ctx context.Context, in *vzdv1.SetScriptRequest) (*vzdv1.SetScriptResponse, error) {
	if s.SetScriptFn != nil {
		return s.SetScriptFn(ctx, in)
	}
	return &vzdv1.SetScriptResponse{Script: in.Script}, nil
}

func (s *Server) DeleteScript(ctx context.Context, in *vzdv1.DeleteScriptRequest) (*vzdv1.DeleteScriptResponse, error) {
	if s.DeleteScriptFn != nil {
		return s.DeleteScriptFn(ctx, in)
	}
	return &vzdv1.DeleteScriptResponse{Deleted: in.Name}, nil
}

// randomSuffix returns a per-test unique-ish suffix (test name +
// nanosecond counter) so concurrent t.Parallel runs don't clash on
// the /tmp socket path.
func randomSuffix(t *testing.T) string {
	t.Helper()
	// time.Now().UnixNano() is enough — tests are quick and the
	// suffix is also used in tempdir creation.
	return time.Now().Format("150405.000000000")
}
