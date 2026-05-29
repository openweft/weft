// Package hclconfig parses the openweft HCL configuration directory and
// exposes the daemon-level settings block plus the table of VM definitions.
//
// It owns the parser end-to-end: types, HCL parsing, OCI/HTTP resolution,
// and the row-building logic consumed by weft, weft-ui, and
// terraform-provider-weft.
package hclconfig

// ReadVMs is a convenience wrapper that parses the HCL config directory and
// returns the resolved VM rows with no deployment prefix and no live
// VM-state or OCI overrides. It is the entry point used by external tools
// (e.g. terraform-provider-weft) that only need to enumerate VMs.
func ReadVMs(configDir string) ([]Row, error) {
	ociMap := LoadOCIFroms(configDir)
	return BuildRowsFromConfig(configDir, "", map[string]map[string]interface{}{}, ociMap)
}
