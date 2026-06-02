# `grafana-ha`

Three Grafana replicas behind the Caddy proxy plane (sticky-by-cookie
sessions), OIDC auth, CockroachDB-backed state — or SQLite-on-NFS for
small deploys.

When you want **dashboards that survive a DC outage** — pairs with `prometheus-ha` and `loki-ha`.

## What it does

- Creates a dedicated `dashboards` network (`10.62.0.0/24`, NAT) plus
  `grafana-dashboards` SG (3000 ingress from Caddy edge, 9094 gossip
  for unified alerting, egress to Cockroach + OIDC + on-cluster
  `prometheus-ha:9090` / `loki-ha:3100`) and a `grafana-db` SG.
- Three Grafana replicas (2 vCPU, 4 GiB RAM) with `az = "different"`.
- Three CockroachDB replicas (2 vCPU, 4 GiB RAM, 100 GiB data) when
  `db_backend=cockroach` (default) ; skipped on `sqlite-nfs`.
- Emits a `proxy_route "grafana"` block consumed by the Caddy plane
  with `sticky = "cookie:grafana_session"` (required for Explore /
  live-tail / unified-alerting UI).

## Inputs

| Input                | Required | Secret | Default                       | Notes                                                |
|----------------------|----------|--------|-------------------------------|------------------------------------------------------|
| `image`              | no       | no     | `grafana/grafana-oss:11.6`    | Upstream OSS image, no openweft fork                 |
| `admin_password`     | yes      | yes    | —                             | Local admin escape hatch ; OIDC handles day-to-day   |
| `oidc_issuer`        | no       | no     | `""`                          | Empty = inherit weft's own issuer                    |
| `oidc_client_id`     | yes      | no     | —                             | OAuth2 client ID                                     |
| `oidc_client_secret` | yes      | yes    | —                             | OAuth2 client secret                                 |
| `db_backend`         | no       | no     | `cockroach`                   | `cockroach` (HA) or `sqlite-nfs` (small deploys)     |
| `domain`             | yes      | no     | —                             | FQDN Grafana answers on (e.g. `grafana.example.com`) |
| `admin_group`        | no       | no     | `weft:admin`                  | OIDC group → Grafana Admin                           |
| `viewer_group`       | no       | no     | `""`                          | Empty = any authenticated user                       |

## Operator pre-flight

1. `caddy-edge` is installed (Grafana ingress goes through Caddy).
2. OIDC client registered ; redirect URI =
   `https://<domain>/login/generic_oauth`, same issuer weft trusts.
3. DNS for `<domain>` points at the Caddy edge.
4. Pick `db_backend` : `cockroach` (default, 3-node HA) or
   `sqlite-nfs` (small deploys ; same escape hatch as
   `catalogue/jupyterhub-ha`).
5. Install :

   ```
   weft plugin install grafana-ha --project observability \
     --input admin_password=$GF_ADMIN \
     --input oidc_client_id=grafana \
     --input oidc_client_secret=$GF_OIDC \
     --input domain=grafana.example.com
   ```

6. Import the openweft default dashboard (commit `86b71860`) :
   ```
   curl -X POST -H "Content-Type: application/json" -u "admin:$GF_ADMIN" \
     -d @grafana/weft-agent.json \
     https://grafana.example.com/api/dashboards/db
   ```

## Verify

```
curl https://grafana.example.com/api/health   # {"database":"ok","version":"11.6.x"}
# Sticky-session smoke : Caddy pins the cookie to one upstream.
curl -c jar -s https://grafana.example.com/login | grep -i set-cookie
curl -b jar -s https://grafana.example.com/api/user | jq '.login // "anon"'
```

## What's NOT included

- **TLS termination** : handled by `caddy-edge`, not Grafana itself.
- **Alertmanager** : Grafana unified alerting drives contact points
  directly ; a separate Alertmanager plugin is pending for
  Prometheus-rules-based alerting.
- **Plugin sideloading** : enterprise plugins not pre-installed ;
  set `GF_INSTALL_PLUGINS` via a follow-up env override.
- **SMTP** : OIDC bypasses invites + password reset ; wire SMTP only
  for local-admin recovery.
