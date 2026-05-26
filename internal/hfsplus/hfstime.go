// Package hfsplus implements a write-once builder for the HFS+ filesystem
// format described in Apple Technical Note TN1150.
//
// "Write-once" means the package builds a complete, self-consistent volume
// image from an in-memory description in a single forward pass. There is
// no support for opening or modifying existing volumes.
package hfsplus

import "time"

// macEpoch is HFS+'s reference instant: 1904-01-01 00:00:00 UTC.
// All timestamps in the format are 32-bit unsigned seconds past this point.
var macEpoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)

// MacTime converts a Go time into HFS+'s 32-bit second-precision form,
// clamped to the representable range [0, 2^32).
//
// Clamp policy:
//   - A zero time.Time, or any time at or before 1904-01-01 00:00:00
//     UTC, returns 0. The helper is safe for the "if t.IsZero() use
//     now" pattern the public API uses; callers that want fsck_hfs to
//     pass should rewrite a returned 0 to 1 (fsck_hfs flags zero
//     timestamps as a "checked volume" issue).
//   - Times past 2040-02-06 06:28:15 UTC saturate at 2^32-1 rather than
//     overflowing. This is a silent rewrite; if you care about
//     post-2040 timestamps, use APFS, not HFS+.
func MacTime(t time.Time) uint32 {
	if t.IsZero() {
		return 0
	}
	delta := t.Unix() - macEpoch.Unix()
	switch {
	case delta < 0:
		return 0
	case delta > 0xFFFFFFFF:
		return 0xFFFFFFFF
	default:
		return uint32(delta)
	}
}
