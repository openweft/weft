package weft

// resources_adapter.go — Adapter glue for the six resource
// registries added by proto v0.9.0 :
//
//   * volumePropReg     — per-volume host-set KV annotations
//   * shareReg          — CubeFS share catalogue (registry side ;
//                         the mount fan-out lives in share.go)
//   * bucketReg         — S3 bucket catalogue (data on versitygw /
//                         CubeFS objectnode ; we track creds + policy)
//   * sshKeyCatReg      — cluster-wide named SSH key catalogue
//                         (distinct from per-VM vm_sshkeys.go)
//   * schedRuleReg      — nominal placement rules
//                         ([[openweft_nominal_binding]])
//   * registryRemReg    — OCI registry alias catalogue
//
// Mirrors inventory_adapter.go + network_plane_adapter.go : load
// per-registry via the Storage factory, resilient empty-on-error,
// mutation handlers Publish PlatformEvents.

import (
	"context"
	"fmt"
	"os"
)

// initResources loads the six registries. Same resilience contract
// as the other init* functions : a registry whose Load fails gets
// replaced by an empty in-memory copy so subsequent mutations keep
// trying to save.
func (a *Adapter) initResources() {
	vpStorage := a.storageFactory(volumePropertyRegistryFileName)
	vpReg, err := loadVolumePropertyRegistry(context.Background(), vpStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load volume-property registry: %v\n", err)
		vpReg = &volumePropertyRegistry{
			storage:  vpStorage,
			byVolume: make(map[string]map[string]VolumeProperty),
		}
	}
	a.volumePropReg = vpReg

	shStorage := a.storageFactory(shareRegistryFileName)
	shReg, err := loadShareRegistry(context.Background(), shStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load share registry: %v\n", err)
		shReg = &shareRegistry{
			storage: shStorage,
			byUUID:  make(map[string]Share),
			nameIdx: make(map[string]string),
		}
	}
	a.shareReg = shReg

	bkStorage := a.storageFactory(bucketRegistryFileName)
	bkReg, err := loadBucketRegistry(context.Background(), bkStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load bucket registry: %v\n", err)
		bkReg = &bucketRegistry{
			storage: bkStorage,
			byUUID:  make(map[string]Bucket),
			nameIdx: make(map[string]string),
		}
	}
	a.bucketReg = bkReg

	skStorage := a.storageFactory(sshKeyCatalogueRegistryFileName)
	skReg, err := loadSSHKeyCatalogueRegistry(context.Background(), skStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load sshkey catalogue registry: %v\n", err)
		skReg = &sshKeyCatalogueRegistry{
			storage: skStorage,
			byUUID:  make(map[string]SSHKeyCatalogueEntry),
			nameIdx: make(map[string]string),
			fpIdx:   make(map[string]string),
		}
	}
	a.sshKeyCatReg = skReg

	srStorage := a.storageFactory(schedulingRuleRegistryFileName)
	srReg, err := loadSchedulingRuleRegistry(context.Background(), srStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load scheduling rule registry: %v\n", err)
		srReg = &schedulingRuleRegistry{
			storage: srStorage,
			byUUID:  make(map[string]SchedulingRuleEntry),
			nameIdx: make(map[string]string),
		}
	}
	a.schedRuleReg = srReg

	rrStorage := a.storageFactory(registryRemoteRegistryFileName)
	rrReg, err := loadRegistryRemoteRegistry(context.Background(), rrStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load registry remote registry: %v\n", err)
		rrReg = &registryRemoteRegistry{
			storage: rrStorage,
			byUUID:  make(map[string]RegistryRemote),
			nameIdx: make(map[string]string),
		}
	}
	a.registryRemReg = rrReg
}

// --- Volume properties ------------------------------------------

func (a *Adapter) GetVolumeProperty(volumeUUID, key string) (VolumeProperty, bool) {
	if a.volumePropReg == nil {
		return VolumeProperty{}, false
	}
	return a.volumePropReg.get(volumeUUID, key)
}

func (a *Adapter) SetVolumeProperty(volumeUUID, key, value string) (VolumeProperty, error) {
	if a.volumePropReg == nil {
		return VolumeProperty{}, fmt.Errorf("volume property registry not initialised")
	}
	p, err := a.volumePropReg.set(volumeUUID, key, value)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:    "volume.property.set",
			Subject: volumeUUID,
			Meta:    map[string]string{"key": key},
		})
	}
	return p, err
}

func (a *Adapter) DeleteVolumeProperty(volumeUUID, key string) error {
	if a.volumePropReg == nil {
		return fmt.Errorf("volume property registry not initialised")
	}
	if err := a.volumePropReg.delete(volumeUUID, key); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:    "volume.property.deleted",
		Subject: volumeUUID,
		Meta:    map[string]string{"key": key},
	})
	return nil
}

