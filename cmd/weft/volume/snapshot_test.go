package volume

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openweft/weft/cmd/weft/internal/testutil"
	weftv1 "github.com/openweft/weft-proto"
)

// TestSnapshotCommand_Structure asserts the four snapshot leaves
// are wired and named as the runbook claims.
func TestSnapshotCommand_Structure(t *testing.T) {
	cmd := snapshotCommand(strPtr("/sock"), strPtr(""), strPtr(""))
	if cmd.Use != "snapshot" {
		t.Errorf("Use = %q", cmd.Use)
	}
	want := []string{"create", "list", "restore", "delete"}
	got := map[string]bool{}
	for _, c := range cmd.Commands() {
		got[strings.SplitN(c.Use, " ", 2)[0]] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}

// TestSnapshotCreate_HappyPath drives the create RPC and asserts the
// printed line carries uuid + parent + name + size.
func TestSnapshotCreate_HappyPath(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateVolumeSnapshotFn = func(_ context.Context, in *weftv1.CreateVolumeSnapshotRequest) (*weftv1.CreateVolumeSnapshotResponse, error) {
		if in.VolumeUuid != "v-1" || in.Name != "snap-1" {
			t.Errorf("wrong req: %+v", in)
		}
		return &weftv1.CreateVolumeSnapshotResponse{Snapshot: &weftv1.VolumeSnapshotInfo{
			Uuid: "s-1", VolumeUuid: in.VolumeUuid, Name: in.Name, SizeGib: 10,
		}}, nil
	}
	cmd := snapshotCreateCmd(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"--volume=v-1", "--name=snap-1"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "s-1") || !strings.Contains(out, "snap-1") || !strings.Contains(out, "10 GiB") {
		t.Errorf("missing fields in %q", out)
	}
}

// TestSnapshotCreate_ServerError surfaces the RPC error verbatim.
func TestSnapshotCreate_ServerError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.CreateVolumeSnapshotFn = func(_ context.Context, _ *weftv1.CreateVolumeSnapshotRequest) (*weftv1.CreateVolumeSnapshotResponse, error) {
		return nil, errors.New("volume not found")
	}
	cmd := snapshotCreateCmd(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"--volume=missing", "--name=x"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "volume not found") {
		t.Errorf("err = %v", err)
	}
}

// TestSnapshotList_TableFormat covers the default (tab-separated) renderer.
func TestSnapshotList_TableFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVolumeSnapshotsFn = func(_ context.Context, _ *weftv1.ListVolumeSnapshotsRequest) (*weftv1.ListVolumeSnapshotsResponse, error) {
		return &weftv1.ListVolumeSnapshotsResponse{Snapshots: []*weftv1.VolumeSnapshotInfo{
			{Uuid: "s-1", VolumeUuid: "v-1", Project: "p", Name: "snap", SizeGib: 3, CreatedAtUnixNs: 1700000000000000000},
		}}, nil
	}
	cmd := snapshotListCmd(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs(nil)
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"UUID", "s-1", "v-1", "snap", "3 GiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

// TestSnapshotList_JSONFormat asserts --format=json drops a parseable array.
func TestSnapshotList_JSONFormat(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.ListVolumeSnapshotsFn = func(_ context.Context, _ *weftv1.ListVolumeSnapshotsRequest) (*weftv1.ListVolumeSnapshotsResponse, error) {
		return &weftv1.ListVolumeSnapshotsResponse{Snapshots: []*weftv1.VolumeSnapshotInfo{
			{Uuid: "s-1", VolumeUuid: "v-1", Project: "p", Name: "snap", SizeGib: 3, CreatedAtUnixNs: 1700000000000000000},
		}}, nil
	}
	cmd := snapshotListCmd(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"--format=json"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, `"uuid": "s-1"`) || !strings.Contains(out, `"size_gib": 3`) {
		t.Errorf("not valid json shape: %s", out)
	}
}

// TestSnapshotRestore_HappyPath drives the restore RPC.
func TestSnapshotRestore_HappyPath(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.RestoreVolumeSnapshotFn = func(_ context.Context, in *weftv1.RestoreVolumeSnapshotRequest) (*weftv1.RestoreVolumeSnapshotResponse, error) {
		if in.SnapshotUuid != "s-1" || in.NewVolumeName != "restored" {
			t.Errorf("wrong req: %+v", in)
		}
		return &weftv1.RestoreVolumeSnapshotResponse{Volume: &weftv1.VolumeInfo{
			Uuid: "v-2", Name: in.NewVolumeName, SizeGib: 10,
		}}, nil
	}
	cmd := snapshotRestoreCmd(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"--snapshot=s-1", "--new-volume=restored"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "v-2") || !strings.Contains(out, "restored") {
		t.Errorf("missing fields in %q", out)
	}
}

// TestSnapshotDelete_HappyPath drives the delete RPC and prints the uuid.
func TestSnapshotDelete_HappyPath(t *testing.T) {
	srv := testutil.NewServer(t)
	var seen string
	srv.DeleteVolumeSnapshotFn = func(_ context.Context, in *weftv1.DeleteVolumeSnapshotRequest) (*weftv1.DeleteVolumeSnapshotResponse, error) {
		seen = in.Uuid
		return &weftv1.DeleteVolumeSnapshotResponse{}, nil
	}
	cmd := snapshotDeleteCmd(strPtr(srv.Socket()), strPtr(""), strPtr(""))
	cmd.SetArgs([]string{"--uuid=s-1"})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})
	if seen != "s-1" {
		t.Errorf("server saw uuid = %q", seen)
	}
	if !strings.Contains(out, "s-1") {
		t.Errorf("missing uuid in output: %q", out)
	}
}
