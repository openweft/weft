# weft-e2e — live end-to-end harness

A single binary you point at a running weft cluster ; runs a battery
of operator-visible assertions and exits non-zero on the first
regression.

## Why

Unit tests catch logic errors in isolation. Every cross-host bug
we've debugged recently (anti-affinity collapse, OCI label drift,
RestartVM falling into the local-default-project resolver, Uninstall
leaving cross-host VMs alive) only surfaces when an RPC actually
crosses DCs. This harness exercises the platform end-to-end against
a real cluster + asserts the invariants those bugs broke.

## Run

```
# Smoke suite (~1s, read-only) against the local agent
weft-e2e

# Smoke against a remote host via SSH
weft-e2e --ssh-socket admin@dc1-r1-h1:/home/admin/.weft/weft-ssh.sock \
         --ssh-key ~/.ssh/id_ed25519

# Full suite (~3 min, installs + uninstalls redis-ha)
weft-e2e --suite=full -v

# Re-run a single test by name
weft-e2e --suite=full --run=uninstall
```

Exit codes : 0 = all passed, 1 = ≥1 failure, 2 = transport / setup
error before any test ran.

## What it asserts

### Smoke (always safe — read-only)

- **hosts/list** — `ListHosts` returns ≥1 host.
- **hosts/all-active** — every host has `state="active"`.
- **hosts/all-connected** — every host's UUID appears in
  `connected_host_uuids` (catches a registered-but-down host).
- **vms/list** — `ListVMs` returns without error.
- **vms/cross-host-visible** — on a multi-host cluster, VMs cover
  ≥2 distinct host UUIDs (catches a broken cross-host registry merge).
- **vms/image-label-not-placeholder** — no VM carries the synthetic
  `microvm/direct_linux` image label (catches stale records that
  never got lifted by V0.4.71's OCI plumbing).
- **vms/restart-cross-host** — picks a VM on a non-local host, calls
  `RestartVM`, asserts success (catches the cross-host dispatch gap
  that produced "kernel not found at state/vz/<usr-admin>/<vm>").
- **plugin/catalogue-non-empty** — `ListPluginCatalogue` returns ≥1
  entry (catches a broken static fallback when etcd hasn't seeded).
- **plugin/installed-snapshot** — `ListInstalledPlugins` returns
  without error.

### Full (mutates cluster state)

- **plugin/install-redis-ha-3dc-spread** — uninstalls any existing
  redis-ha, installs fresh, waits for 3 replicas, asserts spread
  over ≥3 distinct hosts.
- **plugin/uninstall-redis-ha-cleans-all-dcs** — uninstalls the
  redis-ha instance THIS suite just created (filtered by instance
  UUID prefix) + asserts every replica VM is gone from every host.

## Adding a test

Drop a `Case` literal into `cases.go`'s `allCases` slice :

```go
{Name: "my/check", Suite: "smoke", Order: 50, Fn: testMyCheck},
```

Then the function :

```go
func testMyCheck(c *Ctx) {
    ctx, cancel := bg(10 * time.Second)
    defer cancel()
    resp, err := c.Client.ListVMs(ctx, &weftv1.ListVMsRequest{})
    c.require(err == nil, "ListVMs: %v", err)
    c.expect(len(resp.Vms) > 0, "want ≥1 VM")
    c.logf("found %d VMs", len(resp.Vms))
}
```

`require` panics + fails the test ; `expect` records the failure
but continues so the run can collect every broken assertion at
once. `logf` prints under `-v`. `c.eventually(timeout, fn)` polls
for cluster convergence.

## CI

Wire into a GitHub Actions workflow against a live test cluster
(the 3-DC Tart fleet under dc1-r1-h1, dc2-r1-h1, dc3-r1-h1) ; the
binary needs `--ssh-socket` + `--ssh-key` to reach the seed agent.
Phase 3 will host this on a Docker-in-Docker harness so CI doesn't
need a live cluster.
