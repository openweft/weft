package weft

// vsock_alloc.go hands out guest CIDs without collisions. The hash in
// AllocateVsockCID is only the starting point of a linear probe: a CID
// is taken for good only when it is claimed in the cluster-wide store
// (etcd create-if-absent, key = CID, value = "project/name") or, when
// no store with that capability is wired, in a host-local map that is
// also checked against the VM inventory.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"
)

// vsockCIDKVName is the kvStorageFactory name of the CID claim store.
const vsockCIDKVName = "vsockcids"

// maxCIDProbes bounds the linear probe; the window is ~4 billion wide,
// so exhausting this means something is badly wrong, not that the
// space is full.
const maxCIDProbes = 1 << 16

func vsockOwner(projectUUID, name string) string { return projectUUID + "/" + name }

func nextCID(c uint32) uint32 {
	if c >= VsockCIDLast {
		return VsockCIDFirst
	}
	return c + 1
}

// inventoryHolds reports whether a VM other than owner already carries
// cid on its record (VMs registered before claims existed hold hash
// CIDs that no store knows about).
func (a *Adapter) inventoryHolds(cid uint32, owner string) bool {
	if a.vmReg == nil {
		return false
	}
	for _, v := range a.vmReg.list() {
		if v.VsockCID == cid && vsockOwner(v.ProjectUUID, v.Name) != owner {
			return true
		}
	}
	return false
}

// claimVsockCID returns a CID for (projectUUID, name) that no other VM
// holds, 0 for an empty name. The same VM always gets the same CID
// back (reuse). Cluster-wide when the adapter's KV store supports
// create-if-absent; otherwise host-local (a store error also degrades
// to host-local, logged).
func (a *Adapter) claimVsockCID(projectUUID, name string) uint32 {
	start := AllocateVsockCID(projectUUID, name)
	if start == 0 {
		return 0
	}
	owner := vsockOwner(projectUUID, name)
	a.vsockMu.Lock()
	defer a.vsockMu.Unlock()

	// A VM that already carries a CID keeps it (start the probe there).
	if a.vmReg != nil {
		for _, v := range a.vmReg.list() {
			if v.Name == name && v.ProjectUUID == projectUUID && IsVsockGuestCID(v.VsockCID) {
				start = v.VsockCID
				break
			}
		}
	}

	if a.kvStorageFactory != nil {
		kv := a.kvStorageFactory(vsockCIDKVName)
		if cr, ok := kv.(KVCreator); ok {
			cid, err := a.claimInKV(kv, cr, start, owner)
			if err == nil {
				return cid
			}
			fmt.Fprintf(os.Stderr, "weft: vsock cid claim in store failed, using host-local allocation: %v\n", err)
		}
	}
	return a.claimLocal(start, owner)
}

func (a *Adapter) claimInKV(kv KVStorage, cr KVCreator, start uint32, owner string) (uint32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cid := start
	for i := 0; i < maxCIDProbes; i++ {
		key := strconv.FormatUint(uint64(cid), 10)
		if !a.inventoryHolds(cid, owner) {
			created, err := cr.CreateOne(ctx, key, []byte(owner))
			if err != nil {
				return 0, err
			}
			if created {
				return cid, nil
			}
			cur, err := kv.GetOne(ctx, key)
			if err != nil {
				return 0, err
			}
			if string(cur) == owner {
				return cid, nil
			}
		}
		cid = nextCID(cid)
	}
	return 0, fmt.Errorf("no free vsock CID within %d probes", maxCIDProbes)
}

func (a *Adapter) claimLocal(start uint32, owner string) uint32 {
	if a.vsockLocal == nil {
		a.vsockLocal = make(map[uint32]string)
	}
	cid := start
	for i := 0; i < maxCIDProbes; i++ {
		if cur, taken := a.vsockLocal[cid]; (!taken || cur == owner) && !a.inventoryHolds(cid, owner) {
			a.vsockLocal[cid] = owner
			return cid
		}
		cid = nextCID(cid)
	}
	return 0
}

// releaseVsockCID frees the claim of (projectUUID, name) when the VM
// goes away, so the CID can be reused. Only the owner's claim is
// removed.
func (a *Adapter) releaseVsockCID(projectUUID, name string, cid uint32) {
	if cid == 0 || name == "" {
		return
	}
	owner := vsockOwner(projectUUID, name)
	a.vsockMu.Lock()
	defer a.vsockMu.Unlock()
	if a.vsockLocal[cid] == owner {
		delete(a.vsockLocal, cid)
	}
	if a.kvStorageFactory == nil {
		return
	}
	kv := a.kvStorageFactory(vsockCIDKVName)
	if _, ok := kv.(KVCreator); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key := strconv.FormatUint(uint64(cid), 10)
	cur, err := kv.GetOne(ctx, key)
	if err != nil || string(cur) != owner {
		return
	}
	if err := kv.DeleteOne(ctx, key); err != nil {
		fmt.Fprintf(os.Stderr, "weft: release vsock cid %d: %v\n", cid, err)
	}
}
