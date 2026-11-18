//go:build !linux

package cache

// readMapCount is Linux's default count of mappings a process may hold:
// darwin (the only other build target, internal/store's platform.go) has no
// count of the same order, and a production Gitlet node runs Linux, in a
// container on a Mac too, so a darwin build holds itself to the bound that
// node meets.
func readMapCount() (int, error) { return DefaultMapCount, nil }
