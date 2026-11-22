package memsync

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// ROWS IN FLIGHT, THE MARK AT THE ACKNOWLEDGED PREFIX.
//
// A publish has up to rowsInFlight rows sent and not yet answered, and moves a
// range's mark only past rows acknowledged in the order it sent them. The
// cases run the shipped rowsInFlight on a bubble's clock against pacedJS,
// which counts what is out at once.

// pacedJS is a changelog whose every row is answered once trip, and its bytes'
// crossing at pace, have passed on the case's clock and answer, when set, lets
// it through; or with the row's context's error once that ends first. It
// counts the rows out at once and their bytes as a link's window counts them
// (jsapi.RequestBytes), and records each row asked and each acknowledged, in
// the order asked and acknowledged. Every field it counts is under mu, so it carries any number of
// rows at once.
type pacedJS struct {
	jetstream.JetStream
	trip   time.Duration
	pace   time.Duration
	answer func(ctx context.Context, subject string) error

	mu                    sync.Mutex
	out, outBytes         int
	mostOut, mostOutBytes int
	asked, landed         []string
}

func (j *pacedJS) Options() jetstream.JetStreamOptions {
	return jetstream.JetStreamOptions{DefaultTimeout: jsapi.RequestTerm}
}

func (j *pacedJS) Publish(ctx context.Context, subject string, data []byte,
	_ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {

	bytes := jsapi.RequestBytes(len(data))
	j.mu.Lock()
	j.asked = append(j.asked, subject)
	j.out++
	j.outBytes += bytes
	j.mostOut, j.mostOutBytes = max(j.mostOut, j.out), max(j.mostOutBytes, j.outBytes)
	j.mu.Unlock()
	err := j.wait(ctx, subject, jsapi.Sending(len(data), j.pace))
	j.mu.Lock()
	defer j.mu.Unlock()
	j.out--
	j.outBytes -= bytes
	if err != nil {
		return nil, err
	}
	j.landed = append(j.landed, subject)
	return &jetstream.PubAck{Stream: topics.MemoryStream}, nil
}

// wait is one row's round trip and crossing, then its answer.
func (j *pacedJS) wait(ctx context.Context, subject string, crossing time.Duration) error {
	if took := j.trip + crossing; took > 0 {
		answered := time.NewTimer(took) //fixedwait:simulated a row's round trip and crossing on a bubble's clock
		defer answered.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-answered.C:
		}
	}
	if j.answer != nil {
		return j.answer(ctx, subject)
	}
	return nil
}

// outNow is how many rows are out now.
func (j *pacedJS) outNow() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.out
}

// landedNow is every row acknowledged so far, in the order acknowledged.
func (j *pacedJS) landedNow() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.landed)
}

// askedNow is every row asked so far, in the order asked.
func (j *pacedJS) askedNow() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.asked)
}

// most is the most rows, and the most bytes, that were ever out at once.
func (j *pacedJS) most() (rows, bytes int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.mostOut, j.mostOutBytes
}

// heldOver is a syncer of the seat over db publishing to js with the shipped
// rowsInFlight, holding the seat as the node that has held it all along
// ([heldFromEmpty]).
func heldOver(db *store.DB, js jetstream.JetStream) *Syncer {
	return heldFromEmpty(newSyncer(db, js, func(string) string { return seat.AgentID }), seat.Handle)
}

