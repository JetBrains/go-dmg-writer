package hfsplus

import "encoding/binary"

// VolumeSignature "HX" for HFSX (case-sensitive, binary-compare catalog).
// We use HFSX because it lets us sort catalog keys with a simple
// codepoint-by-codepoint binary comparison (no need to implement Apple's
// case-folding table). HFSX is a strict superset of HFS+ and is mountable
// and notarizable on every macOS release since 10.3.
const VolumeSignature uint16 = 0x4858

// VolumeVersion is 5 for HFSX (TN1150 § "HFSX Differences").
const VolumeVersion uint16 = 5

// Reserved catalog node IDs (TN1150 §"Reserved CNIDs").
const (
	CNIDRootParent uint32 = 1
	CNIDRootFolder uint32 = 2
	CNIDExtents    uint32 = 3
	CNIDCatalog    uint32 = 4
	CNIDBadBlock   uint32 = 5
	CNIDAllocation uint32 = 6
	CNIDStartup    uint32 = 7
	CNIDAttributes uint32 = 8
	CNIDFirstUser  uint32 = 16
)

// Volume attribute bits.
const (
	VolHardwareLock           uint32 = 1 << 7
	VolUnmounted              uint32 = 1 << 8 // kHFSVolumeUnmountedBit
	VolSparedBlocks           uint32 = 1 << 9
	VolNoCacheRequired        uint32 = 1 << 10
	VolBootVolumeInconsistent uint32 = 1 << 11
	VolCNIDsReused            uint32 = 1 << 12
	VolJournaled              uint32 = 1 << 13
	VolSoftwareLock           uint32 = 1 << 15
	VolUnusedNodeFix          uint32 = 1 << 31
)

// SectorSize is the HFS+ physical sector (always 512).
const SectorSize = 512

// ExtentDescriptor is a contiguous run of allocation blocks.
type ExtentDescriptor struct {
	StartBlock uint32
	BlockCount uint32
}

// ExtentRecord is the inline 8-entry array used in ForkData and in
// extent-overflow-tree leaves.
type ExtentRecord [8]ExtentDescriptor

// ForkData describes one fork (data or resource) of a file or special file.
type ForkData struct {
	LogicalSize uint64
	ClumpSize   uint32
	TotalBlocks uint32
	Extents     ExtentRecord
}

func (f *ForkData) encode(b []byte) {
	binary.BigEndian.PutUint64(b[0:8], f.LogicalSize)
	binary.BigEndian.PutUint32(b[8:12], f.ClumpSize)
	binary.BigEndian.PutUint32(b[12:16], f.TotalBlocks)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(b[16+i*8:16+i*8+4], f.Extents[i].StartBlock)
		binary.BigEndian.PutUint32(b[16+i*8+4:16+i*8+8], f.Extents[i].BlockCount)
	}
}

// ForkDataSize is the on-disk size of a ForkData (80 bytes).
const ForkDataSize = 8 + 4 + 4 + 8*8

// VolumeHeader is the top-level volume descriptor at offsets 1024 and
// volumeSize-1024 (the alternate copy). TN1150 § "HFSPlusVolumeHeader".
type VolumeHeader struct {
	Signature          uint16
	Version            uint16
	Attributes         uint32
	LastMountedVersion uint32
	JournalInfoBlock   uint32

	CreateDate  uint32
	ModifyDate  uint32
	BackupDate  uint32
	CheckedDate uint32

	FileCount   uint32
	FolderCount uint32

	BlockSize      uint32
	TotalBlocks    uint32
	FreeBlocks     uint32
	NextAllocation uint32
	RsrcClumpSize  uint32
	DataClumpSize  uint32
	NextCatalogID  uint32

	WriteCount      uint32
	EncodingsBitmap uint64

	FinderInfo [8]uint32

	AllocationFile ForkData
	ExtentsFile    ForkData
	CatalogFile    ForkData
	AttributesFile ForkData
	StartupFile    ForkData
}

// VolumeHeaderSize is the on-disk size of a VolumeHeader (512 bytes).
const VolumeHeaderSize = 2 + 2 + 4 + 4 + 4 + 16 + 8 + 4 + 4 + 4 + 4 + 4 + 4 + 4 + 4 + 8 + 32 + 5*ForkDataSize

