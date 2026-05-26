package hfsplus

import (
	"fmt"
	"io"
)

// FileOpener returns a fresh ReadCloser over a user file's data fork. The
// builder calls it once, reads exactly `size` bytes, and closes.
type FileOpener func() (io.ReadCloser, error)

// VolumeInputs is the full input to [WriteVolume]: an in-memory catalog
// model and the sources for each user file's data fork.
type VolumeInputs struct {
	// Entries is the catalog model, INCLUDING the root folder at
	// CNIDRootFolder. The slice must satisfy:
	//   - exactly one entry has CNID == CNIDRootFolder
	//   - every non-root entry's ParentCNID refers to a folder entry
	//     in the same slice
	//   - the slice does NOT need to be sorted; the builder sorts
	//     records internally before packing
	Entries []*Entry

	// UserFiles aligns with the file-kind entries in Entries. Each
	// FileOpener gives bytes for the corresponding Entry's data fork.
	// The order here drives the order files are laid out in the image
	// (and therefore the start-block assignment).
	UserFiles []UserFileInput

	// Attrs is the (possibly nil) list of extended attributes to embed
	// in the attributes B-tree.
	Attrs []Attr

	// BlockSize is the HFS+ allocation block size in bytes. Defaults to
	// [DefaultBlockSize] if zero.
	BlockSize uint32

	// MacTime is the timestamp (in HFS+'s seconds-past-1904 form) baked
	// into every catalog record. Must be non-zero or fsck will complain.
	MacTime uint32
}

// UserFileInput pairs an Entry with the means to read its data-fork bytes.
type UserFileInput struct {
	Entry  *Entry
	Size   uint64
	Opener FileOpener
}

