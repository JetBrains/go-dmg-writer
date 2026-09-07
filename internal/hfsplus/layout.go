package hfsplus

import "fmt"

// SpecialNodeSizes are the per-tree default node sizes hdiutil uses. They
// also match what fsck_hfs expects; deviating is supported by the format
// but invites compatibility headaches.
const (
	CatalogNodeSize    uint16 = 4096
	ExtentsNodeSize    uint16 = 1024
	AttributesNodeSize uint16 = 8192
)

// DefaultBlockSize is the HFS+ allocation block size we use unless the
// caller overrides it.
const DefaultBlockSize uint32 = 4096

// UserFile pairs an Entry with the source of its data-fork bytes. The
// builder consumes this struct when streaming user data into the volume
// image.
type UserFile struct {
	Entry *Entry
	// Size is the logical size of the data fork in bytes (already
	// reflected in Entry.DataLogicalSize after planning).
	Size uint64
}

// Plan is the fully-resolved volume layout, ready to be written.
type Plan struct {
	BlockSize       uint32
	TotalBlocks     uint32
	NextAllocation  uint32 // first block of free space (== TotalBlocks-1 for a packed volume)
	EncodingsBitmap uint64
	FileCount       uint32
	FolderCount     uint32
	NextCatalogID   uint32

	AllocFile      ForkData
	CatalogFile    ForkData
	ExtentsFile    ForkData
	AttributesFile ForkData

	AllocStartBlock      uint32
	CatalogStartBlock    uint32
	ExtentsStartBlock    uint32
	AttributesStartBlock uint32

	UserDataStart uint32 // first block holding user file data

	AllocBitmap *AllocationBitmap

	// Per-user-file placement, in the same order as the input slice
	// supplied to BuildPlan.
	Placements []FilePlacement
}

// FilePlacement records where in the volume a single file's data lives.
type FilePlacement struct {
	StartBlock uint32
	Blocks     uint32
}

// forkBlocks converts a special file's byte size into an allocation
// block count. Every special file gets at least one block, even an empty
// one, because a fork with no extent has nowhere to put its B-tree
// header node. A count that does not fit the uint32 the on-disk fork
// record uses is an error rather than a wrapped value.
func forkBlocks(name string, size uint64, blockSize uint32) (uint32, error) {
	n := DivRoundUp(size, uint64(blockSize))
	if n == 0 {
		n = 1
	}
	if n > uint64(^uint32(0)) {
		return 0, fmt.Errorf("hfsplus: %s file needs %d allocation blocks, which exceeds the %d-block maximum",
			name, n, uint64(^uint32(0)))
	}
	return uint32(n), nil
}

