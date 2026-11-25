package weft

// boot_tokens.go holds the per-boot tokens of KVM/QEMU guests. A token is
// 32 random bytes generated at StartVM, kept in this process's memory
// only (never persisted, never logged), handed to the driver over the
// plugin RPC, and delivered by the driver to that one VM instance over a
// channel that is not at rest (QEMU fw_cfg fed from a pipe; see
// weft-driver-qemu). The GuestSecrets service requires it, next to the
// peer CID, whenever one is held for the pod. It is dropped at StopVM,
// when the VM exits, and at UnregisterVM.

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"sync"

	driversAPI "github.com/openweft/weft-drivers"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// BootTokenLen is the length of a per-boot token.
const BootTokenLen = 32

// startHypervisor starts the VM at dir. When the driver implements
// drivers.BootTokenStarter, a fresh token is generated, recorded for the
// pod, and handed over; an old driver (gRPC Unimplemented) falls back to a
// plain start with a loud warning, leaving the pod CID-only. A failed start
// forgets the token.
func (a *Adapter) startHypervisor(ctx context.Context, hyp driversAPI.HypervisorDriver, name, dir string) error {
	bs, ok := hyp.(driversAPI.BootTokenStarter)
	if !ok {
		return hyp.StartVM(ctx, dir)
	}
	tok, err := newBootToken()
	if err != nil {
		return fmt.Errorf("boot token: %w", err)
	}
	defer clear(tok)
	a.putBootToken(name, tok)
	err = bs.StartVMWithBootToken(ctx, dir, tok)
	if err == nil {
		return nil
	}
	a.ForgetBootToken(name)
	if status.Code(err) == codes.Unimplemented {
		log.Printf("weft: WARNING: driver for %s cannot deliver a boot token (old driver); guest secrets for this VM are CID-only", name)
		return hyp.StartVM(ctx, dir)
	}
	return err
}

type bootTokenStore struct {
	mu sync.RWMutex
	m  map[string][]byte
}

// randRead fills b with random bytes (crypto/rand; a test seam for its
// failure).
var randRead = rand.Read

// newBootToken returns 32 fresh random bytes.
func newBootToken() ([]byte, error) {
	t := make([]byte, BootTokenLen)
	if _, err := randRead(t); err != nil {
		return nil, err
	}
	return t, nil
}

// putBootToken records the token of podID's current boot, replacing (and
// wiping) the previous one.
func (a *Adapter) putBootToken(podID string, token []byte) {
	s := &a.bootTokens
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string][]byte)
	}
	clear(s.m[podID])
	s.m[podID] = append([]byte(nil), token...)
}

// ForgetBootToken drops (and wipes) the token of podID. Safe on unknown pods.
func (a *Adapter) ForgetBootToken(podID string) {
	s := &a.bootTokens
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.m[podID])
	delete(s.m, podID)
}

// HeldBootToken returns a copy of the token held for podID's current boot;
// ok=false means none is held (the driver cannot deliver one).
func (a *Adapter) HeldBootToken(podID string) ([]byte, bool) {
	s := &a.bootTokens
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.m[podID]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), t...), true
}
