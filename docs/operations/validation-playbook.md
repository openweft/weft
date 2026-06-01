# Post-deploy validation playbook

> **TL;DR** — after `weft up` returns, run
> `scripts/validation/run-all.sh <host1> <host2> <host3>` on the operator
> laptop with the env vars listed below. Eight gates run end-to-end against
> the live cluster. PASS means the install is functionally green ; FAIL
> tells you exactly which subsystem to look at first.

## What this is for

`weft up` converges a 1-host or 3-DC cluster from `cluster.hcl`. When it
returns, the **planner** thinks the cluster is up — every host has
weft-agent running, etcd has quorum, the proxy is listening, the OIDC
issuer is reachable. This playbook is the **bare-metal counterpart** that
proves the planner is right: every script in `scripts/validation/`
exercises a real cluster API and asserts the observable shape, not just
the planner's internal "I think I'm done" flag.

Run it **once after every `weft up`** and **once per upgrade** (link to
[upgrade.md](upgrade.md) — the post-rollback checklist in section 8
also calls this playbook out by name).

## Why it's separate from `tests/integration/3host/`

`tests/integration/3host/` is a **compile-only Go harness** : it drives
the SDK against a mock weft-agent (`tests/integration/3host/harness_test.go`).
It catches API-shape regressions in CI ; it cannot catch real-world failures
(disk full, ACME rate-limited, OIDC mapper misconfigured, etcd flapping
between two leaders) because none of those exist in the mock.

The validation playbook is the inverse : it runs against **real hosts**,
talks to **real etcd** and **real OIDC**, and **cannot run in CI** because
CI doesn't have a 3-DC cluster sitting around. Both layers exist for a
reason ; keep them distinct.

| Layer | Lives in | Runs on | Catches |
|---|---|---|---|
| SDK contract | `tests/integration/3host/` | CI | API/wire-shape regressions |
| Functional | `scripts/validation/` | operator laptop | install / upgrade / config bugs |
| Performance | `scripts/perf/` | operator (planned) | scaling regressions |
| Security pentest | 3rd party | external | adversarial scenarios |

## Prerequisites on the operator's machine

All scripts are pkgx-bash (`#!/usr/bin/env -S pkgx bash`, per memory
`feedback_pkgx_bash`). On a fresh laptop:

- `pkgx` — package runner (`curl -fsS https://pkgx.sh | sh`)
- `curl`, `openssl`, `ssh`, `awk`, `grep` — system-supplied on macOS/Linux
- `jq` — `pkgx jq` works ; the scripts degrade where possible if absent
- `etcdctl` — `pkgx etcdctl` works ; the scripts auto-detect this
- `weft` — the same binary the cluster runs, on your `$PATH`

No esoteric deps. No Python. No Go toolchain on the operator's side.

## Running it

```sh
# Minimal run — 1-host cluster, no OIDC, no proxy:
HOSTS=10.0.0.1 \
ETCD_ENDPOINTS=10.0.0.1:2379 \
WEFT_AGENT=10.0.0.1 \
  scripts/validation/run-all.sh 10.0.0.1

# Full run — 3-DC cluster with OIDC + ACME + audit:
HOSTS=10.0.0.1,10.0.0.2,10.0.0.3 \
ETCD_ENDPOINTS=10.0.0.1:2379,10.0.0.2:2379,10.0.0.3:2379 \
OIDC_ISSUER=https://dex.example.com \
OIDC_CLIENT_ID=weft-validation \
OIDC_CLIENT_SECRET=$(cat ./validation-client-secret) \
PROXY_HOST=cluster.example.com \
WEFT_AGENT=10.0.0.1 \
NON_ADMIN_TOKEN=$(cat ./project-only-token.jwt) \
  scripts/validation/run-all.sh
```

`run-all.sh` does NOT short-circuit on first failure ; every script runs,
so the final summary tells you whether the failure is isolated (one
script red) or systemic (most scripts red).

## What each script proves

### 01-hosts-running.sh

Iterates `HOSTS`, does ICMP, `systemctl is-active weft-agent.service`
over SSH, and `GET /metrics`. **Expected output**: `[host=X] ok` per
host, then `all N host(s) green`. **Debug**: failures print
`[host=X check=Y] reason` — go look at `journalctl -u weft-agent` on
the host named in the message.

### 02-etcd-quorum.sh

Calls `etcdctl endpoint health --cluster` then `endpoint status --cluster`,
asserts every member is healthy AND exactly one is leader. **Expected
output**: `etcd quorum healthy: N endpoint(s), 1 leader`. **Debug**: if
the leader count is 0, an election is in progress (wait 30s, re-run) ;
if it's 2, you have a split-brain — check disk latency
(`scripts/perf/etcd-write-rate.sh` will surface it).

### 03-oidc-login.sh

