// Package gpt frames a bare HFS+ volume as a whole disk: protective MBR,
// primary GUID Partition Table, volume, backup table: the geometry
// `hdiutil create -layout GPTSPUD` writes. macOS mounts a map-less image
// fine; the map is for readers that parse a `.dmg` without mounting it.
// Write-once and little-endian, unlike the big-endian UDIF wrapper.
//
// The framing has two halves that must agree. [Layout.LeadingMap] and
// [Layout.TrailingMap] are the bytes on the disk, and [Layout.Regions]
// is the same geometry as a list of spans for the UDIF resource fork,
// which carries one blkx table per span. Change one half and change the
// other, or the image says two different things about the same disk.
//
// Every GUID here is derived rather than random, so two builds of one
// input give one image; see guidFrom for what that costs.
package gpt

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"time"
	"unicode/utf16"
)

// SectorSize is the logical block size UDIF and every GPT consumer here
// assume.
const SectorSize = 512

// Geometry of the entry array and the header, in the header's own units.
const (
	EntryCount    = 128
	EntrySize     = 128
	EntryArrayLen = EntryCount * EntrySize
	HeaderLen     = 92

	// EntryArraySectors is how many sectors the entry array occupies.
	// Every LBA below derives from it, so changing EntryCount or
	// EntrySize moves the map and the header together instead of
	// letting them drift apart.
	EntryArraySectors = EntryArrayLen / SectorSize
)

// The map's fixed LBAs. Sector 0 is the protective MBR and sector 1 the
// primary header, so the entry array starts at 2 and everything after it
// follows from its size.
const (
	// EntryArrayLBA is where the primary entry array starts.
	EntryArrayLBA = 2
	// FirstUsableLBA is the first LBA outside the primary map, as the
	// header reports it, which is not where the partition starts.
	FirstUsableLBA = EntryArrayLBA + EntryArraySectors
	// BackupSectors is the trailing map: the backup entry array plus the
	// backup header in the very last sector.
	BackupSectors = EntryArraySectors + 1
)

// PartitionStartLBA is where the payload starts. [FirstUsableLBA] is the
// GPT minimum; Apple starts at 40 and leaves the sectors in between
// unused, so we do too.
const PartitionStartLBA = 40

// Location of the partition name inside one entry: UTF-16LE, not
// NUL-terminated, and not the volume name Finder shows.
const (
	entryNameOffset = 56
	entryNameLen    = EntrySize - entryNameOffset
)

// Compile-time guards on the geometry above. Each is a negative constant
// (so the build fails on the uint conversion) the moment an invariant the
// rest of this file assumes stops holding.
const (
	_ = uint(EntryArraySectors*SectorSize - EntryArrayLen) // the entry array is a whole number of sectors
	_ = uint(PartitionStartLBA - FirstUsableLBA)           // the payload starts outside the primary map
	_ = uint(entryNameLen - 2*len(partitionName))          // the partition name fits its field
)

// AppleHFSTypeGUID is 48465300-0000-11AA-AA11-00306543ECAC, Apple's
// "Apple HFS/HFS+" type stored mixed-endian. `hdiutil` types an HFSX
// volume with it too, so the filesystem comes from the volume header
// rather than from this GUID.
var AppleHFSTypeGUID = [16]byte{
	0x00, 0x53, 0x46, 0x48, 0x00, 0x00, 0xAA, 0x11,
	0xAA, 0x11, 0x00, 0x30, 0x65, 0x43, 0xEC, 0xAC,
}

// partitionName is the partition's own name, separate from the volume
// name Finder shows. `hdiutil` writes exactly this.
const partitionName = "disk image"

// ErrEmptyVolume: there is nothing to put in the partition.
var ErrEmptyVolume = errors.New("gpt: cannot partition an empty volume")

// Layout is the sector geometry of the finished disk, derived once by
// [New] so the two maps cannot disagree.
type Layout struct {
	volumeSectors uint64
	totalSectors  uint64
	// The GUIDs are derived from the seeds once in [New], rather than on
	// every access, so the primary and the backup map cannot disagree and
	// framing an image hashes twice instead of four times per call.
	diskGUID      [16]byte
	partitionGUID [16]byte
}

