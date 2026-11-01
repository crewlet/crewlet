package home_test

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CHECK WORK OVER STAND-INS: one repository's changes owing their checks
// work, its targets due a reading of their checks file and its protected
// targets owing tip work, as the `checks_due` listing answers them
// (checkEstate.due, .targets, .tips), the checks file each commit holds and
// what each head touches, as a git node answers them — and every write the
// worker asks of the repository's checks writer and of its committer, in the
// order it asks them.
type checkEstate struct {
	mu      sync.Mutex
	due     []code.ChangeDue
	targets []code.CheckTargetDue
	tips    []code.TipDue
	files   map[string]string
	facts   map[string]code.RevisionFactsAnswer
	read    []string
	writes  []code.ChecksRequest
	records []code.QueueProposal
}

func newCheckEstate() *checkEstate {
	return &checkEstate{files: map[string]string{}, facts: map[string]code.RevisionFactsAnswer{}}
}

func (e *checkEstate) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case q.Kind == code.ReadChangesDue || q.Kind == code.ReadQueueDue:
		return code.ReadAnswer{}, nil
	case q.Kind != code.ReadChecksDue:
		return code.ReadAnswer{}, errors.New("the check work asked a read that is not its listing")
	case q.ChecksDue == nil || q.ChecksDue.RefreshBefore.IsZero():
		return code.ReadAnswer{}, errors.New("the check work's listing named no instant a target's reading " +
			"is due again by")
	case q.ChecksDue.RepoID == "":
		return code.ReadAnswer{DueRepos: []string{repoA}}, nil
	}
	return code.ReadAnswer{DueChanges: slices.Clone(e.due), DueTargets: slices.Clone(e.targets),
		DueTips: slices.Clone(e.tips), Position: statelog.Position{Stream: topics.CodeLogStream, Seq: 50}}, nil
}

