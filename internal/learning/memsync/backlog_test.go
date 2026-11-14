package memsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/store"
)

// A SEAT'S BACKLOG, CARRIED A PAGE AT A TIME AND MARKED PER ROW (#1033).
//
// The cases below publish to a changelog of their own (roundTripJS), so a case
// states each row's round trip, refuses the row it names, or acts inside the
// publish of the row it names. The ones about a bound run on a synctest
// bubble's clock, where a round trip is exactly publishTrip and a bound cuts a
// publish at exactly the row the arithmetic says. The store is opened and
// seeded outside the bubble, and inside it only the case's own goroutine
// reads and writes it.

// publishTrip is one row's broker round trip in the cases on a bubble's clock:
// a leaf link across regions, the deployment #1033 reports, and a value that
// divides neither bound, so no row lands on one. A cycle then has 428 rows
// acknowledged, the 429th landing at 30.03 s, and a flush 71.
const publishTrip = 70 * time.Millisecond

const (
	// cycleBound is the memory sync's cycle and flushBound its release
	// flush: the engine's memorySyncInterval and memoryFlushTimeout, which
	// this package cannot import. The cases claim how a backlog relates to
	// them, not the numbers.
	cycleBound = 30 * time.Second
	flushBound = 5 * time.Second

	// perCycle and perFlush are the rows one cycle and one flush have
	// acknowledged at publishTrip a row.
	perCycle = int(cycleBound / publishTrip)
	perFlush = int(flushBound / publishTrip)
)

// noteWidth is a vector's width in float32s: text-embedding-3-small's 1 536,
// 6 KiB stored and 8 KiB carried as base64, so a page holds about 125 notes
// that carry one.
const noteWidth = 1536

// roundTripJS is a changelog that acknowledges each row it is handed once
// trip, when set, lets it through, and records the row's subject. on, when
// set, is called with the count acknowledged so far, inside the publish of
// the row that made it, so a case acts at the row it names.
//
// Embedded rather than stubbed, as openFailsJS is: a publish of a seat calls
// nothing else, and anything else called on it is a nil interface's panic.
type roundTripJS struct {
	jetstream.JetStream
	trip  func(ctx context.Context) error
	acked []string
	on    func(n int)
}

func (j *roundTripJS) Publish(ctx context.Context, subject string, _ []byte,
	_ ...jetstream.PublishOpt) (*jetstream.PubAck, error) {

	if j.trip != nil {
		if err := j.trip(ctx); err != nil {
			return nil, err
		}
	}
	j.acked = append(j.acked, subject)
	if j.on != nil {
		j.on(len(j.acked))
	}
	return &jetstream.PubAck{}, nil
}

// syncerOver is a syncer of the seat over db whose changelog is js, which
// answers publishes and nothing else, so it publishes and never hydrates.
func syncerOver(db *store.DB, js jetstream.JetStream) *Syncer {
	return New(db, js, func(string) string { return seat.AgentID })
}

// registered is the registry's entry for the table named, so a case says
// which table it means rather than where the registry lists it.
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
// a vector of width float32s (none at 0), and answers their ids in the order
// they were stamped. The i-th written is note i, under the id n<n-i>, zero
// padded, so the stamps rise as the ids fall: an export in key order is not
// one in stamp order.
func writeNotes(t *testing.T, db *store.DB, n, width int) []string {
	t.Helper()
	return writeNotesUnder(t, db, "n", n, width)
}

// writeNotesUnder is [writeNotes] with the ids under prefix rather than n, so
// a case writes notes more than once.
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

// subjectOf is a row's subject on the changelog, by the id its table is keyed
// on.
func subjectOf(spec table, id string) string {
	return spec.subject(seat.AgentID, map[string]any{spec.key[0]: id})
}

// noteNumbers maps each note's subject on the changelog to its number in the
// order ids lists them, from 1.
func noteNumbers(t *testing.T, ids []string) map[string]int {
	t.Helper()
	diary := registered(t, "agent_diary")
	numbers := make(map[string]int, len(ids))
	for i, id := range ids {
		numbers[subjectOf(diary, id)] = i + 1
	}
	return numbers
}

