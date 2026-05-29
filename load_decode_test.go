//go:build darwin

package weft

// load_decode_test.go covers the HCL-decode-error branch in every
// registry loader: a non-empty but malformed blob makes
// hclsimple.Decode fail, so loadXRegistry returns an error (which
// the Adapter init* wrappers turn into an empty-registry fallback).

import (
	"context"
	"testing"
)

// garbage is a non-empty blob that is NOT valid HCL for any of the
// registry schemas — forces the parse-error branch.
var garbageHCL = []byte("this is not valid hcl { { {")

func TestLoadVolumeRegistry_DecodeError(t *testing.T) {
	if _, err := loadVolumeRegistry(context.Background(), NewMemStorageWith(garbageHCL)); err == nil {
		t.Errorf("malformed blob should error")
	}
}

func TestLoadNetworkRegistry_DecodeError(t *testing.T) {
	if _, err := loadNetworkRegistry(context.Background(), NewMemStorageWith(garbageHCL)); err == nil {
		t.Errorf("malformed blob should error")
	}
}

func TestLoadSecurityGroupRegistry_DecodeError(t *testing.T) {
	if _, err := loadSecurityGroupRegistry(context.Background(), NewMemStorageWith(garbageHCL)); err == nil {
		t.Errorf("malformed blob should error")
	}
}

func TestLoadVMRegistry_DecodeError(t *testing.T) {
	if _, err := loadVMRegistry(context.Background(), NewMemStorageWith(garbageHCL)); err == nil {
		t.Errorf("malformed blob should error")
	}
}

func TestLoadPortRegistry_DecodeError(t *testing.T) {
	if _, err := loadPortRegistry(context.Background(), NewMemStorageWith(garbageHCL)); err == nil {
		t.Errorf("malformed blob should error")
	}
}

func TestLoadProjectRegistry_DecodeError(t *testing.T) {
	if _, err := loadProjectRegistry(context.Background(), NewMemStorageWith(garbageHCL)); err == nil {
		t.Errorf("malformed blob should error")
	}
}

func TestLoadUserRegistry_DecodeError(t *testing.T) {
	if _, err := loadUserRegistry(context.Background(), NewMemStorageWith(garbageHCL)); err == nil {
		t.Errorf("malformed blob should error")
	}
}

func TestLoadHostRegistry_DecodeError(t *testing.T) {
	if _, err := loadHostRegistry(context.Background(), NewMemStorageWith(garbageHCL)); err == nil {
		t.Errorf("malformed blob should error")
	}
}

// Also cover the Load-error branch (Storage.Load itself fails) for
// one loader — the fallback path the Adapter inits rely on.
func TestLoadVolumeRegistry_LoadError(t *testing.T) {
	if _, err := loadVolumeRegistry(context.Background(), failingStorage{}); err == nil {
		t.Errorf("Storage.Load failure should propagate")
	}
}