func (e *checkEstate) GitRead(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	if err := q.Validate(); err != nil {
		return code.GitAnswer{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch q.Kind {
	case code.GitChecksFile:
		commit := q.ChecksFile.Commit
		e.read = append(e.read, commit)
		a := &code.ChecksFileAnswer{Commit: commit}
		if raw, ok := e.files[commit]; ok {
			a.OID, a.Type, a.Size, a.Raw = blobOf(raw), "blob", int64(len(raw)), []byte(raw)
		}
		return code.GitAnswer{Kind: code.GitChecksFile, RepoID: q.RepoID, ChecksFile: a}, nil
	case code.GitRevisionFacts:
		f, ok := e.facts[q.RevisionFacts.Head]
		if !ok {
			return code.GitAnswer{}, code.ErrObjectNotFound
		}
		return code.GitAnswer{Kind: code.GitRevisionFacts, RepoID: q.RepoID, RevisionFacts: &f}, nil
	}
	return code.GitAnswer{}, errors.New("the check work asked a git read that is not its own")
}

func (e *checkEstate) Checks(_ context.Context, r code.ChecksRequest) (code.ChecksResult, error) {
	if err := r.Validate(); err != nil {
		return code.ChecksResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.writes = append(e.writes, r)
	return code.ChecksResult{Outcome: statelog.OutcomeApplied, Position: statelog.Position{
		Stream: topics.CodeLogStream, Seq: uint64(60 + len(e.writes))}}, nil
}

// Queue records a green or a broken the tip work asks of the repository's
// committer, in order.
func (e *checkEstate) Queue(_ context.Context, p code.QueueProposal) (code.QueueResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, p)
	return code.QueueResult{Outcome: statelog.OutcomeApplied, Position: statelog.Position{
		Stream: topics.CodeLogStream, Seq: uint64(90 + len(e.records))}}, nil
}

func (e *checkEstate) Converse(context.Context, code.ConversationWrite) (code.ConversationResult, error) {
	return code.ConversationResult{}, errors.New("the check work asked a write of a change's conversation")
}

func (e *checkEstate) Change(context.Context, code.ChangeProposal) (code.ChangeResult, error) {
	return code.ChangeResult{}, errors.New("the check work asked a write of a change's lifecycle")
}

// asked is every write the worker asked, and every commit whose checks file
// it read, in order.
func (e *checkEstate) asked() ([]code.ChecksRequest, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.writes), slices.Clone(e.read)
}

// blobOf is the id a blob of raw would have — any whole id, one a content.
func blobOf(raw string) string {
	sum := sha1.Sum([]byte("blob " + raw))
	return hex.EncodeToString(sum[:])
}

// checkLoop is a loop over e, never started: a case runs its check work's
// turns itself (home.CheckWork).
func checkLoop(t *testing.T, e *checkEstate) *home.Loop {
	t.Helper()
	l, err := home.New(home.Options{Estate: e, Placement: asNode{placement: &placement{}, node: "git-a"},
		Nudges: newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t},
		Tasks: unqueued{t: t}, NodeID: "git-a"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

// owing is change n of repoA, its head head and its target main at tip,
// owing w.
func owing(n int64, head, tip string, w code.CheckWork) code.ChangeDue {
	return code.ChangeDue{ID: changeID(repoA, n), Number: n, Revision: 1, Head: head,
		SourceRef: "refs/heads/feature-" + string(rune('a'+n)), TargetRef: "refs/heads/main", Tip: tip,
		TipPosition: statelog.Position{Stream: topics.CodeLogStream, Seq: 30}, Checks: &w}
}

// checksFile is a checks file defining each of defs, a line of YAML each
// beside the name: `run: make <name>` and a timeout, then what the entry
// adds.
func checksFile(defs ...string) string {
	var b strings.Builder
	b.WriteString("checks:\n")
	for _, d := range defs {
		name, extra, _ := strings.Cut(d, " ")
		b.WriteString("  - name: " + name + "\n    run: make " + name + "\n    timeout_seconds: 60\n")
		if extra != "" {
			b.WriteString("    " + extra + "\n")
		}
	}
	return b.String()
}

// queuedOf is every attempt writes queue, by name, and dispatchesOf the
// dispatches they record, in order.
func queuedOf(writes []code.ChecksRequest) map[string]code.CheckQueued {
	out := map[string]code.CheckQueued{}
	for _, w := range writes {
		if w.Queued != nil {
			out[w.Queued.Name] = *w.Queued
		}
	}
	return out
}

func dispatchesOf(writes []code.ChecksRequest) []code.CheckDispatched {
	var out []code.CheckDispatched
	for _, w := range writes {
		if w.Dispatched != nil {
			out = append(out, *w.Dispatched)
		}
	}
	return out
}

// headsOf is the commits d names dispatched.
func headsOf(d code.CheckDispatched) []string {
	out := make([]string, 0, len(d.Heads))
	for _, h := range d.Heads {
		out = append(out, h.OID)
	}
	return out
}

// CHECK DEFINITIONS COME FROM THE TARGET: a change whose head rewrites the
// checks file — a check of its own added, the target's check given another
// command — is dispatched by the file its target's tip holds, every check the
// target defines queued under the target's definition and DefsOID, the head's
// own file never read; and the dispatch is recorded at that tip, naming the
// head under the target's file, after everything it queued.
//
// Mutation: read the checks file at the change's head — the head's rogue
// check is queued and the target's command replaced, red.
func TestCheckDefinitionsComeFromTheTarget(t *testing.T) {
	t.Parallel()
	head, tip := oid("1"), oid("2")
	e := newCheckEstate()
	e.files[tip] = checksFile("unit")
	e.files[head] = strings.Replace(checksFile("unit", "rogue"), "make unit", "true", 1)
	e.due = []code.ChangeDue{owing(1, head, tip, code.CheckWork{Dispatch: true})}
	l := checkLoop(t, e)
	if err := home.CheckWork(t.Context(), l, repoA, statelog.Position{}); err != nil {
		t.Fatalf("the check work: %v", err)
	}
	writes, read := e.asked()
	queued := queuedOf(writes)
	unit, ok := queued["unit"]
	switch {
	case !slices.Equal(read, []string{tip}):
		t.Fatalf("the check work read the checks files of %v, want the target's tip's alone", read)
	case len(queued) != 1 || !ok:
		t.Fatalf("the check work queued %v, want the target's unit alone", queued)
	case unit.Def == nil || unit.Def.Run != "make unit" || unit.DefsOID != blobOf(e.files[tip]) ||
		unit.Purpose != code.PurposeHead || unit.Attempt != 1 || unit.OID != head || unit.Skipped ||
		unit.Target != "refs/heads/main":
		t.Fatalf("unit is queued as %+v (its definition %+v), want attempt 1 of the head for main under the "+
			"target's definition and file", unit, unit.Def)
	}
	d := dispatchesOf(writes)
	if len(d) != 1 || d[0].Tip != tip || d[0].Target != "refs/heads/main" ||
		!slices.Equal(headsOf(d[0]), []string{head}) || d[0].DefsOID != blobOf(e.files[tip]) ||
		writes[len(writes)-1].Dispatched == nil {
		t.Fatalf("the dispatches recorded are %+v, of %d writes; want the head under main's file at its tip, "+
			"last", d, len(writes))
	}
}

// A PATHS CHECK IS SKIPPED WHERE NOTHING MATCHES: a check naming paths is
// queued where the change touches one its globs match and recorded SKIPPED
// where it touches none — judged against what the head touches against the
// target's tip — a check naming none is queued whatever the change touches,
// and a change touching more than a list holds runs every check.
//
// Mutation: queue a paths check whatever the change touches — docs is queued
// for a change to cmd/main.go, red.
func TestAPathsCheckIsSkippedWhereNothingMatches(t *testing.T) {
	t.Parallel()
	tip := oid("2")
	narrow, wide := oid("3"), oid("4")
	e := newCheckEstate()
	e.files[tip] = checksFile(`docs paths: ["docs/**"]`, `go paths: ["**/*.go"]`, "all")
	e.facts[narrow] = code.RevisionFactsAnswer{Head: narrow, Target: tip, Touched: []string{"cmd/main.go"}}
	e.facts[wide] = code.RevisionFactsAnswer{Head: wide, Target: tip, TouchesAll: true}
	e.due = []code.ChangeDue{owing(1, narrow, tip, code.CheckWork{Dispatch: true}),
		owing(2, wide, tip, code.CheckWork{Dispatch: true})}
	l := checkLoop(t, e)
	if err := home.CheckWork(t.Context(), l, repoA, statelog.Position{}); err != nil {
		t.Fatalf("the check work: %v", err)
	}
	writes, read := e.asked()
	if len(read) != 1 {
		t.Fatalf("the check work read the tip's checks file %d times in one turn, want once", len(read))
	}
	skipped := map[string]map[string]bool{}
	for _, w := range writes {
		if q := w.Queued; q != nil {
			if skipped[q.OID] == nil {
				skipped[q.OID] = map[string]bool{}
			}
			skipped[q.OID][q.Name] = q.Skipped
		}
	}
	if d := dispatchesOf(writes); len(d) != 1 || !slices.Equal(headsOf(d[0]), []string{narrow, wide}) {
		t.Fatalf("the dispatches recorded are %+v, want one naming both heads under their target's tip", d)
	}
	want := map[string]map[string]bool{
		narrow: {"docs": true, "go": false, "all": false},
		wide:   {"docs": false, "go": false, "all": false},
	}
	for head, checks := range want {
		for name, skip := range checks {
			if got, ok := skipped[head][name]; !ok || got != skip {
				t.Errorf("check %s of head %s: skipped %t (queued %t), want skipped %t", name, head[:4], got, ok,
					skip)
			}
		}
	}
}

// A SUPERSEDED HEAD'S CHECKS ARE CANCELLED: a change dispatched again for a
// new head first ends every attempt the listing names still queued or running
// at the head it moved on from, or for the target it left — cancelled,
// superseded, saying what it moved on to — and only then queues the new
// head's checks and records the dispatch.
//
// Mutation: dispatch the new head without ending the old head's attempts —
// they run on for a head nobody merges, red.
func TestASupersededHeadsChecksAreCancelled(t *testing.T) {
	t.Parallel()
	old, head, tip := oid("1"), oid("5"), oid("2")
	e := newCheckEstate()
	e.files[tip] = checksFile("unit")
	stale := []code.CheckKey{{RepoID: repoA, OID: old, Target: "refs/heads/main", Name: "lint", Attempt: 2},
		{RepoID: repoA, OID: head, Target: "refs/heads/release", Name: "unit", Attempt: 1}}
	e.due = []code.ChangeDue{owing(1, head, tip, code.CheckWork{Dispatch: true, Stale: stale})}
	l := checkLoop(t, e)
	if err := home.CheckWork(t.Context(), l, repoA, statelog.Position{}); err != nil {
		t.Fatalf("the check work: %v", err)
	}
	writes, _ := e.asked()
	if len(writes) != 4 {
		t.Fatalf("the check work wrote %d records, want two cancellations, the new head's unit and its "+
			"dispatch", len(writes))
	}
	for i, key := range stale {
		f := writes[i].Finished
		if f == nil || f.CheckKey != key || f.State != code.CheckStateCancelled ||
			f.Reason != code.CheckReasonSuperseded || !strings.Contains(f.Summary, head[:12]) {
			t.Fatalf("write %d is %+v, want attempt %+v cancelled superseded, naming the new head", i, f, key)
		}
	}
	if q := writes[2].Queued; q == nil || q.OID != head || q.Name != "unit" || writes[3].Dispatched == nil {
		t.Fatalf("after the cancellations the check work wrote %+v and %+v, want the new head's unit queued "+
			"and its dispatch", writes[2], writes[3])
	}
}

// A REQUIRED CHECK ADDED AT THE TARGET REACHES A WAITING HEAD: a change
// dispatched while its target's gate requires lint and its target's checks
// file defines no lint queues the checks the file defines and records its
// dispatch under that file, lint left to the gate to wait on. Once the target
// moved to a tip whose file defines lint, the target's READING is due: the
// turn reads the file at the new tip and records it, naming no head; and the
// head — whose dispatch now names another file than its target's reading —
// is listed again owing its dispatch, which queues lint Required under the
// new tip's file, nothing the head holds already, and records the head under
// the new file.
//
// Mutation: record a due target's reading without reading its file at the
// new tip — the reading keeps the old file, and the waiting head is never
// due again, red.
func TestARequiredCheckAddedAtTheTargetReachesAWaitingHead(t *testing.T) {
	t.Parallel()
	head, before, after := oid("1"), oid("2"), oid("6")
	e := newCheckEstate()
	e.files[before] = checksFile("unit")
	e.files[after] = checksFile("unit", "lint")
	e.due = []code.ChangeDue{owing(1, head, before, code.CheckWork{Dispatch: true, Required: []string{"lint"}})}
	l := checkLoop(t, e)
	if err := home.CheckWork(t.Context(), l, repoA, statelog.Position{}); err != nil {
		t.Fatalf("the dispatch: %v", err)
	}
	writes, _ := e.asked()
	if queued, d := queuedOf(writes), dispatchesOf(writes); len(queued) != 1 || queued["unit"].Required ||
		len(d) != 1 || d[0].DefsOID != blobOf(e.files[before]) {
		t.Fatalf("the dispatch before lint was defined queued %v and recorded %+v, want unit unrequired and "+
			"the head under the file at the tip it read", queued, d)
	}

	moved := statelog.Position{Stream: topics.CodeLogStream, Seq: 40}
	e.mu.Lock()
	e.writes, e.due = nil, nil
	e.targets = []code.CheckTargetDue{{TargetRef: "refs/heads/main", Tip: after, TipPosition: moved}}
	e.mu.Unlock()
	if err := home.CheckWork(t.Context(), l, repoA, statelog.Position{}); err != nil {
		t.Fatalf("the moved target's reading: %v", err)
	}
	writes, read := e.asked()
	if d := dispatchesOf(writes); len(writes) != 1 || len(d) != 1 || d[0].Tip != after ||
		d[0].TipPosition != moved || d[0].DefsOID != blobOf(e.files[after]) || len(d[0].Heads) != 0 ||
		read[len(read)-1] != after {
		t.Fatalf("the moved target's turn wrote %+v, want its reading at the new tip alone, naming the file "+
			"it holds and no head", writes)
	}

	waiting := owing(1, head, after, code.CheckWork{Dispatch: true, Held: []string{"unit"},
		Required: []string{"lint"}})
	waiting.TipPosition = moved
	e.mu.Lock()
	e.writes, e.targets, e.due = nil, nil, []code.ChangeDue{waiting}
	e.mu.Unlock()
	if err := home.CheckWork(t.Context(), l, repoA, statelog.Position{}); err != nil {
		t.Fatalf("the waiting head's turn: %v", err)
	}
	writes, _ = e.asked()
	queued := queuedOf(writes)
	lint, ok := queued["lint"]
	d := dispatchesOf(writes)
	if len(writes) != 2 || !ok || !lint.Required || lint.DefsOID != blobOf(e.files[after]) ||
		lint.Def == nil || lint.Def.Run != "make lint" || len(d) != 1 ||
		!slices.Equal(headsOf(d[0]), []string{head}) || d[0].DefsOID != blobOf(e.files[after]) {
		t.Fatalf("the waiting head's turn wrote %+v, want lint alone queued Required under the new tip's file, "+
			"then the head's dispatch under it", writes)
	}
}

// AN INVALID CHECKS FILE ERRORS EVERY REQUIRED CHECK: a target whose checks
// file is no file that runs errors each name its gate requires at the
// change's dispatch — no definition, the parse's refusal its summary — and
// queues nothing else, since nothing else is known of the file; the dispatch
// is recorded under the file's blob, so the head waits on the errors rather
// than on a check nobody queues.
//
// Mutation: queue nothing for an invalid file — the gate's checks wait in
// silence, red.
func TestAnInvalidChecksFileErrorsEveryRequiredCheck(t *testing.T) {
	t.Parallel()
	head, tip := oid("1"), oid("2")
	e := newCheckEstate()
	e.files[tip] = "checks:\n  - name: unit\n    script: make test\n"
	e.due = []code.ChangeDue{owing(1, head, tip, code.CheckWork{Dispatch: true,
		Required: []string{"lint", "unit"}})}
	l := checkLoop(t, e)
	if err := home.CheckWork(t.Context(), l, repoA, statelog.Position{}); err != nil {
		t.Fatalf("the check work: %v", err)
	}
	writes, _ := e.asked()
	queued := queuedOf(writes)
	for _, name := range []string{"lint", "unit"} {
		q, ok := queued[name]
		if !ok || q.Def != nil || !q.Required || !strings.Contains(q.Invalid, "script") ||
			q.Reason != code.QueueDispatch {
			t.Errorf("required check %s is %+v, want errored at its dispatch with the parse's refusal", name, q)
		}
	}
	if d := dispatchesOf(writes); len(queued) != 2 || len(d) != 1 || d[0].DefsOID != blobOf(e.files[tip]) {
		t.Fatalf("the check work queued %v and recorded %+v, want the two required checks errored and the "+
			"dispatch under the invalid file", queued, d)
	}
}

// A MENDED CHECKS FILE REACHES THE HEADS IT ERRORED: a head whose dispatch
// errored lint and unit for a checks file that did not run is listed again
// once its target's file runs, its two attempts named invalid; its dispatch
// queues each as its NEXT attempt, Required, under the mended file, and
// records the head under it — where queuing the first attempt again would be
// answered from the errored row, and the gate would wait on an error for ever.
//
// Mutation: read an invalid attempt as held — nothing is queued, red.
func TestAMendedChecksFileReachesTheHeadsItErrored(t *testing.T) {
	t.Parallel()
	head, tip := oid("1"), oid("2")
	e := newCheckEstate()
	e.files[tip] = checksFile("lint", "unit")
	invalid := []code.CheckKey{{RepoID: repoA, OID: head, Target: "refs/heads/main", Name: "lint", Attempt: 1},
		{RepoID: repoA, OID: head, Target: "refs/heads/main", Name: "unit", Attempt: 1}}
	e.due = []code.ChangeDue{owing(1, head, tip, code.CheckWork{Dispatch: true, Invalid: invalid,
		Required: []string{"lint", "unit"}})}
	l := checkLoop(t, e)
	if err := home.CheckWork(t.Context(), l, repoA, statelog.Position{}); err != nil {
		t.Fatalf("the check work: %v", err)
	}
	writes, _ := e.asked()
	queued := queuedOf(writes)
	for _, name := range []string{"lint", "unit"} {
		q, ok := queued[name]
		if !ok || q.Attempt != 2 || !q.Required || q.Def == nil || q.Invalid != "" ||
			q.Reason != code.QueueDispatch || q.DefsOID != blobOf(e.files[tip]) {
			t.Errorf("check %s is %+v, want its attempt 2 queued Required under the mended file", name, q)
		}
	}
	if d := dispatchesOf(writes); len(d) != 1 || d[0].DefsOID != blobOf(e.files[tip]) {
		t.Fatalf("the check work recorded %+v, want the head under the mended file", d)
	}
}

// A TURN'S DISPATCH NAMES AT MOST code.MaxDispatchHeads HEADS: the heads one
// turn dispatches under one target's tip share its records, one more past
// every MaxDispatchHeads of them, each head named once.
//
// Mutation: record a target's heads in one record whatever their number —
// the writer refuses it past the bound, red.
func TestATurnsDispatchNamesAtMostMaxDispatchHeads(t *testing.T) {
	t.Parallel()
	tip := oid("2")
	e := newCheckEstate()
	for i := range code.MaxDispatchHeads + 1 {
		c := owing(int64(i+1), fmt.Sprintf("%040x", i+1), tip, code.CheckWork{Dispatch: true})
		c.ID = fmt.Sprintf("01920000-0000-7000-8000-%012x", i+1)
		e.due = append(e.due, c)
	}
	l := checkLoop(t, e)
	if err := home.CheckWork(t.Context(), l, repoA, statelog.Position{}); err != nil {
		t.Fatalf("the check work: %v", err)
	}
	writes, _ := e.asked()
	d := dispatchesOf(writes)
	if len(writes) != 2 || len(d) != 2 || len(d[0].Heads) != code.MaxDispatchHeads || len(d[1].Heads) != 1 {
		t.Fatalf("%d heads under one tip were recorded in %d writes, %d dispatches; want two records of %d and 1",
			code.MaxDispatchHeads+1, len(writes), len(d), code.MaxDispatchHeads)
	}
}
