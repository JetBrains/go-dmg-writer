package hfsplus

import (
	"encoding/binary"
	"errors"

	"golang.org/x/text/unicode/norm"
)

// MaxHFSNameRunes is the hard limit on HFS+ filename length in UTF-16 code
// units (TN1150). Names longer than this are an error.
const MaxHFSNameRunes = 255

// ErrNameTooLong is returned when a name exceeds [MaxHFSNameRunes] code units
// after NFD normalization.
var ErrNameTooLong = errors.New("hfsplus: name exceeds 255 UTF-16 code units")

// HFSName is the in-memory representation of a name in the HFS+ catalog:
// UTF-16BE code units, prefixed on disk by a 16-bit length.
type HFSName struct {
	Units []uint16 // UTF-16 code units in host order
}

// NewName converts a UTF-8 string into the HFS+ name form: Unicode NFD
// (Apple's canonical decomposition) then UTF-16. Note that Apple's NFD
// differs from standard Unicode NFD in a handful of code points
// (U+2000-U+2FFF, certain Hangul forms). For the common case of ASCII
// names — which is what an app-bundle DMG typically contains — the two
// forms are identical. Non-ASCII names should still mount correctly but
// may not match the byte-for-byte output of macOS's own `hdiutil`.
//
// TODO(non-ascii): implement the Apple-NFD exception table.
func NewName(s string) (HFSName, error) {
	nfd := norm.NFD.String(s)
	units := encodeUTF16(nfd)
	if len(units) > MaxHFSNameRunes {
		return HFSName{}, ErrNameTooLong
	}
	return HFSName{Units: units}, nil
}

// Bytes returns the wire form of n as UTF-16BE (no length prefix).
func (n HFSName) Bytes() []byte {
	out := make([]byte, 2*len(n.Units))
	for i, u := range n.Units {
		binary.BigEndian.PutUint16(out[i*2:i*2+2], u)
	}
	return out
}

// LenU16 returns the length of the name in UTF-16 code units.
func (n HFSName) LenU16() int { return len(n.Units) }

// IsASCII reports whether every code unit of n is in [0x20, 0x7F]. Used to
// pick the right textEncoding hint (0 = MacRoman, 0x7F = Unicode) on
// catalog records.
func (n HFSName) IsASCII() bool {
	for _, u := range n.Units {
		if u < 0x20 || u >= 0x7F {
			return false
		}
	}
	return true
}

// HFSCompare implements the HFS+ binary-compare ordering: code units are
// compared as unsigned 16-bit values. Returns -1, 0, or +1.
func (n HFSName) HFSCompare(o HFSName) int {
	la, lb := len(n.Units), len(o.Units)
	min := la
	if lb < min {
		min = lb
	}
	for i := 0; i < min; i++ {
		a, b := n.Units[i], o.Units[i]
		if a < b {
			return -1
		}
		if a > b {
			return 1
		}
	}
	switch {
	case la < lb:
		return -1
	case la > lb:
		return 1
	default:
		return 0
	}
}

// encodeUTF16 converts a UTF-8 string into a slice of UTF-16 code units in
// host byte order. Reimplemented here (instead of using unicode/utf16) to
// keep the package's import surface tiny and to make the conversion
// allocation pattern easy to reason about.
func encodeUTF16(s string) []uint16 {
	out := make([]uint16, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x10000:
			out = append(out, uint16(r))
		default:
			r -= 0x10000
			out = append(out, 0xD800|uint16(r>>10), 0xDC00|uint16(r&0x3FF))
		}
	}
	return out
}
