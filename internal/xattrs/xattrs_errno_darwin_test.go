package xattrs

import (
	"errors"
	"testing"

	"golang.org/x/sys/unix"
)

// platformBenignErrnos are the "nothing here" errnos that exist only on
// Darwin. Both are separate numbers from their Linux equivalents, which
// is the whole reason the list is built per platform.
func platformBenignErrnos() []struct {
	name  string
	errno unix.Errno
} {
	return []struct {
		name  string
		errno unix.Errno
	}{
		{"ENOATTR", unix.ENOATTR},
		{"EOPNOTSUPP", unix.EOPNOTSUPP},
	}
}

// TestDarwinErrnosAreDistinct documents why the darwin list needs its
// extra two entries. If a future Darwin release collapsed these onto
// ENODATA / ENOTSUP the list would merely be redundant, not wrong, but
// the assumption is worth stating out loud.
func TestDarwinErrnosAreDistinct(t *testing.T) {
	if errors.Is(unix.ENODATA, unix.ENOATTR) {
		t.Error("ENOATTR == ENODATA on this platform; the darwin errno list has a redundant entry")
	}
	if errors.Is(unix.EOPNOTSUPP, unix.ENOTSUP) {
		t.Error("EOPNOTSUPP == ENOTSUP on this platform; the darwin errno list has a redundant entry")
	}
}