// WriteVolume builds an HFS+ volume image into out. out must support
// seeking and writing; we wrote the volume bytes out of order (header at
// the start, body in the middle, alternate header at the end) so a pure
// io.Writer would not suffice.
//
// On success the returned VolumeSize is the exact byte length written
// (always a multiple of out's block size).
func WriteVolume(out io.WriteSeeker, in *VolumeInputs) (volumeSize uint64, err error) {
	if in.BlockSize == 0 {
		in.BlockSize = DefaultBlockSize
	}
	if in.MacTime == 0 {
		return 0, fmt.Errorf("hfsplus: MacTime must be non-zero (fsck_hfs rejects zero dates)")
	}

	// Tag entries that have any xattr so the catalog packer sets the
	// HasAttributes flag bit correctly.
	if len(in.Attrs) > 0 {
		fileIDsWithAttrs := make(map[uint32]struct{}, len(in.Attrs))
		for _, a := range in.Attrs {
			fileIDsWithAttrs[a.FileID] = struct{}{}
		}
		for _, e := range in.Entries {
			if _, ok := fileIDsWithAttrs[e.CNID]; ok {
				e.HasAttributes = true
			}
		}
	}

	// 1. Pack the trees once with placeholder file extents — that is the
	// first time we learn how big each tree's bytes are. The record
	// sizes don't depend on the extent values, so the result of the
	// second pack below is guaranteed identical in size.
	catRes, err := BuildCatalogTree(in.Entries, CatalogNodeSize)
	if err != nil {
		return 0, fmt.Errorf("hfsplus: catalog: %w", err)
	}
	extRes, err := BuildExtentsTree()
	if err != nil {
		return 0, fmt.Errorf("hfsplus: extents: %w", err)
	}
	attrRes, err := BuildAttributesTree(in.Attrs)
	if err != nil {
		return 0, fmt.Errorf("hfsplus: attributes: %w", err)
	}

	// 2. Convert UserFileInput → UserFile slice (planner cares only about
	// the entry and the size).
	plannerFiles := make([]UserFile, len(in.UserFiles))
	for i, u := range in.UserFiles {
		plannerFiles[i] = UserFile{Entry: u.Entry, Size: u.Size}
	}

	plan, err := BuildPlan(in.BlockSize, in.Entries, plannerFiles,
		uint64(len(catRes.Bytes)),
		uint64(len(extRes.Bytes)),
		uint64(len(attrRes.Bytes)))
	if err != nil {
		return 0, err
	}

	// 3. Re-pack the catalog with the now-resolved extent values. (Sizes
	// are identical to step 1 because record sizes don't depend on
	// extent contents.)
	catRes, err = BuildCatalogTree(in.Entries, CatalogNodeSize)
	if err != nil {
		return 0, fmt.Errorf("hfsplus: catalog (second pass): %w", err)
	}

	// 4a. Pad each tree's image to exactly the number of blocks the
	// planner allocated for it. fsck_hfs verifies that a B-tree's
	// header.TotalNodes equals (fork blocks * blockSize / nodeSize),
	// so the tree must exactly fill its fork.
	if err := catRes.PadToBlocks(plan.CatalogFile.TotalBlocks, plan.BlockSize); err != nil {
		return 0, fmt.Errorf("hfsplus: pad catalog: %w", err)
	}
	if err := extRes.PadToBlocks(plan.ExtentsFile.TotalBlocks, plan.BlockSize); err != nil {
		return 0, fmt.Errorf("hfsplus: pad extents: %w", err)
	}
	if err := attrRes.PadToBlocks(plan.AttributesFile.TotalBlocks, plan.BlockSize); err != nil {
		return 0, fmt.Errorf("hfsplus: pad attributes: %w", err)
	}

	// 5. Build the volume header.
	vh := BuildVolumeHeader(plan, in.MacTime)

	volumeSize = uint64(plan.TotalBlocks) * uint64(plan.BlockSize)

	// 6. Pre-extend the output to volumeSize so seeks past the end land
	// safely. Writing one zero byte at the very last offset is the
	// portable way to truncate-extend a [io.WriteSeeker].
	if _, err := out.Seek(int64(volumeSize)-1, io.SeekStart); err != nil {
		return 0, err
	}
	if _, err := out.Write([]byte{0}); err != nil {
		return 0, err
	}

	// 7. Write the primary volume header at byte 1024.
	if err := writeAt(out, 1024, vh.Encode()); err != nil {
		return 0, err
	}

	// 8. Write the allocation bitmap.
	if err := writeAt(out, int64(plan.AllocStartBlock)*int64(plan.BlockSize), plan.AllocBitmap.Bytes()); err != nil {
		return 0, err
	}

	// 9. Write the three system B-trees.
	if err := writeAt(out, int64(plan.CatalogStartBlock)*int64(plan.BlockSize), catRes.Bytes); err != nil {
		return 0, err
	}
	if err := writeAt(out, int64(plan.ExtentsStartBlock)*int64(plan.BlockSize), extRes.Bytes); err != nil {
		return 0, err
	}
	if err := writeAt(out, int64(plan.AttributesStartBlock)*int64(plan.BlockSize), attrRes.Bytes); err != nil {
		return 0, err
	}

	// 10. Stream every user file into its allotted blocks.
	for i, u := range in.UserFiles {
		p := plan.Placements[i]
		if p.Blocks == 0 {
			continue // zero-byte file
		}
		off := int64(p.StartBlock) * int64(plan.BlockSize)
		if err := streamFileAt(out, off, u.Opener, u.Size); err != nil {
			return 0, fmt.Errorf("hfsplus: writing file cnid=%d: %w", u.Entry.CNID, err)
		}
	}

	// 11. Write the alternate volume header at volumeSize-1024.
	if err := writeAt(out, int64(volumeSize)-1024, vh.Encode()); err != nil {
		return 0, err
	}

	return volumeSize, nil
}

func writeAt(out io.WriteSeeker, off int64, b []byte) error {
	if _, err := out.Seek(off, io.SeekStart); err != nil {
		return err
	}
	for len(b) > 0 {
		n, err := out.Write(b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

func streamFileAt(out io.WriteSeeker, off int64, opener FileOpener, size uint64) error {
	rc, err := opener()
	if err != nil {
		return err
	}
	defer rc.Close()
	if _, err := out.Seek(off, io.SeekStart); err != nil {
		return err
	}
	written, err := io.CopyN(out, rc, int64(size))
	if err != nil {
		return err
	}
	if uint64(written) != size {
		return fmt.Errorf("short file: wrote %d of %d bytes", written, size)
	}
	return nil
}
