package trashreap_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/trashreap"
)

// term is the thirty days the engine passes.
const term = 30 * 24 * time.Hour

// noon is the clock every case starts on: twelve minutes into a slot, so the
// slot's start and the clock differ and a case reading the wrong one fails.
var noon = time.Date(2026, 10, 17, 12, 12, 0, 0, time.UTC)

// slotOf is the start of the slot a pass at now runs in.
func slotOf(now time.Time) time.Time { return now.Truncate(trashreap.PassInterval) }

// clock is a settable clock.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

// trash is a fake source: a domain's trash that decides each purge again
// against its own rows, as a real one does in its snapshot.
type trash struct {
	name string

	mu      sync.Mutex
	trashed map[string]time.Time // id -> when it went into the trash
	purged  []string
	opIDs   map[string]string // id -> the op id its purge was asked under
	asked   map[string]int    // id -> purges asked

	// listed counts the listings; listErr fails them.
	listed  int
	listErr error

	// failing purges answer this error and purge nothing.
	failing map[string]error
	// sticky purges answer nil and leave the item listed: a purge durable
	// on the log and not yet applied on this node's copy.
	sticky map[string]bool
	// onList runs after each listing, before any purge: the place a case
	// restores an item between the listing and its purge.
	onList func()
}

func newTrash(name string) *trash {
	return &trash{
		name: name, trashed: map[string]time.Time{}, opIDs: map[string]string{},
		asked: map[string]int{}, failing: map[string]error{}, sticky: map[string]bool{},
	}
}

func (t *trash) Name() string { return t.name }

func (t *trash) put(id string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.trashed[id] = at
}

func (t *trash) restore(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.trashed, id)
}

func (t *trash) TrashedBefore(_ context.Context, cutoff time.Time, limit int) ([]string, error) {
	t.mu.Lock()
	t.listed++
	if t.listErr != nil {
		err := t.listErr
		t.mu.Unlock()
		return nil, err
	}
	type item struct {
		id string
		at time.Time
	}
	var due []item
	for id, at := range t.trashed {
		if at.Before(cutoff) {
			due = append(due, item{id, at})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].at.Equal(due[j].at) {
			return due[i].at.Before(due[j].at)
		}
		return due[i].id < due[j].id
	})
	if len(due) > limit {
		due = due[:limit]
	}
	out := make([]string, 0, len(due))
	for _, d := range due {
		out = append(out, d.id)
	}
	onList := t.onList
	t.mu.Unlock()
	if onList != nil {
		onList()
	}
	return out, nil
}

func (t *trash) PurgeTrashed(_ context.Context, id string, cutoff time.Time, opID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.asked[id]++
	t.opIDs[id] = opID
	if err, ok := t.failing[id]; ok {
		return err
	}
	at, ok := t.trashed[id]
	switch {
	case !ok:
		return fmt.Errorf("%w: %s is not in the trash", trashreap.ErrNotEligible, id)
	case !at.Before(cutoff):
		return fmt.Errorf("%w: %s went in at %s", trashreap.ErrNotEligible, id, at)
	}
	if t.sticky[id] {
		return nil
	}
	delete(t.trashed, id)
	t.purged = append(t.purged, id)
	return nil
}

func (t *trash) purgedIDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := slices.Clone(t.purged)
	slices.Sort(out)
	return out
}

func (t *trash) remaining() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.trashed)
}

// rig is a reaper over sources, a memory fleet and a settable clock.
type rig struct {
	reaper *trashreap.Reaper
	fleet  *memory.Fleet
	clock  *clock
}

func newRig(t *testing.T, node string, fleet *memory.Fleet, c *clock,
	sources ...trashreap.Source) rig {

	t.Helper()
	r, err := trashreap.New(trashreap.Deps{
		Sources: sources, Term: term, Reports: fleet, Node: node, Now: c.now,
	})
	if err != nil {
		t.Fatalf("build the reaper: %v", err)
	}
	return rig{reaper: r, fleet: fleet, clock: c}
}

