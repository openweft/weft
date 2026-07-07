package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	weftv1 "github.com/openweft/weft-proto"
)

// allCases is the test registry. Order : smoke first (so a partial
// run on a broken cluster still surfaces the cheap-and-load-bearing
// checks), then the longer plugin / lifecycle scenarios.
//
// To add a test : drop a new Case literal in here ; the runner picks
// it up by reflection on the slice. No init() magic, no test files
// — keeps `go run` workflow trivial.
var allCases = []Case{
	{Name: "hosts/list", Suite: "smoke", Order: 10, Fn: testHostsList},
	{Name: "hosts/all-active", Suite: "smoke", Order: 11, Fn: testHostsActive},
	{Name: "hosts/all-connected", Suite: "smoke", Order: 12, Fn: testHostsConnected},
	{Name: "vms/list", Suite: "smoke", Order: 20, Fn: testVMsList},
	{Name: "vms/cross-host-visible", Suite: "smoke", Order: 21, Fn: testVMsCrossHostVisible},
	{Name: "vms/image-label-not-placeholder", Suite: "smoke", Order: 22, Fn: testVMImageLabel},
	{Name: "vms/flavor-matches-catalogue", Suite: "smoke", Order: 23, Fn: testVMFlavorMatchesCatalogue},
	{Name: "vms/restart-cross-host", Suite: "smoke", Order: 30, Fn: testRestartCrossHost},
	{Name: "plugin/catalogue-non-empty", Suite: "smoke", Order: 40, Fn: testPluginCatalogue},
	{Name: "plugin/installed-snapshot", Suite: "smoke", Order: 41, Fn: testInstalledPlugins},

	// full suite : touches cluster state (install / uninstall).
	{Name: "plugin/install-redis-ha-3dc-spread", Suite: "full", Order: 100, Fn: testRedisHA3DCSpread},
	{Name: "plugin/uninstall-redis-ha-cleans-all-dcs", Suite: "full", Order: 101, Fn: testRedisHAUninstallClean},
}

// --- smoke ---

// testHostsList asserts ListHosts returns at least one host. Pure
// connectivity check : if this fails the agent isn't reachable or
// the registry didn't initialise.
func testHostsList(c *Ctx) {
	ctx, cancel := bg(10 * time.Second)
	defer cancel()
	resp, err := c.Client.ListHosts(ctx, &weftv1.ListHostsRequest{})
	c.require(err == nil, "ListHosts: %v", err)
	c.require(len(resp.Hosts) > 0, "ListHosts returned 0 hosts")
	c.logf("found %d hosts", len(resp.Hosts))
}

// testHostsActive asserts every host's state is "active". Surfaces a
// host that's draining / down / inactive before the rest of the suite
// makes assumptions about placement.
func testHostsActive(c *Ctx) {
	ctx, cancel := bg(10 * time.Second)
	defer cancel()
	resp, err := c.Client.ListHosts(ctx, &weftv1.ListHostsRequest{})
	c.require(err == nil, "ListHosts: %v", err)
	for _, h := range resp.Hosts {
		c.expect(h.State == "active", "host %s state=%q (want active)", h.Hostname, h.State)
	}
}

// testHostsConnected asserts the registry-reported connected_host_uuids
// matches every host's UUID. Catches an agent that's registered but
// whose etcdcoord lease expired (so the rest of the cluster sees it
// as down even though `state=active`).
func testHostsConnected(c *Ctx) {
	ctx, cancel := bg(10 * time.Second)
	defer cancel()
	resp, err := c.Client.ListHosts(ctx, &weftv1.ListHostsRequest{})
	c.require(err == nil, "ListHosts: %v", err)
	connected := map[string]bool{}
	for _, u := range resp.ConnectedHostUuids {
		connected[u] = true
	}
	for _, h := range resp.Hosts {
		c.expect(connected[h.Uuid], "host %s (%s) not in connected_host_uuids", h.Hostname, h.Uuid[:8])
	}
}

