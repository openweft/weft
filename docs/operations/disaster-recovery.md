# Disaster recovery — cold-start from etcd snapshot

**TL;DR** — quorum is gone, the cluster cannot be revived by booting
a host back. With a recent etcd snapshot, the `cluster.hcl`, and
three fresh hosts, this runbook restores the snapshot into a new
3-member etcd cluster, starts weft on all three hosts, and validates
the control plane. 15-30 min on a lab cluster.

## When this runbook applies

The **cold-start** path. Use it only when:

- All three etcd peers are offline **and** at least two of the
  underlying hosts are permanently destroyed (disks gone), or
- Two hosts are destroyed and the single survivor's etcd data dir is
  itself corrupted.

i.e. no surviving etcd member from which raft can re-establish quorum
by adding new peers. The cluster must be rebuilt **from a snapshot**.

If one healthy etcd member survives, use
[`ha-failover.md`](ha-failover.md) instead (single-DC loss with quorum
intact) and replace dead members with `etcdctl member remove` +
`member add`. Coming here destroys any committed writes that exist on
the survivor but not in the snapshot, so double-check first.

## What you need before starting

1. **A verified etcd snapshot**. See
   [`etcd-backup.md`](etcd-backup.md) for creation and `snapshot
   status` verification — use the freshest one that passes status.
2. **The cluster's `cluster.hcl`**. Source of truth for host
   identities, DC layout, network CIDRs, peer URLs. If lost, stop
   (see ["Not covered"](#what-this-runbook-does-not-cover)).
3. **Three fresh hosts**, provisioned per
   [`cloud-init.md`](cloud-init.md) /
   `examples/cloud-init/debian-host.yaml`. Reachable over SSH and
   from each other on etcd client (2379) and peer (2380) ports.
4. **`etcdctl` v3.5+** on operator workstation and each host.

New hosts need not reuse old hostnames/IPs unless `cluster.hcl` pins
them. If IPs change, edit `cluster.hcl` first — the
`--initial-cluster` flag below must match what each `weft agent`
advertises.

## The restore procedure

### Step 1 — fence the survivor

If one host is still running etcd (even degraded), **stop it now**.
Restoring while a survivor is reachable causes split-brain.

```sh
ssh root@<surviving-host> 'systemctl stop weft'
ssh root@<surviving-host> 'mv /etc/weft/etcd-embed/data /etc/weft/etcd-embed/data.preserved.$(date +%s)'
```

Move, don't delete — recoverable if the restore turns out wrong.

### Step 2 — restore the snapshot on host 1

Copy the snapshot to host 1, then `etcdctl snapshot restore` with
**all three** new peer URLs in `--initial-cluster` and a freshly
generated `--initial-cluster-token` (any string unique to this
restore — must differ from any token a previous cluster used).

```sh
scp etcd-20260531T0300.db root@host1:/var/tmp/snap.db
ssh root@host1
TOKEN=weft-restore-20260601
sudo etcdctl snapshot restore /var/tmp/snap.db \
    --name host1 \
    --initial-cluster host1=http://10.0.1.1:2380,host2=http://10.0.2.1:2380,host3=http://10.0.3.1:2380 \
    --initial-advertise-peer-urls http://10.0.1.1:2380 \
    --initial-cluster-token "$TOKEN" \
    --data-dir /etc/weft/etcd-embed/data
```

Substitute names from `cluster.hcl` and the peer URLs each host
advertises. The data dir must match what `weft agent` expects
(default `<configDir>/etcd-embed/data`, see `cmd/weft/embed_etcd.go`).

### Step 3 — seed the other two hosts

Each member needs its **own** data dir but must start from the **same
restored state**. Re-run `snapshot restore` on host 2 and host 3 with
their own `--name` / `--initial-advertise-peer-urls`, but the
**same** snapshot, `--initial-cluster`, and `--initial-cluster-token`:

```sh
# On host 2 (same on host 3, swap host2→host3 and 10.0.2.1→10.0.3.1):
sudo etcdctl snapshot restore /var/tmp/snap.db \
    --name host2 \
    --initial-cluster host1=http://10.0.1.1:2380,host2=http://10.0.2.1:2380,host3=http://10.0.3.1:2380 \
    --initial-advertise-peer-urls http://10.0.2.1:2380 \
    --initial-cluster-token "$TOKEN" \
    --data-dir /etc/weft/etcd-embed/data
```

Do **not** scp host 1's data dir to host 2/3 — that clones member
identity and etcd refuses to form a cluster. Fix ownership on all
three: `sudo chown -R weft:weft /etc/weft/etcd-embed/data`.

### Step 4 — start weft on all three hosts (simultaneously)

`weft agent` must start etcd with `--initial-cluster-state=existing`
so it joins the pre-declared 3-member set instead of bootstrapping a
new single-member one. Set in each host's `weft.hcl` **before**
starting (see `cmd/weft/embed_etcd.go` for the mapping):

```hcl
storage { etcd { initial_cluster_state = "existing" } }
```

Then start all three together — each member blocks on raft until a
majority is up. Election completes ~30s after the third is running:

```sh
ssh root@host1 'systemctl start weft' &
ssh root@host2 'systemctl start weft' &
ssh root@host3 'systemctl start weft' &
wait
```

### Step 5 — validate quorum

```sh
etcdctl --endpoints=http://10.0.1.1:2379,http://10.0.2.1:2379,http://10.0.3.1:2379 \
    endpoint status -w table --cluster
```

Expected: three rows, one `IS LEADER`, same `RAFT TERM`, `DB SIZE`
close to the snapshot's `TOTAL SIZE`. A missing or timing-out row
means that host's `weft agent` didn't start cleanly — check logs
(usually a peer URL mismatch between the restore command and the
agent's advertise).

## Validation

- [ ] `etcdctl endpoint status -w table --cluster` shows 3 members,
  one leader, identical raft term.
- [ ] `etcdctl endpoint health --cluster` shows all 3 healthy.
- [ ] `weft hosts list` returns rows; every host in `cluster.hcl`
  appears, initially `state=down` until each agent re-registers
  (~30s).
- [ ] `weft microvm list` shows the inventory **as of the snapshot
  moment** (see "What's lost").
- [ ] Smoke-test VM create succeeds (`weft microvm register
  --name=postrestore-canary --image=ghcr.io/openweft/weft-test-fixture:latest
  --placement="dc:any"`).

## What's lost

Anything committed to etcd **after** the snapshot is gone:

- **VMs created after the snapshot** — no registry row. Their qcow2 /
  reflink disks may still be on the hyperviser's local storage but
  are unreferenced.
- **Catalogue mutations** — image pulls, network / security-group
  edits, scheduling rules, tenant changes.
- **Dynamic per-VM config writes** — mesh / mount config pushed via
  NATS between snapshot and failure.

Reconciling orphan disks against the restored registry is a separate
operation (TODO: `docs/operations/orphan-reconciliation.md`).
Conservative move: list each host's state dir against `weft microvm
list`, decide per-VM whether to re-register or delete.

## What this runbook does NOT cover

- **Loss of hyperviser disks** (VM disks themselves gone). Recovery
  depends on the per-host volume backup strategy (rsync, btrfs-send,
  Longhorn replication, ...). TODO:
  `docs/operations/host-disk-recovery.md`.
- **Loss of the `cluster.hcl`**. No recovery — operators must back
  it up out-of-band alongside the etcd snapshot.
- **External HA etcd backend** (`storage-backend=etcd` against a
  non-embedded cluster). Same shape, commands run against the
  external members; see upstream etcd "Restoring a cluster".