// held is a duty claim that always answers held.
func held(context.Context) (bool, error) { return true, nil }

// A PASS PURGES ONLY WHAT WENT INTO THE TRASH BEFORE THE CUTOFF, and the cutoff
// is the slot's start less the term: an item exactly at the cutoff, or after
// it, is still inside its term and stays recoverable.
func TestAPassPurgesOnlyWhatWasTrashedBeforeTheCutoff(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	cutoff := slotOf(noon).Add(-term)
	pages.put("old", cutoff.Add(-time.Hour))
	pages.put("just-past", cutoff.Add(-time.Microsecond))
	pages.put("at-the-cutoff", cutoff)
	pages.put("recent", cutoff.Add(time.Minute))
	r := newRig(t, "node-a", memory.NewFleet(), &clock{at: noon}, pages)

	report, err := r.reaper.Pass(t.Context())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got, want := pages.purgedIDs(), []string{"just-past", "old"}; !slices.Equal(got, want) {
		t.Fatalf("purged %v, want %v", got, want)
	}
	if !report.Cutoff.Equal(cutoff) || !report.Slot.Equal(slotOf(noon)) {
		t.Errorf("the report says slot %s cutoff %s, want %s and %s",
			report.Slot, report.Cutoff, slotOf(noon), cutoff)
	}
	if s := report.Sources[0]; s.Purged != 2 || s.Skipped != 0 || s.Failed != 0 || s.More {
		t.Errorf("the report counts %+v", s)
	}
}

// AN ITEM RESTORED AFTER IT WAS LISTED IS NOT PURGED. The list only nominates:
// a person taking a page back between the listing and the purge must win, and
// the reaper counts the refusal as skipped rather than as a failure.
func TestAnItemRestoredAfterItWasListedIsNotPurged(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	cutoff := slotOf(noon).Add(-term)
	pages.put("taken-back", cutoff.Add(-time.Hour))
	pages.put("left", cutoff.Add(-2*time.Hour))
	pages.onList = func() { pages.restore("taken-back") }
	r := newRig(t, "node-a", memory.NewFleet(), &clock{at: noon}, pages)

	report, err := r.reaper.Pass(t.Context())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := pages.purgedIDs(); !slices.Equal(got, []string{"left"}) {
		t.Fatalf("purged %v, want only the item nobody took back", got)
	}
	if s := report.Sources[0]; s.Purged != 1 || s.Skipped != 1 || s.Failed != 0 {
		t.Errorf("the report counts %+v, want one purged and one skipped", s)
	}
	if !report.Next.Equal(slotOf(noon).Add(trashreap.PassInterval)) {
		t.Errorf("a skip moved the next pass to %s; it is no failure", report.Next)
	}
}

// TWO REAPERS IN ONE SLOT PURGE UNDER THE SAME OPERATION IDS, so the ledger
// and the broker collapse the second into the first while the duty moves; and
// every other slot, source or item is a different operation.
func TestTwoReapersInOneSlotPurgeUnderTheSameOperationIds(t *testing.T) {
	t.Parallel()
	cutoff := slotOf(noon).Add(-term)
	fleet := memory.NewFleet()
	first, second := newTrash("pages"), newTrash("pages")
	for _, tr := range []*trash{first, second} {
		tr.put("p1", cutoff.Add(-time.Hour))
		tr.put("p2", cutoff.Add(-time.Hour))
	}
	a := newRig(t, "node-a", fleet, &clock{at: noon}, first)
	b := newRig(t, "node-b", fleet, &clock{at: noon.Add(40 * time.Minute)}, second)
	if _, err := a.reaper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.reaper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"p1", "p2"} {
		if first.opIDs[id] == "" || first.opIDs[id] != second.opIDs[id] {
			t.Errorf("item %s was purged under %q and %q in one slot", id,
				first.opIDs[id], second.opIDs[id])
		}
		if err := statelog.CheckCallerOpID(first.opIDs[id]); err != nil {
			t.Errorf("the op id of %s is not one the engine's grammar carries: %v", id, err)
		}
		if at, ok := statelog.OpMintedAt(first.opIDs[id]); !ok || !at.Equal(slotOf(noon)) {
			t.Errorf("the op id of %s carries %s (%v), want the slot's start %s",
				id, at, ok, slotOf(noon))
		}
	}
	if first.opIDs["p1"] == first.opIDs["p2"] {
		t.Error("two items share one operation id")
	}
	slot := slotOf(noon)
	base := trashreap.OpID(slot, "pages", "p1")
	for what, other := range map[string]string{
		"another slot":   trashreap.OpID(slot.Add(trashreap.PassInterval), "pages", "p1"),
		"another source": trashreap.OpID(slot, "artifacts", "p1"),
		"another item":   trashreap.OpID(slot, "pages", "p2"),
	} {
		if other == base {
			t.Errorf("%s derives the same operation id", what)
		}
	}
	if trashreap.OpID(slot, "pages", "p1") != base {
		t.Error("the same slot, source and item derive two operation ids")
	}
}

