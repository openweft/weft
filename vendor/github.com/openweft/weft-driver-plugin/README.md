<p align="center"><img src="https://raw.githubusercontent.com/openweft/brand/main/social/openweft.png" alt="openweft" width="720"></p>

# weft-driver-plugin

go-plugin gRPC transport between `weft agent` and the `weft-driver-*`
binaries.

## Per-boot token

`Hypervisor.StartVMWithBootToken` carries a 32-byte per-boot secret to
drivers implementing `drivers.BootTokenStarter` (otherwise it answers
`Unimplemented`). The token is validated on both sides and must never
be logged or placed in an error.
