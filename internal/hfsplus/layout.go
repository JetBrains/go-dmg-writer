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

	catalogBlocks := uint32(DivRoundUp(catalogTreeSize, uint64(blockSize)))
	if catalogBlocks == 0 {
		catalogBlocks = 1
	}
	extentsBlocks := uint32(DivRoundUp(extentsTreeSize, uint64(blockSize)))
	if extentsBlocks == 0 {
		extentsBlocks = 1
	}
	attrsBlocks := uint32(DivRoundUp(attributesTreeSize, uint64(blockSize)))
	if attrsBlocks == 0 {
		attrsBlocks = 1
	}

	// Compute per-file block counts and total user data blocks.
	placements := make([]FilePlacement, len(files))
	var userBlocks uint32
	for i, f := range files {
		blks := uint32(DivRoundUp(f.Size, uint64(blockSize)))
		placements[i].Blocks = blks
		userBlocks += blks
	}

	// Block 0 is reserved (boot + primary VH).
	// One trailing block is reserved (alternate VH).
	// Allocation bitmap size depends on totalBlocks, so we iterate.
	allocBlocks := uint32(1)
	for {
		total := uint32(1) + allocBlocks + catalogBlocks + extentsBlocks + attrsBlocks + userBlocks + 1
		need := uint32(DivRoundUp(uint64(total), uint64(blockSize)*8))
		if need <= allocBlocks {
			break
		}
		allocBlocks = need
	}

	totalBlocks := uint32(1) + allocBlocks + catalogBlocks + extentsBlocks + attrsBlocks + userBlocks + 1

	// fsck_hfs caps each B-tree fork's clumpSize at totalBlocks/4 *
	// blockSize ("max clump = 1/4 volume size", SVerify1.c). For tiny
	// volumes the B-tree forks themselves can exceed that, which would
	// force fsck to fall back to a smaller fcbClumpSize and then complain
	// that VH.<fork>.clumpSize doesn't match. Pad the trailing free
	// region so totalBlocks >= 4*max(btree fork blocks).
	maxBTreeBlocks := catalogBlocks
	if extentsBlocks > maxBTreeBlocks {
		maxBTreeBlocks = extentsBlocks
	}
	if attrsBlocks > maxBTreeBlocks {
		maxBTreeBlocks = attrsBlocks
	}
	if minTotal := 4 * maxBTreeBlocks; totalBlocks < minTotal {
		totalBlocks = minTotal
	}

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
		f.Entry.DataExtents = LogicalToExtent(cursor, placements[i].Blocks)
		f.Entry.DataTotalBlocks = placements[i].Blocks
		f.Entry.DataLogicalSize = f.Size
		cursor += placements[i].Blocks
	}

	// Build the allocation bitmap: every block used except the user-data
	// region beyond `cursor` (none in our packed layout — we touch every
	// block from 0 up to and including the trailing alt-VH block).
	bm := NewAllocationBitmap(totalBlocks)
	bm.MarkUsed(0, 1)                          // boot + primary VH
	bm.MarkUsed(allocStart, allocBlocks)       // allocation bitmap itself
	bm.MarkUsed(catStart, catalogBlocks)       // catalog
	bm.MarkUsed(extStart, extentsBlocks)       // extents overflow
	bm.MarkUsed(attrStart, attrsBlocks)        // attributes
	for _, p := range placements {
		bm.MarkUsed(p.StartBlock, p.Blocks)
	}
	bm.MarkUsed(totalBlocks-1, 1) // alternate VH

	// fileCount/folderCount/encodingsBitmap come straight from the
	// entries; the root folder is NOT counted in folderCount per TN1150.
	var fileCount, folderCount uint32
	var encodings uint64
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
		encodings |= 1 << e.EncodingBit()
	}

	// NextAllocation points at the first free block (first block past
	// the user-data region). For a packed layout with padding this is
	// `cursor`; if cursor happens to land on the alt-VH block it wraps
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
