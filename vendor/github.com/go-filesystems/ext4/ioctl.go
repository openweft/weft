package filesystem_ext4

import (
	"encoding/binary"
)

// GetFlags returns the ext4 inode flags for the file at path.
func (fs *ext4FS) GetFlags(path string) (uint32, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	rw := getRW(fs)
	in, err := lookupPath(rw, fs.partOffset, fs.sb, path)
	if err != nil {
		return 0, err
	}
	return in.flags(), nil
}

// SetFlags sets the ext4 inode flags for the file at path and persists the
// inode (including updated checksum when enabled).
func (fs *ext4FS) SetFlags(path string, flags uint32) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	rw := getRW(fs)
	in, err := lookupPath(rw, fs.partOffset, fs.sb, path)
	if err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(in.raw[inodeOffFlags:], flags)
	return writeInode(rw, fs.partOffset, fs.sb, in)
}
