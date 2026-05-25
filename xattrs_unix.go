//go:build darwin || linux

package dmg

import (
    "strings"

    "golang.org/x/sys/unix"

    "github.com/jetbrains/go-dmg-writer/internal/hfsplus"
)

// readXattrs reads every extended attribute on path and returns them as
// [hfsplus.Attr] records keyed by fileID. Uses Llistxattr / Lgetxattr so
// symlinks report their own xattrs rather than the target's.
func readXattrs(path string, fileID uint32) ([]hfsplus.Attr, error) {
    sz, err := unix.Llistxattr(path, nil)
    if err != nil || sz <= 0 {
        return nil, nil
    }
    buf := make([]byte, sz)
    n, err := unix.Llistxattr(path, buf)
    if err != nil || n <= 0 {
        return nil, nil
    }

    var out []hfsplus.Attr
    for _, name := range strings.Split(strings.TrimRight(string(buf[:n]), "\x00"), "\x00") {
        if name == "" {
            continue
        }
        vsz, err := unix.Lgetxattr(path, name, nil)
        if err != nil || vsz < 0 {
            continue
        }
        val := make([]byte, vsz)
        vn, err := unix.Lgetxattr(path, name, val)
        if err != nil || vn < 0 {
            continue
        }
        out = append(out, hfsplus.Attr{
            FileID: fileID,
            Name:   name,
            Data:   val[:vn],
        })
    }
    return out, nil
}
