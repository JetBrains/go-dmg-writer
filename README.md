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
- **Three output modes**:
  - `ModeReadWrite` (UDRW) — uncompressed, raw, mountable read-write.
    Useful while iterating. Not suitable for distribution because any
    modification invalidates the UDIF data-fork CRC32 and notarization
    refuses non-read-only DMGs.
  - `ModeReadOnly` (UDRO) — uncompressed UDIF, read-only at mount time.
  - `ModeReadOnlyCompressed` (UDZO) — per-chunk zlib compression. This is
    the format you want for shipping signed and notarized apps.
- **Minimal extended attributes** — passes through `com.apple.quarantine`
  and other small xattrs found on source files, plus an optional
  `RootFinderInfo` field for setting a custom volume icon / Finder window.
- **Deterministic output** — given the same source folder and a fixed
  `Time`, repeated runs produce byte-identical DMGs (useful for
  reproducible builds and CI caching).

## Status

Format support is engineered to pass `hdiutil verify` and `fsck_hfs -fn`
so the produced images survive `codesign` + `notarytool submit`.

## Limitations

- HFS+ journaling (deprecated for distribution images)
- Hard links
- Resource forks (modern code signing lives inside the bundle, not in
  resource forks)
- FileVault encryption
- Apple Partition Map / GPT prefix (we emit partition-image DMGs,
  matching `hdiutil create -srcfolder -format UDZO`)
- Reading existing DMGs (this library only writes)

## Example CLI

```
$ go run ./examples/mkdmg -src ./build/MyApp -out MyApp.dmg -mode udzo -name MyApp
```

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
