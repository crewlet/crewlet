package host_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE PACK BOUND AT THE RECEIVER: a push carrying a pack into a repository its
// own sync showed at code.MaxPacksPerRepo live packs is refused
// compaction_behind before its bundle is uploaded — but only once the
// repository's row, read AGAIN, still says so. Each case pushes into a
// repository that holds two pushes' packs while every answer about its row
// says it holds code.MaxPacksPerRepo − 2 more ([boundCase]), so a third push
// meets the bound with no two thousand packs to store; the committer's decide
// reads the rows themselves, and is the authority.

// extraPacks is how many live packs more than its rows hold a bound case's
// answers say its repository holds: two real pushes put the next at the bound.
const extraPacks = code.MaxPacksPerRepo - 2

// boundCase is a case whose repository holds the packs of two pushes, every
// snapshot its git node's cache syncs by and every resolve the host asks
// counting extraPacks more; the person who pushed them, their clone and a
// third commit made in it and not yet pushed.
type boundCase struct {
	*codetest.Case
	fleet *codetest.SoloFleet
	e     *recording
	p     *codetest.Person
	work  string
	// head is the second push's commit: every object the two pushes stored
	// is reachable from it.
	head string

	mu sync.Mutex
	// pins are the positions of the snapshots answered through the barrier,
	// in order: a push's own sync is the last one before its scratch.
	pins []statelog.Position
	// asked are the resolves asked at session — a landing's read of the row
	// again — and reread answers them, when set, in place of the rows.
	asked  []code.ResolveQuery
	reread func(ctx context.Context, inner host.Estate, q code.ResolveQuery) (code.RepoRef, error)
}

// newBoundCase is a bound case: two pushes landed, each read again nowhere —
// below the bound, a landing reads nothing more — and a third commit made.
func newBoundCase(t *testing.T) *boundCase {
	t.Helper()
	b := &boundCase{e: &recording{}}
	b.e.state = func(ctx context.Context, inner host.Estate, q code.StateQuery) (code.RepoSnapshot, error) {
		snap, err := inner.RepoState(ctx, q)
		if err != nil {
			return snap, err
		}
		snap.Repo.LivePacks += extraPacks
		if q.Fresh.Level == statelog.ReadLinearizable {
			b.mu.Lock()
			b.pins = append(b.pins, snap.Position)
			b.mu.Unlock()
		}
		return snap, nil
	}
	b.e.resolve = func(ctx context.Context, inner host.Estate, q code.ResolveQuery) (code.RepoRef, error) {
		if q.Fresh.Level == statelog.ReadSession {
			b.mu.Lock()
			b.asked = append(b.asked, q)
			reread := b.reread
			b.mu.Unlock()
			if reread != nil {
				return reread(ctx, inner, q)
			}
		}
		ref, err := inner.Resolve(ctx, q)
		if err != nil {
			return ref, err
		}
		ref.LivePacks += extraPacks
		return ref, nil
	}
	b.fleet = codetest.SoloNode(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: b.e.wrap}})
	b.fleet.Declare(t, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	b.Case = &codetest.Case{T: t, Fleet: b.fleet, Ctx: t.Context()}
	b.p = codetest.NewPerson(t, b.fleet, "ceo")
	b.work = b.p.Init()
	b.p.Commit(b.work, map[string]string{"README.md": "one\n"}, "one")
	b.MustPush(b.p, b.work, "main")
	b.head = b.p.Commit(b.work, map[string]string{"two.txt": "two\n"}, "two")
	b.MustPush(b.p, b.work, "main")
	if asked := b.reads(); len(asked) > 0 {
		t.Fatalf("two pushes below the bound read the row again %d times (%+v): a landing below the bound "+
			"reads nothing more", len(asked), asked)
	}
	if packs := b.Rows().Repo.LivePacks; packs != 2 {
		t.Fatalf("the rows hold %d live packs after two pushes, want 2", packs)
	}
	b.p.Commit(b.work, map[string]string{"three.txt": "three\n"}, "three")
	return b
}

// reads are the resolves asked at session so far.
func (b *boundCase) reads() []code.ResolveQuery {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.asked)
}

// answerReadsWith has every later resolve at session answered by reread.
func (b *boundCase) answerReadsWith(reread func(context.Context, host.Estate, code.ResolveQuery) (code.RepoRef,
	error)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reread = reread
}

