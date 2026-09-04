package udif

import (
	"encoding/binary"
)

// MishSignature is the magic at the start of a BLKX/mish table ("mish").
const MishSignature uint32 = 0x6D697368

// Block run types recognized by macOS for entries in a BLKX table.
//
// Note on zero runs: 0x00000000 and 0x00000002 are NOT synonyms, and the
// difference decides the checksum. hdiutil emits both, and it feeds the
// sectors of a 0x00000000 run into the table's CRC-32 as zeros while it
// skips the sectors of a 0x00000002 run entirely. A writer that emits
// 0x00000002 and then counts those sectors produces a checksum hdiutil
// rejects. [BlkxTable.Checksum] says which sectors a table's CRC covers.
const (
	BlockZeroFill   uint32 = 0x00000000 // all-zero, CompLength=0, counted in the CRC
	BlockRaw        uint32 = 0x00000001 // chunk is stored verbatim
	BlockZero       uint32 = 0x00000002 // free/ignore, CompLength=0, NOT counted in the CRC
	BlockADC        uint32 = 0x80000004 // Apple Data Compression (not used)
	BlockZLIB       uint32 = 0x80000005 // zlib-compressed chunk
	BlockBZIP2      uint32 = 0x80000006 // bzip2-compressed (not used)
	BlockLZFSE      uint32 = 0x80000007 // LZFSE (not used)
	BlockComment    uint32 = 0x7FFFFFFE
	BlockTerminator uint32 = 0xFFFFFFFF
)

// BlockRun is one entry in a BLKX table. SectorStart/SectorCount address
// logical 512-byte sectors inside the partition; CompOffset/CompLength
// address bytes inside the data fork on disk.
type BlockRun struct {
	Type        uint32
	Reserved    uint32
	SectorStart uint64
	SectorCount uint64
	CompOffset  uint64
	CompLength  uint64
}

// blockRunSize is the on-disk size of a single BlockRun (40 bytes).
const blockRunSize = 40

func (r *BlockRun) encode(b []byte) {
	_ = b[blockRunSize-1]
	binary.BigEndian.PutUint32(b[0:4], r.Type)
	binary.BigEndian.PutUint32(b[4:8], r.Reserved)
	binary.BigEndian.PutUint64(b[8:16], r.SectorStart)
	binary.BigEndian.PutUint64(b[16:24], r.SectorCount)
	binary.BigEndian.PutUint64(b[24:32], r.CompOffset)
	binary.BigEndian.PutUint64(b[32:40], r.CompLength)
}

// BlkxTable is the in-memory representation of a single BLKX (mish)
// resource. One table describes how every sector of the partition is laid
// out inside the DMG's data fork.
type BlkxTable struct {
	Signature              uint32 // 'mish'
	InfoVersion            uint32 // 1
	FirstSectorNumber      uint64
	SectorCount            uint64
	DataStart              uint64
	DecompressBufRequested uint32 // largest run's uncompressed size in bytes
	BlocksDescriptor       uint32 // partition number (or ENTIRE_DEVICE)
	Reserved               [6]uint32

	// Checksum is the CRC-32 of this table's *uncompressed* sectors, in
	// sector order, with the sectors of every [BlockZero] run left out.
	// See the note on the run types for why those sectors do not count.
	Checksum UDIFChecksum

	Runs []BlockRun
}

// blkxHeaderSize is the on-disk size of a BlkxTable excluding the Runs
// payload and excluding the embedded UDIFChecksum.
const blkxHeaderSize = 4 + 4 + 8 + 8 + 8 + 4 + 4 + 6*4 + 136 + 4

// Encode serializes the BLKX table to its on-disk form.
func (t *BlkxTable) Encode() []byte {
	out := make([]byte, blkxHeaderSize+len(t.Runs)*blockRunSize)
	binary.BigEndian.PutUint32(out[0:4], t.Signature)
	binary.BigEndian.PutUint32(out[4:8], t.InfoVersion)
	binary.BigEndian.PutUint64(out[8:16], t.FirstSectorNumber)
	binary.BigEndian.PutUint64(out[16:24], t.SectorCount)
	binary.BigEndian.PutUint64(out[24:32], t.DataStart)
	binary.BigEndian.PutUint32(out[32:36], t.DecompressBufRequested)
	binary.BigEndian.PutUint32(out[36:40], t.BlocksDescriptor)
	for i, r := range t.Reserved {
		binary.BigEndian.PutUint32(out[40+i*4:40+i*4+4], r)
	}
	t.Checksum.encode(out[64:200])
	binary.BigEndian.PutUint32(out[200:204], uint32(len(t.Runs)))
	for i, run := range t.Runs {
		off := blkxHeaderSize + i*blockRunSize
		run.encode(out[off : off+blockRunSize])
	}
	return out
}
