# dex micro-VM

OIDC identity provider. Federates upstream identity sources
(LDAP, GitHub Enterprise, SAML, …) and issues OIDC tokens to
everyone in the platform : vzd's API, zot's bearer realm, vzc
device-grant logins.

## Why dex needs etcd

Storage backend `type: etcd` (see [plan.hcl](plan.hcl)) means
dex's sessions, refresh tokens, and static-client registrations
live in the same 3-DC etcd cluster that backs vzd's own
registries. Native fit :

- Linearizable — a refresh-token rotation issued on dex-dc1 is
  immediately visible on dex-dc2 / dex-dc3.
- Quorum HA — losing one DC doesn't lose tokens.
- Same backup story — one etcd snapshot covers vzd + dex.

dex's etcd keys live under prefix `/dex/<env>/` ; vzd's under
`/vzd/<env>/`. No collision.

## Bootstrap : statics → federation

The first deploy ships a **static admin user** baked into
[plan.hcl](plan.hcl) (email `admin@<base-domain>`, bcrypt hash
from `$ADMIN_BCRYPT_HASH` injected at deploy time). This is
just-enough auth for vzd to come up and start managing real
identities.

Once dex is reachable and vzd is in etcd-storage mode, federation
to the upstream IdP is a config-only change :

```sh
vzd infra federate-dex --upstream-ldap=ldaps://ldap.example.com \
  --bind-dn='cn=dex,ou=service,dc=example,dc=com' \
  --user-search-base='ou=users,dc=example,dc=com'
```

…which patches the dex config.yaml, gets pushed via etcd, and
each dex VM auto-reloads. The static admin stays as a break-glass
account.

## Token validation in consumers

Every cloud-platform service speaking OIDC against dex follows
the same shape :

1. Caller's HTTP request carries `Authorization: Bearer <token>`.
2. Server validates the token's signature against dex's JWKS
   (`https://dex.<base-domain>/keys`) — cached, refreshed on
   `kid` miss.
3. `sub` is the user UUID ; `groups` carry tenant memberships ;
   `azp` identifies which client (vzd-api vs vzc-cli vs zot-registry).

The validation logic is shared between consumers via a small Go
package (TBD : `pkg/openweft/vzd-auth` ?) so the bug fixes apply
once.

## Why dex over the alternatives

Documented in [oidc-server-dex memory entry](../../../../../../.claude/projects/-Users-david-delavennat-Documents-VCS-GIT-localhost-cloud-boot/memory/oidc_server_dex.md).
TL;DR : CNCF, Go-native, identity-broker model fits the federate-
upstream-source approach, etcd storage backend is co-tenant with
the rest of our control plane.

## Plan source

[plan.hcl](plan.hcl)
