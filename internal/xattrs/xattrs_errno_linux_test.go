package xattrs

import "golang.org/x/sys/unix"

// platformBenignErrnos is empty on Linux: ENOTSUP and EOPNOTSUPP are the
// same number there, and ENOATTR does not exist as a separate constant -
// glibc defines it as an alias for ENODATA, which the shared list
// already covers.
func platformBenignErrnos() []struct {
	name  string
	errno unix.Errno
} {
	return nil
}