// New derives the geometry for a volume of volumeSize bytes; name and
// when seed the GUIDs. A size that is not a whole number of sectors is
// refused, because it would put the backup map off a sector boundary.
func New(volumeSize uint64, name string, when time.Time) (Layout, error) {
	if volumeSize == 0 {
		return Layout{}, ErrEmptyVolume
	}
	if volumeSize%SectorSize != 0 {
		return Layout{}, fmt.Errorf("gpt: volume size %d is not a whole number of %d-byte sectors", volumeSize, SectorSize)
	}
	sectors := volumeSize / SectorSize
	total := PartitionStartLBA + sectors + BackupSectors
	return Layout{
		volumeSectors: sectors,
		totalSectors:  total,
		diskGUID:      guidFrom("gpt:disk", name, when, total),
		partitionGUID: guidFrom("gpt:partition", name, when, sectors),
	}, nil
}

// DiskSize is the framed disk in bytes: leading map, volume, trailing map.
func (l Layout) DiskSize() uint64 { return l.totalSectors * SectorSize }

// Region is one span of the framed disk, in sector order. Name is the
// partition's own name and is empty for free space; Type is the
// partition type as `hdiutil` spells it.
type Region struct {
	Name    string
	Type    string
	Sectors uint64
}

// Regions describe the disk one span at a time, so the UDIF resource
// fork can carry a blkx table per span instead of one table that claims
// the whole disk is a filesystem. The names are the ones `hdiutil`
// writes for a GPTSPUD image, and the counts add up to [Layout.DiskSize]
// in sectors.
//
// A reader that parses the image without mounting it starts here, so a
// wrong name is not cosmetic: it sends that reader to sector 0 expecting
// a volume header and handing it a protective MBR instead.
//
// Unlike hdiutil we leave no free gap in front of the backup table
// because we do not round the disk up to a multiple of 8 sectors, so
// this is 7 regions where hdiutil emits 8.
func (l Layout) Regions() []Region {
	return []Region{
		{Name: "Protective Master Boot Record", Type: "MBR", Sectors: 1},
		{Name: "GPT Header", Type: "Primary GPT Header", Sectors: 1},
		{Name: "GPT Partition Data", Type: "Primary GPT Table", Sectors: EntryArraySectors},
		{Name: "", Type: "Apple_Free", Sectors: PartitionStartLBA - FirstUsableLBA},
		// Named after the PARTITION, not the volume, and typed from the
		// filesystem rather than from [AppleHFSTypeGUID]: hence
		// "disk image" and "Apple_HFSX" regardless of the volume name
		{Name: partitionName, Type: "Apple_HFSX", Sectors: l.volumeSectors},
		{Name: "GPT Partition Data", Type: "Backup GPT Table", Sectors: EntryArraySectors},
		{Name: "GPT Header", Type: "Backup GPT Header", Sectors: 1},
	}
}

// LeadingMap is the MBR, the primary header, the entries and the unused
// 34..39 gaps. It goes at offset 0 of the disk.
func (l Layout) LeadingMap() []byte {
	entries := l.entryArray()
	out := make([]byte, PartitionStartLBA*SectorSize)

	copy(out[:SectorSize], l.protectiveMBR())
	copy(out[SectorSize:SectorSize+HeaderLen], l.header(entries, headerPrimary))
	copy(out[EntryArrayLBA*SectorSize:EntryArrayLBA*SectorSize+EntryArrayLen], entries)
	return out
}

// TrailingMap is the backup entries plus the backup header in the final
// sector. It goes right after the volume.
func (l Layout) TrailingMap() []byte {
	entries := l.entryArray()
	out := make([]byte, BackupSectors*SectorSize)

	copy(out[:EntryArrayLen], entries)
	at := (BackupSectors - 1) * SectorSize
	copy(out[at:at+HeaderLen], l.header(entries, headerBackup))
	return out
}

func (l Layout) lastLBA() uint64 { return l.totalSectors - 1 }

// lastUsableLBA is the last sector a partition can use, which here is
// also the partition's own last sector.
func (l Layout) lastUsableLBA() uint64 { return l.totalSectors - BackupSectors - 1 }

func (l Layout) partitionLastLBA() uint64 { return PartitionStartLBA + l.volumeSectors - 1 }