// testVMsList exercises ListVMs without project filtering ; just
// the wire-level check that the call doesn't error.
func testVMsList(c *Ctx) {
	ctx, cancel := bg(10 * time.Second)
	defer cancel()
	resp, err := c.Client.ListVMs(ctx, &weftv1.ListVMsRequest{})
	c.require(err == nil, "ListVMs: %v", err)
	c.logf("found %d VMs", len(resp.Vms))
}

// testVMsCrossHostVisible asserts the operator sees VMs from EVERY
// host (not just the one the socket dials). On a 3-DC cluster the
// inventory should surface at least one VM per host_uuid otherwise
// the cross-host registry merge is broken.
func testVMsCrossHostVisible(c *Ctx) {
	ctx, cancel := bg(10 * time.Second)
	defer cancel()
	hostsResp, err := c.Client.ListHosts(ctx, &weftv1.ListHostsRequest{})
	c.require(err == nil, "ListHosts: %v", err)
	if len(hostsResp.Hosts) < 2 {
		c.skip("single-host cluster — cross-host visibility N/A")
	}
	vmsResp, err := c.Client.ListVMs(ctx, &weftv1.ListVMsRequest{})
	c.require(err == nil, "ListVMs: %v", err)
	byHost := map[string]int{}
	for _, v := range vmsResp.Vms {
		if v.HostUuid != "" {
			byHost[v.HostUuid]++
		}
	}
	if len(byHost) < 2 {
		c.expect(false, "VMs landed on only %d distinct host(s) ; expected ≥2 on a multi-host cluster (sample: %v)",
			len(byHost), byHost)
	}
	c.logf("VMs spread across %d host(s)", len(byHost))
}

// testVMImageLabel asserts no VM record carries the synthetic
// "microvm/direct_linux" placeholder — every microVM should have
// the OCI ref it was hatched from. Pre-V0.4.71 records stuck on the
// placeholder ; the registry lift in adapter.go should converge
// them on the next RegisterMicroVM. Cluster-wide check : flags any
// stale record without a fresh redeploy.
func testVMImageLabel(c *Ctx) {
	ctx, cancel := bg(10 * time.Second)
	defer cancel()
	resp, err := c.Client.ListVMs(ctx, &weftv1.ListVMsRequest{})
	c.require(err == nil, "ListVMs: %v", err)
	var stale []string
	for _, v := range resp.Vms {
		if v.Image == "microvm/direct_linux" {
			stale = append(stale, v.Name)
		}
	}
	if len(stale) > 0 {
		c.expect(false, "%d VM(s) still carry the synthetic image placeholder : %v", len(stale), stale)
	}
}

// testVMFlavorMatchesCatalogue asserts every VM in the inventory
// has a (cpu, mem_mb) tuple that matches some entry in the Flavor
// catalogue. The "custom" placeholder the TUI renders when no
// flavor matches is a smell — it means the workload booted on a
// shape the catalogue doesn't acknowledge. Operator directive
// 2026-06-30 : "on ne peut pas demarrer sur autre choses que les
// flavors listés dans le catalogue".
//
// Skips VMs with cpu=0 + mem_mb=0 (legacy records without the
// shape stamped — covered by the flavor backfill on RegisterMicroVM,
// they converge on the next restart). Reports the first non-match
// so the failure points the operator at the offending plugin.
func testVMFlavorMatchesCatalogue(c *Ctx) {
	ctx, cancel := bg(10 * time.Second)
	defer cancel()
	flavorsResp, err := c.Client.ListFlavors(ctx, &weftv1.ListFlavorsRequest{})
	c.require(err == nil, "ListFlavors: %v", err)
	type shape struct{ cpu uint32; mem uint64 }
	known := map[shape]string{}
	for _, f := range flavorsResp.Flavors {
		known[shape{cpu: uint32(f.Vcpu), mem: uint64(ramToMB(f.Ram))}] = f.Name
	}
	vmsResp, err := c.Client.ListVMs(ctx, &weftv1.ListVMsRequest{})
	c.require(err == nil, "ListVMs: %v", err)
	var custom []string
	for _, v := range vmsResp.Vms {
		if v.Cpu == 0 && v.MemMb == 0 {
			continue
		}
		if _, ok := known[shape{cpu: v.Cpu, mem: v.MemMb}]; !ok {
			custom = append(custom, fmt.Sprintf("%s(%dvCPU/%dMB)", v.Name, v.Cpu, v.MemMb))
		}
	}
	if len(custom) > 0 {
		c.expect(false, "%d VM(s) running on shapes outside the Flavor catalogue : %v", len(custom), custom)
	}
}

