# etcd micro-VM

3-DC etcd cluster serving as the platform's control-plane storage.
Backs every weft registry through `EtcdStorage` (see
[../../storage.go](../../storage.go)) and dex's session store.

## What's special about etcd's bootstrap

etcd is the foundation : nothing else can come up before it. So
**step 1 of the platform bootstrap** brings up etcd from a weft
that's still running in **FILE** storage mode (per
[infra_in_micro_vms.md](../../../../../../.claude/projects/-Users-david-delavennat-Documents-VCS-GIT-localhost-cloud-boot/memory/infra_in_micro_vms.md)).

```sh
# 1. weft is in FILE mode — projects.hcl on disk, no etcd yet.
weft infra deploy etcd
# Pulls quay.io/coreos/etcd:v3.6.0 from the upstream registry
# (or zot once zot is up, see ../zot/README.md), creates 3 VMs
# under project "infra", names "etcd-dc1", "etcd-dc2", "etcd-dc3",
# attaches each to volume "etcd-data-dc<N>", boots, polls
# the health check until all 3 are Ready, declares the cluster
# alive.

# 2. Verify across all 3 endpoints.
etcdctl --endpoints=https://10.255.1.10:2379,https://10.255.1.11:2379,https://10.255.1.12:2379 \
  endpoint health
```

After this step, all other infra services can be deployed with
`storage_backend: etcd:https://10.255.1.10:2379,...` in their
plans.

## TLS bootstrap

The etcd VMs need server + peer certs at deploy time. weft's
deploy command generates them from a local CA (`~/.config/weft/
ca.{crt,key}`) on first run. The CA cert is then exposed via
virtio-fs to every infra-VM that talks to etcd (dex, zot, weft
itself), so they all trust the same root.

After self-promote (step 5 of the bootstrap dance) the CA can be
rotated to a per-DC intermediate — the etcd cluster supports
live cert reload via SIGHUP.

## Disaster recovery

Each etcd-VM's `/var/lib/etcd` is on its own dedicated volume.
Losing one DC's VM is non-fatal (HA via Raft quorum); losing 2
of 3 stops writes until quorum is restored.

The volumes survive `weft delete-vm` (volumes are independent
resources keyed by their own UUID, per
[[weft-uuid-keyed-resources]]). To rebuild after host loss :

```sh
weft infra deploy etcd --restore --from-volume=etcd-data-dc1
```

…boots a new VM mounting the existing volume; the etcd binary
reads the WAL and rejoins the cluster.

## Plan source

[plan.hcl](plan.hcl)