// push pushes the third commit to main through the first git node, holding the
// push once its scratch exists — past its own sync, which pinned what the
// landing judges — to run during (nil for nothing), and answers what git
// printed and the position the push's sync pinned.
func (b *boundCase) push(t *testing.T, during func()) (codetest.Pushed, statelog.Position) {
	t.Helper()
	held, reached := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	b.Fleet.Faults().Arm(codefault.HostAfterScratch, codefault.Action{Once: true, Block: held, Reached: reached})
	done := make(chan codetest.Pushed, 1)
	go func() { done <- b.p.Push(b.Ctx, b.work, b.URL(0), "main") }()
	reachedFirst(t, reached, done)
	b.mu.Lock()
	var pin statelog.Position
	if len(b.pins) > 0 {
		pin = b.pins[len(b.pins)-1]
	}
	b.mu.Unlock()
	if pin.IsZero() {
		t.Fatal("the push reached its scratch with no snapshot answered through the barrier")
	}
	if during != nil {
		during()
	}
	release()
	return <-done, pin
}

// readOnce holds the landing to ONE read of the row again: of its repository,
// at session, floored at pin — the position its push's own sync pinned —
// never through the barrier, and never at another floor.
func (b *boundCase) readOnce(t *testing.T, pin statelog.Position) {
	t.Helper()
	asked := b.reads()
	if len(asked) != 1 || asked[0].Name != codetest.RepoName || asked[0].Fresh.MinPosition != pin {
		t.Fatalf("the landing read the row again %d times (%+v), want once, of %s at session floored at the "+
			"push's own sync (%s)", len(asked), asked, codetest.RepoName, pin)
	}
}

// compact merges the repository's two packs into one as a compaction does: a
// pack of every object the two pushes stored, written by the person's git,
// laid out and stored as one bundle and proposed to the data node's
// committer, which applies it — so the rows hold one live pack fewer. It
// answers the position the compaction applied at.
func (b *boundCase) compact(t *testing.T) statelog.Position {
	t.Helper()
	rows := b.Rows()
	if len(rows.Packs) != 2 {
		t.Fatalf("the rows name %d packs, want the two pushes'", len(rows.Packs))
	}
	retire := []string{rows.Packs[0].PackID, rows.Packs[1].PackID}
	slices.Sort(retire)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "objects", "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	checksum := gittest.Pack(t, b.p.Git(), dir, b.work, b.head)
	packPath := filepath.Join(dir, "objects", "pack", "pack-"+checksum+".pack")
	id := statelog.NewOpID(time.Now(), "")
	out, err := code.LayBundle(code.DeriveCompactionPackID(id, 0), packPath, checksum)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := objstore.NewStore(b.Fleet.Objects())
	if err != nil {
		t.Fatal(err)
	}
	if err := code.StoreBundle(b.Ctx, objects, packPath, out, nil); err != nil {
		t.Fatalf("store the compaction's output: %v", err)
	}
	res, err := b.fleet.Data().Served().ProposeCompaction(b.Ctx, code.CompactionProposal{
		Compaction: code.Compaction{RepoID: rows.Repo.ID, CompactionID: id, Node: b.Fleet.Nodes()[0].ID(),
			Seen: rows.Position, Retire: retire, Add: []code.PackRef{*out}}})
	if err != nil || res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the compaction of the two packs answered %+v, %v; want applied", res, err)
	}
	return res.Position
}

