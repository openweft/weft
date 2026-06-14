// SPDX-License-Identifier: BSD-3-Clause
// Copyright (c) 2026, the go-tpm2/common authors. All rights reserved.

package common

// Error is the typed error returned by this package. Using a string-kind
// error lets callers compare against the exported sentinels with == and
// keeps every error value a compile-time constant (no allocation, safe
// for comparison across package boundaries).
type Error string

// Error implements the error interface.
func (e Error) Error() string { return string(e) }
