# openweft coding conventions

This document is the canonical reference for cross-repo coding conventions
in the openweft project. Every rule below is sourced from a project
memory note and is enforced either by `scripts/lint/check-conventions.sh`
(run in CI by `.github/workflows/conventions.yml` and locally via
pre-commit), by `golangci-lint`, or by manual review.

If you change a convention, update **both** the memory note and this
document, and adjust the lint script as needed.

---

## R1 -- cobra-only CLIs

**Memory:** `feedback_cli_cobra`

All openweft CLIs (`weft`, `weft-microvm-agent`, `weft-driver-*`, every
mini-binary) use [`spf13/cobra`](https://github.com/spf13/cobra) — never
the standard-library `flag` package. This holds even for tiny
single-command binaries: porting `flag.*` over to a one-command cobra
root is part of the work.

Bad (`cmd/weft-foo/main.go`):

```go
package main

import (
    "flag"
    "fmt"
)

func main() {
    name := flag.String("name", "", "thing name")
    flag.Parse()
    fmt.Println(*name)
}
```

Good:

```go
package main

import (
    "fmt"

    "github.com/spf13/cobra"
)

func main() {
    var name string
    root := &cobra.Command{
        Use: "weft-foo",
        RunE: func(*cobra.Command, []string) error {
            fmt.Println(name)
            return nil
        },
    }
    root.Flags().StringVar(&name, "name", "", "thing name")
    _ = root.Execute()
}
```

**Enforced by:** `check-conventions.sh` (R1) — any non-test `*.go` under
`cmd/` that imports `"flag"` and not `spf13/cobra` fails. Also covered
by `golangci-lint`'s `forbidigo` rule in `.pre-commit-config.yaml`.

---

## R2 -- shell shebangs use `pkgx bash`

**Memory:** `feedback_pkgx_bash`

All openweft shell scripts run under pkgx-managed bash 5.x so they get
modern features (associative arrays, `${var,,}`, etc.) regardless of the
host. Apple's `/bin/bash` is 3.2 and breaks these scripts silently.

The canonical shebang is:

```sh
#!/usr/bin/env -S pkgx bash
```

Exception: scripts under `image/` are in-VM init scripts (the VM has a
real bash; no pkgx). The check skips that directory. Other legitimate
exceptions (cloud-init `runcmd`, embedded init shipped without pkgx)
trigger a **warning** rather than a hard failure — review case by case.

Bad:

```sh
#!/bin/bash
declare -A m=()              # silently empty on macOS bash 3.2
m[k]=v
```

Good:

```sh
#!/usr/bin/env -S pkgx bash
declare -A m=()
m[k]=v
```

**Enforced by:** `check-conventions.sh` (R2). Warn-only; review surfaces
in CI logs.

---

## R3 -- no auto-publish on push to `main`

**Memory:** `feedback_no_autopublish_dev`

GitHub Actions workflows that **publish artifacts** (container images,
OCI artifacts, GitHub releases, package registries) must be gated on a
deliberate trigger — `workflow_dispatch` and/or a tag-push filter — and
must **not** publish on every commit to `main`. Otherwise a routine
merge can ship a release.

Bad (`.github/workflows/release.yml`):

```yaml
name: release
on:
  push:
    branches: [main]   # every merge cuts a release -- forbidden
```

Good:

```yaml
name: release
on:
  workflow_dispatch: {}
  push:
    tags: ['v*']       # only version tags publish
```

The heuristic for "publishing workflow" is filename match against
`release|image|publish|oci`. If you add a publishing workflow under a
different name, add it to the heuristic in `check-conventions.sh`.

**Enforced by:** `check-conventions.sh` (R3) — fails when a publishing
workflow's `push:` block names `main` without a `tags:` filter.

---

## R4 -- terraform-provider-weft uses plugin-framework only

**Memory:** `project_tfprovider_framework`

`terraform-provider-weft` (and any other openweft Terraform provider)
uses HashiCorp's [terraform-plugin-framework](https://github.com/hashicorp/terraform-plugin-framework)
**exclusively**. The legacy `terraform-plugin-sdk/v2` must not appear in
`go.mod` or in any import statement — even temporarily, even as a
"helper" import.

Bad:

```go
import "github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
```

Good:

```go
import (
    "github.com/hashicorp/terraform-plugin-framework/resource"
    "github.com/hashicorp/terraform-plugin-framework/resource/schema"
)
```

**Enforced by:** `check-conventions.sh` (R4) — any `*.go` under a path
matching `terraform-provider-weft|tfprovider` that imports
`hashicorp/terraform-plugin-sdk/v2` fails.

---

## How to run the checks

Locally, opt in once with pre-commit:

```sh
pkgx pre-commit install
```

Or run the script directly:

```sh
bash scripts/lint/check-conventions.sh        # exits 0 / 1
bash scripts/lint/check-conventions.sh -v     # verbose: list every file
```

In CI, `.github/workflows/conventions.yml` runs the same script on every
push and pull request against `main`. A red build means a convention
violation — not a flaky test.
