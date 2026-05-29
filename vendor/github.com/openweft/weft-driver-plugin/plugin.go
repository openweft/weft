package weftdriverplugin

// plugin.go wires the four driver services into one HashiCorp go-plugin over
// gRPC. A single plugin process per host serves all four services on one
// connection; the host dispenses a *DriverSet of client stubs.
//
// The contract was anticipated by weft-drivers/doc.go: the driver interfaces
// were "designed from day one … so [they] can later be swapped for a go-plugin
// process … without touching call sites." This package is that swap.

import (
	"context"

	plugin "github.com/hashicorp/go-plugin"
	"github.com/openweft/weft-driver-plugin/driverpb"
	"google.golang.org/grpc"
)

// PluginName is the dispense key shared by host and plugin.
const PluginName = "weft_driver"

// Handshake gates host/plugin compatibility. A mismatched magic cookie makes
// the plugin refuse to talk (and print a human hint when run directly);
// ProtocolVersion bumps whenever the driverpb contract changes incompatibly.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "WEFT_DRIVER_PLUGIN",
	MagicCookieValue: "f0b9c1e2-weft-driver-grpc",
}

// Launch-time configuration is passed to the plugin process via these env
// vars (read in the plugin's main before it constructs its driver bundle).
// They're launch-static, so env beats an Init RPC.
const (
	EnvHostUUID = "WEFT_DRIVER_HOST_UUID"
	EnvHostname = "WEFT_DRIVER_HOSTNAME"
	EnvAZ       = "WEFT_DRIVER_AZ"
	EnvStateDir = "WEFT_DRIVER_STATE_DIR"
)

// BundlePlugin is the go-plugin adapter. On the plugin side Impl carries the
// concrete drivers; on the host side it's nil and GRPCClient builds stubs.
type BundlePlugin struct {
	plugin.NetRPCUnsupportedPlugin
	Impl *DriverSet
}

// GRPCServer registers all four services on the plugin's gRPC server.
func (p *BundlePlugin) GRPCServer(_ *plugin.GRPCBroker, s *grpc.Server) error {
	driverpb.RegisterHypervisorServer(s, &hypervisorServer{impl: p.Impl.Hypervisor})
	driverpb.RegisterNetworkServer(s, &networkServer{impl: p.Impl.Network})
	driverpb.RegisterVolumeServer(s, &volumeServer{impl: p.Impl.Volume})
	driverpb.RegisterImageServer(s, &imageServer{impl: p.Impl.Image})
	return nil
}

// GRPCClient builds the four client stubs over the single shared connection
// and returns them as a *DriverSet.
func (p *BundlePlugin) GRPCClient(_ context.Context, _ *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &DriverSet{
		Hypervisor: &hypervisorClient{c: driverpb.NewHypervisorClient(c)},
		Network:    &networkClient{c: driverpb.NewNetworkClient(c)},
		Volume:     &volumeClient{c: driverpb.NewVolumeClient(c)},
		Image:      &imageClient{c: driverpb.NewImageClient(c)},
	}, nil
}

// pluginMap is the single-entry dispense table shared by host and plugin.
func pluginMap() map[string]plugin.Plugin {
	return map[string]plugin.Plugin{PluginName: &BundlePlugin{}}
}

// Serve runs the calling process as a weft driver plugin, serving set over
// gRPC. It blocks until the host disconnects. Plugin executables call this
// from main when launched with no arguments (go-plugin handshake mode).
func Serve(set DriverSet) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         map[string]plugin.Plugin{PluginName: &BundlePlugin{Impl: &set}},
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}
