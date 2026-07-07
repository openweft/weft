// Package safeio provides small, dependency-free allocation, bounds, and
// loop guards for parsing UNTRUSTED on-disk filesystem images.
//
// The filesystem drivers in this org parse images supplied by an attacker.
// A malicious image must never panic the host, read out of bounds,
// integer-overflow into a bad allocation or slice, loop forever, or OOM.
// This package makes the four near-universal defenses easy to apply:
//
//   - class (A) unbounded make([]byte, N) → OOM:        MakeBytes, ReadAtFull
//   - class (B) unbounded chain/tree traversal → loop:  LoopGuard, VisitSet
//   - class (C) fixed-offset read without length check: CheckBounds, Slice
//   - class (D) unvalidated geometry → divide-by-zero:  (callers compare to 0)
//
// All helpers return errors instead of panicking. The sentinel errors all
// wrap [ErrSafeIO], so callers can match either the specific cause
// (errors.Is(err, ErrTooLarge)) or the family (errors.Is(err, ErrSafeIO)).
//
// The package has no dependencies outside the standard library and is
// compatible with go 1.25 and CGO_ENABLED=0.
package safeio

import (
	"errors"
	"fmt"
	"io"
)

// ErrSafeIO is the base error that every sentinel in this package wraps,
// so callers can match the whole family with errors.Is(err, ErrSafeIO).
var ErrSafeIO = errors.New("safeio")

// Sentinel errors. Each wraps [ErrSafeIO].
var (
	// ErrTooLarge is returned when a requested size is negative or exceeds
	// the supplied ceiling (class A: unbounded allocation).
	ErrTooLarge = fmt.Errorf("%w: size too large", ErrSafeIO)
	// ErrOutOfBounds is returned when an offset/length pair would read or
	// slice outside the available buffer (class C: out-of-bounds access).
	ErrOutOfBounds = fmt.Errorf("%w: out of bounds", ErrSafeIO)
	// ErrLoopLimit is returned by a LoopGuard once its iteration budget is
	// exhausted (class B: unbounded traversal).
	ErrLoopLimit = fmt.Errorf("%w: loop iteration limit exceeded", ErrSafeIO)
	// ErrCycle is returned (or signalled) when a VisitSet observes an
	// already-visited node id (class B: cyclic traversal).
	ErrCycle = fmt.Errorf("%w: cycle detected", ErrSafeIO)
)

// MakeBytes returns make([]byte, n) after validating n against max, the
// universal fix for class (A) unbounded allocations. Callers pass the
// device/image size (or a sane ceiling) as max.
//
// It returns [ErrTooLarge] if n < 0, max < 0, or n > max. n == 0 yields a
// non-nil empty slice. Because n and max are int64, callers can pass raw
// on-disk fields without a lossy conversion to int first; the result length
// still fits in int on every supported (64-bit) platform once n <= max.
func MakeBytes(n, max int64) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("%w: negative size %d", ErrTooLarge, n)
	}
	if max < 0 {
		return nil, fmt.Errorf("%w: negative ceiling %d", ErrTooLarge, max)
	}
	if n > max {
		return nil, fmt.Errorf("%w: size %d exceeds ceiling %d", ErrTooLarge, n, max)
	}
	return make([]byte, n), nil
}

// CheckBounds verifies that the half-open range [off, off+n) lies entirely
// within a buffer of the given length, i.e. off >= 0 && n >= 0 &&
// off+n <= length. The sum is computed in int64 so it cannot wrap on a
// 64-bit platform, defeating class (C) overflow tricks such as
// off = maxint, n = 1.
//
// It returns nil when the range is valid, otherwise [ErrOutOfBounds].
func CheckBounds(off, n, length int) error {
	if off < 0 {
		return fmt.Errorf("%w: negative offset %d", ErrOutOfBounds, off)
	}
	if n < 0 {
		return fmt.Errorf("%w: negative length %d", ErrOutOfBounds, n)
	}
	if length < 0 {
		return fmt.Errorf("%w: negative buffer length %d", ErrOutOfBounds, length)
	}
	// Compute off+n in int64. On a 64-bit platform int is int64, so the sum
	// itself can wrap (e.g. off=MaxInt, n=1); detect that explicitly.
	end := int64(off) + int64(n)
	if end < int64(off) || end > int64(length) {
		return fmt.Errorf("%w: range [%d,%d+%d) exceeds buffer length %d",
			ErrOutOfBounds, off, off, n, length)
	}
	return nil
}

