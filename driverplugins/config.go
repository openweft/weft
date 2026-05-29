package driverplugins

import (
	"os"
	"strings"
)

// Default OCI source for driver plugins. "les registre des repo github" → GHCR
// under the openweft org. Version defaults to "latest"; deployments pin it.
const (
	DefaultRegistry = "ghcr.io/openweft"
	DefaultVersion  = "latest"

	// Env overrides (the file-with-env-override surface). Per-hv refs use
	// WEFT_DRIVER_<HV>_REF, e.g. WEFT_DRIVER_VZ_REF / WEFT_DRIVER_QEMU_REF.
	EnvRegistry = "WEFT_DRIVER_REGISTRY"
	EnvVersion  = "WEFT_DRIVER_VERSION"
	EnvToken    = "WEFT_DRIVER_REGISTRY_TOKEN"
)

// Config is the driver-plugin OCI source. Build it from Default()+ApplyEnv (or
// FromEnv), or from the cluster.hcl drivers block, which weft up turns into the
// same env vars on each agent.
type Config struct {
	// Registry is the base for derived refs, e.g. "ghcr.io/openweft".
	Registry string
	// Version is the tag for derived refs, e.g. "v0.3.1" or "latest".
	Version string
	// Refs overrides the full OCI ref for a hypervisor key ("vz" / "qemu"),
	// bypassing Registry+Version. Empty/missing → derived ref.
	Refs map[string]string
	// Token is an optional bearer/password for private registries. Empty →
	// anonymous pull (public GHCR images).
	Token string
}

// Default returns the built-in config (public GHCR, latest).
func Default() Config {
	return Config{Registry: DefaultRegistry, Version: DefaultVersion, Refs: map[string]string{}}
}

// FromEnv returns Default() overlaid with WEFT_DRIVER_* env overrides.
func FromEnv() Config {
	c := Default()
	c.ApplyEnv()
	return c
}

// ApplyEnv overlays WEFT_DRIVER_* environment variables onto c. Empty vars
// leave the existing value untouched, so this composes after a file load.
func (c *Config) ApplyEnv() {
	if c.Refs == nil {
		c.Refs = map[string]string{}
	}
	if v := os.Getenv(EnvRegistry); v != "" {
		c.Registry = v
	}
	if v := os.Getenv(EnvVersion); v != "" {
		c.Version = v
	}
	if v := os.Getenv(EnvToken); v != "" {
		c.Token = v
	}
	for _, hv := range []string{"vz", "qemu"} {
		if v := os.Getenv("WEFT_DRIVER_" + strings.ToUpper(hv) + "_REF"); v != "" {
			c.Refs[hv] = v
		}
	}
}

// Ref returns the OCI reference for hypervisor key hv ("vz" / "qemu"): an
// explicit per-hv override if set, else <Registry>/weft-driver-<hv>:<Version>.
func (c Config) Ref(hv string) string {
	if r := c.Refs[hv]; r != "" {
		return r
	}
	reg := c.Registry
	if reg == "" {
		reg = DefaultRegistry
	}
	ver := c.Version
	if ver == "" {
		ver = DefaultVersion
	}
	return reg + "/weft-driver-" + hv + ":" + ver
}
