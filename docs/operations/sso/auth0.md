# SSO recipe : Auth0

## 1. Why this IdP

Auth0 (now part of Okta but operationally distinct) is the default
pick for startups and product orgs that want SaaS SSO without an
LDAP backbone, and for B2C scenarios where the IdP also fronts
social logins. The free tier is generous enough for early-stage
weft clusters ; the Action / Rule system is the easiest way in the
industry to inject custom token claims.

## 2. What you need from the IdP

Create an Auth0 **Application** (Dashboard → Applications →
Applications → Create Application) :

| Field | Value |
|---|---|
| Name | `weft` |
| Application Type | `Regular Web Application` |
| Allowed Callback URLs | `https://<webui>/api/auth/callback` |
| Allowed Logout URLs | `https://<webui>/` |
| Allowed Web Origins | `https://<webui>` |
| Token Endpoint Authentication Method | `Post` |

Then collect :

- **Issuer URL** : `https://<your-tenant>.<region>.auth0.com/` —
  trailing slash matters (Auth0's discovery doc includes it ;
  `coreos/go-oidc` strict-matches `iss`).
- **Client ID** : application Settings tab.
- **Client secret** : application Settings tab.
- **Redirect URI** : `https://<webui>/api/auth/callback`.

## 3. Provider-specific gotchas

### Groups don't exist natively

This is the big one. Auth0 has no first-class "group" object on its
core platform — there's an `Organizations` concept and a `Roles`
concept, but neither shows up as a `groups` claim by default. You
have two ways to fix it :

**(a) Action-based (recommended)** : Auth0 → Actions → Library →
Create Action → Login flow. Inject groups from
`user.app_metadata.groups` (populated via the Management API or
SCIM at provisioning time) :

```js
exports.onExecutePostLogin = async (event, api) => {
  const groups = (event.user.app_metadata && event.user.app_metadata.groups) || [];
  api.idToken.setCustomClaim("groups", groups);
  api.accessToken.setCustomClaim("groups", groups);
};
```

Attach it to the Login flow. Seed `app_metadata.groups` with
`["weft:admin"]`, `["weft:project:abc"]`, etc.

**(b) External-API-based** : same shape, but fetch from your own
group service (LDAP gateway, HR system) — skeleton in the
[Auth0 Actions docs](https://auth0.com/docs/customize/actions).
Cache aggressively ; Actions run on every login.

### Refresh tokens may drop the id_token

Some Auth0 configurations omit `id_token` from refresh responses.
`weft-webui/internal/auth/oidc.go` keeps the previously stored
claims on refresh (see `RefreshSession`), so this is benign, but
group changes won't propagate until a full re-login. Toggle "OIDC
Conformant" ON in Advanced Settings for cleanest behaviour.

### Tenant region in the issuer URL

Auth0 issuer URLs include the region for tenants created after 2020
(`https://<tenant>.us.auth0.com/`). Copy the issuer from the app's
Advanced Settings → Endpoints → "OpenID Configuration" (strip the
`.well-known/...` suffix). Trailing slash matters.

## 4. weft side configuration

### `weft.hcl` (the agent)

```hcl
# /etc/weft/weft.hcl
oidc {
  issuer    = "https://your-tenant.us.auth0.com/"
  client_id = "XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
}
```

Equivalent CLI flags : `--oidc-issuer` and `--oidc-client-id` (see
`cmd/weft/main.go`). Token verification only ; no client secret
here.

### `weft-webui` (env vars)

```sh
WEBUI_OIDC_ISSUER=https://your-tenant.us.auth0.com/
WEBUI_OIDC_CLIENT_ID=XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX
WEBUI_OIDC_CLIENT_SECRET=<your-client-secret>
WEBUI_OIDC_REDIRECT_URL=https://<webui>/api/auth/callback
WEBUI_OIDC_SCOPES=openid,email,profile,groups
WEBUI_AUTH_MODE=oidc
```

Variable names from `weft-webui/internal/config/config.go`. The
`groups` scope is requested by default ; Auth0 ignores it as a
scope (it's not a registered Auth0 scope) but the Action above
will populate the claim regardless.

## 5. Verifying the integration

[`weft-webui/tools/oidc-smoke/main.go`](../../../../weft-webui/tools/oidc-smoke/main.go)
drives the canonical OIDC flow against Dex. For Auth0, steps 1 and
4-5 (`/api/auth/login` redirect, callback, `/api/me`) work as-is ;
step 3 (POSTing the login form) does not — Auth0's Universal Login
is a JS-rendered page.

Run the flow in a browser the first time, then verify `/api/me` :

```sh
open https://<webui>/api/auth/login
# complete Auth0 login, copy session cookie from devtools, then :
curl -s --cookie "<cookie>" https://<webui>/api/me | jq .groups
```

For CI : enable Auth0's Resource Owner Password Grant on the app,
fetch a token directly from `/oauth/token`, and feed it to the
agent as a bearer header — see the
[Auth0 ROP grant docs](https://auth0.com/docs/api/authentication#resource-owner-password).
Suboptimal for production, fine for a smoke gate.

## 6. What about MFA

Weft trusts the OIDC token. MFA enforcement happens entirely inside
Auth0 — configure under Security → Multi-factor Auth (TOTP, push,
WebAuthn, SMS, …) and set the policy to require it on weft logins.
The token only reaches weft after Auth0's flow has gated it ; weft
honors it transparently with nothing to configure on the weft side.
