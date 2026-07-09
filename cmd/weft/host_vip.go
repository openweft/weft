package main

// host_vip.go bootstraps the hostvip.Controller from environment
// variables when the agent boots on a control-plane host. Pure-Go
// equivalent of keepalived/VRRP : etcd lease election + gratuitous
// ARP, no apt-installed daemon. The TUI dials the VIP instead of any
// individual host, so a CP failover doesn't disrupt the operator's
// session.
//
// Configuration is env-var-driven for the V0.1 rollout (no HCL
// schema change yet so existing cluster.hcl keep working). The
// `weft up` planner can populate these via Environment= lines in the
// systemd unit ; until then operators set them manually on the
// CP hosts only :
//
//	WEFT_VIP_ADDRESS    192.168.105.100/24
//	WEFT_VIP_INTERFACE  enp0s1
//	WEFT_VIP_LEASE_TTL  5     (optional, seconds, default 5)
//
// Setting WEFT_VIP_ADDRESS empty / unset = no controller starts.

import (
	"context"
	"log"
	"log/slog"
	"net/netip"
	"os"
	"strconv"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/openweft/weft/hostvip"
)

// startHostVIP spins up a Controller goroutine when the env config
// is populated. Returns a closer the caller defers to terminate the
// Run loop on agent shutdown ; nil when the feature is disabled.
//
// Logs every transition through the supplied logger AND through
// slog.Default() (NATS fan-out) so the failover is visible in the
// same observability stream as the rest of the agent.
func startHostVIP(cli *clientv3.Client, hostUUID string, logger *log.Logger) func() {
	addrStr := os.Getenv("WEFT_VIP_ADDRESS")
	if addrStr == "" {
		return nil
	}
	iface := os.Getenv("WEFT_VIP_INTERFACE")
	if iface == "" {
		logger.Printf("hostvip: WEFT_VIP_ADDRESS=%s set but WEFT_VIP_INTERFACE empty ; VIP disabled", addrStr)
		return nil
	}
	prefix, err := netip.ParsePrefix(addrStr)
	if err != nil {
		logger.Printf("hostvip: invalid WEFT_VIP_ADDRESS=%q : %v ; VIP disabled", addrStr, err)
		return nil
	}
	ttl := 5
	if s := os.Getenv("WEFT_VIP_LEASE_TTL"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v > 0 {
			ttl = v
		}
	}

	ctrl, err := hostvip.NewController(hostvip.Config{
		Address:   prefix,
		Interface: iface,
		LeaseTTL:  ttl,
		Identity:  hostUUID,
		Logger:    slog.Default().With("component", "hostvip"),
	}, cli, hostvip.NewLinuxReconciler())
	if err != nil {
		logger.Printf("hostvip: controller init failed : %v ; VIP disabled", err)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := ctrl.Run(ctx); err != nil {
			logger.Printf("hostvip: Run exited with error : %v", err)
		}
	}()
	logger.Printf("hostvip: controller started ; address=%s iface=%s ttl=%ds host=%s", prefix, iface, ttl, hostUUID)

	return func() {
		cancel()
		_ = ctrl.Close()
	}
}
