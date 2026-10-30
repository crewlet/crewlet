package sandbox

import (
	"fmt"
	"testing"
)

// WriteRuntime is [writeRuntime] for this package's external cases: a
// stand-in they put on a step's PATH is an executable this suite writes, so
// it is written and settled by the one writer
// TestEveryExecutableThisSuiteWritesIsSettled holds every such write to —
// written any other way, its first exec can fail ETXTBSY under a fork beside
// it, and the step it stands in for reads as the recipe being wrong.
func WriteRuntime(t *testing.T, path, body string) {
	t.Helper()
	writeRuntime(t, path, body)
}

// NewContainerBoxAt is a container box whose directory is root, for the file
// contract suite: a container box's reads and writes happen on the HOST side
// of its bind mount, so its file contract can be certified without a container
// runtime. Nothing it reaches runs a command. It answers the box and the host
// directory its home is — the one the mount would carry, which is not root
// itself, since root also holds the engine's records about the box.
func NewContainerBoxAt(root string) (Sandbox, string) {
	layout := boxLayout{id: "contract", root: root}
	return &containerBox{layout: layout, cli: containerCLI{path: "docker"}, container: "contract"}, layout.home()
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
