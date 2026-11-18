package memsync

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// A SEAT'S MEMORY IS PUBLISHED ONLY WHILE THIS NODE HOLDS IT.
//
// The changelog keeps the last message on each row's subject, so a node that
// publishes a seat it does not hold puts its own, possibly older, copies over
// the holder's. A seat is carried only under its holding here: live from a
// hydration that completed, for the cycle's calls; releasing from the start of
// its release, for the flush's; none otherwise. The cases below hold each
// publisher: the cycle during a hydration, after a Forget and across a
// release, and the flush of a seat whose hydration failed.

// heldFromEmpty begins s's live holding of handle's seat at boundary 0, as a
// hydration onto an empty store and changelog records it, so every row s's
// store holds or comes to hold for the seat is the holding's own, and answers
// s: the node that has held the seat all along, for a case that never
// hydrates it.
func heldFromEmpty(s *Syncer, handle string) *Syncer {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holdings[handle] = &seatHolding{state: live}
	return s
}

// boundary is where the seat's own writes begin on s
// ([seatHolding.boundary]), and 0 for a seat with no holding.
func (s *Syncer) boundary(handle string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held := s.holdings[handle]; held != nil {
		return held.boundary
	}
	return 0
}

// backlogRows counts the seat's rows in db the backlog carries: in a table it
// carries, stamped at or below through.
func backlogRows(t *testing.T, db *store.DB, through int64) int {
	t.Helper()
	n := 0
	for _, spec := range backlogPassOrder {
		n += len(stamped(t, db, spec, 0, through))
	}
	return n
}

// noMarksOf fails the case for every mark s keeps for the seat, which a case
// after a Forget wants none of.
func noMarksOf(t *testing.T, s *Syncer) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, mark := range s.marks {
		if key.handle == seat.Handle {
			t.Errorf("%s's %s mark is %d, want none: the seat's holding here has ended",
				key.table, key.of, mark)
		}
	}
}

// takingJS is a changelog over a real one that runs during once, as the
// hydration's first replay opens: while the seat is still being taken. It
// records every row subject published through it, barriers aside.
type takingJS struct {
	jetstream.JetStream
	during    func()
	ran       bool
	published []string
}

func (j *takingJS) CreateConsumer(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	if !j.ran {
		j.ran = true
		j.during()
	}
	return j.JetStream.CreateConsumer(ctx, stream, cfg)
}

func (j *takingJS) Publish(ctx context.Context, subject string, body []byte,
	opts ...jetstream.PublishOpt) (*jetstream.PubAck, error) {

	if subject != barrierSubject(seat.AgentID) {
		j.published = append(j.published, subject)
	}
	return j.JetStream.Publish(ctx, subject, body, opts...)
}

// A SEAT STILL BEING TAKEN IS NOT PUBLISHED BY THE CYCLE.
//
// The seat host counts a seat still being acquired as held, and the cycle
// publishes every held seat: this node's older copies would land over the
// newer rows the hydration had yet to read. The cycle's two calls run here as
// the hydration opens its first replay, on a node new to the seat and on one
// still holding it from before. Once hydrated, the backlog is everything this
// node holds from its first row: an earlier holding's mark would skip rows the
// republish onto a restored or recreated changelog needs.
//
// Mutation: publish with no live holding, begin the holding as the hydration
// begins, or keep a holding it found or its marks, and this goes red.
func TestASeatStillBeingTakenIsNotPublishedByTheCycle(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// heldBefore says this node's syncer still held the seat as its
		// hydration began.
		heldBefore bool
	}{
		{"taken for the first time", false},
		{"taken while a holding from before was still in place", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := broker(t)
			peer := openStore(t)
			seedMemory(t, peer)
			writeNotes(t, peer, 5, 0)
			if _, err := syncerOn(t, peer, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
				t.Fatalf("the peer's publish: %v", err)
			}

			kept := openStore(t)
			seedMemory(t, kept)
			writeNotesUnder(t, kept, "k", 3, 0)
			changelog := jsOn(t, conn)
			s := newSyncer(kept, changelog, func(string) string { return seat.AgentID })
			if tc.heldBefore {
				// THE HOLDING FROM BEFORE, its backlog carried (marking every
				// row) onto a changelog of its own, so the peer's rows stay
				// the newer.
				s.js = &roundTripJS{}
				if err := s.recordBoundary(t.Context(), seat.Handle); err != nil {
					t.Fatalf("record the boundary from before: %v", err)
				}
				if carried, err := s.PublishBacklog(t.Context(), seat.Handle); err != nil ||
					carried.Rows == 0 {
					t.Fatalf("the premise: the backlog from before answered %+v (%v), want "+
						"its rows carried", carried, err)
				}
				s.js = changelog
			}
			var own, backlog Carried
			var ownErr, backlogErr error
			js := &takingJS{JetStream: changelog, during: func() {
				own, ownErr = s.PublishOwn(t.Context(), seat.Handle)
				backlog, backlogErr = s.PublishBacklog(t.Context(), seat.Handle)
			}}
			s.js = js
			if _, err := s.Hydrate(t.Context(), seat.Handle); err != nil {
				t.Fatalf("hydrate: %v", err)
			}
			if !js.ran {
				t.Fatal("the premise: no replay opened, so the cycle's calls never ran")
			}
			if own != (Carried{}) || !errors.Is(ownErr, ErrNotHeld) ||
				backlog != (Carried{}) || !errors.Is(backlogErr, ErrNotHeld) {
				t.Errorf("while the seat was being taken its own pass answered %+v (%v) and its "+
					"backlog %+v (%v), want nothing carried and ErrNotHeld from each",
					own, ownErr, backlog, backlogErr)
			}
			if len(js.published) > 0 {
				t.Errorf("while the seat was being taken the cycle published %s, want nothing: "+
					"this node's copies would replace the newer ones the hydration had yet "+
					"to read", words.AndMore(js.published, 6))
			}

			// AND ONCE THE HYDRATION HAS COMPLETED, the cycle carries
			// everything this node held for the seat as its backlog.
			want := backlogRows(t, kept, s.boundary(seat.Handle))
			if carried, err := s.PublishBacklog(t.Context(), seat.Handle); err != nil ||
				carried.Rows != want {
				t.Errorf("after the hydration the seat's backlog answered %+v (%v), want its "+
					"%d rows carried: none skipped by a mark of the holding before it",
					carried, err, want)
			}
		})
	}
}

