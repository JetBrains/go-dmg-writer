package gpt

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
	"time"
)

// testTime is the fixed timestamp the GUIDs are seeded with, so a test
// asserting determinism is not asserting anything about the clock.
var testTime = time.Unix(1700000000, 0).UTC()

// framed returns the whole disk a volume of volumeSectors sectors,
// filled with filler, is framed into.
func framed(t *testing.T, volumeSectors uint64, filler byte) []byte {
	t.Helper()
	layout, err := New(volumeSectors*SectorSize, "Air", testTime)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	out := layout.LeadingMap()
	out = append(out, bytes.Repeat([]byte{filler}, int(volumeSectors*SectorSize))...)
	out = append(out, layout.TrailingMap()...)
	if got, want := uint64(len(out)), layout.DiskSize(); got != want {
		t.Fatalf("disk size: got %d want %d, the DiskSize report disagrees with the two maps", got, want)
	}
	return out
}

func leU64(b []byte, at int) uint64 { return binary.LittleEndian.Uint64(b[at : at+8]) }

func leU32(b []byte, at int) uint32 { return binary.LittleEndian.Uint32(b[at : at+4]) }

// TestDescribesOneAppleHFSPartition is the point of the map: a parser
// that starts at the partition table finds an Apple HFS partition, and
// it starts exactly where the volume was copied to.
func TestDescribesOneAppleHFSPartition(t *testing.T) {
	const volumeSectors = 2048
	image := framed(t, volumeSectors, 0xA5)

	if got := image[510:512]; !bytes.Equal(got, []byte{0x55, 0xAA}) {
		t.Errorf("protective MBR signature: got %v want [85 170]", got)
	}
	if got := image[450]; got != 0xEE {
		t.Errorf("MBR partition type: got %#x want 0xee, the GPT protective type", got)
	}
	if got := string(image[512:520]); got != "EFI PART" {
		t.Errorf("primary GPT header: got %q want \"EFI PART\"", got)
	}

	entry := image[1024:1152]
	if !bytes.Equal(entry[0:16], AppleHFSTypeGUID[:]) {
		t.Errorf("partition type GUID: got %v want the Apple HFS GUID %v", entry[0:16], AppleHFSTypeGUID)
	}
	if got := leU64(entry, 32); got != PartitionStartLBA {
		t.Errorf("partition first LBA: got %d want %d", got, PartitionStartLBA)
	}
	if got, want := leU64(entry, 40), uint64(PartitionStartLBA+volumeSectors-1); got != want {
		t.Errorf("partition last LBA: got %d want %d", got, want)
	}

	at := PartitionStartLBA * SectorSize
	if got := image[at]; got != 0xA5 {
		t.Errorf("the volume does not start at the partition's first LBA: got %#x want 0xa5", got)
	}
	if got := image[at+volumeSectors*SectorSize-1]; got != 0xA5 {
		t.Errorf("the volume is truncated: last byte got %#x want 0xa5", got)
	}
	if got, want := len(image), (PartitionStartLBA+volumeSectors+BackupSectors)*SectorSize; got != want {
		t.Errorf("disk size: got %d want %d (MBR + primary map + volume + backup map)", got, want)
	}

	// Only the first entry is used. A stray second entry makes a reader that expects one partition fail.
	for i, b := range image[1152 : 1024+EntryArrayLen] {
		if b != 0 {
			t.Fatalf("the entry array holds a second partition: byte %d is %#x", 1152+i, b)
		}
	}
}

// TestChecksumsCoverTheHeaderAndTheEntries checks both CRC-32 fields. If
// either is wrong, macOS falls back to the backup map, or rejects the
// disk outright.
func TestChecksumsCoverTheHeaderAndTheEntries(t *testing.T) {
	image := framed(t, 64, 0)

	for _, c := range []struct {
		label  string
		header []byte
	}{
		{"primary", image[512 : 512+HeaderLen]},
		{"backup", image[len(image)-SectorSize:][:HeaderLen]},
	} {
		zeroed := append([]byte(nil), c.header...)
		copy(zeroed[16:20], []byte{0, 0, 0, 0})
		if got, want := leU32(c.header, 16), crc32.ChecksumIEEE(zeroed); got != want {
			t.Errorf("%s header CRC-32: got %#x want %#x", c.label, got, want)
		}

		at := int(leU64(c.header, 72)) * SectorSize
		if got, want := leU32(c.header, 88), crc32.ChecksumIEEE(image[at:at+EntryArrayLen]); got != want {
			t.Errorf("%s entry-array CRC-32: got %#x want %#x", c.label, got, want)
		}
	}
}