// ramToMB parses a Flavor.RAM string ("4Gi", "256Mi") into MiB.
// Mirrors weft.RAMToMiB so the e2e harness doesn't need the server-
// side dep. Same parser, same suffixes.
func ramToMB(s string) int {
	if s == "" {
		return 0
	}
	low := strings.ToLower(s)
	switch {
	case strings.HasSuffix(low, "gib"):
		n := strings.TrimSuffix(low, "gib")
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return v * 1024
	case strings.HasSuffix(low, "gi"):
		n := strings.TrimSuffix(low, "gi")
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return v * 1024
	case strings.HasSuffix(low, "g"):
		n := strings.TrimSuffix(low, "g")
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return v * 1024
	case strings.HasSuffix(low, "mib"):
		n := strings.TrimSuffix(low, "mib")
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return v
	case strings.HasSuffix(low, "mi"):
		n := strings.TrimSuffix(low, "mi")
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return v
	case strings.HasSuffix(low, "m"):
		n := strings.TrimSuffix(low, "m")
		v, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return v
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

// testRestartCrossHost picks the first VM whose host_uuid != the
// caller's local host, calls RestartVM against it, and asserts the
// RPC succeeds. Regression for the 2026-06-29 bug where RestartVM
// fell into the local vmDir(name) resolver → "kernel not found at
// state/vz/<usr-admin>/<vm>" on cross-host targets.
func testRestartCrossHost(c *Ctx) {
	ctx, cancel := bg(30 * time.Second)
	defer cancel()
	vmsResp, err := c.Client.ListVMs(ctx, &weftv1.ListVMsRequest{})
	c.require(err == nil, "ListVMs: %v", err)
	// Pick a non-zombie VM placed on a host distinct from the
	// caller's. Skip when no such VM exists (single-host cluster).
	var target *weftv1.VMInfo
	hostsByCount := map[string]int{}
	for _, v := range vmsResp.Vms {
		hostsByCount[v.HostUuid]++
	}
	// "Local" host = the one carrying the most VMs (heuristic ;
	// the dialed socket usually hosts the install pipeline + its
	// own infra fleet). We restart against any host that's NOT
	// the local one.
	var localHost string
	maxCount := 0
	for h, n := range hostsByCount {
		if h != "" && n > maxCount {
			maxCount = n
			localHost = h
		}
	}
	for _, v := range vmsResp.Vms {
		if v.HostUuid != "" && v.HostUuid != localHost && v.State == weftv1.VMState_VM_STATE_RUNNING {
			target = v
			break
		}
	}
	if target == nil {
		c.skip("no running VM placed on a non-local host — cross-host restart N/A")
	}
	c.logf("target vm=%s project=%s host=%s", target.Name, target.Project, target.HostUuid[:8])
	_, err = c.Client.RestartVM(ctx, &weftv1.RestartVMRequest{
		Name:     target.Name,
		Project:  target.Project,
		HostUuid: target.HostUuid,
	})
	c.require(err == nil, "RestartVM cross-host failed : %v", err)
}

// testPluginCatalogue asserts ListPluginCatalogue returns at least
// the static fallback set. An empty catalogue means the etcd store
// didn't seed AND staticPluginCatalogue() didn't kick in — the
// webui's Plugins tab would render an empty table.
func testPluginCatalogue(c *Ctx) {
	ctx, cancel := bg(10 * time.Second)
	defer cancel()
	resp, err := c.Client.ListPluginCatalogue(ctx, &weftv1.ListPluginCatalogueRequest{})
	c.require(err == nil, "ListPluginCatalogue: %v", err)
	c.require(len(resp.Entries) > 0, "ListPluginCatalogue returned 0 entries (static fallback broken?)")
	c.logf("catalogue has %d entries", len(resp.Entries))
}

// testInstalledPlugins asserts the snapshot ListInstalledPlugins
// returns without error. Doesn't assert specific instances — the
// plugin/install-* tests in the full suite cover state changes.
func testInstalledPlugins(c *Ctx) {
	ctx, cancel := bg(10 * time.Second)
	defer cancel()
	resp, err := c.Client.ListInstalledPlugins(ctx, &weftv1.ListInstalledPluginsRequest{})
	c.require(err == nil, "ListInstalledPlugins: %v", err)
	c.logf("%d installed plugin instance(s)", len(resp.Instances))
}

// --- full ---

// testRedisHA3DCSpread installs the redis-ha plugin + asserts the 3
// replicas land on 3 distinct hosts. Regression for the 2026-06-29
// "anti-affinity collapsed" bug (all replicas on dc1).
//
// Idempotent : already-installed instances are reused. The test
// fails when fewer than 3 distinct host_uuids cover the redis-*
// VMs, regardless of why (plugin manager not wired, dispatch
// broken, picker missing the AZ axis).
func testRedisHA3DCSpread(c *Ctx) {
	ctx, cancel := bg(2 * time.Minute)
	defer cancel()
	hostsResp, err := c.Client.ListHosts(ctx, &weftv1.ListHostsRequest{})
	c.require(err == nil, "ListHosts: %v", err)
	azs := map[string]bool{}
	for _, h := range hostsResp.Hosts {
		if h.State == "active" {
			azs[h.Az] = true
		}
	}
	if len(azs) < 3 {
		c.skip("less than 3 active AZs — anti-affinity test N/A")
	}
	// Pre-cleanup : the install path's deterministic UUID can
	// collide with a leftover instance from a different StateStore
	// (CLI's ~/.local/state vs gRPC's ~/.weft/plugins, divergence
	// flagged 2026-06-30). Walk the inventory + drop any redis-ha-*
	// VMs whose name matches an instance prefix we'd produce, AND
	// uninstall any redis-ha plugin instances from etcd. Idempotent
	// — silent when nothing's there. Without this, a network-name
	// collision aborts the install before placement is checked.
	if installed, ierr := c.Client.ListInstalledPlugins(ctx, &weftv1.ListInstalledPluginsRequest{}); ierr == nil {
		for _, p := range installed.Instances {
			if p.Name == "redis-ha" {
				_, _ = c.Client.UninstallPlugin(ctx, &weftv1.UninstallPluginRequest{
					Name:         "redis-ha",
					InstanceUuid: p.InstanceUuid,
				})
				c.logf("pre-cleanup uninstalled %s", p.InstanceUuid[:8])
			}
		}
		// Give the dispatch chain a moment to fan DeleteVM /
		// DeleteNetwork out to peer agents.
		time.Sleep(5 * time.Second)
	}
	// Install. Idempotent — already-present instance short-circuits.
	_, err = c.Client.InstallPlugin(ctx, &weftv1.InstallPluginRequest{
		Name:    "redis-ha",
		Project: "infra",
		Inputs:  map[string]string{"password": "e2etest"},
	})
	c.require(err == nil, "InstallPlugin redis-ha: %v", err)
	// Wait up to 90s for 3 replicas to appear ; converge over etcd
	// watches has a few seconds of latency.
	c.eventually(90*time.Second, func() bool {
		vms, err := c.Client.ListVMs(ctx, &weftv1.ListVMsRequest{})
		if err != nil {
			return false
		}
		return countRedis(vms.Vms) == 3
	})
	vms, err := c.Client.ListVMs(ctx, &weftv1.ListVMsRequest{})
	c.require(err == nil, "ListVMs: %v", err)
	hostsForRedis := map[string]bool{}
	for _, v := range vms.Vms {
		if strings.HasPrefix(v.Name, "redis-ha-") && strings.Contains(v.Name, "-redis-") {
			hostsForRedis[v.HostUuid] = true
		}
	}
	c.logf("redis-ha replicas on %d distinct hosts", len(hostsForRedis))
	c.expect(len(hostsForRedis) >= 3, "redis-ha replicas spread over only %d host(s) ; want ≥3", len(hostsForRedis))
}

// testRedisHAUninstallClean tears down the redis-ha instance THIS
// harness created (via testRedisHA3DCSpread above) + asserts every
// replica VM keyed by THAT instance UUID is gone from the inventory.
// Regression for the 2026-06-29 "uninstall left dc2/dc3 VMs alive"
// leftover. Filters by instance UUID so a stale instance from an
// earlier manual install doesn't confound the assertion.
//
// Best-effort : skips if NO redis-ha instance exists (test ordering
// matters when --run filters out the install test).
func testRedisHAUninstallClean(c *Ctx) {
	ctx, cancel := bg(2 * time.Minute)
	defer cancel()
	installed, err := c.Client.ListInstalledPlugins(ctx, &weftv1.ListInstalledPluginsRequest{})
	c.require(err == nil, "ListInstalledPlugins: %v", err)
	var inst *weftv1.PluginInstance
	for _, p := range installed.Instances {
		if p.Name == "redis-ha" {
			inst = p
			break
		}
	}
	if inst == nil {
		c.skip("redis-ha not installed — uninstall N/A")
	}
	c.logf("uninstalling instance %s", inst.InstanceUuid)
	// VM names embed shortUUID(instance_uuid) — capture the prefix
	// so we can scope the post-uninstall assertion to THIS install's
	// VMs (leftover from manual testing on the same cluster don't
	// confound the test).
	prefix := "redis-ha-" + shortInstanceUUID(inst.InstanceUuid) + "-redis-"
	_, err = c.Client.UninstallPlugin(ctx, &weftv1.UninstallPluginRequest{
		Name:         "redis-ha",
		InstanceUuid: inst.InstanceUuid,
	})
	c.require(err == nil, "UninstallPlugin redis-ha: %v", err)
	cleaned := c.eventually(90*time.Second, func() bool {
		vms, err := c.Client.ListVMs(ctx, &weftv1.ListVMsRequest{})
		if err != nil {
			return false
		}
		return countRedisWithPrefix(vms.Vms, prefix) == 0
	})
	c.expect(cleaned, "redis-ha replicas for instance %s still present after uninstall", inst.InstanceUuid[:8])
}

// shortInstanceUUID returns the first 8 chars of the instance UUID —
// matches pluginstore's qualifiedName/replicaName shortUUID convention
// so VM-name prefix matching lines up.
func shortInstanceUUID(u string) string {
	if len(u) >= 8 {
		return u[:8]
	}
	return u
}

// countRedisWithPrefix tallies VMs whose name starts with the given
// instance-scoped prefix. Used by the uninstall test to assert only
// THIS install's VMs are gone, not every redis-ha-* across the cluster.
func countRedisWithPrefix(vms []*weftv1.VMInfo, prefix string) int {
	n := 0
	for _, v := range vms {
		if strings.HasPrefix(v.Name, prefix) {
			n++
		}
	}
	return n
}

// countRedis tallies how many VMInfo entries are redis-ha-derived
// (any project, any host). Used by the install + uninstall tests.
func countRedis(vms []*weftv1.VMInfo) int {
	n := 0
	for _, v := range vms {
		if strings.HasPrefix(v.Name, "redis-ha-") && strings.Contains(v.Name, "-redis-") {
			n++
		}
	}
	return n
}
