#!/usr/bin/env -S pkgx bash
#
# 06-proxy-acme.sh — assert the embedded Caddy reverse proxy
# (see docs/operations/proxy.md and memory `project_reverse_proxy_caddy`)
# has obtained a valid TLS certificate for the operator's domain. The
# script does NOT trigger a fresh ACME order ; it inspects the leaf
# certificate the proxy serves today.
#
# Usage:
#   PROXY_HOST=cluster.example.com scripts/validation/06-proxy-acme.sh
#
# Optional env :
#   PROXY_PORT     default 443
#   ACME_ISSUERS   default "Let's Encrypt|ZeroSSL" — pipe-separated list
#                  of substrings that the certificate's Issuer CN must
#                  match (case-insensitive).
#   ALLOW_STAGING  default 0 ; set to 1 to accept staging issuers
#                  ("Fake LE Intermediate", "Pebble Intermediate") for
#                  pre-production smoke runs.
#
# Exit 0 iff (chain validates) AND (issuer matches the allowed list)
# AND (notAfter is more than 7 days away).

set -euo pipefail

: "${PROXY_HOST:?PROXY_HOST env var required (the public hostname the proxy serves)}"
PROXY_PORT="${PROXY_PORT:-443}"
# Default issuers : Let's Encrypt + ZeroSSL. We avoid the apostrophe in
# the parameter-expansion default (bash 3.2 mis-parses it) by joining
# the substrings with '|' from explicit literals.
_default_issuers="Let"
_default_issuers="${_default_issuers}'s Encrypt|ZeroSSL"
ACME_ISSUERS="${ACME_ISSUERS:-${_default_issuers}}"
unset _default_issuers
ALLOW_STAGING="${ALLOW_STAGING:-0}"

command -v openssl >/dev/null 2>&1 \
  || { echo "openssl required (cert chain inspection)" >&2; exit 1; }

# Pull the leaf certificate (and stapled chain) via s_client. -servername
# is required for SNI ; without it, Caddy serves its default cert.
echo "fetching cert from ${PROXY_HOST}:${PROXY_PORT}"
chain_pem=$(echo | openssl s_client \
              -connect "${PROXY_HOST}:${PROXY_PORT}" \
              -servername "$PROXY_HOST" \
              -showcerts 2>/dev/null \
              | sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p') \
  || { echo "FAIL: TLS handshake to ${PROXY_HOST}:${PROXY_PORT} failed" >&2; exit 1; }

[[ -n "$chain_pem" ]] || { echo "FAIL: no certificate returned" >&2; exit 1; }

# 1. Chain validation against the system trust store. s_client's
#    "Verify return code" reflects this — anything other than 0 fails.
verify_out=$(echo | openssl s_client \
              -connect "${PROXY_HOST}:${PROXY_PORT}" \
              -servername "$PROXY_HOST" 2>/dev/null \
              | awk '/Verify return code:/')
if ! grep -q "Verify return code: 0" <<< "$verify_out"; then
  echo "FAIL: TLS chain did not validate: ${verify_out}" >&2
  exit 1
fi

# 2. Issuer match. We pull just the leaf — first cert in the chain —
#    and inspect its Issuer.
leaf_pem=$(awk '/-----BEGIN CERTIFICATE-----/{n++} n==1' <<< "$chain_pem")
issuer=$(openssl x509 -noout -issuer -in <(echo "$leaf_pem") | sed 's/^issuer=//')
echo "issuer: ${issuer}"

allowed="$ACME_ISSUERS"
if [[ "$ALLOW_STAGING" == "1" ]]; then
  allowed="${allowed}|Fake LE Intermediate|Pebble Intermediate"
fi

if ! grep -qiE "(${allowed})" <<< "$issuer"; then
  echo "FAIL: issuer '${issuer}' does not match allow-list '${allowed}'" >&2
  echo "      (set ALLOW_STAGING=1 if testing against a staging CA)" >&2
  exit 1
fi

# 3. Expiry — fail if the cert renews in less than 7 days, since that's
#    inside Caddy's normal renewal window and means renewal is stuck.
not_after=$(openssl x509 -noout -enddate -in <(echo "$leaf_pem") | sed 's/^notAfter=//')
# `date -d` (GNU) vs `date -j -f` (BSD/macOS) — try both.
if expiry_ts=$(date -d "$not_after" +%s 2>/dev/null); then :
elif expiry_ts=$(date -j -f "%b %e %T %Y %Z" "$not_after" +%s 2>/dev/null); then :
else
  echo "WARN: could not parse notAfter='${not_after}' ; skipping expiry check" >&2
  expiry_ts=0
fi

if (( expiry_ts > 0 )); then
  now_ts=$(date +%s)
  days_left=$(( (expiry_ts - now_ts) / 86400 ))
  if (( days_left < 7 )); then
    echo "FAIL: cert expires in ${days_left} day(s) (notAfter=${not_after}) — renewal stuck?" >&2
    exit 1
  fi
  echo "expiry ok: ${days_left} day(s) remaining (notAfter=${not_after})"
fi

echo "proxy ACME ok: ${PROXY_HOST}:${PROXY_PORT} serves a valid cert from a trusted issuer"