Fetches the OIDC discovery doc, runs `client_credentials`, base64-decodes
the JWT payload, asserts the `groups` claim is present, is an array, and
contains at least one entry matching `EXPECT_GROUP` (default `weft:`).
**Expected output**: `OIDC ok: token from <issuer>, groups claim valid`.
**Debug**: per-failure, the script names the failed sub-check
(discovery, token endpoint, claim absent, claim wrong shape). For
provider-specific fixes see `docs/operations/sso/keycloak.md` and
`docs/operations/sso/auth0.md` — the most common cause is a mapper
that injects `groups` only on the ID token but not the access token.

### 04-vm-roundtrip.sh

Boots a canary VM (`weft instance start`), waits for state=Running,
stops, and removes it. Defaults to a small Debian cloud image ;
override `IMAGE` for air-gapped deploys. **Expected output**: four
`[N/4]` step lines then `VM roundtrip ok`. **Debug**: if the canary
never reaches Running, `KEEP_ON_FAILURE=1` (default) leaves it in place
so you can run `weft instance status --name <canary>` and
`weft instance logs --name <canary>` for triage.

### 05-snapshot-roundtrip.sh

Creates a 1 GiB volume, snapshots it, restores the snapshot into a new
volume, then tears all three down. Exercises the reflink/FICLONE path
documented in memory `project_cow_clone`. **Expected output**: five
`[N/5]` step lines then `snapshot roundtrip ok`. **Debug**: on btrfs
or XFS-reflink, this should be sub-second ; if it takes more than 10s,
the storage layer is falling back to a full copy — check `dmesg` for
`EOPNOTSUPP` from FICLONE and confirm the volume root is on a CoW
filesystem.

### 06-proxy-acme.sh

`openssl s_client` against the proxy hostname, asserts the chain
validates, the issuer is in the allow-list (default Let's Encrypt or
ZeroSSL), and the cert has >7 days left. **Expected output**:
`proxy ACME ok`. **Debug**: if the issuer is `Caddy Local Authority`
or similar, ACME has not yet succeeded — check the Caddy admin API
on the proxy host for the issuer state (`docs/operations/proxy.md`).
For staging, set `ALLOW_STAGING=1`.

### 07-metrics-shape.sh

Scrapes `/metrics` and greps for the 8 metric families the Grafana
dashboard (`docs/operations/grafana/README.md` "Panel references")
binds to : the gRPC server family + the Go runtime / process family.
A miss = dashboard blanks. **Expected output**: per-host `ok (N
families present)` then `metrics shape ok on all N host(s)`. **Debug**:
a missing `grpc_server_*` family means the metrics interceptor isn't
wired — check `cmd/weft/main.go` for the metrics-listen flag and
that the agent was started with it.

### 08-audit-log-write.sh

Sends a non-admin `host register` (which RBAC denies per
`docs/operations/rbac.md`), then SSHes to each host and greps
`/var/log/weft/audit.jsonl` for a `"decision":"deny"` line. Confirms
**both** that RBAC is enforcing AND that the audit sink is wired.
**Expected output**: `audit log ok: deny was provoked and at least
one host recorded it`. **Debug**: if the non-admin call SUCCEEDS, the
agent is running in `--oidc-issuer=""` dev mode (everything passes)
— check the HCL config. If it's denied but no audit line appears,
the audit sink path is not configured ; see `rbac.md` "Audit log".

## When to run

- After every `weft up` on a fresh cluster.
- After every host added with `weft host register`.
- After every upgrade — `docs/operations/upgrade.md` step 8 calls this
  playbook out by name.
- Optionally on a cron job, once per day, as a synthetic monitor.

## What's NOT in scope

- **Load / scaling tests** — see `scripts/perf/` (`bring-up-N.sh`,
  `etcd-write-rate.sh`, `mesh-fanout.sh`). Those answer "how fast" ;
  this playbook answers "does it work at all".
- **Security pentest** — phishing, lateral movement, vuln scans. That's
  a 3rd-party concern ; we hand the security firm a converged cluster
  + the output of this playbook and they take it from there.
- **Data-plane functional tests of guest workloads** — the canary VM in
  `04-vm-roundtrip.sh` proves the lifecycle works ; it does NOT prove
  your particular guest image boots correctly. That's a tenant
  concern, not a platform concern.

## References

- `tests/integration/3host/README.md` — the CI counterpart this
  playbook complements.
- `docs/operations/upgrade.md` — calls this playbook from the
  post-roll checklist.
- `docs/operations/rbac.md` — the contract `03-oidc-login.sh` and
  `08-audit-log-write.sh` test against.
- `docs/operations/grafana/README.md` — the metric family list
  `07-metrics-shape.sh` enforces.
- Memory `feedback_pkgx_bash` — why every script in this dir is
  pkgx-bash, not Apple's bash 3.2.
