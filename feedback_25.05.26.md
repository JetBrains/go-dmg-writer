# `go-dmg` Review: 

A pure-Go, cross-platform writer for Apple UDIF (`.dmg`) disk images containing HFS+ (technically HFSX) volumes. This review covers architecture, correctness, API design, portability, performance, testing, and documentation.

---

## 1. Overall Impression

This is a **carefully engineered, well-scoped library** with a clear mission: produce notarization-friendly read-only DMGs without `cgo` or `hdiutil`. The code is unusually well-commented for a format-encoding library — most non-obvious choices reference TN1150 sections or empirical `fsck_hfs` invariants, which makes the project legible and maintainable.

**Strengths:**

- Clean separation of concerns: `dmg` (orchestration), `internal/hfsplus` (filesystem), `internal/udif` (container).
- Pure Go, no cgo; cross-platform with build-tagged xattr backends.
- Reproducible-build friendly (deterministic walk + fixed timestamp).
- Sensible scope boundary — the README clearly lists what is *not* implemented.
- Constants and structs match TN1150 names; documentation is reference-grade.

**Main weaknesses:**

- API ergonomics around zero-value sentinels.
- Some defensive correctness gaps (silent error swallowing, missing input validation).
- Mode-handling has a likely bug where `ModeReadWrite` and `ModeReadOnly` produce indistinguishable output.
- A latent bit-shift bug in the encodings bitmap that becomes visible with non-ASCII names.

---

## 2. Architecture

### 2.1 Layering

```plain text
dmg.DMG.Create
  ├─ walk / walkSorted        (Pass 1: collect, Pass 2: build entries)
  ├─ readXattrs               (build-tagged: darwin/linux vs others)
  ├─ hfsplus.WriteVolume
  │     ├─ BuildCatalogTree   (1st pass — for sizes)
  │     ├─ BuildExtentsTree
  │     ├─ BuildAttributesTree
  │     ├─ BuildPlan          (resolve block placement + bitmap)
  │     ├─ BuildCatalogTree   (2nd pass — with real extents)
  │     ├─ PadToBlocks ×3
  │     └─ writeAt / streamFileAt for VH, bitmap, BTrees, user data
  └─ udif.Write               (wrap raw HFS image as UDIF)
```


The layering is correct and minimal. There are no circular dependencies between `hfsplus` and `udif`, and the orchestrator owns the FS walk plus xattr collection.

### 2.2 The Two-Pass Packing

The catalog tree is packed twice: first with placeholder extents to learn the byte size, then again after the layout planner has assigned real block placements. The comment correctly justifies this — record sizes don't depend on extent contents (the fork descriptor is fixed-size), so the second pack is guaranteed to produce identically-sized bytes.

**Suggestion:** this could be made cheaper. Instead of fully re-packing, mutate the per-record bytes in place for data-fork-bearing records (the byte offset of the `DataFork` block inside each catalog record is deterministic). For large volumes the double-pack is the most expensive non-I/O step. Not necessary at the current scope, but worth noting.

### 2.3 Single-Extent Assumption

The planner assigns every file a single contiguous extent, and the extents-overflow tree is always emitted empty. This is internally consistent and well-documented.

A defensive runtime check would be welcome: assert that no file's block count exceeds what fits in a single `ExtentDescriptor`. With 32-bit block counts at 4 KiB blocks this is ~16 TiB per file, so practically infinite — but the assertion makes the contract explicit.

---

## 3. Correctness — Items Worth Fixing

### 3.1 `OwnerID == 0` Silently Becomes 99

The zero value of `OwnerID` is overridden to 99 (the HFS+ "unknown user" sentinel). This silently rewrites a legitimately requested owner of `root` (UID 0) into 99.

**Fix:** use a pointer (`*uint32`) for "unset", or a documented sentinel like `^uint32(0)`. Same applies to `GroupID`.

### 3.2 `Mode` Is Accepted Without Validation

An invalid `Mode` value (e.g., `Mode(99)`) is silently treated as `ModeReadOnly`. Either validate at the top of `Create` and return an error, or use an exhaustive `switch` that explicitly rejects unknown values.

### 3.3 `ModeReadWrite` Is Indistinguishable from `ModeReadOnly` on Disk

This is the single most material concern in the public API. The current orchestrator collapses both modes into the same uncompressed UDIF output and never plumbs the distinction down to the UDIF writer. The accompanying comment acknowledges that the on-disk container layout is identical, and the parameter is explicitly discarded.

If UDRW vs UDRO needs a different `ImageVariant` field in the UDIF property list (the conventional Apple distinction), this needs to be plumbed through to the UDIF writer. If both *legitimately* produce identical bytes, then either:

- drop one mode from the public API, or
- tag them differently in the UDIF plist (`UDRO` vs `UDRW` variant string), so consumers can distinguish.