// A PASS STOPS AT ITS CAP, says so, and the next pass is due at once: the cap
// bounds a burst on the domain's log, not how fast a backlog drains.
func TestAPassStopsAtItsCapAndTheNextIsDueAtOnce(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	cutoff := slotOf(noon).Add(-term)
	total := trashreap.MaxPerPass + 5
	for i := range total {
		pages.put(fmt.Sprintf("p%05d", i), cutoff.Add(-time.Duration(total-i)*time.Second))
	}
	c := &clock{at: noon}
	r := newRig(t, "node-a", memory.NewFleet(), c, pages)

	report, err := r.reaper.Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s := report.Sources[0]
	if s.Purged != trashreap.MaxPerPass || !s.More {
		t.Fatalf("a pass over %d due items reports %+v, want %d purged and more",
			total, s, trashreap.MaxPerPass)
	}
	// OLDEST FIRST: what is left is the youngest five.
	if pages.remaining() != 5 {
		t.Fatalf("%d items remain, want 5", pages.remaining())
	}
	if _, ok := pages.trashed["p00000"]; ok {
		t.Error("the oldest item survived a capped pass")
	}
	if !report.Next.Equal(report.Finished) {
		t.Errorf("after a capped pass the next is due at %s, want at once (%s)",
			report.Next, report.Finished)
	}
	r.reaper.Turn(t.Context(), held)
	if pages.remaining() != 0 {
		t.Fatalf("%d items remain after the next turn", pages.remaining())
	}
}

// A FAILED PURGE IS COUNTED, NAMED AND RETRIED, and it neither stops the pass
// nor loops it: every other due item is purged, the failing one is asked once
// a pass, and the next pass comes after the retry interval rather than a slot
// later.
func TestAFailedPurgeIsRetriedAfterTheRetryInterval(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	cutoff := slotOf(noon).Add(-term)
	for _, id := range []string{"a", "b", "c"} {
		pages.put(id, cutoff.Add(-time.Hour))
	}
	pages.failing["b"] = errors.New("doclet: the outcome is unknown")
	c := &clock{at: noon}
	r := newRig(t, "node-a", memory.NewFleet(), c, pages)

	report, err := r.reaper.Pass(t.Context())
	if err == nil || !strings.Contains(err.Error(), "purge b") {
		t.Fatalf("a pass with a failed purge answers %v, want it named", err)
	}
	s := report.Sources[0]
	if s.Purged != 2 || s.Failed != 1 || !strings.Contains(s.Error, "unknown") {
		t.Fatalf("the report counts %+v", s)
	}
	if pages.asked["b"] != 1 {
		t.Errorf("the failing item was asked %d times in one pass, want once", pages.asked["b"])
	}
	if want := report.Finished.Add(trashreap.RetryInterval); !report.Next.Equal(want) {
		t.Errorf("the next pass is due %s, want %s", report.Next, want)
	}

	// NOT BEFORE THE RETRY INTERVAL...
	c.set(noon.Add(trashreap.RetryInterval - time.Second))
	r.reaper.Turn(t.Context(), held)
	if pages.asked["b"] != 1 {
		t.Fatal("a turn inside the retry interval ran a pass")
	}
	// ...AND THEN UNDER THE SAME OPERATION ID, since the slot is the same.
	first := pages.opIDs["b"]
	delete(pages.failing, "b")
	c.set(noon.Add(trashreap.RetryInterval))
	r.reaper.Turn(t.Context(), held)
	if got := pages.purgedIDs(); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("after the retry purged %v", got)
	}
	if pages.opIDs["b"] != first {
		t.Errorf("the retry in the same slot asked under %q, the first under %q",
			pages.opIDs["b"], first)
	}
}

