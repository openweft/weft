# SSO recipe : Okta

## 1. Why this IdP

Okta is the default pick for SaaS, enterprise-IT-managed SSO. If
your company already centralises on Okta for VPN, Slack, GitHub,
AWS, etc., adding weft as another OIDC app is a five-minute job for
the IT team and gives you single-sign-on, lifecycle deprovisioning,
and SCIM (out of scope here) for free.

## 2. What you need from the IdP

Create an Okta **OIDC Application** (Admin → Applications →
Applications → Create App Integration) :

| Field | Value |
|---|---|
| Sign-in method | `OIDC – OpenID Connect` |
| Application type | `Web Application` |
| Grant type | `Authorization Code` (PKCE recommended) |
| Sign-in redirect URI | `https://<webui>/api/auth/callback` |
| Sign-out redirect URI | `https://<webui>/` |
| Controlled access | assign the groups you want to log in to weft |

Then collect :

- **Issuer URL** : you have two options. The **org-level** issuer
  (`https://<org>.okta.com`) requires no Authorization Server
  config but gives a token without a `groups` claim. The
  **custom Authorization Server** issuer (`https://<org>.okta.com/oauth2/<authzServerId>`,
  default `/oauth2/default`) lets you add the groups claim — use it.
- **Client ID** : on the application's General tab.
- **Client secret** : on the application's General tab, "Client
  Credentials" section.
- **Redirect URI** : `https://<webui>/api/auth/callback`.

## 3. Provider-specific gotchas

### Groups claim is opt-in

Okta does NOT put groups in the ID token by default. Add a custom
claim on your Authorization Server :

- Security → API → Authorization Servers → `default` (or your
  custom one) → Claims → Add Claim
- Name : `groups`
- Include in token type : `ID Token` AND `Access Token` (`Always`)
- Value type : `Groups`
- Filter : `Matches regex` → `^weft:.*` — this scopes the claim to
  weft-prefixed groups only, so a user's unrelated Okta group
  membership doesn't leak into the token.

If you can't use a regex (e.g. on the free developer tier), use
`Starts with` → `weft:`. The intent is the same : `Groups.startsWith("weft:")`.

### Group naming inside Okta

Create Okta groups whose names match weft's contract literally —
`weft:admin`, `weft:project:<uuid>`. Okta allows colons in group
names, despite IT-team folklore to the contrary ; if your IT policy
forbids them, fall back to a **Token Inline Hook** that rewrites
`weft-admin` → `weft:admin` server-side. See the
[Okta token hook docs](https://developer.okta.com/docs/concepts/inline-hooks/#token-hook).

### Org vs custom Authorization Server

If you accidentally point weft at `https://<org>.okta.com` (org-level
issuer), Okta will issue a token but the groups claim won't be
populated — every weft ACL check will deny because the caller has
no groups. Always use the custom AuthZ server URL :
`https://<org>.okta.com/oauth2/default` or `/oauth2/<custom-id>`.

## 4. weft side configuration

### `weft.hcl` (the agent)

```hcl
# /etc/weft/weft.hcl
oidc {
  issuer    = "https://example.okta.com/oauth2/default"
  client_id = "0oaXXXXXXXXXXXXXXXX5"
}
```

Equivalent CLI flags : `--oidc-issuer` and `--oidc-client-id` (see
`cmd/weft/main.go`). The agent verifies tokens against the issuer's
JWKS — no client secret is needed here.

### `weft-webui` (env vars)

```sh
WEBUI_OIDC_ISSUER=https://example.okta.com/oauth2/default
WEBUI_OIDC_CLIENT_ID=0oaXXXXXXXXXXXXXXXX5
WEBUI_OIDC_CLIENT_SECRET=<your-client-secret>
WEBUI_OIDC_REDIRECT_URL=https://<webui>/api/auth/callback
WEBUI_OIDC_SCOPES=openid,email,profile,groups
WEBUI_AUTH_MODE=oidc
```

Variable names from `weft-webui/internal/config/config.go`. Note
that Okta requires the `groups` scope to be REQUESTED for the
claim to be included even when the AuthZ server is configured to
emit it — keep `groups` in `WEBUI_OIDC_SCOPES`.

## 5. Verifying the integration

[`weft-webui/tools/oidc-smoke/main.go`](../../../../weft-webui/tools/oidc-smoke/main.go)
is the Dex-targeted smoke. Steps 1, 4, and 5 (`/api/auth/login`
redirect → callback → `/api/me`) work unmodified against Okta. Step
3 (POST the login form) won't : Okta uses a JS-driven login widget
that's not scrapable. Two options :

```sh
# Option A : skip step 3 ; run flow manually in a browser
open https://<webui>/api/auth/login
# log in via Okta, then verify in a separate terminal:
curl -s --cookie "<copy webui session cookie>" https://<webui>/api/me

# Option B : rewrite step 3 to drive Okta's authentication API
#   POST https://<org>.okta.com/api/v1/authn  with primaryAuth body
# then resume oidc-smoke at step 4 with the resulting sessionToken
```

Option B is fiddlier but reproducible in CI — the
[Okta authn API](https://developer.okta.com/docs/reference/api/authn/)
returns a `sessionToken` you append as `?sessionToken=...` to the
authorize URL Dex/Okta returned at step 1.

## 6. What about MFA

Weft trusts the OIDC token. MFA enforcement happens entirely inside
Okta — configure Authentication Policies (Security → Authentication
Policies) to require MFA on the weft application. The token only
reaches weft after Okta's policy has gated it ; weft honors it
transparently and there is nothing to configure on the weft side.
