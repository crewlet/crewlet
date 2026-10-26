//go:build !linux

package cache

// readMapCount is Linux's default count of mappings a process may hold:
// darwin, the one other platform the engine builds for (internal/store's
// platform.go), keeps no such count of the same order, and a git node in
// production runs Linux — in a container on a Mac too — so the bound a node
// there meets is the one a darwin build holds itself to.
func readMapCount() (int, error) { return DefaultMapCount, nil }