// --- Shares -----------------------------------------------------

func (a *Adapter) Shares(projectUUID string) []Share {
	if a.shareReg == nil {
		return nil
	}
	return a.shareReg.list(projectUUID)
}

func (a *Adapter) ShareByUUID(uuid string) (Share, bool) {
	if a.shareReg == nil {
		return Share{}, false
	}
	return a.shareReg.lookupByUUID(uuid)
}

func (a *Adapter) CreateShare(projectUUID, name string, sizeGB int64, readonly bool, backend string) (Share, bool, error) {
	if a.shareReg == nil {
		return Share{}, false, fmt.Errorf("share registry not initialised")
	}
	s, created, err := a.shareReg.create(projectUUID, name, sizeGB, readonly, backend)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:        "share.created",
			Subject:     s.UUID,
			ProjectUUID: s.ProjectUUID,
			Meta:        map[string]string{"name": s.Name, "backend": s.Backend},
		})
	}
	return s, created, err
}

func (a *Adapter) ResizeShare(uuid string, newSizeGB int64) (Share, error) {
	if a.shareReg == nil {
		return Share{}, fmt.Errorf("share registry not initialised")
	}
	s, err := a.shareReg.resize(uuid, newSizeGB)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:        "share.resized",
			Subject:     uuid,
			ProjectUUID: s.ProjectUUID,
			Meta:        map[string]string{"size_gb": fmt.Sprintf("%d", s.SizeGB)},
		})
	}
	return s, err
}

func (a *Adapter) DeleteShare(uuid string) error {
	if a.shareReg == nil {
		return fmt.Errorf("share registry not initialised")
	}
	prev, _ := a.shareReg.lookupByUUID(uuid)
	if err := a.shareReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "share.deleted",
		Subject:     uuid,
		ProjectUUID: prev.ProjectUUID,
	})
	return nil
}

// --- Buckets ----------------------------------------------------

func (a *Adapter) Buckets(projectUUID string) []Bucket {
	if a.bucketReg == nil {
		return nil
	}
	return a.bucketReg.list(projectUUID)
}

func (a *Adapter) BucketByUUID(uuid string) (Bucket, bool) {
	if a.bucketReg == nil {
		return Bucket{}, false
	}
	return a.bucketReg.lookupByUUID(uuid)
}

func (a *Adapter) CreateBucket(projectUUID, name, endpoint, region, accessKey, secretKey string) (Bucket, bool, error) {
	if a.bucketReg == nil {
		return Bucket{}, false, fmt.Errorf("bucket registry not initialised")
	}
	b, created, err := a.bucketReg.create(projectUUID, name, endpoint, region, accessKey, secretKey)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:        "bucket.created",
			Subject:     b.UUID,
			ProjectUUID: b.ProjectUUID,
			Meta:        map[string]string{"name": b.Name, "endpoint": b.Endpoint},
		})
	}
	return b, created, err
}

func (a *Adapter) SetBucketPolicy(uuid, policy string) (Bucket, error) {
	if a.bucketReg == nil {
		return Bucket{}, fmt.Errorf("bucket registry not initialised")
	}
	b, err := a.bucketReg.setPolicy(uuid, policy)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:        "bucket.policy.set",
			Subject:     uuid,
			ProjectUUID: b.ProjectUUID,
		})
	}
	return b, err
}

func (a *Adapter) DeleteBucket(uuid string) error {
	if a.bucketReg == nil {
		return fmt.Errorf("bucket registry not initialised")
	}
	prev, _ := a.bucketReg.lookupByUUID(uuid)
	if err := a.bucketReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "bucket.deleted",
		Subject:     uuid,
		ProjectUUID: prev.ProjectUUID,
	})
	return nil
}

// --- SSH key catalogue ------------------------------------------

func (a *Adapter) SSHKeyCatalogue() []SSHKeyCatalogueEntry {
	if a.sshKeyCatReg == nil {
		return nil
	}
	return a.sshKeyCatReg.list()
}

func (a *Adapter) SSHKeyCatalogueByUUID(uuid string) (SSHKeyCatalogueEntry, bool) {
	if a.sshKeyCatReg == nil {
		return SSHKeyCatalogueEntry{}, false
	}
	return a.sshKeyCatReg.lookupByUUID(uuid)
}

func (a *Adapter) SSHKeyCatalogueByName(name string) (SSHKeyCatalogueEntry, bool) {
	if a.sshKeyCatReg == nil {
		return SSHKeyCatalogueEntry{}, false
	}
	return a.sshKeyCatReg.lookupByName(name)
}

