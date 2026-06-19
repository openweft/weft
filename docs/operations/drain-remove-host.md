# Removing a hypervisor : drain + remove

Operator runbook for taking a host out of a running weft cluster
without losing user VMs or destabilising etcd quorum. Three phases :
**cordon** (stop new schedules), **drain** (move existing VMs off),
**remove** (delete the registry entry + optionally drop the etcd
voting membership).

Always cordon + drain first. `weft host rm` is destructive at the
registry level but does **not** stop VMs running on the host — orphan
VM records are the most common consequence of skipping drain.

## 1. Cordon the host

```sh
weft host cordon <uuid-or-hostname>
```

The cordon flag flips immediately ; the scheduler drops the host from
candidate sets on the next placement decision. Existing VMs stay put,
the host stays Active + reachable. The operation is idempotent — re-running
on an already-cordoned host is a no-op.

Verify :

```sh
weft host show <uuid>
# expected: cordoned=true, state=active
```

If you change your mind at this point, `weft host uncordon <uuid>`
reverts ; no other state has changed.

## 2. Drain : move VMs off the host

**v0.1 status** — there is no `weft instance migrate` verb yet (live
migration is on the V0.2 track). Drain in v0.1 means : **stop the VM,
then start it again so the scheduler re-places it on a different
host**.

For each VM running on the host :

```sh
# Identify VMs pinned to this host
weft instance ls --host <uuid>

# For each, stop and restart — scheduler picks a new host since the
# current one is cordoned.
weft instance stop <vm-uuid>
weft instance start <vm-uuid>
```

For SchedulingRule-managed VMs (HA platform plugins, runner pools),
just `stop` is enough — the rule's Reconciler respawns the VM
elsewhere automatically. Reference :
[`project_respawn_v013_true_ha`](../../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/project_respawn_v013_true_ha.md).

For stateful VMs (etcd members, databases) drain in this order :

1. Confirm the workload has its own HA story (replicas elsewhere,
   leader election, etc.).
2. Stop on the draining host.
3. Wait for the replacement to catch up (etcd : `member list` shows
   the new member healthy ; Postgres-HA : the standby promotes).
4. Only then proceed to step 3.

Stateful workloads without their own HA cannot be drained
non-disruptively — schedule a maintenance window or accept the
restart-time downtime.

### Verify drain

```sh
weft host show <uuid>
# expected: cordoned=true, active_vms=0

weft instance ls --host <uuid>
# expected: empty
```

If any rows remain, repeat step 2 for them before continuing. **Do
not** proceed to remove with active VMs on the host.

## 3. Remove from the registry

```sh
weft host rm <uuid>
```

What this does :

- Deletes the host from the inventory (`weft host ls` no longer
  shows it).
- Removes its mesh peer entry from the other hosts on the next
  reconcile pass.
- Cancels its heartbeat lease.

What this does **not** do :

- Stop any VM still running on the host (that's why step 2 matters).
- Remove the host's etcd voting member (see step 5).
- Power off the host or stop `weft.service` — do that out-of-band
  once the registry is clean.

## 4. If the host was in `cluster.hcl`

The convergent path requires the HCL to match reality. After step 3 :

```hcl
cluster "prod" {
  # remove the host "h4" { ... } block

  host "h1" { address = "10.0.0.11" dc = "dc1" hypervisor = "qemu" ssh { user = "admin" } }
  host "h2" { address = "10.0.0.12" dc = "dc2" hypervisor = "qemu" ssh { user = "admin" } }
  host "h3" { address = "10.0.0.13" dc = "dc3" hypervisor = "qemu" ssh { user = "admin" } }
}
```

Then re-apply :

```sh
weft up -f cluster.hcl --apply
```

The planner cleans up the mesh peer references on the remaining hosts
and re-emits agent configs without `h4`. If you skip this step, the
host is gone from the registry but the HCL diverges from reality —
future operators applying that HCL will silently re-add the host on
the next `weft up --apply`.

## 5. Remove the etcd voting member (only if applicable)

This step applies **only** if the host was carrying an etcd voting
member. Hosts added via `weft host register` (Path B in
[scale-out.md](scale-out.md)) typically are not.

Check first :

```sh
etcdctl --endpoints=$E1,$E2,$E3 member list -w table
# look for a member whose peerURL points at the host being removed
```

If a row matches, remove it :

```sh
etcdctl --endpoints=$E1,$E2,$E3 member remove <member-id>
```

**Quorum warning** — dropping a member shrinks the voting set. Valid
counts are 3 → 5 → 7 (odd numbers). If you're at 3 members and remove
one, you're down to 2 — quorum is now 2/2 and **any further outage
takes the cluster read-only**. The safe sequence to shrink permanently
is :

1. Grow to 5 (add two new hosts to the HCL, see
   [scale-out.md](scale-out.md)).
2. Drain + remove the host you're replacing.
3. `etcdctl member remove` against its member ID.
4. Cluster now sits at 4 members — uncomfortable but writable.
5. Drain + remove one more, ending at 3.

For temporary maintenance (host coming back), prefer cordon over
remove ; the etcd member stays in place, quorum is unaffected, and
the host re-joins automatically on next boot.

## Common pitfalls

**Removing a host with active VMs.** `weft host rm` succeeds, but
leaves orphan VM records keyed to the deleted host UUID. ZombieGC
(memory ref :
[`project_zombiegc`](../../../../.claude/projects/-Volumes-My-Shared-Files-share-github-com/memory/project_zombiegc.md))
eventually reaps them, but reads against `weft instance ls` are noisy
in the meantime (rows with `host=<deleted-uuid>`). Always drain first.

**Dropping an etcd voting member without `member remove`.** If you
just power off the host (or `weft host rm` without step 5), the
etcd cluster keeps the member in its config and tries to reach it on
every write. The cluster degrades to N-1 effective voters with the
same quorum threshold — you've lost a voter without lowering the
threshold. Always `member remove` *before* the host goes permanently
offline.

**Removing the wrong host.** `weft host rm` takes a UUID, not a
hostname — UUIDs look like hostnames at a glance and a typo here
destroys the wrong inventory entry. Run `weft host show <uuid>` and
read back the hostname before the delete. There is no undo (the host
can be re-registered, but VM history attached to the deleted UUID is
lost).

**Forgetting the HCL clean-up.** A host removed from the registry but
still in `cluster.hcl` will silently be re-added on the next
`weft up --apply`. Always do step 4 right after step 3 ; treat them
as a single transaction.
