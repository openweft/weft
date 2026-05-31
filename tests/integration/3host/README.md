# 3-host integration harness

End-to-end acceptance tests for `weft up` / `weft down` against a real
3-host cluster.

## What it covers

| Test                       | Asserts                                                    |
| -------------------------- | ---------------------------------------------------------- |
| `TestAccClusterBringUp`    | `weft up --apply` converges; agents listen on every host. |
| `TestAccEtcdQuorum`        | Embedded-etcd reports 3 members from each peer's vantage. |
| `TestAccCreateVMRoundtrip` | A `CreateVM` gRPC round-trip works against every host.    |
| `TestAccClusterDown`       | `weft down --apply` un-provisions every host.              |

Every test starts with `if os.Getenv("WEFT_INTEGRATION_HOSTS_PREFIX") == "" { t.Skip(...) }`,
so compiling the package on a machine without the cluster is free:

```sh
go test -tags integration -run NeverMatches ./tests/integration/3host/...
```

The default `go test ./...` never sees this package (build-tagged
`integration`).

## Pre-requisites

The harness assumes the three Tart Debian VMs are already booted and
reachable from the box driving the tests — it does not provision them.

1. **Three Tart VMs up at known IPs.** Typical local-dev layout:
   `<PREFIX>.11`, `<PREFIX>.12`, `<PREFIX>.13`. Default Tart subnet on
   macOS is `192.168.64.0/24`. The fixture (`cluster.hcl`) hard-codes
   the last octets at `11/12/13` and pins `hypervisor = "qemu"` on each
   host (Apple-VZ cannot nest, QEMU/TCG can — see the project's
   `env_no_nested_virt` memory).

2. **`ssh-copy-id` deployed.** `weft up` drives every host over SSH as
   user `admin` (per the `host { ssh { user = "admin" } }` blocks).
   Confirm `ssh admin@<PREFIX>.11 true` succeeds without a password
   prompt before running the harness; otherwise the `Apply` step will
   hang on key-auth.

3. **`WEFT_INTEGRATION_HOSTS_PREFIX` exported.** Setting the env var
   both unlocks the tests (`requireEnv` no longer skips) and tells the
   harness which prefix to use when constructing the per-host
   addresses:

   ```sh
   export WEFT_INTEGRATION_HOSTS_PREFIX=192.168.64
   ```

   If your Tart subnet is something else (custom `tart` network, or a
   non-default Apple-VZ allocator), point this at the right `/24`.

## Running

From the repo root:

```sh
task integration:3host
```

That's a thin wrapper around:

```sh
WEFT_INTEGRATION_HOSTS_PREFIX=192.168.64 \
go test -tags integration -timeout 30m ./tests/integration/3host/...
```

The 30-minute timeout covers the full bring-up → quorum-wait → create-VM →
tear-down loop on a cold cluster. Set `WEFT_BIN=/abs/path/to/weft` to use
an ad-hoc dev build instead of the `weft` on `$PATH`.

## Why these four scenarios

They map 1:1 to the four operator-visible cluster transitions:
provision (`up`), reach quorum, accept workload (`CreateVM`), and
decommission (`down`). If any one fails, the production bring-up story
is broken — these are the smoke tests gating release candidates.
