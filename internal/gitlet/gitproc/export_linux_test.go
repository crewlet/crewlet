package gitproc

import "io/fs"

// ReadLinuxMemory reads the memory a process under root may hold — root a
// fixture tree of /proc and cgroup files — and the file that bound it.
func ReadLinuxMemory(root fs.FS) (int64, string, error) {
	m, err := readLinuxMemory(root)
	return m.Bytes, m.From, err
}
