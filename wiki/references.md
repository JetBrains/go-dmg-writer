# References

Curated pointers into the public material this writer was built from.
None of these were copied or translated; they are *cross-checks* for
findings we reached empirically by running `hdiutil verify` and
`fsck_hfs` against our output and reading hex dumps.

## Apple primary sources

### TN1150 — *HFS Plus Volume Format*

The single most useful document. Apple Technical Note from 2004,
maintained in their archive.

- HTML: <https://developer.apple.com/library/archive/technotes/tn/tn1150.html>
- PDF mirror (often the cleanest read):
  <https://developer.apple.com/library/archive/technotes/tn/tn1150.pdf>

Sections we leaned on most:

- **§ "Volume Header"** — every field of `HFSPlusVolumeHeader`.
- **§ "B-Trees"** — node anatomy, key/record layout, and the offset
  table (note its wording about *"last two bytes hold the offset of
  the first record"* — see
  [btree-investigation.md](btree-investigation.md)).
- **§ "Catalog File"** — folder/file record formats, thread records,
  Finder info structs.
- **§ "Extents Overflow File"** — extent record format.
- **§ "Attributes File"** — inline / fork / extents attribute kinds.
- **§ "Hot Files"** — not implemented; mentioned for completeness.
- **§ "Allocation File"** — bitmap semantics.
- **§ "Unicode Subtleties"** — NFD requirement.
- **§ "Date and Time"** — Mac-epoch (1904-01-01) seconds.
- **§ "Case-Insensitive Comparison"** — for plain HFS+; we sidestep
  by using HFSX (binary compare).

### Apple `hfs` repository (open source)

<https://github.com/apple-oss-distributions/hfs>

The published source of the kernel HFS+ driver, `fsck_hfs`, and
related tools. We used these *only* to corroborate or interpret
behavior we observed empirically — not as a code source.

Files we cited:

