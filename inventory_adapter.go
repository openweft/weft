package weft

// inventory_adapter.go — Adapter glue for the AZ + Rack registries.
// Kept in its own file (vs. injected into the 3,000-line adapter.go)
// because the inventory hierarchy is its own subsystem : the
// registries are loaded together, and DeleteAZ / DeleteRack need
// cross-registry visibility for cascade safety that doesn't belong
// on the registry types themselves.

import (
	"context"
	"fmt"
	"os"
)

// initInventory loads the AZ + Rack registries from their Storage
// backends. Mirrors initProjects' pattern : a registry whose Load
// errors out gets replaced by an empty in-memory copy so subsequent
// mutations still try to save (and likely succeed if the underlying
// blip was transient).
//
// Wired from the same call sites that already invoke initProjects()
// at Adapter startup — see the matching NewWithStorage in adapter.go.
func (a *Adapter) initInventory() {
	if err := os.MkdirAll(a.vmsDir(), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "weft: mkdir vmsDir: %v\n", err)
	}

	azStorage := a.storageFactory(azRegistryFileName)
	azReg, err := loadAZRegistry(context.Background(), azStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load az registry: %v\n", err)
		azReg = &azRegistry{
			storage: azStorage,
			byUUID:  make(map[string]AZ),
			codeIdx: make(map[string]string),
		}
	}
	a.azReg = azReg

	rackStorage := a.storageFactory(rackRegistryFileName)
	rackReg, err := loadRackRegistry(context.Background(), rackStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load rack registry: %v\n", err)
		rackReg = &rackRegistry{
			storage: rackStorage,
			byUUID:  make(map[string]Rack),
			byKey:   make(map[rackKey]string),
		}
	}
	a.rackReg = rackReg
}

// AZs returns a snapshot of every registered AZ + the derived
// rack + host counts the proto's AZInfo carries.
func (a *Adapter) AZs() []AZ {
	if a.azReg == nil {
		return nil
	}
	return a.azReg.list()
}

// AZByUUID resolves a UUID. Empty UUID returns (zero, false).
func (a *Adapter) AZByUUID(uuid string) (AZ, bool) {
	if a.azReg == nil {
		return AZ{}, false
	}
	return a.azReg.lookupByUUID(uuid)
}

// AZByCode resolves the immutable short identifier ("DC-A").
func (a *Adapter) AZByCode(code string) (AZ, bool) {
	if a.azReg == nil {
		return AZ{}, false
	}
	return a.azReg.lookupByCode(code)
}

// AZRackCount + AZHostCount surface the derived counts the proto's
// AZInfo carries (so the response doesn't depend on the client
// running its own ListRacks / ListHosts round-trips).
func (a *Adapter) AZRackCount(azUUID string) int32 {
	if a.rackReg == nil {
		return 0
	}
	return a.rackReg.countForAZ(azUUID)
}

// AZHostCount counts hosts whose AZ field matches the row's code.
// Hosts carry the AZ code (not UUID) for backwards compatibility
// with the long-standing RegisterHostRequest.az = string convention.
func (a *Adapter) AZHostCount(azUUID string) int32 {
	if a.hostReg == nil || a.azReg == nil {
		return 0
	}
	az, ok := a.azReg.lookupByUUID(azUUID)
	if !ok {
		return 0
	}
	return a.hostCountByAZCode(az.Code)
}

// hostCountByAZCode walks the host registry counting matches on
// HostInfo.AZ. Cheap (the host inventory is single-digit to low
// tens for the typical 3-DC cluster) ; if it ever becomes
// hot-path-relevant we'd add a secondary index.
func (a *Adapter) hostCountByAZCode(code string) int32 {
	hosts := a.hostReg.list()
	var n int32
	for _, h := range hosts {
		if h.AZ == code {
			n++
		}
	}
	return n
}

// CreateAZ registers a new AZ. Returns (row, created, err) where
// created=false means the code already existed (idempotent insert,
// mirrors getOrCreate on projects).
func (a *Adapter) CreateAZ(code, name, region, status string) (AZ, bool, error) {
	if a.azReg == nil {
		return AZ{}, false, fmt.Errorf("az registry not initialised")
	}
	az, created, err := a.azReg.create(code, name, region, status)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:    "az.created",
			Subject: az.UUID,
			Meta:    map[string]string{"code": az.Code, "region": az.Region},
		})
	}
	return az, created, err
}

