package filesystem_ext4

import (
	"errors"
	"fmt"
	"io"

	"github.com/go-volumes/gpt"
)

// partitionOffset returns the byte offset from the start of the image to the
// beginning of the requested partition. partIndex selects a specific partition
// (0-based); pass -1 to auto-select the first Linux data partition.
//
// deviceSize is the total byte size of the backing device; it lets the shared
// go-volumes/gpt parser reject partition entries or entry arrays that fall
// outside the device (M2 — previously the entrySize/numParts fields were used
// unbounded). When deviceSize is not known (<= 0) a generous ceiling is used so
// the parser still performs its internal overflow checks.
//
// A bare filesystem image (no GPT header, no MBR signature) yields offset 0,
// preserving the original fallback behaviour.
func partitionOffset(r io.ReaderAt, partIndex int, deviceSize int64) (int64, error) {
	if deviceSize <= 0 {
		// Unknown size: use a large but finite ceiling so go-volumes/gpt still
		// validates entry-array bounds and partition ranges against overflow.
		deviceSize = 1 << 62
	}

	if partIndex >= 0 {
		part, err := gpt.ByIndex(r, deviceSize, partIndex)
		if err != nil {
			if errors.Is(err, gpt.ErrNoTable) {
				return 0, nil
			}
			return 0, fmt.Errorf("ext4: %w", err)
		}
		return part.StartOffset, nil
	}

	// Auto-select the first Linux data partition. Mirror the historical rule:
	// for GPT match the Linux-filesystem type GUID; for MBR match partition
	// type 0x83 (Linux). A bare image (no table) means offset 0.
	parts, err := gpt.List(r, deviceSize)
	if err != nil {
		if errors.Is(err, gpt.ErrNoTable) {
			return 0, nil
		}
		return 0, fmt.Errorf("ext4: %w", err)
	}
	for _, p := range parts {
		if p.Scheme == gpt.SchemeGPT && p.TypeGUID == gpt.LinuxFilesystemGUID {
			return p.StartOffset, nil
		}
		// For MBR, mirror the historical rule: a Linux (type 0x83) entry whose
		// start LBA is 0 was treated as unused and skipped.
		if p.Scheme == gpt.SchemeMBR && p.MBRType == 0x83 && p.StartOffset != 0 {
			return p.StartOffset, nil
		}
	}
	return 0, fmt.Errorf("ext4: no Linux data partition found")
}
