package weftdriverplugin

// launch.go is the host-side entry point: locate a driver plugin executable,
// start it, handshake over gRPC, and dispense the *DriverSet. The returned
// *plugin.Client owns the child process — the caller must Kill() it (or rely
// on Cleanup() at process exit; go-plugin children also self-exit when the
// host dies, so an unkilled client is bounded by host lifetime).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	hclog "github.com/hashicorp/go-hclog"
	plugin "github.com/hashicorp/go-plugin"
)

// EnvPluginDir names an extra directory to search for plugin executables.
const EnvPluginDir = "WEFT_PLUGIN_DIR"

// LaunchOptions configures a single plugin launch.
type LaunchOptions struct {
	// Executable is the plugin binary name (e.g. "weft-driver-vz") resolved
	// via SearchDirs / $WEFT_PLUGIN_DIR / the weft binary's dir / $PATH, OR an
	// absolute path used as-is.
	Executable string
	// SearchDirs are checked before $WEFT_PLUGIN_DIR and the weft binary dir.
	SearchDirs []string
	// Host context handed to the plugin via env so it can build its bundle.
	HostUUID string
	Hostname string
	AZ       string
	StateDir string
	// Logger receives go-plugin + plugin-stderr lines. nil → a Warn-level
	// stderr logger (keeps normal runs quiet).
	Logger hclog.Logger
}

// Launch starts the plugin and returns its dispensed driver set plus the
// client that controls the child process.
func Launch(opts LaunchOptions) (*DriverSet, *plugin.Client, error) {
	path, err := locate(opts.Executable, opts.SearchDirs)
	if err != nil {
		return nil, nil, err
	}

	logger := opts.Logger
	if logger == nil {
		logger = hclog.New(&hclog.LoggerOptions{
			Name:   "weft-driver",
			Output: os.Stderr,
			Level:  hclog.Warn,
		})
	}

	cmd := exec.Command(path) // no args → the plugin enters serve mode
	cmd.Env = append(os.Environ(),
		EnvHostUUID+"="+opts.HostUUID,
		EnvHostname+"="+opts.Hostname,
		EnvAZ+"="+opts.AZ,
		EnvStateDir+"="+opts.StateDir,
	)

	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          pluginMap(),
		Cmd:              cmd,
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Managed:          true, // Cleanup() kills any leaked clients at exit
		Logger:           logger,
	})

	rpcClient, err := client.Client()
	if err != nil {
		client.Kill()
		return nil, nil, fmt.Errorf("driver plugin %q handshake: %w", opts.Executable, err)
	}
	raw, err := rpcClient.Dispense(PluginName)
	if err != nil {
		client.Kill()
		return nil, nil, fmt.Errorf("driver plugin %q dispense: %w", opts.Executable, err)
	}
	set, ok := raw.(*DriverSet)
	if !ok {
		client.Kill()
		return nil, nil, fmt.Errorf("driver plugin %q dispensed unexpected type %T", opts.Executable, raw)
	}
	return set, client, nil
}

// Cleanup kills all managed plugin clients. Hosts defer this in main.
func Cleanup() { plugin.CleanupClients() }

// locate resolves a plugin executable name to a path. Absolute/relative paths
// with a separator are used verbatim; bare names are searched in SearchDirs,
// $WEFT_PLUGIN_DIR, the weft binary's directory, then $PATH.
func locate(name string, searchDirs []string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("driver plugin: empty executable name")
	}
	if filepath.IsAbs(name) || filepath.Base(name) != name {
		if isExecutable(name) {
			return name, nil
		}
		return "", fmt.Errorf("driver plugin %q is not an executable file", name)
	}

	var dirs []string
	dirs = append(dirs, searchDirs...)
	if d := os.Getenv(EnvPluginDir); d != "" {
		dirs = append(dirs, d)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if isExecutable(p) {
			return p, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("driver plugin %q not found (searched %v, $%s, weft dir, $PATH)", name, dirs, EnvPluginDir)
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	return fi.Mode().Perm()&0o111 != 0
}
