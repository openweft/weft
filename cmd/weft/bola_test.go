package main

// bola_test.go pins object-level authorization on the READ RPCs: a
// caller authorised on project A must be refused on project B's
// object, with the same PermissionDenied whether the object belongs to
// B or does not exist (so a UUID cannot be probed). Every case also
// runs as an owner of B, to prove the call itself works and the refusal
// comes from the authorization check.

import (
	"context"
	"errors"
	"testing"

	"github.com/openweft/weft"
	drivers "github.com/openweft/weft-drivers"
	weftv1 "github.com/openweft/weft-proto"
	"github.com/openweft/weft/pluginstore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pluginTestAdapter is a MemStorage adapter for handlers that only need
// AuthorizeProject / VisibleProjects (dev caller: every project allowed).
func pluginTestAdapter(t *testing.T) *weft.Adapter {
	t.Helper()
	factory := func(string) weft.Storage { return weft.NewMemStorage() }
	return weft.NewWithStorage(t.TempDir(), factory).(*weft.Adapter)
}

func TestReadRPCsRefuseCrossProject(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	projA, _, err := adp.CreateProject("bola-a")
	if err != nil {
		t.Fatal(err)
	}
	projB, _, err := adp.CreateProject("bola-b")
	if err != nil {
		t.Fatal(err)
	}

	bucket, _, err := adp.CreateBucket(projB.UUID, "bk", "https://s3.example.com", "r", "AK")
	if err != nil {
		t.Fatal(err)
	}
	netw, err := adp.CreateNetwork(weft.CreateNetworkSpec{ProjectUUID: projB.UUID, Name: "n", CIDR: "10.9.0.0/24"})
	if err != nil {
		t.Fatal(err)
	}
	subnet, _, err := adp.CreateSubnet(netw.UUID, "sn", "", "10.9.0.0/25", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	lb, _, err := adp.CreateLoadBalancer(projB.UUID, "lb", "10.9.0.5:80", "l4_tcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	zone, _, err := adp.CreateDNSZone(projB.UUID, "b.example.", "admin@example.com", 300)
	if err != nil {
		t.Fatal(err)
	}
	share, _, err := adp.CreateShare(projB.UUID, "sh", 1, false, "")
	if err != nil {
		t.Fatal(err)
	}
	vol, err := adp.CreateVolume(weft.CreateVolumeSpec{ProjectUUID: projB.UUID, Name: "v", SizeGiB: 1, Format: weft.VolumeFormatRaw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adp.SetVolumeProperty(vol.UUID, "k", "v"); err != nil {
		t.Fatal(err)
	}

	s.vmProps, _ = weft.LoadVMPropertyRegistry(context.Background(), weft.NewMemStorage())
	s.uefiVars, _ = weft.LoadUEFIVarRegistry(context.Background(), weft.NewMemStorage())
	s.vmKeys, _ = weft.LoadVMSSHKeyRegistry(context.Background(), weft.NewMemStorage())
	s.plugins = &fakePluginManager{installed: []pluginstore.Instance{{Name: "p", UUID: "inst-b", Project: projB.UUID}}}

	cases := []struct {
		name string
		call func(ctx context.Context) error
	}{
		{"ListBuckets", func(ctx context.Context) error {
			_, err := s.ListBuckets(ctx, &weftv1.ListBucketsRequest{Project: projB.UUID})
			return err
		}},
		{"GetBucket", func(ctx context.Context) error {
			_, err := s.GetBucket(ctx, &weftv1.GetBucketRequest{Uuid: bucket.UUID})
			return err
		}},
		{"GetBucketPolicy", func(ctx context.Context) error {
			_, err := s.GetBucketPolicy(ctx, &weftv1.GetBucketPolicyRequest{Uuid: bucket.UUID})
			return err
		}},
		{"ListSubnets", func(ctx context.Context) error {
			_, err := s.ListSubnets(ctx, &weftv1.ListSubnetsRequest{NetworkUuid: netw.UUID})
			return err
		}},
		{"GetSubnet", func(ctx context.Context) error {
			_, err := s.GetSubnet(ctx, &weftv1.GetSubnetRequest{Uuid: subnet.UUID})
			return err
		}},
		{"ListLoadBalancers", func(ctx context.Context) error {
			_, err := s.ListLoadBalancers(ctx, &weftv1.ListLoadBalancersRequest{Project: projB.UUID})
			return err
		}},
		{"GetLoadBalancer", func(ctx context.Context) error {
			_, err := s.GetLoadBalancer(ctx, &weftv1.GetLoadBalancerRequest{Uuid: lb.UUID})
			return err
		}},
		{"ListDNSZones", func(ctx context.Context) error {
			_, err := s.ListDNSZones(ctx, &weftv1.ListDNSZonesRequest{Project: projB.UUID})
			return err
		}},
		{"GetDNSZone(uuid)", func(ctx context.Context) error {
			_, err := s.GetDNSZone(ctx, &weftv1.GetDNSZoneRequest{Uuid: zone.UUID})
			return err
		}},
		{"GetDNSZone(name)", func(ctx context.Context) error {
			_, err := s.GetDNSZone(ctx, &weftv1.GetDNSZoneRequest{Name: zone.Name})
			return err
		}},
		{"ListDNSRecords", func(ctx context.Context) error {
			_, err := s.ListDNSRecords(ctx, &weftv1.ListDNSRecordsRequest{ZoneUuid: zone.UUID})
			return err
		}},
		{"ListShares", func(ctx context.Context) error {
			_, err := s.ListShares(ctx, &weftv1.ListSharesRequest{Project: projB.UUID})
			return err
		}},
		{"GetShare", func(ctx context.Context) error {
			_, err := s.GetShare(ctx, &weftv1.GetShareRequest{Uuid: share.UUID})
			return err
		}},
		{"GetVolumeProperty", func(ctx context.Context) error {
			_, err := s.GetVolumeProperty(ctx, &weftv1.GetVolumePropertyRequest{VolumeUuid: vol.UUID, Key: "k"})
			return err
		}},
		{"ListVMProperties", func(ctx context.Context) error {
			_, err := s.ListVMProperties(ctx, &weftv1.ListVMPropertiesRequest{Project: projB.UUID, VmName: "vm"})
			return err
		}},
		{"ListUEFIVars", func(ctx context.Context) error {
			_, err := s.ListUEFIVars(ctx, &weftv1.ListUEFIVarsRequest{Project: projB.UUID, VmName: "vm"})
			return err
		}},
		{"ListVMSSHKeys", func(ctx context.Context) error {
			_, err := s.ListVMSSHKeys(ctx, &weftv1.ListVMSSHKeysRequest{Project: projB.UUID, VmName: "vm"})
			return err
		}},
		{"EnablePlugin", func(ctx context.Context) error {
			_, err := s.EnablePlugin(ctx, &weftv1.EnablePluginRequest{Name: "p", InstanceUuid: "inst-b"})
			return err
		}},
		{"DisablePlugin", func(ctx context.Context) error {
			_, err := s.DisablePlugin(ctx, &weftv1.DisablePluginRequest{Name: "p", InstanceUuid: "inst-b"})
			return err
		}},
		// Last of the plugin cases: the owner-side run removes the instance.
		{"UninstallPlugin", func(ctx context.Context) error {
			_, err := s.UninstallPlugin(ctx, &weftv1.UninstallPluginRequest{Name: "p", InstanceUuid: "inst-b"})
			return err
		}},
		{"InstallPlugin", func(ctx context.Context) error {
			_, err := s.InstallPlugin(ctx, &weftv1.InstallPluginRequest{Name: "p", Project: projB.UUID})
			return err
		}},
		{"DeleteVolumeBackup", func(ctx context.Context) error {
			_, err := s.DeleteVolumeBackup(ctx, &weftv1.DeleteVolumeBackupRequest{Url: "fs:///nowhere/b"})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Authorised on A only: refused on B's object.
			if err := tc.call(scopedCtx(projA.UUID)); status.Code(err) != codes.PermissionDenied {
				t.Errorf("caller scoped to A on B's object: got %v, want PermissionDenied", err)
			}
		})
		if tc.name == "DeleteVolumeBackup" {
			continue // no block driver in this fixture: no owner-side control possible
		}
		t.Run(tc.name+"/owner", func(t *testing.T) {
			// Authorised on B: the authorization check lets the call through.
			if err := tc.call(scopedCtx(projB.UUID)); status.Code(err) == codes.PermissionDenied {
				t.Errorf("owner of B refused: %v", err)
			}
		})
	}
}

// An unknown UUID answers the same PermissionDenied as another
// project's object, so a UUID cannot be probed.
func TestReadRPCsAbsentEqualsNotYours(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	projA, _, _ := adp.CreateProject("bola-probe")
	ctx := scopedCtx(projA.UUID)
	const missing = "00000000-0000-0000-0000-000000000000"
	_, err1 := s.GetBucket(ctx, &weftv1.GetBucketRequest{Uuid: missing})
	_, err2 := s.GetBucketPolicy(ctx, &weftv1.GetBucketPolicyRequest{Uuid: missing})
	_, err3 := s.GetShare(ctx, &weftv1.GetShareRequest{Uuid: missing})
	for i, err := range []error{err1, err2, err3} {
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("probe %d: got %v, want PermissionDenied", i, err)
		}
	}
}

// An unknown network answers PermissionDenied, never an empty list.
func TestListSubnetsUnknownNetworkDenied(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	projA, _, _ := adp.CreateProject("bola-sn2")
	_, err := s.ListSubnets(scopedCtx(projA.UUID), &weftv1.ListSubnetsRequest{NetworkUuid: "00000000-0000-0000-0000-000000000000"})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("unknown network: got %v, want PermissionDenied", err)
	}
}

// Dependency failures surface as errors, never as an empty (or
// unfiltered) answer.
func TestPluginAndBackupListErrorsSurface(t *testing.T) {
	s, _ := newSnapshotTestServer(t)
	s.plugins = &fakePluginManager{listErr: errors.New("store down")}
	if _, err := s.UninstallPlugin(devCtx(), &weftv1.UninstallPluginRequest{Name: "p", InstanceUuid: "i"}); status.Code(err) != codes.Internal {
		t.Errorf("UninstallPlugin with failing store: got %v, want Internal", err)
	}
	// No caller in the context: VisibleProjects fails and must be reported.
	if _, err := s.ListInstalledPlugins(context.Background(), &weftv1.ListInstalledPluginsRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("ListInstalledPlugins without caller: got %v, want Unauthenticated", err)
	}
	if _, err := s.ListVolumeBackups(context.Background(), &weftv1.ListVolumeBackupsRequest{Target: "t"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("ListVolumeBackups without caller: got %v, want Unauthenticated", err)
	}
}

// fakeBlockVolume is a "block" VolumeDriver serving canned backups and
// recording deletes and restores.
type fakeBlockVolume struct {
	drivers.VolumeDriver
	backups  []drivers.Backup
	deleted  []string
	restored []string
}

func (*fakeBlockVolume) Name() string { return "block" }
func (f *fakeBlockVolume) ListBackups(context.Context, string, string) ([]drivers.Backup, error) {
	return f.backups, nil
}
func (f *fakeBlockVolume) DeleteBackup(_ context.Context, u string) error {
	f.deleted = append(f.deleted, u)
	return nil
}
func (f *fakeBlockVolume) RestoreBackup(_ context.Context, u string, _ drivers.VolumeSpec) error {
	f.restored = append(f.restored, u)
	return nil
}

// Backups are owned by the project stamped in their label: another
// project (or a caller with no right to an unlabelled one) can neither
// delete nor restore them; the owner can.
func TestBackupOwnership(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	projA, _, _ := adp.CreateProject("bk-a")
	projB, _, _ := adp.CreateProject("bk-b")
	fb := &fakeBlockVolume{backups: []drivers.Backup{
		{URL: "fs:///b/owned", Labels: map[string]string{"weft.project_uuid": projB.UUID}},
		{URL: "fs:///b/bare"},
	}}
	if err := adp.RegisterHostHandle("h1", &weft.HostHandle{Volume: fb}); err != nil {
		t.Fatal(err)
	}
	a, b := scopedCtx(projA.UUID), scopedCtx(projB.UUID)

	del := func(ctx context.Context, u string) error {
		_, err := s.DeleteVolumeBackup(ctx, &weftv1.DeleteVolumeBackupRequest{Url: u})
		return err
	}
	if err := del(a, "fs:///b/owned"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("A deletes B's backup: got %v, want PermissionDenied", err)
	}
	if err := del(b, "fs:///b/bare"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("non-admin deletes unlabelled backup: got %v, want PermissionDenied", err)
	}
	if err := del(a, "fs:///b/missing"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("non-admin deletes unknown backup: got %v, want PermissionDenied", err)
	}
	if len(fb.deleted) != 0 {
		t.Fatalf("refused deletes reached the driver: %v", fb.deleted)
	}
	if err := del(b, "fs:///b/owned"); err != nil {
		t.Errorf("owner deletes own backup: %v", err)
	}
	if err := del(devCtx(), "fs:///b/bare"); err != nil {
		t.Errorf("admin deletes unlabelled backup: %v", err)
	}
	if len(fb.deleted) != 2 {
		t.Errorf("driver deletes = %v, want 2", fb.deleted)
	}

	restore := func(ctx context.Context, proj string) error {
		_, err := s.RestoreVolumeBackup(ctx, &weftv1.RestoreVolumeBackupRequest{Url: "fs:///b/owned", NewVolumeName: "r", Project: proj})
		return err
	}
	if err := restore(a, projA.UUID); status.Code(err) != codes.PermissionDenied {
		t.Errorf("A restores B's backup into A: got %v, want PermissionDenied", err)
	}
	if len(fb.restored) != 0 {
		t.Fatalf("refused restore reached the driver: %v", fb.restored)
	}
	if err := restore(b, projB.UUID); status.Code(err) == codes.PermissionDenied {
		t.Errorf("owner restores own backup: got %v", err)
	}
}

// Listing with an empty network UUID would enumerate every project's
// subnets: only an admin may.
func TestListSubnetsEmptyNetworkNeedsAdmin(t *testing.T) {
	s, adp := newSnapshotTestServer(t)
	projA, _, _ := adp.CreateProject("bola-sn")
	_, err := s.ListSubnets(scopedCtx(projA.UUID), &weftv1.ListSubnetsRequest{})
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("non-admin empty network_uuid: got %v, want PermissionDenied", err)
	}
	if _, err := s.ListSubnets(devCtx(), &weftv1.ListSubnetsRequest{}); err != nil {
		t.Errorf("dev caller: %v", err)
	}
}
