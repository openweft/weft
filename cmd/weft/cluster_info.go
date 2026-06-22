package main

// cluster_info.go owns the GetClusterInfo + SetClusterName handlers
// that back the TUI title-bar / webui chrome decoration showing
// which federated cluster the operator is currently connected to.
//
// The cluster name is persisted at the etcd key /weft/cluster/name
// when the agent has an etcd client wired (production HA) and falls
// back to a per-process state file when running in single-host dev
// without etcd. The state file lives at <stateDir>/cluster-name to
// stay alongside the other agent metadata (host-uuid, etc.).

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	weft "github.com/openweft/weft"
	weftv1 "github.com/openweft/weft-proto"
)

// clusterNameEtcdKey is the etcd path the cluster identity lives at.
// One key per cluster (not per host) — every agent in the same etcd
// quorum reads the same value. Federation peers see DIFFERENT etcd
// clusters and therefore DIFFERENT cluster names.
const clusterNameEtcdKey = "/weft/cluster/name"

// clusterNameLocalFile is the single-host dev fallback location when
// no etcd client is configured. Lives under the agent's state dir so
// it survives a process restart but doesn't claim to be cluster-wide.
const clusterNameLocalFile = "cluster-name"

// GetClusterInfo reads the persisted cluster name + reports the
// local host UUID. Open to every authenticated caller — chrome
// decoration is not sensitive.
func (s *weftServer) GetClusterInfo(ctx context.Context, _ *weftv1.GetClusterInfoRequest) (*weftv1.GetClusterInfoResponse, error) {
	name, err := s.readClusterName(ctx)
	if err != nil {
		// Not finding a name is normal (never set). Other errors
		// (etcd hiccup) come through as Unavailable so the client
		// can degrade gracefully to flag/env.
		if !errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.Unavailable, "read cluster name: %v", err)
		}
	}
	return &weftv1.GetClusterInfoResponse{
		ClusterName:   name,
		LocalHostUuid: s.localHostUUID,
	}, nil
}

// SetClusterName persists a new cluster name. Admin-only ; the
// operator runs `weft admin cluster set-name <name>` exactly once
// per cluster at provisioning time.
func (s *weftServer) SetClusterName(ctx context.Context, req *weftv1.SetClusterNameRequest) (*weftv1.SetClusterNameResponse, error) {
	if err := weft.RequireAdmin(ctx, "set cluster name"); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.ClusterName)
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "cluster_name is required ; pass empty via DeleteClusterName when that exists")
	}
	if err := s.writeClusterName(ctx, name); err != nil {
		return nil, status.Errorf(codes.Unavailable, "persist cluster name: %v", err)
	}
	return &weftv1.SetClusterNameResponse{ClusterName: name}, nil
}

// readClusterName picks the right backend (etcd > local file) +
// returns the persisted value. Empty string + nil error means
// "never set" — the caller surfaces that as a 0-value response.
func (s *weftServer) readClusterName(ctx context.Context) (string, error) {
	if s.etcdCli != nil {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		resp, err := s.etcdCli.Get(readCtx, clusterNameEtcdKey)
		if err != nil {
			return "", err
		}
		if len(resp.Kvs) == 0 {
			return "", nil
		}
		return string(resp.Kvs[0].Value), nil
	}
	// Single-host dev : read from the local state file.
	path := s.clusterNameLocalPath()
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// writeClusterName persists via the chosen backend. Atomic on the
// local-file path (temp + rename) ; etcd Put is naturally atomic.
func (s *weftServer) writeClusterName(ctx context.Context, name string) error {
	if s.etcdCli != nil {
		writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := s.etcdCli.Put(writeCtx, clusterNameEtcdKey, name, clientv3.WithPrevKV())
		return err
	}
	path := s.clusterNameLocalPath()
	if path == "" {
		return errors.New("no persistence available (etcd nor state dir)")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "cluster-name-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(name + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// clusterNameLocalPath returns the on-disk path for the local
// fallback, or "" when the agent's state dir isn't resolved.
func (s *weftServer) clusterNameLocalPath() string {
	if s.cfgDir == "" {
		return ""
	}
	return filepath.Join(s.cfgDir, clusterNameLocalFile)
}
