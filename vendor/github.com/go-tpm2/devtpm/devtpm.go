// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/devtpm authors. All rights reserved.

package devtpm

import (
	"io"
	"os"

	"github.com/go-tpm2/common"
)

// DefaultDevice is the Linux kernel TPM *resource-manager* character
// device. Each open file descriptor on /dev/tpmrm0 is an independent
// TPM2 command channel multiplexed by the in-kernel resource manager
// (the "tpm2-space" / kernel-space RM): it virtualizes transient-object
// and session handle slots so concurrent users do not exhaust the TPM's
// scarce volatile memory, and it flushes a client's transient context
// when that client closes the device.
//
// The raw, non-resource-managed device is /dev/tpm0: it speaks the same
// one-write-one-command / one-read-one-response framing but with no
// handle virtualization and a single-open exclusivity constraint, so a
// caller that leaks a transient handle can wedge the whole TPM. Prefer
// /dev/tpmrm0 unless you specifically need raw, unmediated access.
//
// Linux exposes both through drivers/char/tpm: tpm_dev_common.c
// implements the file_operations such that a write() delivers exactly
// one complete command to the TPM and the matching read() returns
// exactly one complete response (the driver buffers the response and
// hands it back whole), which is the framing this transport relies on.
const DefaultDevice = "/dev/tpmrm0"

// maxResponse is the size of the buffer Send reads a response into. A
// TPM 2.0 response is bounded by the 4096-byte maximum the TCG TPM 2.0
// Library permits for a command/response on a TPM character device, and
// the Linux tpm character driver returns the entire buffered response in
// a single read(), so one read into a buffer of this size always
// captures a full response.
const maxResponse = 4096

// Error sentinels for the devtpm transport, typed as common.Error so
// callers may compare with ==.
const (
	// ErrShortWrite is returned when the device accepts fewer bytes than
	// the full command. The Linux tpm character device treats each
	// write() as one whole command and never performs a partial write of
	// a valid command, so a short write means the command was not
	// delivered intact and must not be paired with a read.
	ErrShortWrite = common.Error("devtpm: device accepted a short write of the command")
	// ErrShortResponse is returned when a read() returns fewer bytes than
	// a TPM 2.0 response header (common.HeaderSize). Such a buffer cannot
	// contain a parseable response.
	ErrShortResponse = common.Error("devtpm: response shorter than a TPM 2.0 header")
)

// Transport is a github.com/go-tpm2/common.Transport over a Linux TPM
// character device (typically /dev/tpmrm0, the kernel resource-manager
// channel). It carries the raw TPM 2.0 command/response byte stream: one
// Write delivers one complete command and one Read returns the whole
// response, matching the drivers/char/tpm file-operations contract.
//
// The wrapped io.ReadWriteCloser is normally the *os.File returned by
// Open, but New accepts any io.ReadWriteCloser that honors the same
// one-write-one-command / one-read-one-response framing — for example a
// unix-domain socket to swtpm's --server data channel, which speaks the
// identical raw TPM2 protocol the character device carries.
type Transport struct {
	rwc io.ReadWriteCloser
}

// compile-time assertion that *Transport satisfies common.Transport.
var _ common.Transport = (*Transport)(nil)

// Open opens the TPM character device at path for reading and writing and
// wraps it in a Transport. Pass DefaultDevice for the resource-manager
// channel (/dev/tpmrm0).
//
// It opens with os.O_RDWR and no O_CREAT: the device node must already
// exist (the kernel tpm driver creates it), so a nonexistent path is an
// error rather than a freshly created regular file.
func Open(path string) (*Transport, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return New(f), nil
}

// New wraps an existing io.ReadWriteCloser as a Transport. It exists for
// tests and for non-file transports that nonetheless honor the
// one-write-one-command / one-read-one-response framing — such as a unix
// socket to swtpm's raw-TPM2 data channel — letting them ride the same
// code path as the real character device.
func New(rwc io.ReadWriteCloser) *Transport {
	return &Transport{rwc: rwc}
}

// Send transmits one fully-marshaled TPM 2.0 command buffer and returns
// the full response buffer (header + parameters). It satisfies
// common.Transport.
//
// The Linux tpm character device frames each command as a single write()
// and each response as a single read() (drivers/char/tpm,
// tpm_dev_common.c): Send therefore writes the whole command in one Write
// and reads the whole response in one Read.
//
//  1. Write the entire command in one Write. The driver does not perform
//     partial writes of a valid command, so a short count means the
//     command was not delivered intact; that is reported as ErrShortWrite
//     rather than silently retried, since a re-issued tail would be
//     misframed as a new command.
//  2. Read the response in one Read into a maxResponse buffer. The driver
//     returns the entire buffered response in a single read().
//  3. Validate that at least a TPM 2.0 header was returned
//     (common.HeaderSize); otherwise return ErrShortResponse.
//
// Write and read errors from the underlying device are returned
// unwrapped so callers can inspect the concrete os/syscall error.
func (t *Transport) Send(cmd []byte) (rsp []byte, err error) {
	n, err := t.rwc.Write(cmd)
	if err != nil {
		return nil, err
	}
	if n != len(cmd) {
		return nil, ErrShortWrite
	}

	buf := make([]byte, maxResponse)
	n, err = t.rwc.Read(buf)
	if err != nil {
		return nil, err
	}
	if n < common.HeaderSize {
		return nil, ErrShortResponse
	}
	return buf[:n], nil
}

// Close closes the underlying device. For /dev/tpmrm0 this releases the
// resource-manager session, flushing any transient objects and sessions
// the kernel RM created on this client's behalf.
func (t *Transport) Close() error {
	return t.rwc.Close()
}
