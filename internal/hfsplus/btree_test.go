package hfsplus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
	"time"
)

func TestBuildTreeSingleRecord(t *testing.T) {
	rec := Record{Key: []byte("hello"), Data: []byte("world!")}
	r, err := BuildTree([]Record{rec}, BuildOpts{
		NodeSize:     512,
		Attributes:   BTBigKeysMask | BTVariableIndexKeys,
		MaxKeyLength: 32,
	})
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if r.TotalNodes < 2 {
		t.Fatalf("TotalNodes: got %d want >= 2 (header + 1 leaf)", r.TotalNodes)
	}
	if r.TreeDepth != 1 {
		t.Errorf("TreeDepth: got %d want 1", r.TreeDepth)
	}
	if r.LeafRecords != 1 {
		t.Errorf("LeafRecords: got %d want 1", r.LeafRecords)
	}
	if r.RootNode != r.FirstLeaf {
		t.Errorf("single-leaf root: RootNode=%d FirstLeaf=%d", r.RootNode, r.FirstLeaf)
	}
}

func TestBuildTreeManyRecords(t *testing.T) {
	const n = 256
	recs := make([]Record, n)
	for i := range recs {
		var k [8]byte
		binary.BigEndian.PutUint64(k[:], uint64(i))
		recs[i] = Record{Key: k[:], Data: []byte(fmt.Sprintf("rec-%04d-XXXXXXXX", i))}
	}
	r, err := BuildTree(recs, BuildOpts{
		NodeSize:     512,
		Attributes:   BTBigKeysMask | BTVariableIndexKeys,
		MaxKeyLength: 8,
	})
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if r.LeafRecords != n {
		t.Errorf("LeafRecords: got %d want %d", r.LeafRecords, n)
	}
	if r.TreeDepth < 2 {
		t.Errorf("TreeDepth: got %d want >= 2 (256 recs in 512-byte nodes)", r.TreeDepth)
	}
	// Sanity: the tree should consist of a header + at least 4 leaves + index level(s).
	if r.TotalNodes < 6 {
		t.Errorf("TotalNodes: got %d want >= 6", r.TotalNodes)
	}
}

func TestBuildTreeNodeBoundaries(t *testing.T) {
	// Records small enough that 5 fit per leaf, give us exactly 12 leaves.
	const n = 60
	const recsPerLeaf = 5
	recs := make([]Record, n)
	for i := range recs {
		var k [16]byte
		binary.BigEndian.PutUint64(k[:8], uint64(i))
		recs[i] = Record{Key: k[:], Data: make([]byte, 64)}
	}
	// Each record is 2 (keyLength) + 16 (key) + 64 (data) + 2 (offset
	// slot) = 84 bytes, and a node has NodeSize-16 to spend, so five fit
	// in a 512-byte node and the 60 records need exactly 12 leaves.
	r, err := BuildTree(recs, BuildOpts{
		NodeSize:     512,
		Attributes:   BTBigKeysMask | BTVariableIndexKeys,
		MaxKeyLength: 16,
	})
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if r.LeafRecords != n {
		t.Errorf("LeafRecords: got %d want %d", r.LeafRecords, n)
	}
	// 12 leaves plus one index root plus the header node.
	if want := uint32(n/recsPerLeaf + 2); r.TotalNodes != want {
		t.Errorf("TotalNodes: got %d want %d (%d leaves + root + header)",
			r.TotalNodes, want, n/recsPerLeaf)
	}
	if r.TreeDepth != 2 {
		t.Errorf("TreeDepth: got %d want 2 (leaves plus one index level)", r.TreeDepth)
	}
}

func TestBuildTreeEmpty(t *testing.T) {
	r, err := BuildTree(nil, BuildOpts{
		NodeSize:     512,
		Attributes:   BTBigKeysMask | BTVariableIndexKeys,
		MaxKeyLength: 16,
	})
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if r.LeafRecords != 0 {
		t.Errorf("LeafRecords: got %d want 0", r.LeafRecords)
	}
	// TN1150 specifies an empty tree has only the header node, rootNode=0,
	// firstLeaf=0, lastLeaf=0, treeDepth=0.
	if r.TotalNodes != 1 {
		t.Errorf("TotalNodes: got %d want 1 (just the header)", r.TotalNodes)
	}
	if r.TreeDepth != 0 {
		t.Errorf("TreeDepth: got %d want 0", r.TreeDepth)
	}
	if r.RootNode != 0 || r.FirstLeaf != 0 || r.LastLeaf != 0 {
		t.Errorf("Root/First/Last: got %d/%d/%d want 0/0/0", r.RootNode, r.FirstLeaf, r.LastLeaf)
	}
	if int8(r.Bytes[8]) != BTHeaderNode {
		t.Errorf("header node kind: got %d want %d", int8(r.Bytes[8]), BTHeaderNode)
	}
}

