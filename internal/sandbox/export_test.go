package sandbox

import (
	"fmt"
	"os"
	"testing"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// NewContainerBoxAt is a container box rooted at root, for the file contract
// suite: a container box reads and writes on the HOST side of its bind mount,
// so its contract is certified without a runtime, and nothing it reaches runs
// a command. It returns the box and its home (what the mount carries, not root,
// which also holds the engine's records), made as Create makes it, since every
// access to the box goes through it.
func NewContainerBoxAt(root string) (Sandbox, string) {
	layout := boxLayout{id: "contract", root: root}
	if err := os.MkdirAll(layout.home(), hostbox.DirMode); err != nil {
		panic(fmt.Sprintf("NewContainerBoxAt: %v", err))
	}
	return &containerBox{layout: layout, cli: containerCLI{path: "docker"}, container: "contract"}, layout.home()
}

// WriteExecutable is [writeExecutable] for the stubs the external suite's
// shell runs (setuprecipe_test.go, clone_test.go): they share this binary and
// its forks, and TestEveryExecutableThisSuiteWritesIsWrittenUnderTheForkLock
// holds every executable write of either suite to the one writer.
func WriteExecutable(t *testing.T, path, script string) {
	t.Helper()
	writeExecutable(t, path, script)
}

// CapReads caps a local box's whole reads at n bytes, so the file contract
// suite certifies the refusal one byte past the cap and the whole read at it
// (the same branch at any cap) without two 32 MiB files per backend.
// [FakeSandbox.CapReads] is exported because suites outside this package need it.
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

// ObserveCredentialWrites hands every login write-back l's boxes make the
// observer steps ([hostbox.CredentialSteps]), for the kill matrix's writer
// (writebackwriter_test.go): set before l makes a box, which takes it with its
// layout.
func ObserveCredentialWrites(l *Local, steps hostbox.CredentialSteps) { l.credentialSteps = steps }

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
