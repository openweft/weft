#!/usr/bin/env -S pkgx bash
#
# 03-oidc-login.sh — assert the OIDC issuer is reachable, accepts our
# client_credentials grant, and returns a token whose `groups` claim is
# shaped the way RBAC expects (see docs/operations/rbac.md, "Group
# naming convention").
#
# Usage:
#   OIDC_ISSUER=https://dex.example.com \
#   OIDC_CLIENT_ID=weft-validation \
#   OIDC_CLIENT_SECRET=$(cat ./secret) \
#     scripts/validation/03-oidc-login.sh
#
# Optional env :
#   OIDC_SCOPES   default "openid groups email"
#   EXPECT_GROUP  default "weft:" — a substring that must appear in at
#                 least one returned group. Set to "weft:admin" if your
#                 client_credentials principal is the cluster admin
#                 service-account.
#
# Failure modes are explicit: missing discovery doc, token endpoint 4xx,
# claim absent, claim shape wrong (string instead of array, no `weft:`
# prefix). Each prints a one-liner that names the failed check.

set -euo pipefail

: "${OIDC_ISSUER:?OIDC_ISSUER env var required}"
: "${OIDC_CLIENT_ID:?OIDC_CLIENT_ID env var required}"
: "${OIDC_CLIENT_SECRET:?OIDC_CLIENT_SECRET env var required}"
OIDC_SCOPES="${OIDC_SCOPES:-openid groups email}"
EXPECT_GROUP="${EXPECT_GROUP:-weft:}"

command -v jq >/dev/null 2>&1 \
  || { echo "jq required (token claim decoding) ; install jq" >&2; exit 1; }

issuer="${OIDC_ISSUER%/}"

# 1. Discovery doc — every OIDC provider serves this. If it 4xxs, the
#    issuer URL is wrong or the provider is down.
disco=$(curl -sS --fail --max-time 10 \
          "${issuer}/.well-known/openid-configuration") \
  || { echo "discovery doc unreachable at ${issuer}/.well-known/openid-configuration" >&2; exit 1; }

token_ep=$(jq -er '.token_endpoint' <<< "$disco") \
  || { echo "discovery doc missing token_endpoint" >&2; exit 1; }

# 2. Client-credentials grant. Tested provider: Dex (the openweft
#    default per docs/operations/sso). Keycloak + Auth0 use the same
#    endpoint shape per their SSO recipes.
resp=$(curl -sS --fail --max-time 10 \
         -u "${OIDC_CLIENT_ID}:${OIDC_CLIENT_SECRET}" \
         -d "grant_type=client_credentials" \
         -d "scope=${OIDC_SCOPES}" \
         "$token_ep") \
  || { echo "token endpoint rejected client_credentials grant" >&2; exit 1; }

access_token=$(jq -er '.access_token' <<< "$resp") \
  || { echo "token response missing access_token (got: $resp)" >&2; exit 1; }

# 3. Decode the JWT payload (no signature check ; the agent does that
#    on the wire). Three segments, base64url-encoded ; the middle is
#    the claims JSON.
payload_b64=$(awk -F. '{print $2}' <<< "$access_token")
[[ -n "$payload_b64" ]] || { echo "access_token not JWT-shaped" >&2; exit 1; }

# Pad to a multiple of 4 chars, then base64-decode (URL alphabet).
pad=$(( (4 - ${#payload_b64} % 4) % 4 ))
payload_b64_padded="${payload_b64}$(printf '%*s' "$pad" '' | tr ' ' '=')"
claims=$(printf '%s' "$payload_b64_padded" | tr '_-' '/+' | base64 -d 2>/dev/null) \
  || { echo "failed to base64-decode JWT payload" >&2; exit 1; }

# 4. Shape assertions on the `groups` claim:
#    a. present
#    b. an array
#    c. at least one element matches EXPECT_GROUP
shape=$(jq -r 'if has("groups") | not then "absent"
               elif (.groups | type) != "array" then "not-array"
               else "array" end' <<< "$claims")

case "$shape" in
  absent)
    echo "FAIL: token has no \`groups\` claim. RBAC will deny every request." >&2
    echo "      check the provider mapper that injects \`groups\` (see docs/operations/sso/)." >&2
    exit 1 ;;
  not-array)
    echo "FAIL: \`groups\` claim is not an array. RBAC expects []string." >&2
    echo "      claim shape:" >&2
    jq '.groups' <<< "$claims" >&2
    exit 1 ;;
  array) ;;
esac

if ! jq -e --arg sub "$EXPECT_GROUP" '[.groups[] | select(contains($sub))] | length > 0' \
       <<< "$claims" >/dev/null; then
  echo "FAIL: no \`groups\` entry contains '${EXPECT_GROUP}'." >&2
  echo "      got:" >&2
  jq '.groups' <<< "$claims" >&2
  exit 1
fi

echo "OIDC ok: token from ${issuer}, groups claim valid, contains '${EXPECT_GROUP}'"
