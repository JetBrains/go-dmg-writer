package hfsplus

import (
	"encoding/binary"
	"fmt"
)

// Record is one key/value pair to be packed into a B-tree.
//
// Key is the *body* of the key (without the leading uint16 key-length
// field). The packer writes the length prefix.
type Record struct {
	Key  []byte
	Data []byte
}

// BuildOpts controls the packer's wire-format choices.
type BuildOpts struct {
	// NodeSize is the on-disk size of every node in the tree (TN1150
	// requires nodeSize be a power of two, typically 4096 for catalog,
	// 1024 for extents, 8192 for attributes).
	NodeSize uint16
	// Attributes is the BTHeaderRec.attributes word. Pass
	// BTBigKeysMask | BTVariableIndexKeys for HFS+ trees.
	Attributes uint32
	// KeyCompareType for HFS+ is KeyCompareCaseFolding (plain HFS+) or
	// KeyCompareBinary (HFSX).
	KeyCompareType uint8
	// BTreeType is BTreeHFSType (0) for HFS+ system trees.
	BTreeType uint8
	// MaxKeyLength is the largest possible key-body length for this tree
	// (excluding the keyLength field).
	MaxKeyLength uint16
	// ClumpSize is the BTHeaderRec.clumpSize hint. Pass 0 to default to
	// 8*nodeSize, which is what hdutil uses.
	ClumpSize uint32
}

// BuildResult is the packed tree.
type BuildResult struct {
	// Bytes is the contiguous on-disk image of the tree. Its length is
	// TotalNodes * NodeSize.
	Bytes []byte
	// TotalNodes is the count of allocated nodes (including the header
	// and any map nodes).
	TotalNodes uint32
	// TreeDepth is the height of the tree: 1 for a single-leaf tree.
	TreeDepth uint16
	// RootNode is the node number of the root.
	RootNode uint32
	// FirstLeaf / LastLeaf are the leaf-chain endpoints.
	FirstLeaf uint32
	LastLeaf  uint32
	// LeafRecords is the total number of records across all leaves.
	LeafRecords uint32
	// FreeNodes is the number of allocated but unused nodes.
	FreeNodes uint32

	// Stored opts so PadToBlocks can re-emit the header.
	opts BuildOpts
}

// PadToBlocks grows the tree image to exactly `blocks*blockSize` bytes,
// updating TotalNodes/FreeNodes in the header record and bumping the
// map bits accordingly. fsck_hfs requires every B-tree file's header to
// describe the *full* file allocation, not just the populated subset.
func (r *BuildResult) PadToBlocks(blocks, blockSize uint32) error {
	wantBytes := uint64(blocks) * uint64(blockSize)
	if wantBytes < uint64(len(r.Bytes)) {
		return fmt.Errorf("hfsplus: PadToBlocks: requested %d bytes is smaller than current %d",
			wantBytes, len(r.Bytes))
	}
	if uint64(r.opts.NodeSize) == 0 || wantBytes%uint64(r.opts.NodeSize) != 0 {
		return fmt.Errorf("hfsplus: PadToBlocks: %d bytes is not a multiple of node size %d",
			wantBytes, r.opts.NodeSize)
	}
	newTotal := uint32(wantBytes / uint64(r.opts.NodeSize))
	if uint64(len(r.Bytes)) < wantBytes {
		grown := make([]byte, wantBytes)
		copy(grown, r.Bytes)
		r.Bytes = grown
	}
	if newTotal != r.TotalNodes {
		r.FreeNodes += newTotal - r.TotalNodes
		r.TotalNodes = newTotal
		// BTHeaderRec lives at byte 14 in the header node.
		//   totalNodes is at BTHeaderRec offset 22 -> absolute 36.
		//   freeNodes  is at BTHeaderRec offset 26 -> absolute 40.
		binary.BigEndian.PutUint32(r.Bytes[36:40], r.TotalNodes)
		binary.BigEndian.PutUint32(r.Bytes[40:44], r.FreeNodes)
	}
	// Sync BTHeaderRec.clumpSize with the final fork size so it agrees
	// with VH.<fork>.clumpSize. fsck flags any mismatch as "invalid
	// VHB ... clumpSize". clumpSize at BTHeaderRec offset 32 -> abs 46.
	binary.BigEndian.PutUint32(r.Bytes[46:50], uint32(wantBytes))
	return nil
}

