package codetest_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
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

// okOnPendingWrite is okOnPending for an ENGINE WRITE alone: a person's push
// is answered honestly, a write's pending proposal reported applied.
type okOnPendingWrite struct{ host.Estate }

func (e okOnPendingWrite) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	if p.Via != code.ViaEngine {
		return e.Estate.Propose(ctx, p)
	}
	return okOnPending(e).Propose(ctx, p)
}

// A suite whose check passes a liar certifies nothing: OkOnlyFromARow must
// catch a host that answers an engine write applied from a decision, its
// pushes answered honestly — the half of the check that holds a write.
func TestTheSuiteCatchesAWriteAnsweredOkWithoutARow(t *testing.T) {
	liar := codetest.SoloOptions{Shape: codetest.Shape{
		WrapEstate: func(e host.Estate) host.Estate { return okOnPendingWrite{e} }}}
	err := codetest.OkOnlyFromARow(t, codetest.Solo(liar, nil))
	if err == nil || !strings.Contains(err.Error(), "an applied write answered from a decision") {
		t.Errorf("OkOnlyFromARow over a host that answers a pending write applied = %v, want the decision "+
			"named", err)
	}
}

// A suite whose check passes a liar certifies nothing:
// NothingButSyncWritesACache must catch an engine write whose build writes
// into the cache it leans on — here a git that, asked to hash a write's files
// in its scratch, writes an object into the scratch's alternate too — the
// mutation of running a build's writer in the copy rather than beside it.
func TestTheSuiteCatchesAnEngineWriteIntoACache(t *testing.T) {
	git := gittest.Wrap(t, `if [ "${1-}" = hash-object ] && [ -f "${GIT_DIR-}/objects/info/alternates" ]; then
	cache=$(sed -n 1p "$GIT_DIR/objects/info/alternates")
	printf 'intruder\n' | GIT_DIR="${cache%/objects}" "$real_git" hash-object -w --stdin >/dev/null
fi`)
	err := codetest.NothingButSyncWritesACache(t, codetest.Solo(codetest.SoloOptions{GitPath: git}, nil))
	if err == nil || !strings.Contains(err.Error(), "loose objects") {
		t.Errorf("NothingButSyncWritesACache over a build that writes into the cache = %v, want the loose "+
			"object named", err)
	}
}

// graceless is a data node's applier that LIES about a retirement: every
// record it applies leaves no row in code_packs_retired, as an applier that
// moved a compaction's inputs out of code_packs without keeping them for their
// grace would — so nothing shields a retired pack's object from the collector
// once its upload's own grace has passed.
type graceless struct{ statelog.Applier }

func (a graceless) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {
	rows, err := a.Applier.Apply(ctx, tx, rec, opts)
	if err != nil {
		return rows, err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM code_packs_retired")
	if err != nil {
		return rows, err
	}
	gone, err := res.RowsAffected()
	return rows + int(gone), err
}

// A suite whose check passes a liar certifies nothing:
// ARetiredPackOutlivesItsLongestRead (C4) must catch a topology whose applier
// keeps no retired row — the first collection past every input's upload grace
// deletes the objects a reader may still be reading — and pass an honest one.
func TestTheSuiteCatchesARetirementWithNoGrace(t *testing.T) {
	t.Run("honest", func(t *testing.T) {
		if err := codetest.ARetiredPackOutlivesItsLongestRead(t, codetest.Solo(codetest.SoloOptions{}, nil)); err != nil {
			t.Errorf("ARetiredPackOutlivesItsLongestRead over an honest fleet: %v", err)
		}
	})
	t.Run("liar", func(t *testing.T) {
		liar := codetest.SoloOptions{WrapApplier: func(a statelog.Applier) statelog.Applier { return graceless{a} }}
		err := codetest.ARetiredPackOutlivesItsLongestRead(t, codetest.Solo(liar, nil))
		if err == nil || !strings.Contains(err.Error(), "retired objects inside their grace") {
			t.Errorf("ARetiredPackOutlivesItsLongestRead over an applier that keeps no retired row = %v, want the "+
				"objects deleted inside their grace named", err)
		}
	})
}

// droppingDeleted is a data node's applier that LIES about an expunge's
// commit: the dropping packs it retires leave code_packs_retired at once, as
// a commit that deleted them rather than retiring them would — so nothing
// shields their objects from the collector once their uploads' own grace has
// passed, while a reader that opened one before the commit may still be on
// it.
type droppingDeleted struct{ statelog.Applier }

func (a droppingDeleted) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {
	rows, err := a.Applier.Apply(ctx, tx, rec, opts)
	if err != nil {
		return rows, err
	}
	m, err := code.Decode(rec.Payload)
	if err != nil || m.Op != code.OpExpungeCommit {
		return rows, err
	}
	var commit code.ExpungeCommit
	if err := json.Unmarshal(m.Mutation, &commit); err != nil {
		return rows, err
	}
	for _, id := range commit.Retire {
		res, err := tx.ExecContext(ctx, "DELETE FROM code_packs_retired WHERE repo_id = ? AND pack_id = ?",
			commit.RepoID, id)
		if err != nil {
			return rows, err
		}
		gone, err := res.RowsAffected()
		if err != nil {
			return rows, err
		}
		rows += int(gone)
	}
	return rows, nil
}

// A suite whose check passes a liar certifies nothing:
// ExpungeRetiredPackOutlivesItsLongestRead (F50) must catch a topology whose
// expunge commit deletes the dropping packs' rows rather than retiring them —
// the first collection past their uploads' grace deletes what a reader may
// still be reading — and pass an honest one.
func TestTheSuiteCatchesAnExpungeThatDeletesWhatItDrops(t *testing.T) {
	t.Run("honest", func(t *testing.T) {
		if err := codetest.ExpungeRetiredPackOutlivesItsLongestRead(t, codetest.Solo(codetest.SoloOptions{}, nil)); err != nil {
			t.Errorf("ExpungeRetiredPackOutlivesItsLongestRead over an honest fleet: %v", err)
		}
	})
	t.Run("liar", func(t *testing.T) {
		liar := codetest.SoloOptions{WrapApplier: func(a statelog.Applier) statelog.Applier {
			return droppingDeleted{a}
		}}
		err := codetest.ExpungeRetiredPackOutlivesItsLongestRead(t, codetest.Solo(liar, nil))
		if err == nil || !strings.Contains(err.Error(), "retired objects inside their grace") {
			t.Errorf("ExpungeRetiredPackOutlivesItsLongestRead over a commit that deletes its dropping packs' rows "+
				"= %v, want the objects deleted inside their grace named", err)
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