### 3.4 `EncodingsBitmap` Bit-Shift Overflow

The volume header's encodings bitmap is built by OR-ing `1 << e.EncodingBit()` for every entry. `EncodingBit()` returns either `0` (MacRoman) or `0x7F` (Unicode). In Go, shifting a `uint64` by 127 produces a value modulo 64 — i.e. the Unicode bit is **never actually set**.

In practice, macOS HFS+ images almost always carry `EncodingsBitmap == 1` (MacRoman bit only) regardless of names, because the per-record `TextEncoding` field is authoritative. So the symptom may be invisible to `hdiutil verify`, but the code does not do what its author thinks it does. Recommend either explicitly always setting bit 0, or using a small switch to map `0x7F` → bit 1 (or whatever the documented Unicode bit position is).

### 3.5 Silent xattr Error Swallowing on Unix

The xattr reader swallows every error from `Llistxattr` and `Lgetxattr`. `ENOTSUP` and `ENODATA` are legitimate "nothing here" signals; everything else (`EACCES`, `EPERM`, I/O errors) should be surfaced.

There is also a TOCTOU race: the size returned by the first `Llistxattr` call may not match the second. The current code truncates silently. Use a growing-buffer loop, or at minimum re-call until the returned size fits.

### 3.6 Deferred `Close` Errors Are Discarded

The output file's `Close` is deferred but its error is not captured. A flush failure here would silently produce a corrupt DMG. Use a named return value and a deferred closure that propagates the close error when no other error has occurred.

### 3.7 CNID "Reserve Then Maybe Release" Is Fragile

In the first walker pass, a CNID is allocated before the file-type switch and decremented when the entry is skipped (sockets, devices, fifos). The current code is correct, but the pattern is easy to break in future edits. Cleaner: increment the counter only inside the supported-kind branches, eliminating the rollback entirely.

### 3.8 `time.Time{}` Zero-Clamp Silently Rewrites Pre-1904 Times

When `MacTime` returns zero (for pre-1904 times), it is silently set to `1`. This is a defensive measure for `fsck_hfs`, but it silently rewrites a user-provided timestamp. Either reject out-of-range times explicitly, or document the clamp policy in the field comment.

### 3.9 Symlink Targets on Windows

