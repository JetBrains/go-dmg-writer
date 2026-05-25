# UDIF (DMG) container format

UDIF is Apple's container for disk images. A `.dmg` file's anatomy is:

```
offset    contents
────────  ───────────────────────────────────────────────────────────────
0 ...     data fork (raw HFS+ image, or zlib-compressed chunks of it)
   ...    XML plist (resource fork emulation: blkx tables, checksums)
size-512  KOLY trailer (512 bytes, fixed at end of file)
```

There is **no header**; the trailer is the source of truth. Readers
(`hdiutil`, the macOS DiskImages kext, our writer) start by seeking to
`size − 512`, validating the KOLY magic, and using offsets it contains.

The two relevant Apple sources are:

- The disassembled DiskImages framework (no public source).
- Community write-ups of UDIF, the canonical one being
  [vu1tur's CSPotato page](http://newosxbook.com/DMG.html)
  and [the unofficial Apple disk-image
  format](http://newosxbook.com/articles/DMG.html).
- The `libdmg-hfsplus` project's reverse-engineered structs (GPL, not
  read or copied by us; we re-derived from the format references).

We do not parse DMGs, only write them.

## KOLY trailer

`internal/udif/koly.go` defines the 512-byte UDIFResourceFile structure
("KOLY" is the magic, "Apple DiskImage" in disassembled binaries).

The fields we set (big-endian):

| Field                  | Value                                                                     |
| ---------------------- | ------------------------------------------------------------------------- |
| `signature`            | `'koly'` (`0x6B6F6C79`)                                                  |
| `version`              | `4`                                                                       |
| `headerSize`           | `512`                                                                     |
| `flags`                | `1`                                                                       |
| `runningDataForkOffset`| `0`                                                                       |
| `dataForkOffset`       | `0`                                                                       |
| `dataForkLength`       | byte length of the compressed/raw data fork                              |
| `rsrcForkOffset`       | offset of the XML plist within the file                                  |
| `rsrcForkLength`       | byte length of the XML plist                                             |
| `segmentNumber`        | `1`                                                                       |
| `segmentCount`         | `1`                                                                       |
| `segmentID`            | random 16-byte UUID                                                       |
| `dataChecksumType`     | `2` (CRC32-IEEE)                                                          |
| `dataChecksumSize`     | `32`                                                                      |
| `dataChecksum`         | CRC32 of the **decompressed** HFS+ image                                  |
| `xmlOffset`/`xmlLength`| same as rsrcFork{Offset,Length}                                          |
| `imageVariant`         | `2`                                                                       |
| `sectorCount`          | total 512-byte sectors in the decompressed image                          |
| `masterChecksumType`   | `2`                                                                       |
| `masterChecksumSize`   | `32`                                                                      |
| `masterChecksum`       | CRC32 of all per-block CRC32s, in BLKX emission order                     |

The split between `dataChecksum` (over the decompressed plaintext) and
`masterChecksum` (over the per-block CRCs) is what makes UDIF's
verification work even when chunks were compressed.

## BLKX block tables

`internal/udif/blkx.go` defines `BLKXTable` and `BLKXRunEntry`. There is
**one BLKX table per partition**. For a DMG containing a single HFS+
volume there is exactly one BLKX, named conventionally
`disk image (Apple_HFSX)`.

Each BLKX is a sequence of runs:

| Run type                  | Value     | Meaning |
| ------------------------- | --------- | ------- |
| `BlockZeroFill`           | `0`       | Decompresses to N sectors of zero. No data fork bytes. |
| `BlockRaw`                | `1`       | Sectors are stored uncompressed in the data fork. |
| `BlockZlib`               | `0x80000005` | Sectors compressed with zlib (the "Z" of UDZO). |
| `BlockBZip2`              | `0x80000006` | Not produced by us. |
| `BlockLZFSE`              | `0x80000007` | Not produced by us. |
| `BlockTerminator`         | `0xFFFFFFFF` | Sentinel, last run only. |

We emit one run per 1 MiB chunk of the decompressed HFS+ image:

- A 1-MiB-sized run of `0x00` becomes a single `BlockZeroFill`.
- In `UDRO` / `UDRW` modes: all non-zero runs are `BlockRaw`.
- In `UDZO` mode: non-zero runs are `BlockZlib`-compressed. If the
  compressed payload is **larger** than the raw payload, we fall back
  to `BlockRaw` for that chunk — a small but real saving for
  already-compressed data (like a code-signed Mach-O binary in the
  source tree).

The `BLKXTable` carries its own CRC32 (`uDIFChecksum`) covering the
decompressed bytes of all runs in the table, plus a per-run starting
sector and sector count.

## XML plist (resource-fork emulation)

`internal/udif/plist.go` produces the XML plist that lives between the
data fork and the KOLY trailer. Its top-level structure mirrors a
classic Mac OS resource fork:

```xml
<plist version="1.0">
<dict>
    <key>resource-fork</key>
    <dict>
        <key>blkx</key>
        <array>
            <dict>
                <key>Attributes</key>   <string>0x0050</string>
                <key>CFName</key>       <string>disk image (Apple_HFSX)</string>
                <key>Data</key>         <data>...base64-encoded BLKXTable...</data>
                <key>ID</key>           <string>0</string>
                <key>Name</key>         <string>disk image (Apple_HFSX)</string>
            </dict>
        </array>
        <key>plst</key>
        <array>
            <dict> ...base64-encoded "partition list" resource... </dict>
        </array>
        <key>size</key>
        <array>
            <dict> ...base64-encoded size record... </dict>
        </array>
    </dict>
</dict>
</plist>
```

We deliberately:

- Hand-write the XML (rather than use `encoding/xml`) for byte-stable
  output. See `internal/udif/plist.go: WriteTo`.
- Use 64-char tab-indented base64 lines to match `hdiutil`'s output and
  make a diff against a reference DMG readable.
- Set the conventional `Attributes` value `0x0050` on every `blkx`
  resource — `hdiutil verify` checks for this.

We do **not** emit `cSum` resources (legacy compressed-checksum hints).
Modern macOS does not require them, and `hdiutil verify` passes
without them.

## Why two checksums?

The KOLY's `dataChecksum` covers the **plaintext** bytes of the HFS+
image — a single CRC32 over the entire decompressed disk image as it
will be seen at mount time. Computed during emission by feeding each
chunk's plaintext bytes through `crc32.IEEE` (see
`internal/udif/checksum.go`).