// protectiveMBR is one 0xEE partition covering the disk, so an MBR-only
// tool sees it as occupied rather than blank and ready to format.
func (l Layout) protectiveMBR() []byte {
	mbr := make([]byte, SectorSize)
	e := mbr[446:462]
	_ = e[15]
	e[0] = 0x00 // not bootable
	// Both CHS fields are saturated; the LBA fields are what counts.
	copy(e[1:4], []byte{0xFE, 0xFF, 0xFF})
	e[4] = 0xEE // GPT protective
	copy(e[5:8], []byte{0xFE, 0xFF, 0xFF})
	binary.LittleEndian.PutUint32(e[8:12], 1) // starts at LBA 1
	// Saturates past 2 TiB, as the spec prescribes; the GPT LBAs stay exact.
	sectors := min(l.totalSectors-1, 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(e[12:16], uint32(sectors))
	copy(mbr[510:512], []byte{0x55, 0xAA})
	return mbr
}

// headerRole picks which header [Layout.header] builds. The two differ
// in three fields, so the backup is not a plain copy.
type headerRole int

const (
	headerPrimary headerRole = iota
	headerBackup
)

// header encodes one 92-byte GPT header over the entry array entries.
func (l Layout) header(entries []byte, role headerRole) []byte {
	var myLBA, alternateLBA, entryLBA uint64
	switch role {
	case headerBackup:
		myLBA, alternateLBA, entryLBA = l.lastLBA(), 1, l.lastUsableLBA()+1
	default:
		myLBA, alternateLBA, entryLBA = 1, l.lastLBA(), EntryArrayLBA
	}

	h := make([]byte, HeaderLen)
	_ = h[91]
	copy(h[0:8], "EFI PART")
	binary.LittleEndian.PutUint32(h[8:12], 0x00010000) // revision 1.0
	binary.LittleEndian.PutUint32(h[12:16], HeaderLen)
	// 16..20 is the header CRC-32, written below with the field zeroed;
	// 20..24 stays reserved.
	binary.LittleEndian.PutUint64(h[24:32], myLBA)
	binary.LittleEndian.PutUint64(h[32:40], alternateLBA)
	binary.LittleEndian.PutUint64(h[40:48], FirstUsableLBA)
	binary.LittleEndian.PutUint64(h[48:56], l.lastUsableLBA())
	copy(h[56:72], l.diskGUID[:])
	binary.LittleEndian.PutUint64(h[72:80], entryLBA)
	binary.LittleEndian.PutUint32(h[80:84], EntryCount)
	binary.LittleEndian.PutUint32(h[84:88], EntrySize)
	binary.LittleEndian.PutUint32(h[88:92], crc32.ChecksumIEEE(entries))

	binary.LittleEndian.PutUint32(h[16:20], crc32.ChecksumIEEE(h))
	return h
}

// entryArray is the 16 KiB entry array. Only the first entry is used: a
// stray second one breaks a reader that expects a single partition.
func (l Layout) entryArray() []byte {
	entries := make([]byte, EntryArrayLen)
	// Full slice expression: capping the capacity as well as the length
	// is what confines a write to this entry. A plain entries[:EntrySize]
	// would keep the whole array's capacity, and a slice expression is
	// bounds-checked against capacity, so an overrun would land in the
	// next entry instead of panicking.
	e := entries[:EntrySize:EntrySize]

	copy(e[0:16], AppleHFSTypeGUID[:])
	copy(e[16:32], l.partitionGUID[:])
	binary.LittleEndian.PutUint64(e[32:40], PartitionStartLBA)
	binary.LittleEndian.PutUint64(e[40:48], l.partitionLastLBA())
	// 48..56 is the attribute bitmask, unset for a plain data partition.
	// The name goes in the rest of the entry. name is capacity-capped to
	// the field, so a name too long for it panics here instead of running
	// on into the next entry; the compile-time guard on entryNameLen
	// keeps even that unreachable.
	name := e[entryNameOffset : entryNameOffset+entryNameLen : entryNameOffset+entryNameLen]
	for i, unit := range utf16.Encode([]rune(partitionName)) {
		binary.LittleEndian.PutUint16(name[2*i:2*i+2], unit)
	}
	return entries
}

// guidFrom derives a GUID, never random, so two builds of the same input
// produce the same image. It takes the first 16 bytes of SHA-256 over the
// seeds, matching udif's segment ID; the tag separates the disk's GUID
// from the partition's. Nothing reads a GUID's version or variant bits.
func guidFrom(tag, name string, when time.Time, sectors uint64) [16]byte {
	h := sha256.New()
	h.Write([]byte(tag))
	h.Write([]byte(name))
	var num [8]byte
	binary.BigEndian.PutUint64(num[:], uint64(when.Unix()))
	h.Write(num[:])
	binary.BigEndian.PutUint64(num[:], sectors)
	h.Write(num[:])

	var guid [16]byte
	copy(guid[:], h.Sum(nil))
	return guid
}
