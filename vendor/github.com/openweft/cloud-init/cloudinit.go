// Package cloudinit builds minimal ISO 9660 cloud-init seed images and
// generates #cloud-config user-data payloads. No external commands are
// required; everything is produced in memory.
package cloudinit

import (
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const isoSectorSize = 2048

// BuildCloudInitISO creates a minimal ISO 9660 image suitable for cloud-init's
// NoCloud datasource. The image contains two files in the root directory:
//   - meta-data  (instance-id + local-hostname)
//   - user-data  (the user-supplied shell script or cloud-config)
//
// The ISO volume label is "cidata", which cloud-init detects via blkid without
// any network configuration. Attach the resulting image to a VM as a read-only
// disk as a read-only drive and cloud-init will pick it up on first boot.
//
// No external commands are required; the ISO is built entirely in memory.
func BuildCloudInitISO(instanceID, hostname, userData string) ([]byte, error) {
	if instanceID == "" {
		instanceID = "iid-default"
	}
	if hostname == "" {
		hostname = instanceID
	}

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, hostname)
	file1 := []byte(metaData)
	file2 := []byte(userData)

	// Sector layout:
	//   0-15  system area (zeros)
	//   16    Primary Volume Descriptor
	//   17    Volume Descriptor Set Terminator
	//   18    L-type Path Table
	//   19    M-type Path Table
	//   20    Root directory
	//   21    meta-data content
	//   22+   user-data content
	const (
		sectorPVD        = 16
		sectorVDST       = 17
		sectorLPathTable = 18
		sectorMPathTable = 19
		sectorRootDir    = 20
		sectorFile1      = 21
	)

	sectFile1 := sectorsNeeded(len(file1))
	sectorFile2 := sectorFile1 + sectFile1
	sectFile2 := sectorsNeeded(len(file2))
	totalSectors := sectorFile2 + sectFile2

	buf := make([]byte, totalSectors*isoSectorSize)
	now := time.Now()

	// ── Primary Volume Descriptor (sector 16) ──────────────────────────────
	pvd := buf[sectorPVD*isoSectorSize:]
	pvd[0] = 1
	copy(pvd[1:6], "CD001")
	pvd[6] = 1

	isoFill(pvd[8:40], ' ')                    // system identifier
	isoStr(pvd[40:72], "cidata")               // volume identifier (label)
	isoBothU32(pvd[80:], uint32(totalSectors)) // volume space size
	isoBothU16(pvd[120:], 1)                   // volume set size
	isoBothU16(pvd[124:], 1)                   // volume sequence number
	isoBothU16(pvd[128:], 2048)                // logical block size

	// Path table: root only → 10 bytes
	isoBothU32(pvd[132:], 10)
	binary.LittleEndian.PutUint32(pvd[140:], sectorLPathTable)
	binary.LittleEndian.PutUint32(pvd[144:], 0) // optional L-type (none)
	binary.BigEndian.PutUint32(pvd[148:], sectorMPathTable)
	binary.BigEndian.PutUint32(pvd[152:], 0) // optional M-type (none)

	// Root directory record embedded in PVD (34 bytes at offset 156)
	rootSize := dirEntryLen("\x00") + dirEntryLen("\x01") +
		dirEntryLen("META-DATA;1") + dirEntryLen("USER-DATA;1")
	writeDirRecord(pvd[156:], "\x00", sectorRootDir, uint32(rootSize), true, now)

	isoFill(pvd[190:318], ' ')  // volume set identifier
	isoFill(pvd[318:446], ' ')  // publisher identifier
	isoFill(pvd[446:574], ' ')  // data preparer identifier
	isoFill(pvd[574:702], ' ')  // application identifier
	isoFill(pvd[702:739], ' ')  // copyright file identifier
	isoFill(pvd[739:776], ' ')  // abstract file identifier
	isoFill(pvd[776:813], ' ')  // bibliographic file identifier
	isoDateTime(pvd[813:], now) // creation date/time
	isoDateTime(pvd[830:], now) // modification date/time
	// expiration (847) and effective (864) left as zeros (unspecified)
	pvd[881] = 1 // file structure version

	// ── Volume Descriptor Set Terminator (sector 17) ──────────────────────
	vdst := buf[sectorVDST*isoSectorSize:]
	vdst[0] = 0xFF
	copy(vdst[1:6], "CD001")
	vdst[6] = 1

	// ── L-type Path Table (sector 18, little-endian) ──────────────────────
	lpt := buf[sectorLPathTable*isoSectorSize:]
	lpt[0] = 1 // directory identifier length
	lpt[1] = 0 // extended attribute record length
	binary.LittleEndian.PutUint32(lpt[2:], sectorRootDir)
	binary.LittleEndian.PutUint16(lpt[6:], 1) // parent directory number
	lpt[8] = 0x01                             // root directory identifier
	// lpt[9] = 0 (padding: id length 1 is odd → pad to even, already zero)

	// ── M-type Path Table (sector 19, big-endian) ─────────────────────────
	mpt := buf[sectorMPathTable*isoSectorSize:]
	mpt[0] = 1
	mpt[1] = 0
	binary.BigEndian.PutUint32(mpt[2:], sectorRootDir)
	binary.BigEndian.PutUint16(mpt[6:], 1)
	mpt[8] = 0x01

	// ── Root Directory (sector 20) ────────────────────────────────────────
	root := buf[sectorRootDir*isoSectorSize:]
	off := 0
	off += writeDirRecord(root[off:], "\x00", sectorRootDir, uint32(rootSize), true, now) // "."
	off += writeDirRecord(root[off:], "\x01", sectorRootDir, uint32(rootSize), true, now) // ".."
	off += writeDirRecord(root[off:], "META-DATA;1", sectorFile1, uint32(len(file1)), false, now)
	_ = writeDirRecord(root[off:], "USER-DATA;1", uint32(sectorFile2), uint32(len(file2)), false, now)

	// ── File data ─────────────────────────────────────────────────────────
	copy(buf[sectorFile1*isoSectorSize:], file1)
	copy(buf[sectorFile2*isoSectorSize:], file2)

	return buf, nil
}

