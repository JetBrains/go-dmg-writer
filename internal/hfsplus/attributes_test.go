package hfsplus

import (
	"encoding/binary"
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
