package driverplugins

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnvPluginDir mirrors weft-driver-plugin's local search dir env var.
const EnvPluginDir = "WEFT_PLUGIN_DIR"

// Resolve returns a local path to the driver plugin executable execName
// (e.g. "weft-driver-qemu"), local-first then OCI-fallback:
//
//   - a binary found in $WEFT_PLUGIN_DIR, the weft binary's dir, or $PATH wins;
//   - otherwise it is pulled from cfg.Ref(<hv>) into cacheDir and that path is
//     returned.
//
// cacheDir is the host-local plugin cache (typically <stateDir>/plugins).
func Resolve(ctx context.Context, execName, cacheDir string, cfg Config) (string, error) {
	if execName == "" {
		return "", fmt.Errorf("driverplugins: empty executable name")
	}
	if p, ok := findLocal(execName); ok {
		return p, nil
	}

	hv := strings.TrimPrefix(execName, "weft-driver-")
	ref := cfg.Ref(hv)
	path, err := pullExecutable(ctx, ref, cacheDir, execName, cfg.Token)
	if err != nil {
		return "", fmt.Errorf("driver %q not found locally and OCI pull from %s failed: %w", execName, ref, err)
	}
	return path, nil
}

// findLocal looks for a bare executable name in the same places
// weft-driver-plugin's launcher does, so a pre-installed binary always wins.
func findLocal(name string) (string, bool) {
	var dirs []string
	if d := os.Getenv(EnvPluginDir); d != "" {
		dirs = append(dirs, d)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if isExecutable(p) {
			return p, true
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	return "", false
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}