// Encode serializes the volume header to its 512-byte on-disk form.
func (v *VolumeHeader) Encode() []byte {
	b := make([]byte, VolumeHeaderSize)
	binary.BigEndian.PutUint16(b[0:2], v.Signature)
	binary.BigEndian.PutUint16(b[2:4], v.Version)
	binary.BigEndian.PutUint32(b[4:8], v.Attributes)
	binary.BigEndian.PutUint32(b[8:12], v.LastMountedVersion)
	binary.BigEndian.PutUint32(b[12:16], v.JournalInfoBlock)
	binary.BigEndian.PutUint32(b[16:20], v.CreateDate)
	binary.BigEndian.PutUint32(b[20:24], v.ModifyDate)
	binary.BigEndian.PutUint32(b[24:28], v.BackupDate)
	binary.BigEndian.PutUint32(b[28:32], v.CheckedDate)
	binary.BigEndian.PutUint32(b[32:36], v.FileCount)
	binary.BigEndian.PutUint32(b[36:40], v.FolderCount)
	binary.BigEndian.PutUint32(b[40:44], v.BlockSize)
	binary.BigEndian.PutUint32(b[44:48], v.TotalBlocks)
	binary.BigEndian.PutUint32(b[48:52], v.FreeBlocks)
	binary.BigEndian.PutUint32(b[52:56], v.NextAllocation)
	binary.BigEndian.PutUint32(b[56:60], v.RsrcClumpSize)
	binary.BigEndian.PutUint32(b[60:64], v.DataClumpSize)
	binary.BigEndian.PutUint32(b[64:68], v.NextCatalogID)
	binary.BigEndian.PutUint32(b[68:72], v.WriteCount)
	binary.BigEndian.PutUint64(b[72:80], v.EncodingsBitmap)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(b[80+i*4:80+i*4+4], v.FinderInfo[i])
	}
	off := 112
	v.AllocationFile.encode(b[off : off+ForkDataSize])
	off += ForkDataSize
	v.ExtentsFile.encode(b[off : off+ForkDataSize])
	off += ForkDataSize
	v.CatalogFile.encode(b[off : off+ForkDataSize])
	off += ForkDataSize
	v.AttributesFile.encode(b[off : off+ForkDataSize])
	off += ForkDataSize
	v.StartupFile.encode(b[off : off+ForkDataSize])
	return b
}

// BTNode kinds (TN1150 § "BTNodeDescriptor").
const (
	BTLeafNode   int8 = -1
	BTIndexNode  int8 = 0
	BTHeaderNode int8 = 1
	BTMapNode    int8 = 2
)

// BTKeyCompareType values. We always use binary for plain HFS+.
const (
	KeyCompareCaseFolding uint8 = 0xCF // HFS+ case-insensitive (default)
	KeyCompareBinary      uint8 = 0xBC // HFSX binary
)

// BTHeader attributes.
const (
	BTBadCloseMask      uint32 = 1 << 0
	BTBigKeysMask       uint32 = 1 << 1
	BTVariableIndexKeys uint32 = 1 << 2
)

// BTreeType values.
const (
	BTreeHFSType      uint8 = 0
	BTreeUserBTree    uint8 = 128
	BTreeReservedType uint8 = 255
)

// BTNodeDescriptor is the 14-byte header at the start of every B-tree node.
type BTNodeDescriptor struct {
	FLink      uint32
	BLink      uint32
	Kind       int8
	Height     uint8
	NumRecords uint16
	Reserved   uint16
}

// BTNodeDescriptorSize is the on-disk size (14 bytes).
const BTNodeDescriptorSize = 14

func (d *BTNodeDescriptor) encode(b []byte) {
	binary.BigEndian.PutUint32(b[0:4], d.FLink)
	binary.BigEndian.PutUint32(b[4:8], d.BLink)
	b[8] = byte(d.Kind)
	b[9] = d.Height
	binary.BigEndian.PutUint16(b[10:12], d.NumRecords)
	binary.BigEndian.PutUint16(b[12:14], d.Reserved)
}

// BTHeaderRec is the per-tree metadata stored as the first record of the
// header node (node 0).
type BTHeaderRec struct {
	TreeDepth      uint16
	RootNode       uint32
	LeafRecords    uint32
	FirstLeafNode  uint32
	LastLeafNode   uint32
	NodeSize       uint16
	MaxKeyLength   uint16
	TotalNodes     uint32
	FreeNodes      uint32
	Reserved1      uint16
	ClumpSize      uint32
	BTreeType      uint8
	KeyCompareType uint8
	Attributes     uint32
	Reserved3      [16]uint32
}

// BTHeaderRecSize is the on-disk size of a BTHeaderRec (106 bytes).
const BTHeaderRecSize = 2 + 4 + 4 + 4 + 4 + 2 + 2 + 4 + 4 + 2 + 4 + 1 + 1 + 4 + 16*4