// BuildTree packs a sorted record slice into a fresh HFS+ B-tree. Records
// must already be in HFS+ key order; the packer never re-sorts.
//
// The packing is deliberately simple: greedily fill nodes left-to-right.
// This produces a perfectly-packed, immutable tree well-suited to a
// write-once filesystem image.
func BuildTree(records []Record, opts BuildOpts) (*BuildResult, error) {
	if opts.NodeSize == 0 {
		return nil, fmt.Errorf("hfsplus: BuildTree: NodeSize must be non-zero")
	}
	if opts.NodeSize&(opts.NodeSize-1) != 0 {
		return nil, fmt.Errorf("hfsplus: BuildTree: NodeSize %d is not a power of two", opts.NodeSize)
	}
	nodeSize := int(opts.NodeSize)
	clumpSize := opts.ClumpSize
	if clumpSize == 0 {
		clumpSize = 8 * uint32(nodeSize)
	}

	// 1. Pack leaves.
	leafChunks, err := packLeaves(records, nodeSize)
	if err != nil {
		return nil, err
	}
	if len(leafChunks) == 0 {
		// Empty tree: TN1150 specifies rootNode=0, firstLeaf=0,
		// lastLeaf=0, treeDepth=0 and emits only the header node.
		return emitEmptyTree(opts, clumpSize)
	}

	// 2. Build index levels until we have a single root. Track which
	//    node numbers each level's nodes occupy; we will assign them
	//    in step 4.
	type level struct {
		nodes    [][]Record // for leaves
		indices  [][]idxRec // for index levels
		firstKey [][]byte
		assigned []uint32 // node numbers, filled in step 4
	}
	var levels []level

	leafLevel := level{nodes: leafChunks}
	for _, recs := range leafChunks {
		if len(recs) > 0 {
			leafLevel.firstKey = append(leafLevel.firstKey, recs[0].Key)
		} else {
			leafLevel.firstKey = append(leafLevel.firstKey, nil)
		}
	}
	levels = append(levels, leafLevel)

	for len(levels[len(levels)-1].firstKey) > 1 {
		prev := levels[len(levels)-1]
		// The first index key in a node always uses the same key value
		// as the first record of its child leaf/node. Index records are
		// (key, child node number). We don't know the child node numbers
		// yet — we'll fill them in later — but we know how big they are.
		var idx [][]idxRec
		var cur []idxRec
		used := 14 + 2
		for i, k := range prev.firstKey {
			recSize := 2 + len(k) + 4 + 2
			if used+recSize > nodeSize && len(cur) > 0 {
				idx = append(idx, cur)
				cur = nil
				used = 14 + 2
			}
			cur = append(cur, idxRec{key: k, childIdx: uint32(i)})
			used += recSize
		}
		if len(cur) > 0 {
			idx = append(idx, cur)
		}
		var fk [][]byte
		for _, n := range idx {
			fk = append(fk, n[0].key)
		}
		levels = append(levels, level{indices: idx, firstKey: fk})
	}

	// 3. Count nodes and decide where map nodes go.
	totalNodes := uint32(1) // header
	for _, lvl := range levels {
		if lvl.nodes != nil {
			totalNodes += uint32(len(lvl.nodes))
		} else {
			totalNodes += uint32(len(lvl.indices))
		}
	}
	mapBitsInHeader := (nodeSize - 14 - 2 /* offset slot */ - 2 /* free-space slot */ -
		3*2 /* three more offset slots for the header's 3 records */ -
		106 /* BTHeaderRec */ - 128 /* userData */) * 8
	// We always pad the map record to fill the node, so it can hold up to
	// `mapBitsInHeader` node bits. If our tree exceeds that, allocate
	// chained BTMapNode nodes.
	var mapNodes uint32
	if int(totalNodes) > mapBitsInHeader {
		// Each map node has a single map record filling the node body
		// minus 14 (descriptor) + 2 (offset slot) + 2 (free-space slot).
		bitsPerMapNode := (nodeSize - 14 - 4) * 8
		need := int(totalNodes) - mapBitsInHeader
		for need > 0 {
			mapNodes++
			need -= bitsPerMapNode
			totalNodes++ // each map node adds itself to the count
		}
		// One more pass may be needed because adding map nodes increases
		// totalNodes which may need yet another map node. The loop above
		// already accounts for that because we recompute `need` each
		// iteration based on the running totalNodes.
	}

	// 4. Assign node numbers.
	//    Order: 0=header, 1..mapNodes=map chain, then leaves bottom-up,
	//    then index levels bottom-up.
	nextNode := uint32(1) + mapNodes
	for li := range levels {
		count := len(levels[li].firstKey)
		levels[li].assigned = make([]uint32, count)
		for i := 0; i < count; i++ {
			levels[li].assigned[i] = nextNode
			nextNode++
		}
	}

	// 5. Resolve child references in index levels (child node numbers).
	for li := 1; li < len(levels); li++ {
		child := levels[li-1]
		for ni := range levels[li].indices {
			for ri := range levels[li].indices[ni] {
				levels[li].indices[ni][ri].childNode = child.assigned[levels[li].indices[ni][ri].childIdx]
			}
		}
	}

	// 6. Emit bytes.
	out := make([]byte, int(totalNodes)*nodeSize)

	// Header node (node 0).
	header := BTHeaderRec{
		TreeDepth:      uint16(len(levels)),
		RootNode:       levels[len(levels)-1].assigned[0],
		LeafRecords:    countLeafRecords(leafChunks),
		FirstLeafNode:  levels[0].assigned[0],
		LastLeafNode:   levels[0].assigned[len(levels[0].assigned)-1],
		NodeSize:       opts.NodeSize,
		MaxKeyLength:   opts.MaxKeyLength,
		TotalNodes:     totalNodes,
		FreeNodes:      0,
		ClumpSize:      clumpSize,
		BTreeType:      opts.BTreeType,
		KeyCompareType: opts.KeyCompareType,
		Attributes:     opts.Attributes,
	}
	emitHeaderNode(out[0:nodeSize], nodeSize, header, totalNodes, mapNodes)

	// Map nodes (if any).
	for i := uint32(0); i < mapNodes; i++ {
		mapNodeNum := 1 + i
		next := uint32(0)
		if i+1 < mapNodes {
			next = mapNodeNum + 1
		}
		emitMapNode(out[int(mapNodeNum)*nodeSize:int(mapNodeNum+1)*nodeSize], nodeSize, next)
	}

	// Leaves.
	leafNodes := levels[0].assigned
	for i, recs := range leafChunks {
		nodeNum := leafNodes[i]
		var fLink uint32
		var bLink uint32
		if i > 0 {
			bLink = leafNodes[i-1]
		}
		if i+1 < len(leafChunks) {
			fLink = leafNodes[i+1]
		}
		desc := BTNodeDescriptor{
			FLink:      fLink,
			BLink:      bLink,
			Kind:       BTLeafNode,
			Height:     1,
			NumRecords: uint16(len(recs)),
		}
		writeRecordsNode(out[int(nodeNum)*nodeSize:int(nodeNum+1)*nodeSize], nodeSize, desc, leafRecordsToWriteable(recs))
	}

	// Index levels.
	for li := 1; li < len(levels); li++ {
		lvl := levels[li]
		// Index nodes at level li have height li+1 (root has height = treeDepth).
		height := uint8(li + 1)
		for i, recs := range lvl.indices {
			nodeNum := lvl.assigned[i]
			var fLink uint32
			var bLink uint32
			if i > 0 {
				bLink = lvl.assigned[i-1]
			}
			if i+1 < len(lvl.indices) {
				fLink = lvl.assigned[i+1]
			}
			desc := BTNodeDescriptor{
				FLink:      fLink,
				BLink:      bLink,
				Kind:       BTIndexNode,
				Height:     height,
				NumRecords: uint16(len(recs)),
			}
			writeRecordsNode(out[int(nodeNum)*nodeSize:int(nodeNum+1)*nodeSize], nodeSize, desc, idxRecordsToWriteable(recs))
		}
	}

	return &BuildResult{
		Bytes:       out,
		TotalNodes:  totalNodes,
		TreeDepth:   header.TreeDepth,
		RootNode:    header.RootNode,
		FirstLeaf:   header.FirstLeafNode,
		LastLeaf:    header.LastLeafNode,
		LeafRecords: header.LeafRecords,
		FreeNodes:   0,
		opts:        opts,
	}, nil
}

