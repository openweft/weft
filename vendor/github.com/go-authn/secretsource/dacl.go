package secretsource

import "fmt"

// The Windows access check, as plain data so it is tested on every platform.
// windows.go reads the descriptor of the open file and turns it into these.

// ace is one entry of a discretionary ACL, reduced to what the verdict reads.
type ace struct {
	Type  byte   // ACCESS_*_ACE_TYPE
	Flags byte   // ACE flags
	Mask  uint32 // access rights
	SID   string // empty unless hasSID(Type)
}

const (
	aceAllowed        = 0x00
	aceDenied         = 0x01
	aceAllowedCB      = 0x09
	aceDeniedCB       = 0x0A
	aceDeniedObject   = 0x06
	aceDeniedCBObject = 0x0C

	aceInheritOnly = 0x08 // the entry applies to children, never to this file

	sidSystem         = "S-1-5-18"
	sidAdministrators = "S-1-5-32-544"

	// Rights that let a principal read the secret or replace it, or take the
	// file over: FILE_READ_DATA, FILE_WRITE_DATA, FILE_APPEND_DATA,
	// FILE_WRITE_EA, DELETE, WRITE_DAC, WRITE_OWNER, and the generic rights
	// (GENERIC_ALL, GENERIC_WRITE, GENERIC_READ) a descriptor may still carry
	// unmapped. Attribute, EA-read, READ_CONTROL and SYNCHRONIZE reveal nothing.
	aceDangerous uint32 = 0x1 | 0x2 | 0x4 | 0x10 | 0x10000 | 0x40000 | 0x80000 |
		0x10000000 | 0x40000000 | 0x80000000
)

// hasSID reports whether an entry of this type holds its SID at the plain
// offset (right after the mask), which is the only layout read. Object and
// compound entries put it elsewhere and do not occur on files.
func hasSID(t byte) bool {
	switch t {
	case aceAllowed, aceDenied, aceAllowedCB, aceDeniedCB:
		return true
	}
	return false
}

// daclVerdict accepts the file when no entry grants a dangerous right to a
// SID other than the owner, SYSTEM and the Administrators group.
//
// Soundness. aces is the effective list of the open file, inherited entries
// included, so inheritance is already applied. A deny entry never grants, so
// it is ignored: skipping it can only refuse more, never accept more, and the
// order of a non-canonical list cannot matter. An entry that only
// inherits to children does not apply to this file and is skipped. Any other
// type than a plain or callback allow (object, compound, unknown) is refused
// because its SID was not read. A callback entry is treated as unconditional.
func daclVerdict(owner string, aces []ace) error {
	for _, a := range aces {
		if a.Type == aceDenied || a.Type == aceDeniedCB || a.Type == aceDeniedObject || a.Type == aceDeniedCBObject {
			continue
		}
		if a.Flags&aceInheritOnly != 0 {
			continue
		}
		if !hasSID(a.Type) {
			return fmt.Errorf("an access entry of type %#x, which cannot be checked", a.Type)
		}
		if a.Mask&aceDangerous == 0 || a.SID == owner || a.SID == sidSystem || a.SID == sidAdministrators {
			continue
		}
		return fmt.Errorf("the access list grants rights %#x to %s, who is not the owner, SYSTEM or the Administrators group", a.Mask&aceDangerous, a.SID)
	}
	return nil
}