func (h *BTHeaderRec) encode(b []byte) {
	binary.BigEndian.PutUint16(b[0:2], h.TreeDepth)
	binary.BigEndian.PutUint32(b[2:6], h.RootNode)
	binary.BigEndian.PutUint32(b[6:10], h.LeafRecords)
	binary.BigEndian.PutUint32(b[10:14], h.FirstLeafNode)
	binary.BigEndian.PutUint32(b[14:18], h.LastLeafNode)
	binary.BigEndian.PutUint16(b[18:20], h.NodeSize)
	binary.BigEndian.PutUint16(b[20:22], h.MaxKeyLength)
	binary.BigEndian.PutUint32(b[22:26], h.TotalNodes)
	binary.BigEndian.PutUint32(b[26:30], h.FreeNodes)
	binary.BigEndian.PutUint16(b[30:32], h.Reserved1)
	binary.BigEndian.PutUint32(b[32:36], h.ClumpSize)
	b[36] = h.BTreeType
	b[37] = h.KeyCompareType
	binary.BigEndian.PutUint32(b[38:42], h.Attributes)
	for i := 0; i < 16; i++ {
		binary.BigEndian.PutUint32(b[42+i*4:42+i*4+4], h.Reserved3[i])
	}
}

// Catalog record types (16-bit values in HFSPlusCatalogRecord).
const (
	RecTypeFolder       int16 = 0x0001
	RecTypeFile         int16 = 0x0002
	RecTypeFolderThread int16 = 0x0003
	RecTypeFileThread   int16 = 0x0004
)

// Catalog flag bits.
const (
	CatFlagFileLocked     uint16 = 0x0001
	CatFlagThreadExists   uint16 = 0x0002
	CatFlagHasAttributes  uint16 = 0x0004
	CatFlagHasSecurity    uint16 = 0x0008
	CatFlagHasFolderCount uint16 = 0x0010
)

// BSDInfo is the POSIX-permissions block embedded in catalog records.
type BSDInfo struct {
	OwnerID    uint32
	GroupID    uint32
	AdminFlags uint8
	OwnerFlags uint8
	FileMode   uint16
	Special    uint32 // iNodeNum / linkCount / rawDevice
}

// BSDInfoSize is the on-disk size (16 bytes).
const BSDInfoSize = 4 + 4 + 1 + 1 + 2 + 4

func (i *BSDInfo) encode(b []byte) {
	binary.BigEndian.PutUint32(b[0:4], i.OwnerID)
	binary.BigEndian.PutUint32(b[4:8], i.GroupID)
	b[8] = i.AdminFlags
	b[9] = i.OwnerFlags
	binary.BigEndian.PutUint16(b[10:12], i.FileMode)
	binary.BigEndian.PutUint32(b[12:16], i.Special)
}

// FileInfo of Finder (16 bytes) + extended (16 bytes). We zero them by
// default and only fill them in for special cases (e.g. symlinks).
type FileInfo struct {
	FileType    uint32
	FileCreator uint32
	FinderFlags uint16
	Location    [4]byte // 2x int16 (v, h)
	Reserved    uint16
}

const FileInfoSize = 4 + 4 + 2 + 4 + 2

func (i *FileInfo) encode(b []byte) {
	binary.BigEndian.PutUint32(b[0:4], i.FileType)
	binary.BigEndian.PutUint32(b[4:8], i.FileCreator)
	binary.BigEndian.PutUint16(b[8:10], i.FinderFlags)
	copy(b[10:14], i.Location[:])
	binary.BigEndian.PutUint16(b[14:16], i.Reserved)
}

// ExtendedFileInfo is the 16-byte extended Finder info block.
type ExtendedFileInfo struct {
	Reserved1           [8]byte
	ExtendedFinderFlags uint16
	Reserved2           uint16
	PutAwayFolderID     uint32
}

const ExtendedFileInfoSize = 16

func (i *ExtendedFileInfo) encode(b []byte) {
	copy(b[0:8], i.Reserved1[:])
	binary.BigEndian.PutUint16(b[8:10], i.ExtendedFinderFlags)
	binary.BigEndian.PutUint16(b[10:12], i.Reserved2)
	binary.BigEndian.PutUint32(b[12:16], i.PutAwayFolderID)
}

// FolderInfo / ExtendedFolderInfo mirror FileInfo for folders. Same sizes.
type FolderInfo struct {
	WindowBounds [8]byte // top/left/bottom/right
	FinderFlags  uint16
	Location     [4]byte
	Reserved     uint16
}

func (i *FolderInfo) encode(b []byte) {
	copy(b[0:8], i.WindowBounds[:])
	binary.BigEndian.PutUint16(b[8:10], i.FinderFlags)
	copy(b[10:14], i.Location[:])
	binary.BigEndian.PutUint16(b[14:16], i.Reserved)
}

type ExtendedFolderInfo struct {
	ScrollPosition      [4]byte
	Reserved1           uint32
	ExtendedFinderFlags uint16
	Reserved2           uint16
	PutAwayFolderID     uint32
}