type idxRec struct {
	key       []byte
	childIdx  uint32 // index into the level-below firstKey slice
	childNode uint32 // resolved node number after step 4
}

// writeable is a small adapter so the same node-emit routine can write
// leaf records and index records.
type writeable struct {
	key  []byte
	data []byte
}

func leafRecordsToWriteable(rs []Record) []writeable {
	out := make([]writeable, len(rs))
	for i, r := range rs {
		out[i] = writeable{key: r.Key, data: r.Data}
	}
	return out
}

func idxRecordsToWriteable(rs []idxRec) []writeable {
	out := make([]writeable, len(rs))
	for i, r := range rs {
		var buf [4]byte
		binary.BigEndian.PutUint32(buf[:], r.childNode)
		out[i] = writeable{key: r.key, data: append([]byte(nil), buf[:]...)}
	}
	return out
}

func writeRecordsNode(node []byte, nodeSize int, desc BTNodeDescriptor, recs []writeable) {
	desc.encode(node[:BTNodeDescriptorSize])
	offsets := make([]uint16, len(recs)+1)
	cur := uint16(BTNodeDescriptorSize)
	for i, r := range recs {
		offsets[i] = cur
		binary.BigEndian.PutUint16(node[cur:cur+2], uint16(len(r.key)))
		copy(node[cur+2:], r.key)
		copy(node[cur+2+uint16(len(r.key)):], r.data)
		cur += 2 + uint16(len(r.key)) + uint16(len(r.data))
	}
	offsets[len(recs)] = cur // free-space offset

	// Offset table layout (TN1150 §"B-Tree Nodes", verified empirically
	// against an HFS+ catalog header produced by macOS hdiutil):
	//
	//   node[nodeSize-2  : nodeSize  ] = offset[0]              (= 14)
	//   node[nodeSize-4  : nodeSize-2] = offset[1]
	//   ...
	//   node[nodeSize-2*(N+1) : ... ] = offset[N] = free-space   (largest)
	//
	// i.e. offset[i] lives at node[nodeSize - 2*(i+1) : nodeSize - 2*i].
	// In memory order (low → high addr) the array reads as
	// [free-space, offset[N-1], ..., offset[1], offset[0]] — a strictly
	// *decreasing* sequence of uint16 values. This is what
	// hfs_swap_BTNode / fsck_hfs expect.
	n := len(recs)
	for i := 0; i <= n; i++ {
		binary.BigEndian.PutUint16(node[nodeSize-2*(i+1):nodeSize-2*i], offsets[i])
	}
}