// numbersOf is the notes the changelog acknowledged, by number, in the order
// it acknowledged them.
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

// runsOf says which notes were acknowledged, as runs in the order they were
// ("1-428, 1-71"): a thousand numbers read as nothing.
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

// publishFor is one own pass of the seat under bound, which on a bubble's
// clock is exactly bound: the rows the changelog acknowledged in it, in
// order, and what it answered. The seat here has no boundary, so every row is
// its own, as every row was a publish's before the boundary.
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
// The mark moved only once a table was published whole (#1033), so a cycle
// its bound cut kept nothing of what the changelog had acknowledged: the next
// cycle read the same rows above the same mark and was cut at the same row,
// for as long as the backlog outlasted a cycle. A mark per acknowledged row
// keeps each cycle's progress, and 1 000 notes at publishTrip a row are
// carried in two cycles and part of a third, each note once. Each cycle says
// so in what it answers, which is how the memory sync tells a backlog
// draining from a cycle that carried nothing that stays carried: a cut cycle
// answers the rows it had acknowledged, each of which moved the diary's mark,
// and the diary as the table it stopped in, which keeps what it carried.
//
// Mutation: move the mark back after the table, to the page's last row at
// its first acknowledgement, or read a page from the mark's own row (>= for
// >); read the page in key order; or count a cut table's marked rows only
// once it completes, or call every table a cut stopped in a whole one: this
// goes red.
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
			{Rows: perCycle, Marked: perCycle, StoppedIn: "agent_diary"},
			{Rows: perCycle, Marked: perCycle, StoppedIn: "agent_diary"},
			{Rows: last, Marked: last},
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
// The flush is one more publish, under a bound of its own. With the mark
// moving only after a whole table it began at the mark the cycles had left
// unmoved, and was cut inside the same prefix they were (#1033), carrying
// again what they had already had acknowledged. It begins at the first row
// they did not reach.
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
// behind the backlog, and behind a mark that never moved it waited for good:
// nothing a seat learned on a node whose backlog outlasted a cycle reached
// the changelog (#1033). It is carried, once, by the cycle that carries the
// last of the backlog.
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
// The mark moves past a row only once the changelog has acknowledged it, so
// a publish the broker refused at its fifth row keeps the four before it and
// nothing after, and the seat's next publish, a cycle's or its release's
// flush, starts at the fifth: not at the first, which republishes what was
// carried, and not at the sixth, which leaves the refused row behind for as
// long as this node holds the seat.
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

// A PUBLISH THAT OUTLIVES A FORGET DOES NOT START ITS TABLE AGAIN.
//
// A cycle publishes from a snapshot of the seats it holds and takes no seat's
// lock, so a release can run the seat's flush and [Syncer.Forget] while the
// cycle is still publishing it. The publish in flight goes on, as it did when
// it read its table whole: it continues the table it is in, and re-creates
// the seat's marks as it has rows acknowledged. So each page is read above
// the larger of the mark and the last row the publish had acknowledged
// itself, its cursor: a Forget between two pages, after one page's last mark
// and before the next page's read, leaves the cursor ahead of the zeroed
// mark, and a page read from the mark alone sends the table back to its
// first row. The case puts the Forget there through the syncer's seam, since
// the publish of a row cannot: the row's own mark, set once it is
// acknowledged, re-creates a mark forgotten inside its publish at the
// cursor's value.
//
// Mutation: read each page from the mark alone, and this goes red.
func TestAPublishThatOutlivesAForgetDoesNotStartItsTableAgain(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 300, noteWidth)
	numbers := noteNumbers(t, ids)
	js := &roundTripJS{}
	s := syncerOver(db, js)
	pages := 0
	s.beforePage = func(_ markRange, table string) {
		if table != "agent_diary" {
			return
		}
		// BETWEEN THE FIRST PAGE AND THE SECOND.
		if pages++; pages == 2 {
			s.Forget(seat.Handle)
		}
	}
	if _, err := s.PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pages < 3 {
		t.Fatalf("the %d notes were read in %d pages; this case needs a Forget between "+
			"two pages and a page after it", len(ids), pages)
	}
	if got := numbersOf(t, js.acked, numbers); !slices.Equal(got, span(1, len(ids))) {
		t.Fatalf("a publish a Forget overtook between its first two pages had notes %s "+
			"acknowledged, want each of the %d once, in order", runsOf(got), len(ids))
	}
}