// BuildPlan computes the volume layout from a set of entries, user files,
// and the sizes of the packed special files (catalog, extents, attributes).
// The function does not allocate I/O; it just decides offsets.
//
// catalogTreeSize/extentsTreeSize/attributesTreeSize are in bytes and
// must be already padded to whole blocks by the caller's tree builder
// (they will be padded up here regardless, so passing the exact tree
// byte count is fine).
func BuildPlan(
	blockSize uint32,
	entries []*Entry,
	files []UserFile,
	catalogTreeSize, extentsTreeSize, attributesTreeSize uint64,
) (*Plan, error) {
	if blockSize == 0 {
		blockSize = DefaultBlockSize
	}
	if blockSize < 512 || blockSize&(blockSize-1) != 0 {
		return nil, fmt.Errorf("hfsplus: blockSize %d must be a power of two >= 512", blockSize)
	}

	catalogBlocks, err := forkBlocks("catalog", catalogTreeSize, blockSize)
	if err != nil {
		return nil, err
	}
	extentsBlocks, err := forkBlocks("extents overflow", extentsTreeSize, blockSize)
	if err != nil {
		return nil, err
	}
	attrsBlocks, err := forkBlocks("attributes", attributesTreeSize, blockSize)
	if err != nil {
		return nil, err
	}

	// Compute per-file block counts and total user data blocks.
	//
	// The single-extent invariant: every file is laid out as ONE
	// contiguous run of blocks (the extents-overflow tree is left
	// empty by [BuildExtentsTree]). A single ExtentDescriptor uses
	// 32-bit BlockCount, so the practical per-file ceiling is
	// 0xFFFFFFFF blocks (~16 TiB at 4 KiB blocks). Reject anything
	// over that explicitly so the contract is visible.
	placements := make([]FilePlacement, len(files))
	// Summed as a uint64: a set of files whose block counts together
	// exceed a uint32 has to be an error, not a wrapped total that
	// silently lays every file on top of the volume header.
	var userBlocks uint64
	for i, f := range files {
		blks64 := DivRoundUp(f.Size, uint64(blockSize))
		if blks64 > uint64(^uint32(0)) {
			return nil, fmt.Errorf("hfsplus: file size %d (%d blocks) exceeds single-extent maximum %d blocks",
				f.Size, blks64, ^uint32(0))
		}
		placements[i].Blocks = uint32(blks64)
		userBlocks += blks64
	}

	// fsck_hfs caps each B-tree fork's clumpSize at totalBlocks/4 *
	// blockSize ("max clump = 1/4 volume size", SVerify1.c). For tiny
	// volumes the B-tree forks themselves can exceed that, which would
	// force fsck to fall back to a smaller fcbClumpSize and then complain
	// that VH.<fork>.clumpSize doesn't match. Pad the trailing free
	// region so totalBlocks >= 4*max(btree fork blocks).
	maxBTreeBlocks := uint64(catalogBlocks)
	if uint64(extentsBlocks) > maxBTreeBlocks {
		maxBTreeBlocks = uint64(extentsBlocks)
	}
	if uint64(attrsBlocks) > maxBTreeBlocks {
		maxBTreeBlocks = uint64(attrsBlocks)
	}
	minTotal := 4 * maxBTreeBlocks

	// Block 0 is reserved (boot + primary VH).
	// One trailing block is reserved (alternate VH).
	//
	// The allocation bitmap lives inside the volume it describes, so its
	// size feeds back into the total, and we iterate to a fixed point.
	// The clump-size pad has to be applied INSIDE that loop: growing the
	// volume after the bitmap is sized leaves the bitmap too small for
	// it, and the surplus bytes then overrun the fork into the catalog
	allocBlocks64 := uint64(1)
	var totalBlocks64 uint64
	for {
		total := 1 + allocBlocks64 + uint64(catalogBlocks) + uint64(extentsBlocks) +
			uint64(attrsBlocks) + userBlocks + 1
		if total < minTotal {
			total = minTotal
		}
		need := DivRoundUp(total, uint64(blockSize)*8)
		if need <= allocBlocks64 {
			totalBlocks64 = total
			break
		}
		allocBlocks64 = need
	}
	if totalBlocks64 > uint64(^uint32(0)) {
		return nil, fmt.Errorf("hfsplus: volume needs %d allocation blocks of %d bytes, which exceeds the %d-block maximum of an HFS+ volume",
			totalBlocks64, blockSize, uint64(^uint32(0)))
	}
	totalBlocks := uint32(totalBlocks64)
	allocBlocks := uint32(allocBlocks64)

	allocStart := uint32(1)
	catStart := allocStart + allocBlocks
	extStart := catStart + catalogBlocks
	attrStart := extStart + extentsBlocks
	userStart := attrStart + attrsBlocks

	// Place each user file sequentially starting at userStart.
	cursor := userStart
	for i, f := range files {
		placements[i].StartBlock = cursor
		// Update the entry's data fork now so the catalog packer sees
		// the real extent value before writing.
		if placements[i].Blocks == 0 {
			placements[i].StartBlock = 0
			f.Entry.DataExtents = ExtentRecord{}
		} else {
			f.Entry.DataExtents = LogicalToExtent(cursor, placements[i].Blocks)
		}
		f.Entry.DataTotalBlocks = placements[i].Blocks
		f.Entry.DataLogicalSize = f.Size
		cursor += placements[i].Blocks
	}

	// Build the allocation bitmap: every block used except the user-data
	// region beyond `cursor` (none in our packed layout - we touch every
	// block from 0 up to and including the trailing alt-VH block).
	bm := NewAllocationBitmap(totalBlocks)
	bm.MarkUsed(0, 1)                    // boot + primary VH
	bm.MarkUsed(allocStart, allocBlocks) // allocation bitmap itself
	bm.MarkUsed(catStart, catalogBlocks) // catalog
	bm.MarkUsed(extStart, extentsBlocks) // extents overflow
	bm.MarkUsed(attrStart, attrsBlocks)  // attributes
	for _, p := range placements {
		bm.MarkUsed(p.StartBlock, p.Blocks)
	}
	bm.MarkUsed(totalBlocks-1, 1) // alternate VH

	// fileCount/folderCount come straight from the entries; the root
	// folder is NOT counted in folderCount per TN1150.
	var fileCount, folderCount uint32
	var maxCNID uint32
	for _, e := range entries {
		if e.CNID == CNIDRootParent {
			continue
		}
		if e.CNID > maxCNID {
			maxCNID = e.CNID
		}
		switch e.Kind {
		case KindFolder:
			if e.CNID != CNIDRootFolder {
				folderCount++
			}
		case KindFile, KindSymlink:
			fileCount++
		}
	}

	// EncodingsBitmap is a hint that tells legacy mounters which
	// text-encoding tables the catalog references. Apple's hdiutil
	// always sets bit 0 (MacRoman) regardless of which scripts the
	// names actually use, because on HFSX the per-record TextEncoding
	// field is authoritative. We do the same: bit 0 is always set,
	// other bits are always clear.
	const encodings uint64 = 1

	// NextAllocation points at the first free block (first block past
	// the user-data region). For a packed layout with padding this is
	// `cursor`; if the cursor happens to land on the alt-VH block, it wraps
	// to zero per TN1150 ("undefined" but conventionally 0).
	nextAlloc := cursor
	if nextAlloc >= totalBlocks-1 {
		nextAlloc = 0
	}
	return &Plan{
		BlockSize:       blockSize,
		TotalBlocks:     totalBlocks,
		NextAllocation:  nextAlloc,
		EncodingsBitmap: encodings,
		FileCount:       fileCount,
		FolderCount:     folderCount,
		NextCatalogID:   maxCNID + 1,
		// fsck_hfs enforces TWO invariants on B-tree fork clumpSizes
		// (catalog/extents/attributes):
		//   1. VH.<fork>.clumpSize == totalBlocks * blockSize  (= logSize)
		//      ("Invalid file clump size" otherwise)
		//   2. VH.<fork>.clumpSize == BTHeaderRec.clumpSize     ("invalid VHB
		//      <fork>File.clumpSize" otherwise)
		// We satisfy both by setting clumpSize to the actual on-disk fork
		// size everywhere. BuildResult.PadToBlocks rewrites the inner
		// BTHeaderRec.clumpSize to match.
		AllocFile: ForkData{
			LogicalSize: uint64(allocBlocks) * uint64(blockSize),
			ClumpSize:   allocBlocks * blockSize,
			TotalBlocks: allocBlocks,
			Extents:     LogicalToExtent(allocStart, allocBlocks),
		},
		CatalogFile: ForkData{
			LogicalSize: uint64(catalogBlocks) * uint64(blockSize),
			ClumpSize:   catalogBlocks * blockSize,
			TotalBlocks: catalogBlocks,
			Extents:     LogicalToExtent(catStart, catalogBlocks),
		},
		ExtentsFile: ForkData{
			LogicalSize: uint64(extentsBlocks) * uint64(blockSize),
			ClumpSize:   extentsBlocks * blockSize,
			TotalBlocks: extentsBlocks,
			Extents:     LogicalToExtent(extStart, extentsBlocks),
		},
		AttributesFile: ForkData{
			LogicalSize: uint64(attrsBlocks) * uint64(blockSize),
			ClumpSize:   attrsBlocks * blockSize,
			TotalBlocks: attrsBlocks,
			Extents:     LogicalToExtent(attrStart, attrsBlocks),
		},
		AllocStartBlock:      allocStart,
		CatalogStartBlock:    catStart,
		ExtentsStartBlock:    extStart,
		AttributesStartBlock: attrStart,
		UserDataStart:        userStart,
		AllocBitmap:          bm,
		Placements:           placements,
	}, nil
}
