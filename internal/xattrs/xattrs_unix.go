//go:build darwin || linux

package xattrs

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/jetbrains/go-dmg-writer/internal/hfsplus"
)

// ReadXattrs reads every extended attribute on the path and returns them as
// [hfsplus.Attr] records keyed by fileID. Uses Llistxattr / Lgetxattr so
// symlinks report their own xattrs rather than the target's.
//
// Policy: every xattr is passed through verbatim, including the
// `com.apple.system.*` namespace. The macOS notarization pipeline
// expects this - it relies on `com.apple.quarantine` and friends being
// present on bundle contents. If you need to filter, do it before
// calling [DMG.Create].
//
// Errors:
//   - "the filesystem has no xattrs" and "this object has no such
//     attribute" are both treated as "nothing here" and return
//     (nil, nil). The exact errnos differ per platform; see
//     [isNoXattrErr] and noXattrErrnos.
//   - Anything else (EACCES, EPERM, EIO, ...) is wrapped and returned.
func ReadXattrs(path string, fileID uint32) ([]hfsplus.Attr, error) {
	names, err := listXattrNames(path)
	if err != nil {
		if isNoXattrErr(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("dmg: listxattr %q: %w", path, err)
	}

	var out []hfsplus.Attr
	for _, name := range names {
		if name == "" {
			continue
		}
		val, err := getXattrValue(path, name)
		if err != nil {
			if isNoXattrErr(err) {
				// Attribute disappeared between list and get (or
				// the filesystem dropped support mid-walk).
				continue
			}
			return nil, fmt.Errorf("dmg: getxattr %q on %q: %w", name, path, err)
		}
		out = append(out, hfsplus.Attr{
			FileID: fileID,
			Name:   name,
			Data:   val,
		})
	}
	return out, nil
}

// listXattrNames runs Llistxattr in a grow-and-retry loop so we don't
// truncate when the set of attrs grows between the size probe and the
// data read (TOCTOU). The loop terminates when the buffer is large
// enough that a follow-up call doesn't return ERANGE.
func listXattrNames(path string) ([]string, error) {
	// Probe the required size; the kernel returns 0 when there are
	// no attributes and ERANGE if the buffer (which we pass as nil
	// here, asking only for size) is too small for a future caller.
	sz, err := unix.Llistxattr(path, nil)
	if err != nil {
		return nil, err
	}
	if sz == 0 {
		return nil, nil
	}

	// Retry up to a small bound to handle attrs being added between
	// the size probe and the data read. Each iteration grows the
	// buffer; ERANGE means the kernel rejected our buffer because
	// it shrank between probe and read.
	buf := make([]byte, sz)
	for attempt := 0; attempt < 8; attempt++ {
		n, err := unix.Llistxattr(path, buf)
		if err == nil {
			return splitNullDelimited(buf[:n]), nil
		}
		if !errors.Is(err, unix.ERANGE) {
			return nil, err
		}
		// Buffer was too small. Re-probe and grow.
		sz, err = unix.Llistxattr(path, nil)
		if err != nil {
			return nil, err
		}
		if sz <= len(buf) {
			// Pathological: kernel says ERANGE but probe agrees the
			// buffer is large enough. Grow defensively.
			sz = len(buf) * 2
		}
		buf = make([]byte, sz)
	}
	return nil, fmt.Errorf("dmg: listxattr %q: too many ERANGE retries", path)
}

// getXattrValue reads one attribute's bytes with the same grow-and-retry
// pattern as [listXattrNames], so a value growing between probe and read
// doesn't truncate.
func getXattrValue(path, name string) ([]byte, error) {
	vsz, err := unix.Lgetxattr(path, name, nil)
	if err != nil {
		return nil, err
	}
	if vsz == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, vsz)
	for attempt := 0; attempt < 8; attempt++ {
		n, err := unix.Lgetxattr(path, name, buf)
		if err == nil {
			return buf[:n], nil
		}
		if !errors.Is(err, unix.ERANGE) {
			return nil, err
		}
		vsz, err = unix.Lgetxattr(path, name, nil)
		if err != nil {
			return nil, err
		}
		if vsz <= len(buf) {
			vsz = len(buf) * 2
		}
		buf = make([]byte, vsz)
	}
	return nil, fmt.Errorf("dmg: getxattr %q on %q: too many ERANGE retries", name, path)
}

// isNoXattrErr reports whether err means "this filesystem / object has
// no extended attributes" - those are legitimate "nothing here"
// signals, distinct from real I/O or permission failures.
//
// The set is per-platform ([noXattrErrnos]) because the errno numbers
// are: Darwin gives ENOATTR, ENODATA, ENOTSUP, and EOPNOTSUPP four
// distinct values, while Linux has no ENOATTR at all and makes ENOTSUP
// and EOPNOTSUPP the same number.
func isNoXattrErr(err error) bool {
	for _, errno := range noXattrErrnos {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

func splitNullDelimited(buf []byte) []string {
	// Llistxattr returns NUL-separated names. Trim the trailing NUL to
	// avoid an empty trailing element from strings.Split.
	s := strings.TrimRight(string(buf), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}
