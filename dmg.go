/*
Package dmg writes Apple UDIF disk images (.dmg files) containing an
HFS+ volume. The package is implemented in pure Go and works on macOS,
Linux, and Windows.

* Quick start:

	d := &dmg.DMG{VolumeName: "MyApp"}
	if err := d.Create("./build/MyApp", "MyApp.dmg", dmg.ModeReadOnlyCompressed); err != nil {
		return err
	}

The package is a clean-room implementation from public format
specifications (Apple's TN1150 for HFS+, and the publicly documented
UDIF layout for the .dmg container). It is licensed under Apache-2.0
and may be used inside closed-source applications.

* Implementation notes

  - Symbolic links are stored as the classic 'slnk'/'rhap' file
    pair with the link target written into the data fork as a POSIX
    path. On Windows, [os.Readlink] returns paths with backslash
    separators; those bytes are written verbatim, so a Windows-built
    image with absolute symlinks may not resolve when mounted on macOS.

  - Hard links in the source tree are silently duplicated. The walker
    uses [os.Lstat] and treats each directory entry as an independent
    file, so two hard links pointing at the same inode become two
    catalog entries each with their own copy of the data. For
    read-only distribution DMGs this is benign; if your input contains
    intentional hard links you should consolidate them before calling
    [DMG.Create].
*/
package dmg

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/jetbrains/go-dmg-writer/internal/hfsplus"
	"github.com/jetbrains/go-dmg-writer/internal/udif"
)

// Mode selects how the produced DMG encodes its data fork.
type Mode int

const (
	// ModeReadOnly produces a UDRO image: uncompressed UDIF, immutable
	// at mount time. This is the right choice when you want the
	// fastest mount time and don't care about file size on disk.
	ModeReadOnly Mode = iota
	// ModeReadOnlyCompressed produces a UDZO image: per-chunk zlib
	// compression, immutable at mount time. This is the format you
	// want for shipping codesigned and notarized applications.
	ModeReadOnlyCompressed
)

