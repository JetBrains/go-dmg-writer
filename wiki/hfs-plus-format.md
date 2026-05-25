# HFS+ on-disk format (as we emit it)

This document is a tour of the HFS+ structures the writer produces. It
is not a complete HFS+ reference — that's what
[TN1150](https://developer.apple.com/library/archive/technotes/tn/tn1150.html)
is for. The goal here is to explain *why each field has the value it
does*, with citations into Apple's open `hfs` repository
([`apple-oss-distributions/hfs`](https://github.com/apple-oss-distributions/hfs))
when the rule is not obvious from TN1150.

All multi-byte integers in HFS+ are **big-endian**. The `binary.BigEndian`
encodings in `internal/hfsplus/types.go` reflect that.

## Volume signature: HFSX, not HFS+

The classic HFS+ volume signature is `'H+'` (`0x482B`), version `4`. We
emit **HFSX** instead: signature `'HX'` (`0x4858`), version `5`. The
relevant constants live in `internal/hfsplus/types.go`.

Why? HFSX volumes use **binary comparison** of catalog keys instead of
the case-insensitive Unicode collation HFS+ requires. The collation
table for HFS+ is large, non-trivial, and would have to be perfectly
byte-equivalent to Apple's table to produce verifier-clean trees. HFSX
sidesteps that entirely: catalog records are sorted by `(parentID, name)`
where `name` is a UTF-16BE NFD byte string compared bytewise. The HFS+
Volume Header documents this case-sensitivity bit in
`HFSPlusVolumeFinderInfo[5]`, but switching to the HFSX signature is the
cleaner and more conventional way to express it.

Reference:
[`apple-oss-distributions/hfs:core/hfs_format.h`](https://github.com/apple-oss-distributions/hfs/blob/main/core/hfs_format.h)
(`kHFSPlusSigWord`, `kHFSXSigWord`, `kHFSXVersion`).

## Volume layout

Sector 0–1 (the first 1024 bytes) is a reserved boot area, traditionally
zeroed for HFS+ images that don't boot. Our layout is:

```
offset                     contents
─────────────────────────  ───────────────────────────────────────────────
0  …  1023                 reserved boot blocks (zero-filled)
1024 …  1535               Volume Header (HFSPlusVolumeHeader, 512 bytes)
nextBlock × blockSize …    allocation bitmap fork
                           extents-overflow B-tree fork (empty / minimal)
                           catalog B-tree fork
                           attributes B-tree fork
                           user data (file forks, concatenated)
size-1024 …  size-513      Alternate Volume Header
size-512  …  size-1        reserved (zeroed)
```

The Volume Header lives at byte 1024 (sector 2). The Alternate Volume
Header lives in the second-to-last sector (`size - 1024`). The very last
sector is reserved for older boot mechanisms and stays zero. `fsck_hfs`
treats the alt-VH as authoritative when the primary is damaged, so we
write both with identical contents.

## Block size

We use **4096-byte allocation blocks** unconditionally. HFS+ supports
512-byte through 65536-byte blocks, but 4096 matches modern macOS
defaults, keeps the bitmap small for small volumes, and matches what
`hdiutil` emits for read-only/UDZO images.

## Volume Header fields, explained

`internal/hfsplus/volumeheader.go` constructs the `HFSPlusVolumeHeader`.
The interesting fields:

| Field                    | Value we set                                                          | Notes |
| ------------------------ | --------------------------------------------------------------------- | ----- |
| `signature` / `version`  | `0x4858` / `5`                                                        | HFSX (see above). |
| `attributes`             | `kHFSVolumeUnmountedMask`                                             | Bit 8 — "cleanly unmounted". `fsck_hfs` requires this for clean volumes. |
| `lastMountedVersion`     | `'10.0'` (`0x31302E30`)                                               | Conventional value for hdiutil-style images. |
| `journalInfoBlock`       | `0`                                                                   | We do not journal. The `kHFSVolumeJournaledMask` bit is **off**. |
| `createDate`/`modifyDate`/`backupDate`/`checkedDate` | Mac-epoch seconds (see `hfstime.go`) | All four set to the same instant for determinism. |
| `fileCount`/`folderCount`| Computed counts                                                       | Excludes the root folder itself (see TN1150 §"Number of Files / Folders" — root counts as a directory but `folderCount` is "number of folders **other than** the root"). |
| `blockSize`              | `4096`                                                                | See above. |
| `totalBlocks`            | Computed; padded — see `layout.go`                                    | Includes alt-VH block. We deliberately pad to satisfy the `maxClump = totalBlocks/4 × blockSize` rule (see [fsck-hfs-rules.md](fsck-hfs-rules.md)). |
| `freeBlocks`             | `totalBlocks − allocated`                                             | Must match what the bitmap shows; verified by `fsck_hfs`. |
| `nextAllocation`         | First free block after user data, or `0`                              | Hint, not authoritative. |
| `rsrcClumpSize`/`dataClumpSize` | `4 × blockSize` (16 KiB)                                       | Conventional. Not enforced by `fsck_hfs` for read-only volumes. |
| `nextCatalogID`          | Max CNID used + 1                                                     | First user CNID we hand out is `kHFSFirstUserCatalogNodeID = 16` (TN1150). |
| `writeCount`             | `0`                                                                   | Increments on every mount-for-write; we ship a fresh image. |
| `encodingsBitmap`        | bit 0 set (MacRoman) by convention                                    | The actual `textEncoding` per record matters more (see "Text encoding" below). |
| `finderInfo[0..7]`       | All zero unless `RootFinderInfo` is supplied                          | We expose `DMG.RootFinderInfo` for setting a blessed folder / window position. |
| `allocationFile`         | ForkData for the bitmap                                               | One extent covers the whole bitmap; we never fragment it. |
| `extentsFile`            | ForkData for an empty extents-overflow tree                           | A single node holding only the header; see [btree-investigation.md](btree-investigation.md). |
| `catalogFile`            | ForkData                                                              | Holds the catalog tree. |
| `attributesFile`         | ForkData                                                              | Holds the attributes tree (empty unless xattrs are present). |
| `startupFile`            | All zero                                                              | We don't support booting from these images. |

## ForkData and clumpSize consistency

Each `HFSPlusForkData` carries:

- `logicalSize` — bytes used by the fork
- `totalBlocks` — allocation blocks covered by the fork's extents
- `clumpSize` — the "growth unit" for the fork, in bytes
- `extents[0..7]` — up to eight `(startBlock, blockCount)` records (we
  use exactly one extent per fork for B-trees, since we know the size up
  front)

`fsck_hfs` enforces that the `clumpSize` in `HFSPlusForkData` **matches**
the `clumpSize` stored inside the corresponding B-tree's `BTHeaderRec`.
See [fsck-hfs-rules.md](fsck-hfs-rules.md) for the full derivation and
the `apple-oss-distributions/hfs` references.

It also enforces a "max clump" rule: `clumpSize ≤ totalBlocks/4 ×
blockSize`. For small volumes this can be the binding constraint, and
`layout.go` pads `totalBlocks` accordingly.

## Text encoding (`textEncoding`)

HFS+ records carry a `textEncoding` hint per file/folder that has nothing
to do with the actual catalog name (which is always UTF-16BE NFD). It's
a leftover from the HFS-to-HFS+ migration era. `fsck_hfs` warns if the
value is missing for ASCII-only names, and `hdiutil` always sets it.

We set `textEncoding = 0` (MacRoman) on every catalog record. Doing so
is the conventional value `hdiutil` produces for newly created HFS+
volumes and matches what the verifier expects to see.

## Catalog records

Three record kinds appear in the catalog tree (see `internal/hfsplus/catalog.go`):

- **Folder thread record** (`kHFSPlusFolderThreadRecord = 3`): one per
  folder, keyed `(folderCNID, "")`. Stores the parent CNID and the
  folder's name.
- **Folder record** (`kHFSPlusFolderRecord = 1`): keyed `(parentCNID,
  name)`, contains valence (number of immediate children), folder count,
  CNID, permissions, timestamps, Finder info.
- **File record** (`kHFSPlusFileRecord = 2`): same key shape, plus
  `dataFork` ForkData and `resourceFork` ForkData.

We emit one **file thread record** per file iff the file is hard-linked
(we never produce hard links, so we never emit file threads). Folder
threads we always emit.

Folder records have to set `kHFSHasFolderCount` in `flags` and populate
`folderCount` correctly (number of *immediate* sub-folders). This is one
of the most subtle `fsck_hfs` checks — see
[fsck-hfs-rules.md](fsck-hfs-rules.md).

## Unicode normalization (NFD)

HFS+ stores filenames in UTF-16BE in **Unicode Normalization Form D**
(canonical decomposition). On macOS this is visible: `ls` on an HFS+
volume shows `e\u0301` for `é` even if the file was created with the
single-codepoint form. `internal/hfsplus/unicode.go` does the conversion
with `golang.org/x/text/unicode/norm.NFD`.

The integration test `dmg_darwin_test.go: TestMountAndCompare` mounts
the produced DMG and compares filenames against the source. macOS APFS
typically returns NFC, so the comparison uses
`norm.NFC.String(...)` on both sides — otherwise `café.txt` would appear
"different" purely due to normalization form.

References:
- [Unicode UAX #15](http://www.unicode.org/reports/tr15/) — Normalization
  Forms.
- [`apple-oss-distributions/hfs:core/UnicodeWrappers.c`](https://github.com/apple-oss-distributions/hfs/blob/main/core/UnicodeWrappers.c)
  — Apple's own NFD wrapper used by the kernel HFS+ driver.

## Dates

HFS+ stores dates as **unsigned 32-bit seconds since 1904-01-01 00:00:00
local time** (yes, *local* — this is a 1980s decision that we just
inherit). `internal/hfsplus/hfstime.go` does:

```go
const hfsEpochOffset = 2082844800 // seconds from 1970-01-01 UTC to 1904-01-01 UTC
hfsTime := uint32(t.Unix() + hfsEpochOffset)
```

Because the HFS+ convention is "local time stored as seconds since a
fixed point", strictly speaking we should add the local time zone
offset. In practice every reader (Finder, `mdutil`, `hdiutil`) treats
the value as UTC, and that's what we do. This matches what
`hdiutil create` itself produces. The off-by-`tz` is well-known
([writeup](https://en.wikipedia.org/wiki/HFS_Plus#Timestamps)) and
universally ignored.

## Extended attributes

The attributes B-tree (`internal/hfsplus/attributes.go`) holds:

- A `RootFinderInfo` attribute on the root folder iff the caller sets
  `DMG.RootFinderInfo`. This is how the volume can boot, have a custom
  icon, or open a custom Finder window position.
- Pass-through of any xattrs found on source files (read via
  `xattrs_darwin.go` / `xattrs_linux.go`). The most common one is
  `com.apple.quarantine`; signed apps may also carry
  `com.apple.metadata:_kMDItemUserTags` and similar.

We only emit **inline** attributes (`kHFSPlusAttrInlineData = 0x10`).
Fork-based and extents-based attributes (for xattrs >3802 bytes) are
not implemented — codesign data lives inside Mach-O segments, not in
xattrs, so this is rarely a real limit for app distribution.

Attribute records must be **2-byte aligned** in the leaf nodes. Inline
attrs with an odd data length get a single trailing pad byte. See
[fsck-hfs-rules.md](fsck-hfs-rules.md) for the rule and how we satisfy
it.

## Allocation bitmap

The bitmap is a packed bit array, one bit per allocation block, with bit
0 of byte 0 corresponding to block 0. `1` = allocated, `0` = free. We
mark as allocated:

- Reserved sectors (blocks containing the boot blocks and VH)
- Bitmap fork blocks
- Each B-tree fork's blocks
- Each file's data fork blocks
- The alt-VH block

Everything else is `0`. `fsck_hfs` walks the catalog + extents-overflow,
reconstructs an expected bitmap, and compares — any mismatch is a
verifier error.

## B-trees

See [btree-investigation.md](btree-investigation.md) for the full deep
dive. Short version:

- All trees use **fixed-size nodes**: catalog 8192, extents 4096,
  attributes 4096.
- Layout per node: `BTNodeDescriptor` (14 bytes) → records → free space
  → offset table (2 bytes per record + one for free-space, stored from
  the **high address** down).
- We emit **write-once** trees: build a sorted slice of records, pack
  them into leaf nodes until each is ~75% full, build index nodes one
  level up, repeat until a single root node remains. Empty trees get a
  special single-node ("header only") layout that the verifier accepts.

## What we don't implement

- **Journaling**. Journaled HFS+ has a separate journal info block at
  the start of the volume and a "journaled" attribute bit. Distribution
  images are unmounted-clean and don't need a journal.
- **Hard links**. They would require special files in a private
  `\0\0\0\0HFS+ Private Data\r` folder and CNID indirection. macOS apps
  rarely contain hard links, and DMGs are write-once for distribution.
- **Resource forks** (file-attached). Code signing on modern macOS uses
  Mach-O `LC_CODE_SIGNATURE` load commands, not classic Mac resource
  forks.
- **Extents overflow**. We always emit exactly one extent per fork. If
  a single file or B-tree were to exceed 8 extents (which never happens
  in practice for our two-pass planner, because we know fork sizes up
  front and allocate them contiguously), we'd need overflow records.
  The tree is still emitted (`fsck_hfs` requires a valid B-tree exists)
  but is always effectively empty.
