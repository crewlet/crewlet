//go:build !linux

package sandbox

// boxOOMScore has nothing to answer off Linux: no other platform this backend
// runs on gives an unprivileged process a kernel OOM priority to set (macOS's
// memory-pressure priorities are the system's), so a box and its engine
// compete as any two processes do there.
func boxOOMScore() (int, bool) { return 0, false }

// oomFirst is argv as written off Linux; see [boxOOMScore].
func oomFirst(argv []string) []string { return argv }
