package gitproc_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/nodememory"
)

// A POOL KNOWS THE MEMORY ITS LANES WERE SIZED BY: the node's own, positive
// and named, read at New when nothing gives a figure and again on demand; a
// figure the options give is held for the pool's life, so a suite's lanes and
// budgets never follow the machine it runs on.
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
	want := nodememory.Memory{Bytes: 3 << 30, From: "gitproc.Options.Memory"}
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
	_, err := gitproc.ResolveMemory(gitproc.Options{}, func() (nodememory.Memory, error) {
		return nodememory.Memory{}, errors.New("reading /proc/meminfo: no such file")
	})
	if err == nil || !strings.Contains(err.Error(), "/proc/meminfo") || !strings.Contains(err.Error(), "transfer lane") {
		t.Fatalf("a node whose memory cannot be read = %v, want a refusal naming what and the lane it sizes", err)
	}
	m, err := gitproc.ResolveMemory(gitproc.Options{Memory: 1 << 30}, func() (nodememory.Memory, error) {
		return nodememory.Memory{}, errors.New("never asked")
	})
	if err != nil || m.Bytes != 1<<30 {
		t.Errorf("a figure the options give = %+v, %v; want it, the node never read", m, err)
	}
}

// A NODE KEEPS WHAT RUNS BESIDE GITLET BEFORE IT SIZES ITS TRANSFERS: the
// default lane is the one a node of the memory Options.Reserve leaves would
// run, the reserve is asked of the memory read (the machine's, where the
// options give none) and logged on code_git_ready, and a reserve that takes
// the memory leaves the one transfer a node needs to serve a clone. An
// operator's gitlet.max_transfers is kept as written, and a negative reserve
// refused.
//
// Mutations, each red: size the default lane by the whole memory; ask the
// reserve of the options' memory rather than the memory read; log no reserve.
func TestANodeKeepsItsReserveBeforeSizingItsTransfers(t *testing.T) {
	t.Parallel()
	const pools = int64(1216) << 20
	for _, tc := range []struct {
		name               string
		cores              int
		memory, reserve    int64
		maxTransfers, want int
	}{
		{"two GiB a core, no reserve", 2, 4 << 30, 0, 0, 8},
		{"two GiB a core beside the pools", 2, 4 << 30, pools, 0, 5},
		{"the reserve on top of two GiB a core", 2, 4<<30 + pools, pools, 0, 8},
		{"one core of two GiB beside the pools", 1, 2 << 30, pools, 0, 1},
		{"a reserve past the memory", 2, 1 << 30, pools, 0, 1},
		{"an operator's lane", 2, 2 << 30, pools, 7, 7},
		{"the machine's memory beside the pools", 2, 0, pools, 0, -1},
	} {
		var asked int64
		var logs logBuffer
		o := gitproc.Options{MaxProcesses: tc.cores, MaxTransfers: tc.maxTransfers, Memory: tc.memory,
			Logger: logs.logger()}
		if tc.reserve > 0 {
			o.Reserve = func(memory int64) int64 { asked = memory; return tc.reserve }
		}
		p := fakePool(t, "exit 0", o)
		if got := p.Slots(gitproc.ClassTransfer); tc.want >= 0 && got != tc.want {
			t.Errorf("%s: %d transfers, want %d", tc.name, got, tc.want)
		}
		if p.Reserved() != tc.reserve {
			t.Errorf("%s: the pool kept %d bytes beside Gitlet, want %d", tc.name, p.Reserved(), tc.reserve)
		}
		if read := p.Memory().Bytes; tc.reserve > 0 && (asked != read || read <= 0) {
			t.Errorf("%s: the reserve was asked of %d bytes, want the memory read, %d", tc.name, asked, read)
		}
		if logs.lines("code_git_ready", fmt.Sprintf(" reserved=%d ", tc.reserve)) != 1 {
			t.Errorf("%s: code_git_ready does not log reserved=%d: %s", tc.name, tc.reserve, logs.String())
		}
	}
	o := gitproc.Options{Path: gittest.Fake(t, fakeVersion, "exit 0"), Home: filepath.Join(t.TempDir(), "home"),
		Memory: 2 << 30, Reserve: func(int64) int64 { return -1 }}
	if _, err := gitproc.New(t.Context(), o); err == nil || !strings.Contains(err.Error(), "Options.Reserve") {
		t.Errorf("New with a negative reserve = %v, want a refusal naming Options.Reserve", err)
	}
}