// Slice returns buf[off:off+n] after a [CheckBounds] validation, so a
// malformed offset/length yields an error rather than a slice-bounds panic
// (class C). The returned slice aliases buf; callers that need an
// independent copy must copy it themselves.
func Slice(buf []byte, off, n int) ([]byte, error) {
	if err := CheckBounds(off, n, len(buf)); err != nil {
		return nil, err
	}
	return buf[off : off+n], nil
}

// ReadAtFull allocates a bounded buffer of n bytes (rejecting n > max or
// n < 0 via [MakeBytes]) and fills it from r at off using io.ReadFull
// semantics: it reads exactly n bytes or returns an error. A short image
// therefore yields io.ErrUnexpectedEOF (or io.EOF when n > 0 and nothing
// could be read) instead of a partially-populated buffer.
//
// off, n, and max are int64 so callers can pass raw on-disk fields without
// a lossy narrowing. This is the combined fix for classes (A) and (C) on
// the common "seek to an attacker-controlled offset and read an
// attacker-controlled length" pattern.
func ReadAtFull(r io.ReaderAt, off, n, max int64) ([]byte, error) {
	if off < 0 {
		return nil, fmt.Errorf("%w: negative offset %d", ErrOutOfBounds, off)
	}
	buf, err := MakeBytes(n, max)
	if err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(io.NewSectionReader(r, off, n), buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// LoopGuard bounds the number of iterations of a chain or tree walk where a
// full visited-set is overkill (e.g. a FAT cluster chain or an extent
// chain). Construct it with [NewLoopGuard] and call [LoopGuard.Next] once
// per iteration; after max successful calls the next call returns
// [ErrLoopLimit]. The zero value is not usable; use NewLoopGuard.
type LoopGuard struct {
	max int
	n   int
}

// NewLoopGuard returns a LoopGuard that permits up to max iterations. A
// non-positive max means "no iterations are allowed": the first
// [LoopGuard.Next] returns [ErrLoopLimit], which is the safe default for an
// attacker-supplied or nonsensical bound.
func NewLoopGuard(max int) *LoopGuard {
	return &LoopGuard{max: max}
}

// Next records one iteration. It returns nil for the first max calls and
// [ErrLoopLimit] thereafter, so a malformed image that forms an unbounded
// or cyclic chain terminates the walk with an error instead of spinning
// forever.
func (g *LoopGuard) Next() error {
	if g.n >= g.max {
		return fmt.Errorf("%w: %d", ErrLoopLimit, g.max)
	}
	g.n++
	return nil
}

// Count reports how many times [LoopGuard.Next] has returned nil so far.
func (g *LoopGuard) Count() int { return g.n }

// VisitSet detects revisited node ids during a traversal that must not
// follow a cycle (e.g. a B-tree whose block pointers form a loop). The zero
// value is ready to use.
type VisitSet struct {
	seen map[uint64]struct{}
}

// Add records id as visited and reports whether this is the first time id
// has been seen. A false return means id was already present, i.e. the
// traversal has looped back; callers should treat that as [ErrCycle] via
// [VisitSet.Check] or by bailing out directly.
func (s *VisitSet) Add(id uint64) (firstTime bool) {
	if s.seen == nil {
		s.seen = make(map[uint64]struct{})
	}
	if _, ok := s.seen[id]; ok {
		return false
	}
	s.seen[id] = struct{}{}
	return true
}

// Check is a convenience wrapper around [VisitSet.Add] that returns
// [ErrCycle] (annotated with id) when id has already been visited, and nil
// otherwise.
func (s *VisitSet) Check(id uint64) error {
	if !s.Add(id) {
		return fmt.Errorf("%w: node %d", ErrCycle, id)
	}
	return nil
}

// Len reports how many distinct ids have been recorded.
func (s *VisitSet) Len() int { return len(s.seen) }
