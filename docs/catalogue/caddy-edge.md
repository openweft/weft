# `caddy-edge`

A 3-replica Caddy farm at network edge, serving as the north-south
L7 ingress for tenant workloads. ACME-managed TLS by default.

When you want **an external-facing HTTPS proxy farm fronting the
cluster from outside**, typically behind a cloud LB or anycast IP.

## Why this is separate from the in-cluster proxy

weft-agent embeds Caddy (`project_reverse_proxy_caddy`) for
*cluster-internal* routing only — no public listener. This plugin
gives you a **distinct** Caddy farm with its own trust boundary,
blast radius, and upgrade cadence for the external traffic plane.

## What it does

- Creates an `edge` network (`10.54.0.0/24`, NAT) and a
  `caddy-edge-public` SG: 80+443/tcp ingress from `0.0.0.0/0`, egress
  to upstreams + Let's Encrypt + the config URL + DNS.
- Creates **three** micro-VMs (2 vCPU, 2 GiB RAM, 10 GiB root + a
  2 GiB `certs` ACME cache at `/data/caddy`), hard anti-affinity.
- Each replica fetches `caddy_config_url` at boot and polls it every
  `config_poll_seconds`.

## Inputs

| Input                  | Required | Secret | Default                              | Notes                                                  |
|------------------------|----------|--------|--------------------------------------|--------------------------------------------------------|
| `image`                | no       | no     | `ghcr.io/openweft/weft-proxy:v0.1.0` | Same binary as the in-cluster proxy, standalone mode   |
| `caddy_config_url`     | yes      | no     | —                                    | HTTPS URL of Caddyfile / JSON config                   |
| `acme_email`           | yes      | no     | —                                    | Let's Encrypt expiry-warning address                   |
| `listen_https`         | no       | no     | `443`                                | Public HTTPS port                                      |
| `listen_http`          | no       | no     | `80`                                 | HTTP→HTTPS redirect + ACME HTTP-01                     |
| `config_poll_seconds`  | no       | no     | `30`                                 | Poll interval for the config URL                       |
| `trusted_proxies_cidr` | no       | no     | `0.0.0.0/0`                          | Upstream LB CIDR trusted for `X-Forwarded-For`         |

## Operator pre-flight

1. **Host the Caddy config** somewhere all three replicas can reach
   over HTTPS (internal `versitygw-ha` bucket, presigned S3 URL, raw
   GitHub blob, your CDN). Caddyfile or JSON, auto-detected.

2. **Plan the public IP.** Three replicas → three private addresses.
   Front them with a cloud LB, anycast IP, or DNS round-robin.

3. **Install.**

   ```
   weft plugin install caddy-edge \
     --project edge \
     --input caddy_config_url=https://s3.example.com/caddy.json \
     --input acme_email=ops@example.com \
     --input trusted_proxies_cidr=10.99.0.0/16
   ```

4. **Wire DNS** to the LB / anycast IP, or the three replica addresses
   returned by `weft plugin status caddy-edge`.

## Verify

```
curl -I http://hub.example.com/      # 308 → https://hub.example.com/
curl -I https://hub.example.com/     # 200, served via Caddy

# Inspect live config after pushing a new caddy.json
# (admin endpoint is loopback-only):
weft instance shell caddy-edge-<short>-caddy-0 -- \
  curl -s http://127.0.0.1:2019/config/apps/http/servers/srv0
```

## What's NOT included

- **L4 (TCP/UDP) ingress**: L7 only. For L4 layer in `caddy-l4`
  via a custom config ; BGP egress is `project_router_lb_gonative`.
- **WAF / bot management**: no ModSecurity-style rules. Combine
  with a CDN (Cloudflare, Fastly) in front if you need them.
- **OIDC at the edge**: Caddy can verify JWTs but this plugin
  doesn't preconfigure it ; add `caddy-security` to your config.
- **Internal cluster routing**: tenant→tenant routes belong inside the agent's embedded Caddy, not here.
