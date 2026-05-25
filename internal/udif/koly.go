// Package udif implements the writer side of Apple's Universal Disk Image
// Format (UDIF), aka the .dmg container. The format is documented by Apple
// indirectly through the `hdiutil` man page and has been thoroughly
// characterized by public reverse-engineering efforts.
//
// The on-disk wire format is big-endian throughout, so all encoders in this
// package use [encoding/binary.BigEndian].
package udif

import (
	"encoding/binary"
	"errors"
	"io"
)

// KolySignature is the 4-byte magic at the start of a UDIF trailer ("koly").
const KolySignature uint32 = 0x6B6F6C79

// KolyVersion is the trailer-format version. All modern DMGs use version 4.
const KolyVersion uint32 = 4

// KolySize is the on-disk size of the UDIF resource file (trailer). The
// trailer is always exactly 512 bytes and lives at the very end of the file.
const KolySize = 512

// ImageVariant values for the [Koly.ImageVariant] field.
const (
	ImageVariantDevice    uint32 = 1
	ImageVariantPartition uint32 = 2
)

// Flag bits for the [Koly.Flags] field.
const (
	FlagFlattened uint32 = 1 << 0
)

// UDIFChecksum is the 136-byte checksum container embedded in [Koly] for
// both the data-fork and master checksums.
//
// The Data array has space for up to 32 uint32s (128 bytes); only the first
// entry is used for CRC32 sums. Size is the number of *bits* of checksum
// present (0x20 = 32 bits for CRC32).
type UDIFChecksum struct {
	Type uint32
	Size uint32 // in bits
	Data [32]uint32
}

// Checksum type values for [UDIFChecksum.Type].
const (
	ChecksumNone    uint32 = 0x00000000
	ChecksumCRC32   uint32 = 0x00000002
	ChecksumMKBlock uint32 = 0x00000002 // same wire value, different semantics
)

func (c *UDIFChecksum) encode(b []byte) {
	_ = b[135]
	binary.BigEndian.PutUint32(b[0:4], c.Type)
	binary.BigEndian.PutUint32(b[4:8], c.Size)
	for i := 0; i < 32; i++ {
		binary.BigEndian.PutUint32(b[8+i*4:8+i*4+4], c.Data[i])
	}
}

// UDIFID is a 16-byte opaque segment identifier embedded in [Koly]. It is
// conventionally treated as a random GUID-like value; we derive ours
// deterministically from the volume name + timestamp so output is
// reproducible.
type UDIFID struct {
	Data1, Data2, Data3, Data4 uint32
}

func (u *UDIFID) encode(b []byte) {
	_ = b[15]
	binary.BigEndian.PutUint32(b[0:4], u.Data1)
	binary.BigEndian.PutUint32(b[4:8], u.Data2)
	binary.BigEndian.PutUint32(b[8:12], u.Data3)
	binary.BigEndian.PutUint32(b[12:16], u.Data4)
}

// Koly is the UDIF resource trailer that lives in the last 512 bytes of a
// DMG file. Field names mirror Apple's public hdiutil semantics but are
// renamed to idiomatic Go.
type Koly struct {
	Signature  uint32 // 'koly'
	Version    uint32 // 4
	HeaderSize uint32 // 512
	Flags      uint32

	RunningDataForkOffset uint64
	DataForkOffset        uint64
	DataForkLength        uint64
	RsrcForkOffset        uint64
	RsrcForkLength        uint64

	SegmentNumber uint32
	SegmentCount  uint32
	SegmentID     UDIFID

	DataForkChecksum UDIFChecksum

	XMLOffset uint64
	XMLLength uint64

	// 120 reserved bytes between XMLLength and MasterChecksum.
	Reserved1 [120]byte

	MasterChecksum UDIFChecksum

	ImageVariant uint32
	SectorCount  uint64

	Reserved2 uint32
	Reserved3 uint32
	Reserved4 uint32
}

