package filesystem_ext4

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

var (
	debugOnce     sync.Once
	debugMu       sync.Mutex
	debugBuf      []byte
	debugEnabled  bool
	debugMax      = 10 * 1024 * 1024 // keep last 10MB of debug output
	debugFilePath string
	debugFile     *os.File
	assocMu       sync.Mutex
)

func initDebug() {
	debugEnabled = os.Getenv("EXT4_LOCK_DEBUG") == "1"
	if p := os.Getenv("EXT4_LOCK_DEBUG_FILE"); p != "" {
		debugFilePath = p
		// best-effort open for append; keep file handle for the life of process
		if f, err := os.OpenFile(debugFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			debugFile = f
		}
	}
}

// debugPrintf records formatted debug output into an in-memory circular
// buffer to avoid blocking hot code paths on stdout/stderr. Callers should
// only use this when diagnostics are enabled via EXT4_LOCK_DEBUG.
func debugPrintf(format string, a ...interface{}) {
	debugOnce.Do(initDebug)
	if !debugEnabled {
		return
	}
	s := fmt.Sprintf(format, a...)
	debugMu.Lock()
	defer debugMu.Unlock()
	if len(debugBuf)+len(s) > debugMax {
		// drop the oldest bytes to keep buffer within limit
		over := len(debugBuf) + len(s) - debugMax
		if over >= len(debugBuf) {
			debugBuf = []byte(s)
			// also write to file if configured
			if debugFile != nil {
				_, _ = debugFile.WriteString(s)
			}
			return
		}
		debugBuf = append(debugBuf[over:], s...)
		if debugFile != nil {
			_, _ = debugFile.WriteString(s)
		}
		return
	}
	debugBuf = append(debugBuf, s...)
	if debugFile != nil {
		_, _ = debugFile.WriteString(s)
	}
}

// DumpDebugLog writes the accumulated debug buffer to the provided writer.
func DumpDebugLog(w io.Writer) {
	debugMu.Lock()
	defer debugMu.Unlock()
	if len(debugBuf) == 0 {
		return
	}
	_, _ = w.Write(debugBuf)
}

// appendAssocLog appends a small immediate log line to a separate file
// used for timing-sensitive instrumentation so we avoid the in-memory
// circular buffer delays. Only active when debug is enabled.
func appendAssocLog(s string) {
	debugOnce.Do(initDebug)
	if !debugEnabled {
		return
	}
	assocMu.Lock()
	defer assocMu.Unlock()
	f, err := os.OpenFile("/tmp/ext4_assoc_debug.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(s + "\n")
	_ = f.Close()
}

// dumpForensicSnapshot writes an on-disk forensic snapshot for the
// provided reservation context. It creates a small text log plus raw
// binary dumps of the last-read block-group descriptor and bitmap (if
// available) under /tmp with a PID+timestamp prefix. This is intended
// to capture authoritative state for races that in-memory logs may miss.
func dumpForensicSnapshot(key string, bits []int, wantMap map[int]reservationInfo, d *bgd, bmap []byte) {
	debugOnce.Do(initDebug)
	pid := os.Getpid()
	ts := time.Now().UnixNano()
	base := fmt.Sprintf("/tmp/ext4_forensic-%d-%d", pid, ts)

	// Text summary
	lf, err := os.OpenFile(base+".log", os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = lf.WriteString(fmt.Sprintf("forensic snapshot key=%s ts=%d bits=%v\n", key, ts, bits))
		for _, bit := range bits {
			if ri, ok := wantMap[bit]; ok {
				_, _ = lf.WriteString(fmt.Sprintf("bit=%d want=%t seq=%d ack=%t ts=%d\n", bit, ri.want, ri.seq, ri.ack != nil, ri.ts))
			} else {
				_, _ = lf.WriteString(fmt.Sprintf("bit=%d <no reservation entry>\n", bit))
			}
		}
		if d != nil {
			_, _ = lf.WriteString(fmt.Sprintf("bgd BlockBitmapBlock=%d FreeBlocksCount=%d InodeBitmapBlock=%d InodeTableBlock=%d UsedDirsCount=%d Flags=%d\n", d.BlockBitmapBlock, d.FreeBlocksCount, d.InodeBitmapBlock, d.InodeTableBlock, d.UsedDirsCount, d.Flags))
		}
		if bmap != nil {
			_, _ = lf.WriteString(fmt.Sprintf("bitmap_len=%d\n", len(bmap)))
		}
		_ = lf.Close()
	}

	// Raw bitmap bytes
	if bmap != nil {
		bf, err := os.OpenFile(base+"_bitmap.bin", os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = bf.Write(bmap)
			_ = bf.Close()
		}
	}

	// Raw BGD bytes
	if d != nil && len(d.raw) > 0 {
		df, err := os.OpenFile(base+"_bgd.bin", os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = df.Write(d.raw)
			_ = df.Close()
		}
	}

	// Also append a short immediate trace so the watcher sees activity.
	appendImmediate("/tmp/ext4_unreserve_debug.log", fmt.Sprintf("forensicDump base=%s key=%s ts=%d", base, key, ts))
}