func TestBuildTreeHeaderRecordCorrectness(t *testing.T) {
	r, err := BuildTree([]Record{{Key: []byte("a"), Data: []byte("b")}}, BuildOpts{
		NodeSize:     512,
		Attributes:   BTBigKeysMask | BTVariableIndexKeys,
		MaxKeyLength: 16,
	})
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	header := r.Bytes[:512]
	// Node descriptor: bytes 0-13. Kind byte at offset 8.
	if int8(header[8]) != BTHeaderNode {
		t.Errorf("header node kind: got %d want %d", int8(header[8]), BTHeaderNode)
	}
	if header[9] != 0 {
		t.Errorf("header node height: got %d want 0", header[9])
	}
	// Header node has 3 records. Offset table layout (TN1150 + verified
	// against hdiutil-produced volumes):
	//   header[504:506] = free-space offset (largest)
	//   header[506:508] = offset[2]
	//   header[508:510] = offset[1]
	//   header[510:512] = offset[0] = BTNodeDescriptorSize
	off0 := binary.BigEndian.Uint16(header[510:512])
	if off0 != BTNodeDescriptorSize {
		t.Errorf("offset[0] in header node: got %d want %d", off0, BTNodeDescriptorSize)
	}
	off1 := binary.BigEndian.Uint16(header[508:510])
	if off1 != BTNodeDescriptorSize+BTHeaderRecSize {
		t.Errorf("offset[1] in header node: got %d want %d",
			off1, BTNodeDescriptorSize+BTHeaderRecSize)
	}
	off2 := binary.BigEndian.Uint16(header[506:508])
	if off2 != BTNodeDescriptorSize+BTHeaderRecSize+128 {
		t.Errorf("offset[2] in header node: got %d want %d",
			off2, BTNodeDescriptorSize+BTHeaderRecSize+128)
	}
	// BTHeaderRec: skim TotalNodes (at offset 14+22 = 36 inside the node).
	totalNodes := binary.BigEndian.Uint32(header[36:40])
	if totalNodes != r.TotalNodes {
		t.Errorf("TotalNodes inside header rec: got %d want %d", totalNodes, r.TotalNodes)
	}
	// MaxKeyLength at offset 14+20 = 34.
	maxKey := binary.BigEndian.Uint16(header[34:36])
	if maxKey != 16 {
		t.Errorf("MaxKeyLength: got %d want 16", maxKey)
	}
	// The map record (record 2) should have at least the header bit set.
	// Map record starts at offset 14+106+128 = 248.
	if header[248]&0x80 == 0 {
		t.Errorf("map bit for node 0 not set")
	}
	// Free-space offset is the *first* uint16 of the offset region (lowest
	// addr, largest value) - see writeRecordsNode for layout.
	free := binary.BigEndian.Uint16(header[504:506])
	if free == 0 {
		t.Errorf("free-space offset is zero")
	}
	if free <= off2 {
		t.Errorf("free-space (%d) must be > offset[2] (%d)", free, off2)
	}
	_ = bytes.Equal // keep import alive
}

// countMapBits walks the header node's map record and the chained map
// nodes and returns the node numbers marked in use, in the same MSB-first
// order the on-disk map uses.
func countMapBits(t *testing.T, r *BuildResult) uint32 {
	t.Helper()
	nodeSize := int(r.opts.NodeSize)

	var set uint32
	countRecord := func(body []byte) {
		for _, b := range body {
			for bit := 0; bit < 8; bit++ {
				if b&(1<<(7-bit)) != 0 {
					set++
				}
			}
		}
	}

	// Header node: record 2 runs from the end of the user-data record to
	// the free-space offset.
	off2 := BTNodeDescriptorSize + BTHeaderRecSize + 128
	countRecord(r.Bytes[off2 : nodeSize-4*2])

	// Follow the map chain from the header's FLink.
	for node := binary.BigEndian.Uint32(r.Bytes[0:4]); node != 0; {
		start := int(node) * nodeSize
		if start+nodeSize > len(r.Bytes) {
			t.Fatalf("map node %d runs past the tree image", node)
		}
		body := r.Bytes[start : start+nodeSize]
		if int8(body[8]) != BTMapNode {
			t.Fatalf("node %d in the map chain has kind %d, want %d", node, int8(body[8]), BTMapNode)
		}
		countRecord(body[BTNodeDescriptorSize : nodeSize-2*2])
		node = binary.BigEndian.Uint32(body[0:4])
	}
	return set
}

