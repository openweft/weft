package pod

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ImageConfigFile is the on-disk shape weft microvm pull writes at
// <rootfs>/.weft-microvm/config.json — the resolved OCI image
// entrypoint+cmd+env+cwd+user. Mirrors weft-microvm/configspec.go's
// processSpec exactly so both sides stay byte-compatible.
//
// The host writes it for every pulled image (single-container and
// pod modes). The guest reads it as a fallback when the pod manifest
// (or the synthesised single-container spec) leaves Command empty —
// see EnrichFromImage below.
type ImageConfigFile struct {
	Process ImageProcess `json:"process"`
}

type ImageProcess struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"`
	User struct {
		UID uint32 `json:"uid"`
		GID uint32 `json:"gid"`
	} `json:"user"`
}

// LoadImageConfig reads the resolved OCI process spec from
// <rootfsMount>/.weft-microvm/config.json. Returns os.ErrNotExist when
// the file is absent (host pulled the image with an older puller).
func LoadImageConfig(rootfsMount string) (*ImageConfigFile, error) {
	p := filepath.Join(rootfsMount, ".weft-microvm", "config.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var cf ImageConfigFile
	if err := json.Unmarshal(b, &cf); err != nil {
		return nil, fmt.Errorf("decode %s: %w", p, err)
	}
	return &cf, nil
}

// EnrichFromImage fills empty Container fields (Command, Env, Workdir,
// User) from the rootfs-resolved OCI image config. Already-set fields
// are preserved : a host that explicitly overrides Command/Env wins.
//
// rootfsMount is the path where the container's rootfs share is mounted
// inside the guest (typically /run/weft/rootfs/<id>). It must be called
// AFTER mountShares — the .weft-microvm/config.json lives inside the
// share, so the share must be mounted first.
//
// Returns os.ErrNotExist when no image config is present and the
// container had no Command — that surfaces back as the runtime's
// "command is required" error, with a clearer message.
func EnrichFromImage(c *Container, rootfsMount string) error {
	if len(c.Command) > 0 {
		// Host already resolved the entrypoint (pod-mode classic). Don't
		// re-read — the host's manifest overrides win by design.
		return nil
	}
	cf, err := LoadImageConfig(rootfsMount)
	if err != nil {
		return err
	}
	if len(cf.Process.Args) == 0 {
		return fmt.Errorf("image config at %s/.weft-microvm/config.json has empty process.args", rootfsMount)
	}
	c.Command = cf.Process.Args
	if len(c.Env) == 0 && len(cf.Process.Env) > 0 {
		c.Env = make(map[string]string, len(cf.Process.Env))
		for _, e := range cf.Process.Env {
			for i := 0; i < len(e); i++ {
				if e[i] == '=' {
					c.Env[e[:i]] = e[i+1:]
					break
				}
			}
		}
	}
	if c.Workdir == "" {
		c.Workdir = cf.Process.Cwd
	}
	if c.User == "" && (cf.Process.User.UID != 0 || cf.Process.User.GID != 0) {
		c.User = fmt.Sprintf("%d:%d", cf.Process.User.UID, cf.Process.User.GID)
	}
	return nil
}
