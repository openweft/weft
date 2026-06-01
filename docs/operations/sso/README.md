# SSO integration recipes

Weft's authentication layer is a stock OIDC bearer-token validator
(`coreos/go-oidc`) ; any provider that speaks Authorization Code +
OIDC discovery + a `groups` claim works. Dex is what we wire in CI
and what the [`oidc-smoke`](../../../../weft-webui/tools/oidc-smoke/main.go)
operator drill targets, but the validator is provider-agnostic.

**Before picking an IdP** read [`rbac.md`](../rbac.md) — it lays out
the group-naming contract every recipe below relies on
(`weft:admin`, `weft:project:<uuid>`, `weft:tenant:<uuid>` reserved
for the future). Your IdP MUST be able to project group memberships
into a token claim using exactly that prefix ; without it, weft
falls through to anonymous and every ACL check denies.

## Pick your IdP

| Recipe | When to use it |
|---|---|
| [Keycloak](keycloak.md) | Self-hosted, open-source, common in regulated orgs and on-prem clouds where the IdP must live inside the trust boundary. |
| [Okta](okta.md) | SaaS, enterprise-IT-managed, common in mid-to-large companies that already centralise on Okta for everything else. |
| [Auth0](auth0.md) | SaaS, B2C-friendly, common in startups and product orgs that don't need an LDAP backbone. |

All three recipes follow the same six-section template:

1. Why this IdP
2. What you need from the IdP
3. Provider-specific gotchas
4. weft side configuration (`weft.hcl` + `weft-webui` env)
5. Verifying the integration (retargeting `oidc-smoke`)
6. What about MFA — short answer : weft trusts the token, MFA is the
   IdP's job

## What weft validates, regardless of IdP

- `iss` matches the configured `WEBUI_OIDC_ISSUER` / `oidc { issuer
  = ... }` exactly.
- `aud` matches the configured client ID (unless
  `skip_client_id_check = true`).
- `exp` is in the future (auto by `coreos/go-oidc`).
- Signature against the issuer's JWKS (cached + auto-rotated).
- `groups` claim is decoded into `[]string` ; ACL primitives in
  [`acl.go`](../../../acl.go) walk that slice. Missing claim ≠ error,
  just an empty group set → every project-scope check denies.

If your IdP can produce a token that passes those four checks AND
carries the right `groups`, you're done.