// gatedJS is a pacedJS that answers no row until opened is called, or until
// the row's context ends.
func gatedJS() (js *pacedJS, opened func()) {
	gate := make(chan struct{})
	return &pacedJS{answer: func(ctx context.Context, _ string) error {
		select {
		case <-gate:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}, sync.OnceFunc(func() { close(gate) })
}

// sortedNumbers is the notes acked names, by number, sorted: the rows a
// publish has out at once are answered in any order.
func sortedNumbers(t *testing.T, acked []string, numbers map[string]int) []int {
	t.Helper()
	return slices.Sorted(slices.Values(numbersOf(t, acked, numbers)))
}

// A FLUSH CARRIES ROWS AS FAST AS THE LINK DOES, NOT ONE A ROUND TRIP.
//
// One row a round trip carried a flush's five seconds some seventy rows at publishTrip,
// whatever the link's rate, so a burst the seat wrote just before a release,
// larger than that, was lost to its next holder (crewlet/crewlet#1041). With
// rowsInFlight out at once, 1 000 notes are carried well inside the bound, and
// never more than rowsInFlight are out.
//
// Mutation: send one row at a time, or put no bound on the rows out, and this
// goes red.
func TestAFlushCarriesRowsAsFastAsTheLinkNotOneARoundTrip(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 1000, 0)
	synctest.Test(t, func(t *testing.T) {
		js := &pacedJS{trip: publishTrip}
		s := heldOver(db, js)
		if !s.Releasing(seat.Handle) {
			t.Fatal("the premise: Releasing found no live holding to turn")
		}
		ctx, cancel := context.WithTimeout(t.Context(), flushBound)
		defer cancel()
		carried, err := s.FlushOwn(ctx, seat.Handle)
		if err != nil || carried.Rows != len(ids) {
			t.Fatalf("the flush answered %+v (%v), want all %d notes inside its %s: at %s a round "+
				"trip, one row at a time carries about %d", carried, err, len(ids), flushBound,
				publishTrip, int(flushBound/publishTrip))
		}
		if rows, _ := js.most(); rows != rowsInFlight {
			t.Errorf("the flush had at most %d rows out at once, want %d", rows, rowsInFlight)
		}
	})
}

// AN ACKNOWLEDGEMENT LOST MID-WINDOW LEAVES THE MARK AT THE ACKNOWLEDGED PREFIX.
//
// Rows out together are answered in any order, so a row acknowledged after one
// that failed must not move the mark past it: the failed row would be skipped
// for as long as this node holds the seat. Here note 10 is refused only once
// twenty notes after it have been acknowledged. The publish answers the nine
// before it, the mark stands at note 9, and the next publish sends the rest
// from note 10, each once.
//
// Mutation: move the mark to the highest acknowledgement, count a row
// acknowledged after the failure, or send one row at a time, and this goes red.
func TestAnAcknowledgementLostMidWindowLeavesTheMarkAtTheAcknowledgedPrefix(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 100, 0)
	numbers := noteNumbers(t, ids)
	stamps := stampsOf(t, db)
	synctest.Test(t, func(t *testing.T) {
		refused := errors.New("the broker refused the row")
		later := make(chan struct{})
		var after atomic.Int32
		refusing := true
		js := &pacedJS{answer: func(ctx context.Context, subject string) error {
			if !refusing {
				return nil
			}
			switch n := numbers[subject]; {
			case n == 10:
				select {
				case <-later:
				case <-ctx.Done():
				}
				return refused
			case n > 10 && after.Add(1) == 20:
				close(later)
			}
			return nil
		}}
		s := heldOver(db, js)
		ctx, cancel := context.WithTimeout(t.Context(), cycleBound)
		defer cancel()
		carried, err := s.PublishOwn(ctx, seat.Handle)
		first := js.landedNow()
		if n := after.Load(); n < 20 {
			t.Fatalf("%d notes after note 10 were acknowledged while it was out, want 20 or more: "+
				"rows after it go out before it is answered", n)
		}
		if want := (Carried{Rows: 9, StoppedIn: "agent_diary"}); !errors.Is(err, refused) ||
			carried != want {
			t.Errorf("the publish answered %+v (%v), want %+v and the refusal: the nine notes "+
				"before the refused one, and none acknowledged after it", carried, err, want)
		}
		if got, want := s.mark(seat.Handle, "agent_diary", ownRange), stamps[subjectOf(registered(t,
			"agent_diary"), ids[8])]; got != want {
			t.Errorf("the diary's mark is %d, want %d, note 9's: the last note acknowledged "+
				"before the refused one", got, want)
		}

		refusing = false
		carried, err = s.PublishOwn(ctx, seat.Handle)
		if err != nil || carried != (Carried{Rows: len(ids) - 9}) {
			t.Fatalf("the next publish answered %+v (%v), want the %d notes from note 10",
				carried, err, len(ids)-9)
		}
		if got := sortedNumbers(t, js.landedNow()[len(first):], numbers); !slices.Equal(got,
			span(10, len(ids))) {
			t.Errorf("the next publish had notes %s acknowledged, want %s, each once",
				runsOf(got), runsOf(span(10, len(ids))))
		}
	})
}

