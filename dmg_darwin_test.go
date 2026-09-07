//go:build darwin

package dmg

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"
)

// TestHdiutilVerify exercises the produced DMGs against Apple's own
// `hdiutil verify` tool. A failure here is a release blocker - it means
// the DMG would not survive notarization either. We run all three modes.
//
// The test is darwin-gated; on Linux/Windows the build tag at the top of
// this file excludes it from the suite.
func TestHdiutilVerify(t *testing.T) {
	hdiutil, err := exec.LookPath("hdiutil")
	if err != nil {
		t.Skipf("hdiutil not in PATH: %v", err)
	}

	src := makeSourceTreeWithSymlink(t)

	modes := []struct {
		name         string
		mode         Mode
		partitionMap bool
	}{
		{"UDRO", ModeReadOnly, false},
		{"UDZO", ModeReadOnlyCompressed, false},
		{"UDRO-GPT", ModeReadOnly, true},
		{"UDZO-GPT", ModeReadOnlyCompressed, true},
	}
	for _, m := range modes {
		m := m
		t.Run(m.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), m.name+".dmg")
			d := &DMG{
				VolumeName:   "verify-" + m.name,
				Time:         time.Unix(1700000000, 0).UTC(),
				PartitionMap: m.partitionMap,
			}
			if err := d.Create(src, out, m.mode); err != nil {
				t.Fatalf("Create: %v", err)
			}

			cmd := exec.Command(hdiutil, "verify", out)
			combined, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("hdiutil verify failed:\nexit: %v\noutput:\n%s", err, combined)
			}
		})
	}
}

// TestFsckHFS attaches the DMG without mounting, runs fsck_hfs against
// the resulting (cooked) /dev/diskN node, then detaches. It catches
// structural bugs that hdiutil verify accepts (because verify only
// checks the UDIF checksum, not the inner HFS+ tree).
//
// We use /dev/diskN (not /dev/rdiskN) so fsck_hfs doesn't require root.
func TestFsckHFS(t *testing.T) {
	hdiutil, err := exec.LookPath("hdiutil")
	if err != nil {
		t.Skipf("hdiutil not in PATH: %v", err)
	}
	fsck, err := exec.LookPath("fsck_hfs")
	if err != nil {
		t.Skipf("fsck_hfs not in PATH: %v", err)
	}

	src := makeSourceTreeWithSymlink(t)
	out := filepath.Join(t.TempDir(), "fsck.dmg")
	d := &DMG{VolumeName: "fsck-vol", Time: time.Unix(1700000000, 0).UTC()}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create: %v", err)
	}

	attach := exec.Command(hdiutil, "attach", "-nobrowse", "-nomount", "-readonly", out)
	combined, err := attach.CombinedOutput()
	// This image is *partitionless*: the raw HFS+ volume fills the whole
	// data fork, so hdiutil reports one node with no type and no name:
	//   /dev/diskN <tab> <empty type><tab><empty name>
	// That one node is both the whole disk and the volume, so fsck and
	// detach both get it. A framed image reads back differently; see
	// TestPartitionMapAttachAndFsck.
	var dev string
	for _, line := range strings.Split(string(combined), "\n") {
		f := strings.Fields(line)
		if len(f) >= 1 && strings.HasPrefix(f[0], "/dev/disk") {
			dev = f[0]
			break
		}
	}
	defer func() {
		if dev != "" {
			_ = exec.Command(hdiutil, "detach", dev, "-force").Run()
		}
	}()
	if err != nil {
		t.Fatalf("hdiutil attach -nomount failed: %v\n%s", err, combined)
	}
	if dev == "" {
		t.Fatalf("no /dev/disk entry in hdiutil output:\n%s", combined)
	}

	cmd := exec.Command(fsck, "-fnd", dev)
	out2, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fsck_hfs failed: %v\noutput:\n%s", err, out2)
	}
}

// TestMountAndCompare attaches the produced DMG, walks its mounted
// filesystem, and compares the names/sizes to the source folder. This
// catches catalog/extent bugs that hdiutil verify would miss because it
// only checks structural invariants, not actual file content.
//
// It runs both framings, because a GPT-framed image has to mount just
// like a map-less one does.
func TestMountAndCompare(t *testing.T) {
	for _, partitionMap := range []bool{false, true} {
		t.Run(fmt.Sprintf("PartitionMap=%v", partitionMap), func(t *testing.T) {
			mountAndCompare(t, partitionMap)
		})
	}
}

