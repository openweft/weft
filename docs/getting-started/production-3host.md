# Day-0 : déployer un cluster weft 3-DC en production

Walkthrough orienté opérateur : zéro à 3 hôtes Debian convergés, fronted
par Caddy, OIDC en place, observabilité câblée, premier VM provisionné.

Prérequis fixes :
- 3 machines bare-metal ou IaaS (Debian 12+, KVM activé dans le BIOS, 1 IP
  routable par hôte, 1 disque dédié pour `/var/lib/weft/`).
- 1 station de travail (Linux ou macOS) avec accès SSH vers les 3 hôtes,
  où vit le binaire `weft` côté opérateur.
- 1 IdP OIDC reachable depuis les 3 hôtes (Dex, Keycloak, Okta, Auth0 —
  voir `../operations/sso/`).
- 1 nom de domaine sous ton contrôle (DNS A vers chaque hôte ou wildcard
  vers un VIP — au choix).

Volume horaire estimé : 2 h si l'IdP est déjà configuré, 4 h si tu
provisionnes Keycloak en même temps.

## Étape 1 — installer la CLI sur la station

```sh
gh release download v0.1.0 --repo openweft/weft \
  --pattern "weft-$(uname -s | tr A-Z a-z)-$(uname -m | sed s/x86_64/amd64/)_*.tar.gz"
tar xzf weft-*.tar.gz
sudo mv weft /usr/local/bin/
weft version
```

Vérifie la signature avant d'exécuter — voir
[../operations/cosign-verify.md](../operations/cosign-verify.md) pour la
commande `cosign verify-blob` à passer en amont.

## Étape 2 — provisionner les 3 hôtes

Pose `examples/cloud-init/debian-host.yaml` dans ta seed d'installation
(ISO custom, PXE, ou drop dans Tart/Proxmox). Le fichier installe le
binaire `weft` côté agent, crée le service systemd, ouvre les ports
firewall (etcd 2379/2380, mesh WireGuard UDP, gRPC 9090, metrics 9101) et
laisse l'agent en attente de configuration.

À la sortie de cloud-init, chaque hôte est joignable en SSH (clé que tu
as déposée via `ssh_authorized_keys`) mais l'agent n'a pas encore de
config — il logue `awaiting /etc/weft/weft.hcl`.

Vérifie depuis la station :

```sh
for ip in 10.0.0.11 10.0.0.12 10.0.0.13; do
  ssh admin@$ip systemctl is-active weft.service
done
# expected: 3× "activating" (waiting for config)
```

## Étape 3 — écrire `cluster.hcl`

Sur la station, dans un dossier dédié :

```hcl
cluster "prod" {
  overlay { subnet = "10.9.0.0/24" }

  agent_config {
    socket = "/var/run/weft/weft.sock"

    oidc {
      issuer    = "https://sso.example.com/realms/weft"
      client_id = "weft-agent"
    }

    storage {
      backend = "etcd"
      etcd {
        endpoints = [
          "http://10.9.0.11:2379",
          "http://10.9.0.12:2379",
          "http://10.9.0.13:2379",
        ]
      }
    }

    proxy {
      enabled = true
      acme    { email = "ops@example.com" }
    }

    metrics_listen = ":9101"
    audit_log      = "/var/log/weft/audit.jsonl"
  }

  host "h1" { address = "10.0.0.11" dc = "dc1" hypervisor = "qemu" ssh { user = "admin" } }
  host "h2" { address = "10.0.0.12" dc = "dc2" hypervisor = "qemu" ssh { user = "admin" } }
  host "h3" { address = "10.0.0.13" dc = "dc3" hypervisor = "qemu" ssh { user = "admin" } }
}
```

Remplace les IPs / domaines / IdP par les tiens. Le block `proxy` active
Caddy en supervisor avec ACME — il faut que tes 3 hôtes soient joignables
en TCP 80/443 pour la validation HTTP-01, sinon utilise un challenge DNS
(voir `../operations/proxy.md`).

Pour piloter l'`hypervisor` : `qemu` est le défaut Linux/KVM portable.
`vz` n'a de sens que pour un hôte macOS (Apple Virtualization) — pas
recommandé en prod, voir mémoire `env_no_nested_virt`.

## Étape 4 — `weft up`

```sh
weft up -f cluster.hcl --apply
```

Le planner fait : provisionner SSH key vers chaque hôte → pousser
`/etc/weft/weft.hcl` → pull les images OCI (`weft-microvm-kernel`,
drivers, `weft-proxy`) sur chaque hôte → démarrer `weft.service` → former
le quorum etcd → activer Caddy → enregistrer chaque hôte dans la
registry.

Sortie attendue (~3-5 min) :

```
[1/3] h1: weft.hcl pushed, agent started, joined cluster
[2/3] h2: weft.hcl pushed, agent started, joined cluster
[3/3] h3: weft.hcl pushed, agent started, joined cluster
cluster prod ready (3 hosts, quorum: 3/3, proxy: enabled)
```