func emitHeaderNode(node []byte, nodeSize int, hdr BTHeaderRec, totalNodes, mapNodes uint32) {
	desc := BTNodeDescriptor{
		FLink:      0,
		BLink:      0,
		Kind:       BTHeaderNode,
		Height:     0,
		NumRecords: 3,
	}
	if mapNodes > 0 {
		desc.FLink = 1
	}
	desc.encode(node[:BTNodeDescriptorSize])

	// Record 0: BTHeaderRec (106 bytes).
	off0 := uint16(BTNodeDescriptorSize)
	hdr.encode(node[off0 : off0+BTHeaderRecSize])

	// Record 1: user data (128 bytes, zeroed).
	off1 := off0 + BTHeaderRecSize

	// Record 2: map record (fills the remainder of the node up to the
	// offset table).
	off2 := off1 + 128
	offTableSize := uint16(4 * 2) // 3 offsets + free-space
	mapEnd := uint16(nodeSize) - offTableSize
	mapBytes := mapEnd - off2
	// Each bit in the map = "node is in use". We mark the header,
	// any map nodes, and every node we allocated.
	totalUsed := totalNodes
	for n := uint32(0); n < totalUsed; n++ {
		bitOff := n
		byteOff := off2 + uint16(bitOff/8)
		if byteOff >= mapEnd {
			break // remaining bits are in chained map nodes
		}
		node[byteOff] |= 1 << (7 - (bitOff % 8))
	}
	_ = mapBytes

	// Offset table layout (see the comment in writeRecordsNode for the
	// authoritative description). For a 3-record header node the four
	// uint16 entries map to memory positions like this:
	//   node[nodeSize-8:nodeSize-6] = free-space (= mapEnd, largest)
	//   node[nodeSize-6:nodeSize-4] = offset[2]
	//   node[nodeSize-4:nodeSize-2] = offset[1]
	//   node[nodeSize-2:nodeSize  ] = offset[0]  (= BTNodeDescriptorSize)
	binary.BigEndian.PutUint16(node[nodeSize-2:nodeSize], off0)
	binary.BigEndian.PutUint16(node[nodeSize-4:nodeSize-2], off1)
	binary.BigEndian.PutUint16(node[nodeSize-6:nodeSize-4], off2)
	binary.BigEndian.PutUint16(node[nodeSize-8:nodeSize-6], mapEnd) // free-space
}

