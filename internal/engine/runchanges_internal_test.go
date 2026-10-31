package engine

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// changeHost is the code host as a collection asks it of the changes a run's
// branches head: each repository's live changes by branch, an open that
// lands one — numbered from 7 — unless its branch heads one, and a read by
// object id answering one commit's subject; every question recorded.
type changeHost struct {
	mu      sync.Mutex
	heads   map[pushKey]code.BranchChange
	next    int64
	subject string
	reads   []code.ReadQuery
	writes  []code.ChangeProposal
	gits    []code.GitQuery
}

func newChangeHost() *changeHost {
	return &changeHost{heads: map[pushKey]code.BranchChange{}, next: 7, subject: "Retry the upload once"}
}

func (h *changeHost) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads = append(h.reads, q)
	if q.Kind != code.ReadBranchChanges || q.BranchChanges == nil {
		return code.ReadAnswer{}, fmt.Errorf("%w: a collection read kind %s", code.ErrInvalid, q.Kind)
	}
	out := code.ReadAnswer{Position: firstPageAt, Level: q.Fresh.Level}
	for _, ref := range q.BranchChanges.Refs {
		if bc, ok := h.heads[pushKey{repo: q.BranchChanges.RepoID, ref: ref}]; ok {
			out.BranchChanges = append(out.BranchChanges, bc)
		}
	}
	return out, nil
}

func (h *changeHost) Change(_ context.Context, p code.ChangeProposal) (code.ChangeResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.writes = append(h.writes, p)
	o := p.Open
	key := pushKey{repo: o.RepoID, ref: o.SourceRef}
	if live, ok := h.heads[key]; ok {
		return code.ChangeResult{Refusal: code.ChangeRefusalChangeExists,
			Detail: fmt.Sprintf("%s already heads change %d", o.SourceRef, live.Number)}, nil
	}
	h.heads[key] = code.BranchChange{Ref: o.SourceRef, ChangeID: o.ChangeID, Number: h.next}
	h.next++
	return code.ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: o.ChangeID, Number: h.next - 1,
		Version: 1, Position: firstPageAt}, nil
}

func (h *changeHost) GitRead(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gits = append(h.gits, q)
	return code.GitAnswer{Kind: q.Kind, RepoID: q.RepoID, Log: &code.LogAnswer{
		Commits: []code.LogCommit{{OID: q.Log.Rev, Subject: h.subject}}}}, nil
}

// asked is what the host was asked, copied under its lock.
func (h *changeHost) asked() ([]code.ReadQuery, []code.ChangeProposal, []code.GitQuery) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reads), slices.Clone(h.writes), slices.Clone(h.gits)
}

// openingRun is pushedRun asking to open a change from web's branch into
// main, charged to a native task, its launch begun at an instant of its own.
func openingRun() sandbox.PendingRun {
	run := pushedRun()
	run.Repos[0].Open = &sandbox.OpenChange{Target: "refs/heads/main"}
	run.WorkItem = &types.WorkItem{Backend: types.WorkNative, ID: "0190d8f0-5555-7000-8000-0123456789ab",
		Key: "ENG-42", Project: "ENG"}
	run.Launch.StartedAt = time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	return run
}