// A PUBLISH ACROSS A LINK HOLDS ITS ROWS IN FLIGHT TO THE LINK'S WINDOW.
//
// What a node sends across its link queues ahead of every request it sends
// after, a lease renewal among them, each held to a five-second term; so the
// rows a publish has out take their bytes from the link's one window
// (ADR-0161), as the replay's pulls and the object store's transfers do. Here
// 200 notes with their vectors, some 9 KiB a row: a dozen or so fit the
// window, never the rowsInFlight that small rows would.
//
// Mutation: take nothing from the window, or give a row's bytes back before
// it is answered, and this goes red.
func TestAPublishAcrossALinkHoldsItsRowsInFlightToTheLinksWindow(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 200, noteWidth)
	synctest.Test(t, func(t *testing.T) {
		js := &pacedJS{trip: publishTrip}
		s := heldFromEmpty(newSyncerAcross(db, js, func(string) string { return seat.AgentID }),
			seat.Handle)
		carried, err := s.PublishOwn(t.Context(), seat.Handle)
		if err != nil || carried.Rows != len(ids) {
			t.Fatalf("the publish answered %+v (%v), want its %d notes", carried, err, len(ids))
		}
		rows, bytes := js.most()
		if rows < 2 || bytes > jsapi.LinkWindow {
			t.Errorf("the publish had at most %d rows of %d bytes out at once, want more than one "+
				"and no more than the link's window of %d bytes", rows, bytes, jsapi.LinkWindow)
		}
		if held, waiting := s.window.Held(); held != 0 || waiting != 0 {
			t.Errorf("after the publish the window holds %d bytes with %d takes waiting, want "+
				"every row's bytes given back", held, waiting)
		}
	})
}

// A FLUSH STARTS ONCE EVERY ROW THE CYCLE HAD OUT IS ANSWERED.
//
// A cycle partway through a seat as its release begins has up to rowsInFlight
// rows out, read before the release; one landing after the flush's copy of
// the same row would replace it. So the flush starts only once each of them
// is answered and marked, and then past them. Here the cycle's rows are held
// until the release has begun and the flush has been asked.
//
// Mutation: start the flush before the cycle's rows are answered, or mark a
// row after counting it answered, and this goes red.
func TestAFlushStartsOnceEveryRowTheCycleHadOutIsAnswered(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 300, 0)
	numbers := noteNumbers(t, ids)
	synctest.Test(t, func(t *testing.T) {
		js, opened := gatedJS()
		defer opened()
		s := heldOver(db, js)
		var cycle, flush Carried
		var cycleErr, flushErr error
		cycled, flushed := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(cycled)
			cycle, cycleErr = s.PublishOwn(t.Context(), seat.Handle)
		}()
		synctest.Wait()
		if out := js.outNow(); out != rowsInFlight {
			t.Fatalf("the cycle has %d rows out, want %d", out, rowsInFlight)
		}
		if !s.Releasing(seat.Handle) {
			t.Fatal("the premise: Releasing found no live holding to turn")
		}
		go func() {
			defer close(flushed)
			var backlogErr error
			flush, flushErr = s.FlushOwn(t.Context(), seat.Handle)
			_, backlogErr = s.FlushBacklog(t.Context(), seat.Handle)
			flushErr = errors.Join(flushErr, backlogErr)
		}()
		synctest.Wait()
		if out, landed := js.outNow(), len(js.landedNow()); out != rowsInFlight || landed != 0 {
			t.Errorf("with the cycle's %d rows out the flush brought the rows out to %d, %d "+
				"landed, want none of its own: its copies would land beside the cycle's",
				rowsInFlight, out, landed)
		}
		opened()
		<-cycled
		<-flushed

		if cycle.Rows != rowsInFlight || !errors.Is(cycleErr, ErrNotHeld) {
			t.Errorf("the cycle answered %+v (%v), want its %d rows and ErrNotHeld", cycle,
				cycleErr, rowsInFlight)
		}
		if flushErr != nil || flush.Rows != len(ids)-rowsInFlight {
			t.Errorf("the flush answered %+v (%v), want the %d notes past the cycle's", flush,
				flushErr, len(ids)-rowsInFlight)
		}
		landed := js.landedNow()
		if len(landed) != len(ids) {
			t.Fatalf("%d rows landed, want each of the %d notes once", len(landed), len(ids))
		}
		if got := sortedNumbers(t, landed[:rowsInFlight], numbers); !slices.Equal(got,
			span(1, rowsInFlight)) {
			t.Errorf("the first %d rows to land were notes %s, want the cycle's, %s", rowsInFlight,
				runsOf(got), runsOf(span(1, rowsInFlight)))
		}
		if got := sortedNumbers(t, landed[rowsInFlight:], numbers); !slices.Equal(got,
			span(rowsInFlight+1, len(ids))) {
			t.Errorf("after the cycle's rows the flush landed notes %s, want %s", runsOf(got),
				runsOf(span(rowsInFlight+1, len(ids))))
		}
	})
}