// TestBuildTreeMapNodesCarryBits pins the bug where a tree big enough to
// need chained map nodes got them allocated, linked and left empty. Every
// node those map nodes covered read as free, which fsck_hfs reports as
// damage on a tree it can otherwise walk.
func TestBuildTreeMapNodesCarryBits(t *testing.T) {
	const nodeSize = 512
	// mapBitsInHeaderNode(512) is (512-14-8-106-128)*8 = 2048 bits, so a
	// tree past 2048 nodes needs a chain. Records sized to one per leaf
	// get there with a manageable record count.
	recs := make([]Record, 3000)
	for i := range recs {
		var k [16]byte
		binary.BigEndian.PutUint64(k[:8], uint64(i))
		recs[i] = Record{Key: k[:], Data: make([]byte, 400)}
	}
	r, err := BuildTree(recs, BuildOpts{
		NodeSize:     nodeSize,
		Attributes:   BTBigKeysMask | BTVariableIndexKeys,
		MaxKeyLength: 16,
	})
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if r.TotalNodes <= uint32(mapBitsInHeaderNode(nodeSize)) {
		t.Fatalf("tree has %d nodes, not enough to need a map node (header holds %d bits)",
			r.TotalNodes, mapBitsInHeaderNode(nodeSize))
	}
	// The header must point at the chain.
	if flink := binary.BigEndian.Uint32(r.Bytes[0:4]); flink != 1 {
		t.Fatalf("header FLink = %d, want 1 (the first map node)", flink)
	}
	if got := countMapBits(t, r); got != r.TotalNodes {
		t.Errorf("allocation map marks %d nodes in use, want %d; %d used nodes read as free",
			got, r.TotalNodes, r.TotalNodes-got)
	}
}

// TestBuildTreeMapBitsMatchNodeCount checks the same invariant over the
// sizes that do and do not need a chain, so the header-only path stays
// correct too.
func TestBuildTreeMapBitsMatchNodeCount(t *testing.T) {
	for _, n := range []int{1, 10, 500, 3000, 8000} {
		recs := make([]Record, n)
		for i := range recs {
			var k [16]byte
			binary.BigEndian.PutUint64(k[:8], uint64(i))
			recs[i] = Record{Key: k[:], Data: make([]byte, 400)}
		}
		r, err := BuildTree(recs, BuildOpts{
			NodeSize:     512,
			Attributes:   BTBigKeysMask | BTVariableIndexKeys,
			MaxKeyLength: 16,
		})
		if err != nil {
			t.Fatalf("BuildTree(%d records): %v", n, err)
		}
		if got := countMapBits(t, r); got != r.TotalNodes {
			t.Errorf("%d records: map marks %d of %d nodes in use", n, got, r.TotalNodes)
		}
	}
}

// TestPadToBlocksRejectsUncoveredNodes: padding adds free nodes, and a
// free node still needs a bit in the map. Growing past what the map can
// describe has to fail rather than emit a header that counts nodes the
// map never mentions.
func TestPadToBlocksRejectsUncoveredNodes(t *testing.T) {
	r, err := BuildTree([]Record{{Key: []byte("a"), Data: []byte("b")}}, BuildOpts{
		NodeSize:     512,
		Attributes:   BTBigKeysMask | BTVariableIndexKeys,
		MaxKeyLength: 16,
	})
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	// The header's map record covers 2048 nodes at this node size.
	if err := r.PadToBlocks(2048, 512); err != nil {
		t.Errorf("PadToBlocks to exactly the map's capacity: %v", err)
	}
	if err := r.PadToBlocks(2049, 512); err == nil {
		t.Error("PadToBlocks grew the tree past what its allocation map can describe")
	}
}

// TestBuildTreeRejectsUnshrinkableIndex covers the index level that could
// not get smaller: with keys wide enough that only one index record fits
// per node, every level has as many nodes as the one below it, so the
// loop that builds levels never reaches a single root.
func TestBuildTreeRejectsUnshrinkableIndex(t *testing.T) {
	// At NodeSize 512, two index records of a 300-byte key need
	// 16+2*(300+8) = 632 bytes, so only one fits.
	recs := make([]Record, 40)
	for i := range recs {
		k := make([]byte, 300)
		binary.BigEndian.PutUint64(k[:8], uint64(i))
		recs[i] = Record{Key: k, Data: []byte{1}}
	}
	donec := make(chan error, 1)
	go func() {
		_, err := BuildTree(recs, BuildOpts{
			NodeSize:     512,
			Attributes:   BTBigKeysMask | BTVariableIndexKeys,
			MaxKeyLength: 300,
		})
		donec <- err
	}()
	select {
	case err := <-donec:
		if err == nil {
			t.Fatal("BuildTree accepted a tree whose index level cannot shrink")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("BuildTree did not return within 20s (the index loop never terminates)")
	}
}

// TestBuildTreeRejectsTinyNodes: a node too small to hold the header's
// three records has nowhere to put an allocation map.
func TestBuildTreeRejectsTinyNodes(t *testing.T) {
	_, err := BuildTree([]Record{{Key: []byte("a"), Data: []byte("b")}}, BuildOpts{
		NodeSize:     128,
		Attributes:   BTBigKeysMask | BTVariableIndexKeys,
		MaxKeyLength: 16,
	})
	if err == nil {
		t.Fatal("BuildTree accepted a 128-byte node size")
	}
}