// A RETRY IS NEVER LATER THAN THE NEXT SLOT: a failure near a slot's end is
// retried with that slot's pass, not ten minutes into it.
func TestARetryIsNeverLaterThanTheNextSlot(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	pages.listErr = errors.New("the copy is closing")
	late := slotOf(noon).Add(trashreap.PassInterval - time.Minute)
	r := newRig(t, "node-a", memory.NewFleet(), &clock{at: late}, pages)
	report, err := r.reaper.Pass(t.Context())
	if err == nil || !strings.Contains(err.Error(), "list the trash") {
		t.Fatalf("a failed listing answers %v", err)
	}
	if !strings.Contains(report.Sources[0].Error, "closing") {
		t.Errorf("the report does not carry the listing's failure: %+v", report.Sources[0])
	}
	if want := slotOf(late).Add(trashreap.PassInterval); !report.Next.Equal(want) {
		t.Errorf("the next pass is due %s, want the next slot %s", report.Next, want)
	}
}

// AN ITEM A PURGE DID NOT TAKE OFF THIS NODE'S COPY IS ASKED ONCE A PASS: a
// purge durable on the log and not yet applied here is listed again, and asking
// it again for the rest of the pass would spin until the cap.
func TestAnItemStillListedAfterItsPurgeIsAskedOncePerPass(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	cutoff := slotOf(noon).Add(-term)
	// THE OLDEST IS THE ONE STILL LISTED, and a full batch behind it, so the
	// pass has to list a second time and meets it again.
	pages.put("pending", cutoff.Add(-2*time.Hour))
	pages.sticky["pending"] = true
	for i := range trashreap.BatchSize {
		pages.put(fmt.Sprintf("p%03d", i), cutoff.Add(-time.Hour))
	}
	r := newRig(t, "node-a", memory.NewFleet(), &clock{at: noon}, pages)

	report, err := r.reaper.Pass(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if pages.listed < 2 {
		t.Fatalf("the pass listed %d times; the case needs it to list again", pages.listed)
	}
	if pages.asked["pending"] != 1 {
		t.Errorf("a purge still listed was asked %d times in one pass", pages.asked["pending"])
	}
	if s := report.Sources[0]; s.Purged != trashreap.BatchSize+1 || s.More {
		t.Errorf("the report counts %+v", s)
	}
}

// EVERY SOURCE IS VISITED, each in its own row, and one source's failure does
// not keep the reaper from another's trash.
func TestEverySourceIsReapedWhateverAnotherDoes(t *testing.T) {
	t.Parallel()
	cutoff := slotOf(noon).Add(-term)
	broken, artifacts := newTrash("pages"), newTrash("artifacts")
	broken.listErr = errors.New("no copy")
	artifacts.put("x", cutoff.Add(-time.Hour))
	r := newRig(t, "node-a", memory.NewFleet(), &clock{at: noon}, broken, artifacts)

	report, err := r.reaper.Pass(t.Context())
	if err == nil {
		t.Fatal("a pass with a failed source answered no error")
	}
	if len(report.Sources) != 2 || report.Sources[0].Source != "pages" ||
		report.Sources[1].Source != "artifacts" {
		t.Fatalf("the report's rows are %+v", report.Sources)
	}
	if got := artifacts.purgedIDs(); !slices.Equal(got, []string{"x"}) {
		t.Errorf("the healthy source purged %v", got)
	}
}

// EACH PASS IS RECORDED where every node reads it, and the node that takes the
// duty next resumes from it: it runs no pass until the last holder said the
// next is due, then runs one.
func TestTheDutyResumesFromTheFleetsLastReport(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	cutoff := slotOf(noon).Add(-term)
	first, second := newTrash("pages"), newTrash("pages")
	a := newRig(t, "node-a", fleet, &clock{at: noon}, first)
	a.reaper.Turn(t.Context(), held)

	last, found, err := trashreap.LastReport(t.Context(), fleet)
	if err != nil || !found {
		t.Fatalf("no report recorded: %v, %v", found, err)
	}
	if last.Node != "node-a" || !last.Next.Equal(slotOf(noon).Add(trashreap.PassInterval)) {
		t.Fatalf("the recorded report is %+v", last)
	}

	// THE DUTY MOVES to node-b, twenty minutes later, in the same slot.
	second.put("p", cutoff.Add(-time.Hour))
	c := &clock{at: noon.Add(20 * time.Minute)}
	b := newRig(t, "node-b", fleet, c, second)
	b.reaper.Turn(t.Context(), held)
	if second.listed != 0 {
		t.Fatal("the node that took the duty ran a pass the last holder had already run")
	}
	c.set(last.Next)
	b.reaper.Turn(t.Context(), held)
	if second.listed == 0 || len(second.purgedIDs()) != 1 {
		t.Fatalf("the next slot's pass did not run on the new holder (listed %d)", second.listed)
	}
	again, _, err := trashreap.LastReport(t.Context(), fleet)
	if err != nil || again.Node != "node-b" || again.Sources[0].Purged != 1 {
		t.Fatalf("the new holder's pass is recorded as %+v (%v)", again, err)
	}
}

// A DUTY THIS NODE DOES NOT HOLD RUNS NO PASS, and neither does one whose claim
// could not be answered: an unknown claim says nothing about who holds it.
func TestADutyNotHeldRunsNoPass(t *testing.T) {
	t.Parallel()
	for name, claim := range map[string]func(context.Context) (bool, error){
		"held by a peer": func(context.Context) (bool, error) { return false, nil },
		"unanswered": func(context.Context) (bool, error) {
			return false, errors.New("the coordination store did not answer")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pages := newTrash("pages")
			pages.put("p", slotOf(noon).Add(-term-time.Hour))
			fleet := memory.NewFleet()
			r := newRig(t, "node-a", fleet, &clock{at: noon}, pages)
			r.reaper.Turn(t.Context(), claim)
			if pages.listed != 0 || pages.remaining() != 1 {
				t.Fatal("a duty this node does not hold ran a pass")
			}
			if _, found, _ := fleet.TrashReap(t.Context()); found {
				t.Fatal("a duty this node does not hold recorded a pass")
			}
		})
	}
}

