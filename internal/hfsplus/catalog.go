package hfsplus

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// EntryKind discriminates the three kinds of catalog entry we emit.
type EntryKind uint8

const (
	KindFolder EntryKind = iota
	KindFile
	KindSymlink
)

// Entry is the in-memory description of one catalog object: a folder, a
// regular file, or a symlink. The catalog packer derives the on-disk
// representation entirely from this struct, so all verifier-relevant
// bookkeeping (valence, fileCount/folderCount, encoding hints) is driven
// from a single source of truth.
type Entry struct {
	CNID       uint32 // this entry's catalog node ID
	ParentCNID uint32 // for the root folder, set to CNIDRootParent (1)
	Name       HFSName
	Kind       EntryKind

	// POSIX bits.
	Mode    uint16 // file mode bits (chmod) — type bits should be set too
	OwnerID uint32
	GroupID uint32

	// Timestamps (all five MUST be non-zero for fsck cleanliness).
	CreateTime       uint32 // Mac epoch seconds
	ContentModTime   uint32
	AttributeModTime uint32
	AccessTime       uint32
	BackupTime       uint32

	// For folders: count of immediate children (files + symlinks +
	// sub-folders). Maps to CatalogFolder.valence on disk.
	Valence uint32

	// For folders: count of immediate sub-folders only. Drives the
	// CatalogFolder.folderCount field (HFSX-specific, but required by
	// fsck_hfs whenever the kHFSHasFolderCountMask flag is set).
	SubFolderCount uint32

	// For files and symlinks: data-fork description. The packer copies
	// these straight into the CatalogFile.DataFork field.
	DataLogicalSize uint64
	DataTotalBlocks uint32
	DataExtents     ExtentRecord

	// HasAttributes is true if this entry has an extended attribute in
	// the attributes B-tree. Drives the CatFlagHasAttributes bit.
	HasAttributes bool

	// For symlinks: the link target stored as the data-fork content.
	// (Informational here; the actual bytes live in the volume image.)
	SymlinkTarget string
}

// TextEncodingHint returns the textEncoding NUMBER (not a bit position!)
// stored in CatalogFile.TextEncoding / CatalogFolder.TextEncoding: 0
// (MacRoman) for ASCII-only names, 0x7F (Unicode) otherwise.
//
// Do NOT shift this value into a bitmap: the volume header's
// encodingsBitmap uses bit positions, and `1 << 0x7F` on a uint64 is 0.
// The header is built independently in [BuildPlan].
func (e *Entry) TextEncodingHint() uint32 {
	if e.Name.IsASCII() {
		return 0
	}
	return 0x7F
}

// BuildCatalogTree builds the catalog B-tree from the entry table.
//
// entries must cover every CNID referenced (including the root folder at
// CNIDRootFolder). The packer does not implicitly create a root entry.
//
// nodeSize defaults to 4096 if zero; that's the value hdiutil writes for
// every HFS+ volume below ~1 GB and what fsck expects in practice.
func BuildCatalogTree(entries []*Entry, nodeSize uint16) (*BuildResult, error) {
	if nodeSize == 0 {
		nodeSize = 4096
	}
	records := buildCatalogRecords(entries)
	sort.Slice(records, func(i, j int) bool {
		return compareCatalogKeys(records[i].Key, records[j].Key) < 0
	})
	return BuildTree(records, BuildOpts{
		NodeSize:       nodeSize,
		Attributes:     BTBigKeysMask | BTVariableIndexKeys,
		KeyCompareType: KeyCompareBinary,
		BTreeType:      BTreeHFSType,
		// Max key body: 4 (parentID) + 2 (name length) + 2*255 = 516.
		MaxKeyLength: 516,
	})
}

// buildCatalogRecords expands every Entry into its pair of catalog records
// (forward + thread). It does NOT sort; sorting is done in the caller after
// all records exist.
func buildCatalogRecords(entries []*Entry) []Record {
	out := make([]Record, 0, len(entries)*2)
	for _, e := range entries {
		// Skip the implicit root parent (CNID 1). It has no catalog entry.
		if e.CNID == CNIDRootParent {
			continue
		}

		// Forward record: key = (parentCNID, name), data = folder/file record.
		fwdKey := encodeCatalogKey(e.ParentCNID, e.Name)
		var fwdData []byte
		switch e.Kind {
		case KindFolder:
			fwdData = encodeCatalogFolderRecord(e)
		case KindFile, KindSymlink:
			fwdData = encodeCatalogFileRecord(e)
		default:
			panic(fmt.Sprintf("hfsplus: unknown entry kind %d", e.Kind))
		}
		out = append(out, Record{Key: fwdKey, Data: fwdData})

		// Thread record: key = (cnid, ""), data = parent reference.
		threadKey := encodeCatalogKey(e.CNID, HFSName{})
		threadData := encodeThreadRecord(e)
		out = append(out, Record{Key: threadKey, Data: threadData})
	}
	return out
}

