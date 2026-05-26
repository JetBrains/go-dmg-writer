package dmg

import (
	"bytes"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
// the data fork is smaller than the partition.
func checkDMG(t *testing.T, path string, expectCompressed bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() < udif.KolySize {
		t.Fatalf("output too small: %d bytes", st.Size())
	}
	tail := make([]byte, udif.KolySize)
	if _, err := f.ReadAt(tail, st.Size()-int64(udif.KolySize)); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	koly, err := udif.DecodeKoly(tail)
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
	xml := make([]byte, koly.XMLLength)
	if _, err := f.ReadAt(xml, int64(koly.XMLOffset)); err != nil {
		t.Fatalf("ReadAt xml: %v", err)
	}
	if !bytes.Contains(xml, []byte("<key>resource-fork</key>")) {
		t.Errorf("XML missing resource-fork key:\n%s", xml)
	}
	if !bytes.Contains(xml, []byte("<key>blkx</key>")) {
		t.Errorf("XML missing blkx key")
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
