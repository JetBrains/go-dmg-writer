package hfsplus

import "testing"

func TestAllocationBitmap(t *testing.T) {
	bm := NewAllocationBitmap(100)
	if bm.FreeBlocks() != 100 {
		t.Errorf("initial FreeBlocks: got %d want 100", bm.FreeBlocks())
	}
	bm.MarkUsed(0, 1)
	bm.MarkUsed(50, 10)
	bm.MarkUsed(99, 1)
	if bm.FreeBlocks() != 100-12 {
		t.Errorf("FreeBlocks after marking: got %d want %d", bm.FreeBlocks(), 88)
	}
	// Bit 0 = high bit of byte 0.
	if bm.Bytes()[0]&0x80 == 0 {
		t.Errorf("bit 0 not set")
	}
	// Bit 99 = bit 3 of byte 12 (99/8 = 12 rem 3).
	if bm.Bytes()[12]&(1<<(7-3)) == 0 {
		t.Errorf("bit 99 not set")
	}
}

func TestAllocationOverflow(t *testing.T) {
	// Mark past totalBlocks; should be a no-op without panic.
	bm := NewAllocationBitmap(8)
	bm.MarkUsed(7, 100)
	if bm.FreeBlocks() != 7 {
		t.Errorf("got %d want 7", bm.FreeBlocks())
	}
}

func TestDivRoundUp(t *testing.T) {
	cases := []struct{ n, d, want uint64 }{
		{0, 4, 0},
		{1, 4, 1},
		{4, 4, 1},
		{5, 4, 2},
		{8, 4, 2},
		{9, 4, 3},
	}
	for _, c := range cases {
		got := DivRoundUp(c.n, c.d)
		if got != c.want {
			t.Errorf("DivRoundUp(%d,%d): got %d want %d", c.n, c.d, got, c.want)
		}
	}
}
