package pluginstore

import (
	"context"
	"fmt"

	"github.com/openweft/weft-microvm"
	weftv1 "github.com/openweft/weft-proto"
)

// AgentClient wraps a real *weftv1.WeftAgentClient and exposes the
// narrow Client interface this package needs. The wrapper drops
// the variadic grpc.CallOption from each signature so plain fakes
// (no gRPC import) can satisfy the interface in tests.
//
// V0.5 : carries the agent socket path so MicroVMRun can drive the
// shared weft-microvm library (which dials its own grpc connection
// rather than reusing this one). Plain Classic plugins ignore the
// socket.
type AgentClient struct {
	c          weftv1.WeftAgentClient
	weftSocket string
}

// NewAgentClient adapts a real gRPC stub. socket is the path to the
// agent's Unix socket — passed through to microvm.Run for the
// microVM-runtime install path. Empty socket disables MicroVMRun ;
// existing classic-VM plugins are unaffected.
func NewAgentClient(c weftv1.WeftAgentClient, socket string) *AgentClient {
	return &AgentClient{c: c, weftSocket: socket}
}

func (a *AgentClient) CreateNetwork(ctx context.Context, in *weftv1.CreateNetworkRequest) (*weftv1.CreateNetworkResponse, error) {
	return a.c.CreateNetwork(ctx, in)
}
func (a *AgentClient) CreateSecurityGroup(ctx context.Context, in *weftv1.CreateSecurityGroupRequest) (*weftv1.CreateSecurityGroupResponse, error) {
	return a.c.CreateSecurityGroup(ctx, in)
}
func (a *AgentClient) SetNetworkDefaultSecurityGroups(ctx context.Context, in *weftv1.SetNetworkDefaultSecurityGroupsRequest) (*weftv1.SetNetworkDefaultSecurityGroupsResponse, error) {
	return a.c.SetNetworkDefaultSecurityGroups(ctx, in)
}
func (a *AgentClient) CreateVM(ctx context.Context, in *weftv1.CreateVMRequest) (*weftv1.CreateVMResponse, error) {
	return a.c.CreateVM(ctx, in)
}
func (a *AgentClient) DeleteVM(ctx context.Context, in *weftv1.DeleteVMRequest) (*weftv1.DeleteVMResponse, error) {
	return a.c.DeleteVM(ctx, in)
}
func (a *AgentClient) DeleteNetwork(ctx context.Context, in *weftv1.DeleteNetworkRequest) (*weftv1.DeleteNetworkResponse, error) {
	return a.c.DeleteNetwork(ctx, in)
}
func (a *AgentClient) DeleteSecurityGroup(ctx context.Context, in *weftv1.DeleteSecurityGroupRequest) (*weftv1.DeleteSecurityGroupResponse, error) {
	return a.c.DeleteSecurityGroup(ctx, in)
}
func (a *AgentClient) CreateVolume(ctx context.Context, in *weftv1.CreateVolumeRequest) (*weftv1.CreateVolumeResponse, error) {
	return a.c.CreateVolume(ctx, in)
}
func (a *AgentClient) DeleteVolume(ctx context.Context, in *weftv1.DeleteVolumeRequest) (*weftv1.DeleteVolumeResponse, error) {
	return a.c.DeleteVolume(ctx, in)
}

// MicroVMRun drives the shared weft-microvm library to register +
// start a microVM around an OCI image (auto-pulling on cache miss).
// Used by the V0.5 plugin schema when a vm block declares
// runtime = "microvm". Returns ErrMicroVMUnavailable if the
// AgentClient was constructed without a socket — the install path
// degrades to a clear "wire the socket" error rather than a silent
// classic-VM fallback.
func (a *AgentClient) MicroVMRun(ctx context.Context, image, project string) error {
	if a.weftSocket == "" {
		return fmt.Errorf("pluginstore: MicroVMRun called without an agent socket (plugin runtime=microvm requires NewAgentClient with a non-empty socket)")
	}
	return microvm.Run(microvm.Args{
		Image:      image,
		Project:    project,
		Detach:     true,
		WeftSocket: a.weftSocket,
	})
}

// SetVMProperties stamps the V0.1.8 property map on a freshly-installed VM
// so plugin-declared properties (typically deployment.type=ha + role=…)
// reach the registry. Used after MicroVMRun by the V0.5 plugin
// installer's microvm dispatch.
func (a *AgentClient) SetVMProperties(ctx context.Context, in *weftv1.SetVMPropertiesRequest) (*weftv1.SetVMPropertiesResponse, error) {
	return a.c.SetVMProperties(ctx, in)
}