// A PUBLISH THAT STARTS AFTER FORGET CARRIES NOTHING.
//
// A cycle walks the seats held as it began, so it can reach one its release
// has flushed and forgotten; with the marks gone it would publish everything
// this node held for the seat over what the next holder wrote since. Here the
// seat is held, written to and released (turned, flushed, forgotten), then a
// cycle's two calls and a flush's reach it.
//
// Mutation: publish a seat with no holding, or flush one whose holding is
// live rather than releasing, and this goes red.
func TestAPublishThatStartsAfterForgetCarriesNothing(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedMemory(t, db)
	held := writeNotesUnder(t, db, "b", 20, 0)
	js := &roundTripJS{}
	s := newSyncer(db, js, func(string) string { return seat.AgentID })
	if err := s.recordBoundary(t.Context(), seat.Handle); err != nil {
		t.Fatalf("record the boundary: %v", err)
	}
	boundary := s.boundary(seat.Handle)
	written := writeNotesUnder(t, db, "a", 10, 0)
	name := names{}.noted(t, held).noted(t, written)
	var wroteRows, heldRows int
	for _, spec := range backlogPassOrder {
		wroteRows += len(stamped(t, db, spec, boundary, sequenceAt(t, db)))
		heldRows += len(stamped(t, db, spec, 0, boundary))
	}

	// THE RELEASE, as the engine makes it.
	if !s.Releasing(seat.Handle) {
		t.Fatal("the premise: Releasing found no live holding to turn")
	}
	own, ownErr := s.FlushOwn(t.Context(), seat.Handle)
	backlog, backlogErr := s.FlushBacklog(t.Context(), seat.Handle)
	if ownErr != nil || backlogErr != nil || own.Marked != wroteRows ||
		backlog.Marked != heldRows {
		t.Fatalf("the release's flush answered %+v (%v) and %+v (%v), want the %d rows the "+
			"holding wrote with its tables sent whole, and the %d it held", own, ownErr,
			backlog, backlogErr, wroteRows, heldRows)
	}
	s.Forget(seat.Handle)
	flushed := len(js.acked)

	pages := 0
	s.beforePage = func(markRange, string) { pages++ }
	for _, call := range []struct {
		name    string
		publish func(context.Context, string) (Carried, error)
	}{
		{"the cycle's own pass", s.PublishOwn},
		{"the cycle's backlog", s.PublishBacklog},
		{"a flush's own pass", s.FlushOwn},
		{"a flush's backlog", s.FlushBacklog},
	} {
		carried, err := call.publish(t.Context(), seat.Handle)
		if carried != (Carried{}) || !errors.Is(err, ErrNotHeld) {
			t.Errorf("%s after the release answered %+v (%v), want nothing carried and "+
				"ErrNotHeld", call.name, carried, err)
		}
	}
	if late := js.acked[flushed:]; len(late) > 0 || pages > 0 {
		t.Errorf("after the release the calls had %s acknowledged and read %d pages, want "+
			"none of either", name.said(late), pages)
	}
	noMarksOf(t, s)
}