// A PUBLISH SKIPS WHAT ANOTHER PUBLISH OF THE SEAT HAD ACKNOWLEDGED.
//
// Two publishes of one seat can run at once: a cycle publishes from a
// snapshot of the seats it holds and takes no seat's lock, so a release's
// flush runs beside it. Each page is read above the larger of the seat's mark
// and the call's own cursor, so a page read after the other call has had rows
// acknowledged starts above them, and the two send again at most the page
// each already held. The case runs the second publish, whole, inside the
// first's publish of its first row.
//
// Mutation: read each page from the call's own cursor alone, and the first
// publish sends the table again from where its first page ended.
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
// The export read every row above the mark into memory before the first was
// published (#1033), so a publish held the backlog whole: after a hydration,
// everything the changelog keeps of the seat. A publish reads a page, sends it
// and reads the next, so a row deleted before its page is read is never
// published, which is what shows the later pages were not already in memory.
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
// A row is bounded only by the transport, and an episode keeps its ask whole,
// so one row can hold more than a page's budget. A page takes the row that
// passes its budget and stops after it, so such a row ends the page it is
// read into, and a page is never empty: one that stopped short of it would
// read the same nothing for ever, and the rows after it with it. The case
// watches each page of the episodes through the syncer's seam, and stops the
// publish at a page read after one that took no row and said there were
// more, so that publish fails here, naming what it did, rather than at the
// test binary's timeout.
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
	// read is how many rows had been acknowledged when the episodes' last
	// page was read. A page read with none acknowledged since follows a page
	// that took no row and said there were more.
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
// What a publish answers is how the memory sync tells a backlog draining from
// a sync that carries nothing that stays carried: every row the changelog
// acknowledged, the rows among them that moved a mark, and, for a publish that
// stopped short, the table it stopped in and whether that table is carried
// whole, keeping nothing of a cut publish. The seed is one row in each table,
// so the first publish carries six and marks the three of the watermark
// tables, the second carries only the whole tables again, and the third,
// refused at its first row, stops in the first whole table the registry lists.
//
// Mutation: count the whole tables' rows as marked, and this goes red.
func TestAPublishSaysWhatItCarriedAndWhereItStopped(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedMemory(t, db)
	whole, firstWhole := 0, ""
	for _, spec := range tables {
		if spec.wholeEachCycle {
			whole++
			if firstWhole == "" {
				firstWhole = spec.name
			}
		}
	}
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
	if want := (Carried{Rows: len(tables), Marked: len(tables) - whole}); err != nil || first != want {
		t.Fatalf("the first publish answered %+v (%v), want %+v: the seed's row of every table "+
			"acknowledged, and those of the watermark tables marked", first, err, want)
	}
	second, err := s.PublishOwn(t.Context(), seat.Handle)
	if want := (Carried{Rows: whole}); err != nil || second != want {
		t.Fatalf("the second publish answered %+v (%v), want %+v: the whole tables carried "+
			"again, and nothing marked", second, err, want)
	}
	refuse = true
	third, err := s.PublishOwn(t.Context(), seat.Handle)
	if want := (Carried{StoppedIn: firstWhole, Whole: true}); !errors.Is(err, refused) || third != want {
		t.Fatalf("the publish refused at its first row answered %+v (%v), want %+v and the "+
			"refusal", third, err, want)
	}
}
