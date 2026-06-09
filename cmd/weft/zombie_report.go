package main

// zombie_report.go implements the V0.1.15 GetZombieReport gRPC
// handler. Returns a snapshot of the running zombiegc reconciler's
// LastReport + cumulative deleted-counter so operators can see the
// live classification without running their own classifier in the
// CLI.
//
// Read-only ; the reconciler runs on its own ticker independently
// of this RPC. Auth : RequireAdmin (cluster-wide visibility of
// every project's VMs).

import (
	"context"

	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
)

func (s *weftServer) GetZombieReport(ctx context.Context, _ *weftv1.GetZombieReportRequest) (*weftv1.GetZombieReportResponse, error) {
	if err := weft.RequireAdmin(ctx, "get zombie report"); err != nil {
		return nil, err
	}
	if s.zombieReconciler == nil {
		// Agent boot-mode without GC wiring (tests, CLI mode).
		// Return empty rather than Unimplemented — the surface is
		// always callable.
		return &weftv1.GetZombieReportResponse{}, nil
	}
	rep := s.zombieReconciler.LastReport()
	stats := s.zombieReconciler.StatsSnapshot()

	out := &weftv1.GetZombieReportResponse{
		DeletedTotal:    stats.DeletedTotal,
		ZombiesByKind:   make(map[string]int32, len(stats.ZombiesByKind)),
	}
	if !stats.LastSweepAt.IsZero() {
		out.LastSweepAtUnixNs = stats.LastSweepAt.UnixNano()
	}
	for k, v := range stats.ZombiesByKind {
		out.ZombiesByKind[string(k)] = int32(v)
	}
	out.Zombies = make([]*weftv1.ZombieEntry, 0, len(rep.Zombies))
	for _, z := range rep.Zombies {
		entry := &weftv1.ZombieEntry{
			Uuid:             z.UUID,
			Name:             z.Name,
			ProjectUuid:      z.ProjectUUID,
			HostUuid:         z.HostUUID,
			Kind:             string(z.Kind),
			Reason:           z.Reason,
			DeploymentType:   z.DeploymentType,
		}
		if !z.DetectedAt.IsZero() {
			entry.DetectedAtUnixNs = z.DetectedAt.UnixNano()
		}
		if !z.HostDownSince.IsZero() {
			entry.HostDownSinceUnixNs = z.HostDownSince.UnixNano()
		}
		out.Zombies = append(out.Zombies, entry)
	}
	return out, nil
}
