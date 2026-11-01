package sandbox

import (
	"fmt"
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// NewContainerBoxAt is a container box whose directory is root, for the file
// contract suite: a container box's reads and writes happen on the HOST side
// of its bind mount, so its file contract can be certified without a container
// runtime. Nothing it reaches runs a command. It answers the box and the host
// directory its home is — the one the mount would carry, which is not root
// itself, since root also holds the engine's records about the box. The home
// is made as Create makes it: every access to the box goes through it.
func NewContainerBoxAt(root string) (Sandbox, string) {
	layout := boxLayout{id: "contract", root: root}
	if err := os.MkdirAll(layout.home(), hostbox.DirMode); err != nil {
		panic(fmt.Sprintf("NewContainerBoxAt: %v", err))
	}
	return &containerBox{layout: layout, cli: containerCLI{path: "docker"}, container: "contract"}, layout.home()
}

// WriteExecutable is [writeExecutable], for the stubs the package's external
// suite writes and its shell runs (setuprecipe_test.go, clone_test.go): those
// cases share this binary, and so every fork its other cases make — and
// TestEveryExecutableThisSuiteWritesIsWrittenUnderTheForkLock holds every
// executable write of either suite to the one writer.
func WriteExecutable(t *testing.T, path, script string) {
	t.Helper()
	writeExecutable(t, path, script)
}

// CapReads holds a local box's whole reads to n bytes, for the file contract
// suite: it certifies the refusal one byte past the cap and the whole read at
// it, which is the same branch at any cap and two 32 MiB files per backend at
// the real one. The fake's own [FakeSandbox.CapReads] is exported, because the
// runner suites outside this package need it too.
func CapReads(box Sandbox, n int) Sandbox {
	switch b := box.(type) {
	case *directBox:
		b.readCap = n
	case *containerBox:
		b.readCap = n
	default:
		panic(fmt.Sprintf("CapReads: %T is not a local box", box))
	}
	return box
}

// ReadLimit is the most box reads whole.
func ReadLimit(box Sandbox) int {
	switch b := box.(type) {
	case *directBox:
		return readLimit(b.readCap)
	case *containerBox:
		return readLimit(b.readCap)
	case *FakeSandbox:
		return b.ReadCap()
	}
	panic(fmt.Sprintf("ReadLimit: %T reads by no cap this package holds", box))
}
