package dmg

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jetbrains/go-dmg-writer/internal/gpt"
	"github.com/jetbrains/go-dmg-writer/internal/hfsplus"
	"github.com/jetbrains/go-dmg-writer/internal/udif"
)

// makeSourceTree creates a small test directory with a fixed structure
// and returns its path. The caller passes t so cleanup is automatic.
func makeSourceTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "alpha.txt"), "alpha contents\n")
	mustWrite(t, filepath.Join(root, "beta.bin"), string(make([]byte, 1234)))
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "subdir", "nested.txt"), "in a sub-folder\n")
	return root
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCreateUDRO(t *testing.T) {
	src := makeSourceTree(t)
	out := filepath.Join(t.TempDir(), "out.dmg")
	d := &DMG{
		VolumeName: "TEST",
		Time:       time.Unix(1700000000, 0).UTC(),
	}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create UDRO: %v", err)
	}
	checkDMG(t, out /*compressed*/, false)
}

func TestCreateUDZO(t *testing.T) {
	src := makeSourceTree(t)
	out := filepath.Join(t.TempDir(), "out.dmg")
	d := &DMG{VolumeName: "Compressed", Time: time.Unix(1700000000, 0).UTC()}
	if err := d.Create(src, out, ModeReadOnlyCompressed); err != nil {
		t.Fatalf("Create UDZO: %v", err)
	}
	checkDMG(t, out, true)
}

func TestCreateDeterministic(t *testing.T) {
	src := makeSourceTree(t)
	out1 := filepath.Join(t.TempDir(), "out1.dmg")
	out2 := filepath.Join(t.TempDir(), "out2.dmg")
	fixedTime := time.Unix(1700000000, 0).UTC()
	for _, mode := range []Mode{ModeReadOnly, ModeReadOnlyCompressed} {
		d := &DMG{VolumeName: "DET", Time: fixedTime}
		if err := d.Create(src, out1, mode); err != nil {
			t.Fatalf("create 1 (mode=%d): %v", mode, err)
		}
		if err := d.Create(src, out2, mode); err != nil {
			t.Fatalf("create 2 (mode=%d): %v", mode, err)
		}
		h1 := sha256sum(t, out1)
		h2 := sha256sum(t, out2)
		if h1 != h2 {
			t.Errorf("mode=%d: output not deterministic\n  first:  %x\n  second: %x", mode, h1, h2)
		}
	}
}

func TestCreateLargeFile(t *testing.T) {
	src := t.TempDir()
	// File larger than one UDIF chunk (1 MiB) to exercise multi-chunk blkx.
	big := make([]byte, 3*1024*1024+777)
	for i := range big {
		big[i] = byte(i ^ (i >> 8))
	}
	if err := os.WriteFile(filepath.Join(src, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "big.dmg")
	d := &DMG{Time: time.Unix(1700000000, 0).UTC()}
	if err := d.Create(src, out, ModeReadOnlyCompressed); err != nil {
		t.Fatalf("Create: %v", err)
	}
	checkDMG(t, out, true)
}

func TestCreateSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevation on Windows")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "real.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "sym.dmg")
	d := &DMG{Time: time.Unix(1700000000, 0).UTC()}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create: %v", err)
	}
	checkDMG(t, out, false)
}

func TestCreateRejectsBadSource(t *testing.T) {
	out := filepath.Join(t.TempDir(), "bad.dmg")
	d := &DMG{}
	if err := d.Create("/nonexistent/path/should/fail", out, ModeReadOnly); err == nil {
		t.Fatal("expected error for missing source, got nil")
	}
}

func TestCreateRejectsNonDirectory(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "src-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	out := filepath.Join(t.TempDir(), "out.dmg")
	d := &DMG{}
	if err := d.Create(tmp.Name(), out, ModeReadOnly); err == nil {
		t.Fatal("expected error for non-directory source, got nil")
	}
}