func (a *Adapter) AddSSHKeyCatalogue(name, publicKey, comment string) (SSHKeyCatalogueEntry, bool, error) {
	if a.sshKeyCatReg == nil {
		return SSHKeyCatalogueEntry{}, false, fmt.Errorf("sshkey catalogue registry not initialised")
	}
	k, added, err := a.sshKeyCatReg.add(name, publicKey, comment)
	if err == nil && added {
		a.bus.Publish(PlatformEvent{
			Kind:    "sshkey.catalogue.added",
			Subject: k.UUID,
			Meta:    map[string]string{"name": k.Name, "fingerprint": k.Fingerprint},
		})
	}
	return k, added, err
}

func (a *Adapter) RemoveSSHKeyCatalogue(uuid string) error {
	if a.sshKeyCatReg == nil {
		return fmt.Errorf("sshkey catalogue registry not initialised")
	}
	if err := a.sshKeyCatReg.remove(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{Kind: "sshkey.catalogue.removed", Subject: uuid})
	return nil
}

func (a *Adapter) ImportSSHKeyCatalogue(namePrefix, blob, comment string) ([]SSHKeyCatalogueEntry, int32, error) {
	if a.sshKeyCatReg == nil {
		return nil, 0, fmt.Errorf("sshkey catalogue registry not initialised")
	}
	return a.sshKeyCatReg.importBlob(namePrefix, blob, comment)
}

// --- Scheduling rules -------------------------------------------

func (a *Adapter) SchedulingRules() []SchedulingRuleEntry {
	if a.schedRuleReg == nil {
		return nil
	}
	return a.schedRuleReg.list()
}

func (a *Adapter) SchedulingRuleByUUID(uuid string) (SchedulingRuleEntry, bool) {
	if a.schedRuleReg == nil {
		return SchedulingRuleEntry{}, false
	}
	return a.schedRuleReg.lookupByUUID(uuid)
}

func (a *Adapter) CreateSchedulingRule(name, selector string, targetCount int32, antiAffinity string) (SchedulingRuleEntry, bool, error) {
	if a.schedRuleReg == nil {
		return SchedulingRuleEntry{}, false, fmt.Errorf("scheduling rule registry not initialised")
	}
	sr, created, err := a.schedRuleReg.create(name, selector, targetCount, antiAffinity)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:    "schedulingrule.created",
			Subject: sr.UUID,
			Meta:    map[string]string{"name": sr.Name, "selector": sr.Selector},
		})
	}
	return sr, created, err
}

func (a *Adapter) UpdateSchedulingRule(uuid, selector string, targetCount int32, antiAffinity string) (SchedulingRuleEntry, error) {
	if a.schedRuleReg == nil {
		return SchedulingRuleEntry{}, fmt.Errorf("scheduling rule registry not initialised")
	}
	sr, err := a.schedRuleReg.update(uuid, selector, targetCount, antiAffinity)
	if err == nil {
		a.bus.Publish(PlatformEvent{Kind: "schedulingrule.updated", Subject: uuid})
	}
	return sr, err
}

func (a *Adapter) DeleteSchedulingRule(uuid string) error {
	if a.schedRuleReg == nil {
		return fmt.Errorf("scheduling rule registry not initialised")
	}
	if err := a.schedRuleReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{Kind: "schedulingrule.deleted", Subject: uuid})
	return nil
}

// --- Registry remotes -------------------------------------------

func (a *Adapter) RegistryRemotes() []RegistryRemote {
	if a.registryRemReg == nil {
		return nil
	}
	return a.registryRemReg.list()
}

func (a *Adapter) RegistryRemoteByUUID(uuid string) (RegistryRemote, bool) {
	if a.registryRemReg == nil {
		return RegistryRemote{}, false
	}
	return a.registryRemReg.lookupByUUID(uuid)
}

func (a *Adapter) RegistryRemoteByName(name string) (RegistryRemote, bool) {
	if a.registryRemReg == nil {
		return RegistryRemote{}, false
	}
	return a.registryRemReg.lookupByName(name)
}

func (a *Adapter) SetRegistryRemote(name, endpoint string, insecure bool, credSecretRef string) (RegistryRemote, bool, error) {
	if a.registryRemReg == nil {
		return RegistryRemote{}, false, fmt.Errorf("registry remote registry not initialised")
	}
	rr, created, err := a.registryRemReg.set(name, endpoint, insecure, credSecretRef)
	if err == nil {
		kind := "registryremote.updated"
		if created {
			kind = "registryremote.created"
		}
		a.bus.Publish(PlatformEvent{
			Kind:    kind,
			Subject: rr.UUID,
			Meta:    map[string]string{"name": rr.Name, "endpoint": rr.Endpoint},
		})
	}
	return rr, created, err
}

func (a *Adapter) DeleteRegistryRemote(uuid string) error {
	if a.registryRemReg == nil {
		return fmt.Errorf("registry remote registry not initialised")
	}
	if err := a.registryRemReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{Kind: "registryremote.deleted", Subject: uuid})
	return nil
}