// A RUN OPENS ITS CHANGE ONCE, AT ITS COLLECTION (M4-decisions D33): a run that
// asked to open a change from its branch, and pushed it, has the change
// opened by the collection — as the seat it ran for, from that branch into
// the target its launch resolved, titled by the subject of the commit it
// delivered, carrying the work item it was charged to and naming its turn and
// its run, decided at or past the read that found the branch heading none —
// under an operation that is a step of the launch's own, so the same
// collection asked again opens nothing twice and names the change it opened
// as its own; and the ref it was opened from names it. A collection not asked
// to open opens nothing, and a run that pushed nothing to its branch is told
// so.
//
// Mutation: mint the open's operation at the collection (a fresh id each
// time) — the second collection's open is another operation, and the change
// the first opened is named as not this run's, red.
func TestARunOpensItsChangeOnceAtCollect(t *testing.T) {
	t.Parallel()
	run := openingRun()
	branch := "refs/heads/seats/swe/ENG-42"
	pushed := []sandbox.PushedRef{{Repo: "web", Ref: branch, Old: code.ZeroOID, New: oidOf("1"), Pushes: 2}}
	host := newChangeHost()

	named, opened, err := readerOf(host).Changes(t.Context(), run, pushed, true)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	want := []sandbox.OpenedChange{{Repo: "web", Branch: "seats/swe/ENG-42", Change: "web!7", Opened: true}}
	if !reflect.DeepEqual(opened, want) {
		t.Fatalf("what became of the run's ask: %+v, want %+v", opened, want)
	}
	if len(named) != 1 || named[0].Change != "web!7" {
		t.Fatalf("the ref the change was opened from is %+v, want it naming web!7", named)
	}
	reads, writes, gits := host.asked()
	if len(reads) != 1 || reads[0].Fresh.Level != statelog.ReadLinearizable {
		t.Fatalf("the collection read %+v, want one linearizable read of web's branches", reads)
	}
	if len(gits) != 1 || gits[0].Log == nil || gits[0].Log.Rev != oidOf("1") || gits[0].Asker.Seat != "swe" {
		t.Fatalf("the collection titled the change by %+v, want one read of the delivered commit as swe", gits)
	}
	op := openOpID(run, runWebID)
	wantOpen := code.ChangeProposal{OpID: op, Asker: code.Principal{Kind: code.PrincipalAgent, Seat: "swe"},
		Seen: firstPageAt, Open: &code.ChangeOpen{RepoID: runWebID, ChangeID: code.OpenChangeID(op),
			SourceRef: branch, TargetRef: "refs/heads/main", Title: "Retry the upload once",
			WorkItem: run.WorkItem, TurnID: "t1", RunID: runLaunch}}
	if len(writes) != 1 || !reflect.DeepEqual(writes[0], wantOpen) {
		t.Fatalf("the collection asked %+v, want ONE open %+v", writes, wantOpen)
	}
	if err := statelog.CheckCallerOpID(op); err != nil {
		t.Fatalf("the open's operation %q is no operation id the engine accepts: %v", op, err)
	}

	// THE SAME COLLECTION ASKED AGAIN, on this node or another, opens
	// nothing twice: the branch heads the change this launch opened.
	_, again, err := readerOf(host).Changes(t.Context(), run, pushed, true)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("the collection asked again: %+v, %v; want %+v", again, err, want)
	}
	if _, writes, _ := host.asked(); len(writes) != 1 {
		t.Fatalf("the collection asked again wrote %d opens, want the first alone", len(writes))
	}

	// ANOTHER LAUNCH of the turn's run is another operation: it names the
	// change the first opened as one its branch already headed.
	relaunched := run
	relaunched.LaunchID = "0190d8f0-0000-7000-8000-0000000000f2"
	if _, other, err := readerOf(host).Changes(t.Context(), relaunched, pushed, true); err != nil ||
		len(other) != 1 || other[0].Opened || other[0].Change != "web!7" {
		t.Fatalf("a relaunch's collection: %+v, %v; want web!7 named as the branch's, not opened", other, err)
	}

	// NOT ASKED TO OPEN — a run that did not finish — it opens nothing.
	quiet := newChangeHost()
	if _, none, err := readerOf(quiet).Changes(t.Context(), run, pushed, false); err != nil || none != nil {
		t.Fatalf("a collection not asked to open answered %+v, %v", none, err)
	}
	if _, writes, _ := quiet.asked(); len(writes) != 0 {
		t.Fatalf("a collection not asked to open wrote %+v", writes)
	}

	// A RUN THAT PUSHED NOTHING TO ITS BRANCH proposes nothing.
	idle := newChangeHost()
	_, unopened, err := readerOf(idle).Changes(t.Context(), run, nil, true)
	if err != nil || len(unopened) != 1 || unopened[0].Change != "" ||
		!strings.Contains(unopened[0].Unopened, "pushed nothing") {
		t.Fatalf("a run that pushed nothing: %+v, %v; want it told no change was opened", unopened, err)
	}
	if _, writes, _ := idle.asked(); len(writes) != 0 {
		t.Fatalf("a run that pushed nothing asked to open %+v", writes)
	}
}