func TestCreateRejectsInvalidMode(t *testing.T) {
	src := makeSourceTree(t)
	out := filepath.Join(t.TempDir(), "out.dmg")
	d := &DMG{}
	err := d.Create(src, out, Mode(99))
	if err == nil {
		t.Fatal("expected error for invalid Mode, got nil")
	}
	if !strings.Contains(err.Error(), "invalid Mode") {
		t.Errorf("error should mention invalid Mode, got: %v", err)
	}
}

// TestCreateOwnerIDUnset verifies the OwnerIDUnset sentinel resolves to
// the HFS+ "unknown user" (99) value the way hdiutil does, without
// confusing it with a literal UID 0.
func TestCreateOwnerIDUnset(t *testing.T) {
	src := makeSourceTree(t)
	out := filepath.Join(t.TempDir(), "ownerless.dmg")
	d := &DMG{
		VolumeName: "ownerless",
		Time:       time.Unix(1700000000, 0).UTC(),
		OwnerID:    OwnerIDUnset,
		GroupID:    OwnerIDUnset,
	}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create: %v", err)
	}
	checkDMG(t, out, false)
}

// TestCreateEmptyDirectory exercises the zero-files path: no user
// content, just the root catalog entry. fsck_hfs is happy with this.
func TestCreateEmptyDirectory(t *testing.T) {
	src := t.TempDir()
	out := filepath.Join(t.TempDir(), "empty.dmg")
	d := &DMG{VolumeName: "empty", Time: time.Unix(1700000000, 0).UTC()}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create: %v", err)
	}
	checkDMG(t, out, false)
}

// TestCreateRejectsUnreadableFile asserts we surface a clear error
// rather than silently producing a truncated image when a source file
// can't be opened. The 0o000 mode makes the file unreadable to anyone
// other than root, so we skip if running as root.
func TestCreateRejectsUnreadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows doesn't enforce POSIX 0o000 perms")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: 0o000 is still readable")
	}
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "secret"), []byte("can't touch this"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "secret"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Restore so t.TempDir cleanup can remove the file.
		_ = os.Chmod(filepath.Join(src, "secret"), 0o644)
	})

	out := filepath.Join(t.TempDir(), "perm.dmg")
	d := &DMG{Time: time.Unix(1700000000, 0).UTC()}
	if err := d.Create(src, out, ModeReadOnly); err == nil {
		t.Fatal("expected error opening unreadable file, got nil")
	}
}

// TestCreateSymlinkOutsideTree verifies we store an absolute symlink
// target verbatim (we don't try to canonicalise or rewrite paths).
func TestCreateSymlinkOutsideTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevation on Windows")
	}
	src := t.TempDir()
	const externalTarget = "/etc/hosts"
	if err := os.Symlink(externalTarget, filepath.Join(src, "external")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "extlink.dmg")
	d := &DMG{Time: time.Unix(1700000000, 0).UTC()}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The link target bytes should appear verbatim in the data fork
	// area of the image. We don't do a full HFS+ walk here; a substring
	// search over the produced .dmg is enough to assert that the bytes
	// landed.
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(externalTarget)) {
		t.Errorf("DMG should contain symlink target %q verbatim", externalTarget)
	}
}

// TestCreateRootFinderInfoBadSize asserts the error includes the actual
// length so callers get a useful diagnostic.
func TestCreateRootFinderInfoBadSize(t *testing.T) {
	src := makeSourceTree(t)
	out := filepath.Join(t.TempDir(), "fi.dmg")
	d := &DMG{
		Time:           time.Unix(1700000000, 0).UTC(),
		RootFinderInfo: make([]byte, 16), // wrong size
	}
	err := d.Create(src, out, ModeReadOnly)
	if err == nil {
		t.Fatal("expected error for short RootFinderInfo, got nil")
	}
	if !strings.Contains(err.Error(), "got 16") {
		t.Errorf("error should include actual length, got: %v", err)
	}
}