// A PUBLISH A FORGET OVERTAKES STOPS AT ITS NEXT ROW AND LEAVES NO MARK.
//
// A cycle takes no seat's lock, so a release can forget a seat mid-publish; a
// call that went on would re-create an ended holding's marks, skipping rows of
// the next holding's backlog. So the holding is checked before each row, and
// the row in flight at the Forget moves no mark. Here 300 notes (a page is
// about 125) and a row of every table, in the seat's range and backlog, with
// the Forget in the tenth row's publish or between pages. A call going on
// rereads its page forever, so the changelog refuses it rows: it fails.
//
// Mutation: check the holding only as the call begins, or once per page or
// per table, or move a mark under a holding that has ended, and this goes red.
func TestAPublishAForgetOvertakesStopsAtItsNextRowAndLeavesNoMark(t *testing.T) {
	t.Parallel()
	for _, of := range []markRange{ownRange, backlogRange} {
		for _, inRow := range []bool{true, false} {
			where := "between two pages"
			if inRow {
				where = "inside a row's publish"
			}
			t.Run(string(of)+" range, "+where, func(t *testing.T) {
				t.Parallel()
				db := openStore(t)
				seedMemory(t, db)
				writeNotes(t, db, 300, noteWidth)
				js := &roundTripJS{most: 300 + len(tables)}
				s := newSyncer(db, js, func(string) string { return seat.AgentID })
				publish := s.PublishOwn
				if of == ownRange {
					heldFromEmpty(s, seat.Handle)
				} else {
					publish = s.PublishBacklog
					if err := s.recordBoundary(t.Context(), seat.Handle); err != nil {
						t.Fatalf("record the boundary: %v", err)
					}
				}
				forgotten := -1
				pages := 0
				if inRow {
					js.on = func(n int) {
						if n == 10 {
							s.Forget(seat.Handle)
							forgotten = n
						}
					}
				} else {
					s.beforePage = func(_ markRange, table string) {
						if table != "agent_diary" {
							return
						}
						if pages++; pages == 2 {
							s.Forget(seat.Handle)
							forgotten = len(js.acked)
						}
					}
				}
				carried, err := publish(t.Context(), seat.Handle)
				if forgotten < 0 {
					t.Fatalf("the premise: the Forget never came, with %d rows acknowledged "+
						"and %d pages read", len(js.acked), pages)
				}
				if !errors.Is(err, ErrNotHeld) || carried.StoppedIn != "agent_diary" {
					t.Errorf("the call answered %+v (%v), want ErrNotHeld in agent_diary", carried,
						err)
				}
				if after := len(js.acked) - forgotten; after > 0 {
					t.Errorf("the call had %d rows acknowledged after the Forget, want none: "+
						"the seat is no longer this node's to publish", after)
				}
				noMarksOf(t, s)
			})
		}
	}
}

// A CYCLE PARTWAY THROUGH A SEAT AT ITS RELEASE PUBLISHES NOTHING AFTER THE
// FLUSH BEGINS.
//
// A cycle that went on past the flush would land its copies of the seat's
// profiles, skills and onboarding marker, read before the flush's, over them.
// The release turns the holding to releasing before its flush, and the cycle
// stops at its next row. Here 300 notes and a row of every table, the release
// inside the cycle's publish of its hundredth row.
//
// Mutation: check the holding only as a call begins, flush under the live
// holding, or let the cycle go on under a releasing one, and this goes red.
func TestACyclePartWayThroughASeatAtItsReleasePublishesNothingAfterTheFlushBegins(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedMemory(t, db)
	ids := writeNotes(t, db, 300, 0)
	numbers := noteNumbers(t, ids)
	js := &roundTripJS{}
	s := syncerOver(db, js)
	var flushOwn, flushBacklog Carried
	var flushErr error
	from, to := -1, -1
	js.on = func(n int) {
		if n != 100 || from >= 0 {
			return
		}
		// THE RELEASE, inside the cycle's publish of its hundredth row: its
		// holding turned, and its flush run whole.
		from = n
		if !s.Releasing(seat.Handle) {
			t.Error("the premise: Releasing found no live holding to turn")
		}
		var ownErr, backlogErr error
		flushOwn, ownErr = s.FlushOwn(t.Context(), seat.Handle)
		flushBacklog, backlogErr = s.FlushBacklog(t.Context(), seat.Handle)
		flushErr = errors.Join(ownErr, backlogErr)
		to = len(js.acked)
	}
	own, ownErr := s.PublishOwn(t.Context(), seat.Handle)
	backlog, backlogErr := s.PublishBacklog(t.Context(), seat.Handle)
	if from < 0 {
		t.Fatalf("the premise: the cycle had %d rows acknowledged, never its hundredth",
			len(js.acked))
	}
	if flushErr != nil || flushOwn.Rows == 0 {
		t.Fatalf("the release's flush answered %+v and %+v (%v), want the seat carried",
			flushOwn, flushBacklog, flushErr)
	}
	var flushedNotes []int
	for _, subject := range js.acked[from:to] {
		if n, ok := numbers[subject]; ok {
			flushedNotes = append(flushedNotes, n)
		}
	}
	// THE ROW IN FLIGHT, the hundredth the cycle sent: seedMemory's note went
	// first.
	inFlight := numbers[js.acked[from-1]]
	if want := span(inFlight, len(ids)); inFlight == 0 || !slices.Equal(flushedNotes, want) {
		t.Errorf("the flush had notes %s acknowledged, want %s: from the row the cycle was "+
			"sending as the release began", runsOf(flushedNotes), runsOf(want))
	}
	if after := js.acked[to:]; len(after) > 0 {
		t.Errorf("the cycle had %d rows acknowledged after the flush began, want none: its "+
			"copies of the seat's whole tables land after the flush's and replace them",
			len(after))
	}
	if own.Rows != 100 || !errors.Is(ownErr, ErrNotHeld) {
		t.Errorf("the cycle's own pass answered %+v (%v), want its 100 rows and ErrNotHeld",
			own, ownErr)
	}
	if backlog != (Carried{}) || !errors.Is(backlogErr, ErrNotHeld) {
		t.Errorf("the cycle's backlog answered %+v (%v), want nothing and ErrNotHeld", backlog,
			backlogErr)
	}
}

