//go:build darwin

package main

import "github.com/spf13/cobra"

// registerHostCommands is now a no-op on macOS too: the host-local datapath
// subcommands (vz-vm-run, provision) moved into the weft-driver-vz plugin
// executable when the cgo datapath was externalised. weft launches that
// plugin over go-plugin; it no longer hosts the VZ commands itself.
func registerHostCommands(*cobra.Command) {}
