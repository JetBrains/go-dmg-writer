package xattrs

import "golang.org/x/sys/unix"

// noXattrErrnos are the Linux errnos that mean "there is nothing here",
// as opposed to a real failure. It is shorter than the Darwin list
// because EOPNOTSUPP is the same number as ENOTSUP (95) here, and
// because ENOATTR - glibc's name for ENODATA (61) - has no constant on
// Linux in golang.org/x/sys/unix.
var noXattrErrnos = []error{
	unix.ENOTSUP,
	unix.ENOSYS,
	unix.ENODATA,
}
