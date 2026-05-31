# etcd backup + restore

Operator runbook for snapshotting and restoring the weft cluster's etcd
state. Read once end-to-end before drilling production; the embedded
backend has a few non-obvious endpoint-discovery quirks.

## What lives in etcd

weft persists **cluster state** in etcd: host registry, image / network /
volume / security-group catalogues, scheduling rules, dynamic per-VM
config (mesh + mounts), tenant + keypair registries. A clean restore
brings the control plane back to the snapshotted moment.

**Not in etcd** — per-VM disk volumes (raw qcow2 / reflink clones under
the agent state dir). Those need a separate backup story (rsync /
zfs send / btrfs send), and crash-consistency between an etcd snapshot
and the underlying volumes is the operator's responsibility. Flag this
when planning DR: an etcd-only restore brings back the catalogue but the
clones it references must still exist on disk.

## Prerequisites

- `etcdctl` v3.5+ on PATH (`pkgx etcdctl` works).
- Write access to the backup destination dir.
- For the embedded backend (single-host dev / lab): SSH to the host
  running `weft agent`, since the embedded etcd binds to loopback only.
- For the external HA backend: client TLS material + endpoint list as
  configured in `cluster.hcl` `storage.etcd { endpoints }`.

## Backend variants

weft runs etcd in two shapes (per `storage-backend` flag):

| Backend | Where it lives | How to reach it |
|---|---|---|
| `embed-etcd` | in-process, `<configDir>/etcd-embed/data`, kernel-picked loopback ports | local-only, see below |
| `etcd` | external 3-DC cluster | `storage.etcd.endpoints` from `cluster.hcl` |

The embedded mode picks free 127.0.0.1:0 ports at startup so two
operators on the same box don't collide (see `cmd/weft/embed_etcd.go`).
This means **the client URL changes every restart** and is not exposed
on a fixed port. Two ways to discover it:

```sh
# 1. From the weft startup log line (preferred — printed by the storage factory):
journalctl -u weft -o cat | grep -m1 'embed-etcd endpoints'
# → embed-etcd endpoints=[http://127.0.0.1:54321]

# 2. From the listening sockets of the weft process:
sudo lsof -nP -iTCP -sTCP:LISTEN -a -p "$(pgrep -x weft)" | grep 127.0.0.1
# The client URL is the lower-numbered of the two loopback ports;
# the higher one is the peer URL (unused from outside).
```

The embedded backend has **no client auth** (loopback only, same-process
trust model). Skip `--user` / `--cacert` when dialling it.

## Snapshot save

### Embedded backend

```sh
EP=$(journalctl -u weft -o cat | grep -m1 'embed-etcd endpoints' \
        | sed -E 's/.*\[(http[^]]+)\].*/\1/')
etcdctl --endpoints="$EP" snapshot save /var/backups/weft/etcd-$(date +%Y%m%dT%H%M%S).db
```

Expected output:

```
{"level":"info","ts":"…","msg":"created temporary db file",…}
{"level":"info","ts":"…","msg":"saved","path":"/var/backups/weft/etcd-….db"}
Snapshot saved at /var/backups/weft/etcd-….db
```

### External HA backend

```sh
etcdctl \
    --endpoints=https://etcd-0:2379,https://etcd-1:2379,https://etcd-2:2379 \
    --cacert=/etc/weft/etcd-ca.pem \
    --cert=/etc/weft/etcd-client.pem \
    --key=/etc/weft/etcd-client-key.pem \
    snapshot save /var/backups/weft/etcd-$(date +%Y%m%dT%H%M%S).db
```

Endpoint discovery happens against any single reachable member; the
snapshot is taken from that member's bbolt mmap (consistent point-in-time,
no quorum stop required).

## Snapshot verify

Always run `snapshot status` immediately after `save` — a zero-byte file
or a torn write will not error on `save` but will fail to restore.

```sh
etcdctl --write-out=table snapshot status /var/backups/weft/etcd-….db
```

Expected output:

```
+----------+----------+------------+------------+
|   HASH   | REVISION | TOTAL KEYS | TOTAL SIZE |
+----------+----------+------------+------------+
| 1a2b3c4d |    87421 |       1842 |      12 MB |
+----------+----------+------------+------------+
```