// String implements [fmt.Stringer] so Modes render readably in error
// messages.
func (m Mode) String() string {
	switch m {
	case ModeReadOnly:
		return "ModeReadOnly"
	case ModeReadOnlyCompressed:
		return "ModeReadOnlyCompressed"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// OwnerIDUnset is the sentinel value for [DMG.OwnerID] / [DMG.GroupID]
// meaning "no specific owner; record HFS+'s unknown-user value (99)".
// Setting this is what `hdiutil` does and what makes the produced image
// mountable regardless of the host's UID layout (HFS+ treats owner==99
// as "ignore stored permissions, use the mounter's identity").
//
// The struct's zero value (0) is the legitimate root UID, NOT unset; if
// you want the unknown-user behavior you must opt in by setting OwnerID
// (and/or GroupID) to OwnerIDUnset explicitly.
const OwnerIDUnset uint32 = ^uint32(0)

// hfsPlusUnknownUser is the resolved "owner==99" value HFS+ uses to mean
// "this catalog record has no specific owner; use the mounter's
// identity for permission checks."
const hfsPlusUnknownUser uint32 = 99

// DMG holds the optional knobs for [DMG.Create]. The zero value works for
// most callers.
type DMG struct {
	// VolumeName is the user-facing label that appears in Finder when
	// the image is mounted. Defaults to "disk image".
	VolumeName string
	// BlockSize is the HFS+ allocation block size. Defaults to 4096.
	BlockSize uint32
	// Time is the timestamp baked into the volume header and every
	// catalog record. Pass time.Time{} to use time.Now(); pass a fixed
	// value for deterministic output (useful for reproducible builds).
	//
	// Times before 1904-01-01 (HFS+'s epoch) are silently clamped to
	// 1 second past the epoch because fsck_hfs rejects zero-valued
	// dates outright.
	Time time.Time
	// ChunkSectors is the UDIF chunk size in 512-byte sectors. Default
	// 2048 (1 MiB).
	ChunkSectors uint32
	// RootFinderInfo, if non-nil, is written as the com.apple.FinderInfo
	// xattr on the root folder. Must be exactly 32 bytes (the Finder
	// info block size). Used to set custom volume icon, Finder window
	// position, etc.
	RootFinderInfo []byte
	// OwnerID / GroupID are baked into every catalog record. The
	// zero value is the legitimate root UID/GID 0; set to
	// [OwnerIDUnset] for the HFS+ "unknown user" (99) sentinel that
	// `hdiutil` uses by default. See [OwnerIDUnset] for why you might
	// prefer the latter.
	OwnerID uint32
	GroupID uint32
}

// Create writes a DMG to outPath containing every file under srcFolder.
func (d *DMG) Create(srcFolder, outPath string, mode Mode) (err error) {
	switch mode {
	case ModeReadOnly, ModeReadOnlyCompressed:
		// ok
	default:
		return fmt.Errorf("dmg: invalid Mode %s", mode)
	}

	srcFolder = filepath.Clean(srcFolder)
	st, err := os.Stat(srcFolder)
	if err != nil {
		return fmt.Errorf("dmg: stat source: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("dmg: source %q is not a directory", srcFolder)
	}

	when := d.Time
	if when.IsZero() {
		when = time.Now().UTC()
	}
	macTime := hfsplus.MacTime(when)
	if macTime == 0 {
		macTime = 1 // fsck rejects zero
	}

	ownerID := d.OwnerID
	if ownerID == OwnerIDUnset {
		ownerID = hfsPlusUnknownUser
	}
	groupID := d.GroupID
	if groupID == OwnerIDUnset {
		groupID = hfsPlusUnknownUser
	}

	volName := d.VolumeName
	if volName == "" {
		volName = "disk image"
	}

	// Walk the source folder, assigning CNIDs and collecting xattrs.
	scan, err := walk(srcFolder, macTime, ownerID, groupID, volName)
	if err != nil {
		return err
	}

	// Optionally attach RootFinderInfo as an xattr on the root folder.
	if d.RootFinderInfo != nil {
		if got := len(d.RootFinderInfo); got != 32 {
			return fmt.Errorf("dmg: RootFinderInfo must be exactly 32 bytes (got %d)", got)
		}
		scan.attrs = append(scan.attrs, hfsplus.Attr{
			FileID: hfsplus.CNIDRootFolder,
			Name:   hfsplus.FinderInfoAttr,
			Data:   append([]byte(nil), d.RootFinderInfo...),
		})
	}

	// Build the HFS+ image into a scratch file.
	scratch, err := os.CreateTemp("", "go-dmg-*.hfs")
	if err != nil {
		return fmt.Errorf("dmg: scratch file: %w", err)
	}
	defer func() {
		_ = scratch.Close()
		_ = os.Remove(scratch.Name())
	}()

	inputs := &hfsplus.VolumeInputs{
		Entries:   scan.entries,
		UserFiles: scan.files,
		Attrs:     scan.attrs,
		BlockSize: d.BlockSize,
		MacTime:   macTime,
	}
	volumeSize, err := hfsplus.WriteVolume(scratch, inputs)
	if err != nil {
		return fmt.Errorf("dmg: build hfs+ image: %w", err)
	}

	// Rewind the scratch file and wrap it as UDIF.
	if _, err := scratch.Seek(0, io.SeekStart); err != nil {
		return err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("dmg: open output: %w", err)
	}
	// Capture the close error so a flush failure here doesn't silently
	// produce a corrupt DMG.
	defer func() {
		if cerr := out.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("dmg: close output: %w", cerr)
		}
	}()

	udifMode := udif.CompressionNone
	if mode == ModeReadOnlyCompressed {
		udifMode = udif.CompressionZlib
	}

	udifOpts := udif.Options{
		VolumeName:   volName,
		Compression:  udifMode,
		ChunkSectors: d.ChunkSectors,
		Time:         when,
	}
	if err := udif.Write(out, scratch, int64(volumeSize), udifOpts); err != nil {
		return fmt.Errorf("dmg: wrap udif: %w", err)
	}
	return nil
}

// scanResult is the in-memory model populated by [walk].
type scanResult struct {
	entries []*hfsplus.Entry
	files   []hfsplus.UserFileInput
	attrs   []hfsplus.Attr
}

func walk(root string, macTime, ownerID, groupID uint32, volumeName string) (*scanResult, error) {
	out := &scanResult{}

	// Map directory absolute path -> CNID, so children can look up parents.
	parentMap := map[string]uint32{}
	parentMap[root] = hfsplus.CNIDRootFolder

	// Root folder entry. The root's name IS the volume name: HFS+/HFSX
	// don't have a separate "volume label" field, the volume name field
	// in the volume header is literally the name on the root folder's
	// catalog record.
	rootEntry := newFolderEntry(hfsplus.CNIDRootFolder, hfsplus.CNIDRootParent, volumeName, macTime, ownerID, groupID, 0o755)
	out.entries = append(out.entries, rootEntry)

	nextCNID := hfsplus.CNIDFirstUser
	// Two passes: first collect every entry so CNIDs and valences are
	// known, then collect file openers/sizes/xattrs against finalized
	// CNIDs. Pass 1.
	type pendingPath struct {
		path   string
		cnid   uint32
		parent uint32
		info   os.FileInfo
	}
	var pending []pendingPath

	// Sort directory entries so output order is deterministic.
	walkErr := walkSorted(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}

		parentPath := filepath.Dir(path)
		parentCNID, ok := parentMap[parentPath]
		if !ok {
			return fmt.Errorf("dmg: walk: no parent CNID for %q", path)
		}

		// Allocate a CNID only inside the supported-kind branches; that
		// avoids the rollback pattern (reserve-then-maybe-release) that
		// would otherwise break the moment a future edit forgets to
		// decrement on an unsupported kind.
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			pending = append(pending, pendingPath{path, nextCNID, parentCNID, info})
			nextCNID++
		case info.IsDir():
			parentMap[path] = nextCNID
			pending = append(pending, pendingPath{path, nextCNID, parentCNID, info})
			nextCNID++
		case info.Mode().IsRegular():
			pending = append(pending, pendingPath{path, nextCNID, parentCNID, info})
			nextCNID++
		default:
			// Skip unsupported file types (sockets, devices, fifos).
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}

	// Pass 2: materialise Entry / UserFileInput / Attr lists.
	valence := map[uint32]uint32{}
	subFolderCount := map[uint32]uint32{}
	for _, p := range pending {
		base := filepath.Base(p.path)
		name, err := hfsplus.NewName(base)
		if err != nil {
			return nil, fmt.Errorf("dmg: name %q: %w", base, err)
		}
		valence[p.parent]++

		switch {
		case p.info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p.path)
			if err != nil {
				return nil, fmt.Errorf("dmg: readlink %q: %w", p.path, err)
			}
			tBytes := []byte(target)
			entry := &hfsplus.Entry{
				CNID:             p.cnid,
				ParentCNID:       p.parent,
				Name:             name,
				Kind:             hfsplus.KindSymlink,
				Mode:             modeBits(p.info) | symlinkTypeBits,
				OwnerID:          ownerID,
				GroupID:          groupID,
				CreateTime:       macTime,
				ContentModTime:   macTime,
				AttributeModTime: macTime,
				AccessTime:       macTime,
				BackupTime:       macTime,
				DataLogicalSize:  uint64(len(tBytes)),
				SymlinkTarget:    target,
			}
			out.entries = append(out.entries, entry)
			out.files = append(out.files, hfsplus.UserFileInput{
				Entry: entry,
				Size:  uint64(len(tBytes)),
				Opener: func(b []byte) hfsplus.FileOpener {
					return func() (io.ReadCloser, error) {
						return io.NopCloser(bytes.NewReader(b)), nil
					}
				}(tBytes),
			})

		case p.info.IsDir():
			entry := newFolderEntry(p.cnid, p.parent, base, macTime, ownerID, groupID, modeBits(p.info))
			entry.Name = name
			subFolderCount[p.parent]++
			out.entries = append(out.entries, entry)
			attrs, err := readXattrs(p.path, p.cnid)
			if err != nil {
				return nil, err
			}
			out.attrs = append(out.attrs, attrs...)

		case p.info.Mode().IsRegular():
			entry := &hfsplus.Entry{
				CNID:             p.cnid,
				ParentCNID:       p.parent,
				Name:             name,
				Kind:             hfsplus.KindFile,
				Mode:             modeBits(p.info) | regularTypeBits,
				OwnerID:          ownerID,
				GroupID:          groupID,
				CreateTime:       macTime,
				ContentModTime:   macTime,
				AttributeModTime: macTime,
				AccessTime:       macTime,
				BackupTime:       macTime,
				DataLogicalSize:  uint64(p.info.Size()),
			}
			out.entries = append(out.entries, entry)
			path := p.path
			out.files = append(out.files, hfsplus.UserFileInput{
				Entry: entry,
				Size:  uint64(p.info.Size()),
				Opener: func() (io.ReadCloser, error) {
					return os.Open(path)
				},
			})
			attrs, err := readXattrs(p.path, p.cnid)
			if err != nil {
				return nil, err
			}
			out.attrs = append(out.attrs, attrs...)
		}
	}

	// Backfill folder counters (valence + sub-folder count) for every
	// folder including the root.
	for _, e := range out.entries {
		if e.Kind == hfsplus.KindFolder {
			e.Valence = valence[e.CNID]
			e.SubFolderCount = subFolderCount[e.CNID]
		}
	}

	return out, nil
}

