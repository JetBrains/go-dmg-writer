package hfsplus

// AllocationBitmap is the HFS+ "allocation file": one bit per allocation
// block, MSB = lowest block (TN1150 § "Allocation File").
//
// We store the bitmap in memory, mark blocks used during layout planning,
// then write the full bitmap into the volume image once.
type AllocationBitmap struct {
	bytes []byte
	bits  uint32 // total number of blocks the bitmap covers
}

// NewAllocationBitmap returns a bitmap covering totalBlocks blocks, with
// all blocks initially free.
//
// The byte slice is sized to ceil(totalBlocks/8) bytes; the trailing bits
// past totalBlocks must remain zero.
func NewAllocationBitmap(totalBlocks uint32) *AllocationBitmap {
	n := (totalBlocks + 7) / 8
	return &AllocationBitmap{bytes: make([]byte, n), bits: totalBlocks}
}

// MarkUsed flips bits [start, start+count) to "used".
func (a *AllocationBitmap) MarkUsed(start, count uint32) {
	for i := uint32(0); i < count; i++ {
		b := start + i
		if b >= a.bits {
			break
		}
		a.bytes[b/8] |= 1 << (7 - (b % 8))
	}
}

// FreeBlocks returns the number of bits still cleared.
func (a *AllocationBitmap) FreeBlocks() uint32 {
	var used uint32
	for _, b := range a.bytes {
		used += uint32(popcount8(b))
	}
	return a.bits - used
}

// Bytes returns the raw on-disk bitmap (length ceil(totalBlocks/8) bytes).
func (a *AllocationBitmap) Bytes() []byte { return a.bytes }

// Bits returns the total number of allocation blocks the bitmap covers.
func (a *AllocationBitmap) Bits() uint32 { return a.bits }

func popcount8(b byte) int {
	b = (b & 0x55) + ((b >> 1) & 0x55)
	b = (b & 0x33) + ((b >> 2) & 0x33)
	return int((b & 0x0F) + ((b >> 4) & 0x0F))
}

// LogicalToExtent fills the first slot of an [ExtentRecord] with a single
// contiguous extent. Special files in the MVP always fit in one extent.
func LogicalToExtent(startBlock, blockCount uint32) ExtentRecord {
	var er ExtentRecord
	er[0] = ExtentDescriptor{StartBlock: startBlock, BlockCount: blockCount}
	return er
}

// DivRoundUp returns ceil(n/d). d must be non-zero.
func DivRoundUp(n, d uint64) uint64 {
	return (n + d - 1) / d
}
