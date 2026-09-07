//go:build darwin || linux

package xattrs

import (
	"fmt"
	"testing"

	"golang.org/x/sys/unix"
)

// TestIsNoXattrErr checks the classification that decides whether a
// missing attribute aborts the whole build or is passed over.
//
// The Darwin case is the one that used to be wrong: getxattr reports a
// missing attribute as ENOATTR, which is a different number from
// ENODATA there (93 vs. 96), so an attribute that disappeared between the
// listing and the read failed the build on the library's main target.
func TestIsNoXattrErr(t *testing.T) {
	benign := []struct {
		name  string
		errno unix.Errno
	}{
		{"ENOTSUP", unix.ENOTSUP},
		{"ENOSYS", unix.ENOSYS},
		{"ENODATA", unix.ENODATA},
	}
	benign = append(benign, platformBenignErrnos()...)

	for _, tt := range benign {
		t.Run(tt.name, func(t *testing.T) {
			if !isNoXattrErr(tt.errno) {
				t.Errorf("isNoXattrErr(%s) = false; a missing attribute or an "+
					"xattr-less filesystem must not fail the build", tt.name)
			}
			// The real call sites see the errno wrapped by x/sys and
			// then by us, so the check has to survive wrapping.
			if !isNoXattrErr(fmt.Errorf("getxattr: %w", tt.errno)) {
				t.Errorf("isNoXattrErr(wrapped %s) = false", tt.name)
			}
		})
	}

	// Real failures must still surface: a build that silently drops
	// com.apple.quarantine because the read was denied is worse than one
	// that stops.
	for _, tt := range []struct {
		name  string
		errno unix.Errno
	}{
		{"EACCES", unix.EACCES},
		{"EPERM", unix.EPERM},
		{"EIO", unix.EIO},
		{"ERANGE", unix.ERANGE},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if isNoXattrErr(tt.errno) {
				t.Errorf("isNoXattrErr(%s) = true; a real failure must not be "+
					"mistaken for an absent attribute", tt.name)
			}
		})
	}
}