// TestCreateDeterministicWithRootFinderInfo verifies determinism is
// preserved when an extra xattr (the RootFinderInfo) is injected. The
// attributes B-tree's record ordering is stable and shouldn't drift
// across runs.
func TestCreateDeterministicWithRootFinderInfo(t *testing.T) {
	src := makeSourceTree(t)
	finderInfo := make([]byte, 32)
	for i := range finderInfo {
		finderInfo[i] = byte(i + 1)
	}
	out1 := filepath.Join(t.TempDir(), "out1.dmg")
	out2 := filepath.Join(t.TempDir(), "out2.dmg")
	fixed := time.Unix(1700000000, 0).UTC()
	for _, mode := range []Mode{ModeReadOnly, ModeReadOnlyCompressed} {
		d := &DMG{VolumeName: "DET", Time: fixed, RootFinderInfo: finderInfo}
		if err := d.Create(src, out1, mode); err != nil {
			t.Fatalf("create 1 (mode=%s): %v", mode, err)
		}
		if err := d.Create(src, out2, mode); err != nil {
			t.Fatalf("create 2 (mode=%s): %v", mode, err)
		}
		h1 := sha256sum(t, out1)
		h2 := sha256sum(t, out2)
		if h1 != h2 {
			t.Errorf("mode=%s: output not deterministic with RootFinderInfo\n  first:  %x\n  second: %x",
				mode, h1, h2)
		}
	}
}

// checkDMG performs format-level sanity checks on a produced DMG: KOLY
// trailer is valid, sector count is sensible, and (for compressed images)
// the data fork is smaller than the partition. It returns the whole file
// and its decoded trailer so a caller can go on to assert on the image
// without opening and parsing it a second time.
func checkDMG(t *testing.T, path string, expectCompressed bool) ([]byte, *udif.Koly) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(raw) < udif.KolySize {
		t.Fatalf("output too small: %d bytes", len(raw))
	}
	koly, err := udif.DecodeKoly(raw[len(raw)-udif.KolySize:])
	if err != nil {
		t.Fatalf("DecodeKoly: %v", err)
	}
	if koly.Signature != udif.KolySignature {
		t.Errorf("Signature: got %#x want %#x", koly.Signature, udif.KolySignature)
	}
	if koly.Version != udif.KolyVersion {
		t.Errorf("Version: got %d want %d", koly.Version, udif.KolyVersion)
	}
	if koly.SectorCount == 0 {
		t.Error("SectorCount is zero")
	}
	if koly.XMLLength == 0 {
		t.Error("XMLLength is zero")
	}
	if koly.XMLOffset != koly.DataForkLength {
		t.Errorf("XMLOffset (%d) != DataForkLength (%d)", koly.XMLOffset, koly.DataForkLength)
	}
	if expectCompressed {
		// Compressed DMG should be (substantially) smaller than the
		// partition it describes.
		partitionBytes := koly.SectorCount * uint64(udif.SectorSize)
		if koly.DataForkLength >= partitionBytes {
			t.Errorf("UDZO compression failed: data fork %d >= partition %d", koly.DataForkLength, partitionBytes)
		}
	}

	// The XML plist between the data fork and KOLY must look like a
	// plist and reference a blkx entry.
	xml := imageXML(t, raw, koly)
	if !bytes.Contains(xml, []byte("<key>resource-fork</key>")) {
		t.Errorf("XML missing resource-fork key:\n%s", xml)
	}
	if !bytes.Contains(xml, []byte("<key>blkx</key>")) {
		t.Errorf("XML missing blkx key")
	}
	return raw, koly
}

// imageXML is the resource-fork plist between the data fork and the KOLY.
func imageXML(t *testing.T, raw []byte, koly *udif.Koly) []byte {
	t.Helper()
	end := koly.XMLOffset + koly.XMLLength
	if end > uint64(len(raw)) {
		t.Fatalf("XML at %d+%d runs past the %d-byte image", koly.XMLOffset, koly.XMLLength, len(raw))
	}
	return raw[koly.XMLOffset:end]
}

