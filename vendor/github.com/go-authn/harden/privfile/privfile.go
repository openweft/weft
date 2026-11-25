// Package privfile writes a file only its owner can read, the way a ticket
// cache, a credential cache or an association key has to be written.
//
// [Write] puts the data in a 0600 temporary file in the target's own
// directory, syncs it and renames it over the target. A reader sees the old
// file or the new one, never a part; a symbolic link left at the target is
// replaced, never followed; and a directory that others could write into is
// refused before anything is written.
package privfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Mode is the permission of a file [Write] creates.
const Mode fs.FileMode = 0o600

// Write puts data at path, mode 0600, replacing what is there.
//
// The directory of path must exist and, on Unix, belong to the caller and not
// be writable by group or others, nor be a symbolic link: what is kept there
// could be swapped by somebody else. Write does not create it; a caller that
// wants one makes it 0700 first.
//
// path itself may be a symbolic link, a file of loose mode, or absent: the
// rename replaces whatever it is, so nothing the link points at is touched
// and no earlier mode survives. A directory at path is an error.
func Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := checkDir(dir); err != nil {
		return fmt.Errorf("privfile: %w", err)
	}
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		return fmt.Errorf("privfile: %s is a directory", path)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("privfile: %w", err)
	}
	// os.CreateTemp opens O_EXCL, 0600: a name an attacker planted is not reused.
	tmp, err := createTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("privfile: %w", err)
	}
	name := tmp.Name()
	if err := fill(tmp, data); err != nil {
		os.Remove(name)
		return fmt.Errorf("privfile: %w", err)
	}
	if err := rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("privfile: %w", err)
	}
	// The rename, made durable: without it a power loss can bring back the file
	// the rename replaced. Not every system can sync a directory (Windows).
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// file is the part of *os.File that fill uses.
type file interface {
	Write([]byte) (int, error)
	Chmod(fs.FileMode) error
	Sync() error
	Close() error
}

// fill writes, protects, syncs and closes f; a failure still closes it.
func fill(f file, data []byte) error {
	var err error
	if _, werr := f.Write(data); werr != nil {
		err = werr
	} else if cerr := protect(f); cerr != nil {
		err = cerr
	} else {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// Replaced by tests.
var (
	createTemp = newTemp
	rename     = os.Rename
)

// namedFile is what createTemp returns: a file with a name.
type namedFile interface {
	file
	Name() string
}
