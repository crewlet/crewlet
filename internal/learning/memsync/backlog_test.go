package memsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/store"
)

// A SEAT'S BACKLOG, CARRIED A PAGE AT A TIME AND MARKED PER ROW.
//
// The cases publish to a changelog of their own (roundTripJS), so a case
// states each row's round trip, refuses the row it names, or acts inside that
// row's publish. The bound cases run on a synctest bubble's clock, where a
// round trip is exactly publishTrip and a bound cuts at exactly the row the
// arithmetic says. The store is opened and seeded outside the bubble; inside
// it only the case's own goroutine touches it.

// publishTrip is one row's broker round trip on a bubble's clock: a leaf link
// across regions, and a value dividing neither bound, so no row lands on one.
// A row at a time, a cycle then sends 429 rows, the last at 29.96 s, which its
// own budget lets land at 30.03 s; a flush sends 72.
const publishTrip = 70 * time.Millisecond

const (
	// cycleBound and flushBound are the engine's memorySyncInterval and
	// memoryFlushTimeout, which this package cannot import. The cases claim
	// how a backlog relates to them, not the numbers.
	cycleBound = 30 * time.Second
	flushBound = 5 * time.Second

	// perCycle and perFlush are the rows one cycle and one flush have
	// acknowledged at publishTrip a row: every row sent inside the bound, the
	// last landing past it (crewlet/crewlet#1075).
	perCycle = int(cycleBound/publishTrip) + 1
	perFlush = int(flushBound/publishTrip) + 1
)

// noteWidth is a vector's width in float32s: text-embedding-3-small's 1 536,
// 6 KiB stored and 8 KiB as base64, so a page holds about 125 such notes.
const noteWidth = 1536

// roundTripJS is a changelog that acknowledges each row once trip, when set,
// lets it through, recording its subject. on, when set, is called with the
// count acknowledged so far, inside that row's publish, so a case acts at the
// row it names. Embedded rather than stubbed, as openFailsJS is: a publish
// calls nothing else, and anything else is a nil interface's panic. trip and
// on are called with no lock held, so a case whose hooks share state steps its
// syncer ([stepping]); acked is read once the calls have returned, or through
// [roundTripJS.ackedNow] while one runs.
type roundTripJS struct {
	jetstream.JetStream
	trip func(ctx context.Context) error
	on   func(n int)

	mu    sync.Mutex
	acked []string

	// most, when above 0, caps the rows it acknowledges ([errPastMost]), so
	// a call re-reading a page it can no longer mark fails on its own terms,
	// not at the test binary's timeout.
	most int
}

// errPastMost is a roundTripJS's refusal of a row past its most.
var errPastMost = errors.New("the changelog was asked for more rows than the seat holds")

func (j *roundTripJS) Publish(ctx context.Context, subject string, _ []byte,
	_ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {

	if j.most > 0 && len(j.ackedNow()) >= j.most {
		return nil, errPastMost
	}
	if j.trip != nil {
		if err := j.trip(ctx); err != nil {
			return nil, err
		}
	}
	j.mu.Lock()
	j.acked = append(j.acked, subject)
	n := len(j.acked)
	j.mu.Unlock()
	if j.on != nil {
		j.on(n)
	}
	return &jetstream.PubAck{}, nil
}

// Options is the client's own, which a row's budget is counted from.
func (j *roundTripJS) Options() jetstream.JetStreamOptions {
	return jetstream.JetStreamOptions{DefaultTimeout: jsapi.RequestTerm}
}

// ackedNow is every row acknowledged so far, in order, read while a call may
// still be adding to it.
func (j *roundTripJS) ackedNow() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.acked)
}

// syncerOver is a syncer of the seat over db publishing to js, which answers
// publishes only, so it never hydrates: it HOLDS the seat as the node that
// has held it all along ([heldFromEmpty]), and sends a row at a time
// ([stepping]).
func syncerOver(db *store.DB, js jetstream.JetStream) *Syncer {
	return stepping(heldFromEmpty(newSyncer(db, js, func(string) string { return seat.AgentID }),
		seat.Handle))
}