// blkxRun is the subset of a BLKX entry the tests assert on.
type blkxRun struct {
	runType     uint32
	sectorStart uint64
	sectorCount uint64
	compOffset  uint64
	compLength  uint64
}

// blkxTable pulls the blkx resource out of an image's XML plist and
// decodes the mish table's chunk geometry. It returns
// DecompressBufRequested (in sectors) and every run, terminator included.
func blkxTable(t *testing.T, raw []byte, koly *udif.Koly) (uint32, []blkxRun) {
	t.Helper()
	xml := imageXML(t, raw, koly)

	// The blkx array is the first resource in the fork, so its <data>
	// block is the first one after the key.
	key := bytes.Index(xml, []byte("<key>blkx</key>"))
	if key < 0 {
		t.Fatalf("no blkx key in the plist:\n%s", xml)
	}
	rest := xml[key:]
	start := bytes.Index(rest, []byte("<data>"))
	end := bytes.Index(rest, []byte("</data>"))
	if start < 0 || end < start {
		t.Fatalf("no <data> block after the blkx key:\n%s", rest)
	}
	b64 := strings.Join(strings.Fields(string(rest[start+len("<data>"):end])), "")
	table, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode blkx base64: %v", err)
	}

	// mish header: the 'mish' magic, then the fields at the offsets
	// udif.BlkxTable.Encode writes. The run array follows the 204-byte
	// header, 40 bytes per run.
	const (
		headerSize = 204
		runSize    = 40
	)
	if len(table) < headerSize {
		t.Fatalf("blkx too short: %d bytes", len(table))
	}
	if got := binary.BigEndian.Uint32(table[0:4]); got != udif.MishSignature {
		t.Fatalf("blkx signature: got %#x want %#x", got, udif.MishSignature)
	}
	decompressBuf := binary.BigEndian.Uint32(table[32:36])
	count := int(binary.BigEndian.Uint32(table[200:204]))
	if want := headerSize + count*runSize; len(table) != want {
		t.Fatalf("blkx is %d bytes, want %d for %d runs", len(table), want, count)
	}
	runs := make([]blkxRun, count)
	for i := range runs {
		r := table[headerSize+i*runSize:][:runSize]
		runs[i] = blkxRun{
			runType:     binary.BigEndian.Uint32(r[0:4]),
			sectorStart: binary.BigEndian.Uint64(r[8:16]),
			sectorCount: binary.BigEndian.Uint64(r[16:24]),
			compOffset:  binary.BigEndian.Uint64(r[24:32]),
			compLength:  binary.BigEndian.Uint64(r[32:40]),
		}
	}
	if count == 0 || runs[count-1].runType != udif.BlockTerminator {
		t.Fatalf("blkx does not end with a terminator run (%d runs)", count)
	}
	return decompressBuf, runs
}