// BuildSSHCloudConfig returns a #cloud-config user-data string that injects
// the provided SSH public keys into the default user's authorized_keys via
// the cloud-init native ssh_authorized_keys directive. Each key is trimmed
// before inclusion. Returns "" when pubKeys is empty.
//
// `users: [default]` is included explicitly so cloud-init configures the
// distro's default user (e.g. `ubuntu`) even when the image's cloud.cfg
// requires an explicit users declaration. Without it, some Ubuntu 24.04
// images skip the ssh module for the default user.
func BuildSSHCloudConfig(pubKeys []string, distribution string) string {
	if len(pubKeys) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("#cloud-config\nusers:\n  - default\nssh_authorized_keys:\n")
	for _, k := range pubKeys {
		k = strings.TrimSpace(k)
		if k != "" {
			sb.WriteString("  - " + k + "\n")
		}
	}
	return sb.String()
}

// sectorsNeeded returns the number of 2048-byte sectors required for n bytes.
func sectorsNeeded(n int) int {
	if n == 0 {
		return 1
	}
	return (n + isoSectorSize - 1) / isoSectorSize
}

// dirEntryLen returns the padded length of a directory record with the given
// file identifier string.
func dirEntryLen(id string) int {
	n := 33 + len(id)
	if n%2 != 0 {
		n++
	}
	return n
}

// writeDirRecord writes a directory record at b and returns the number of
// bytes written (including padding).
func writeDirRecord(b []byte, fileID string, extentLoc, dataLen uint32, isDir bool, t time.Time) int {
	idLen := len(fileID)
	recLen := 33 + idLen
	if recLen%2 != 0 {
		recLen++
	}
	b[0] = byte(recLen)
	b[1] = 0
	isoBothU32(b[2:], extentLoc)
	isoBothU32(b[10:], dataLen)
	isoRecordingTime(b[18:], t)
	if isDir {
		b[25] = 0x02
	}
	// b[26] = file unit size = 0
	// b[27] = interleave gap size = 0
	isoBothU16(b[28:], 1) // volume sequence number
	b[32] = byte(idLen)
	copy(b[33:], fileID)
	return recLen
}

// isoBothU32 writes v in both-endian 32-bit format (8 bytes: LE then BE).
func isoBothU32(b []byte, v uint32) {
	binary.LittleEndian.PutUint32(b[0:], v)
	binary.BigEndian.PutUint32(b[4:], v)
}

// isoBothU16 writes v in both-endian 16-bit format (4 bytes: LE then BE).
func isoBothU16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b[0:], v)
	binary.BigEndian.PutUint16(b[2:], v)
}

// isoStr copies s into b (space-padded to len(b)).
func isoStr(b []byte, s string) {
	isoFill(b, ' ')
	copy(b, s)
}

// isoFill fills b with the byte c.
func isoFill(b []byte, c byte) {
	for i := range b {
		b[i] = c
	}
}

// isoDateTime writes a 17-byte ISO 9660 date/time field.
func isoDateTime(b []byte, t time.Time) {
	s := fmt.Sprintf("%04d%02d%02d%02d%02d%02d00",
		t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second())
	copy(b[:16], s)
	b[16] = 0 // GMT offset
}

// isoRecordingTime writes a 7-byte directory-record date/time field.
func isoRecordingTime(b []byte, t time.Time) {
	b[0] = byte(t.Year() - 1900)
	b[1] = byte(t.Month())
	b[2] = byte(t.Day())
	b[3] = byte(t.Hour())
	b[4] = byte(t.Minute())
	b[5] = byte(t.Second())
	b[6] = 0 // GMT
}