// A LONE NODE RUNS THE DUTY ALONE: a nil claim is a node with nobody to share
// it with, never a duty refused.
func TestALoneNodeRunsTheDuty(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	pages.put("p", slotOf(noon).Add(-term-time.Hour))
	r := newRig(t, "node-a", memory.NewFleet(), &clock{at: noon}, pages)
	r.reaper.Turn(t.Context(), nil)
	if pages.remaining() != 0 {
		t.Fatal("a lone node did not run its pass")
	}
}

// A PASS WHOSE CONTEXT ENDED RECORDS NOTHING: the node is stopping, and the
// next holder resumes from the last pass that finished rather than from one
// that stopped part of the way through.
func TestAPassCutShortRecordsNothing(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	pages.put("p", slotOf(noon).Add(-term-time.Hour))
	fleet := memory.NewFleet()
	r := newRig(t, "node-a", fleet, &clock{at: noon}, pages)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.reaper.Pass(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled pass answers %v", err)
	}
	if _, found, _ := fleet.TrashReap(t.Context()); found {
		t.Fatal("a cancelled pass was recorded")
	}
	if pages.remaining() != 1 {
		t.Fatal("a cancelled pass purged")
	}
}

// A REAPER IS REFUSED where it could not run honestly: no trash to empty, two
// sources under one name, a name an operation id cannot carry, a term nobody
// could restore within, nowhere to report and no node to name.
func TestAReaperIsRefusedWhatItCouldNotRunHonestly(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	ok := trashreap.Deps{
		Sources: []trashreap.Source{newTrash("pages")}, Term: term,
		Reports: fleet, Node: "node-a",
	}
	if _, err := trashreap.New(ok); err != nil {
		t.Fatalf("the sound deps were refused: %v", err)
	}
	for name, mutate := range map[string]func(d *trashreap.Deps){
		"no source":      func(d *trashreap.Deps) { d.Sources = nil },
		"a nil source":   func(d *trashreap.Deps) { d.Sources = []trashreap.Source{nil} },
		"two names":      func(d *trashreap.Deps) { d.Sources = append(d.Sources, newTrash("pages")) },
		"a dotted name":  func(d *trashreap.Deps) { d.Sources = []trashreap.Source{newTrash("pa.ges")} },
		"an empty name":  func(d *trashreap.Deps) { d.Sources = []trashreap.Source{newTrash("")} },
		"a short term":   func(d *trashreap.Deps) { d.Term = trashreap.MinTerm - time.Second },
		"no term":        func(d *trashreap.Deps) { d.Term = 0 },
		"no reports":     func(d *trashreap.Deps) { d.Reports = nil },
		"no node":        func(d *trashreap.Deps) { d.Node = " " },
		"an upper token": func(d *trashreap.Deps) { d.Sources = []trashreap.Source{newTrash("Pages")} },
	} {
		d := ok
		d.Sources = slices.Clone(ok.Sources)
		mutate(&d)
		if _, err := trashreap.New(d); err == nil {
			t.Errorf("%s: the reaper was built", name)
		}
	}
}