// logicalDisk rebuilds what the image describes, sector for sector, by
// replaying the BLKX table over the data fork: zero runs expand back to
// zeros, raw runs are copied, zlib runs are inflated.
//
// Byte offsets in the file are NOT disk offsets. An all-zero chunk costs
// no bytes in the data fork, and a compressed one costs fewer, so any
// assertion about a particular sector has to be made against this buffer
// rather than against the file.
func logicalDisk(t *testing.T, raw []byte, koly *udif.Koly) []byte {
	t.Helper()
	_, runs := blkxTable(t, raw, koly)
	forkEnd := koly.DataForkOffset + koly.DataForkLength
	if forkEnd > uint64(len(raw)) {
		t.Fatalf("data fork at %d+%d runs past the %d-byte image", koly.DataForkOffset, koly.DataForkLength, len(raw))
	}
	fork := raw[koly.DataForkOffset:forkEnd]

	disk := make([]byte, koly.SectorCount*udif.SectorSize)
	for i, r := range runs {
		if r.runType == udif.BlockTerminator {
			continue
		}
		at := r.sectorStart * udif.SectorSize
		size := r.sectorCount * udif.SectorSize
		if at+size > uint64(len(disk)) {
			t.Fatalf("run %d covers sectors %d..%d, past the volume's %d", i, r.sectorStart, r.sectorStart+r.sectorCount, koly.SectorCount)
		}
		if r.compOffset+r.compLength > uint64(len(fork)) {
			t.Fatalf("run %d reads %d+%d of a %d-byte data fork", i, r.compOffset, r.compLength, len(fork))
		}
		payload := fork[r.compOffset : r.compOffset+r.compLength]

		switch r.runType {
		case udif.BlockZero, udif.BlockZeroFill:
			// The buffer is already zeroed; a zero run stores no bytes.
		case udif.BlockRaw:
			if uint64(len(payload)) != size {
				t.Fatalf("raw run %d stores %d bytes for %d sectors", i, len(payload), r.sectorCount)
			}
			copy(disk[at:], payload)
		case udif.BlockZLIB:
			zr, err := zlib.NewReader(bytes.NewReader(payload))
			if err != nil {
				t.Fatalf("run %d: zlib reader: %v", i, err)
			}
			plain, err := io.ReadAll(zr)
			if err != nil {
				t.Fatalf("run %d: inflate: %v", i, err)
			}
			if err := zr.Close(); err != nil {
				t.Fatalf("run %d: zlib close: %v", i, err)
			}
			if uint64(len(plain)) != size {
				t.Fatalf("run %d inflates to %d bytes, want %d", i, len(plain), size)
			}
			copy(disk[at:], plain)
		default:
			t.Fatalf("run %d has unexpected type %#x", i, r.runType)
		}
	}
	return disk
}

func sha256sum(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// TestCreatePartitionMap checks the GPT framing end to end: the
// protective MBR, the primary header and the partition entry all decode,
// and the volume lands at the partition's first LBA. Every offset is read
// out of the replayed disk, not out of the file, so the assertions hold
// for a compressed image too.
func TestCreatePartitionMap(t *testing.T) {
	src := makeSourceTree(t)
	when := time.Unix(1700000000, 0).UTC()

	for _, mode := range []Mode{ModeReadOnly, ModeReadOnlyCompressed} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := t.TempDir()
			bare, framed := filepath.Join(dir, "bare.dmg"), filepath.Join(dir, "framed.dmg")
			if err := (&DMG{VolumeName: "TEST", Time: when}).Create(src, bare, mode); err != nil {
				t.Fatalf("Create without a map: %v", err)
			}
			if err := (&DMG{VolumeName: "TEST", Time: when, PartitionMap: true}).Create(src, framed, mode); err != nil {
				t.Fatalf("Create with a map: %v", err)
			}
			compressed := mode == ModeReadOnlyCompressed
			_, bareKoly := checkDMG(t, bare, compressed)
			raw, koly := checkDMG(t, framed, compressed)

			// The map is the only difference: the same volume, plus the
			// leading and trailing sectors the framing costs.
			bareSectors, framedSectors := bareKoly.SectorCount, koly.SectorCount
			if want := bareSectors + gpt.PartitionStartLBA + gpt.BackupSectors; framedSectors != want {
				t.Errorf("framed sector count: got %d want %d (%d volume sectors plus the map)", framedSectors, want, bareSectors)
			}

			// The KOLY keeps saying "partition" even though the data fork
			// is now a whole disk: hdiutil refuses an image whose variant
			// is "device" unless the resource fork also splits the BLKX
			// table per region, which this writer does not do. See the
			// gpt package doc.
			if got := koly.ImageVariant; got != udif.ImageVariantPartition {
				t.Errorf("framed ImageVariant: got %d want %d (partition)", got, udif.ImageVariantPartition)
			}

			disk := logicalDisk(t, raw, koly)
			if want := int(framedSectors) * gpt.SectorSize; len(disk) != want {
				t.Fatalf("replayed disk is %d bytes, want %d", len(disk), want)
			}

			if got := disk[510:512]; !bytes.Equal(got, []byte{0x55, 0xAA}) {
				t.Errorf("protective MBR signature: got %v want [85 170]", got)
			}
			if got := disk[450]; got != 0xEE {
				t.Errorf("MBR partition type: got %#x want 0xee, the GPT protective type", got)
			}
			if got := string(disk[gpt.SectorSize : gpt.SectorSize+8]); got != "EFI PART" {
				t.Errorf("primary GPT header: got %q want \"EFI PART\"", got)
			}

			entry := disk[gpt.EntryArrayLBA*gpt.SectorSize:][:gpt.EntrySize]
			if !bytes.Equal(entry[0:16], gpt.AppleHFSTypeGUID[:]) {
				t.Errorf("partition type GUID: got %v want the Apple HFS GUID %v", entry[0:16], gpt.AppleHFSTypeGUID)
			}
			if got := binary.LittleEndian.Uint64(entry[32:40]); got != gpt.PartitionStartLBA {
				t.Errorf("partition first LBA: got %d want %d", got, gpt.PartitionStartLBA)
			}
			if got, want := binary.LittleEndian.Uint64(entry[40:48]), gpt.PartitionStartLBA+bareSectors-1; got != want {
				t.Errorf("partition last LBA: got %d want %d", got, want)
			}

			// The volume header sits 1024 bytes into the volume, which now
			// sits at the partition's first LBA rather than at offset 0.
			vh := disk[gpt.PartitionStartLBA*gpt.SectorSize+1024:]
			if got := binary.BigEndian.Uint16(vh[0:2]); got != hfsplus.VolumeSignature {
				t.Errorf("HFSX signature at the partition start: got %#x want %#x", got, hfsplus.VolumeSignature)
			}
			// And the backup header closes the last sector of the disk.
			last := disk[(framedSectors-1)*gpt.SectorSize:][:8]
			if got := string(last); got != "EFI PART" {
				t.Errorf("backup GPT header in the last sector: got %q want \"EFI PART\"", got)
			}
		})
	}
}

