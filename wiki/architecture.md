# Architecture

This document describes how the source tree is organized, the data flow
from a source folder to a final `.dmg` on disk, and why responsibilities
are split the way they are.

## Package layout

```
github.com/deneb/go-dmg
├── dmg.go                     // public API: type DMG, func (DMG) Create
├── xattrs_{darwin,linux,other}.go
├── examples/mkdmg/main.go     // tiny CLI demo
└── internal/
    ├── hfsplus/               // HFS+ filesystem image
    │   ├── types.go           // on-disk struct definitions (big-endian)
    │   ├── hfstime.go         // Go time.Time ↔ HFS+ Mac-epoch seconds
    │   ├── unicode.go         // UTF-8 → UTF-16BE NFD; binary key compare
    │   ├── allocation.go      // allocation bitmap construction
    │   ├── btree.go           // generic write-once B-tree packer
    │   ├── catalog.go         // catalog records + tree build
    │   ├── extents.go         // extents-overflow tree (always empty)
    │   ├── attributes.go      // attributes tree for xattrs
    │   ├── volumeheader.go    // VolumeHeader construction
    │   ├── layout.go          // block accounting / fork extent planner
    │   └── builder.go         // top-level: walk → entries → bytes
    └── udif/                  // UDIF/DMG container
        ├── koly.go            // 512-byte KOLY trailer struct
        ├── blkx.go            // BLKX block-table structs + chunk types
        ├── checksum.go        // CRC32-IEEE streaming, UDIFChecksum
        ├── plist.go           // XML plist (resource-fork emulation)
        └── writer.go          // ties the above together; streams DMG out
```

`internal/` packages are deliberately not exported. The public surface is
intentionally tiny:

- `type DMG` (`dmg.go`)
- `type WriteMode`, with `ModeReadWrite`, `ModeReadOnly`,
  `ModeReadOnlyCompressed`
- `func (*DMG) Create(srcDir, outPath string, mode WriteMode) error`

The rationale is that DMG and HFS+ have many knobs we don't want to
expose as configuration (block size, encoding bitmap, fork ordering,
…). Forcing all consumers through a narrow API gives us room to change
internals without breaking callers.

## End-to-end data flow

The flow below uses arrows to show where bytes physically move:

```
                                                                                              ┌──────────────────────┐
folder on disk ──►  filepath.Walk + lstat + xattrs   ──►  []catalog.Entry                     │ scratch HFS+ image   │
                    (dmg.go : walk)                       (in-memory, deterministic order)    │ (os.TempFile)        │
                                                                ▼                             │                      │
                                                       layout.BuildPlan                       │                      │
                                                       (block accounting,                     │                      │
                                                        bitmap, fork sizes)                   │                      │
                                                                ▼                             │                      │
                                                       builder.BuildImage                     │                      │
                                                       writes:                                │   ┌──── VH @ 1024    │
                                                          • Volume Header                     │   ◄────── bitmap     │
                                                          • allocation bitmap                 │   ◄────── extents BT │
                                                          • extents B-tree (empty)            │   ◄────── catalog BT │
                                                          • catalog B-tree                    │   ◄────── attrs BT   │
                                                          • attributes B-tree                 │   ◄────── user data  │
                                                          • per-file data forks               │   ◄────── alt-VH     │
                                                          • alt-VH @ size-1024                │   │ scratch HFS+ img │
                                                                ▼                             └───┴──────────────────┘
                                                                │
                              ┌─────────────────────────────────┘
                              ▼
                   udif.Writer streams scratch image in 1 MiB chunks:
                      • per-chunk RAW or ZLIB block ─► data fork
                      • CRC32 of plaintext bytes    ─► master checksum
                      • per-block CRC32 (decompressed) ─► block checksum
                      • final mish/BLKX struct      ─► XML plist resource
                              ▼
                   udif.Writer writes:
                      [ compressed/raw data fork bytes ][ XML plist ][ 512-byte KOLY trailer ]
                              ▼
                          final .dmg
```

The scratch HFS+ image is always materialized as a temporary file —
never held entirely in memory — so a multi-GiB source folder produces a
multi-GiB scratch file but a fixed memory footprint. The UDIF writer
then streams the scratch file out in 1 MiB chunks.

## Why two passes (scratch file + UDIF wrap)?

Two reasons:

1. **HFS+ wants final block counts up front.** The Volume Header sits at
   offset 1024 and contains the total allocation block count, the
   freeBlocks counter, the locations of every B-tree fork, and the
   `nextCatalogID`. We can't know these without first laying everything
   out. A two-pass design (plan in memory → write to disk) is far simpler
   than a single-pass streaming HFS+ encoder.

2. **UDIF blocks the whole image into chunks** with per-chunk CRC32s and
   optional zlib compression. Doing this on a stream is awkward when the
   underlying data isn't known at the start (because we need to fill in
   the data-fork size and master CRC32 in the trailer). Reading from a
   scratch file and writing forwards-only into the `.dmg` keeps the UDIF
   side a clean single pass.

## Determinism

A central design goal is "build twice from the same source, get the same
bytes out". Sources of non-determinism we deliberately suppress:

- **map iteration**: catalog, extents, and attributes entries are
  collected into slices and `sort.Slice`-d into HFS+ binary order before
  emission.
- **walk order**: `filepath.Walk` is alphabetically sorted by Go, so
  the source-side ordering is also deterministic.
- **clock**: `DMG.Time` defaults to the source folder's mtime if unset,
  giving stable timestamps as long as the source is stable. Callers can
  override with a fixed time.
- **plist whitespace and key order**: the XML plist is hand-written
  (`internal/udif/plist.go`) rather than going through `encoding/xml`,
  which would shuffle attribute order.

`dmg_test.go: TestCreateDeterministic` runs `Create` twice and asserts
byte-equality.

## Why the public API takes a folder path, not an `fs.FS`

We considered `fs.FS` for a more idiomatic interface, but rejected it
because:

- `lstat` is not part of the `fs.FS` interface (symlinks become opaque),
  and symlinks are first-class entries in HFS+ that we need to preserve.
- Extended attributes are filesystem-specific and only accessible
  through OS calls on the real path. There is no portable `fs.FS`
  abstraction for xattrs.

If a virtual-filesystem layer is desired later, it would live above
`DMG.Create` and materialize to a real folder before calling.
