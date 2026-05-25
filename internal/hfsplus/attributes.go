package hfsplus

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// Attribute record types (HFSPlusAttrRecord.recordType field).
const (
	AttrRecordInline      uint32 = 0x10 // kHFSPlusAttrInlineData
	AttrRecordForkData    uint32 = 0x20 // kHFSPlusAttrForkData (not used in MVP)
	AttrRecordExtents     uint32 = 0x30 // kHFSPlusAttrExtents (not used in MVP)
)

// Attr is one extended attribute attached to some catalog entry.
type Attr struct {
	FileID uint32
	Name   string // UTF-8; will be NFD'd + UTF-16BE on the wire
	Data   []byte // raw bytes; must be <= maxInlineAttrData
}

// MaxAttrNameRunes is the hard limit imposed by HFS+ on xattr name length
// in UTF-16 code units. fsck rejects longer names.
const MaxAttrNameRunes = 127

// FinderInfoAttr is the canonical xattr name for the 32-byte Finder
// metadata block that drives volume icon, custom window position, etc.
const FinderInfoAttr = "com.apple.FinderInfo"

// BuildAttributesTree builds the attributes B-tree from a list of Attr
// records (one per (fileID, attrName) pair). Passing nil yields an empty
// tree with just the header node, which is what fsck wants when no
// catalog entry has any xattrs.
func BuildAttributesTree(attrs []Attr) (*BuildResult, error) {
	records, err := buildAttrRecords(attrs)
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool {
		return compareAttrKeys(records[i].Key, records[j].Key) < 0
	})
	return BuildTree(records, BuildOpts{
		NodeSize:       AttributesNodeSize,
		Attributes:     BTBigKeysMask | BTVariableIndexKeys,
		KeyCompareType: KeyCompareBinary,
		BTreeType:      BTreeHFSType,
		// 2 pad + 4 fileID + 4 startBlock + 2 nameLen + 2*127 = 266.
		MaxKeyLength: 266,
	})
}

func buildAttrRecords(attrs []Attr) ([]Record, error) {
	out := make([]Record, 0, len(attrs))
	for _, a := range attrs {
		name, err := NewName(a.Name)
		if err != nil {
			return nil, fmt.Errorf("hfsplus: attr %q: %w", a.Name, err)
		}
		if name.LenU16() > MaxAttrNameRunes {
			return nil, fmt.Errorf("hfsplus: attr name %q exceeds %d UTF-16 code units", a.Name, MaxAttrNameRunes)
		}
		if len(a.Data) > maxInlineAttrData(AttributesNodeSize) {
			return nil, fmt.Errorf("hfsplus: attr %q on cnid %d: %d bytes exceeds inline limit", a.Name, a.FileID, len(a.Data))
		}
		key := encodeAttrKey(a.FileID, 0, name)
		data := encodeInlineAttrData(a.Data)
		out = append(out, Record{Key: key, Data: data})
	}
	return out, nil
}

// maxInlineAttrData returns the largest inline payload that fits in a
// single node of the given size.
func maxInlineAttrData(nodeSize uint16) int {
	// node payload = nodeSize - 14 (descriptor) - 2 (offset slot) - 2 (free space slot)
	// per record overhead = 2 (keyLength) + 266 (max key body) + 16 (inline record header)
	overhead := 2 + 266 + 16
	return int(nodeSize) - 14 - 2 - 2 - overhead
}

func encodeAttrKey(fileID, startBlock uint32, name HFSName) []byte {
	// HFSPlusAttrKey on disk (without the leading uint16 keyLength):
	//   uint16 pad = 0
	//   uint32 fileID
	//   uint32 startBlock
	//   uint16 nameLength
	//   uint16[nameLength] name
	nameBytes := name.Bytes()
	buf := make([]byte, 2+4+4+2+len(nameBytes))
	binary.BigEndian.PutUint16(buf[0:2], 0)
	binary.BigEndian.PutUint32(buf[2:6], fileID)
	binary.BigEndian.PutUint32(buf[6:10], startBlock)
	binary.BigEndian.PutUint16(buf[10:12], uint16(name.LenU16()))
	copy(buf[12:], nameBytes)
	return buf
}

func compareAttrKeys(a, b []byte) int {
	// Skip the 2-byte pad; compare fileID, then startBlock, then name.
	fa := binary.BigEndian.Uint32(a[2:6])
	fb := binary.BigEndian.Uint32(b[2:6])
	if fa != fb {
		if fa < fb {
			return -1
		}
		return 1
	}
	sa := binary.BigEndian.Uint32(a[6:10])
	sb := binary.BigEndian.Uint32(b[6:10])
	if sa != sb {
		if sa < sb {
			return -1
		}
		return 1
	}
	la := int(binary.BigEndian.Uint16(a[10:12]))
	lb := int(binary.BigEndian.Uint16(b[10:12]))
	min := la
	if lb < min {
		min = lb
	}
	for i := 0; i < min; i++ {
		ua := binary.BigEndian.Uint16(a[12+i*2 : 12+i*2+2])
		ub := binary.BigEndian.Uint16(b[12+i*2 : 12+i*2+2])
		if ua < ub {
			return -1
		}
		if ua > ub {
			return 1
		}
	}
	switch {
	case la < lb:
		return -1
	case la > lb:
		return 1
	default:
		return 0
	}
}

// encodeInlineAttrData encodes a kHFSPlusAttrInlineData record:
//
//	uint32 recordType   (0x10)
//	uint32 reserved[2]  (zero)
//	uint32 attrSize     (length of data in bytes)
//	uint8[attrSize] data
//	uint8 padTo2        (if len(data) is odd)
//
// The trailing pad keeps the next record's offset 2-byte aligned, which
// fsck_hfs enforces strictly ("offset #N invalid" otherwise).
func encodeInlineAttrData(data []byte) []byte {
	size := 4 + 8 + 4 + len(data)
	if size%2 != 0 {
		size++
	}
	buf := make([]byte, size)
	binary.BigEndian.PutUint32(buf[0:4], AttrRecordInline)
	binary.BigEndian.PutUint32(buf[12:16], uint32(len(data)))
	copy(buf[16:], data)
	return buf
}