// AN UNREADABLE REPORT RUNS THE PASS rather than waiting on a record nobody can
// read: a pass in a slot that had one finds the trash already emptied.
func TestAnUnreadableReportRunsThePass(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	if err := fleet.RecordTrashReap(t.Context(), []byte("{not json")); err != nil {
		t.Fatal(err)
	}
	pages := newTrash("pages")
	pages.put("p", slotOf(noon).Add(-term-time.Hour))
	r := newRig(t, "node-a", fleet, &clock{at: noon}, pages)
	r.reaper.Turn(t.Context(), held)
	if pages.remaining() != 0 {
		t.Fatal("an unreadable report kept the pass from running")
	}
	if _, err := trashreap.DecodeReport([]byte("{not json")); err == nil {
		t.Error("an unreadable report decoded")
	}
}

// termed is a source that keeps a term of its own, or cannot say what it is
// (err).
type termed struct {
	*trash
	term time.Duration
	err  error
}

func (t termed) Term(context.Context) (time.Duration, error) { return t.term, t.err }

// A SOURCE THAT KEEPS ITS OWN TERM IS REAPED AT IT, beside a source that keeps
// the reaper's: each is listed against its own cutoff, and the report names the
// cutoff the source was reaped at. Mutation: reap every source at the
// reaper's cutoff and the conversation a week past its term survives.
func TestASourceWithATermOfItsOwnIsReapedAtIt(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	week := 7 * 24 * time.Hour
	assistant := termed{trash: newTrash("assistant"), term: week}
	own := slotOf(noon).Add(-week)
	assistant.put("past-its-week", own.Add(-time.Hour))
	assistant.put("inside-its-week", own.Add(time.Hour))
	pages.put("inside-the-month", own.Add(-time.Hour))
	r := newRig(t, "node-a", memory.NewFleet(), &clock{at: noon}, pages, assistant)

	report, err := r.reaper.Pass(t.Context())
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if got := assistant.purgedIDs(); !slices.Equal(got, []string{"past-its-week"}) {
		t.Errorf("the source with its own term purged %v, want the one past its week", got)
	}
	if got := pages.purgedIDs(); len(got) != 0 {
		t.Errorf("the source on the reaper's term purged %v inside its month", got)
	}
	if s := report.Sources[1]; !s.Cutoff.Equal(own) {
		t.Errorf("the report names the source's cutoff as %s, want %s", s.Cutoff, own)
	}
	if s := report.Sources[0]; !s.Cutoff.IsZero() {
		t.Errorf("the report names a cutoff of its own for a source on the reaper's term: %s", s.Cutoff)
	}
}