// A ROW SENT INSIDE THE BOUND IS CARRIED WHOLE, NOT CUT AND SENT AGAIN.
//
// A row was published under its call's bound, so one a slow link carries in
// longer than the bound left was cut while still crossing, sent again whole by
// the next call, and cut again, every row after it waiting
// (crewlet/crewlet#1075). Here two notes of a mebibyte across a link at its
// floor, thirty seconds each, and a cycle that reaches the seat with five
// seconds left: it sends the first, which lands under its own budget, and
// nothing after it; the next call carries the second, not the first again.
//
// Mutation: publish a row under its call's context, or send a row once the
// call's bound has run out, and this goes red.
func TestARowSentInsideTheBoundIsCarriedWholeNotCutAndSentAgain(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	writeNotesOfSize(t, db, 2, 1<<20)
	diary := registered(t, "agent_diary")
	first, second := subjectOf(diary, "p0000"), subjectOf(diary, "p0001")
	stamps := stampsOf(t, db)
	synctest.Test(t, func(t *testing.T) {
		js := &pacedJS{trip: publishTrip, pace: objstore.MiBPace}
		s := heldFromEmpty(newSyncerAcross(db, js, func(string) string { return seat.AgentID }),
			seat.Handle)
		late, cancel := context.WithTimeout(t.Context(), flushBound)
		defer cancel()
		carried, err := s.PublishOwn(late, seat.Handle)
		if want := (Carried{Rows: 1, StoppedIn: "agent_diary"}); err == nil || carried != want {
			t.Errorf("the cycle answered %+v (%v), want %+v and its bound: the first note "+
				"carried, nothing sent after its bound", carried, err, want)
		}
		if asked := js.askedNow(); !slices.Equal(asked, []string{first}) {
			t.Errorf("the cycle sent %d rows, want the first note alone", len(asked))
		}
		if got, want := s.mark(seat.Handle, "agent_diary", ownRange), stamps[first]; got != want {
			t.Fatalf("the diary's mark is %d after the cycle, want %d, the first note's: a row "+
				"that crosses in thirty seconds lands though five were left", got, want)
		}

		carried, err = s.PublishOwn(t.Context(), seat.Handle)
		if err != nil || carried != (Carried{Rows: 1}) {
			t.Errorf("the next call answered %+v (%v), want the second note", carried, err)
		}
		if asked := js.askedNow(); !slices.Equal(asked, []string{first, second}) {
			t.Errorf("the two calls sent %d rows, want each note once", len(asked))
		}
	})
}

