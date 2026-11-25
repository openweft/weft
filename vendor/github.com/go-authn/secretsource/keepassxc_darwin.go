//go:build darwin

package secretsource

import (
	"os"
	"strings"

	"github.com/ebitengine/purego"
)

// csDarwinUserTempDir is _CS_DARWIN_USER_TEMP_DIR from <unistd.h>.
const csDarwinUserTempDir = 65537

// confstrTempDir asks the OS for the per-user temporary directory, the way
// KeePassXC does (BrowserShared.cpp getUserTemporaryDirectory): $TMPDIR can
// point elsewhere and the socket is not there.
func confstrTempDir() string {
	lib, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return os.TempDir()
	}
	var confstr func(name int32, buf *byte, n uintptr) uintptr
	purego.RegisterLibFunc(&confstr, lib, "confstr")
	n := confstr(csDarwinUserTempDir, nil, 0)
	if n == 0 || n > 1024 {
		return os.TempDir()
	}
	buf := make([]byte, n)
	confstr(csDarwinUserTempDir, &buf[0], n)
	return strings.TrimSuffix(strings.TrimRight(string(buf), "\x00"), "/")
}

func init() { userTempDir = confstrTempDir }