// TestCreatePartitionMapDeterministic guards the reproducible build: the
// framing carries no timestamp and no random GUID.
func TestCreatePartitionMapDeterministic(t *testing.T) {
	src := makeSourceTree(t)
	dir := t.TempDir()
	when := time.Unix(1700000000, 0).UTC()

	var sums [2][32]byte
	for i := range sums {
		out := filepath.Join(dir, fmt.Sprintf("run%d.dmg", i))
		d := &DMG{VolumeName: "TEST", Time: when, PartitionMap: true}
		if err := d.Create(src, out, ModeReadOnlyCompressed); err != nil {
			t.Fatalf("Create run %d: %v", i, err)
		}
		sums[i] = sha256sum(t, out)
	}
	if sums[0] != sums[1] {
		t.Errorf("two runs with a partition map differ: %x != %x", sums[0], sums[1])
	}
}

// TestCreateEmptySourcePartitionMapNonEmptyVolume pins the interaction
// between the two "nothing to write" paths: an empty source folder still
// produces a real HFS+ volume (the B-trees and the allocation file exist
// even with no user content), so the GPT framing must describe a
// non-empty partition rather than a zero-length one.
func TestCreateEmptySourcePartitionMapNonEmptyVolume(t *testing.T) {
	src := t.TempDir() // no files at all
	out := filepath.Join(t.TempDir(), "empty-gpt.dmg")
	d := &DMG{VolumeName: "empty", Time: time.Unix(1700000000, 0).UTC(), PartitionMap: true}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create: %v", err)
	}
	raw, koly := checkDMG(t, out /*compressed*/, false)

	// Whatever is left after the leading and trailing map sectors is the
	// volume, and there has to be some of it.
	volumeSectors := int64(koly.SectorCount) - gpt.PartitionStartLBA - gpt.BackupSectors
	if volumeSectors <= 0 {
		t.Fatalf("volume sectors: got %d, want a non-empty volume (total %d sectors)", volumeSectors, koly.SectorCount)
	}

	disk := logicalDisk(t, raw, koly)

	// The partition entry has to cover exactly those sectors.
	entry := disk[gpt.EntryArrayLBA*gpt.SectorSize:][:gpt.EntrySize]
	first := binary.LittleEndian.Uint64(entry[32:40])
	last := binary.LittleEndian.Uint64(entry[40:48])
	if first != gpt.PartitionStartLBA {
		t.Errorf("partition first LBA: got %d want %d", first, gpt.PartitionStartLBA)
	}
	if want := uint64(volumeSectors); last-first+1 != want {
		t.Errorf("partition covers %d sectors, want %d", last-first+1, want)
	}

	// And the volume itself is a formatted HFSX volume with allocation
	// blocks, not a hole full of zeros.
	vh := disk[gpt.PartitionStartLBA*gpt.SectorSize+1024:]
	if got := binary.BigEndian.Uint16(vh[0:2]); got != hfsplus.VolumeSignature {
		t.Fatalf("HFSX signature at the partition start: got %#x want %#x", got, hfsplus.VolumeSignature)
	}
	blockSize := binary.BigEndian.Uint32(vh[40:44])
	totalBlocks := binary.BigEndian.Uint32(vh[44:48])
	if blockSize == 0 || totalBlocks == 0 {
		t.Fatalf("empty volume header: blockSize=%d totalBlocks=%d", blockSize, totalBlocks)
	}
	if got, want := uint64(blockSize)*uint64(totalBlocks), uint64(volumeSectors)*gpt.SectorSize; got != want {
		t.Errorf("volume describes %d bytes, but the partition holds %d", got, want)
	}
}