// A CYCLE'S ROW WAITING FOR THE LINK'S WINDOW IS SENT BY NOBODY ONCE THE
// RELEASE BEGINS.
//
// Across a link a cycle's next row can wait for the window behind the rows it
// has out. The release wakes it, refused, rather than leave it there until the
// cycle's bound; the flush waits for the row the cycle has out, then sends the
// rest itself, each once. Here each note is larger than the window, so the
// cycle's second waits for its first.
//
// Mutation: leave the cycle's row waiting for the window as the release
// begins, or start the flush before the cycle's row is answered, and this goes
// red.
func TestACyclesRowWaitingForTheLinksWindowIsSentByNobodyOnceTheReleaseBegins(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	notes := wideNotes(t, db)
	synctest.Test(t, func(t *testing.T) {
		js, opened := gatedJS()
		defer opened()
		s := heldFromEmpty(newSyncerAcross(db, js, func(string) string { return seat.AgentID }),
			seat.Handle)
		var cycle, flush Carried
		var cycleErr, flushErr error
		cycled, flushed := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(cycled)
			cycle, cycleErr = s.PublishOwn(t.Context(), seat.Handle)
		}()
		synctest.Wait()
		if _, waiting := s.window.Held(); waiting != 1 || !slices.Equal(js.askedNow(), notes[:1]) {
			t.Fatalf("the premise: the cycle sent %d notes with %d takes waiting for the window, "+
				"want its first out and its second waiting", len(js.askedNow()), waiting)
		}
		if !s.Releasing(seat.Handle) {
			t.Fatal("the premise: Releasing found no live holding to turn")
		}
		go func() {
			defer close(flushed)
			flush, flushErr = s.FlushOwn(t.Context(), seat.Handle)
		}()
		synctest.Wait()
		if _, waiting := s.window.Held(); waiting != 0 {
			t.Errorf("after the release began %d takes still wait for the window, want the "+
				"cycle's row woken and refused", waiting)
		}
		if asked := js.askedNow(); !slices.Equal(asked, notes[:1]) {
			t.Errorf("with the cycle's first note out, %d notes were sent, want that one alone: "+
				"the flush waits for it", len(asked))
		}
		opened()
		<-cycled
		<-flushed

		if want := (Carried{Rows: 1, StoppedIn: "agent_diary"}); !errors.Is(cycleErr, ErrNotHeld) ||
			cycle != want {
			t.Errorf("the cycle answered %+v (%v), want %+v and ErrNotHeld", cycle, cycleErr, want)
		}
		if flushErr != nil || flush != (Carried{Rows: 2}) {
			t.Errorf("the flush answered %+v (%v), want the two notes past the cycle's", flush,
				flushErr)
		}
		if asked := js.askedNow(); !slices.Equal(asked, notes) {
			t.Errorf("%d notes were sent, want each of the %d once, in order", len(asked),
				len(notes))
		}
		if held, waiting := s.window.Held(); held != 0 || waiting != 0 {
			t.Errorf("the window holds %d bytes with %d takes waiting, want every row's bytes "+
				"given back", held, waiting)
		}
	})
}