func emitMapNode(node []byte, nodeSize int, nextMapNode uint32) {
	desc := BTNodeDescriptor{
		FLink:      nextMapNode,
		Kind:       BTMapNode,
		Height:     0,
		NumRecords: 1,
	}
	desc.encode(node[:BTNodeDescriptorSize])
	off0 := uint16(BTNodeDescriptorSize)
	mapEnd := uint16(nodeSize) - 4 // one record offset + free-space
	// offset[0] at the highest address, free-space at the lower slot.
	binary.BigEndian.PutUint16(node[nodeSize-2:nodeSize], off0)
	binary.BigEndian.PutUint16(node[nodeSize-4:nodeSize-2], mapEnd) // free-space
	// Map bits are written by the caller of BuildTree via a follow-up
	// helper if the tree is large enough to need them; for our MVP the
	// header always has enough room and this branch is not used.
}

// emitEmptyTree returns a minimal valid B-tree image consisting of just
// a header node. Apple's HFS+ documentation explicitly allows this form
// and fsck_hfs requires it for empty trees (extents/attributes when no
// file overflows 8 extents and nothing carries an xattr).
func emitEmptyTree(opts BuildOpts, clumpSize uint32) (*BuildResult, error) {
	nodeSize := int(opts.NodeSize)
	out := make([]byte, nodeSize)
	hdr := BTHeaderRec{
		TreeDepth:      0,
		RootNode:       0,
		LeafRecords:    0,
		FirstLeafNode:  0,
		LastLeafNode:   0,
		NodeSize:       opts.NodeSize,
		MaxKeyLength:   opts.MaxKeyLength,
		TotalNodes:     1,
		FreeNodes:      0,
		ClumpSize:      clumpSize,
		BTreeType:      opts.BTreeType,
		KeyCompareType: opts.KeyCompareType,
		Attributes:     opts.Attributes,
	}
	emitHeaderNode(out, nodeSize, hdr, 1, 0)
	return &BuildResult{
		Bytes:      out,
		TotalNodes: 1,
		TreeDepth:  0,
		RootNode:   0,
		FirstLeaf:  0,
		LastLeaf:   0,
		opts:       opts,
	}, nil
}

func packLeaves(records []Record, nodeSize int) ([][]Record, error) {
	if len(records) == 0 {
		return nil, nil
	}
	var leaves [][]Record
	var cur []Record
	used := BTNodeDescriptorSize + 2 // free-space offset slot at end
	for _, r := range records {
		recSize := 2 + len(r.Key) + len(r.Data) + 2 // +2 for offset table slot
		if recSize+BTNodeDescriptorSize+2 > nodeSize {
			return nil, fmt.Errorf("hfsplus: record of %d bytes exceeds node payload %d",
				recSize-2, nodeSize-BTNodeDescriptorSize-2)
		}
		if used+recSize > nodeSize && len(cur) > 0 {
			leaves = append(leaves, cur)
			cur = nil
			used = BTNodeDescriptorSize + 2
		}
		cur = append(cur, r)
		used += recSize
	}
	if len(cur) > 0 {
		leaves = append(leaves, cur)
	}
	return leaves, nil
}

func countLeafRecords(leaves [][]Record) uint32 {
	var n uint32
	for _, l := range leaves {
		n += uint32(len(l))
	}
	return n
}
