// Package testutil provides a tiny gRPC server harness for the
// cmd/weft sub-package tests.
//
// Each weft subcommand calls shared.Client(socketPath, …) which dials
// a unix socket. To exercise the RunE callbacks end-to-end the tests
// stand up a real grpc.Server on a t.TempDir-rooted unix socket,
// register an overridable WeftAgentServer impl, and point the
// command at that socket. No cgo, no real weft needed.
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

	weftv1 "github.com/openweft/weft-proto"
	"google.golang.org/grpc"
)

// Server bundles a running grpc.Server with the per-RPC overrides
// the weft sub-package tests want to inject. Every field is a func
// pointer; leaving one nil makes the server return the zero-value
// response for that RPC (or, for the streaming WatchEvents, close
// the stream immediately).
type Server struct {
	weftv1.UnimplementedWeftAgentServer

	socket string
	server *grpc.Server

	// Per-RPC override hooks. Nil = zero-value response.
	ListVMsFn                         func(context.Context, *weftv1.ListVMsRequest) (*weftv1.ListVMsResponse, error)
	VMStatusFn                        func(context.Context, *weftv1.VMStatusRequest) (*weftv1.VMStatusResponse, error)
	StartVMFn                         func(context.Context, *weftv1.StartVMRequest) (*weftv1.StartVMResponse, error)
	StopVMFn                          func(context.Context, *weftv1.StopVMRequest) (*weftv1.StopVMResponse, error)
	CreateVMFn                        func(context.Context, *weftv1.CreateVMRequest) (*weftv1.CreateVMResponse, error)
	DeleteVMFn                        func(context.Context, *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error)
	ProvisionVMFn                     func(context.Context, *weftv1.ProvisionVMRequest) (*weftv1.ProvisionVMResponse, error)
	DeprovisionVMFn                   func(context.Context, *weftv1.DeprovisionVMRequest) (*weftv1.DeprovisionVMResponse, error)
	PullImagesFn                      func(context.Context, *weftv1.PullImagesRequest) (*weftv1.PullImagesResponse, error)
	PullImageFn                       func(context.Context, *weftv1.PullImageRequest) (*weftv1.PullImageResponse, error)
	PatchImageFn                      func(context.Context, *weftv1.PatchImageRequest) (*weftv1.PatchImageResponse, error)
	ListImagesFn                      func(context.Context, *weftv1.ListImagesRequest) (*weftv1.ListImagesResponse, error)
	CleanImagesFn                     func(context.Context, *weftv1.CleanImagesRequest) (*weftv1.CleanImagesResponse, error)
	WaitVMFn                          func(context.Context, *weftv1.WaitVMRequest) (*weftv1.WaitVMResponse, error)
	RegisterMicroVMFn                 func(context.Context, *weftv1.RegisterMicroVMRequest) (*weftv1.RegisterMicroVMResponse, error)
	VMTimingsFn                       func(context.Context, *weftv1.VMTimingsRequest) (*weftv1.VMTimingsResponse, error)
	VMLogsFn                          func(context.Context, *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error)
	ListProjectsFn                    func(context.Context, *weftv1.ListProjectsRequest) (*weftv1.ListProjectsResponse, error)
	CreateProjectFn                   func(context.Context, *weftv1.CreateProjectRequest) (*weftv1.CreateProjectResponse, error)
	RenameProjectFn                   func(context.Context, *weftv1.RenameProjectRequest) (*weftv1.RenameProjectResponse, error)
	DeleteProjectFn                   func(context.Context, *weftv1.DeleteProjectRequest) (*weftv1.DeleteProjectResponse, error)
	AddProjectMemberFn                func(context.Context, *weftv1.AddProjectMemberRequest) (*weftv1.AddProjectMemberResponse, error)
	RemoveProjectMemberFn             func(context.Context, *weftv1.RemoveProjectMemberRequest) (*weftv1.RemoveProjectMemberResponse, error)
	ListProjectMembersFn              func(context.Context, *weftv1.ListProjectMembersRequest) (*weftv1.ListProjectMembersResponse, error)
	ListUsersFn                       func(context.Context, *weftv1.ListUsersRequest) (*weftv1.ListUsersResponse, error)
	GetUserFn                         func(context.Context, *weftv1.GetUserRequest) (*weftv1.GetUserResponse, error)
	MeFn                              func(context.Context, *weftv1.MeRequest) (*weftv1.MeResponse, error)
	SetUserDisplayNameFn              func(context.Context, *weftv1.SetUserDisplayNameRequest) (*weftv1.SetUserDisplayNameResponse, error)
	DeleteUserFn                      func(context.Context, *weftv1.DeleteUserRequest) (*weftv1.DeleteUserResponse, error)
	ListNetworksFn                    func(context.Context, *weftv1.ListNetworksRequest) (*weftv1.ListNetworksResponse, error)
	CreateNetworkFn                   func(context.Context, *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error)
	RenameNetworkFn                   func(context.Context, *weftv1.RenameNetworkRequest) (*weftv1.RenameNetworkResponse, error)
	SetNetworkDNSFn                   func(context.Context, *weftv1.SetNetworkDNSRequest) (*weftv1.SetNetworkDNSResponse, error)
	DeleteNetworkFn                   func(context.Context, *weftv1.DeleteNetworkRequest) (*weftv1.DeleteNetworkResponse, error)
	SetNetworkDefaultSecurityGroupsFn func(context.Context, *weftv1.SetNetworkDefaultSecurityGroupsRequest) (*weftv1.SetNetworkDefaultSecurityGroupsResponse, error)
	ListSecurityGroupsFn              func(context.Context, *weftv1.ListSecurityGroupsRequest) (*weftv1.ListSecurityGroupsResponse, error)
	CreateSecurityGroupFn             func(context.Context, *weftv1.CreateSecurityGroupRequest) (*weftv1.CreateSecurityGroupResponse, error)
	RenameSecurityGroupFn             func(context.Context, *weftv1.RenameSecurityGroupRequest) (*weftv1.RenameSecurityGroupResponse, error)
	SetSecurityGroupDescriptionFn     func(context.Context, *weftv1.SetSecurityGroupDescriptionRequest) (*weftv1.SetSecurityGroupDescriptionResponse, error)
	SetSecurityGroupRulesFn           func(context.Context, *weftv1.SetSecurityGroupRulesRequest) (*weftv1.SetSecurityGroupRulesResponse, error)
	DeleteSecurityGroupFn             func(context.Context, *weftv1.DeleteSecurityGroupRequest) (*weftv1.DeleteSecurityGroupResponse, error)
	ListVolumesFn                     func(context.Context, *weftv1.ListVolumesRequest) (*weftv1.ListVolumesResponse, error)
	CreateVolumeFn                    func(context.Context, *weftv1.CreateVolumeRequest) (*weftv1.CreateVolumeResponse, error)
	RenameVolumeFn                    func(context.Context, *weftv1.RenameVolumeRequest) (*weftv1.RenameVolumeResponse, error)
	ResizeVolumeFn                    func(context.Context, *weftv1.ResizeVolumeRequest) (*weftv1.ResizeVolumeResponse, error)
	AttachVolumeFn                    func(context.Context, *weftv1.AttachVolumeRequest) (*weftv1.AttachVolumeResponse, error)
	DetachVolumeFn                    func(context.Context, *weftv1.DetachVolumeRequest) (*weftv1.DetachVolumeResponse, error)
	DeleteVolumeFn                    func(context.Context, *weftv1.DeleteVolumeRequest) (*weftv1.DeleteVolumeResponse, error)
	WatchEventsFn                     func(*weftv1.WatchEventsRequest, grpc.ServerStreamingServer[weftv1.PlatformEvent]) error
	RenderNATSAuthorizationFn         func(context.Context, *weftv1.RenderNATSAuthorizationRequest) (*weftv1.RenderNATSAuthorizationResponse, error)
	RegisterHostFn                    func(context.Context, *weftv1.RegisterHostRequest) (*weftv1.RegisterHostResponse, error)
	ListHostsFn                       func(context.Context, *weftv1.ListHostsRequest) (*weftv1.ListHostsResponse, error)
	GetHostFn                         func(context.Context, *weftv1.GetHostRequest) (*weftv1.GetHostResponse, error)
	HeartbeatHostFn                   func(context.Context, *weftv1.HeartbeatHostRequest) (*weftv1.HeartbeatHostResponse, error)
	SetHostStateFn                    func(context.Context, *weftv1.SetHostStateRequest) (*weftv1.SetHostStateResponse, error)
	SetHostLabelsFn                   func(context.Context, *weftv1.SetHostLabelsRequest) (*weftv1.SetHostLabelsResponse, error)
	DeleteHostFn                      func(context.Context, *weftv1.DeleteHostRequest) (*weftv1.DeleteHostResponse, error)
	ListFlavorsFn                     func(context.Context, *weftv1.ListFlavorsRequest) (*weftv1.ListFlavorsResponse, error)
	GetFlavorFn                       func(context.Context, *weftv1.GetFlavorRequest) (*weftv1.GetFlavorResponse, error)
	SetFlavorFn                       func(context.Context, *weftv1.SetFlavorRequest) (*weftv1.SetFlavorResponse, error)
	DeleteFlavorFn                    func(context.Context, *weftv1.DeleteFlavorRequest) (*weftv1.DeleteFlavorResponse, error)
	ListScriptsFn                     func(context.Context, *weftv1.ListScriptsRequest) (*weftv1.ListScriptsResponse, error)
	GetScriptFn                       func(context.Context, *weftv1.GetScriptRequest) (*weftv1.GetScriptResponse, error)
	SetScriptFn                       func(context.Context, *weftv1.SetScriptRequest) (*weftv1.SetScriptResponse, error)
	DeleteScriptFn                    func(context.Context, *weftv1.DeleteScriptRequest) (*weftv1.DeleteScriptResponse, error)
	ListVMPropertiesFn                func(context.Context, *weftv1.ListVMPropertiesRequest) (*weftv1.ListVMPropertiesResponse, error)
	SetVMPropertyFn                   func(context.Context, *weftv1.SetVMPropertyRequest) (*weftv1.SetVMPropertyResponse, error)
	DeleteVMPropertyFn                func(context.Context, *weftv1.DeleteVMPropertyRequest) (*weftv1.DeleteVMPropertyResponse, error)
	ListUEFIVarsFn                    func(context.Context, *weftv1.ListUEFIVarsRequest) (*weftv1.ListUEFIVarsResponse, error)
	SetUEFIVarFn                      func(context.Context, *weftv1.SetUEFIVarRequest) (*weftv1.SetUEFIVarResponse, error)
	DeleteUEFIVarFn                   func(context.Context, *weftv1.DeleteUEFIVarRequest) (*weftv1.DeleteUEFIVarResponse, error)
	ListVMSSHKeysFn                   func(context.Context, *weftv1.ListVMSSHKeysRequest) (*weftv1.ListVMSSHKeysResponse, error)
	AddVMSSHKeyFn                     func(context.Context, *weftv1.AddVMSSHKeyRequest) (*weftv1.AddVMSSHKeyResponse, error)
	RemoveVMSSHKeyFn                  func(context.Context, *weftv1.RemoveVMSSHKeyRequest) (*weftv1.RemoveVMSSHKeyResponse, error)
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
	weftv1.RegisterWeftAgentServer(srv, s)
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

func (s *Server) ListVMs(ctx context.Context, in *weftv1.ListVMsRequest) (*weftv1.ListVMsResponse, error) {
	if s.ListVMsFn != nil {
		return s.ListVMsFn(ctx, in)
	}
	return &weftv1.ListVMsResponse{}, nil
}

func (s *Server) VMStatus(ctx context.Context, in *weftv1.VMStatusRequest) (*weftv1.VMStatusResponse, error) {
	if s.VMStatusFn != nil {
		return s.VMStatusFn(ctx, in)
	}
	return &weftv1.VMStatusResponse{Vm: &weftv1.VMInfo{Name: in.Name}}, nil
}

func (s *Server) StartVM(ctx context.Context, in *weftv1.StartVMRequest) (*weftv1.StartVMResponse, error) {
	if s.StartVMFn != nil {
		return s.StartVMFn(ctx, in)
	}
	return &weftv1.StartVMResponse{}, nil
}

func (s *Server) StopVM(ctx context.Context, in *weftv1.StopVMRequest) (*weftv1.StopVMResponse, error) {
	if s.StopVMFn != nil {
		return s.StopVMFn(ctx, in)
	}
	return &weftv1.StopVMResponse{}, nil
}

func (s *Server) CreateVM(ctx context.Context, in *weftv1.CreateVMRequest) (*weftv1.CreateVMResponse, error) {
	if s.CreateVMFn != nil {
		return s.CreateVMFn(ctx, in)
	}
	return &weftv1.CreateVMResponse{}, nil
}

func (s *Server) DeleteVM(ctx context.Context, in *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error) {
	if s.DeleteVMFn != nil {
		return s.DeleteVMFn(ctx, in)
	}
	return &weftv1.DeleteVMResponse{}, nil
}

func (s *Server) ProvisionVM(ctx context.Context, in *weftv1.ProvisionVMRequest) (*weftv1.ProvisionVMResponse, error) {
	if s.ProvisionVMFn != nil {
		return s.ProvisionVMFn(ctx, in)
	}
	return &weftv1.ProvisionVMResponse{}, nil
}

func (s *Server) DeprovisionVM(ctx context.Context, in *weftv1.DeprovisionVMRequest) (*weftv1.DeprovisionVMResponse, error) {
	if s.DeprovisionVMFn != nil {
		return s.DeprovisionVMFn(ctx, in)
	}
	return &weftv1.DeprovisionVMResponse{}, nil
}

func (s *Server) PullImages(ctx context.Context, in *weftv1.PullImagesRequest) (*weftv1.PullImagesResponse, error) {
	if s.PullImagesFn != nil {
		return s.PullImagesFn(ctx, in)
	}
	return &weftv1.PullImagesResponse{}, nil
}

func (s *Server) PullImage(ctx context.Context, in *weftv1.PullImageRequest) (*weftv1.PullImageResponse, error) {
	if s.PullImageFn != nil {
		return s.PullImageFn(ctx, in)
	}
	return &weftv1.PullImageResponse{}, nil
}

func (s *Server) PatchImage(ctx context.Context, in *weftv1.PatchImageRequest) (*weftv1.PatchImageResponse, error) {
	if s.PatchImageFn != nil {
		return s.PatchImageFn(ctx, in)
	}
	return &weftv1.PatchImageResponse{}, nil
}

func (s *Server) ListImages(ctx context.Context, in *weftv1.ListImagesRequest) (*weftv1.ListImagesResponse, error) {
	if s.ListImagesFn != nil {
		return s.ListImagesFn(ctx, in)
	}
	return &weftv1.ListImagesResponse{}, nil
}

func (s *Server) CleanImages(ctx context.Context, in *weftv1.CleanImagesRequest) (*weftv1.CleanImagesResponse, error) {
	if s.CleanImagesFn != nil {
		return s.CleanImagesFn(ctx, in)
	}
	return &weftv1.CleanImagesResponse{}, nil
}

func (s *Server) WaitVM(ctx context.Context, in *weftv1.WaitVMRequest) (*weftv1.WaitVMResponse, error) {
	if s.WaitVMFn != nil {
		return s.WaitVMFn(ctx, in)
	}
	return &weftv1.WaitVMResponse{}, nil
}

func (s *Server) RegisterMicroVM(ctx context.Context, in *weftv1.RegisterMicroVMRequest) (*weftv1.RegisterMicroVMResponse, error) {
	if s.RegisterMicroVMFn != nil {
		return s.RegisterMicroVMFn(ctx, in)
	}
	return &weftv1.RegisterMicroVMResponse{}, nil
}

func (s *Server) VMTimings(ctx context.Context, in *weftv1.VMTimingsRequest) (*weftv1.VMTimingsResponse, error) {
	if s.VMTimingsFn != nil {
		return s.VMTimingsFn(ctx, in)
	}
	return &weftv1.VMTimingsResponse{}, nil
}

func (s *Server) VMLogs(ctx context.Context, in *weftv1.VMLogsRequest) (*weftv1.VMLogsResponse, error) {
	if s.VMLogsFn != nil {
		return s.VMLogsFn(ctx, in)
	}
	return &weftv1.VMLogsResponse{}, nil
}

func (s *Server) ListProjects(ctx context.Context, in *weftv1.ListProjectsRequest) (*weftv1.ListProjectsResponse, error) {
	if s.ListProjectsFn != nil {
		return s.ListProjectsFn(ctx, in)
	}
	return &weftv1.ListProjectsResponse{}, nil
}

func (s *Server) CreateProject(ctx context.Context, in *weftv1.CreateProjectRequest) (*weftv1.CreateProjectResponse, error) {
	if s.CreateProjectFn != nil {
		return s.CreateProjectFn(ctx, in)
	}
	return &weftv1.CreateProjectResponse{Project: &weftv1.ProjectInfo{Name: in.Name}, Created: true}, nil
}

func (s *Server) RenameProject(ctx context.Context, in *weftv1.RenameProjectRequest) (*weftv1.RenameProjectResponse, error) {
	if s.RenameProjectFn != nil {
		return s.RenameProjectFn(ctx, in)
	}
	return &weftv1.RenameProjectResponse{Project: &weftv1.ProjectInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
}

func (s *Server) DeleteProject(ctx context.Context, in *weftv1.DeleteProjectRequest) (*weftv1.DeleteProjectResponse, error) {
	if s.DeleteProjectFn != nil {
		return s.DeleteProjectFn(ctx, in)
	}
	return &weftv1.DeleteProjectResponse{}, nil
}

func (s *Server) AddProjectMember(ctx context.Context, in *weftv1.AddProjectMemberRequest) (*weftv1.AddProjectMemberResponse, error) {
	if s.AddProjectMemberFn != nil {
		return s.AddProjectMemberFn(ctx, in)
	}
	return &weftv1.AddProjectMemberResponse{}, nil
}

func (s *Server) RemoveProjectMember(ctx context.Context, in *weftv1.RemoveProjectMemberRequest) (*weftv1.RemoveProjectMemberResponse, error) {
	if s.RemoveProjectMemberFn != nil {
		return s.RemoveProjectMemberFn(ctx, in)
	}
	return &weftv1.RemoveProjectMemberResponse{}, nil
}

func (s *Server) ListProjectMembers(ctx context.Context, in *weftv1.ListProjectMembersRequest) (*weftv1.ListProjectMembersResponse, error) {
	if s.ListProjectMembersFn != nil {
		return s.ListProjectMembersFn(ctx, in)
	}
	return &weftv1.ListProjectMembersResponse{}, nil
}

func (s *Server) ListUsers(ctx context.Context, in *weftv1.ListUsersRequest) (*weftv1.ListUsersResponse, error) {
	if s.ListUsersFn != nil {
		return s.ListUsersFn(ctx, in)
	}
	return &weftv1.ListUsersResponse{}, nil
}

func (s *Server) GetUser(ctx context.Context, in *weftv1.GetUserRequest) (*weftv1.GetUserResponse, error) {
	if s.GetUserFn != nil {
		return s.GetUserFn(ctx, in)
	}
	return &weftv1.GetUserResponse{User: &weftv1.UserInfo{Uuid: in.Uuid}}, nil
}

func (s *Server) Me(ctx context.Context, in *weftv1.MeRequest) (*weftv1.MeResponse, error) {
	if s.MeFn != nil {
		return s.MeFn(ctx, in)
	}
	return &weftv1.MeResponse{User: &weftv1.UserInfo{}}, nil
}

func (s *Server) SetUserDisplayName(ctx context.Context, in *weftv1.SetUserDisplayNameRequest) (*weftv1.SetUserDisplayNameResponse, error) {
	if s.SetUserDisplayNameFn != nil {
		return s.SetUserDisplayNameFn(ctx, in)
	}
	return &weftv1.SetUserDisplayNameResponse{User: &weftv1.UserInfo{Uuid: in.Uuid, DisplayName: in.DisplayName}}, nil
}

func (s *Server) DeleteUser(ctx context.Context, in *weftv1.DeleteUserRequest) (*weftv1.DeleteUserResponse, error) {
	if s.DeleteUserFn != nil {
		return s.DeleteUserFn(ctx, in)
	}
	return &weftv1.DeleteUserResponse{}, nil
}

func (s *Server) ListNetworks(ctx context.Context, in *weftv1.ListNetworksRequest) (*weftv1.ListNetworksResponse, error) {
	if s.ListNetworksFn != nil {
		return s.ListNetworksFn(ctx, in)
	}
	return &weftv1.ListNetworksResponse{}, nil
}

func (s *Server) CreateNetwork(ctx context.Context, in *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error) {
	if s.CreateNetworkFn != nil {
		return s.CreateNetworkFn(ctx, in)
	}
	return &weftv1.CreateNetworkResponse{Network: &weftv1.NetworkInfo{Name: in.Name, Cidr: in.Cidr, Type: in.Type}}, nil
}

func (s *Server) RenameNetwork(ctx context.Context, in *weftv1.RenameNetworkRequest) (*weftv1.RenameNetworkResponse, error) {
	if s.RenameNetworkFn != nil {
		return s.RenameNetworkFn(ctx, in)
	}
	return &weftv1.RenameNetworkResponse{Network: &weftv1.NetworkInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
}

func (s *Server) SetNetworkDNS(ctx context.Context, in *weftv1.SetNetworkDNSRequest) (*weftv1.SetNetworkDNSResponse, error) {
	if s.SetNetworkDNSFn != nil {
		return s.SetNetworkDNSFn(ctx, in)
	}
	return &weftv1.SetNetworkDNSResponse{Network: &weftv1.NetworkInfo{Uuid: in.Uuid, DnsServers: in.DnsServers}}, nil
}

func (s *Server) DeleteNetwork(ctx context.Context, in *weftv1.DeleteNetworkRequest) (*weftv1.DeleteNetworkResponse, error) {
	if s.DeleteNetworkFn != nil {
		return s.DeleteNetworkFn(ctx, in)
	}
	return &weftv1.DeleteNetworkResponse{}, nil
}

func (s *Server) SetNetworkDefaultSecurityGroups(ctx context.Context, in *weftv1.SetNetworkDefaultSecurityGroupsRequest) (*weftv1.SetNetworkDefaultSecurityGroupsResponse, error) {
	if s.SetNetworkDefaultSecurityGroupsFn != nil {
		return s.SetNetworkDefaultSecurityGroupsFn(ctx, in)
	}
	return &weftv1.SetNetworkDefaultSecurityGroupsResponse{Network: &weftv1.NetworkInfo{Uuid: in.Uuid, DefaultSecurityGroupUuids: in.SecurityGroupUuids}}, nil
}

func (s *Server) ListSecurityGroups(ctx context.Context, in *weftv1.ListSecurityGroupsRequest) (*weftv1.ListSecurityGroupsResponse, error) {
	if s.ListSecurityGroupsFn != nil {
		return s.ListSecurityGroupsFn(ctx, in)
	}
	return &weftv1.ListSecurityGroupsResponse{}, nil
}

func (s *Server) CreateSecurityGroup(ctx context.Context, in *weftv1.CreateSecurityGroupRequest) (*weftv1.CreateSecurityGroupResponse, error) {
	if s.CreateSecurityGroupFn != nil {
		return s.CreateSecurityGroupFn(ctx, in)
	}
	return &weftv1.CreateSecurityGroupResponse{Group: &weftv1.SecurityGroupInfo{Name: in.Name, Description: in.Description, Rules: in.Rules}}, nil
}

func (s *Server) RenameSecurityGroup(ctx context.Context, in *weftv1.RenameSecurityGroupRequest) (*weftv1.RenameSecurityGroupResponse, error) {
	if s.RenameSecurityGroupFn != nil {
		return s.RenameSecurityGroupFn(ctx, in)
	}
	return &weftv1.RenameSecurityGroupResponse{Group: &weftv1.SecurityGroupInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
}

func (s *Server) SetSecurityGroupDescription(ctx context.Context, in *weftv1.SetSecurityGroupDescriptionRequest) (*weftv1.SetSecurityGroupDescriptionResponse, error) {
	if s.SetSecurityGroupDescriptionFn != nil {
		return s.SetSecurityGroupDescriptionFn(ctx, in)
	}
	return &weftv1.SetSecurityGroupDescriptionResponse{Group: &weftv1.SecurityGroupInfo{Uuid: in.Uuid, Description: in.Description}}, nil
}

func (s *Server) SetSecurityGroupRules(ctx context.Context, in *weftv1.SetSecurityGroupRulesRequest) (*weftv1.SetSecurityGroupRulesResponse, error) {
	if s.SetSecurityGroupRulesFn != nil {
		return s.SetSecurityGroupRulesFn(ctx, in)
	}
	return &weftv1.SetSecurityGroupRulesResponse{Group: &weftv1.SecurityGroupInfo{Uuid: in.Uuid, Rules: in.Rules}}, nil
}

func (s *Server) DeleteSecurityGroup(ctx context.Context, in *weftv1.DeleteSecurityGroupRequest) (*weftv1.DeleteSecurityGroupResponse, error) {
	if s.DeleteSecurityGroupFn != nil {
		return s.DeleteSecurityGroupFn(ctx, in)
	}
	return &weftv1.DeleteSecurityGroupResponse{}, nil
}

func (s *Server) ListVolumes(ctx context.Context, in *weftv1.ListVolumesRequest) (*weftv1.ListVolumesResponse, error) {
	if s.ListVolumesFn != nil {
		return s.ListVolumesFn(ctx, in)
	}
	return &weftv1.ListVolumesResponse{}, nil
}

func (s *Server) CreateVolume(ctx context.Context, in *weftv1.CreateVolumeRequest) (*weftv1.CreateVolumeResponse, error) {
	if s.CreateVolumeFn != nil {
		return s.CreateVolumeFn(ctx, in)
	}
	return &weftv1.CreateVolumeResponse{Volume: &weftv1.VolumeInfo{Name: in.Name, SizeGib: in.SizeGib, Format: in.Format}}, nil
}

func (s *Server) RenameVolume(ctx context.Context, in *weftv1.RenameVolumeRequest) (*weftv1.RenameVolumeResponse, error) {
	if s.RenameVolumeFn != nil {
		return s.RenameVolumeFn(ctx, in)
	}
	return &weftv1.RenameVolumeResponse{Volume: &weftv1.VolumeInfo{Uuid: in.Uuid, Name: in.NewName}}, nil
}

func (s *Server) ResizeVolume(ctx context.Context, in *weftv1.ResizeVolumeRequest) (*weftv1.ResizeVolumeResponse, error) {
	if s.ResizeVolumeFn != nil {
		return s.ResizeVolumeFn(ctx, in)
	}
	return &weftv1.ResizeVolumeResponse{Volume: &weftv1.VolumeInfo{Uuid: in.Uuid, SizeGib: in.NewSizeGib}}, nil
}

func (s *Server) AttachVolume(ctx context.Context, in *weftv1.AttachVolumeRequest) (*weftv1.AttachVolumeResponse, error) {
	if s.AttachVolumeFn != nil {
		return s.AttachVolumeFn(ctx, in)
	}
	return &weftv1.AttachVolumeResponse{Volume: &weftv1.VolumeInfo{Uuid: in.Uuid, AttachedToUuid: in.VmUuid}}, nil
}

func (s *Server) DetachVolume(ctx context.Context, in *weftv1.DetachVolumeRequest) (*weftv1.DetachVolumeResponse, error) {
	if s.DetachVolumeFn != nil {
		return s.DetachVolumeFn(ctx, in)
	}
	return &weftv1.DetachVolumeResponse{}, nil
}

func (s *Server) DeleteVolume(ctx context.Context, in *weftv1.DeleteVolumeRequest) (*weftv1.DeleteVolumeResponse, error) {
	if s.DeleteVolumeFn != nil {
		return s.DeleteVolumeFn(ctx, in)
	}
	return &weftv1.DeleteVolumeResponse{}, nil
}

func (s *Server) WatchEvents(in *weftv1.WatchEventsRequest, stream grpc.ServerStreamingServer[weftv1.PlatformEvent]) error {
	if s.WatchEventsFn != nil {
		return s.WatchEventsFn(in, stream)
	}
	return nil
}

func (s *Server) RenderNATSAuthorization(ctx context.Context, in *weftv1.RenderNATSAuthorizationRequest) (*weftv1.RenderNATSAuthorizationResponse, error) {
	if s.RenderNATSAuthorizationFn != nil {
		return s.RenderNATSAuthorizationFn(ctx, in)
	}
	return &weftv1.RenderNATSAuthorizationResponse{Config: []byte("authorization { /* test */ }")}, nil
}

func (s *Server) RegisterHost(ctx context.Context, in *weftv1.RegisterHostRequest) (*weftv1.RegisterHostResponse, error) {
	if s.RegisterHostFn != nil {
		return s.RegisterHostFn(ctx, in)
	}
	return &weftv1.RegisterHostResponse{Host: &weftv1.HostInfo{Uuid: "test-uuid", Hostname: in.Hostname, Az: in.Az, Rack: in.Rack}}, nil
}

func (s *Server) ListHosts(ctx context.Context, in *weftv1.ListHostsRequest) (*weftv1.ListHostsResponse, error) {
	if s.ListHostsFn != nil {
		return s.ListHostsFn(ctx, in)
	}
	return &weftv1.ListHostsResponse{}, nil
}

func (s *Server) GetHost(ctx context.Context, in *weftv1.GetHostRequest) (*weftv1.GetHostResponse, error) {
	if s.GetHostFn != nil {
		return s.GetHostFn(ctx, in)
	}
	return &weftv1.GetHostResponse{Host: &weftv1.HostInfo{Uuid: in.Uuid, Hostname: in.Hostname}}, nil
}

func (s *Server) HeartbeatHost(ctx context.Context, in *weftv1.HeartbeatHostRequest) (*weftv1.HeartbeatHostResponse, error) {
	if s.HeartbeatHostFn != nil {
		return s.HeartbeatHostFn(ctx, in)
	}
	return &weftv1.HeartbeatHostResponse{}, nil
}

func (s *Server) SetHostState(ctx context.Context, in *weftv1.SetHostStateRequest) (*weftv1.SetHostStateResponse, error) {
	if s.SetHostStateFn != nil {
		return s.SetHostStateFn(ctx, in)
	}
	return &weftv1.SetHostStateResponse{}, nil
}

func (s *Server) SetHostLabels(ctx context.Context, in *weftv1.SetHostLabelsRequest) (*weftv1.SetHostLabelsResponse, error) {
	if s.SetHostLabelsFn != nil {
		return s.SetHostLabelsFn(ctx, in)
	}
	return &weftv1.SetHostLabelsResponse{}, nil
}

func (s *Server) DeleteHost(ctx context.Context, in *weftv1.DeleteHostRequest) (*weftv1.DeleteHostResponse, error) {
	if s.DeleteHostFn != nil {
		return s.DeleteHostFn(ctx, in)
	}
	return &weftv1.DeleteHostResponse{}, nil
}

func (s *Server) ListFlavors(ctx context.Context, in *weftv1.ListFlavorsRequest) (*weftv1.ListFlavorsResponse, error) {
	if s.ListFlavorsFn != nil {
		return s.ListFlavorsFn(ctx, in)
	}
	return &weftv1.ListFlavorsResponse{}, nil
}

func (s *Server) GetFlavor(ctx context.Context, in *weftv1.GetFlavorRequest) (*weftv1.GetFlavorResponse, error) {
	if s.GetFlavorFn != nil {
		return s.GetFlavorFn(ctx, in)
	}
	return &weftv1.GetFlavorResponse{Flavor: &weftv1.Flavor{Name: in.Name}}, nil
}

func (s *Server) SetFlavor(ctx context.Context, in *weftv1.SetFlavorRequest) (*weftv1.SetFlavorResponse, error) {
	if s.SetFlavorFn != nil {
		return s.SetFlavorFn(ctx, in)
	}
	return &weftv1.SetFlavorResponse{Flavor: in.Flavor}, nil
}

func (s *Server) DeleteFlavor(ctx context.Context, in *weftv1.DeleteFlavorRequest) (*weftv1.DeleteFlavorResponse, error) {
	if s.DeleteFlavorFn != nil {
		return s.DeleteFlavorFn(ctx, in)
	}
	return &weftv1.DeleteFlavorResponse{Deleted: in.Name}, nil
}

func (s *Server) ListScripts(ctx context.Context, in *weftv1.ListScriptsRequest) (*weftv1.ListScriptsResponse, error) {
	if s.ListScriptsFn != nil {
		return s.ListScriptsFn(ctx, in)
	}
	return &weftv1.ListScriptsResponse{}, nil
}

func (s *Server) GetScript(ctx context.Context, in *weftv1.GetScriptRequest) (*weftv1.GetScriptResponse, error) {
	if s.GetScriptFn != nil {
		return s.GetScriptFn(ctx, in)
	}
	return &weftv1.GetScriptResponse{Script: &weftv1.Script{Name: in.Name}}, nil
}

func (s *Server) SetScript(ctx context.Context, in *weftv1.SetScriptRequest) (*weftv1.SetScriptResponse, error) {
	if s.SetScriptFn != nil {
		return s.SetScriptFn(ctx, in)
	}
	return &weftv1.SetScriptResponse{Script: in.Script}, nil
}

func (s *Server) DeleteScript(ctx context.Context, in *weftv1.DeleteScriptRequest) (*weftv1.DeleteScriptResponse, error) {
	if s.DeleteScriptFn != nil {
		return s.DeleteScriptFn(ctx, in)
	}
	return &weftv1.DeleteScriptResponse{Deleted: in.Name}, nil
}

func (s *Server) ListVMProperties(ctx context.Context, in *weftv1.ListVMPropertiesRequest) (*weftv1.ListVMPropertiesResponse, error) {
	if s.ListVMPropertiesFn != nil {
		return s.ListVMPropertiesFn(ctx, in)
	}
	return &weftv1.ListVMPropertiesResponse{}, nil
}

func (s *Server) SetVMProperty(ctx context.Context, in *weftv1.SetVMPropertyRequest) (*weftv1.SetVMPropertyResponse, error) {
	if s.SetVMPropertyFn != nil {
		return s.SetVMPropertyFn(ctx, in)
	}
	return &weftv1.SetVMPropertyResponse{Property: in.Property}, nil
}

func (s *Server) DeleteVMProperty(ctx context.Context, in *weftv1.DeleteVMPropertyRequest) (*weftv1.DeleteVMPropertyResponse, error) {
	if s.DeleteVMPropertyFn != nil {
		return s.DeleteVMPropertyFn(ctx, in)
	}
	return &weftv1.DeleteVMPropertyResponse{}, nil
}

func (s *Server) ListUEFIVars(ctx context.Context, in *weftv1.ListUEFIVarsRequest) (*weftv1.ListUEFIVarsResponse, error) {
	if s.ListUEFIVarsFn != nil {
		return s.ListUEFIVarsFn(ctx, in)
	}
	return &weftv1.ListUEFIVarsResponse{}, nil
}

func (s *Server) SetUEFIVar(ctx context.Context, in *weftv1.SetUEFIVarRequest) (*weftv1.SetUEFIVarResponse, error) {
	if s.SetUEFIVarFn != nil {
		return s.SetUEFIVarFn(ctx, in)
	}
	return &weftv1.SetUEFIVarResponse{Var: in.Var}, nil
}

func (s *Server) DeleteUEFIVar(ctx context.Context, in *weftv1.DeleteUEFIVarRequest) (*weftv1.DeleteUEFIVarResponse, error) {
	if s.DeleteUEFIVarFn != nil {
		return s.DeleteUEFIVarFn(ctx, in)
	}
	return &weftv1.DeleteUEFIVarResponse{}, nil
}

func (s *Server) ListVMSSHKeys(ctx context.Context, in *weftv1.ListVMSSHKeysRequest) (*weftv1.ListVMSSHKeysResponse, error) {
	if s.ListVMSSHKeysFn != nil {
		return s.ListVMSSHKeysFn(ctx, in)
	}
	return &weftv1.ListVMSSHKeysResponse{}, nil
}

func (s *Server) AddVMSSHKey(ctx context.Context, in *weftv1.AddVMSSHKeyRequest) (*weftv1.AddVMSSHKeyResponse, error) {
	if s.AddVMSSHKeyFn != nil {
		return s.AddVMSSHKeyFn(ctx, in)
	}
	return &weftv1.AddVMSSHKeyResponse{}, nil
}

func (s *Server) RemoveVMSSHKey(ctx context.Context, in *weftv1.RemoveVMSSHKeyRequest) (*weftv1.RemoveVMSSHKeyResponse, error) {
	if s.RemoveVMSSHKeyFn != nil {
		return s.RemoveVMSSHKeyFn(ctx, in)
	}
	return &weftv1.RemoveVMSSHKeyResponse{}, nil
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
