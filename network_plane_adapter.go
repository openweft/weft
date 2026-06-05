package weft

// network_plane_adapter.go — Adapter glue for the Subnet,
// LoadBalancer, DNSZone + DNSRecord registries (proto v0.8.0).
// Mirrors inventory_adapter.go : registries load via the
// Storage-factory, mutations publish PlatformEvents, cascade
// safety lives in the Adapter because only it sees every
// registry (DNSZone delete needs the DNSRecord count, LB delete
// needs the FloatingIP map count).

import (
	"context"
	"fmt"
	"os"
)

// initNetworkPlane loads the Subnet + LoadBalancer + DNSZone +
// DNSRecord registries from their Storage backends. Mirrors
// initInventory's resilience contract : a registry whose Load
// errors out gets replaced by an empty in-memory copy.
func (a *Adapter) initNetworkPlane() {
	subnetStorage := a.storageFactory(subnetRegistryFileName)
	subnetReg, err := loadSubnetRegistry(context.Background(), subnetStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load subnet registry: %v\n", err)
		subnetReg = &subnetRegistry{
			storage:    subnetStorage,
			byUUID:     make(map[string]Subnet),
			networkIdx: make(map[string]map[string]struct{}),
		}
	}
	a.subnetReg = subnetReg

	lbStorage := a.storageFactory(loadBalancerRegistryFileName)
	lbReg, err := loadLoadBalancerRegistry(context.Background(), lbStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load loadbalancer registry: %v\n", err)
		lbReg = &loadBalancerRegistry{
			storage: lbStorage,
			byUUID:  make(map[string]LoadBalancer),
			nameIdx: make(map[string]string),
		}
	}
	a.lbReg = lbReg

	zoneStorage := a.storageFactory(dnsZoneRegistryFileName)
	zoneReg, err := loadDNSZoneRegistry(context.Background(), zoneStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load dns zone registry: %v\n", err)
		zoneReg = &dnsZoneRegistry{
			storage: zoneStorage,
			byUUID:  make(map[string]DNSZone),
			nameIdx: make(map[string]string),
		}
	}
	a.dnsZoneReg = zoneReg

	recordStorage := a.storageFactory(dnsRecordRegistryFileName)
	recordReg, err := loadDNSRecordRegistry(context.Background(), recordStorage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "weft: load dns record registry: %v\n", err)
		recordReg = &dnsRecordRegistry{
			storage: recordStorage,
			byUUID:  make(map[string]DNSRecord),
			zoneIdx: make(map[string]map[string]struct{}),
		}
	}
	a.dnsRecordReg = recordReg
}

// --- Subnets ----------------------------------------------------

func (a *Adapter) Subnets(networkUUID string) []Subnet {
	if a.subnetReg == nil {
		return nil
	}
	return a.subnetReg.list(networkUUID)
}

func (a *Adapter) SubnetByUUID(uuid string) (Subnet, bool) {
	if a.subnetReg == nil {
		return Subnet{}, false
	}
	return a.subnetReg.lookupByUUID(uuid)
}

// CreateSubnet derives projectUUID from the parent network so the
// caller doesn't have to repeat it. Refuses when the parent
// network is missing.
func (a *Adapter) CreateSubnet(networkUUID, name, description, cidr, gateway string, dnsServers []string) (Subnet, bool, error) {
	if a.subnetReg == nil {
		return Subnet{}, false, fmt.Errorf("subnet registry not initialised")
	}
	net, ok := a.NetworkByUUID(networkUUID)
	if !ok {
		return Subnet{}, false, fmt.Errorf("parent network %s not found", networkUUID)
	}
	sn, created, err := a.subnetReg.create(networkUUID, net.ProjectUUID, name, description, cidr, gateway, dnsServers)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:        "subnet.created",
			Subject:     sn.UUID,
			ProjectUUID: sn.ProjectUUID,
			Meta:        map[string]string{"network": sn.NetworkUUID, "cidr": sn.CIDR},
		})
	}
	return sn, created, err
}

func (a *Adapter) UpdateSubnet(uuid, name, description, gateway string, clearDNS bool, dnsServers []string) (Subnet, error) {
	if a.subnetReg == nil {
		return Subnet{}, fmt.Errorf("subnet registry not initialised")
	}
	sn, err := a.subnetReg.update(uuid, name, description, gateway, clearDNS, dnsServers)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:        "subnet.updated",
			Subject:     uuid,
			ProjectUUID: sn.ProjectUUID,
		})
	}
	return sn, err
}