// encodeCatalogKey produces the on-disk key body (without the leading
// keyLength field — the B-tree packer adds that).
func encodeCatalogKey(parentID uint32, name HFSName) []byte {
	// 4 (parentID) + 2 (length) + 2*nlen
	nameBytes := name.Bytes()
	buf := make([]byte, 4+2+len(nameBytes))
	binary.BigEndian.PutUint32(buf[0:4], parentID)
	binary.BigEndian.PutUint16(buf[4:6], uint16(name.LenU16()))
	copy(buf[6:], nameBytes)
	return buf
}

// compareCatalogKeys implements the HFS+ binary-compare ordering on raw
// catalog key bodies. Used to sort records before packing.
func compareCatalogKeys(a, b []byte) int {
	pa := binary.BigEndian.Uint32(a[0:4])
	pb := binary.BigEndian.Uint32(b[0:4])
	if pa < pb {
		return -1
	}
	if pa > pb {
		return 1
	}
	la := int(binary.BigEndian.Uint16(a[4:6]))
	lb := int(binary.BigEndian.Uint16(b[4:6]))
	minimum := la
	if lb < minimum {
		minimum = lb
	}
	for i := 0; i < minimum; i++ {
		ua := binary.BigEndian.Uint16(a[6+i*2 : 6+i*2+2])
		ub := binary.BigEndian.Uint16(b[6+i*2 : 6+i*2+2])
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

func encodeCatalogFolderRecord(e *Entry) []byte {
	// fsck_hfs *requires* every folder to carry kHFSHasFolderCountMask
	// on HFSX volumes (and tolerates it on HFS+ too), with FolderCount
	// holding the count of immediate subfolders.
	flags := uint16(CatFlagHasFolderCount)
	if e.HasAttributes {
		flags |= CatFlagHasAttributes
	}
	r := CatalogFolder{
		RecordType:       RecTypeFolder,
		Flags:            flags,
		Valence:          e.Valence,
		FolderID:         e.CNID,
		CreateDate:       e.CreateTime,
		ContentModDate:   e.ContentModTime,
		AttributeModDate: e.AttributeModTime,
		AccessDate:       e.AccessTime,
		BackupDate:       e.BackupTime,
		Permissions: BSDInfo{
			OwnerID:  e.OwnerID,
			GroupID:  e.GroupID,
			FileMode: e.Mode,
		},
		TextEncoding: e.TextEncodingHint(),
		FolderCount:  e.SubFolderCount,
	}
	buf := make([]byte, CatalogFolderSize)
	r.encode(buf)
	return buf
}

func encodeCatalogFileRecord(e *Entry) []byte {
	flags := uint16(0)
	if e.HasAttributes {
		flags |= CatFlagHasAttributes
	}
	r := CatalogFile{
		RecordType:       RecTypeFile,
		Flags:            flags,
		FileID:           e.CNID,
		CreateDate:       e.CreateTime,
		ContentModDate:   e.ContentModTime,
		AttributeModDate: e.AttributeModTime,
		AccessDate:       e.AccessTime,
		BackupDate:       e.BackupTime,
		Permissions: BSDInfo{
			OwnerID:  e.OwnerID,
			GroupID:  e.GroupID,
			FileMode: e.Mode,
		},
		TextEncoding: e.TextEncodingHint(),
		DataFork: ForkData{
			LogicalSize: e.DataLogicalSize,
			TotalBlocks: e.DataTotalBlocks,
			Extents:     e.DataExtents,
		},
	}
	if e.Kind == KindSymlink {
		r.UserInfo.FileType = SymlinkFileType
		r.UserInfo.FileCreator = SymlinkCreator
	}
	buf := make([]byte, CatalogFileSize)
	r.encode(buf)
	return buf
}

func encodeThreadRecord(e *Entry) []byte {
	// HFSPlusCatalogThread layout: int16 recordType | int16 reserved |
	// uint32 parentID | HFSUniStr255 (uint16 length + uint16[length]).
	var recType int16 = RecTypeFolderThread
	if e.Kind != KindFolder {
		recType = RecTypeFileThread
	}
	nameBytes := e.Name.Bytes()
	buf := make([]byte, 2+2+4+2+len(nameBytes))
	binary.BigEndian.PutUint16(buf[0:2], uint16(recType))
	binary.BigEndian.PutUint16(buf[2:4], 0) // reserved
	binary.BigEndian.PutUint32(buf[4:8], e.ParentCNID)
	binary.BigEndian.PutUint16(buf[8:10], uint16(e.Name.LenU16()))
	copy(buf[10:], nameBytes)
	return buf
}
