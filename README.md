# go-dmg-writer

A pure-Go, cross-platform writer for Apple's UDIF (`.dmg`) disk images
containing an HFS+ filesystem.

```go
package main

import "github.com/jetbrains/go-dmg-writer"

func main() {
    d := &dmg.DMG{VolumeName: "MyApp"}
    if err := d.Create("./build/MyApp", "MyApp.dmg", dmg.ModeReadOnlyCompressed); err != nil {
        panic(err)
    }
}
```

## Features

- **Cross-platform** — builds and runs on macOS, Linux, and Windows. No cgo,
  no `hdiutil` shell-out, no external dependencies beyond
  `golang.org/x/text` (for Unicode normalization) and `golang.org/x/sys`
  (for reading source-side xattrs on POSIX).
- **Two output modes**:
  - `ModeReadOnly` (UDRO) — uncompressed UDIF, read-only at mount time.
  - `ModeReadOnlyCompressed` (UDZO) — per-chunk zlib compression. This is
    the format you want for shipping signed and notarized apps.
- **Minimal extended attributes** — passes through `com.apple.quarantine`
  and other small xattrs found on source files, plus an optional
  `RootFinderInfo` field for setting a custom volume icon / Finder window.
- **Optional GUID partition table** — set `PartitionMap: true` to frame the
  volume as a whole disk (protective MBR, primary and backup GPT, one
  partition carrying Apple's HFS type GUID), matching
  `hdiutil create -layout GPTSPUD`. macOS mounts a map-less image fine; the
  map is what a tool that *parses* a `.dmg` without mounting it needs. Note
  that the framed layout names its payload partition `disk image` rather
  than `VolumeName`, exactly as `hdiutil` does; `VolumeName` is still what
  Finder shows, because it lives on the volume's root folder.
- **Deterministic output** — given the same source folder and a fixed
  `Time`, repeated runs produce byte-identical DMGs (useful for
  reproducible builds and CI caching).

## Limitations

- **The produced volume is HFSX, not classic HFS+.** HFSX is HFS+ with
  binary (case-sensitive) catalog comparison and is mountable on every
  macOS release since 10.3. Most app bundles, frameworks, and assets are
  case-correct already and mount cleanly. A handful of legacy apps —
  typically older installers ported from Windows — depend on
  case-insensitive lookups (e.g. opening `Foo.PNG` when the file on disk
  is `foo.png`) and will fail on a `go-dmg`-built image. If your app
  is one of those, you have to rebuild the bundle with consistent
  casing or use Apple's `hdiutil`.
- **Owner / group defaults are root (UID 0).** The struct's zero-valued
  `OwnerID` / `GroupID` are taken literally as UID/GID 0. Set them to
  `dmg.OwnerIDUnset` to get the conventional `hdiutil` "unknown user"
  (99) behavior, which is what you typically want for distribution
  DMGs that should mount with the same permissions on any host.
- **POSIX mode bits are meaningless when built on Windows.** Go reports
  synthetic `0o666`/`0o777` values from `os.FileMode.Perm()` on
  Windows; those bytes are written verbatim into the catalog.
- **Hard links are silently duplicated.** The walker uses `os.Lstat`
  and treats every directory entry as a unique file. Two hard links
  pointing at the same inode become two independent catalog entries
  with their own copy of the data. For read-only distribution DMGs
  this is benign; consolidate hard links before calling `Create` if it
  isn't.
- **Symlink targets are stored verbatim.** On Windows, `os.Readlink`
  returns paths with backslash separators; those bytes go onto the
  volume unchanged. Mounting that image on macOS will not resolve
  absolute Windows-style targets.

## Not implemented

- HFS+ journaling (deprecated for distribution images)
- Resource forks (modern code signing lives inside the bundle, not in
  resource forks)
- FileVault encryption
- Apple Partition Map (GPT is supported, see `PartitionMap` above; the
  default is a partition-image DMG, matching
  `hdiutil create -srcfolder -format UDZO`)
- Reading existing DMGs (this library only writes)

## License
```
   Copyright 2026 JetBrains s.r.o.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
```