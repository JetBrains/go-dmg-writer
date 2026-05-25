package udif

import (
	"hash"
	"hash/crc32"
)

// CRC32Hasher wraps the stdlib IEEE CRC32 hasher with a tiny convenience
// layer for the UDIF use case: it implements [hash.Hash32] so callers can
// use [hash.Hash32.Sum32] and the standard [io.Writer] interface for
// streaming.
//
// CRC32-IEEE is the polynomial used by both the data-fork checksum and the
// per-BLKX-table checksum in UDIF.
type CRC32Hasher struct {
	hash.Hash32
}

// NewCRC32 returns a fresh CRC32-IEEE hasher.
func NewCRC32() *CRC32Hasher {
	return &CRC32Hasher{Hash32: crc32.NewIEEE()}
}

// AsUDIFChecksum returns a [UDIFChecksum] filled with the current sum.
func (c *CRC32Hasher) AsUDIFChecksum() UDIFChecksum {
	ck := UDIFChecksum{Type: ChecksumCRC32, Size: 32}
	ck.Data[0] = c.Sum32()
	return ck
}

// MasterChecksum computes the UDIF "master checksum" from a sequence of
// per-BLKX-table data-fork checksums. The master checksum is the CRC32 of
// the concatenation of each table's 4-byte data checksum (big-endian).
func MasterChecksum(perTable []uint32) uint32 {
	h := crc32.NewIEEE()
	var buf [4]byte
	for _, v := range perTable {
		buf[0] = byte(v >> 24)
		buf[1] = byte(v >> 16)
		buf[2] = byte(v >> 8)
		buf[3] = byte(v)
		_, _ = h.Write(buf[:])
	}
	return h.Sum32()
}
