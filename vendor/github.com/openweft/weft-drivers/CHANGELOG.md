# Changelog

## v0.7.0

- Add the optional `BootTokenStarter` interface
  (`StartVMWithBootToken(ctx, vmUUID, token []byte) error`), the
  `BootTokenLen = 32` constant, `ValidateBootToken` and
  `ErrInvalidBootToken`. `StartVM` and `HypervisorDriver` are unchanged.