// A TERM UNDER THE FLOOR PURGES NOTHING, and says so: the floor guards against
// a mistake destroying what people expected to keep, whichever source states
// the term. The other sources are reaped as ever.
func TestATermUnderTheFloorPurgesNothing(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	assistant := termed{trash: newTrash("assistant"), term: time.Hour}
	assistant.put("an-hour-old", noon.Add(-2*time.Hour))
	pages.put("old", slotOf(noon).Add(-term-time.Hour))
	r := newRig(t, "node-a", memory.NewFleet(), &clock{at: noon}, assistant, pages)

	report, err := r.reaper.Pass(t.Context())
	if err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Errorf("a term under the floor answers %v", err)
	}
	if assistant.remaining() != 1 || assistant.listed != 0 {
		t.Errorf("a source under the floor was listed %d times and lost items", assistant.listed)
	}
	if report.Sources[0].Error == "" {
		t.Error("the report says nothing about the source it would not reap")
	}
	if got := pages.purgedIDs(); !slices.Equal(got, []string{"old"}) {
		t.Errorf("the other source purged %v", got)
	}
}

// AN UNKNOWN TERM PURGES NOTHING OF ITS SOURCE, and says why: the term is the
// promise of how long an item is kept, and one the source cannot read at this
// pass (the assistant's, on a node that does not run the revision the fleet's
// activation pointer names) leaves nothing to purge by, since a term guessed in
// its place may be shorter than the one the company keeps. The other sources
// are reaped as ever, and the pass is retried as a failed one is, so what
// waited is purged by the first pass that can read the term. Mutation: reap a
// source whose term is unknown at the reaper's own term and the conversation a
// year old is purged.
func TestAnUnknownTermPurgesNothingOfItsSource(t *testing.T) {
	t.Parallel()
	pages := newTrash("pages")
	unknown := errors.New("this node does not run the revision the fleet's pointer names")
	assistant := termed{trash: newTrash("assistant"), err: unknown}
	assistant.put("a-year-unused", noon.Add(-365*24*time.Hour))
	pages.put("old", slotOf(noon).Add(-term-time.Hour))
	r := newRig(t, "node-a", memory.NewFleet(), &clock{at: noon}, assistant, pages)

	report, err := r.reaper.Pass(t.Context())
	if !errors.Is(err, unknown) {
		t.Errorf("a pass over a source whose term is unknown answers %v, want "+
			"the reason it is unknown", err)
	}
	if assistant.remaining() != 1 || assistant.listed != 0 {
		t.Errorf("a source whose term is unknown was listed %d times and kept %d "+
			"of its 1 item", assistant.listed, assistant.remaining())
	}
	if s := report.Sources[0]; !strings.Contains(s.Error, unknown.Error()) {
		t.Errorf("the report says %q of the source it would not reap, want why", s.Error)
	}
	if got := pages.purgedIDs(); !slices.Equal(got, []string{"old"}) {
		t.Errorf("the other source purged %v, want the page past its term", got)
	}
	if want := noon.Add(trashreap.RetryInterval); !report.Next.Equal(want) {
		t.Errorf("the next pass is due at %s, want %s: a term unknown now may be "+
			"read in minutes, so the pass is retried as a failed one is",
			report.Next, want)
	}
}
