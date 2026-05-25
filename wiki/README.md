# `go-dmg` — engineering notes

This folder is a working-developer wiki for the `go-dmg` writer: notes on
the on-disk formats we emit, the verifier rules we had to satisfy, the
architectural choices behind the public API, and citations into the public
Apple sources that informed each finding.

Everything here was produced from a clean-room reading of public
specifications (Apple Technical Note TN1150, the published UDIF format,
and Apple's open-sourced `hfs` repository). No GPL-licensed code from the
parent `libdmg-hfsplus` project — nor any other GPL source — was read,
copied, or translated while building the Go writer. The wiki references
Apple's open sources only to corroborate or explain findings that were
discovered empirically by running `hdiutil verify` and `fsck_hfs` against
our output.

## Where to start

- **[architecture.md](architecture.md)** — how the code is laid out and
  what flows where. Read this first if you're trying to find your way
  around the source tree.
- **[hfs-plus-format.md](hfs-plus-format.md)** — a self-contained tour of
  the HFS+ on-disk structures we emit, with annotations explaining
  *why* each field is set the way it is.
- **[udif-format.md](udif-format.md)** — the DMG container (KOLY trailer,
  BLKX block tables, XML plist) and how `internal/udif` builds it.
- **[btree-investigation.md](btree-investigation.md)** — a deep dive on
  the single hardest problem in this codebase: the B-tree node offset
  table layout, which is documented incorrectly (or at least ambiguously)
  in TN1150 and required empirical inspection of an `hdiutil`-produced
  reference image to nail down.
- **[fsck-hfs-rules.md](fsck-hfs-rules.md)** — every verifier rule we
  discovered the hard way, with pointers into Apple's open-source
  `fsck_hfs` (`apple-oss-distributions/hfs`) so a future maintainer can
  re-derive each rule rather than taking our word for it. This is the
  "lessons learned" doc.
- **[testing.md](testing.md)** — how the test suite is organized, with
  particular attention to the Darwin-gated integration tests that exercise
  `hdiutil verify`, `fsck_hfs`, and a real mount.
- **[references.md](references.md)** — canonical pointers to TN1150, the
  Apple `hfs` repository, the UDIF format references, and a few helpful
  third-party write-ups.

## Quick orientation

```
go-dmg/
├── dmg.go                # public API: type DMG, func Create
├── examples/mkdmg/       # tiny CLI wrapper around dmg.Create
├── internal/
│   ├── hfsplus/          # builds the HFS+ filesystem image
│   └── udif/             # wraps the HFS+ image in a UDIF/DMG container
├── xattrs_*.go           # platform-specific xattr reads (POSIX vs stub)
└── wiki/                 # this folder
```

The pipeline is straightforward:

```
folder on disk
   │
   ▼
walk + xattrs  ──►  catalog entries + extents
                              │
                              ▼
                  HFS+ layout planner (layout.go)
                              │
                              ▼
                  builder.go writes raw HFS+ image to scratch file
                              │
                              ▼
                  UDIF writer chunks the image, emits KOLY + plist
                              │
                              ▼
                          .dmg file
```

## Notes on scope

This wiki documents the parts of the formats we actually emit. We do not
attempt to be a complete reference for HFS+ or UDIF — TN1150 and the
sources cited in [references.md](references.md) already do that. Where
this wiki adds value is in explaining *why* the Go writer makes the
specific choices it does, and in capturing tribal knowledge that does
not appear in any single public document — most notably the
verifier-clean rules and the B-tree node layout.