func (i *ExtendedFolderInfo) encode(b []byte) {
	copy(b[0:4], i.ScrollPosition[:])
	binary.BigEndian.PutUint32(b[4:8], i.Reserved1)
	binary.BigEndian.PutUint16(b[8:10], i.ExtendedFinderFlags)
	binary.BigEndian.PutUint16(b[10:12], i.Reserved2)
	binary.BigEndian.PutUint32(b[12:16], i.PutAwayFolderID)
}

// CatalogFolder is the wire form of a kHFSPlusFolderRecord.
type CatalogFolder struct {
	RecordType       int16 // RecTypeFolder
	Flags            uint16
	Valence          uint32 // count of immediate children
	FolderID         uint32
	CreateDate       uint32
	ContentModDate   uint32
	AttributeModDate uint32
	AccessDate       uint32
	BackupDate       uint32
	Permissions      BSDInfo
	UserInfo         FolderInfo
	FinderInfo       ExtendedFolderInfo
	TextEncoding     uint32
	FolderCount      uint32 // HFSX-only; we set 0 for plain HFS+
}

// CatalogFolderSize is the on-disk size of a CatalogFolder (88 bytes).
const CatalogFolderSize = 2 + 2 + 4 + 4 + 5*4 + BSDInfoSize + 16 + 16 + 4 + 4

func (r *CatalogFolder) encode(b []byte) {
	binary.BigEndian.PutUint16(b[0:2], uint16(r.RecordType))
	binary.BigEndian.PutUint16(b[2:4], r.Flags)
	binary.BigEndian.PutUint32(b[4:8], r.Valence)
	binary.BigEndian.PutUint32(b[8:12], r.FolderID)
	binary.BigEndian.PutUint32(b[12:16], r.CreateDate)
	binary.BigEndian.PutUint32(b[16:20], r.ContentModDate)
	binary.BigEndian.PutUint32(b[20:24], r.AttributeModDate)
	binary.BigEndian.PutUint32(b[24:28], r.AccessDate)
	binary.BigEndian.PutUint32(b[28:32], r.BackupDate)
	r.Permissions.encode(b[32 : 32+BSDInfoSize])
	r.UserInfo.encode(b[48:64])
	r.FinderInfo.encode(b[64:80])
	binary.BigEndian.PutUint32(b[80:84], r.TextEncoding)
	binary.BigEndian.PutUint32(b[84:88], r.FolderCount)
}

// CatalogFile is the wire form of a kHFSPlusFileRecord.
type CatalogFile struct {
	RecordType       int16
	Flags            uint16
	Reserved1        uint32
	FileID           uint32
	CreateDate       uint32
	ContentModDate   uint32
	AttributeModDate uint32
	AccessDate       uint32
	BackupDate       uint32
	Permissions      BSDInfo
	UserInfo         FileInfo
	FinderInfo       ExtendedFileInfo
	TextEncoding     uint32
	Reserved2        uint32
	DataFork         ForkData
	ResourceFork     ForkData
}

// CatalogFileSize is the on-disk size of a CatalogFile (248 bytes).
const CatalogFileSize = 2 + 2 + 4 + 4 + 5*4 + BSDInfoSize + 16 + 16 + 4 + 4 + 2*ForkDataSize

func (r *CatalogFile) encode(b []byte) {
	binary.BigEndian.PutUint16(b[0:2], uint16(r.RecordType))
	binary.BigEndian.PutUint16(b[2:4], r.Flags)
	binary.BigEndian.PutUint32(b[4:8], r.Reserved1)
	binary.BigEndian.PutUint32(b[8:12], r.FileID)
	binary.BigEndian.PutUint32(b[12:16], r.CreateDate)
	binary.BigEndian.PutUint32(b[16:20], r.ContentModDate)
	binary.BigEndian.PutUint32(b[20:24], r.AttributeModDate)
	binary.BigEndian.PutUint32(b[24:28], r.AccessDate)
	binary.BigEndian.PutUint32(b[28:32], r.BackupDate)
	r.Permissions.encode(b[32 : 32+BSDInfoSize])
	r.UserInfo.encode(b[48:64])
	r.FinderInfo.encode(b[64:80])
	binary.BigEndian.PutUint32(b[80:84], r.TextEncoding)
	binary.BigEndian.PutUint32(b[84:88], r.Reserved2)
	r.DataFork.encode(b[88 : 88+ForkDataSize])
	r.ResourceFork.encode(b[88+ForkDataSize : 88+2*ForkDataSize])
}

// SymlinkFileType / SymlinkCreator are the magic FourCC values that mark a
// catalog file record as a symbolic link. The link target is stored in the
// data fork.
const (
	SymlinkFileType uint32 = 0x736C6E6B // 'slnk'
	SymlinkCreator  uint32 = 0x72686170 // 'rhap'
)