Smell-test the row: TOTAL KEYS should be in the same ballpark as the
previous backup (catalogue growth is gradual); a sudden drop to <100
keys means the snapshot caught etcd mid-bootstrap.

## Snapshot restore

### Embedded backend

A restore in this mode is a **stop, replace the data dir, start** dance.
`embed.Etcd` reads its bbolt mmap from `<configDir>/etcd-embed/data` at
startup; `etcdctl snapshot restore` builds a fresh data dir from the
.db file, which we then swap in.

```sh
# 1. Stop the agent.
sudo systemctl stop weft

# 2. Move the live data aside (not delete — recoverable if the restore was wrong).
sudo mv /etc/weft/etcd-embed/data /etc/weft/etcd-embed/data.preserved.$(date +%s)

# 3. Restore into the canonical path. The --name + --initial-cluster
#    values MUST match the embedded config (see cmd/weft/embed_etcd.go:
#    Name="weft-embed", InitialClusterToken="weft-embed").
sudo etcdctl snapshot restore /var/backups/weft/etcd-….db \
    --name weft-embed \
    --initial-cluster weft-embed=http://127.0.0.1:2380 \
    --initial-cluster-token weft-embed \
    --initial-advertise-peer-urls http://127.0.0.1:2380 \
    --data-dir /etc/weft/etcd-embed/data

sudo chown -R weft:weft /etc/weft/etcd-embed/data

# 4. Restart. The agent will pick fresh loopback ports — the peer URL
#    in the restored member metadata gets rewritten by raft on first boot.
sudo systemctl start weft
```

### External HA backend

3-node restore: snapshot once, restore to each member with that member's
own name + peer URL, start the cluster from scratch with the same
`--initial-cluster-token`. Treat it as a fresh cluster bring-up; the
data is preserved but member identity is not. See the upstream etcd
"Restoring a cluster" doc for the per-member commands.

## Scheduled backups

systemd timer (preferred on Linux hosts):

```ini
# /etc/systemd/system/weft-etcd-backup.service
[Unit]
Description=weft etcd snapshot

[Service]
Type=oneshot
User=weft
ExecStart=/usr/local/bin/weft-etcd-backup.sh
```

```ini
# /etc/systemd/system/weft-etcd-backup.timer
[Unit]
Description=hourly weft etcd snapshot

[Timer]
OnCalendar=hourly
Persistent=true

[Install]
WantedBy=timers.target
```

The `weft-etcd-backup.sh` wrapper: discover the endpoint (embedded) or
read from `/etc/weft/etcd-endpoints` (HA), run `snapshot save` to a
date-stamped path under `/var/backups/weft/`, then prune anything older
than 14 days with `find /var/backups/weft -mtime +14 -delete`.

Plain cron equivalent if systemd isn't available:

```
0 * * * * /usr/local/bin/weft-etcd-backup.sh >> /var/log/weft-backup.log 2>&1
```

## Restore drill

A backup you've never restored is a wish, not a backup. Quarterly:

1. Spin a throwaway weft agent on a scratch host (`weft agent
   --state-dir /tmp/drill --storage-backend embed-etcd`).
2. Restore yesterday's production snapshot into it (steps above, but
   under `/tmp/drill/etcd-embed/data`).
3. Start the scratch agent, then `weft host list` / `weft image list` /
   `weft network list` and compare row counts against production.
4. Tear down the scratch host.

If step 3 returns empty catalogues, the snapshot is bad — check
`snapshot status` ran against the file you actually restored.

## Troubleshooting

**`etcdctl: command not found`** — `pkgx etcdctl` or install from the
upstream etcd release tarball; the API v3 binary is the one we want.

**Embedded endpoint changed between backup and restore** — that's
expected (kernel-picked ports). The .db file is portable; only the
`--initial-advertise-peer-urls` flag during restore matters, and the
agent rewrites it on first boot.

**`snapshot status` shows TOTAL KEYS = 0** — the snapshot caught etcd
before the agent finished writing the bootstrap revision. Re-take after
a fresh agent restart has settled (`weft host list` returns the
expected rows).

**Restore complete but agent won't start** — check the restored data
dir ownership matches the `weft` user; `embed.Etcd` opens the bbolt
file for write and silently refuses on EACCES.