Si un hôte refuse de rejoindre, voir
[../operations/ha-failover.md](../operations/ha-failover.md#partition).

## Étape 5 — valider la convergence

```sh
weft host ls
# expected: 3 hosts state=Running, az=dc{1,2,3}

weft cluster status
# expected: etcd quorum=3/3, proxy=running, drivers=qemu

curl -s https://prod.example.com:9101/metrics | head -10
curl -s https://prod.example.com:9101/metrics | head -10
curl -s https://prod.example.com:9101/metrics | head -10
# expected: prometheus exposition format, grpc_server_* family present
```

Si Prometheus est déjà déployé, scrape les 3 endpoints avec le label
`instance=<dc>`. Import du dashboard :
[../operations/grafana/README.md](../operations/grafana/README.md).

## Étape 6 — provisionner le premier VM

```sh
weft instance start \
  --project default \
  --name canary \
  --image ghcr.io/openweft/debian-12-cloud:latest \
  --cpu 2 --memory 2048 \
  --network default
```

Une fois Running :

```sh
weft instance ls
weft instance status canary
weft instance logs canary --follow
```

Le scheduler a posé le VM sur l'un des 3 hôtes selon contraintes (aucune
SchedulingRule par défaut → équilibrage simple par CPU disponible).

## Étape 7 — déployer la webui

La webui (HuMA + Svelte) est un binaire séparé. Trois choix de déploiement :

1. **Bare metal**, à côté de chaque agent (systemd unit fournie dans
   `examples/cloud-init/`).
2. **Kubernetes**, via le Helm chart de
   [../../charts/weft-agent/](../../charts/weft-agent/) qui démarre aussi
   la webui en sidecar.
3. **Container standalone** :
   `docker run -p 8088:8088 ghcr.io/openweft/weft-webui:v0.1.0 \
     -e WEBUI_OIDC_ISSUER=... -e WEBUI_OIDC_CLIENT_ID=... \
     -e WEBUI_AGENT_ADDR=10.9.0.11:9090`

Configure une route Caddy pour `https://weft.example.com` → webui
(éditée via la registry route ; voir `../operations/proxy.md`).

## Étape 8 — câbler Terraform pour les workloads

Sur la station ou le poste developer :

```hcl
terraform {
  required_providers {
    weft = { source = "openweft/weft" version = "~> 0.1" }
  }
}

provider "weft" {
  agent_addr = "10.9.0.11:9090"
  # OIDC token de service-account, voir docs/operations/rbac.md
}

resource "weft_volume" "data" {
  project  = "default"
  name     = "app-data"
  size_gib = 50
}
```

`terraform init && terraform apply` provisionne ressources comme du
plain IaC. 33 RPCs sur 98 sont aujourd'hui exposés via le provider —
le reste tombe sous la CLI ou la webui (voir
`../../GAPS.md`).

## Étape 9 — installer un premier plugin du catalogue (optionnel)

Si tu as besoin de runners CI immédiatement :

```sh
weft plugin list
weft plugin install gitlab-runners-ha \
  --input registration_token=$(cat /tmp/gitlab-token) \
  --input gitlab_url=https://gitlab.example.com
```

Le plugin lance 3 runner VMs spread sur les 3 DCs avec anti-affinity
forte. Voir [../catalogue/README.md](../catalogue/README.md) pour les
autres plugins (github-runners-ha, forgejo-runners-ha, jupyterhub-ha).

## Checklist day-0 finale

Tu cliques `OK` quand chaque ligne est verte :

- [ ] 3 hôtes `weft host ls` state=Running
- [ ] Quorum etcd = 3/3 (`etcdctl endpoint health --cluster`)
- [ ] Caddy répond 200 sur `https://<your-domain>/`
- [ ] OIDC login web fonctionne (`https://weft.example.com/` → IdP → callback)
- [ ] `/metrics` répond sur chaque hôte
- [ ] Prometheus scrape les 3 endpoints
- [ ] Grafana dashboard importé et populated
- [ ] 1 VM canary démarre, ping survit
- [ ] Snapshot reflink fonctionne (`weft volume snapshot create --volume=<uuid> --name=test`)
- [ ] Premier backup etcd pris (`docs/operations/etcd-backup.md` step 1)
- [ ] Audit log écrit dans `/var/log/weft/audit.jsonl` à chaque login

## Day-1 et au-delà

Les bouclages suivants sont documentés dans des runbooks séparés :

- Backup / restore etcd — [../operations/etcd-backup.md](../operations/etcd-backup.md)
- Backup off-host des snapshots — [../operations/backup.md](../operations/backup.md)
- Failover HA — [../operations/ha-failover.md](../operations/ha-failover.md)
- Disaster recovery (quorum perdu) — [../operations/disaster-recovery.md](../operations/disaster-recovery.md)
- Upgrade rolling v0.X → v0.Y — [../operations/upgrade.md](../operations/upgrade.md)
- GPU scheduling H200 / RTX 6000 Ada — [../operations/gpu-scheduling.md](../operations/gpu-scheduling.md)
- Tenant quotas — [../operations/tenant-quotas.md](../operations/tenant-quotas.md)
- RBAC + audit log — [../operations/rbac.md](../operations/rbac.md)
- Cosign verification — [../operations/cosign-verify.md](../operations/cosign-verify.md)
- Observability — [../operations/observability.md](../operations/observability.md)

## Ce qui n'est pas (encore) couvert

- Tests bare-metal hors Tart : le harness 3-host
  (`tests/integration/3host/`) compile, mais n'a jamais été exécuté
  contre du métal réel — les manifestations de bug spécifiques au métal
  sont un découvertes-pour-l'opérateur.
- Per-VM device passthrough fin (PCI, USB) : la base est prête côté
  driver QEMU, le bonnement de l'API est à venir.
- Multi-cluster fédération : un seul cluster (1-host ou 3-DC) en V1.

Si tu butes : ouvre une issue sur
[github.com/openweft/weft](https://github.com/openweft/weft) avec le
output de `weft cluster status -o json` + `journalctl -u weft.service`
des hôtes affectés.