func (a *Adapter) DeleteSubnet(uuid string) error {
	if a.subnetReg == nil {
		return fmt.Errorf("subnet registry not initialised")
	}
	prev, _ := a.subnetReg.lookupByUUID(uuid)
	if err := a.subnetReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{
		Kind:        "subnet.deleted",
		Subject:     uuid,
		ProjectUUID: prev.ProjectUUID,
	})
	return nil
}

// --- LoadBalancers ---------------------------------------------

func (a *Adapter) LoadBalancers(projectUUID string) []LoadBalancer {
	if a.lbReg == nil {
		return nil
	}
	return a.lbReg.list(projectUUID)
}

func (a *Adapter) LoadBalancerByUUID(uuid string) (LoadBalancer, bool) {
	if a.lbReg == nil {
		return LoadBalancer{}, false
	}
	return a.lbReg.lookupByUUID(uuid)
}

func (a *Adapter) CreateLoadBalancer(projectUUID, name, listenAddr, protocol string, backends []LBBackend) (LoadBalancer, bool, error) {
	if a.lbReg == nil {
		return LoadBalancer{}, false, fmt.Errorf("loadbalancer registry not initialised")
	}
	lb, created, err := a.lbReg.create(projectUUID, name, listenAddr, protocol, backends)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:        "loadbalancer.created",
			Subject:     lb.UUID,
			ProjectUUID: lb.ProjectUUID,
			Meta:        map[string]string{"name": lb.Name, "protocol": lb.Protocol},
		})
	}
	return lb, created, err
}

func (a *Adapter) UpdateLoadBalancer(uuid, name, listenAddr, protocol string) (LoadBalancer, error) {
	if a.lbReg == nil {
		return LoadBalancer{}, fmt.Errorf("loadbalancer registry not initialised")
	}
	lb, err := a.lbReg.update(uuid, name, listenAddr, protocol)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:        "loadbalancer.updated",
			Subject:     uuid,
			ProjectUUID: lb.ProjectUUID,
		})
	}
	return lb, err
}

func (a *Adapter) SetLoadBalancerBackends(uuid string, backends []LBBackend) (LoadBalancer, error) {
	if a.lbReg == nil {
		return LoadBalancer{}, fmt.Errorf("loadbalancer registry not initialised")
	}
	lb, err := a.lbReg.setBackends(uuid, backends)
	if err == nil {
		a.bus.Publish(PlatformEvent{
			Kind:        "loadbalancer.backends",
			Subject:     uuid,
			ProjectUUID: lb.ProjectUUID,
		})
	}
	return lb, err
}

// LoadBalancerFIPCount counts how many FloatingIPs currently map
// to the named LB. Used by DeleteLoadBalancer for cascade refusal.
func (a *Adapter) LoadBalancerFIPCount(uuid string) int32 {
	if a.lbReg == nil || a.fipReg == nil {
		return 0
	}
	lb, ok := a.lbReg.lookupByUUID(uuid)
	if !ok {
		return 0
	}
	return int32(len(a.fipReg.listForTarget(FIPTargetLB, lb.Name)))
}

func (a *Adapter) DeleteLoadBalancer(uuid string) (blockedByFIPs int32, err error) {
	if a.lbReg == nil {
		return 0, fmt.Errorf("loadbalancer registry not initialised")
	}
	blockedByFIPs = a.LoadBalancerFIPCount(uuid)
	got, err := a.lbReg.delete(uuid, blockedByFIPs)
	if err != nil {
		if got > 0 {
			blockedByFIPs = got
		}
		return blockedByFIPs, err
	}
	a.bus.Publish(PlatformEvent{Kind: "loadbalancer.deleted", Subject: uuid})
	return 0, nil
}

// --- DNS Zones --------------------------------------------------

func (a *Adapter) DNSZones(projectUUID string) []DNSZone {
	if a.dnsZoneReg == nil {
		return nil
	}
	return a.dnsZoneReg.list(projectUUID)
}

func (a *Adapter) DNSZoneByUUID(uuid string) (DNSZone, bool) {
	if a.dnsZoneReg == nil {
		return DNSZone{}, false
	}
	return a.dnsZoneReg.lookupByUUID(uuid)
}

func (a *Adapter) DNSZoneByName(name string) (DNSZone, bool) {
	if a.dnsZoneReg == nil {
		return DNSZone{}, false
	}
	return a.dnsZoneReg.lookupByName(name)
}