// UpdateAZ patches mutable fields. Empty = keep current.
func (a *Adapter) UpdateAZ(uuid, name, region, status string) (AZ, error) {
	if a.azReg == nil {
		return AZ{}, fmt.Errorf("az registry not initialised")
	}
	az, err := a.azReg.update(uuid, name, region, status)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:    "az.updated",
			Subject: uuid,
			Meta:    map[string]string{"code": az.Code, "status": az.Status},
		})
	}
	return az, err
}

// DeleteAZ refuses when the AZ still has racks or hosts attached.
// Surfaces the blocking counts back to the caller so the response
// can carry them verbatim.
func (a *Adapter) DeleteAZ(uuid string) (blockedRacks, blockedHosts int32, err error) {
	if a.azReg == nil {
		return 0, 0, fmt.Errorf("az registry not initialised")
	}
	blockedRacks = a.AZRackCount(uuid)
	blockedHosts = a.AZHostCount(uuid)
	gotRacks, gotHosts, err := a.azReg.delete(uuid, blockedRacks, blockedHosts)
	if err != nil {
		// Pre-deletion guard fired — keep the counts we already
		// computed for the caller.
		if gotRacks > 0 || gotHosts > 0 {
			blockedRacks, blockedHosts = gotRacks, gotHosts
		}
		return blockedRacks, blockedHosts, err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "az.deleted",
		Subject: uuid,
	})
	return 0, 0, nil
}

// Racks returns a snapshot ; azUUID == "" lists every rack across
// every AZ.
func (a *Adapter) Racks(azUUID string) []Rack {
	if a.rackReg == nil {
		return nil
	}
	return a.rackReg.list(azUUID)
}

// RackByUUID resolves a UUID.
func (a *Adapter) RackByUUID(uuid string) (Rack, bool) {
	if a.rackReg == nil {
		return Rack{}, false
	}
	return a.rackReg.lookupByUUID(uuid)
}

// RackHostCount surfaces the derived count the proto's RackInfo
// carries. Hosts bind to a rack by its `code` for the same
// long-standing-RegisterHostRequest reason hosts bind to az by
// `code`.
func (a *Adapter) RackHostCount(rackUUID string) int32 {
	if a.hostReg == nil || a.rackReg == nil {
		return 0
	}
	rk, ok := a.rackReg.lookupByUUID(rackUUID)
	if !ok {
		return 0
	}
	var n int32
	for _, h := range a.hostReg.list() {
		if h.Rack == rk.Code {
			n++
		}
	}
	return n
}

// CreateRack registers a new rack. The AZ parent MUST exist —
// we resolve the AZ here so the registry stays decoupled from the
// AZ registry struct.
func (a *Adapter) CreateRack(azUUID, code, name, status string, heightU int32) (Rack, bool, error) {
	if a.rackReg == nil {
		return Rack{}, false, fmt.Errorf("rack registry not initialised")
	}
	_, azExists := a.AZByUUID(azUUID)
	rk, created, err := a.rackReg.create(azUUID, code, name, status, heightU, azExists)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:    "rack.created",
			Subject: rk.UUID,
			Meta: map[string]string{
				"az_uuid": rk.AZUUID,
				"code":    rk.Code,
			},
		})
	}
	return rk, created, err
}

// UpdateRack patches mutable fields. heightU = -1 means "keep
// current" (proto3 int32 has no nil).
func (a *Adapter) UpdateRack(uuid, name, status string, heightU int32) (Rack, error) {
	if a.rackReg == nil {
		return Rack{}, fmt.Errorf("rack registry not initialised")
	}
	rk, err := a.rackReg.update(uuid, name, status, heightU)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:    "rack.updated",
			Subject: uuid,
		})
	}
	return rk, err
}

// DeleteRack refuses when hosts still bind to the rack.
func (a *Adapter) DeleteRack(uuid string) (blockedHosts int32, err error) {
	if a.rackReg == nil {
		return 0, fmt.Errorf("rack registry not initialised")
	}
	blockedHosts = a.RackHostCount(uuid)
	got, err := a.rackReg.delete(uuid, blockedHosts)
	if err != nil {
		if got > 0 {
			blockedHosts = got
		}
		return blockedHosts, err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "rack.deleted",
		Subject: uuid,
	})
	return 0, nil
}
