package weft

// hosts_kv.go is the per-record (KVStorage) path the Host registry
// takes when its Storage backend implements KVStorage. Each host
// becomes one etcd key at /weft/hosts/<uuid> ; mutations Put/Delete
// a single record instead of rewriting the whole blob. Watchers
// receive per-record diffs and apply them surgically.
//
// Pattern : mirror of vms_kv.go. See the docstring there for the
// V0.1.4 design rationale.

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// encodeHostRecord serialises a single Host as a one-block HCL doc.
func encodeHostRecord(h Host) []byte {
	f := hclwrite.NewEmptyFile()
	body := f.Body()
	block := body.AppendNewBlock("host", []string{h.UUID})
	bb := block.Body()
	bb.SetAttributeValue("hostname", cty.StringVal(h.Hostname))
	if h.AZ != "" {
		bb.SetAttributeValue("az", cty.StringVal(h.AZ))
	}
	if h.Rack != "" {
		bb.SetAttributeValue("rack", cty.StringVal(h.Rack))
	}
	if h.Endpoint != "" {
		bb.SetAttributeValue("endpoint", cty.StringVal(h.Endpoint))
	}
	if h.Hypervisor != "" {
		bb.SetAttributeValue("hypervisor", cty.StringVal(h.Hypervisor))
	}
	if h.Architecture != "" {
		bb.SetAttributeValue("architecture", cty.StringVal(h.Architecture))
	}
	for _, d := range h.Drivers {
		db := bb.AppendNewBlock("driver", []string{d.Kind}).Body()
		if len(d.Arches) > 0 {
			vals := make([]cty.Value, len(d.Arches))
			for i, a := range d.Arches {
				vals[i] = cty.StringVal(a)
			}
			db.SetAttributeValue("arches", cty.ListVal(vals))
		}
	}
	if len(h.NetworkTypes) > 0 {
		vals := make([]cty.Value, len(h.NetworkTypes))
		for i, s := range h.NetworkTypes {
			vals[i] = cty.StringVal(s)
		}
		bb.SetAttributeValue("network_types", cty.ListVal(vals))
	}
	if len(h.VolumeBackends) > 0 {
		vals := make([]cty.Value, len(h.VolumeBackends))
		for i, s := range h.VolumeBackends {
			vals[i] = cty.StringVal(s)
		}
		bb.SetAttributeValue("volume_backends", cty.ListVal(vals))
	}
	for _, g := range h.GPUs {
		gb := bb.AppendNewBlock("gpu", []string{g.Model}).Body()
		gb.SetAttributeValue("vendor", cty.StringVal(g.Vendor))
		if g.MemoryGiB > 0 {
			gb.SetAttributeValue("memory_gib", cty.NumberIntVal(int64(g.MemoryGiB)))
		}
		if g.MIGCapable {
			gb.SetAttributeValue("mig_capable", cty.BoolVal(true))
		}
	}
	for _, p := range h.PCIDevices {
		pb := bb.AppendNewBlock("pci", []string{p.BDF}).Body()
		if p.VendorID != "" {
			pb.SetAttributeValue("vendor_id", cty.StringVal(p.VendorID))
		}
		if p.DeviceID != "" {
			pb.SetAttributeValue("device_id", cty.StringVal(p.DeviceID))
		}
		if p.Driver != "" {
			pb.SetAttributeValue("driver", cty.StringVal(p.Driver))
		}
	}
	if len(h.Properties) > 0 {
		ctyMap := make(map[string]cty.Value, len(h.Properties))
		for k, v := range h.Properties {
			ctyMap[k] = cty.StringVal(v)
		}
		bb.SetAttributeValue("properties", cty.MapVal(ctyMap))
	}
	if h.State != "" {
		bb.SetAttributeValue("state", cty.StringVal(string(h.State)))
	}
	if h.Cordoned {
		bb.SetAttributeValue("cordoned", cty.BoolVal(true))
	}
	if !h.LastSeenAt.IsZero() {
		bb.SetAttributeValue("last_seen_at", cty.StringVal(h.LastSeenAt.Format(time.RFC3339Nano)))
	}
	bb.SetAttributeValue("created_at", cty.StringVal(h.CreatedAt.Format(time.RFC3339Nano)))
	if h.AKName != "" {
		bb.SetAttributeValue("ak_name", cty.StringVal(h.AKName))
	}
	if h.WGPublicKey != "" {
		bb.SetAttributeValue("wg_public_key", cty.StringVal(h.WGPublicKey))
	}
	if h.WGOverlayIndex != 0 {
		bb.SetAttributeValue("wg_overlay_index", cty.NumberIntVal(int64(h.WGOverlayIndex)))
	}
	return f.Bytes()
}

// decodeHostRecord parses a one-block HCL document into a Host.
func decodeHostRecord(blob []byte) (Host, error) {
	var doc hostsDoc
	if err := hclsimple.Decode("host-record.hcl", blob, nil, &doc); err != nil {
		return Host{}, fmt.Errorf("decode host record: %w", err)
	}
	if len(doc.Hosts) != 1 {
		return Host{}, fmt.Errorf("host record: want exactly 1 block, got %d", len(doc.Hosts))
	}
	return hostFromBlock(doc.Hosts[0]), nil
}