// stepping has s send one row at a time, each answered before the next is
// sent, and answers s: for a case that names the row a call stands at, or
// whose changelog's hooks share state with no lock. The cases of
// pipeline_test.go run the shipped [rowsInFlight].
func stepping(s *Syncer) *Syncer {
	s.inFlight = 1
	return s
}

// registered is the registry's entry for the table named, so a case names a
// table rather than its position in the registry.
func registered(t *testing.T, name string) table {
	t.Helper()
	for _, spec := range tables {
		if spec.name == name {
			return spec
		}
	}
	t.Fatalf("no memory table is named %s", name)
	return table{}
}

// writeNotes writes n diary notes for the seat in one transaction, each with
// a vector of width float32s (none at 0), and answers their ids in stamp
// order. Note i, the i-th written, has the id n<n-i>, zero padded, so the
// stamps rise as the ids fall: an export in key order is not one in stamp
// order.
func writeNotes(t *testing.T, db *store.DB, n, width int) []string {
	t.Helper()
	return writeNotesUnder(t, db, "n", n, width)
}

// writeNotesUnder is [writeNotes] with the ids under prefix rather than n, so
// a case can write notes more than once.
func writeNotesUnder(t *testing.T, db *store.DB, prefix string, n, width int) []string {
	t.Helper()
	var vector, model any
	if width > 0 {
		vector, model = make([]byte, 4*width), "fixture-model"
	}
	ids := make([]string, n)
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range ids {
			ids[i] = fmt.Sprintf("%s%04d", prefix, n-1-i)
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO agent_diary
				(id, agent_id, kind, content, source, created_at, embedding, embedding_model)
				VALUES (?, ?, 'diary_long', ?, 'reflect', 1, ?, ?)`,
				ids[i], seat.AgentID, "note "+ids[i], vector, model); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("write %d notes: %v", n, err)
	}
	return ids
}

// subjectOf is a row's subject on the changelog, by its table's key.
func subjectOf(spec table, id string) string {
	return spec.subject(seat.AgentID, map[string]any{spec.key[0]: id})
}

// noteNumbers maps each note's subject to its 1-based position in ids.
func noteNumbers(t *testing.T, ids []string) map[string]int {
	t.Helper()
	diary := registered(t, "agent_diary")
	numbers := make(map[string]int, len(ids))
	for i, id := range ids {
		numbers[subjectOf(diary, id)] = i + 1
	}
	return numbers
}

// numbersOf is the notes the changelog acknowledged, by number, in order.
func numbersOf(t *testing.T, acked []string, numbers map[string]int) []int {
	t.Helper()
	out := make([]int, 0, len(acked))
	for _, subject := range acked {
		n, ok := numbers[subject]
		if !ok {
			t.Fatalf("the changelog acknowledged %s, which is none of the notes", subject)
		}
		out = append(out, n)
	}
	return out
}

// span is the note numbers from first to last, in order.
func span(first, last int) []int {
	out := make([]int, 0, last-first+1)
	for n := first; n <= last; n++ {
		out = append(out, n)
	}
	return out
}

// runsOf renders note numbers as runs ("1-428, 1-71"): a thousand numbers
// read as nothing.
func runsOf(ns []int) string {
	if len(ns) == 0 {
		return "none"
	}
	var runs []string
	first, last := ns[0], ns[0]
	flush := func() {
		if first == last {
			runs = append(runs, fmt.Sprint(first))
			return
		}
		runs = append(runs, fmt.Sprintf("%d-%d", first, last))
	}
	for _, n := range ns[1:] {
		if n == last+1 {
			last = n
			continue
		}
		flush()
		first, last = n, n
	}
	flush()
	return strings.Join(runs, ", ")
}

// publishFor is one own pass of the seat under bound (exactly bound on a
// bubble's clock): the rows acknowledged in it, in order, and what it
// answered. The seat has no boundary, so every row is its own.
func publishFor(t *testing.T, s *Syncer, js *roundTripJS, bound time.Duration) ([]string, Carried, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()
	from := len(js.acked)
	carried, err := s.PublishOwn(ctx, seat.Handle)
	return slices.Clone(js.acked[from:]), carried, err
}

// A BACKLOG LONGER THAN ONE CYCLE IS CARRIED ACROSS CYCLES, NOT RESTARTED.
//
// A mark per acknowledged row keeps a cut cycle's progress; a mark moved only
// after a whole table would reread and recut the same rows every cycle. 1 000
// notes at publishTrip a row take two cycles and part of a third, each once.
// A cut cycle answers the rows it acknowledged, each marked, and the diary as
// the table it stopped in, so the memory sync sees the backlog draining.
//
// Mutation: move the mark back after the table, to the page's last row at its
// first acknowledgement, or read a page from the mark's own row (>= for >);
// read the page in key order; or count a cut table's rows only once it
// completes: this goes red.
func TestABacklogLongerThanACycleIsCarriedAcrossCyclesNotRestarted(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 1000, 0)
	numbers := noteNumbers(t, ids)
	synctest.Test(t, func(t *testing.T) {
		js := &roundTripJS{trip: func(ctx context.Context) error {
			select {
			case <-time.After(publishTrip):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		s := syncerOver(db, js)
		var cycles [][]int
		var answered []Carried
		done := false
		for !done && len(cycles) < 6 {
			acked, carried, err := publishFor(t, s, js, cycleBound)
			cycles = append(cycles, numbersOf(t, acked, numbers))
			answered = append(answered, carried)
			done = err == nil
		}
		if !done {
			t.Fatalf("after %d cycles the %d notes were still not carried and the diary's "+
				"mark is %d: the last cycle had notes %s acknowledged, so each cycle its "+
				"bound cut started over where the one before it had", len(cycles), len(ids),
				s.mark(seat.Handle, "agent_diary", ownRange), runsOf(cycles[len(cycles)-1]))
		}
		if len(cycles) > 3 {
			t.Fatalf("the %d notes took %d cycles, want 3 at %d a cycle", len(ids), len(cycles), perCycle)
		}
		times := map[int]int{}
		for _, cycle := range cycles {
			for _, n := range cycle {
				times[n]++
			}
		}
		for n := 1; n <= len(ids); n++ {
			if times[n] != 1 {
				t.Fatalf("note %d was acknowledged %d times, want once: the cycles had notes "+
					"%s, then %s", n, times[n], runsOf(cycles[0]), runsOf(slices.Concat(cycles[1:]...)))
			}
		}
		if len(cycles[1]) == 0 || cycles[1][0] != perCycle+1 {
			t.Fatalf("cycle 2 had notes %s acknowledged, want it to begin at note %d, the "+
				"first cycle 1 did not reach", runsOf(cycles[1]), perCycle+1)
		}
		last := len(ids) - 2*perCycle
		wants := []Carried{
			{Rows: perCycle, StoppedIn: "agent_diary"},
			{Rows: perCycle, StoppedIn: "agent_diary"},
			{Rows: last},
		}
		if len(answered) != len(wants) {
			t.Fatalf("the %d notes took %d cycles, want %d at %d a cycle", len(ids), len(answered),
				len(wants), perCycle)
		}
		for i, want := range wants {
			if answered[i] != want {
				t.Errorf("cycle %d answered %+v, want %+v: every note it had acknowledged, "+
					"each of which moved the diary's mark, and, for a cycle its bound cut, "+
					"the diary, which keeps what it carried, as the table it stopped in",
					i+1, answered[i], want)
			}
		}
	})
}

// THE RELEASE FLUSH CONTINUES WHERE THE CYCLES STOPPED.
//
// The flush is one more publish, under a bound of its own, and begins at the
// first row the cycles did not reach, never again at what they had
// acknowledged.
//
// Mutation: move the mark back after the table, and this goes red.
func TestTheReleaseFlushContinuesWhereTheCyclesStopped(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 1000, 0)
	numbers := noteNumbers(t, ids)
	synctest.Test(t, func(t *testing.T) {
		js := &roundTripJS{trip: func(ctx context.Context) error {
			select {
			case <-time.After(publishTrip):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		s := syncerOver(db, js)
		if _, _, err := publishFor(t, s, js, cycleBound); err == nil {
			t.Fatalf("one cycle carried all %d notes; this case needs one its bound cuts", len(ids))
		}
		acked, _, err := publishFor(t, s, js, flushBound)
		if err == nil {
			t.Fatalf("the flush carried the rest of the %d notes; this case needs one its "+
				"bound cuts", len(ids))
		}
		if got, want := numbersOf(t, acked, numbers), span(perCycle+1, perCycle+perFlush); !slices.Equal(got, want) {
			t.Fatalf("the flush had notes %s acknowledged, want %s: the first %d the cycle "+
				"did not reach", runsOf(got), runsOf(want), perFlush)
		}
	})
}

// A ROW WRITTEN AFTER A CUT IS CARRIED WITH THE REST OF THE BACKLOG.
//
// A note the seat writes is stamped above every row before it, so it waits
// behind the backlog, for good if the mark never moves. It is carried, once,
// by the cycle that carries the last of the backlog.
//
// Mutation: move the mark back after the table, and this goes red.
func TestARowWrittenAfterACutIsCarriedWithTheRestOfTheBacklog(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 1000, 0)
	fresh := len(ids) + 1
	numbers := noteNumbers(t, append(slices.Clone(ids), "fresh"))
	synctest.Test(t, func(t *testing.T) {
		js := &roundTripJS{trip: func(ctx context.Context) error {
			select {
			case <-time.After(publishTrip):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		s := syncerOver(db, js)
		if _, _, err := publishFor(t, s, js, cycleBound); err == nil {
			t.Fatalf("one cycle carried all %d notes; this case needs one its bound cuts", len(ids))
		}
		exec(t, db, `INSERT INTO agent_diary (id, agent_id, kind, content, source, created_at)
			VALUES ('fresh', ?, 'diary_long', 'written after the cut', 'reflect', 2)`, seat.AgentID)

		var carriedIn []int
		cycles, done := 1, false
		for !done && cycles < 6 {
			acked, _, err := publishFor(t, s, js, cycleBound)
			cycles++
			for _, n := range numbersOf(t, acked, numbers) {
				if n == fresh {
					carriedIn = append(carriedIn, cycles)
				}
			}
			done = err == nil
		}
		if len(carriedIn) == 0 {
			t.Fatalf("after %d cycles the note written after the first cut was never "+
				"carried: it sorts behind the backlog, and the diary's mark is %d",
				cycles, s.mark(seat.Handle, "agent_diary", ownRange))
		}
		if len(carriedIn) != 1 || carriedIn[0] != 3 {
			t.Fatalf("the note written after the first cut was carried in cycles %s, want "+
				"once, in cycle 3, the one that carried the last of the backlog", runsOf(carriedIn))
		}
	})
}

// A ROW WHOSE PUBLISH FAILED IS WHERE THE NEXT PUBLISH STARTS.
//
// The mark moves past a row only once the changelog acknowledged it, so a
// publish refused at its fifth row keeps the four before it, and the next
// publish starts at the fifth: not the first, which republishes what was
// carried, nor the sixth, which leaves the refused row behind for as long as
// this node holds the seat.
//
// Mutation: move the mark before the publish, back after the table, or to
// the page's last row at its first acknowledgement, and this goes red.
func TestARowWhosePublishFailedIsWhereTheNextPublishStarts(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 10, 0)
	numbers := noteNumbers(t, ids)
	refused := errors.New("the broker refused the row")
	calls := 0
	js := &roundTripJS{trip: func(context.Context) error {
		if calls++; calls == 5 {
			return refused
		}
		return nil
	}}
	s := syncerOver(db, js)
	if _, err := s.PublishOwn(t.Context(), seat.Handle); !errors.Is(err, refused) {
		t.Fatalf("the first publish answered %v, want the refusal of its fifth row", err)
	}
	first := len(js.acked)
	if _, err := s.PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("the next publish: %v", err)
	}
	if got, want := numbersOf(t, js.acked[first:], numbers), span(5, len(ids)); !slices.Equal(got, want) {
		t.Fatalf("the publish after the refusal had notes %s acknowledged, want %s: it begins "+
			"at the refused row", runsOf(got), runsOf(want))
	}
}

// A PUBLISH SKIPS WHAT ANOTHER PUBLISH OF THE SEAT HAD ACKNOWLEDGED.
//
// No call takes a seat's lock, and the mark is reread at each page, so a page
// starts above what another call of the range had acknowledged, and each sends
// again at most the page it already held. The cycle and the flush no longer
// send at once (the flush waits for the cycle's rows), so this guards the
// reread itself: the case runs the second publish whole inside the first's
// publish of its first row.
//
// Mutation: read the mark once per table rather than at each page, and the
// first publish sends the table again from where its first page ended.
func TestAPublishSkipsWhatARacingPublishOfTheSeatHadAcknowledged(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 300, noteWidth)
	numbers := noteNumbers(t, ids)
	js := &roundTripJS{}
	s := syncerOver(db, js)
	// from and to bound the racing publish's acknowledgements in js.acked.
	racing, raced, from, to := false, false, 0, 0
	js.on = func(int) {
		if raced {
			return
		}
		raced, racing, from = true, true, len(js.acked)
		if _, err := s.PublishOwn(t.Context(), seat.Handle); err != nil {
			t.Errorf("the racing publish: %v", err)
		}
		racing, to = false, len(js.acked)
	}
	// second is how many rows had been acknowledged when the first publish
	// read its second page of the diary.
	pages, second := 0, -1
	s.beforePage = func(_ markRange, table string) {
		if racing || table != "agent_diary" {
			return
		}
		if pages++; pages == 2 {
			second = len(js.acked)
		}
	}
	if _, err := s.PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := numbersOf(t, js.acked[from:to], numbers); !slices.Equal(got, span(1, len(ids))) {
		t.Fatalf("the racing publish had notes %s acknowledged, want each of the %d once, in "+
			"order", runsOf(got), len(ids))
	}
	if second < 0 {
		t.Fatalf("the first publish read %d page of the diary; this case needs a second, read "+
			"after the racing publish", pages)
	}
	if got := numbersOf(t, js.acked[to:second], numbers); len(got) == 0 ||
		!slices.Equal(got, span(2, got[len(got)-1])) {
		t.Fatalf("between the racing publish and its own next page, the first publish had notes "+
			"%s acknowledged, want the rest of the page it held, from note 2", runsOf(got))
	}
	if again := js.acked[second:]; len(again) > 0 {
		t.Fatalf("the first publish's pages after the racing one had notes %s acknowledged "+
			"again, want none: the racing publish had all %d acknowledged, and a page starts "+
			"above the seat's mark", runsOf(numbersOf(t, again, numbers)), len(ids))
	}
}

// A PUBLISH HOLDS ONE PAGE OF A BACKLOG, NOT ALL OF IT.
//
// After a hydration the backlog is everything the changelog keeps of the
// seat, so a publish reads a page, sends it and reads the next. A row deleted
// before its page is read is never published, which shows the later pages
// were not already in memory.
//
// Mutation: drop the page's budget, so a page is the whole backlog, and this
// goes red.
func TestAPublishHoldsOnePageOfABacklogNotAllOfIt(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 400, noteWidth)
	numbers := noteNumbers(t, ids)
	gone := ids[len(ids)-1]
	js := &roundTripJS{}
	js.on = func(n int) {
		if n == 1 {
			exec(t, db, "DELETE FROM agent_diary WHERE id = ?", gone)
		}
	}
	s := syncerOver(db, js)
	if _, err := s.PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got, want := numbersOf(t, js.acked, numbers), span(1, len(ids)-1); !slices.Equal(got, want) {
		t.Fatalf("the publish had notes %s acknowledged, want %s: note %d was deleted at the "+
			"first acknowledgement, before its page was read", runsOf(got), runsOf(want), len(ids))
	}
}

// A ROW LARGER THAN A PAGE IS NEVER LEFT BEHIND.
//
// An episode keeps its ask whole, so one row can exceed a page's budget. A
// page takes the row that passes its budget and stops after it, so a page is
// never empty: one that stopped short would read the same nothing for ever,
// stranding every row after it. The case watches the episodes' pages through
// the syncer's seam and stops a publish that reads a page after an empty one
// that said there were more, so it fails here, not at the binary's timeout.
//
// Mutation: stop a page before the row that passes its budget, and this goes
// red.
func TestARowLargerThanAPageIsNeverLeftBehind(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	episodes := registered(t, "episodes")
	for i, ask := range []string{"a short ask", strings.Repeat("a", 2<<20), "another short ask"} {
		exec(t, db, `INSERT INTO episodes (id, agent_handle, agent_role, turn_id,
			started_at, ended_at, plan_summary, task_summary, tool_sequence,
			review_outcome, duration_ms, kind, ask)
			VALUES (?, ?, 'Engineer', 't1', 1, 1, 'p', 't', '[]', 'done', 1, 'raw', ?)`,
			fmt.Sprintf("e%d", i+1), seat.Handle, ask)
	}
	js := &roundTripJS{}
	s := syncerOver(db, js)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	// read is the rows acknowledged when the episodes' last page was read;
	// none acknowledged since means the page before took no row.
	read, stuck := -1, false
	s.beforePage = func(_ markRange, table string) {
		if table != "episodes" {
			return
		}
		if len(js.acked) == read {
			stuck = true
			stop()
		}
		read = len(js.acked)
	}
	_, err := s.PublishOwn(ctx, seat.Handle)
	if stuck {
		t.Fatalf("a page of the episodes took no row and said there were more, so the publish "+
			"read it again with %d of the 3 episodes acknowledged: a page must take the row that "+
			"passes its budget, the 2 MiB ask, or that row and every one after it are left "+
			"behind", len(js.acked))
	}
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	want := []string{subjectOf(episodes, "e1"), subjectOf(episodes, "e2"), subjectOf(episodes, "e3")}
	if !slices.Equal(js.acked, want) {
		t.Fatalf("the publish had %d rows acknowledged, want the 3 episodes, each once and "+
			"in the order they were written: the second's ask is 2 MiB, twice a page", len(js.acked))
	}
}

// A PUBLISH SAYS WHAT IT CARRIED AND WHERE IT STOPPED.
//
// The answer is how the memory sync tells a draining backlog from a sync that
// moves nothing: every row acknowledged, each of which moved a mark, and, for
// a publish that stopped short, the table it stopped in. With one row in each
// table, the first publish carries six, the second nothing, and the third,
// refused at the profile changed since, stops in the profiles.
//
// Mutation: count a row before it is acknowledged, or name no table a refused
// publish stopped in, and this goes red.
func TestAPublishSaysWhatItCarriedAndWhereItStopped(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedMemory(t, db)
	refused := errors.New("the broker refused the row")
	refuse := false
	js := &roundTripJS{trip: func(context.Context) error {
		if refuse {
			return refused
		}
		return nil
	}}
	s := syncerOver(db, js)

	first, err := s.PublishOwn(t.Context(), seat.Handle)
	if want := (Carried{Rows: len(tables)}); err != nil || first != want {
		t.Fatalf("the first publish answered %+v (%v), want %+v: the seed's row of every table "+
			"acknowledged", first, err, want)
	}
	second, err := s.PublishOwn(t.Context(), seat.Handle)
	if err != nil || second != (Carried{}) {
		t.Fatalf("the second publish answered %+v (%v), want nothing: nothing changed",
			second, err)
	}
	exec(t, db, `UPDATE counterparty_profiles SET traits = '{"prefers":"a call"}'
		WHERE observer_handle = ?`, seat.Handle)
	refuse = true
	third, err := s.PublishOwn(t.Context(), seat.Handle)
	if want := (Carried{StoppedIn: "counterparty_profiles"}); !errors.Is(err, refused) || third != want {
		t.Fatalf("the publish refused at its first row answered %+v (%v), want %+v and the "+
			"refusal", third, err, want)
	}
}
