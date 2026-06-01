# SSO recipe : Keycloak

## 1. Why this IdP

Keycloak is the default pick for self-hosted, open-source SSO in
regulated orgs : the IdP lives inside the same trust boundary as
weft, group/role data stays on-prem, and the operator owns the
upgrade cadence. It speaks OIDC + OAuth2 + SAML out of the box and
backs onto LDAP / AD when an existing directory is in play.

## 2. What you need from the IdP

Create a Keycloak **Client** (Realm → Clients → Create) with :

| Field | Value |
|---|---|
| Client type | `OpenID Connect` |
| Client ID | `weft` (this is your `client_id`) |
| Client authentication | `On` (generates a `client_secret`) |
| Valid redirect URIs | `https://<webui>/api/auth/callback` |
| Web origins | `https://<webui>` |
| Standard flow | enabled (Authorization Code + PKCE) |

Then collect :

- **Issuer URL** : `https://<keycloak>/realms/<realm>` — Keycloak's
  discovery doc lives at `<issuer>/.well-known/openid-configuration`.
- **Client ID** : `weft`
- **Client secret** : Credentials tab → `Client secret`
- **Redirect URI** : `https://<webui>/api/auth/callback` (must match
  exactly).

## 3. Provider-specific gotchas

### Groups claim

Keycloak does NOT include groups in the token by default. Add a
**Group Membership** protocol mapper :

- Client → Client scopes → `weft-dedicated` → Add mapper → By
  configured type → **Group Membership**
- Name : `groups`
- Token Claim Name : `groups`
- **Full group path : OFF** (otherwise you get `/weft:admin`, with a
  leading slash, and weft's ACL primitives compare `==`).
- Add to ID token : ON
- Add to access token : ON
- Add to userinfo : ON

### SubGroup-based RBAC

Keycloak groups support nesting (`/tenants/acme/projects/blue`). The
default Group Membership mapper with Full Path = off emits only the
leaf name, which collides if two subgroups share a leaf. If you
need `weft:project:<uuid>` derived from a subgroup, write a custom
**Script** mapper that walks `keycloakSession.userSession.user.groups`
and emits the fully-prefixed names you want. The
[Keycloak admin guide on protocol mappers](https://www.keycloak.org/docs/latest/server_admin/#_protocol_mappers)
covers the API ; weft has no opinion beyond "what arrives in the
`groups` claim is what we ACL on".

### Realm vs master realm

Never put weft clients in the `master` realm — Keycloak's admin UI
hard-codes that realm's tokens for itself, and a token-validation
quirk in older versions lets master tokens cross-validate against
other realms. Use a dedicated realm (e.g. `openweft`).

## 4. weft side configuration

### `weft.hcl` (the agent)

```hcl
# /etc/weft/weft.hcl
oidc {
  issuer    = "https://keycloak.internal.example.com/realms/openweft"
  client_id = "weft"
}
```

Equivalent CLI flags : `--oidc-issuer` and `--oidc-client-id` (see
`cmd/weft/main.go`). The agent only verifies tokens — it does NOT
talk to the token endpoint, so no client secret here.

### `weft-webui` (env vars)

```sh
WEBUI_OIDC_ISSUER=https://keycloak.internal.example.com/realms/openweft
WEBUI_OIDC_CLIENT_ID=weft
WEBUI_OIDC_CLIENT_SECRET=<your-client-secret>
WEBUI_OIDC_REDIRECT_URL=https://<webui>/api/auth/callback
WEBUI_OIDC_SCOPES=openid,email,profile,groups
WEBUI_AUTH_MODE=oidc
```

Variable names are pulled from `weft-webui/internal/config/config.go`.
`WEBUI_OIDC_SCOPES` defaults to `openid,email,profile,groups` ; keep
`groups` in the list or the claim never reaches the callback.

## 5. Verifying the integration

[`weft-webui/tools/oidc-smoke/main.go`](../../../../weft-webui/tools/oidc-smoke/main.go)
drives the full Authorization Code flow end-to-end. It was written
against Dex's mock connector, but the only Dex-specific assumption
is the login-form scraping in step 3 — Keycloak's login page uses a
different form layout, so re-target it like this :

```sh
WEBUI_BASE=https://<webui> \
DEX_ISSUER=https://keycloak.internal.example.com/realms/openweft \
OIDC_USER=<test-user>@example.com \
OIDC_PASS=<your-test-password> \
go run ./weft-webui/tools/oidc-smoke
```

Steps 1 (login redirect), 4 (callback), and 5 (`/api/me`) work as-is
against any OIDC provider. Step 3 (POST the login form) will fail
on the form-field regex — adjust the `usernameRe` / `passwordRe`
pattern in `main.go` to match Keycloak's `kc-form-login` HTML, OR
run the flow once by hand in a browser and confirm `/api/me`
returns 200 with the expected groups.

## 6. What about MFA

Weft trusts the OIDC token. MFA enforcement happens entirely inside
Keycloak (Authentication → Flows → require OTP / WebAuthn /
conditional MFA). Configure it there ; weft will honor it
transparently — a token only reaches weft after Keycloak's flow has
already gated it. There is nothing to configure on the weft side.
