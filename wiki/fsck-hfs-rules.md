# Verifier-clean checklist: rules `fsck_hfs` / `hdiutil verify` care about

A DMG that **mounts** is not the same as a DMG that **verifies**. The
former just needs the kernel HFS+ driver to like the volume header and
catalog tree; the latter has to pass `fsck_hfs -fn`, which is what
`codesign --verify` and Apple's notary service ultimately rely on for
read-only images.

This document is a checklist of every verifier rule we found while
iterating on the writer, with cross-references to the Apple source that
enforces each rule. The goal is not encyclopedic completeness — the
goal is that a future maintainer who *adds a feature* (say, hard links
or extents-overflow records) can scan this list and ask "did I break
any of these?" before re-running tests.

All citations are to
[`apple-oss-distributions/hfs`](https://github.com/apple-oss-distributions/hfs).

---

## 1. Volume Header bookkeeping must add up

| Rule | Where we set it | Verifier ref |
| ---- | --------------- | ------------ |
| `kHFSVolumeUnmountedMask` bit must be on for a clean volume | `internal/hfsplus/volumeheader.go` | `SVerify1.c: VolumeCheck()` |
| `freeBlocks + allocatedBlocks == totalBlocks` | `layout.go` plus `allocation.go` | `SVerify1.c: BcheckHFSPlusBitmap()` |
| `fileCount + folderCount` matches the catalog walk | `builder.go` / `volumeheader.go` | `SVerify1.c: CheckCatalogRecord()` |
| `nextCatalogID > max CNID actually used` | `builder.go` | `SVerify1.c: CheckCatalogBTree()` |
| Primary VH at offset 1024 and Alternate VH at `size − 1024` are byte-equal | `builder.go: writeVolumeHeader` | `SVerify1.c: ComparePrimaryAndBackupVH()` |
| `signature` is `'H+'` or `'HX'`, version matches signature | `types.go` | `SVerify1.c: CheckVolumeHeader()` |
| Reserved fields zero where TN1150 says so | `types.go` `binary.Write` of structs | various |

## 2. Catalog records: thread record per folder, valence, folderCount

`fsck_hfs` rebuilds an expected catalog from scratch by walking nodes
and comparing it to what's on disk. The most error-prone parts:

- **Every folder needs a thread record.** Thread records have key
  `(folderCNID, "")` and record type `kHFSPlusFolderThreadRecord = 3`.
  We emit one for the root folder and one for every sub-folder. *Files
  do not need thread records unless they're hard-linked* — and we
  never produce hard links.

  - Ref: `SVerify1.c: CheckCatalogBTree()` — "Invalid parent CName in
    thread record" is the error you get when the thread record's name
    or parent CNID disagrees with the actual folder record.

- **`valence`** on a folder record must equal the number of immediate
  children (files + sub-folders), counted from the catalog itself, not
  from the source tree (because xattr fork "records" don't count).
  `catalog.go` does this counting by parent-CNID grouping.

- **`folderCount`** must equal the number of immediate **sub-folders
  only**, and the `kHFSHasFolderCount` flag bit must be set on the
  folder record.

  We discovered this rule the hard way:
  ```
  fsck_hfs: HasFolderCount flag needs to be set
  ```
  Fixed in `catalog.go: encodeCatalogFolderRecord` — we always set
  `kHFSHasFolderCount` (so the field is read), and the walker in
  `dmg.go` populates `SubFolderCount` per folder.

  - Ref: `SVerify1.c: CheckFolderRecord()` (the flag and count are
    cross-checked).

- **`textEncoding`** on every catalog record should be set to the
  conventional MacRoman value (`0`). We emit zero. `fsck_hfs` doesn't
  hard-fail on a non-zero value but warns.

## 3. B-tree internal consistency

- **Offset table monotonicity.** See
  [btree-investigation.md](btree-investigation.md). `offset[0]` lives
  at the highest address; values strictly decrease as the index
  increases. `hfs_swap_BTNode` (`core/hfs_btreeio.c`) is what emits
  "offsets X and Y out of order".

- **`BTHeaderRec.totalNodes` == nodes actually present** (counting
  header, leaves, indices, map). For empty trees this is `1`.

- **`BTHeaderRec.firstLeafNode` and `lastLeafNode`** form the endpoints
  of the leaf-node sibling chain. For empty trees both are `0`.

- **Records 2-byte aligned.** Every record in a leaf or index node
  must start at an even offset within the node. Attribute records
  have a variable-length data tail; if the data length is odd we add
  a single pad byte. See `attributes.go: encodeInlineAttrData`. The
  symptom of getting this wrong is:
  ```
  fsck_hfs: offset #1 invalid (0x0101)
  ```
  ("0x0101" was the odd record offset of the second record in a leaf
  node — the misalignment cascades into every subsequent record.)

  - Ref: `SBTree.c: AllocateNode()` and `SVerify1.c:
    CheckAttributeRecord()`.

## 4. `ForkData.clumpSize` consistency and `maxClump` rule

This took the most rounds to chase down. There are *two* coupled
rules:

### 4a. Inner/outer clumpSize must match

The `clumpSize` field appears in two places for each B-tree fork:

- Outside, in the Volume Header's `HFSPlusForkData` for that fork.
- Inside, in the `BTHeaderRec.clumpSize` of the tree itself.

`fsck_hfs` checks they're equal:

```
invalid VHB attributesFile.clumpSize
```

This is emitted in `SVerify2.c: VerifyVolumeHeader()` after rounding
trips through `SVerify1.c: GetVolumeFeatures()` and the
`SetupFCB`/`fcbClumpSize` machinery in `SUtils.c`.

In our code, `BuildResult.PadToBlocks` (in `btree.go`) explicitly
rewrites the `BTHeaderRec.clumpSize` of the just-built tree to match
the allocated fork size (`totalBlocks × blockSize`), and then
`volumeheader.go` reads that same value into the Volume Header's
ForkData.

### 4b. `clumpSize ≤ totalBlocks / 4 × blockSize`

Stuck on the same error after fixing 4a, we found this further check
in `SVerify1.c`:

```c
maxClump = (volumeAllocBlocks / 4) * volumeAllocBlockSize;
if (fcbClumpSize > maxClump) { /* complain */ }
```

For small test volumes the desired B-tree fork size easily exceeds
1/4 of the total volume. `layout.go: BuildPlan` accordingly pads
`totalBlocks` so that:

```
totalBlocks ≥ 4 × max(catalogBlocks, extentsBlocks, attrsBlocks)
```

This ensures no fork's `clumpSize` (== fork size in bytes) exceeds the
verifier's max. `nextAllocation` is adjusted to point past user data,
or to `0` if it would otherwise sit at the alt-VH or beyond.

- Ref: `SVerify1.c: GetVolumeFeatures()` (line area where
  `maxClump` is computed) and `SUtils.c: SetupFCB()` (where
  `fcbClumpSize` defaults are reconciled with both the Volume Header
  and the BT header).

## 5. Allocation bitmap

`fsck_hfs` rebuilds an expected bitmap and `XOR`s it with the on-disk
bitmap. Anything other than all-zeros is an error.

We set bits for:

- VH/boot sectors (blocks 0 and the block containing the VH if
  different)
- Bitmap fork blocks (always block 1..N for tiny volumes)
- Each B-tree fork
- Each file fork (we lay them out contiguously after the trees)
- The alt-VH block (`totalBlocks − 1`)

Common bugs we hit and fixed:

- Forgetting the alt-VH block.
- Off-by-one when computing fork end blocks (HFS+ extents are
  `(startBlock, blockCount)` so `endBlock = startBlock + blockCount`
  is exclusive).
- Treating `nextAllocation` as authoritative; it's only a hint.

- Ref: `SVerify1.c: BcheckHFSPlusBitmap()`.

## 6. Dates and encodings

- All four VH dates (`createDate`, `modifyDate`, `backupDate`,
  `checkedDate`) should be set to a non-zero value. We use the same
  value for all four (either user-supplied `DMG.Time` or the source
  folder's mtime).
- `encodingsBitmap` has bit 0 (MacRoman) set. This is conventional
  and matches `hdiutil`'s output. Records carry per-record
  `textEncoding=0` (also MacRoman).

- Ref: `SVerify1.c: CheckVolumeHeader()` warns on zero dates.

## 7. Extents tree must exist, even if empty

The extents-overflow tree is required even on volumes where no fork
exceeds 8 extents (i.e., all our volumes). We emit a one-node tree
(header only) using `emitEmptyTree`. The Volume Header's `extentsFile`
ForkData points at it.

- Ref: `SVerify1.c: CheckExtentsBTree()`.

## 8. Catalog and extents key compare types

For HFSX volumes (which we always emit), the `keyCompareType` in
`BTHeaderRec` must be `kHFSBinaryCompare = 0xBC`. For classic HFS+
it's `kHFSCaseFolding = 0xCF` and would require the case-folding
table.

- Ref: `core/hfs_format.h: BTHeaderRec`,
  `core/HFSUnicodeWrappers.h: kHFSBinaryCompare`.

## 9. Filename encoding

Catalog keys (and attribute keys) use the `HFSUniStr255` form: a
big-endian `uint16` length followed by up to 255 `uint16` code units
in UTF-16BE. The on-disk form must be **NFD** (canonical
decomposition), otherwise `fsck_hfs` flags the record as having a
malformed name.

`internal/hfsplus/unicode.go` does the normalize-and-encode.

- Ref: `core/UnicodeWrappers.c`.

## 10. CNIDs

- `kHFSRootFolderID = 2` is the root folder's CNID. The thread record
  for the root has parent `kHFSRootParentID = 1`.
- `kHFSFirstUserCatalogNodeID = 16` is the first CNID we hand out to
  user files/folders. CNIDs 3..15 are reserved.
- `nextCatalogID` in the VH must be strictly greater than the largest
  CNID actually present in the catalog.

- Ref: `core/hfs_format.h`: the `enum` defining
  `kHFSRootParentID`/`kHFSRootFolderID`/`kHFSFirstUserCatalogNodeID`.

---

## When a verifier complains, in order:

1. Add `-d` to `fsck_hfs` (verbose). It will tell you which tree, which
   node, which record number it's choking on.
2. `hexdump -C` that node from a raw image. (`hdiutil attach -nomount`
   to get a `/dev/diskN`, then `dd if=/dev/diskN of=/tmp/raw.hfs`.)
3. Compare byte-for-byte against the same offset in a known-good
   image produced by `hdiutil create -srcfolder ... -format UDRW`.
4. If the structure looks right, suspect a cross-field consistency
   check (most often clumpSize, folderCount, or the bitmap).
