package gitproc_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// A POOL KNOWS THE MEMORY ITS LANES WERE SIZED BY: the node's own, read at
// New, when nothing gives it a figure — positive and named, as the engine
// boots — and read again on demand; and a figure the options give is held
// for the pool's life, so a suite's lanes and budgets never follow the
// machine it runs on.
func TestAPoolKnowsTheMemoryItsLanesWereSizedBy(t *testing.T) {
	t.Parallel()
	node := fakePool(t, "exit 0", gitproc.Options{})
	if m := node.Memory(); m.Bytes <= 0 || m.From == "" || m.From == "gitproc.Options.Memory" {
		t.Errorf("a pool given no memory sized its lanes by %+v, want the node's own, named", m)
	}
	again, err := node.ReadMemory()
	if err != nil || again.Bytes <= 0 || again.From == "" {
		t.Errorf("reading the node's memory again = %+v, %v; want the node's own", again, err)
	}

	given := fakePool(t, "exit 0", gitproc.Options{Memory: 3 << 30})
	want := gitproc.Memory{Bytes: 3 << 30, From: "gitproc.Options.Memory"}
	if m := given.Memory(); m != want {
		t.Errorf("a pool given 3 GiB sized its lanes by %+v, want %+v", m, want)
	}
	if m, err := given.ReadMemory(); err != nil || m != want {
		t.Errorf("a pool given 3 GiB read its memory again as %+v, %v; want the figure it was given", m, err)
	}
}

// A MEMORY THAT CANNOT BE READ REFUSES THE POOL, naming what could not be
// read and why it matters — never a lane guessed from nothing, which is the
// OOM the bound exists to prevent.
func TestAMemoryThatCannotBeReadRefusesThePool(t *testing.T) {
	t.Parallel()
	_, err := gitproc.ResolveMemory(gitproc.Options{}, func() (gitproc.Memory, error) {
		return gitproc.Memory{}, errors.New("reading /proc/meminfo: no such file")
	})
	if err == nil || !strings.Contains(err.Error(), "/proc/meminfo") || !strings.Contains(err.Error(), "transfer lane") {
		t.Fatalf("a node whose memory cannot be read = %v, want a refusal naming what and the lane it sizes", err)
	}
	m, err := gitproc.ResolveMemory(gitproc.Options{Memory: 1 << 30}, func() (gitproc.Memory, error) {
		return gitproc.Memory{}, errors.New("never asked")
	})
	if err != nil || m.Bytes != 1<<30 {
		t.Errorf("a figure the options give = %+v, %v; want it, the node never read", m, err)
	}
}