// TestBackupHeaderMirrorsThePrimary checks that the backup header is not
// a copy: `my_lba` and `alternate_lba` are swapped, and it points at its
// own entry array.
func TestBackupHeaderMirrorsThePrimary(t *testing.T) {
	const volumeSectors = 512
	image := framed(t, volumeSectors, 0)
	const total = PartitionStartLBA + volumeSectors + BackupSectors

	primary := image[512 : 512+HeaderLen]
	backup := image[len(image)-SectorSize:][:HeaderLen]
	if got := string(backup[0:8]); got != "EFI PART" {
		t.Errorf("backup GPT header in the last sector: got %q want \"EFI PART\"", got)
	}

	for _, c := range []struct {
		label string
		got   uint64
		want  uint64
	}{
		{"primary my LBA", leU64(primary, 24), 1},
		{"primary alternate LBA", leU64(primary, 32), total - 1},
		{"backup my LBA", leU64(backup, 24), total - 1},
		{"backup alternate LBA", leU64(backup, 32), 1},
		{"primary entry array LBA", leU64(primary, 72), 2},
		{"backup entry array LBA", leU64(backup, 72), total - BackupSectors},
		{"first usable LBA", leU64(primary, 40), FirstUsableLBA},
		{"last usable LBA", leU64(primary, 48), total - BackupSectors - 1},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %d want %d", c.label, c.got, c.want)
		}
	}
}

// TestIsDeterministic guards the reproducible build: the same volume in
// means the same map out, with no timestamp and no randomness, the GUIDs
// included.
func TestIsDeterministic(t *testing.T) {
	if !bytes.Equal(framed(t, 64, 7), framed(t, 64, 7)) {
		t.Errorf("two runs over the same volume disagree")
	}
}

// TestGUIDsFollowTheSeeds: the GUIDs are derived, not random, so they
// have to change when a seed changes and stay put when none does. Two
// images attached at one time are otherwise indistinguishable.
func TestGUIDsFollowTheSeeds(t *testing.T) {
	base, err := New(64*SectorSize, "Air", testTime)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, c := range []struct {
		label  string
		layout Layout
		want   bool
	}{
		{"the same seeds", mustNew(t, 64*SectorSize, "Air", testTime), true},
		{"a different volume name", mustNew(t, 64*SectorSize, "Fleet", testTime), false},
		{"a different timestamp", mustNew(t, 64*SectorSize, "Air", testTime.Add(time.Second)), false},
		{"a different size", mustNew(t, 128*SectorSize, "Air", testTime), false},
	} {
		if got := c.layout.diskGUID() == base.diskGUID(); got != c.want {
			t.Errorf("disk GUID with %s: equal=%v want %v", c.label, got, c.want)
		}
		if got := c.layout.partitionGUID() == base.partitionGUID(); got != c.want {
			t.Errorf("partition GUID with %s: equal=%v want %v", c.label, got, c.want)
		}
	}
	if base.diskGUID() == base.partitionGUID() {
		t.Errorf("the disk and the partition share one GUID; the tag does not separate them")
	}
}

func mustNew(t *testing.T, volumeSize uint64, name string, when time.Time) Layout {
	t.Helper()
	layout, err := New(volumeSize, name, when)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return layout
}

// TestNewRejectsARaggedVolume: a volume that is not a whole number of
// sectors puts the backup map at a non-sector boundary. Refuse it,
// rather than emit a disk no consumer reads.
func TestNewRejectsARaggedVolume(t *testing.T) {
	for _, size := range []uint64{0, 700, SectorSize + 1} {
		if _, err := New(size, "Air", testTime); err == nil {
			t.Errorf("New(%d) returned no error", size)
		}
	}
}

// TestCRC32MatchesTheStandardTestVector is a known-answer check on the
// CRC-32 GPT specifies, so a broken checksum shows up here and not as an
// unmountable disk. "123456789" is the standard CRC-32/ISO-HDLC test
// vector.
func TestCRC32MatchesTheStandardTestVector(t *testing.T) {
	if got := crc32.ChecksumIEEE([]byte("123456789")); got != 0xCBF43926 {
		t.Errorf("crc32(\"123456789\"): got %#x want 0xcbf43926", got)
	}
	if got := crc32.ChecksumIEEE(nil); got != 0 {
		t.Errorf("crc32(\"\"): got %#x want 0", got)
	}
}
