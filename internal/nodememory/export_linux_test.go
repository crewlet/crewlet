package nodememory

import "io/fs"

// ReadLinux reads the memory a process under root may hold (root a fixture
// tree of /proc and cgroup files) and the file that bound it.
func ReadLinux(root fs.FS) (int64, string, error) {
	m, err := readLinux(root)
	return m.Bytes, m.From, err
}
