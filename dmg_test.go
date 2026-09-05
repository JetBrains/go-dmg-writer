package dmg

import (
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
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

// TestCreateZeroFileChecksum guards the run type a checksum must skip.
// A file of zeros gives whole chunks of zeros, and every one of them
// becomes a BlockZero run that stores no bytes. Those sectors must stay
// out of the table CRC. A writer that counts them writes a checksum
// macOS rejects, and `hdiutil verify` is then the only thing that
// notices, because the image still replays sector for sector.
func TestCreateZeroFileChecksum(t *testing.T) {
	src := t.TempDir()
	// Several chunks of zeros, so the run coalesces rather than
	// depending on where one chunk ends.
	mustWrite(t, filepath.Join(src, "hole.bin"), string(make([]byte, 6*1024*1024)))
	mustWrite(t, filepath.Join(src, "alpha.txt"), "alpha contents\n")

	for _, mode := range []Mode{ModeReadOnly, ModeReadOnlyCompressed} {
		for _, withMap := range []bool{false, true} {
			name := mode.String()
			if withMap {
				name += "/PartitionMap"
			}
			t.Run(name, func(t *testing.T) {
				out := filepath.Join(t.TempDir(), "zero.dmg")
				d := &DMG{VolumeName: "TEST", Time: time.Unix(1700000000, 0).UTC(), PartitionMap: withMap}
				if err := d.Create(src, out, mode); err != nil {
					t.Fatalf("Create: %v", err)
				}
				// checkDMG holds every checksum against the sectors it
				// covers; this test only has to prove the skipped run
				// type is really in the image.
				raw, koly := checkDMG(t, out, mode == ModeReadOnlyCompressed)

				var zeroRuns, zeroSectors uint64
				for _, reg := range blkxRegions(t, raw, koly) {
					for _, r := range reg.runs {
						if r.runType == udif.BlockZero {
							zeroRuns++
							zeroSectors += r.sectorCount
						}
					}
				}
				if zeroRuns == 0 {
					t.Fatal("no BlockZero run in the image, so this test proves nothing")
				}
				// The file holds 12288 sectors of zeros. The chunk at
				// each end of it also holds volume data, so it stays a
				// raw or a zlib run; every chunk between them is a zero
				// run. The default chunk is 2048 sectors.
				if want := uint64(6*1024*1024/udif.SectorSize) - 2*2048; zeroSectors < want {
					t.Errorf("%d BlockZero runs cover %d sectors, want at least %d", zeroRuns, zeroSectors, want)
				}
			})
		}
	}
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

// TestCreateTempDir covers where the scratch volume lands. The default
// is the output directory rather than TMPDIR, because the scratch file
// is the whole uncompressed volume and TMPDIR is often small or in RAM.
func TestCreateTempDir(t *testing.T) {
	// An explicit TempDir is used, and the scratch file is cleaned up.
	t.Run("explicit", func(t *testing.T) {
		src := makeSourceTree(t)
		scratchDir := t.TempDir()
		out := filepath.Join(t.TempDir(), "explicit.dmg")
		d := &DMG{TempDir: scratchDir, Time: time.Unix(1700000000, 0).UTC()}
		if err := d.Create(src, out, ModeReadOnly); err != nil {
			t.Fatalf("Create: %v", err)
		}
		left, err := os.ReadDir(scratchDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(left) != 0 {
			t.Errorf("scratch file left behind in TempDir: %v", left)
		}
	})

	// A TempDir that does not exist is a caller error and has to be
	// reported, not silently swapped for TMPDIR.
	t.Run("nonexistent", func(t *testing.T) {
		src := makeSourceTree(t)
		out := filepath.Join(t.TempDir(), "bad.dmg")
		d := &DMG{TempDir: filepath.Join(t.TempDir(), "no", "such", "dir")}
		if err := d.Create(src, out, ModeReadOnly); err == nil {
			t.Fatal("Create accepted a TempDir that does not exist")
		}
	})

	// The default leaves nothing behind in the output directory either.
	t.Run("default is the output directory", func(t *testing.T) {
		src := makeSourceTree(t)
		outDir := t.TempDir()
		out := filepath.Join(outDir, "default.dmg")
		d := &DMG{Time: time.Unix(1700000000, 0).UTC()}
		if err := d.Create(src, out, ModeReadOnly); err != nil {
			t.Fatalf("Create: %v", err)
		}
		ents, err := os.ReadDir(outDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 1 || ents[0].Name() != "default.dmg" {
			t.Errorf("output directory should hold only the image, got %v", ents)
		}
	})
}

// TestCreateRejectsOversizedVolumeName: the volume name IS the root
// folder's catalog name, so a name the catalog cannot hold has to fail.
// It used to be dropped on the floor, producing an image with no volume
// name at all and a nil error.
func TestCreateRejectsOversizedVolumeName(t *testing.T) {
	src := makeSourceTree(t)
	out := filepath.Join(t.TempDir(), "longname.dmg")
	d := &DMG{
		VolumeName: strings.Repeat("A", 300), // over the 255 code-unit limit
		Time:       time.Unix(1700000000, 0).UTC(),
	}
	err := d.Create(src, out, ModeReadOnly)
	if err == nil {
		t.Fatal("Create accepted a 300-character volume name")
	}
	if !strings.Contains(err.Error(), "volume name") {
		t.Errorf("error should name the offending field, got: %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("Create left an output file behind after rejecting the volume name")
	}
}

// TestCreateAcceptsMaxLengthVolumeName is the other side of the bound:
// exactly 255 code units still has to work.
func TestCreateAcceptsMaxLengthVolumeName(t *testing.T) {
	src := makeSourceTree(t)
	out := filepath.Join(t.TempDir(), "maxname.dmg")
	d := &DMG{
		VolumeName: strings.Repeat("A", 255),
		Time:       time.Unix(1700000000, 0).UTC(),
	}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create rejected a 255-character volume name: %v", err)
	}
}

// TestCreateRejectsNamesCollidingUnderNFD: HFS+ catalog keys hold the NFD
// form of a name, so "é" written as U+00E9 and as "e" plus U+0301 are one
// key. A Linux filesystem keeps both files; the catalog cannot. Writing
// both used to succeed and produce a catalog with two equal keys.
func TestCreateRejectsNamesCollidingUnderNFD(t *testing.T) {
	precomposed := "caf\u00e9.txt" // é as one code point
	decomposed := "cafe\u0301.txt" // e + combining acute
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, precomposed), "one\n")
	if err := os.WriteFile(filepath.Join(root, decomposed), []byte("two\n"), 0o644); err != nil {
		// A filesystem that normalizes names itself (APFS, HFS+) stores
		// these as one file, so there is nothing to collide.
		t.Skipf("filesystem cannot hold both spellings: %v", err)
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) < 2 {
		t.Skip("filesystem normalized the two names into one file")
	}

	out := filepath.Join(t.TempDir(), "nfd.dmg")
	d := &DMG{Time: time.Unix(1700000000, 0).UTC()}
	err = d.Create(root, out, ModeReadOnly)
	if err == nil {
		t.Fatal("Create accepted two names that normalize to the same catalog key")
	}
	// The message has to name both paths; that is the whole point of
	// catching this in the walk rather than in the packer.
	if !strings.Contains(err.Error(), precomposed) || !strings.Contains(err.Error(), decomposed) {
		t.Errorf("error should name both source paths, got: %v", err)
	}
}

// TestCreateDeterministicWithRootFinderInfo verifies determinism is
// preserved when an extra xattr (the RootFinderInfo) is injected. The
// attributes B-tree's record ordering are stable and shouldn't drift
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

	checkChecksums(t, raw, koly)
	return raw, koly
}

// checkChecksums recomputes every checksum the image carries and holds it
// against the stored value. hdiutil makes this same check when it
// verifies an image, and it is the one check the rest of the suite cannot
// stand in for: an image whose sectors all replay correctly is still
// broken, and macOS still rejects it, if a checksum counts the wrong
// sectors.
func checkChecksums(t *testing.T, raw []byte, koly *udif.Koly) {
	t.Helper()
	regions := blkxRegions(t, raw, koly)
	forkEnd := koly.DataForkOffset + koly.DataForkLength
	if forkEnd > uint64(len(raw)) {
		t.Fatalf("data fork at %d+%d runs past the %d-byte image", koly.DataForkOffset, koly.DataForkLength, len(raw))
	}
	fork := raw[koly.DataForkOffset:forkEnd]

	perTable := make([]uint32, 0, len(regions))
	forkCRC := crc32.NewIEEE()
	for _, reg := range regions {
		tableCRC := crc32.NewIEEE()
		for i, r := range reg.runs {
			// A BlockZero run marks free space that a reader must not
			// touch, so its sectors stay out of the CRC. The sectors
			// of a BlockZeroFill run go in as zeros. The two run
			// types look alike on the disk and differ only here.
			if r.runType == udif.BlockTerminator || r.runType == udif.BlockZero {
				continue
			}
			_, _ = io.MultiWriter(tableCRC, forkCRC).Write(runBytes(t, fork, reg, i, r))
		}
		if got, want := reg.checksum, tableCRC.Sum32(); got != want {
			t.Errorf("blkx %q checksum: the table says %#08x, its own sectors give %#08x", reg.name, got, want)
		}
		perTable = append(perTable, reg.checksum)
	}
	if got, want := koly.MasterChecksum.Data[0], udif.MasterChecksum(perTable); got != want {
		t.Errorf("MasterChecksum over the %d table checksums: got %#08x want %#08x", len(perTable), got, want)
	}
	// The data fork checksum covers the same sectors as the tables, end
	// to end, in one pass over the whole image.
	if got, want := koly.DataForkChecksum.Data[0], forkCRC.Sum32(); got != want {
		t.Errorf("DataForkChecksum: the trailer says %#08x, the sectors give %#08x", got, want)
	}
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

// blkxRegion is one decoded blkx resource. A map-less image has exactly
// one; a framed image has one per region of its partition map.
type blkxRegion struct {
	name          string
	firstSector   uint64
	sectorCount   uint64
	descriptor    uint32
	checksum      uint32
	decompressBuf uint32
	runs          []blkxRun
}

// blkxRegions pulls every blkx resource out of an image's XML plist and
// decodes each mish table. SectorStart inside a run counts from the
// region's own first sector, not from the start of the disk.
func blkxRegions(t *testing.T, raw []byte, koly *udif.Koly) []blkxRegion {
	t.Helper()
	xml := imageXML(t, raw, koly)

	key := bytes.Index(xml, []byte("<key>blkx</key>"))
	if key < 0 {
		t.Fatalf("no blkx key in the plist:\n%s", xml)
	}
	rest := xml[key:]
	if end := bytes.Index(rest, []byte("</array>")); end >= 0 {
		rest = rest[:end]
	}

	// mish header: the 'mish' magic, then the fields at the offsets
	// udif.BlkxTable.Encode writes. The run array follows the 204-byte
	// header, 40 bytes per run.
	const (
		headerSize = 204
		runSize    = 40
	)
	var out []blkxRegion
	for {
		start := bytes.Index(rest, []byte("<data>"))
		if start < 0 {
			break
		}
		end := bytes.Index(rest, []byte("</data>"))
		if end < start {
			t.Fatalf("unterminated <data> block in the plist:\n%s", rest)
		}

		// CFName is the last <string> in front of the payload.
		name := ""
		if open := bytes.LastIndex(rest[:start], []byte("<string>")); open >= 0 {
			if shut := bytes.Index(rest[open:start], []byte("</string>")); shut > 0 {
				name = string(rest[open+len("<string>") : open+shut])
			}
		}

		b64 := strings.Join(strings.Fields(string(rest[start+len("<data>"):end])), "")
		table, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("decode blkx base64 for %q: %v", name, err)
		}
		if len(table) < headerSize {
			t.Fatalf("blkx %q too short: %d bytes", name, len(table))
		}
		if got := binary.BigEndian.Uint32(table[0:4]); got != udif.MishSignature {
			t.Fatalf("blkx %q signature: got %#x want %#x", name, got, udif.MishSignature)
		}
		count := int(binary.BigEndian.Uint32(table[200:204]))
		if want := headerSize + count*runSize; len(table) != want {
			t.Fatalf("blkx %q is %d bytes, want %d for %d runs", name, len(table), want, count)
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
			t.Fatalf("blkx %q does not end with a terminator run (%d runs)", name, count)
		}
		out = append(out, blkxRegion{
			name:          name,
			firstSector:   binary.BigEndian.Uint64(table[8:16]),
			sectorCount:   binary.BigEndian.Uint64(table[16:24]),
			descriptor:    binary.BigEndian.Uint32(table[36:40]),
			checksum:      binary.BigEndian.Uint32(table[72:76]),
			decompressBuf: binary.BigEndian.Uint32(table[32:36]),
			runs:          runs,
		})
		rest = rest[end+len("</data>"):]
	}
	if len(out) == 0 {
		t.Fatalf("no blkx resource in the plist:\n%s", xml)
	}
	return out
}

// blkxTable returns the single table of a map-less image: its
// DecompressBufRequested (in sectors) and every run, terminator included.
func blkxTable(t *testing.T, raw []byte, koly *udif.Koly) (uint32, []blkxRun) {
	t.Helper()
	regions := blkxRegions(t, raw, koly)
	if len(regions) != 1 {
		t.Fatalf("expected a single blkx table, got %d", len(regions))
	}
	return regions[0].decompressBuf, regions[0].runs
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
	regions := blkxRegions(t, raw, koly)
	forkEnd := koly.DataForkOffset + koly.DataForkLength
	if forkEnd > uint64(len(raw)) {
		t.Fatalf("data fork at %d+%d runs past the %d-byte image", koly.DataForkOffset, koly.DataForkLength, len(raw))
	}
	fork := raw[koly.DataForkOffset:forkEnd]

	disk := make([]byte, koly.SectorCount*udif.SectorSize)
	for _, reg := range regions {
		for i, r := range reg.runs {
			if r.runType == udif.BlockTerminator {
				continue
			}
			// A run addresses its own region; the region says where
			// that sits on the disk.
			at := (reg.firstSector + r.sectorStart) * udif.SectorSize
			if at+r.sectorCount*udif.SectorSize > uint64(len(disk)) {
				t.Fatalf("%q run %d covers sectors %d..%d, past the disk's %d",
					reg.name, i, reg.firstSector+r.sectorStart, reg.firstSector+r.sectorStart+r.sectorCount, koly.SectorCount)
			}
			copy(disk[at:], runBytes(t, fork, reg, i, r))
		}
	}
	return disk
}

// runBytes expands one run to the sectors it stands for: a zero run to
// zeros, a raw run to its bytes in the data fork, a zlib run to what
// those bytes inflate to. The terminator run has no sectors.
func runBytes(t *testing.T, fork []byte, reg blkxRegion, i int, r blkxRun) []byte {
	t.Helper()
	if r.compOffset+r.compLength > uint64(len(fork)) {
		t.Fatalf("%q run %d reads %d+%d of a %d-byte data fork", reg.name, i, r.compOffset, r.compLength, len(fork))
	}
	payload := fork[r.compOffset : r.compOffset+r.compLength]
	size := r.sectorCount * udif.SectorSize

	switch r.runType {
	case udif.BlockTerminator:
		return nil
	case udif.BlockZero, udif.BlockZeroFill:
		// A zero run stores no bytes in the data fork.
		return make([]byte, size)
	case udif.BlockRaw:
		if uint64(len(payload)) != size {
			t.Fatalf("%q raw run %d stores %d bytes for %d sectors", reg.name, i, len(payload), r.sectorCount)
		}
		return payload
	case udif.BlockZLIB:
		zr, err := zlib.NewReader(bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("%q run %d: zlib reader: %v", reg.name, i, err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("%q run %d: inflate: %v", reg.name, i, err)
		}
		if err := zr.Close(); err != nil {
			t.Fatalf("%q run %d: zlib close: %v", reg.name, i, err)
		}
		if uint64(len(plain)) != size {
			t.Fatalf("%q run %d inflates to %d bytes, want %d", reg.name, i, len(plain), size)
		}
		return plain
	default:
		t.Fatalf("%q run %d has unexpected type %#x", reg.name, i, r.runType)
		return nil
	}
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

			// The data fork is a whole disk, so the KOLY says "device"
			// and the resource fork carries one blkx table per region.
			if got := koly.ImageVariant; got != udif.ImageVariantDevice {
				t.Errorf("framed ImageVariant: got %d want %d (device)", got, udif.ImageVariantDevice)
			}
			if got := bareKoly.ImageVariant; got != udif.ImageVariantPartition {
				t.Errorf("map-less ImageVariant: got %d want %d (partition)", got, udif.ImageVariantPartition)
			}

			// One resource per region, named the way hdiutil names them,
			// covering the disk end to end with no gap and no overlap.
			// A reader that parses the image without mounting it starts
			// here, so the names carry real weight.
			regions := blkxRegions(t, raw, koly)
			wantRegions := []struct {
				name    string
				sectors uint64
			}{
				{"Protective Master Boot Record (MBR : 0)", 1},
				{"GPT Header (Primary GPT Header : 1)", 1},
				{"GPT Partition Data (Primary GPT Table : 2)", gpt.EntryArraySectors},
				{" (Apple_Free : 3)", gpt.PartitionStartLBA - gpt.FirstUsableLBA},
				// Apple_HFSX, not Apple_HFS: hdiutil takes this string
				// from the filesystem rather than the partition type
				// GUID, and this library always writes HFSX.
				{"disk image (Apple_HFSX : 4)", bareSectors},
				{"GPT Partition Data (Backup GPT Table : 5)", gpt.EntryArraySectors},
				{"GPT Header (Backup GPT Header : 6)", 1},
			}
			if len(regions) != len(wantRegions) {
				t.Fatalf("framed image has %d blkx resources, want %d", len(regions), len(wantRegions))
			}
			var at uint64
			for i, want := range wantRegions {
				got := regions[i]
				if got.name != want.name {
					t.Errorf("region %d name: got %q want %q", i, got.name, want.name)
				}
				if got.sectorCount != want.sectors {
					t.Errorf("region %d (%s) covers %d sectors, want %d", i, want.name, got.sectorCount, want.sectors)
				}
				if got.firstSector != at {
					t.Errorf("region %d (%s) starts at sector %d, want %d", i, want.name, got.firstSector, at)
				}
				if got.descriptor != uint32(i) {
					t.Errorf("region %d (%s) descriptor: got %d want %d", i, want.name, got.descriptor, i)
				}
				at += got.sectorCount
			}
			if at != framedSectors {
				t.Errorf("the regions cover %d sectors, want the disk's %d", at, framedSectors)
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
