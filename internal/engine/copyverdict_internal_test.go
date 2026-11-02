package engine

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A FAULT IN A DOMAIN NO SEAT DEPENDS ON SHEDS NO SEAT AND WITHHOLDS NO CLAIM —
// even on a node whose view of the fleet cannot name a peer — while a fault in
// one a seat does still makes the node's seats depend on that view (ADR-0072).
//
// A wrong vectors copy goes out of service for the vectors alone: the seat copy
// — the tracker and the knowledge base — is still this node's, so the node
// answers its seats itself and needs no view to keep or claim them. A wrong
// tracker copy takes the seat copy with it: its seats' tracker calls need a
// peer, and a node that cannot name one gives them up — while its knowledge
// base's copy, sound, keeps serving the operations that are its own.
func TestAFaultInADomainThatGatesNoSeatShedsNone(t *testing.T) {
	t.Parallel()
	view, now := staleView(t)
	var verdict atomic.Pointer[copyVerdict]
	set := func(v copyVerdict) { verdict.Store(&v) }
	set(wrongOn(vectorsName))
	e := dataNodeOver(t, view, now, func(context.Context, *core) copyVerdict {
		return *verdict.Load()
	})
	e.native.Store(&native{trackerReader: &worklet.Reader{}, pageReader: &doclet.Reader{}})
	rejudge := func() {
		e.local.mu.Lock()
		e.local.slot = verdictSlot{}
		e.local.mu.Unlock()
		judged(t.Context(), e.local, trackerName)
	}
	serves := func(domain string) bool {
		_, ok := e.local.For(t.Context(), domain)
		return ok
	}
	rejudge()

	if ok, reason := e.serviceable(t.Context(), now()); !ok {
		t.Fatalf("a wrong vectors copy shed the node's seats over a stale view: %s", reason)
	}
	if !e.StateLogHydrated(t.Context()) {
		t.Fatal("a wrong vectors copy withheld the node's seat claims over a stale view")
	}
	switch {
	case serves(vectorsName):
		t.Fatal("a wrong vectors copy is still served")
	case !serves(trackerName) || !serves(pagesName):
		t.Fatal("a wrong vectors copy took the tracker's or the knowledge base's out of service")
	case !serves(estate.SeatCopy):
		t.Fatal("a wrong vectors copy took the seat copy out of service")
	}

	// A DOMAIN A SEAT DEPENDS ON: the seat copy goes with it, and the stale
	// view decides — and cannot.
	set(wrongOn(trackerName))
	rejudge()
	if ok, _ := e.serviceable(t.Context(), now()); ok {
		t.Fatal("a node whose tracker copy is wrong and whose view cannot name a data " +
			"node kept its seats")
	}
	if e.StateLogHydrated(t.Context()) {
		t.Fatal("a node whose tracker copy is wrong and whose view cannot name a data " +
			"node claimed a seat")
	}
	switch {
	case serves(trackerName):
		t.Fatal("a wrong tracker copy is still served")
	case !serves(pagesName):
		t.Fatal("a wrong tracker copy took the knowledge base's copy out of service")
	case serves(estate.SeatCopy):
		t.Fatal("a wrong tracker copy left the seat copy in service")
	}
}

// A LAGGING DOMAIN HOLDS BACK ONLY ITS OWN OPERATIONS: whether a copy answers
// requests is its distance from ITS log, and no tracker read is held to the
// knowledge base's — for the reason no floor on another domain's log is waited
// for. When the answer was one AND over every domain, a pages log a burst put
// past the snapshot slack made every tracker read on the node a last resort.
func TestALaggingDomainHoldsBackOnlyItsOwnOperations(t *testing.T) {
	t.Parallel()
	e := &Engine{backends: &Backends{}}
	withCore(e)
	e.native.Store(&native{trackerReader: &worklet.Reader{}, pageReader: &doclet.Reader{}})
	l := localWith(e, time.Now, func(context.Context, *core) copyVerdict {
		return laggingOn(pagesName)
	})
	work, ok := judged(t.Context(), l, trackerName)
	if !ok || work.Answers == nil || !work.Answers(t.Context()) {
		t.Fatal("a lagging knowledge base held back the tracker's operations")
	}
	wiki, ok := l.For(t.Context(), pagesName)
	if !ok || wiki.Answers == nil || wiki.Answers(t.Context()) {
		t.Fatal("a lagging knowledge base's copy answers as though it were level")
	}
}

