// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/common authors. All rights reserved.

package common

// Regs is the platform-provided MMIO accessor that the register-level
// TPM drivers (CRB, TIS) use to touch the TPM register block. It is the
// analogue of go-virtio's BAR accessor: the platform owns the actual
// mapping (a physical MMIO window, an mmap of /dev/mem, or a test stub)
// and exposes it through these four primitives.
//
// off is a byte offset within the TPM register window (for example, the
// TIS register file conventionally begins at the locality base and the
// CRB control area at its own base; the driver adds the field offset).
// Multi-byte accesses use the platform's native register width; the TPM
// register semantics for which width to use at which offset belong to
// the driver, not here.
type Regs interface {
	Read8(off uint32) uint8
	Read32(off uint32) uint32
	Write8(off uint32, v uint8)
	Write32(off uint32, v uint32)
}

// ReadBytes fills p by reading len(p) consecutive bytes from r starting
// at off, one byte at a time via Read8. It is used to drain the CRB
// command/response buffer and the TIS data FIFO, where the data is a
// byte stream rather than width-defined registers.
func ReadBytes(r Regs, off uint32, p []byte) {
	for i := range p {
		p[i] = r.Read8(off + uint32(i))
	}
}

// WriteBytes writes p to r starting at off, one byte at a time via
// Write8. It is the counterpart of ReadBytes for filling the CRB command
// buffer / TIS FIFO.
func WriteBytes(r Regs, off uint32, p []byte) {
	for i, b := range p {
		r.Write8(off+uint32(i), b)
	}
}
