package main

// zombie_report.go implements the V0.1.15 GetZombieReport + V0.1.16
// TriggerZombieSweep gRPC handlers. GetZombieReport returns a
// snapshot of the running reconciler's LastReport ;
// TriggerZombieSweep runs the reconciler immediately and returns the
// fresh result.
//
// Both are admin-gated (cluster-wide visibility of every project's
// VMs). The reconciler is still running its own ticker independently
// of these RPCs ; the trigger just races a sweep ahead of the next
// natural tick.

import (
	"context"

	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/zombiegc"
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
	return buildReport(s.zombieReconciler.LastReport(), s.zombieReconciler.StatsSnapshot()), nil
}

// TriggerZombieSweep runs the reconciler synchronously + returns the
// fresh report. Closes the V0.1.15 CLI --apply loop : operators
// previously had to wait for the next interval-tick to see GC
// effects. Admin-gated identically to GetZombieReport.
func (s *weftServer) TriggerZombieSweep(ctx context.Context, _ *weftv1.TriggerZombieSweepRequest) (*weftv1.GetZombieReportResponse, error) {
	if err := weft.RequireAdmin(ctx, "trigger zombie sweep"); err != nil {
		return nil, err
	}
	if s.zombieReconciler == nil {
		return &weftv1.GetZombieReportResponse{}, nil
	}
	rep := s.zombieReconciler.Sweep(ctx)
	stats := s.zombieReconciler.StatsSnapshot()
	return buildReport(rep, stats), nil
}

// buildReport converts the in-process zombiegc types into the proto
// response. Shared by GetZombieReport + TriggerZombieSweep since
// their wire shape is identical.
func buildReport(rep zombiegc.Report, stats zombiegc.Stats) *weftv1.GetZombieReportResponse {
	out := &weftv1.GetZombieReportResponse{
		DeletedTotal:  stats.DeletedTotal,
		ZombiesByKind: make(map[string]int32, len(stats.ZombiesByKind)),
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
			Uuid:           z.UUID,
			Name:           z.Name,
			ProjectUuid:    z.ProjectUUID,
			HostUuid:       z.HostUUID,
			Kind:           string(z.Kind),
			Reason:         z.Reason,
			DeploymentType: z.DeploymentType,
		}
		if !z.DetectedAt.IsZero() {
			entry.DetectedAtUnixNs = z.DetectedAt.UnixNano()
		}
		if !z.HostDownSince.IsZero() {
			entry.HostDownSinceUnixNs = z.HostDownSince.UnixNano()
		}
		out.Zombies = append(out.Zombies, entry)
	}
	return out
}
