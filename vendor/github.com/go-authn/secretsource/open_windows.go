//go:build windows

package secretsource

import (
	"fmt"
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

// Seams: the two calls that fail only on a handle the system has gone bad on.
var (
	createFile = windows.CreateFile
	fileAttrs  = func(h windows.Handle) (uint32, error) {
		var info windows.ByHandleFileInformation
		err := windows.GetFileInformationByHandle(h, &info)
		return info.FileAttributes, err
	}
)

// openSecretFile opens the file itself, not what a link at the last component
// names: FILE_FLAG_OPEN_REPARSE_POINT opens the reparse point rather than
// following it, and the attributes of the open handle then say whether it is
// one (a symbolic link, a junction, a mount point). A reparse point is refused,
// the check being made on the handle that is read.
func openSecretFile(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := createFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	attrs, err := fileAttrs(h)
	if err != nil {
		windows.CloseHandle(h)
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("%s: is a symbolic link or other reparse point, which file: does not follow; point at the file it names instead", path)
	}
	return os.NewFile(uintptr(h), path), nil
}
