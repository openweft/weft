# hclconfig

Public façade over the internal HCL configuration loader. Re-exports types and functions from `internal/mock` so that external packages (`weft`, `terraform-provider-weft`, …) can import them without violating Go's internal-package visibility rules.

## Module

```
github.com/configuration-management-tool/mock/pkg/hclconfig
```

Part of the root module — no separate `go.mod`.

## API

```go
// MockBlock holds daemon-level configuration (cache paths, SSH settings, timeouts …).
type MockBlock = mock.MockBlock

// Row holds per-VM display data (name, state, OS, CPU, memory, …).
type Row = mock.Row

// LoadMockBlock parses the HCL config directory and returns the daemon-level
// configuration block.
func LoadMockBlock(cfg string) MockBlock

// BuildRowsFromConfig computes the table of VM rows from the HCL config
// directory, a deployment prefix, a live VM state map, and an OCI image-URL map.
func BuildRowsFromConfig(configPath, prefix string, vmMap map[string]map[string]interface{}, ociMap map[string]string) ([]Row, error)

// LoadOCIFroms returns a map of VM-name → OCI image URL from the HCL config.
func LoadOCIFroms(cfg string) map[string]string
```

## Usage

```go
import "github.com/configuration-management-tool/mock/pkg/hclconfig"

block := hclconfig.LoadMockBlock(".mock/hcl")
rows, err := hclconfig.BuildRowsFromConfig(".mock/hcl", "dev", vmStateMap, ociMap)
```

## Used by

- [`weft`](../weft) — reads daemon config and VM definitions
- [`weft-webui`](../weft-webui) — populates the VM table (Svelte SPA + Go API)
- [`terraform-provider-weft`](../terraform-provider-weft) — reads image URLs from HCL