// A SEAT WHOSE HYDRATION FAILED FLUSHES NOTHING ON ITS RELEASE.
//
// A failed hydration may not have read the seat's profiles, skills and
// onboarding marker, so a flush would put this node's older copies over a
// peer's newer ones. A failed hydration begins no holding, so Releasing
// answers false and a flush carries nothing; one that completes does, and its
// release flushes. Here the hydration fails as a replay opens, on a node new
// to the seat and on one still holding it from before.
//
// Mutation: begin the holding as a hydration begins or on its failure, leave
// a holding the hydration found in place, or flush with no holding, and this
// goes red.
func TestASeatWhoseHydrationFailedFlushesNothingOnItsRelease(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		heldBefore bool
	}{
		{"taken for the first time", false},
		{"taken while a holding from before was still in place", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := broker(t)
			peer := openStore(t)
			seedMemory(t, peer)
			if _, err := syncerOn(t, peer, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
				t.Fatalf("the peer's publish: %v", err)
			}

			kept := openStore(t)
			seedMemory(t, kept)
			writeNotesUnder(t, kept, "k", 3, 0)
			s := newSyncer(kept, openFailsJS{jsOn(t, conn)}, func(string) string { return seat.AgentID })
			if tc.heldBefore {
				heldFromEmpty(s, seat.Handle)
			}
			if _, err := s.Hydrate(t.Context(), seat.Handle); err == nil {
				t.Fatal("the premise: a hydration whose replay could not open reported success")
			}
			js := &roundTripJS{}
			s.js = js
			if s.Releasing(seat.Handle) {
				t.Error("Releasing turned a holding of a seat whose hydration failed, want none " +
					"to turn")
			}
			own, ownErr := s.FlushOwn(t.Context(), seat.Handle)
			backlog, backlogErr := s.FlushBacklog(t.Context(), seat.Handle)
			if own != (Carried{}) || !errors.Is(ownErr, ErrNotHeld) ||
				backlog != (Carried{}) || !errors.Is(backlogErr, ErrNotHeld) {
				t.Errorf("the release's flush answered %+v (%v) and %+v (%v), want nothing "+
					"carried and ErrNotHeld from each", own, ownErr, backlog, backlogErr)
			}
			if len(js.acked) > 0 {
				t.Errorf("the release's flush had %s acknowledged, want nothing: this node's "+
					"copies would replace the peer's newer ones", words.AndMore(js.acked, 6))
			}
			if b := s.boundary(seat.Handle); b != 0 {
				t.Errorf("the failed hydration left the boundary %d, want none", b)
			}

			// THE CONTROL: the same store, whose hydration completes, flushes
			// on its release.
			s.js = jsOn(t, conn)
			if _, err := s.Hydrate(t.Context(), seat.Handle); err != nil {
				t.Fatalf("the control's hydration: %v", err)
			}
			s.js = js
			if !s.Releasing(seat.Handle) {
				t.Error("the control's Releasing found no live holding to turn")
			}
			if carried, err := s.FlushOwn(t.Context(), seat.Handle); err != nil ||
				carried.Rows == 0 {
				t.Errorf("the control's flush answered %+v (%v), want the seat's tables sent "+
					"whole", carried, err)
			}
		})
	}
}