func mountAndCompare(t *testing.T, partitionMap bool) {
	t.Helper()
	hdiutil, err := exec.LookPath("hdiutil")
	if err != nil {
		t.Skipf("hdiutil not in PATH: %v", err)
	}

	src := makeSourceTreeWithSymlink(t)
	out := filepath.Join(t.TempDir(), "mount.dmg")
	d := &DMG{VolumeName: "mounted", Time: time.Unix(1700000000, 0).UTC(), PartitionMap: partitionMap}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create: %v", err)
	}

	mountPoint := filepath.Join(t.TempDir(), "mnt")
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		t.Fatal(err)
	}
	attach := exec.Command(hdiutil, "attach", "-nobrowse", "-mountpoint", mountPoint, out)
	combined, err := attach.CombinedOutput()
	defer func() {
		detach := exec.Command(hdiutil, "detach", mountPoint, "-force")
		_ = detach.Run()
	}()
	if err != nil {
		t.Fatalf("hdiutil attach failed: %v\noutput:\n%s", err, combined)
	}

	// Compare the two trees by walking source and looking up each
	// path under mountPoint.
	srcWalk, err := walkTree(src)
	if err != nil {
		t.Fatal(err)
	}
	mntWalk, err := walkTree(mountPoint)
	if err != nil {
		t.Fatal(err)
	}
	if srcWalk != mntWalk {
		t.Errorf("mounted tree differs from source\n--- source ---\n%s\n--- mounted ---\n%s",
			srcWalk, mntWalk)
	}
}

// TestPartitionMapAttachAndFsck attaches a GPT-framed image without
// mounting it. hdiutil has to read the partition map back and expose an
// Apple_HFS slice, and fsck_hfs has to accept the volume inside it,
// which is what a reader that parses the image instead of mounting it
// does too.
func TestPartitionMapAttachAndFsck(t *testing.T) {
	hdiutil, err := exec.LookPath("hdiutil")
	if err != nil {
		t.Skipf("hdiutil not in PATH: %v", err)
	}
	fsck, err := exec.LookPath("fsck_hfs")
	if err != nil {
		t.Skipf("fsck_hfs not in PATH: %v", err)
	}

	src := makeSourceTreeWithSymlink(t)
	out := filepath.Join(t.TempDir(), "gpt.dmg")
	d := &DMG{VolumeName: "gpt-vol", Time: time.Unix(1700000000, 0).UTC(), PartitionMap: true}
	if err := d.Create(src, out, ModeReadOnly); err != nil {
		t.Fatalf("Create: %v", err)
	}

	attach := exec.Command(hdiutil, "attach", "-nobrowse", "-nomount", "-readonly", out)
	combined, err := attach.CombinedOutput()
	// A framed image attaches the way a real disk does: the whole-disk
	// node carries the GPT scheme, and the payload is a child slice
	// typed Apple_HFS.
	var hfsDev, rootDev string
	for _, line := range strings.Split(string(combined), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || !strings.HasPrefix(f[0], "/dev/disk") {
			continue
		}
		if rootDev == "" {
			rootDev = f[0]
		}
		if len(f) >= 2 && strings.HasPrefix(f[1], "Apple_HFS") {
			hfsDev = f[0]
		}
	}
	defer func() {
		if rootDev != "" {
			_ = exec.Command(hdiutil, "detach", rootDev, "-force").Run()
		}
	}()
	if err != nil {
		t.Fatalf("hdiutil attach -nomount failed: %v\n%s", err, combined)
	}
	if !strings.Contains(string(combined), "GUID_partition_scheme") {
		t.Errorf("hdiutil did not read a GUID partition scheme back:\n%s", combined)
	}
	if hfsDev == "" {
		t.Fatalf("no Apple_HFS slice in hdiutil output:\n%s", combined)
	}

	if out2, err := exec.Command(fsck, "-fnd", hfsDev).CombinedOutput(); err != nil {
		t.Fatalf("fsck_hfs on %s failed: %v\noutput:\n%s", hfsDev, err, out2)
	}
}

// makeSourceTreeWithSymlink builds a source folder that exercises files,
// nested directories, a symlink, and a non-ASCII filename.
func makeSourceTreeWithSymlink(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "alpha.txt"), "alpha\n")
	if err := os.Mkdir(filepath.Join(root, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "subdir", "beta.bin"), "binary blob")
	if err := os.Symlink("alpha.txt", filepath.Join(root, "alpha.link")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "café.txt"), "non-ascii name\n")
	// Several chunks of zeros, so the writer emits BlockZero runs and
	// `hdiutil verify` gets to check that the table checksums leave
	// those sectors out. A checksum that counts them makes verify
	// report INVALID, and nothing else in the suite sees a difference.
	mustWrite(t, filepath.Join(root, "hole.bin"), string(make([]byte, 6*1024*1024)))
	return root
}

func walkTree(root string) (string, error) {
	var b bytes.Buffer
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, root)
		if rel == "" {
			return nil
		}
		// macOS auto-mounts add some hidden files (`.fseventsd`, `.Trashes`,
		// `.Spotlight-V100`) that aren't in the source. Skip them.
		if strings.HasPrefix(filepath.Base(rel), ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// For directories, we deliberately ignore the "size" field
		// because macOS reports the on-disk catalog metadata size,
		// which has nothing to do with the source folder's size on
		// the host filesystem.
		typ := "f"
		size := info.Size()
		if info.IsDir() {
			typ = "d"
			size = 0
		} else if info.Mode()&os.ModeSymlink != 0 {
			typ = "l"
		}
		// HFS+ stores filenames in NFD; APFS (the typical source
		// filesystem on modern Macs) leaves them as the bytes given.
		// Normalize both sides to NFC for comparison.
		fmt.Fprintf(&b, "%s %s %d\n", typ, norm.NFC.String(rel), size)
		return nil
	})
	if err != nil {
		return "", err
	}
	return b.String(), nil
}
