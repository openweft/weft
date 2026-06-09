# wefthcl

Pure-Go HCL configuration parser for openweft. Owns the parser
end-to-end : types, HCL syntax, OCI/HTTP reference resolution, and
the row-building logic that feeds the daemon's VM inventory.

## Module

```
github.com/openweft/weft-hcl   // Go package name : wefthcl
```

## API surface

```go
// Per-environment daemon settings parsed from the top-level `weft`
// block (cache paths, SSH defaults, parallelism, timeouts).
type WeftBlock struct { ... }
func LoadWeftBlock(cfg string) WeftBlock

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
import "github.com/openweft/weft-hcl"

rows, err := wefthcl.ReadVMs("state/hcl")
```

## Consumers

- [`weft`](https://github.com/openweft/weft) — reads daemon config + VM definitions
- [`weft-webui`](https://github.com/openweft/weft-webui) — populates the VM table (Svelte SPA + Go API)
- [`terraform-provider-weft`](https://github.com/openweft/terraform-provider-weft) — reads image URLs from HCL

## License

BSD 3-Clause — see LICENSE.