// THE SEAT COPY IS EXACTLY THE DOMAINS THAT GATE SEATS: made wrong one at a
// time, each registered domain takes the seat copy out of service exactly when
// it declares that a seat depends on it, and takes nothing but itself and the
// seat copy out with it. A shut file serves nothing; a verdict never judged is
// wrong about nothing and answers nothing.
func TestTheSeatCopyIsTheDomainsThatGateSeats(t *testing.T) {
	t.Parallel()
	for _, domain := range registeredDomains() {
		v := wrongOn(domain.Name())
		if got := v.serves(estate.SeatCopy); got != !domain.GatesSeatAdmission() {
			t.Errorf("a wrong %s copy: the seat copy serves = %v, want %v — %s declares "+
				"GatesSeatAdmission %v", domain.Name(), got, !domain.GatesSeatAdmission(),
				domain.Name(), domain.GatesSeatAdmission())
		}
		for _, other := range registeredNames() {
			if got := v.serves(other); got != (other != domain.Name()) {
				t.Errorf("a wrong %s copy: %s serves = %v", domain.Name(), other, got)
			}
		}
		lagging := laggingOn(domain.Name())
		if got := lagging.answers(estate.SeatCopy); got != !domain.GatesSeatAdmission() {
			t.Errorf("a lagging %s copy: the seat copy answers = %v", domain.Name(), got)
		}
	}
	shut := copyVerdict{shut: true}
	for _, name := range append(registeredNames(), estate.SeatCopy) {
		if shut.serves(name) || shut.answers(name) {
			t.Errorf("a shut copy serves or answers %q", name)
		}
	}
	var never copyVerdict
	if !never.serves(estate.SeatCopy) || never.answers(estate.SeatCopy) ||
		!never.serves(trackerName) || never.answers(trackerName) {
		t.Error("a copy never judged must be wrong about nothing and answer nothing")
	}
}

// A BLIP ON ONE DOMAIN KEEPS ONLY THAT DOMAIN'S VERDICT. A reading that could
// not read one log's health says nothing about any other's: the domain it could
// not read keeps what the verdict before it held, and every domain it did read
// takes what it found — so a tracker found sound again is served at once, while
// a knowledge base nobody could read stays as wrong as it was. When the carry
// was the whole verdict, one unread log kept every domain where it had been.
func TestABlipOnOneDomainKeepsOnlyThatDomainsVerdict(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_700_000_000, 0)
	verdict := wrongOn(trackerName)
	e := &Engine{backends: &Backends{}}
	withCore(e)
	e.native.Store(&native{trackerReader: &worklet.Reader{}, pageReader: &doclet.Reader{}})
	l := localWith(e, func() time.Time { return now },
		func(context.Context, *core) copyVerdict { return verdict })
	serves := func(domain string) bool {
		_, ok := judged(t.Context(), l, domain)
		return ok
	}
	if serves(trackerName) || !serves(pagesName) {
		t.Fatal("the premise: the tracker is out of service and the knowledge base is not")
	}

	now = now.Add(servingRecheck)
	verdict = unreadOn(trackerName)
	d := verdict.domains[pagesName]
	d.wrong = true
	verdict.domains[pagesName] = d
	if serves(trackerName) {
		t.Fatal("a reading that could not read the tracker put its wrong copy back into service")
	}
	if serves(pagesName) {
		t.Fatal("a knowledge base found wrong is still served")
	}

	now = now.Add(servingRecheck)
	verdict = unreadOn(pagesName)
	if !serves(trackerName) {
		t.Fatal("a tracker found sound was held out of service by a blip on the knowledge base")
	}
	if serves(pagesName) {
		t.Fatal("a reading that could not read the knowledge base put its wrong copy back")
	}
}

// WHAT GOES OUT OF SERVICE IS ANNOUNCED ONCE, PER DOMAIN, WITH ITS FAULT: the
// line names each domain whose copy this reading found newly wrong and why,
// never one that already was, and a shut file once as the estate.
func TestOnlyANewlyWrongCopyIsAnnounced(t *testing.T) {
	t.Parallel()
	below, evicted := statelog.RefuseBelowFloor, statelog.RefuseEvicted
	mixed := faultyOn(evicted, vectorsName)
	d := mixed.domains[trackerName]
	d.wrong, d.why = true, below
	mixed.domains[trackerName] = d
	for name, tc := range map[string]struct {
		held  copyVerdict
		known bool
		now   copyVerdict
		want  []wentOut
	}{
		"nothing held": {now: mixed,
			want: []wentOut{{trackerName, below}, {vectorsName, evicted}}},
		"one already out": {held: wrongOn(trackerName), known: true,
			now: mixed, want: []wentOut{{vectorsName, evicted}}},
		"none new":    {held: wrongOn(trackerName), known: true, now: wrongOn(trackerName)},
		"a shut file": {held: soundCopy(), known: true, now: copyVerdict{shut: true}, want: []wentOut{{domain: shutEstate}}},
		"still shut":  {held: copyVerdict{shut: true}, known: true, now: copyVerdict{shut: true}},
		"reopened shut": {held: copyVerdict{shut: true}, known: true,
			now: unreadOn(registeredNames()...)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			stored := tc.now.carried(tc.held, tc.known)
			if got := stored.newlyWrong(tc.held, tc.known); !slices.Equal(got, tc.want) {
				t.Fatalf("announced %v, want %v", got, tc.want)
			}
		})
	}
	// AND THE CARRY OF A SHUT FILE: an unread reading after it keeps every
	// domain out of service.
	stored := unreadOn(registeredNames()...).carried(copyVerdict{shut: true}, true)
	for _, name := range registeredNames() {
		if stored.serves(name) {
			t.Errorf("an unread reading after a shut file put %s back into service", name)
		}
	}
	// AND AN UNREAD DOMAIN KEEPS THE FAULT IT WAS FOUND WITH, beside the
	// verdict: the fault is what was read, and nothing new was.
	kept := unreadOn(trackerName).carried(faultyOn(evicted, trackerName), true)
	if got := kept.domains[trackerName]; !got.wrong || got.why != evicted {
		t.Errorf("an unread domain found evicted before carried %+v", got)
	}
}
