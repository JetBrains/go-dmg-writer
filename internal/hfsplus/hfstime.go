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
// clamped to the representable range [0, 2^32). A zero time.Time maps to
// 1904-01-01 itself; the helper is safe for the "if t.IsZero() use now"
// pattern the public API uses.
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
