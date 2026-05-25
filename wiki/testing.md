# Testing

The test suite is organized into three layers, in increasing order of
"how much of the real world has to cooperate":

1. **Unit tests** (cross-platform, run on every PR)
2. **End-to-end tests** that build a real DMG and inspect the bytes
   (cross-platform; rely only on the Go stdlib)
3. **Darwin-gated integration tests** that mount the DMG with
   `hdiutil` and run `fsck_hfs` against the raw HFS+ device

## Unit tests

Each `internal/` package has small focused tests. The notable ones:

- `internal/hfsplus/hfstime_test.go` — round-trips `time.Time` through
  the Mac-epoch encoder.
- `internal/hfsplus/unicode_test.go` — UTF-8 → UTF-16BE NFD; binary
  ordering of two known-tricky names.
- `internal/hfsplus/allocation_test.go` — bitmap setting/clearing.
- `internal/hfsplus/btree_test.go` — packs a tiny set of records and
  asserts golden bytes for the header node. Worth reading if you're
  touching the offset table — it's the test that pinned the
  "offset[0] at the top" layout.
- `internal/hfsplus/catalog_test.go` — folder/file record encoding.
- `internal/hfsplus/attributes_test.go` — inline-attribute records,
  including the odd-length pad-byte case.
- `internal/udif/checksum_test.go` — CRC32-IEEE matching
  `hash/crc32` reference values.
- `internal/udif/koly_test.go` — KOLY trailer big-endian layout.
- `internal/udif/writer_test.go` — end-to-end UDIF wrapper across a
  small synthetic HFS+ image.

`go test ./internal/...` runs all of these in <2 seconds on any
platform.

## End-to-end (`dmg_test.go`)

These build real DMGs from a tempdir source tree and assert
properties about the produced bytes without relying on any platform
tooling. Highlights:

- `TestCreateBasic` — small folder, UDRW. Asserts the file exists,
  has a valid KOLY trailer at the end, the data fork CRC matches a
  freshly-computed one over the decompressed bytes, and the XML plist
  parses.
- `TestCreateDeterministic` — builds the same source folder twice
  with a fixed `DMG.Time` and asserts byte-equality of the two DMGs.
- `TestCreateSymlinks` — source tree has symlinks; verifies that
  catalog records for them are file records with the symlink Finder
  type bits and that the link target is stored in the data fork.
- `TestCreateMultiChunk` — source tree large enough to span multiple
  1 MiB UDIF chunks; asserts that the BLKX table has more than one
  run.
- `TestCreateUDZO` — same content as `TestCreateBasic` but UDZO;
  asserts that the data fork is meaningfully smaller than the raw
  HFS+ image (compression ratio sanity check).

These run on every platform. They don't catch every kind of HFS+
verifier issue (that's what the Darwin tests are for), but they catch
regressions in our own code.

## Darwin integration tests (`dmg_darwin_test.go`)

Gated with `//go:build darwin` so they only compile on macOS.

These shell out to `hdiutil`/`fsck_hfs`, so they can't run inside a
sandbox that restricts subprocesses. In CI they need a real macOS
runner.

### `TestHdiutilVerify`

Runs `hdiutil verify <path>` for each of UDRW, UDRO, UDZO modes. This
is the bar Apple's notary service applies for read-only images:
verifies KOLY, master checksum, data fork checksum, and per-block
CRC32s.

### `TestFsckHFS`

This one was the most fiddly to get right:

1. `hdiutil attach -nomount -noverify <dmg>` to get a `/dev/diskN`
   device for the raw HFS+ filesystem (without mounting). For a DMG
   without a partition map (which is what we emit), the relevant
   device is `/dev/diskN` directly — *not* `/dev/diskNs1`.
2. Parse the `hdiutil` stdout, which prints lines like
   `/dev/diskN GUID_partition_scheme` or just `/dev/diskN` for our
   case. We match the first slash-dev path.
3. `fsck_hfs -fnd /dev/diskN` runs in "force / non-interactive /
   debug" mode. Non-root works because `/dev/diskN` is readable by
   the calling user (`/dev/rdiskN` would not be).
4. `hdiutil detach <device>` cleans up.

If any step in this chain finds a problem, the test fails with the
verifier output. **This is the test that found 90% of the bugs
documented in [fsck-hfs-rules.md](fsck-hfs-rules.md).**

### `TestMountAndCompare`

The most realistic test: attach + mount the DMG, walk the mounted
volume, compare against the source folder.

- Mode loops through UDRW, UDRO, UDZO.
- For each entry: compare name (after `NFC` normalization on both
  sides — see [hfs-plus-format.md §"Unicode normalization
  (NFD)"](hfs-plus-format.md#unicode-normalization-nfd)), file type
  (regular/symlink), and *file size* (only for regular files;
  directory sizes are filesystem-metadata-defined and not
  comparable).
- Symlink targets are compared via `os.Readlink`.

This test catches semantic issues that bytes-on-disk tests can't:
e.g., a record that *looks* fine but causes the HFS+ driver to
present the wrong filename, or a permissions field that comes back
zeroed.

## Running locally

```sh
# unit + e2e (any platform)
go test ./...

# everything, including darwin-gated tests (macOS only)
go test -tags=integration ./...     # currently no build tag needed
                                    # because //go:build darwin handles it
```

For the Darwin tests, ensure `hdiutil` and `fsck_hfs` are in `$PATH`
(default on macOS). No root required.

## CI suggestions

- **Linux CI**: runs `go test ./...` and lints. Catches regressions
  in the deterministic build, the BLKX construction, and the
  byte-level encoding.
- **macOS CI**: same, plus the Darwin-gated tests. This is the gate
  for shipping a release: a green macOS build means the produced
  DMGs pass `hdiutil verify` and `fsck_hfs`, which is the
  signal-quality bar Apple's notarization expects.

## Adding new tests

Some patterns we found useful:

- **Use a temp folder and build the source tree from `WriteFile` /
  `Mkdir` / `Symlink` calls.** Don't rely on git-tracked test
  fixtures; macOS and Linux line endings / xattrs differ and break
  fixture-based comparisons.
- **Always pass a fixed `DMG.Time` if you assert on bytes.** Otherwise
  the test is non-deterministic.
- **Print the `hdiutil` / `fsck_hfs` output on failure**, even when
  the call succeeded — having both `stdout` and `stderr` in the
  failure log makes triage much faster.
- **If a Darwin test starts failing locally but not in CI, run
  `hdiutil detach` for every dangling device first.** The macOS
  DiskImages framework keeps state across crashed test runs.
