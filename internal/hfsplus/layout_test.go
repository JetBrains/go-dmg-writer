package hfsplus

import "testing"

// rootOnly is the smallest legal entry table: a volume still needs its
// root folder even when nothing else is on it.
func rootOnly() []*Entry {
	return []*Entry{{
		CNID:       CNIDRootFolder,
		ParentCNID: CNIDRootParent,
		Kind:       KindFolder,
	}}
}

// checkPlanInvariants asserts the things every plan has to satisfy no
// matter how the inputs are shaped.
func checkPlanInvariants(t *testing.T, p *Plan) {
	t.Helper()

	// The bitmap is written as a single run at AllocStartBlock, so it
	// has to fit the fork the plan gave it.
	forkBytes := uint64(p.AllocFile.TotalBlocks) * uint64(p.BlockSize)
	if got := uint64(len(p.AllocBitmap.Bytes())); got > forkBytes {
		t.Errorf("allocation bitmap is %d bytes but its fork holds only %d; the surplus would overwrite the catalog at block %d",
			got, forkBytes, p.CatalogStartBlock)
	}
	// The bitmap has to describe the whole volume, or fsck reads blocks
	// past its end as free.
	if p.AllocBitmap.Bits() != p.TotalBlocks {
		t.Errorf("bitmap covers %d blocks, volume has %d", p.AllocBitmap.Bits(), p.TotalBlocks)
	}
	// No fork may run past the end of the volume.
	if end := p.UserDataStart; end > p.TotalBlocks {
		t.Errorf("special files end at block %d, past the volume's %d blocks", end, p.TotalBlocks)
	}
	// fsck_hfs caps a B-tree clumpSize at a quarter of the volume.
	for _, f := range []struct {
		name string
		fork ForkData
	}{
		{"catalog", p.CatalogFile},
		{"extents", p.ExtentsFile},
		{"attributes", p.AttributesFile},
	} {
		if 4*f.fork.TotalBlocks > p.TotalBlocks {
			t.Errorf("%s fork is %d blocks, more than a quarter of the volume's %d",
				f.name, f.fork.TotalBlocks, p.TotalBlocks)
		}
	}
}

// TestBuildPlanBitmapFitsItsFork pins the overrun that used to happen
// when a big catalog sat next to little file data. The bitmap was sized
// from the pre-pad total, the clump-size rule then raised totalBlocks to
// 4*catalogBlocks, and the extra bitmap bytes landed in the catalog's
// header node.
func TestBuildPlanBitmapFitsItsFork(t *testing.T) {
	const blockSize = 4096
	// A 40 MiB catalog with no user files: totalBlocks is driven
	// entirely by the clump-size pad to 4*10240 = 40960 blocks, which
	// needs a 5120-byte bitmap. One block only holds 4096.
	p, err := BuildPlan(blockSize, rootOnly(), nil, 40*1024*1024, 0, 0)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	checkPlanInvariants(t, p)
	if p.AllocFile.TotalBlocks < 2 {
		t.Errorf("AllocFile.TotalBlocks = %d; a %d-block volume needs more than one bitmap block",
			p.AllocFile.TotalBlocks, p.TotalBlocks)
	}
}

// TestBuildPlanInvariantsAcrossShapes sweeps the plan over the shapes
// that move the bitmap-size fixed point: a catalog that dominates, file
// data that dominates, and volumes either side of the one-bitmap-block
// boundary (32768 blocks at a 4 KiB block size).
func TestBuildPlanInvariantsAcrossShapes(t *testing.T) {
	tests := []struct {
		name      string
		blockSize uint32
		catalog   uint64
		files     []UserFile
	}{
		{"empty volume", 4096, 0, nil},
		{"tiny catalog", 4096, 8192, nil},
		{"catalog dominates", 4096, 40 * 1024 * 1024, nil},
		{"catalog dominates, one small file", 4096, 40 * 1024 * 1024, sizedFiles(4096)},
		{"just under one bitmap block", 4096, 0, sizedFiles(32000 * 4096)},
		{"just over one bitmap block", 4096, 0, sizedFiles(33000 * 4096)},
		{"file data dominates", 4096, 16384, sizedFiles(512 * 1024 * 1024)},
		{"512-byte blocks", 512, 4 * 1024 * 1024, sizedFiles(8 * 1024 * 1024)},
		{"64 KiB blocks", 65536, 4 * 1024 * 1024, sizedFiles(64 * 1024 * 1024)},
		{"many files", 4096, 1024 * 1024, sizedFiles(4096, 8192, 0, 1, 1024*1024)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := rootOnly()
			for _, f := range tt.files {
				entries = append(entries, f.Entry)
			}
			p, err := BuildPlan(tt.blockSize, entries, tt.files, tt.catalog, 0, 0)
			if err != nil {
				t.Fatalf("BuildPlan: %v", err)
			}
			checkPlanInvariants(t, p)
		})
	}
}

// TestBuildPlanRejectsOversizedVolume covers the block accounting: a set
// of files whose blocks together exceed a uint32 has to be an error, not
// a wrapped total that lays the files back over the volume header.
func TestBuildPlanRejectsOversizedVolume(t *testing.T) {
	const blockSize = 4096
	// Three files of 2^31 blocks each: every one fits a uint32 on its
	// own, the sum does not.
	const each = uint64(1) << 31 * blockSize
	files := sizedFiles(each, each, each)
	entries := rootOnly()
	for _, f := range files {
		entries = append(entries, f.Entry)
	}
	if _, err := BuildPlan(blockSize, entries, files, 4096, 0, 0); err == nil {
		t.Fatal("BuildPlan accepted a volume needing more than 2^32 allocation blocks")
	}
}

// sizedFiles builds a UserFile per size, each with its own entry, which
// is all BuildPlan looks at.
func sizedFiles(sizes ...uint64) []UserFile {
	out := make([]UserFile, len(sizes))
	for i, s := range sizes {
		out[i] = UserFile{
			Entry: &Entry{CNID: CNIDFirstUser + uint32(i), ParentCNID: CNIDRootFolder, Kind: KindFile},
			Size:  s,
		}
	}
	return out
}
