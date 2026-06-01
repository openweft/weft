package pluginstore

import (
	"context"

	weftv1 "github.com/openweft/weft-proto"
)

// AgentClient wraps a real *weftv1.WeftAgentClient and exposes the
// narrow Client interface this package needs. The wrapper drops
// the variadic grpc.CallOption from each signature so plain fakes
// (no gRPC import) can satisfy the interface in tests.
type AgentClient struct {
	c weftv1.WeftAgentClient
}

// NewAgentClient adapts a real gRPC stub.
func NewAgentClient(c weftv1.WeftAgentClient) *AgentClient { return &AgentClient{c: c} }

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
