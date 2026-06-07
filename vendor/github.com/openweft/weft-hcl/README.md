# hclconfig

Pure-Go HCL configuration parser for openweft. Owns the parser
end-to-end : types, HCL syntax, OCI/HTTP reference resolution, and
the row-building logic that feeds the daemon's VM inventory.

## Module

```
github.com/openweft/hclconfig
```

## API surface

```go
// Per-environment daemon settings parsed from the top-level `mock`
// block (the keyword is historical ; the block holds cache paths,
// SSH defaults, parallelism, timeouts).
type MockBlock struct { ... }
func LoadMockBlock(cfg string) MockBlock

// VM table — one row per declared VM, used by webui + tfprovider.
type Row     struct { ... }
type VMDef   struct { ... }
type DiskDef struct { ... }

// Reads + resolves the HCL config directory.
func ReadVMs(configDir string) ([]Row, error)
func BuildRowsFromConfig(configDir, prefix string, vmState map[string]map[string]interface{}, ociMap map[string]string) ([]Row, error)
func LoadOCIFroms(cfg string) map[string]string

// Tabular rendering of the Row slice.
func RenderTableFromRows(rows []Row, w io.Writer)
func MarshalRows(rows []Row) ([]byte, error)
```

## Usage

```go
import "github.com/openweft/hclconfig"

rows, err := hclconfig.ReadVMs(".mock/hcl")
```

## Consumers

- [`weft`](https://github.com/openweft/weft) — reads daemon config + VM definitions
- [`weft-webui`](https://github.com/openweft/weft-webui) — populates the VM table (Svelte SPA + Go API)
- [`terraform-provider-weft`](https://github.com/openweft/terraform-provider-weft) — reads image URLs from HCL

## Naming note

The HCL daemon block keyword (`mock "<label>" { … }`), the `MockBlock`
Go type, and the default config directory (`.mock/hcl`) are
historical — early openweft was scaffolded as a "mock UI". The
naming is slated for cleanup in a future cycle (the rename is a
breaking change for live config files, so it ships behind a deliberate
major-version bump rather than a patch).

## License

BSD 3-Clause — see LICENSE.