func hostFromBlock(b hostBlock) Host {
	created, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
	lastSeen, _ := time.Parse(time.RFC3339Nano, b.LastSeenAt)
	state := HostState(b.State)
	if state == "" {
		state = HostStateActive
	}
	var properties map[string]string
	if len(b.Properties) > 0 {
		properties = make(map[string]string, len(b.Properties))
		for k, v := range b.Properties {
			properties[k] = v
		}
	}
	var drivers []HostDriver
	for _, d := range b.Drivers {
		drivers = append(drivers, HostDriver{
			Kind: d.Kind, Arches: append([]string(nil), d.Arches...),
		})
	}
	var gpus []GPU
	for _, g := range b.GPUs {
		gpus = append(gpus, GPU{
			Vendor: g.Vendor, Model: g.Model,
			MemoryGiB: g.MemoryGiB, MIGCapable: g.MIGCapable,
		})
	}
	var pciDevs []PCIDevice
	for _, p := range b.PCIDevices {
		pciDevs = append(pciDevs, PCIDevice{
			BDF: p.BDF, VendorID: p.VendorID,
			DeviceID: p.DeviceID, Driver: p.Driver,
		})
	}
	return Host{
		UUID:           b.UUID,
		Hostname:       b.Hostname,
		AZ:             b.AZ,
		Rack:           b.Rack,
		Endpoint:       b.Endpoint,
		Hypervisor:     b.Hypervisor,
		Architecture:   b.Architecture,
		Drivers:        drivers,
		NetworkTypes:   append([]string(nil), b.NetworkTypes...),
		VolumeBackends: append([]string(nil), b.VolumeBackends...),
		GPUs:           gpus,
		PCIDevices:     pciDevs,
		Properties:     properties,
		State:          state,
		Cordoned:       b.Cordoned,
		LastSeenAt:     lastSeen,
		CreatedAt:      created,
		AKName:         b.AKName,
		WGPublicKey:    b.WGPublicKey,
		WGOverlayIndex: b.WGOverlayIndex,
	}
}

// loadHostRegistryKV is the per-record load path with legacy-blob
// migration on first run.
func loadHostRegistryKV(ctx context.Context, kv KVStorage, legacy Storage) (*hostRegistry, error) {
	reg := &hostRegistry{
		storage: legacy,
		kv:      kv,
		byUUID:  make(map[string]Host),
		nameIdx: make(map[string]string),
	}
	records, err := kv.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 && legacy != nil {
		blob, err := legacy.Load(ctx)
		if err == nil && len(blob) > 0 {
			if err := migrateHostBlobToKV(ctx, blob, kv); err != nil {
				return nil, fmt.Errorf("migrate host registry blob → kv: %w", err)
			}
			records, err = kv.List(ctx)
			if err != nil {
				return nil, err
			}
			_ = legacy.Save(ctx, nil)
		}
	}
	for _, blob := range records {
		h, err := decodeHostRecord(blob)
		if err != nil {
			continue
		}
		reg.byUUID[h.UUID] = h
		if h.Hostname != "" {
			reg.nameIdx[h.Hostname] = h.UUID
		}
	}
	return reg, nil
}

func migrateHostBlobToKV(ctx context.Context, blob []byte, kv KVStorage) error {
	var doc hostsDoc
	if err := hclsimple.Decode("host-registry.hcl", blob, nil, &doc); err != nil {
		return fmt.Errorf("parse legacy host blob: %w", err)
	}
	for _, b := range doc.Hosts {
		h := hostFromBlock(b)
		if err := kv.PutOne(ctx, h.UUID, encodeHostRecord(h)); err != nil {
			return fmt.Errorf("put migrated host %s: %w", h.UUID, err)
		}
	}
	return nil
}

func (r *hostRegistry) persistOne(h Host) error {
	if r.kv != nil {
		return r.kv.PutOne(context.Background(), h.UUID, encodeHostRecord(h))
	}
	return r.saveLocked()
}

func (r *hostRegistry) persistDelete(uuid string) error {
	if r.kv != nil {
		return r.kv.DeleteOne(context.Background(), uuid)
	}
	return r.saveLocked()
}

func (r *hostRegistry) applyKVEvent(ev KVEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Kind {
	case KVPut:
		h, err := decodeHostRecord(ev.Value)
		if err != nil {
			return
		}
		if prev, ok := r.byUUID[h.UUID]; ok && prev.Hostname != h.Hostname {
			delete(r.nameIdx, prev.Hostname)
		}
		r.byUUID[h.UUID] = h
		if h.Hostname != "" {
			r.nameIdx[h.Hostname] = h.UUID
		}
	case KVDelete:
		uuid := hostKeyToUUID(ev.Key, r.kv.Prefix())
		if prev, ok := r.byUUID[uuid]; ok {
			delete(r.byUUID, uuid)
			if prev.Hostname != "" {
				delete(r.nameIdx, prev.Hostname)
			}
			return
		}
		if ev.PrevValue != nil {
			if prev, err := decodeHostRecord(ev.PrevValue); err == nil {
				delete(r.byUUID, prev.UUID)
				if prev.Hostname != "" {
					delete(r.nameIdx, prev.Hostname)
				}
			}
		}
	}
}

func hostKeyToUUID(key, prefix string) string {
	if len(key) > len(prefix) && bytes.HasPrefix([]byte(key), []byte(prefix)) {
		return key[len(prefix):]
	}
	return key
}
