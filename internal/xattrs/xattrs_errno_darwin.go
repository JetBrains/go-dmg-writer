package xattrs

import "golang.org/x/sys/unix"

// noXattrErrnos are the Darwin errnos that mean "there is nothing here",
// as opposed to a real failure. Darwin gives ENOTSUP (45), ENOATTR (93),
// ENODATA (96) and EOPNOTSUPP (102) four distinct numbers, so all four
// are listed; ENOATTR is the one getxattr returns for an attribute that
// has disappeared since the listing, the benign race [ReadXattrs] tolerates.
//
// Linux collapses some of these and has no ENOATTR at all, so the list
// is per-platform rather than one switch.
var noXattrErrnos = []error{
	unix.ENOTSUP,
	unix.ENOSYS,
	unix.ENODATA,
	unix.ENOATTR,
	unix.EOPNOTSUPP,
}