- [`core/hfs_format.h`](https://github.com/apple-oss-distributions/hfs/blob/main/core/hfs_format.h)
  — every on-disk struct definition (`HFSPlusVolumeHeader`,
  `BTNodeDescriptor`, `BTHeaderRec`, `HFSPlusForkData`,
  `HFSPlusCatalog*`, `HFSPlusAttr*`). Authoritative for field
  layouts and constants (`kHFSRootFolderID`,
  `kHFSFirstUserCatalogNodeID`, `kHFSPlusSigWord`, `kHFSXSigWord`,
  signature versions, attribute kind constants, flag bits like
  `kHFSHasFolderCount` and `kHFSVolumeUnmountedMask`).
- [`core/hfs_btreeio.c`](https://github.com/apple-oss-distributions/hfs/blob/main/core/hfs_btreeio.c)
  — `hfs_swap_BTNode` is the function whose error message
  (`offsets X and Y out of order`) confirmed the offset-table
  ordering.
- [`core/UnicodeWrappers.c`](https://github.com/apple-oss-distributions/hfs/blob/main/core/UnicodeWrappers.c)
  — Apple's own NFD wrapping used by the in-kernel HFS+ driver.
  Confirms NFD is the on-disk normalization form.
- [`lib_fsck_hfs/dfalib/SVerify1.c`](https://github.com/apple-oss-distributions/hfs/blob/main/lib_fsck_hfs/dfalib/SVerify1.c)
  — `CheckVolumeHeader`, `CheckCatalogBTree`, `CheckCatalogRecord`,
  `CheckFolderRecord`, `BcheckHFSPlusBitmap`, `GetVolumeFeatures`,
  `CheckExtentsBTree`. These are the functions whose error
  messages drove most of our verifier fixes.
- [`lib_fsck_hfs/dfalib/SVerify2.c`](https://github.com/apple-oss-distributions/hfs/blob/main/lib_fsck_hfs/dfalib/SVerify2.c)
  — `VerifyVolumeHeader`, including the inner/outer `clumpSize`
  consistency check.
- [`lib_fsck_hfs/dfalib/SUtils.c`](https://github.com/apple-oss-distributions/hfs/blob/main/lib_fsck_hfs/dfalib/SUtils.c)
  — `SetupFCB` and the clump-size reconciliation between
  `HFSPlusForkData.clumpSize` and `BTHeaderRec.clumpSize`.
- [`lib_fsck_hfs/dfalib/SBTree.c`](https://github.com/apple-oss-distributions/hfs/blob/main/lib_fsck_hfs/dfalib/SBTree.c)
  — `AllocateNode`, B-tree traversal, record-alignment checks
  (where odd-offset attribute records fail).

The repository's license is the **Apple Public Source License 2.0**
(APSL 2.0). We did not redistribute, mirror, or derive code from any
of its files. Citations in this wiki are bibliographic and fall under
fair use.

## UDIF (DMG) format

There is no official Apple specification for UDIF. The format has
been reverse-engineered from `hdiutil` and the `DiskImages` private
framework. The best community write-ups:

- **vu1tur**, *Demystifying the DMG Format* — the canonical reference
  for KOLY, BLKX, and chunk types:
  <http://newosxbook.com/DMG.html>
- **Jonathan Levin**, *MacOS and iOS Internals*, "Disk Image Formats"
  chapter — book; published references to the KOLY structure.
- The original Apple-published header in older OS X SDKs that
  defined `UDIFResourceFile`:
  search for it on GitHub via
  <https://github.com/search?q=UDIFResourceFile&type=code>.

We rely on CRC32-IEEE (Go's `hash/crc32` `IEEE` table). The
mathematical definition is RFC 3309 / ITU-T V.42.

## HFS+ background reading

- [Wikipedia: HFS Plus](https://en.wikipedia.org/wiki/HFS_Plus)
  — covers the local-vs-UTC timestamp quirk that's universally
  ignored.
- [Singh, Amit, *Mac OS X Internals*](https://www.osxbook.com/), ch.
  "HFS Plus" — the most thorough public description of the on-disk
  format before TN1150 was published.

## Related Go projects (not used, listed for completeness)

These were considered and rejected, with the reasons:

- **`github.com/aoiflux/libhfs`** — read-only HFS+ parser, no
  license declared. Useful as a future test reference if the
  project ever adopts a permissive license, but cannot be used in
  our clean-room build.
- **`github.com/DHowett/go-plist`** — general-purpose Apple
  property-list transcoder, 2-clause BSD. Compatible with our
  Apache-2.0, but not adopted: see "On `go-plist`" discussion in the
  chat. Our DMG plist is a fixed format, write-only, and needs
  byte-stable output that mirrors `hdiutil`'s.
- **`github.com/planetbeing/libdmg-hfsplus`** — the parent project
  of this repository. GPLv3, **not read** by us during this
  implementation; the Go writer is a clean-room re-implementation
  from public format specs only. The decision to start from
  scratch was driven by the GPLv3 license; a permissive license
  would have changed that calculus.

## Tools used during development

- `hdiutil create -srcfolder ... -format {UDRW,UDRO,UDZO}` for
  reference DMGs.
- `hdiutil attach -nomount -noverify` to get a raw device for
  inspection.
- `hdiutil verify` to check our output against Apple's verifier.
- `fsck_hfs -fnd /dev/diskN` for in-depth verification with debug
  output.
- `hexdump -C`, `od`, `xxd` for poking at on-disk layouts.
- `otool -tV` to disassemble `fsck_hfs` when source wasn't clear.
- Python's `struct` module for one-off "what does this byte range
  mean" experiments while debugging.

## Licensing notes

- Our license: Apache 2.0 (`LICENSE`).
- TN1150 and the Apple UDIF-related docs: Apple's documentation
  license. Bibliographic citation is fine.
- `apple-oss-distributions/hfs`: APSL 2.0. We do not redistribute or
  derive from it; we cite specific files to anchor our findings.
- Community write-ups: linked, not copied.

If you find a place in this wiki where we've described a finding
without a citation, please open an issue — the goal of this folder is
that every non-obvious claim can be traced back to a public source.
