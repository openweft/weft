//go:build windows

package secretsource

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// readSD is the seam: the owner and the DACL of an open handle. Reading them
// from the handle, not the path, keeps the check on the file that is read.
var readSD = func(h windows.Handle) (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
}

// getAce is a seam: GetAce fails only on a corrupt list.
var getAce = windows.GetAce

// checkSecretFile refuses a file whose access list lets anyone but its owner,
// SYSTEM and the Administrators group read or change it. Windows has no mode
// bits (the Go runtime synthesises 0666 or 0444), so the mode is not used.
func checkSecretFile(f *os.File, _ fs.FileInfo, path string) error {
	sd, err := readSD(windows.Handle(f.Fd()))
	if err != nil {
		return fmt.Errorf("%s: reading the access list: %w", path, err)
	}
	owner, aces, err := parseSD(sd)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := daclVerdict(owner, aces); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// parseSD reduces a descriptor to its owner and the entries of its DACL. A
// descriptor with no owner, no DACL, or a null DACL (everyone: full access) is
// refused.
func parseSD(sd *windows.SECURITY_DESCRIPTOR) (string, []ace, error) {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return "", nil, errors.New("no owner in the access list")
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return "", nil, errors.New("no access list, which grants everyone access")
	}
	aces := make([]ace, 0, acl.AceCount)
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var p *windows.ACCESS_ALLOWED_ACE
		if err := getAce(acl, i, &p); err != nil {
			return "", nil, fmt.Errorf("reading access entry %d: %w", i, err)
		}
		a := ace{Type: p.Header.AceType, Flags: p.Header.AceFlags, Mask: uint32(p.Mask)}
		if hasSID(a.Type) {
			a.SID = (*windows.SID)(unsafe.Pointer(&p.SidStart)).String()
		}
		aces = append(aces, a)
	}
	return owner.String(), aces, nil
}
