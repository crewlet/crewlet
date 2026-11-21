// Package nodememory reads the memory this node's processes may hold: the
// least of the machine's physical memory and every cgroup limit above the
// process (memory_linux.go), or the machine's on Darwin. It is the one reader,
// for every holder whose bound is a share of the node's memory: Gitlet's
// transfer lane and cache budget (internal/gitlet/gitproc).
//
// A figure that cannot be read is an error, never a guess: a bound guessed
// from nothing is the OOM it exists to prevent, and each caller decides what
// it does without one.
package nodememory

// Memory is the memory this node's processes may hold, and From the file or
// call that bound it, which an operator reads to see why.
type Memory struct {
	Bytes int64
	From  string
}