// DNSZoneRecordCount surfaces the derived count the proto's
// DNSZoneInfo carries. Cheap (the in-memory zoneIdx is a map lookup).
func (a *Adapter) DNSZoneRecordCount(zoneUUID string) int32 {
	if a.dnsRecordReg == nil {
		return 0
	}
	return a.dnsRecordReg.countForZone(zoneUUID)
}

func (a *Adapter) CreateDNSZone(projectUUID, name, soaEmail string, ttl int32) (DNSZone, bool, error) {
	if a.dnsZoneReg == nil {
		return DNSZone{}, false, fmt.Errorf("dns zone registry not initialised")
	}
	z, created, err := a.dnsZoneReg.create(projectUUID, name, soaEmail, ttl)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:        "dnszone.created",
			Subject:     z.UUID,
			ProjectUUID: z.ProjectUUID,
			Meta:        map[string]string{"name": z.Name},
		})
	}
	return z, created, err
}

func (a *Adapter) UpdateDNSZone(uuid, soaEmail string, ttl int32) (DNSZone, error) {
	if a.dnsZoneReg == nil {
		return DNSZone{}, fmt.Errorf("dns zone registry not initialised")
	}
	z, err := a.dnsZoneReg.update(uuid, soaEmail, ttl)
	if err == nil {
		a.bus.Publish(PlatformEvent{Kind: "dnszone.updated", Subject: uuid, ProjectUUID: z.ProjectUUID})
	}
	return z, err
}

func (a *Adapter) DeleteDNSZone(uuid string) (blockedByRecords int32, err error) {
	if a.dnsZoneReg == nil {
		return 0, fmt.Errorf("dns zone registry not initialised")
	}
	blockedByRecords = a.DNSZoneRecordCount(uuid)
	got, err := a.dnsZoneReg.delete(uuid, blockedByRecords)
	if err != nil {
		if got > 0 {
			blockedByRecords = got
		}
		return blockedByRecords, err
	}
	a.bus.Publish(PlatformEvent{Kind: "dnszone.deleted", Subject: uuid})
	return 0, nil
}

// --- DNS Records ------------------------------------------------

func (a *Adapter) DNSRecords(zoneUUID string) []DNSRecord {
	if a.dnsRecordReg == nil {
		return nil
	}
	return a.dnsRecordReg.list(zoneUUID)
}

func (a *Adapter) DNSRecordByUUID(uuid string) (DNSRecord, bool) {
	if a.dnsRecordReg == nil {
		return DNSRecord{}, false
	}
	return a.dnsRecordReg.lookupByUUID(uuid)
}

func (a *Adapter) CreateDNSRecord(zoneUUID, name, recordType, value string, ttl, priority int32) (DNSRecord, bool, error) {
	if a.dnsRecordReg == nil {
		return DNSRecord{}, false, fmt.Errorf("dns record registry not initialised")
	}
	if _, ok := a.DNSZoneByUUID(zoneUUID); !ok {
		return DNSRecord{}, false, fmt.Errorf("parent dns zone %s not found", zoneUUID)
	}
	rec, created, err := a.dnsRecordReg.create(zoneUUID, name, recordType, value, ttl, priority)
	if err == nil && created {
		a.bus.Publish(PlatformEvent{
			Kind:    "dnsrecord.created",
			Subject: rec.UUID,
			Meta:    map[string]string{"zone": rec.ZoneUUID, "name": rec.Name, "type": rec.Type},
		})
	}
	return rec, created, err
}

func (a *Adapter) UpdateDNSRecord(uuid, value string, ttl, priority int32) (DNSRecord, error) {
	if a.dnsRecordReg == nil {
		return DNSRecord{}, fmt.Errorf("dns record registry not initialised")
	}
	rec, err := a.dnsRecordReg.update(uuid, value, ttl, priority)
	if err == nil {
		a.bus.Publish(PlatformEvent{Kind: "dnsrecord.updated", Subject: uuid})
	}
	return rec, err
}

func (a *Adapter) DeleteDNSRecord(uuid string) error {
	if a.dnsRecordReg == nil {
		return fmt.Errorf("dns record registry not initialised")
	}
	if err := a.dnsRecordReg.delete(uuid); err != nil {
		return err
	}
	a.bus.Publish(PlatformEvent{Kind: "dnsrecord.deleted", Subject: uuid})
	return nil
}