The `masterChecksum` is a CRC32 over **the concatenation of every BLKX
table's per-block checksums**. It exists so the verifier can detect
tampering of the BLKX tables themselves without re-decompressing every
chunk; in practice macOS verifies the full data checksum anyway, but
the field is required.

## CRC32 implementation

We rely on Go's stdlib `hash/crc32` with the `IEEE` polynomial table.
The streaming wrapper in `internal/udif/checksum.go` exists so that we
can feed bytes lazily as we walk the scratch HFS+ file rather than
slurping it into memory.

## Why chunks at 1 MiB?

Empirically that's what `hdiutil` itself uses. Smaller chunks waste
header overhead and increase plist size; larger chunks lengthen seek
latency for random reads from the mounted DMG. There's no protocol
requirement for any particular chunk size — readers handle arbitrary
sizes — but matching `hdiutil` makes our output diff-friendly against
reference DMGs and avoids surprises in the wild.

## What we don't implement

- **APM/GPT prefix DMGs** — DMGs that contain a real partition map
  followed by one or more partitions. We always emit a "raw partition"
  DMG (no APM/GPT), matching `hdiutil create -srcfolder -format
  UDZO`. The `imageVariant=2` flag in the KOLY trailer expresses this.
- **Encrypted DMGs** (FileVault). Requires the `enc` container around
  the UDIF data fork.
- **License agreement resources** (the `LPic`/`STR#` resources that
  show a click-through EULA at mount time). Possible to add as
  additional resource-fork keys; not required for notarization.
- **Multi-segment DMGs** (`-segmentsize`). Out of scope for the MVP.
- **bzip2 / LZFSE compression** for blocks. Zlib is the most
  widely-supported and tooling matches Apple's defaults.

## References

- [`apple-oss-distributions/hfs:lib_fsck_hfs/dfalib/SVerify1.c`](https://github.com/apple-oss-distributions/hfs/blob/main/lib_fsck_hfs/dfalib/SVerify1.c)
  — verifier-side validation we mirror.
- Jonathan Levin, *MacOS and iOS Internals*, "Disk Image Formats"
  chapter — best public write-up of UDIF.
- vu1tur's [UDIF format reference](http://newosxbook.com/DMG.html).