// A ROW LET OUT OF THE LINK'S WINDOW IS SENT ONLY UNDER THE HOLDING IT WAS
// READ UNDER.
//
// A row waits for the window as long as the rows ahead of it take to cross, so
// whether the holding still lets it be sent is asked again once the window
// lets it out. Here the seat is forgotten, as a new hydration of it begins,
// while the cycle's second note waits: it is refused and its bytes given back.
//
// Mutation: send a row the window let out without asking again, or keep the
// bytes of one refused there, and this goes red.
func TestARowLetOutOfTheLinksWindowIsSentOnlyUnderTheHoldingItWasReadUnder(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	notes := wideNotes(t, db)
	synctest.Test(t, func(t *testing.T) {
		js, opened := gatedJS()
		defer opened()
		s := heldFromEmpty(newSyncerAcross(db, js, func(string) string { return seat.AgentID }),
			seat.Handle)
		var cycle Carried
		var cycleErr error
		cycled := make(chan struct{})
		go func() {
			defer close(cycled)
			cycle, cycleErr = s.PublishOwn(t.Context(), seat.Handle)
		}()
		synctest.Wait()
		if _, waiting := s.window.Held(); waiting != 1 {
			t.Fatalf("the premise: %d takes wait for the window, want the cycle's second note's",
				waiting)
		}
		s.Forget(seat.Handle)
		opened()
		<-cycled

		if want := (Carried{Rows: 1, StoppedIn: "agent_diary"}); !errors.Is(cycleErr, ErrNotHeld) ||
			cycle != want {
			t.Errorf("the cycle answered %+v (%v), want %+v and ErrNotHeld", cycle, cycleErr, want)
		}
		if asked := js.askedNow(); !slices.Equal(asked, notes[:1]) {
			t.Errorf("%d notes were sent, want the first alone: the seat was forgotten while the "+
				"second waited for the window", len(asked))
		}
		if held, waiting := s.window.Held(); held != 0 || waiting != 0 {
			t.Errorf("the window holds %d bytes with %d takes waiting, want every row's bytes "+
				"given back", held, waiting)
		}
	})
}

// A FLUSH WHOSE BOUND ENDS BEFORE THE CYCLE'S ROWS ARE ANSWERED SENDS NOTHING.
//
// The flush waits for the rows the cycle had out inside its own bound, so a
// cycle's row still crossing a slow link spends it. Here a note of a mebibyte,
// thirty seconds at the floor, is out as the release begins: the five-second
// flush answers its bound at five seconds, carrying nothing, and the note lands
// under its own budget.
//
// Mutation: wait for the cycle's rows past the flush's bound, and this goes red.
func TestAFlushWhoseBoundEndsBeforeTheCyclesRowsAreAnsweredSendsNothing(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	writeNotesOfSize(t, db, 2, 1<<20)
	first := subjectOf(registered(t, "agent_diary"), "p0000")
	synctest.Test(t, func(t *testing.T) {
		js := &pacedJS{trip: publishTrip, pace: objstore.MiBPace}
		s := heldFromEmpty(newSyncerAcross(db, js, func(string) string { return seat.AgentID }),
			seat.Handle)
		var cycle Carried
		var cycleErr error
		cycled := make(chan struct{})
		go func() {
			defer close(cycled)
			cycle, cycleErr = s.PublishOwn(t.Context(), seat.Handle)
		}()
		synctest.Wait()
		if !s.Releasing(seat.Handle) {
			t.Fatal("the premise: Releasing found no live holding to turn")
		}
		ctx, cancel := context.WithTimeout(t.Context(), flushBound)
		defer cancel()
		start := time.Now()
		flush, err := s.FlushOwn(ctx, seat.Handle)
		if took := time.Since(start); !errors.Is(err, context.DeadlineExceeded) ||
			flush != (Carried{}) || took != flushBound {
			t.Errorf("the flush answered %+v (%v) after %s, want nothing carried and its bound "+
				"at %s", flush, err, took, flushBound)
		}
		<-cycled

		if want := (Carried{Rows: 1, StoppedIn: "agent_diary"}); !errors.Is(cycleErr, ErrNotHeld) ||
			cycle != want {
			t.Errorf("the cycle answered %+v (%v), want %+v and ErrNotHeld", cycle, cycleErr, want)
		}
		if asked := js.askedNow(); !slices.Equal(asked, []string{first}) {
			t.Errorf("%d notes were sent, want the cycle's first alone", len(asked))
		}
	})
}

// wideNotes writes three diary notes for the seat, each larger than a link's
// window, and answers their subjects in stamp order: across a link each row
// crosses alone, so a cycle's second waits for its first.
func wideNotes(t *testing.T, db *store.DB) []string {
	t.Helper()
	writeNotesOfSize(t, db, 3, jsapi.LinkWindow)
	diary := registered(t, "agent_diary")
	return []string{subjectOf(diary, "p0000"), subjectOf(diary, "p0001"),
		subjectOf(diary, "p0002")}
}