// A DELIVERED REF NAMES ITS CHANGE (M4-decisions D33): every branch a collected
// run pushed is named by the live change it heads — the run's own, or one
// anybody opened — read once a repository, the first read linearizable and
// every later one at session at its position; a tag, a deleted branch and a
// branch heading no change name none, and a repository the run was given to
// read is never asked.
//
// Mutation: read every repository linearizable — the second read's level,
// red; name a deleted branch by the change its name heads — red.
func TestADeliveredRefNamesItsChange(t *testing.T) {
	t.Parallel()
	host := newChangeHost()
	host.heads[pushKey{repo: runWebID, ref: "refs/heads/seats/swe/ENG-42"}] = code.BranchChange{
		Ref: "refs/heads/seats/swe/ENG-42", ChangeID: "0190d8f0-6666-7000-8000-0123456789ab", Number: 42}
	host.heads[pushKey{repo: runAPIID, ref: "refs/heads/fix"}] = code.BranchChange{Ref: "refs/heads/fix",
		ChangeID: "0190d8f0-7777-7000-8000-0123456789ab", Number: 5}
	host.heads[pushKey{repo: runAPIID, ref: "refs/heads/old"}] = code.BranchChange{Ref: "refs/heads/old",
		ChangeID: "0190d8f0-8888-7000-8000-0123456789ab", Number: 6}
	pushed := []sandbox.PushedRef{
		{Repo: "web", Ref: "refs/heads/seats/swe/ENG-42", Old: oidOf("1"), New: oidOf("2"), Pushes: 1},
		{Repo: "web", Ref: "refs/tags/v1", Old: code.ZeroOID, New: oidOf("2"), Pushes: 1},
		{Repo: "api", Ref: "refs/heads/fix", Old: oidOf("3"), New: oidOf("4"), Pushes: 1},
		{Repo: "api", Ref: "refs/heads/old", Old: oidOf("5"), New: code.ZeroOID, Pushes: 1},
		{Repo: "api", Ref: "refs/heads/spike", Old: code.ZeroOID, New: oidOf("6"), Pushes: 1},
	}
	named, opened, err := readerOf(host).Changes(t.Context(), pushedRun(), pushed, true)
	if err != nil || opened != nil {
		t.Fatalf("Changes = %+v, %v; want no ask answered", opened, err)
	}
	want := []string{"web!42", "", "api!5", "", ""}
	for i, ref := range named {
		if ref.Change != want[i] {
			t.Errorf("%s %s names change %q, want %q", ref.Repo, ref.Ref, ref.Change, want[i])
		}
	}
	reads, _, _ := host.asked()
	if len(reads) != 2 || reads[0].BranchChanges.RepoID != runWebID || reads[1].BranchChanges.RepoID != runAPIID ||
		reads[0].Fresh.Level != statelog.ReadLinearizable || reads[1].Fresh.Level != statelog.ReadSession ||
		reads[1].Fresh.Floor(firstPageAt.Stream) != firstPageAt {
		t.Fatalf("the collection read %+v, want web linearizable then api at session at the first's position",
			reads)
	}
	if got := reads[1].BranchChanges.Refs; !slices.Equal(got, []string{"refs/heads/fix", "refs/heads/spike"}) {
		t.Errorf("api's read named %v, want its live branches alone", got)
	}
}
