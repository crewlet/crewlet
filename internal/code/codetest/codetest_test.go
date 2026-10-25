package codetest_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/statelog"
)

// TestMain is the hook when real git starts this binary as proc-receive: the
// suite's own checks push through the production relay too.
func TestMain(m *testing.M) {
	if status, ok := codetest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	os.Exit(m.Run())
}

// twoMembers is a cluster of two members, and nothing else: RunFleet must
// refuse it before it asks anything more of it.
type twoMembers struct{ codetest.Cluster }

func (twoMembers) Members() []string { return []string{"m0", "m1"} }

// inRefusedRun marks the run of this binary that RunFleet is expected to
// fail: the parent case's child.
const inRefusedRun = "CODETEST_RUNFLEET_OVER_TWO_MEMBERS"

// A FLEET SUITE HANDED TWO MEMBERS REFUSES TO RUN, saying why: two members have
// no quorum without each other, so the first case that kills one would leave a
// code log nobody can write, and every case after it would fail for that one.
// RunFleet refuses through t.Fatal, which ends the test that calls it — so
// the call is made in a child run of this binary, whose failure and words are
// what the case reads.
func TestRunFleetRefusesTwoMembers(t *testing.T) {
	if os.Getenv(inRefusedRun) != "" {
		codetest.RunFleet(t, func(*testing.T) codetest.Cluster { return twoMembers{} })
		return
	}
	//nolint:gosec // os.Args[0] is this test binary, run again on the one case.
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRunFleetRefusesTwoMembers$",
		"-test.v")
	child.Env = append(os.Environ(), inRefusedRun+"=1")
	out, err := child.CombinedOutput()
	if err == nil {
		t.Fatalf("RunFleet over a cluster of two members passed:\n%s", out)
	}
	if !strings.Contains(string(out), "two members have no quorum without each other") {
		t.Errorf("RunFleet over a cluster of two members failed without saying why:\n%s", out)
	}
	if strings.Contains(string(out), "=== RUN   TestRunFleetRefusesTwoMembers/") {
		t.Errorf("RunFleet ran a case on a cluster of two members:\n%s", out)
	}
}

// okOnPending is a host's estate that LIES: a proposal the committer answered
// pending — durable, applied nowhere — is reported applied, every command ok,
// as a host that answered git from the decision rather than from the row.
type okOnPending struct{ host.Estate }

func (e okOnPending) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	res, err := e.Estate.Propose(ctx, p)
	if err != nil || res.Outcome != statelog.OutcomePending {
		return res, err
	}
	cmds := make([]code.Command, len(p.Commands))
	for i, c := range p.Commands {
		c.Result = code.ResultOK
		cmds[i] = c
	}
	return code.ProposeResult{Outcome: statelog.OutcomeApplied, Position: res.Position, OpID: res.OpID,
		Answer: code.PushAnswer{PushID: p.PushID, Commands: cmds}}, nil
}

// A suite whose check passes a liar certifies nothing: OkOnlyFromARow must
// catch a host that tells git ok from a decision, and pass an honest one.
func TestTheSuiteCatchesAnOkOnPending(t *testing.T) {
	t.Run("honest", func(t *testing.T) {
		if err := codetest.OkOnlyFromARow(t, codetest.Solo(codetest.SoloOptions{}, nil)); err != nil {
			t.Errorf("OkOnlyFromARow over an honest fleet: %v", err)
		}
	})
	t.Run("liar", func(t *testing.T) {
		liar := codetest.SoloOptions{Shape: codetest.Shape{
			WrapEstate: func(e host.Estate) host.Estate { return okOnPending{e} }}}
		err := codetest.OkOnlyFromARow(t, codetest.Solo(liar, nil))
		if err == nil || !strings.Contains(err.Error(), "an ok answered from a decision") {
			t.Errorf("OkOnlyFromARow over a host that reports ok on pending = %v, want the decision named", err)
		}
	})
}

// intruder is a host's estate that WRITES INTO A CACHE: after every applied
// push it leaves a loose ref in every repository directory of every cache —
// something a sync never writes.
type intruder struct {
	host.Estate
	mu   *sync.Mutex
	dirs *[]string
}

func (e intruder) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	res, err := e.Estate.Propose(ctx, p)
	if err != nil || res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) == 0 {
		return res, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, dir := range *e.dirs {
		repos, _ := filepath.Glob(filepath.Join(dir, "repos", "*.git"))
		for _, repo := range repos {
			ref := filepath.Join(repo, "refs", "heads", "intruder")
			_ = os.WriteFile(ref, []byte(res.Answer.Commands[0].New+"\n"), 0o644)
		}
	}
	return res, err
}

// A suite whose check passes a liar certifies nothing:
// NothingButSyncWritesACache must catch a cache something other than its
// sync wrote into, and pass one only the sync wrote.
func TestTheSuiteCatchesAWriteIntoACache(t *testing.T) {
	t.Run("honest", func(t *testing.T) {
		if err := codetest.NothingButSyncWritesACache(t, codetest.Solo(codetest.SoloOptions{}, nil)); err != nil {
			t.Errorf("NothingButSyncWritesACache over an honest fleet: %v", err)
		}
	})
	t.Run("liar", func(t *testing.T) {
		var (
			mu   sync.Mutex
			dirs []string
		)
		newFleet := func(t *testing.T, s codetest.Shape) codetest.Fleet {
			s.WrapEstate = func(e host.Estate) host.Estate { return intruder{Estate: e, mu: &mu, dirs: &dirs} }
			f := codetest.SoloNode(t, codetest.SoloOptions{Shape: s})
			mu.Lock()
			for _, n := range f.Nodes() {
				dirs = append(dirs, n.CacheDir())
			}
			mu.Unlock()
			return f
		}
		err := codetest.NothingButSyncWritesACache(t, newFleet)
		if err == nil || !strings.Contains(err.Error(), "loose refs [heads/intruder]") {
			t.Errorf("NothingButSyncWritesACache over a cache with a loose ref written into it = %v, "+
				"want the loose ref named", err)
		}
	})
}
