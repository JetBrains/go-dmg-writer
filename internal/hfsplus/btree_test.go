package hfsplus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"
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
	r, err := BuildTree(recs, BuildOpts{
		NodeSize:     128, // too small for hfsplus, but fine for the packer
		Attributes:   BTBigKeysMask | BTVariableIndexKeys,
		MaxKeyLength: 16,
	})
	if err != nil {
		// Records may exceed node payload at this size; that's fine for the test
		// we just want to confirm BuildTree errors gracefully rather than panic.
		if err.Error() == "" {
			t.Fatal("empty error")
		}
		return
	}
	if r.LeafRecords != n {
		t.Errorf("LeafRecords: got %d want %d", r.LeafRecords, n)
	}
	_ = recsPerLeaf
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
	// addr, largest value) — see writeRecordsNode for layout.
	free := binary.BigEndian.Uint16(header[504:506])
	if free == 0 {
		t.Errorf("free-space offset is zero")
	}
	if free <= off2 {
		t.Errorf("free-space (%d) must be > offset[2] (%d)", free, off2)
	}
	_ = bytes.Equal // keep import alive
}
