//go:build linux

package s3obj

import "golang.org/x/sys/unix"

// socketUnacked is SIOCOUTQ: the bytes the socket holds that the far end has
// not acknowledged — sent and unacknowledged, and not yet sent.
func socketUnacked(fd uintptr) (int, error) { return unix.IoctlGetInt(int(fd), unix.SIOCOUTQ) }