// THE PRECHECK READS AGAIN BEFORE IT REFUSES. A push whose own sync showed its
// repository at code.MaxPacksPerRepo is refused compaction_behind by its
// receiver only once ONE read of the repository's row — at session, floored at
// that sync's position, through the estate, with no barrier and only on this
// path — still finds it at the bound, so a retry that would only be refused
// uploads nothing. A compaction applied since the sync lowers the row, and
// the push lands rather than being refused by a figure that is no longer so;
// a read that fails, answers another repository (a name claimed since the
// sync) or finds the repository no longer active establishes nothing, and
// the push goes on to its committer, whose decide is the authority.
//
// Mutation: refuse on the sync's own figure and the first case is refused;
// read again through the cache's Sync at session floored at the sync — which
// the cache answers at once from the very copy the push pinned (atOnce) — and
// the first case is refused too; drop the precheck and the second case
// uploads and lands; refuse on a read that failed, on an answer about another
// repository or on one no longer active, and that case is refused; read again
// through the barrier, or at any floor but the sync's, and every case fails
// its count of reads; read again on every push and newBoundCase fails on the
// two pushes below the bound.
func TestThePrecheckReadsAgainBeforeItRefuses(t *testing.T) {
	t.Run("a compaction applied since the sync lets the push land", func(t *testing.T) {
		b := newBoundCase(t)
		var compacted statelog.Position
		res, pin := b.push(t, func() { compacted = b.compact(t) })
		if !res.OK("refs/heads/main") {
			t.Fatalf("a push past a compaction applied since its sync: %s", res)
		}
		b.readOnce(t, pin)
		if before, err := pin.Before(compacted); err != nil || !before {
			t.Fatalf("the push's sync pinned %s, not before the compaction applied at %s (%v)", pin, compacted, err)
		}
		if packs := b.Rows().Repo.LivePacks; packs != 2 {
			t.Errorf("the rows hold %d live packs, want the compaction's output and the push's pack", packs)
		}
	})

	t.Run("a repository still at the bound refuses before the upload", func(t *testing.T) {
		b := newBoundCase(t)
		stored, proposed, pushes := len(objects(t, b.Case)), len(b.e.seen("propose")), len(pushRows(t, b.Case))
		res, pin := b.push(t, nil)
		reason, rejected := res.Rejected("refs/heads/main")
		want := "(compaction_behind: " + code.BehindDetail(codetest.RepoName, code.MaxPacksPerRepo) + ")"
		if !rejected || !strings.Contains(reason, want) {
			t.Fatalf("a push into a repository at the bound printed %q, want %q: %s", reason, want, res)
		}
		b.readOnce(t, pin)
		if got := len(objects(t, b.Case)); got != stored {
			t.Errorf("a push refused at the bound stored %d objects", got-stored)
		}
		if got := len(b.e.seen("propose")); got != proposed {
			t.Errorf("a push refused at the bound was proposed %d times", got-proposed)
		}
		if got := len(pushRows(t, b.Case)); got != pushes {
			t.Errorf("a push refused at the bound left %d push rows", got-pushes)
		}
		audited := b.Fleet.Audited()
		if last := audited[len(audited)-1]; last.Outcome != types.AuditRefused ||
			last.Refusal != string(code.RefusalCompactionBehind) {
			t.Errorf("a push refused at the bound was recorded %s/%s, want refused/compaction_behind",
				last.Outcome, last.Refusal)
		}
	})

	// A READ THAT ESTABLISHES NOTHING — each answering at the bound where it
	// answers at all — leaves the push to its committer, whose rows hold two
	// live packs: it lands.
	for _, tc := range []struct {
		name   string
		reread func(ctx context.Context, inner host.Estate, q code.ResolveQuery) (code.RepoRef, error)
	}{
		{"a read that fails", func(context.Context, host.Estate, code.ResolveQuery) (code.RepoRef, error) {
			return code.RepoRef{}, fmt.Errorf("%w: the case's data nodes answer nothing", code.ErrNotServing)
		}},
		{"an answer about another repository, which the name claims since the sync",
			func(ctx context.Context, inner host.Estate, q code.ResolveQuery) (code.RepoRef, error) {
				ref, err := inner.Resolve(ctx, q)
				ref.ID, ref.LivePacks = uuid.Must(uuid.NewV7()).String(), code.MaxPacksPerRepo
				return ref, err
			}},
		{"an answer that the repository was archived since the sync",
			func(ctx context.Context, inner host.Estate, q code.ResolveQuery) (code.RepoRef, error) {
				ref, err := inner.Resolve(ctx, q)
				ref.Status, ref.LivePacks = code.RepoStatusArchived, code.MaxPacksPerRepo
				return ref, err
			}},
	} {
		t.Run(tc.name+" leaves the push to its committer", func(t *testing.T) {
			b := newBoundCase(t)
			b.answerReadsWith(tc.reread)
			res, pin := b.push(t, nil)
			if !res.OK("refs/heads/main") {
				t.Fatalf("the push was not left to its committer: %s", res)
			}
			b.readOnce(t, pin)
		})
	}
}
