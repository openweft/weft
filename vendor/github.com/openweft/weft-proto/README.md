# vzd-proto

Protobuf/gRPC service definition and generated Go stubs for the `vzd` daemon API.

This module is the shared contract between the server ([`weft`](../weft)), the CLI client ([`vzc`](../vzc)), the graphical dashboard ([`weft-ui`](../weft-ui)), and the Terraform provider ([`terraform-provider-weft`](../terraform-provider-weft)).

## Module

```
github.com/openweft/weft-proto
```

## Service: `VzdService`

| RPC | Description |
|-----|-------------|
| `ListVMs` | List all local VMs with state and resource info |
| `VMStatus` | Get status of a specific VM |
| `StartVM` | Start a VM by name |
| `StopVM` | Stop a VM by name |
| `CreateVM` | Clone an image into a new VM |
| `DeleteVM` | Delete a VM |
| `ProvisionVM` | Clone + cloud-init inject + start + wait for IP |
| `DeprovisionVM` | Stop + delete a VM |
| `PullImage` | Pull a single image by URL |
| `PullImages` | Pull all images referenced in an HCL config directory |
| `ListImages` | List locally cached images |
| `CleanImages` | Remove cached images (dry-run supported) |
| `WaitVM` | Wait until a VM has an IP address |

## Service: `Introspect` (guest-side, package `introspectv1`)

Read-only inspection API a micro-VM serves on its `wg0` address for an operator CLI to reach over WireGuard (see [`weft-vm-agent`](../weft-vm-agent) for the server and [`weft-client/wgdial`](../weft-client/wgdial) for the client transport). Defined in `introspect.proto`, generated into the `introspectv1/` subpackage.

| RPC | Description |
|-----|-------------|
| `ListProcesses` | The VM's process table — gRPC equivalent of `ps aux`, sourced from the guest's `/proc` |

## Key types

```protobuf
message VMInfo {
  string name    = 1;
  VMState state  = 2;  // RUNNING | STOPPED | NOT_CREATED
  string os      = 3;
  uint32 cpu     = 4;
  uint64 mem_mb  = 5;
  uint64 disk_gb = 6;
  string image   = 7;
  string ip      = 8;
}
```

## Regenerate stubs

```sh
# vzd.proto → root package vzdv1 (go_package has no path component)
protoc --go_out=. --go-grpc_out=. \
  --go_opt=paths=source_relative --go-grpc_opt=paths=source_relative \
  vzd.proto

# introspect.proto → introspectv1/ subpackage (go_package carries a path,
# so use module= to land output in the right subdir)
protoc --go_out=. --go-grpc_out=. \
  --go_opt=module=github.com/openweft/weft-proto \
  --go-grpc_opt=module=github.com/openweft/weft-proto \
  introspect.proto
```

Requires `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc`.