// walkSorted is a deterministic variant of [filepath.WalkDir]: every
// directory's entries are visited in lexical order. The visitor receives
// [os.Lstat] info (so symlinks are reported as symlinks, not their
// targets).
//
// Unlike [filepath.WalkDir], walkSorted does NOT recognise sentinel
// errors like [filepath.SkipDir]; any non-nil error returned from the
// visitor aborts the entire walk.
func walkSorted(root string, visit func(path string, info os.FileInfo, err error) error) error {
	info, err := os.Lstat(root)
	if err != nil {
		return visit(root, nil, err)
	}
	if err := visit(root, info, nil); err != nil {
		return err
	}
	if info.IsDir() {
		return walkDir(root, visit)
	}
	return nil
}

func walkDir(dir string, visit func(path string, info os.FileInfo, err error) error) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return visit(dir, nil, err)
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	for _, de := range ents {
		path := filepath.Join(dir, de.Name())
		info, err := os.Lstat(path)
		if err != nil {
			if err := visit(path, nil, err); err != nil {
				return err
			}
			continue
		}
		if err := visit(path, info, nil); err != nil {
			return err
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			if err := walkDir(path, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

// HFS+ file-mode "type" bits. These live in the high four bits of
// HFSPlusBSDInfo.fileMode and mirror POSIX S_IFREG/S_IFDIR/S_IFLNK etc.
const (
	regularTypeBits uint16 = 0o100000 // S_IFREG
	dirTypeBits     uint16 = 0o040000 // S_IFDIR
	symlinkTypeBits uint16 = 0o120000 // S_IFLNK
)

func modeBits(info os.FileInfo) uint16 {
	// Strip non-permission bits from the Go FileMode; the type bits are
	// added by the caller via `bitwise-or`.
	//
	// Note: on Windows os.FileMode.Perm()
	// returns synthetic 0o666/0o777 values rather than real POSIX
	// permission bits, so DMGs built on Windows carry meaningless mode
	// bits in their catalog records.
	return uint16(info.Mode().Perm())
}

func newFolderEntry(cnid, parent uint32, name string, macTime, ownerID, groupID uint32, mode uint16) *hfsplus.Entry {
	n, _ := hfsplus.NewName(name)
	return &hfsplus.Entry{
		CNID:             cnid,
		ParentCNID:       parent,
		Name:             n,
		Kind:             hfsplus.KindFolder,
		Mode:             mode | dirTypeBits,
		OwnerID:          ownerID,
		GroupID:          groupID,
		CreateTime:       macTime,
		ContentModTime:   macTime,
		AttributeModTime: macTime,
		AccessTime:       macTime,
		BackupTime:       macTime,
	}
}