// Encode serializes k into the standard 512-byte UDIF trailer form.
func (k *Koly) Encode() [KolySize]byte {
	var out [KolySize]byte
	binary.BigEndian.PutUint32(out[0:4], k.Signature)
	binary.BigEndian.PutUint32(out[4:8], k.Version)
	binary.BigEndian.PutUint32(out[8:12], k.HeaderSize)
	binary.BigEndian.PutUint32(out[12:16], k.Flags)
	binary.BigEndian.PutUint64(out[16:24], k.RunningDataForkOffset)
	binary.BigEndian.PutUint64(out[24:32], k.DataForkOffset)
	binary.BigEndian.PutUint64(out[32:40], k.DataForkLength)
	binary.BigEndian.PutUint64(out[40:48], k.RsrcForkOffset)
	binary.BigEndian.PutUint64(out[48:56], k.RsrcForkLength)
	binary.BigEndian.PutUint32(out[56:60], k.SegmentNumber)
	binary.BigEndian.PutUint32(out[60:64], k.SegmentCount)
	k.SegmentID.encode(out[64:80])
	k.DataForkChecksum.encode(out[80:216])
	binary.BigEndian.PutUint64(out[216:224], k.XMLOffset)
	binary.BigEndian.PutUint64(out[224:232], k.XMLLength)
	copy(out[232:352], k.Reserved1[:])
	k.MasterChecksum.encode(out[352:488])
	binary.BigEndian.PutUint32(out[488:492], k.ImageVariant)
	binary.BigEndian.PutUint64(out[492:500], k.SectorCount)
	binary.BigEndian.PutUint32(out[500:504], k.Reserved2)
	binary.BigEndian.PutUint32(out[504:508], k.Reserved3)
	binary.BigEndian.PutUint32(out[508:512], k.Reserved4)
	return out
}

// WriteTo writes the trailer to w. Always writes exactly [KolySize] bytes.
func (k *Koly) WriteTo(w io.Writer) (int64, error) {
	buf := k.Encode()
	n, err := w.Write(buf[:])
	return int64(n), err
}

// ErrShortKoly is returned by [DecodeKoly] when given fewer than 512 bytes.
var ErrShortKoly = errors.New("udif: koly trailer must be exactly 512 bytes")

// DecodeKoly parses the trailer from buf. Used by tests to round-trip.
func DecodeKoly(buf []byte) (*Koly, error) {
	if len(buf) < KolySize {
		return nil, ErrShortKoly
	}
	var k Koly
	k.Signature = binary.BigEndian.Uint32(buf[0:4])
	k.Version = binary.BigEndian.Uint32(buf[4:8])
	k.HeaderSize = binary.BigEndian.Uint32(buf[8:12])
	k.Flags = binary.BigEndian.Uint32(buf[12:16])
	k.RunningDataForkOffset = binary.BigEndian.Uint64(buf[16:24])
	k.DataForkOffset = binary.BigEndian.Uint64(buf[24:32])
	k.DataForkLength = binary.BigEndian.Uint64(buf[32:40])
	k.RsrcForkOffset = binary.BigEndian.Uint64(buf[40:48])
	k.RsrcForkLength = binary.BigEndian.Uint64(buf[48:56])
	k.SegmentNumber = binary.BigEndian.Uint32(buf[56:60])
	k.SegmentCount = binary.BigEndian.Uint32(buf[60:64])
	k.SegmentID.Data1 = binary.BigEndian.Uint32(buf[64:68])
	k.SegmentID.Data2 = binary.BigEndian.Uint32(buf[68:72])
	k.SegmentID.Data3 = binary.BigEndian.Uint32(buf[72:76])
	k.SegmentID.Data4 = binary.BigEndian.Uint32(buf[76:80])
	decodeChecksum(&k.DataForkChecksum, buf[80:216])
	k.XMLOffset = binary.BigEndian.Uint64(buf[216:224])
	k.XMLLength = binary.BigEndian.Uint64(buf[224:232])
	copy(k.Reserved1[:], buf[232:352])
	decodeChecksum(&k.MasterChecksum, buf[352:488])
	k.ImageVariant = binary.BigEndian.Uint32(buf[488:492])
	k.SectorCount = binary.BigEndian.Uint64(buf[492:500])
	k.Reserved2 = binary.BigEndian.Uint32(buf[500:504])
	k.Reserved3 = binary.BigEndian.Uint32(buf[504:508])
	k.Reserved4 = binary.BigEndian.Uint32(buf[508:512])
	return &k, nil
}

func decodeChecksum(c *UDIFChecksum, b []byte) {
	c.Type = binary.BigEndian.Uint32(b[0:4])
	c.Size = binary.BigEndian.Uint32(b[4:8])
	for i := 0; i < 32; i++ {
		c.Data[i] = binary.BigEndian.Uint32(b[8+i*4 : 8+i*4+4])
	}
}