`os.Readlink` on Windows returns Windows-style paths (`\` separators, possibly drive letters). HFS+ symlink targets must be POSIX paths. Either normalise separators when storing the link target, or document the limitation clearly.

### 3.10 Hard Links Become Independent Duplicates

The walker uses `os.Lstat` and treats every directory entry as a unique file. Two hard links to the same inode become two independent file entries with duplicated content. For read-only distribution DMGs this is acceptable, and the README does list "hard links" as unimplemented — worth restating in the package doc that the duplication is silent (not an error).

---

## 4. API Design

### 4.1 `DMG` Struct Ergonomics

| Field | Issue |
|---|---|
| `OwnerID`, `GroupID` | Zero-value sentinel collides with real UID/GID 0. See §3.1. |
| `BlockSize` | Zero-default is fine and clearly documented. |
| `ChunkSectors` | Zero-default is fine. |
| `Time` | Zero-value behaviour is documented; clamp policy in §3.8 is not. |
| `RootFinderInfo` | Length check is good; error message could include the actual length. |

### 4.2 No Progress Callback

For large source folders, image creation can take minutes (especially with zlib compression). A `Progress func(stage string, current, total uint64)` field would be valuable for CLI consumers. Not required for v1.

### 4.3 Positional Arguments to `Create`

`Create(srcFolder, outPath string, mode Mode)` is fine at three arguments. If more knobs accumulate (per-file ACL, license agreement plist, etc.), consider migrating to a `CreateRequest` struct.

### 4.4 Error Wrapping

Consistent and good throughout. The `dmg:` and `hfsplus:` prefixes make it easy to identify the layer that originated an error.

### 4.5 Public Surface

The exported surface is minimal and correct: one struct, one enum with three constants, one method. This is the right shape for a focused library.

---

## 5. Portability

### 5.1 Build Tags

The xattr backend is gated on `darwin || linux`, with everything else falling through to a no-op. This leaves FreeBSD (which has `extattr_*`, a different API) and OpenBSD (no xattrs) on the no-op path. That is reasonable, but consider listing the BSDs explicitly in the "other" file's build constraint so the choice is intentional rather than implicit.

### 5.2 Windows File Modes

`info.Mode().Perm()` on Windows returns synthetic values (typically `0o666` / `0o777`). DMGs built on Windows will have meaningless POSIX mode bits. Worth a sentence in the README beside the existing note about `OwnerID`/`GroupID`.

### 5.3 Windows Symlinks

See §3.9.

---

## 6. Performance

- **Bitmap in memory** — at 4 KiB blocks, a 100 GiB volume needs ~3.2 MiB of bitmap RAM. Negligible.
- **User file streaming** — uses `io.CopyN`, which delegates to `ReaderFrom`/`WriterTo` for `*os.File`. Good.
- **Double catalog pack** — see §2.2. Optimisation opportunity, not a correctness issue.
- **Closure variable capture** — the file opener captures `path` via a local variable assignment before the closure, correctly avoiding the loop-variable trap. Good.
- **UDIF compression** — verify that `ChunkSectors` defaults to 2048 (1 MiB) to match what `hdiutil -format UDZO` writes.

---

## 7. Testing

The project has tests at three levels:

- Unit tests in each `internal/hfsplus/*_test.go` and `internal/udif/*_test.go`.
- Cross-platform integration tests.
- Darwin-only end-to-end tests (presumably calling `hdiutil verify` / `fsck_hfs`).

**Coverage gaps to consider:**

1. Non-ASCII filenames — especially those that exercise the Apple-NFD exception table (which is a known TODO in the Unicode helper).
2. Empty source directory (zero files, zero subfolders).
3. Files with `0o000` permissions.
4. Symlinks pointing outside the source tree — verify the target string is preserved verbatim.
5. xattrs at exactly `MaxAttrNameRunes` (127 code units) — boundary case.
6. xattrs larger than the inline limit — currently errors out; assert the error is returned.
7. **Reproducibility test:** build the same DMG twice with a fixed `Time` and assert byte-for-byte equality. This is a marketed feature; it deserves a regression test.

**CI matrix:** ensure all three target OSes (macOS, Linux, Windows) run on CI. The darwin-only end-to-end test should be gated on actually running on macOS.

---

## 8. Documentation

### 8.1 HFSX vs HFS+ Case Sensitivity

The volume signature is `HX` and the catalog uses binary comparison, which means the produced volumes are **case-sensitive HFSX**, not classic case-insensitive HFS+. macOS mounts these case-sensitively. Apps that rely on case-insensitive lookups (most app bundles are fine; some installers are not) could be surprised. The README should call this out explicitly — the design choice is well-reasoned (avoids implementing Apple's case-folding table) but it has user-visible consequences.

### 8.2 Package Doc

The package doc is good. Worth adding:

- Symlinks are stored as `slnk`/`rhap` files with the POSIX target string in the data fork; on Windows the host path separator may need translation.
- Hard links are silently duplicated as independent files (one CNID and one data copy per directory entry).

### 8.3 Inline TODOs

The Apple-NFD exception table is the only known correctness limitation visible as an inline TODO. Track it explicitly — it's the right thing to defer, but it should not be lost.

---

## 9. Minor / Cosmetic

- The orchestrator's step comments include two consecutive "step 6" markers; renumber.
- Variable naming in the walker mixes single-letter (`p`, `e`) and full-word (`out`, `entries`) styles. Acceptable but inconsistent.
- The custom `walkSorted` helper does not document how it handles sentinel errors (`filepath.SkipDir`-style). Either document or align with `filepath.WalkDir`'s contract.
- The xattr reader has no allow/deny list for `com.apple.system.*` namespaces. Currently every xattr is passed through, which is probably correct, but worth a comment confirming the policy is deliberate.

---

## 10. Priority Summary

| Area | Status | Priority |
|---|---|---|
| Architecture & layering | Good | — |
| HFS+ tree builder correctness | Good (per comments) | — |
| `Mode` validation + UDRW vs UDRO differentiation | Bug | **High** |
| `OwnerID == 0` collides with root UID | Bug | **High** |
| `EncodingsBitmap` `1 << 0x7F` overflow | Bug (likely benign) | **High** |
| xattr error swallowing on Unix | Bug | Medium |
| Deferred `Close` error not propagated | Bug | Medium |
| CNID rollback fragility | Smell | Low |
| Windows symlink target separators | Limitation | Low (document) |
| Apple-NFD exception table | Known TODO | Low |
| README clarity on HFSX case-sensitivity | Documentation | Medium |
| Reproducibility regression test missing | Test gap | Medium |
| Hard-link duplication | Limitation | Low (document) |
| Double catalog packing | Performance opportunity | Low |

---

## 11. Conclusion

This is a solid project with a tight scope, careful format work, and excellent inline documentation. The path to a robust v1 is short:

1. Fix the `Mode` handling so UDRW and UDRO are either distinguishable on disk or one is removed.
2. Fix the `OwnerID`/`GroupID` zero-sentinel.
3. Fix the `EncodingsBitmap` shift, even if the symptom is invisible today.
4. Stop swallowing xattr errors silently.
5. Add a reproducibility regression test and a CI matrix covering all three target OSes.
6. Document the HFSX case-sensitivity choice prominently in the README.

Everything else on the list is incremental polish.