// TestCreateChunkSectors checks that [DMG.ChunkSectors] actually reaches
// [udif.Options]: the udif tests cover the option itself, but nothing
// covered the wiring from Create, so dropping the field assignment in
// dmg.go would have gone unnoticed.
func TestCreateChunkSectors(t *testing.T) {
	src := t.TempDir()
	// A payload big enough to span many chunks at the small chunk size.
	body := make([]byte, 512*1024)
	for i := range body {
		body[i] = byte(i ^ (i >> 11))
	}
	mustWrite(t, filepath.Join(src, "payload.bin"), string(body))

	const chunkSectors = 8 // 4 KiB, far below the 2048-sector default
	out := filepath.Join(t.TempDir(), "chunked.dmg")
	d := &DMG{
		VolumeName:   "chunked",
		Time:         time.Unix(1700000000, 0).UTC(),
		ChunkSectors: chunkSectors,
	}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create: %v", err)
	}
	raw, koly := checkDMG(t, out /*compressed*/, false)

	decompressBuf, runs := blkxTable(t, raw, koly)
	if decompressBuf != chunkSectors {
		t.Errorf("blkx DecompressBufRequested: got %d want %d sectors", decompressBuf, chunkSectors)
	}

	// Every run but the terminator is at most one chunk long, and the
	// runs tile the volume without a gap.
	var covered uint64
	for i, r := range runs {
		if r.sectorStart != covered {
			t.Fatalf("run %d starts at sector %d, want %d", i, r.sectorStart, covered)
		}
		if r.sectorCount > chunkSectors {
			t.Errorf("run %d spans %d sectors, more than the %d-sector chunk", i, r.sectorCount, chunkSectors)
		}
		covered += r.sectorCount
	}
	if covered != koly.SectorCount {
		t.Errorf("runs cover %d sectors, want the volume's %d", covered, koly.SectorCount)
	}
	// ceil(SectorCount / chunkSectors) data runs, plus the terminator.
	if want := int((koly.SectorCount+chunkSectors-1)/chunkSectors) + 1; len(runs) != want {
		t.Errorf("run count: got %d want %d (%d sectors in %d-sector chunks)", len(runs), want, koly.SectorCount, chunkSectors)
	}
}
