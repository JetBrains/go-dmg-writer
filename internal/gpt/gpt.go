// Package gpt frames a bare HFS+ volume as a whole disk: protective MBR,
// primary GUID Partition Table, volume, backup table — the geometry
// `hdiutil create -layout GPTSPUD` writes. macOS mounts a map-less image
// fine; the map is for readers that parse a `.dmg` without mounting it.
// Write-once and little-endian, unlike the big-endian UDIF wrapper.
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

// PartitionStartLBA is where the payload starts. 34 is the GPT minimum;
// Apple starts at 40 and leaves 34..39 unused, so we do too.
const PartitionStartLBA = 40

// FirstUsableLBA is the first LBA outside the primary map, as the header
// reports it — not where the partition starts.
const FirstUsableLBA = 34

// BackupSectors is the trailing map: 32 sectors of entries plus the
// backup header in the very last sector.
const BackupSectors = 33

// Geometry of the entry array and the header, in the header's own units.
const (
	EntryCount    = 128
	EntrySize     = 128
	EntryArrayLen = EntryCount * EntrySize
	HeaderLen     = 92
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
	// name and when seed the GUIDs; see [Layout.guidFrom].
	name string
	when time.Time
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
	return Layout{
		volumeSectors: sectors,
		totalSectors:  PartitionStartLBA + sectors + BackupSectors,
		name:          name,
		when:          when,
	}, nil
}

// DiskSize is the framed disk in bytes: leading map, volume, trailing map.
func (l Layout) DiskSize() uint64 { return l.totalSectors * SectorSize }

// LeadingMap is the MBR, the primary header, the entries and the unused
// 34..39 gap. It goes at offset 0 of the disk.
func (l Layout) LeadingMap() []byte {
	entries := l.entryArray()
	out := make([]byte, PartitionStartLBA*SectorSize)

	copy(out[:SectorSize], l.protectiveMBR())
	copy(out[SectorSize:SectorSize+HeaderLen], l.header(entries, headerPrimary))
	copy(out[2*SectorSize:2*SectorSize+EntryArrayLen], entries)
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
	sectors := l.totalSectors - 1
	if sectors > 0xFFFFFFFF {
		sectors = 0xFFFFFFFF
	}
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
		myLBA, alternateLBA, entryLBA = 1, l.lastLBA(), 2
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
	diskGUID := l.diskGUID()
	copy(h[56:72], diskGUID[:])
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
	e := entries[:EntrySize]
	_ = e[127]

	copy(e[0:16], AppleHFSTypeGUID[:])
	partitionGUID := l.partitionGUID()
	copy(e[16:32], partitionGUID[:])
	binary.LittleEndian.PutUint64(e[32:40], PartitionStartLBA)
	binary.LittleEndian.PutUint64(e[40:48], l.partitionLastLBA())
	// 48..56 is the attribute bitmask, unset for a plain data partition.
	// 56..128 is the name, UTF-16LE in a 72-byte field.
	for i, unit := range utf16.Encode([]rune(partitionName)) {
		binary.LittleEndian.PutUint16(e[56+2*i:58+2*i], unit)
	}
	return entries
}

// diskGUID and [Layout.partitionGUID] are derived, never random, so two
// builds of the same input produce the same image.
func (l Layout) diskGUID() [16]byte {
	return l.guidFrom("gpt:disk", l.totalSectors)
}

func (l Layout) partitionGUID() [16]byte {
	return l.guidFrom("gpt:partition", l.volumeSectors)
}

// guidFrom takes the first 16 bytes of SHA-256 over the seeds, matching
// udif's segment ID. Nothing reads a GUID's version or variant bits.
func (l Layout) guidFrom(tag string, sectors uint64) [16]byte {
	h := sha256.New()
	h.Write([]byte(tag))
	h.Write([]byte(l.name))
	var num [8]byte
	binary.BigEndian.PutUint64(num[:], uint64(l.when.Unix()))
	h.Write(num[:])
	binary.BigEndian.PutUint64(num[:], sectors)
	h.Write(num[:])

	var guid [16]byte
	copy(guid[:], h.Sum(nil))
	return guid
}
