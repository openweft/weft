// Package cpio writes the "newc" cpio archive format used by the Linux initramfs.
package cpio

import (
	"fmt"
	"io"
)

const (
	magic   = "070701"
	trailer = "TRAILER!!!"
)

type Writer struct {
	w   io.Writer
	ino uint32
	pos int64
}

func NewWriter(w io.Writer) *Writer { return &Writer{w: w, ino: 721} }

type Header struct {
	Name string
	Mode uint32
	Size uint32
	Uid  uint32
	Gid  uint32
	Mtim uint32
	Link string // target for symlinks; ignored otherwise
}

// WriteFile writes a regular file entry followed by its data.
func (cw *Writer) WriteFile(h Header, data []byte) error {
	h.Size = uint32(len(data))
	if err := cw.writeHeader(h, 1); err != nil {
		return err
	}
	return cw.writeBody(data)
}

// WriteDir writes a directory entry.
func (cw *Writer) WriteDir(name string, mode uint32) error {
	h := Header{Name: name, Mode: 0040000 | (mode & 0o7777)}
	return cw.writeHeader(h, 1)
}

// WriteSymlink writes a symlink entry whose payload is the link target.
func (cw *Writer) WriteSymlink(name, target string, mode uint32) error {
	h := Header{Name: name, Mode: 0120000 | (mode & 0o7777), Size: uint32(len(target))}
	if err := cw.writeHeader(h, 1); err != nil {
		return err
	}
	return cw.writeBody([]byte(target))
}

// WriteNod writes a character or block device node entry.
func (cw *Writer) WriteNod(name string, mode uint32, major, minor uint32) error {
	h := Header{Name: name, Mode: mode}
	return cw.writeHeaderFull(h, 1, major, minor)
}

// Close writes the trailer entry.
func (cw *Writer) Close() error {
	if err := cw.writeHeader(Header{Name: trailer}, 1); err != nil {
		return err
	}
	// Pad final archive to 512-byte boundary (kernel-friendly).
	pad := (512 - int(cw.pos%512)) % 512
	if pad > 0 {
		_, err := cw.w.Write(make([]byte, pad))
		return err
	}
	return nil
}

func (cw *Writer) writeHeader(h Header, nlink uint32) error {
	return cw.writeHeaderFull(h, nlink, 0, 0)
}

func (cw *Writer) writeHeaderFull(h Header, nlink, rmaj, rmin uint32) error {
	name := h.Name
	namesize := uint32(len(name)) + 1
	cw.ino++

	hdr := fmt.Sprintf(
		"%s%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		magic,
		cw.ino,
		h.Mode,
		h.Uid,
		h.Gid,
		nlink,
		h.Mtim,
		h.Size,
		uint32(0), uint32(0), // devmajor, devminor
		rmaj, rmin, // rdevmajor, rdevminor
		namesize,
		uint32(0), // check
	)
	if _, err := cw.write([]byte(hdr)); err != nil {
		return err
	}
	if _, err := cw.write([]byte(name)); err != nil {
		return err
	}
	if _, err := cw.write([]byte{0}); err != nil {
		return err
	}
	return cw.pad4()
}

func (cw *Writer) writeBody(data []byte) error {
	if _, err := cw.write(data); err != nil {
		return err
	}
	return cw.pad4()
}

func (cw *Writer) pad4() error {
	pad := (4 - int(cw.pos%4)) % 4
	if pad == 0 {
		return nil
	}
	_, err := cw.write(make([]byte, pad))
	return err
}

func (cw *Writer) write(b []byte) (int, error) {
	n, err := cw.w.Write(b)
	cw.pos += int64(n)
	return n, err
}
