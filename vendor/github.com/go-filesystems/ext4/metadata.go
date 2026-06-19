package filesystem_ext4

// metadata.go — POSIX metadata mutators (chmod / chown / chtimes), bundled as
// filesystem.MetadataSetter. Each resolves the path to its inode, edits the
// relevant on-disk fields under the per-inode lock, refreshes i_ctime, and
// rewrites the inode (which recomputes its checksum).

import (
	"encoding/binary"
	"os"
	"time"

	filesystem "github.com/go-filesystems/interface"
)

// ext4 inode field byte offsets used here (see struct ext4_inode). The base
// constants in inode.go cover mode/links/flags/block; the rest are spelled out
// literally, matching the style makeDir uses for the timestamp fields.
const (
	inodeOffUID     = 2   // i_uid (low 16)
	inodeOffATime   = 8   // i_atime (32-bit seconds)
	inodeOffCTime   = 12  // i_ctime
	inodeOffMTime   = 16  // i_mtime
	inodeOffGID     = 24  // i_gid (low 16)
	inodeOffUIDHigh = 120 // l_i_uid_high (osd2.linux2)
	inodeOffGIDHigh = 122 // l_i_gid_high (osd2.linux2)
)

// ext4FS implements the bundled POSIX metadata mutators.
var _ filesystem.MetadataSetter = (*ext4FS)(nil)

// withInodeForUpdate resolves path to its inode, re-reads it under the
// per-inode lock so the edit is against current on-disk bytes, applies edit,
// refreshes i_ctime, and writes the inode back.
func (fs *ext4FS) withInodeForUpdate(path string, edit func(in *inode)) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	rw := getRW(fs)
	in, err := lookupPath(rw, fs.partOffset, fs.sb, path)
	if err != nil {
		return err
	}
	owner := NewOwner()
	il := getInodeLock(rw, in.num)
	il.LockOwner(owner)
	defer il.UnlockOwner(owner)
	in, err = readInode(rw, fs.partOffset, fs.sb, in.num)
	if err != nil {
		return err
	}
	edit(in)
	binary.LittleEndian.PutUint32(in.raw[inodeOffCTime:], uint32(time.Now().Unix()))
	return writeInode(rw, fs.partOffset, fs.sb, in)
}

// Chmod replaces the permission + setuid/setgid/sticky bits at path, preserving
// the file-type bits. ctime is refreshed.
func (fs *ext4FS) Chmod(path string, perm os.FileMode) error {
	return fs.withInodeForUpdate(path, func(in *inode) {
		le := binary.LittleEndian
		bits := uint16(perm & 0o777)
		if perm&os.ModeSetuid != 0 {
			bits |= 0o4000
		}
		if perm&os.ModeSetgid != 0 {
			bits |= 0o2000
		}
		if perm&os.ModeSticky != 0 {
			bits |= 0o1000
		}
		mode := le.Uint16(in.raw[inodeOffMode:])
		mode = (mode &^ 0o7777) | bits // keep the type bits, replace the rest
		le.PutUint16(in.raw[inodeOffMode:], mode)
		in.mode = mode
	})
}

// Chown updates uid/gid at path (32-bit, split across the low and high
// halves). ctime is refreshed; mode, body and the other timestamps are left
// alone.
func (fs *ext4FS) Chown(path string, uid, gid uint32) error {
	return fs.withInodeForUpdate(path, func(in *inode) {
		le := binary.LittleEndian
		le.PutUint16(in.raw[inodeOffUID:], uint16(uid))
		le.PutUint16(in.raw[inodeOffUIDHigh:], uint16(uid>>16))
		le.PutUint16(in.raw[inodeOffGID:], uint16(gid))
		le.PutUint16(in.raw[inodeOffGIDHigh:], uint16(gid>>16))
	})
}

// Chtimes sets atime and mtime at path (second resolution, matching this
// driver's 32-bit timestamp handling). ctime is refreshed to now per POSIX.
func (fs *ext4FS) Chtimes(path string, atime, mtime time.Time) error {
	return fs.withInodeForUpdate(path, func(in *inode) {
		le := binary.LittleEndian
		le.PutUint32(in.raw[inodeOffATime:], uint32(atime.Unix()))
		le.PutUint32(in.raw[inodeOffMTime:], uint32(mtime.Unix()))
	})
}
