package hfsplus

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestBuildAttributesTreeEmpty(t *testing.T) {
	r, err := BuildAttributesTree(nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.LeafRecords != 0 {
		t.Errorf("LeafRecords: got %d want 0", r.LeafRecords)
	}
	if r.TotalNodes != 1 {
		t.Errorf("TotalNodes: got %d want 1 (header only, per TN1150)", r.TotalNodes)
	}
}

func TestBuildAttributesTreeOneInline(t *testing.T) {
	attrs := []Attr{
		{
			FileID: 16,
			Name:   "com.apple.quarantine",
			Data:   []byte("0181;abcdefab;Safari;|"),
		},
	}
	r, err := BuildAttributesTree(attrs)
	if err != nil {
		t.Fatal(err)
	}
	if r.LeafRecords != 1 {
		t.Errorf("LeafRecords: got %d want 1", r.LeafRecords)
	}
}

// TestBuildAttributesTreeRejectsDuplicateKeys: an attributes key is
// (fileID, name), and the name goes through the same NFD conversion the
// catalog uses, so a repeated pair - or two names that match after NFD -
// makes two equal keys in one tree.
func TestBuildAttributesTreeRejectsDuplicateKeys(t *testing.T) {
	for _, tt := range []struct {
		name  string
		attrs []Attr
	}{
		{
			name: "same pair twice",
			attrs: []Attr{
				{FileID: 16, Name: "com.apple.quarantine", Data: []byte("a")},
				{FileID: 16, Name: "com.apple.quarantine", Data: []byte("b")},
			},
		},
		{
			name: "names equal after NFD",
			attrs: []Attr{
				{FileID: 16, Name: "user.caf\u00e9", Data: []byte("a")},
				{FileID: 16, Name: "user.cafe\u0301", Data: []byte("b")},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := BuildAttributesTree(tt.attrs); err == nil {
				t.Error("BuildAttributesTree accepted two records with the same key")
			}
		})
	}
}

// TestBuildAttributesTreeAllowsSameNameOnDifferentFiles is the guard
// against over-rejecting: the same xattr on two files is ordinary.
func TestBuildAttributesTreeAllowsSameNameOnDifferentFiles(t *testing.T) {
	attrs := []Attr{
		{FileID: 16, Name: "com.apple.quarantine", Data: []byte("a")},
		{FileID: 17, Name: "com.apple.quarantine", Data: []byte("b")},
	}
	r, err := BuildAttributesTree(attrs)
	if err != nil {
		t.Fatalf("BuildAttributesTree: %v", err)
	}
	if r.LeafRecords != 2 {
		t.Errorf("LeafRecords: got %d want 2", r.LeafRecords)
	}
}

func TestEncodeAttrKeyShape(t *testing.T) {
	name, _ := NewName("com.apple.x")
	k := encodeAttrKey(0xCAFEBABE, 0, name)
	if binary.BigEndian.Uint16(k[0:2]) != 0 {
		t.Errorf("pad: got %d", binary.BigEndian.Uint16(k[0:2]))
	}
	if binary.BigEndian.Uint32(k[2:6]) != 0xCAFEBABE {
		t.Errorf("fileID: got %#x", binary.BigEndian.Uint32(k[2:6]))
	}
	if binary.BigEndian.Uint32(k[6:10]) != 0 {
		t.Errorf("startBlock: got %d", binary.BigEndian.Uint32(k[6:10]))
	}
	if binary.BigEndian.Uint16(k[10:12]) != uint16(name.LenU16()) {
		t.Errorf("nameLen: got %d want %d", binary.BigEndian.Uint16(k[10:12]), name.LenU16())
	}
}

func TestEncodeInlineAttrData(t *testing.T) {
	d := encodeInlineAttrData([]byte("abc"))
	if binary.BigEndian.Uint32(d[0:4]) != AttrRecordInline {
		t.Errorf("recordType: got %#x", binary.BigEndian.Uint32(d[0:4]))
	}
	if binary.BigEndian.Uint32(d[12:16]) != 3 {
		t.Errorf("attrSize: got %d", binary.BigEndian.Uint32(d[12:16]))
	}
	if string(d[16:19]) != "abc" {
		t.Errorf("body: %q", d[16:19])
	}
}

// TestBuildAttributesTreeNameAtBoundary confirms that an xattr name
// whose UTF-16 length is exactly MaxAttrNameRunes is accepted: this is
// the largest length fsck_hfs tolerates without flagging an error.
func TestBuildAttributesTreeNameAtBoundary(t *testing.T) {
	name := strings.Repeat("a", MaxAttrNameRunes)
	if _, err := BuildAttributesTree([]Attr{{
		FileID: 16,
		Name:   name,
		Data:   []byte("x"),
	}}); err != nil {
		t.Fatalf("BuildAttributesTree at exactly MaxAttrNameRunes: %v", err)
	}
}

// TestBuildAttributesTreeNameOverBoundary confirms that an xattr name
// one rune too long is rejected with a name-length error.
func TestBuildAttributesTreeNameOverBoundary(t *testing.T) {
	name := strings.Repeat("a", MaxAttrNameRunes+1)
	_, err := BuildAttributesTree([]Attr{{
		FileID: 16,
		Name:   name,
		Data:   []byte("x"),
	}})
	if err == nil {
		t.Fatal("expected error for name over MaxAttrNameRunes, got nil")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention name, got: %v", err)
	}
}

// TestBuildAttributesTreeOversizeData confirms that an inline attr body
// that wouldn't fit in a single attributes B-tree node is rejected up
// front rather than silently truncated.
func TestBuildAttributesTreeOversizeData(t *testing.T) {
	// Use a value comfortably larger than the inline limit to avoid
	// off-by-one fragility against the helper's exact bookkeeping.
	oversize := make([]byte, int(AttributesNodeSize))
	_, err := BuildAttributesTree([]Attr{{
		FileID: 16,
		Name:   "com.apple.x",
		Data:   oversize,
	}})
	if err == nil {
		t.Fatal("expected error for oversize attr data, got nil")
	}
	if !strings.Contains(err.Error(), "inline limit") {
		t.Errorf("error should mention inline limit, got: %v", err)
	}
}
