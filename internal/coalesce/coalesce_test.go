package coalesce_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coalesce"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/usage"
)

// tally is the suite's record: how many occurrences of one name a flush held.
type tally struct {
	Name string `json:"name"`
	N    int    `json:"n"`
}

func (tally) EventType() string { return "coalesce_tally" }

// count is the suite's bucket: the occurrences folded so far, and how many
// times the fold was handed a fresh one.
type count struct {
	n, fresh int
}

// tallies is the suite's records, logging into lines.
func tallies(lines *lines) coalesce.Records[string, count] {
	return coalesce.Records[string, count]{
		Type: tally{}.EventType(),
		Seal: func(name string, b *count) *events.Event {
			return events.New(tally{Name: name, N: b.n}, events.TraceContext{})
		},
		Held: func(held []*count) []any {
			n := 0
			for _, b := range held {
				n += b.n
			}
			return []any{"occurrences", n}
		},
		Lost:   "the suite's tallies are short of them",
		Logger: slog.New(lines),
	}
}

// add folds n occurrences of name into l, one at a time.
func add(l *coalesce.Ledger[string, count], name string, n int) {
	for range n {
		l.Add(name, func(b *count, fresh bool) {
			b.n++
			if fresh {
				b.fresh++
			}
		})
	}
}

// recorder is a publisher that keeps what it was handed, refusing while told
// to — the broker's two answers — and, while held, holding every publish until
// it is let go.
type recorder struct {
	mu sync.Mutex
	// tried is every event a publish was asked for, published or refused, and
	// got every one published.
	tried, got []*events.Event
	refuse     bool
	refused    int
	// entered is told of every publish as it begins, where it is set.
	entered chan struct{}
	// hold, where set, is what every publish waits on before it answers.
	hold chan struct{}
}

func (r *recorder) Publish(_ context.Context, subject string, ev *events.Event) error {
	if r.entered != nil {
		r.entered <- struct{}{}
	}
	if r.hold != nil {
		<-r.hold
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if subject != "crewlet.events."+ev.Type {
		return errors.New("published on " + subject)
	}
	r.tried = append(r.tried, ev)
	if r.refuse {
		r.refused++
		return errors.New("broker unavailable")
	}
	r.got = append(r.got, ev)
	return nil
}

func (r *recorder) setRefuse(refuse bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refuse = refuse
}

// records is every tally published so far, in order.
func (r *recorder) records(t *testing.T) []tally {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]tally, 0, len(r.got))
	for _, ev := range r.got {
		rec, ok := events.DataAs[*tally](ev)
		if !ok {
			t.Fatalf("published a %s, want a tally", ev.Type)
		}
		out = append(out, *rec)
	}
	return out
}

// lines is a log handler that keeps every line it is handed.
type lines struct {
	mu  sync.Mutex
	got []slog.Record
}

func (l *lines) Enabled(context.Context, slog.Level) bool { return true }
func (l *lines) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *lines) WithGroup(string) slog.Handler            { return l }
func (l *lines) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.got = append(l.got, r.Clone())
	return nil
}

// said is the attributes of the last line logged as msg, and whether there was
// one.
func (l *lines) said(msg string) (map[string]any, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.got) - 1; i >= 0; i-- {
		if l.got[i].Message != msg {
			continue
		}
		attrs := map[string]any{}
		l.got[i].Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value.Any()
			return true
		})
		return attrs, true
	}
	return nil, false
}

// ONE RECORD PER KEY PER FLUSH, carrying every occurrence of it — the fold
// handed a fresh bucket once, at a key's first occurrence — and a flush empties
// the ledger: the next one publishes nothing again.
func TestALedgerFoldsOneKeysOccurrencesIntoOneRecord(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := coalesce.New(pub, tallies(&lines{}))
	add(l, "a", 3)
	add(l, "b", 1)
	l.Flush(t.Context())
	got := pub.records(t)
	byName := map[string]int{}
	for _, r := range got {
		byName[r.Name] = r.N
	}
	if len(got) != 2 || byName["a"] != 3 || byName["b"] != 1 {
		t.Fatalf("records = %+v, want a's three occurrences and b's one", got)
	}
	l.Flush(t.Context())
	if len(pub.records(t)) != 2 {
		t.Error("a second flush published the same occurrences again")
	}
	// A KEY FLUSHED IS FRESH AGAIN: its next occurrence starts a new bucket.
	var fresh []bool
	l.Add("a", func(_ *count, f bool) { fresh = append(fresh, f) })
	l.Add("a", func(_ *count, f bool) { fresh = append(fresh, f) })
	if len(fresh) != 2 || !fresh[0] || fresh[1] {
		t.Errorf("a flushed key's next two folds were fresh %v, want [true false]", fresh)
	}
}

// AN ADD IS NEVER HELD BY A PUBLISH: a flush whose publish the broker has not
// answered — a stream with no leader, a quorum lost — holds no lock an
// occurrence needs, so the request that made the occurrence answers at the
// speed of its own work, and the occurrence is the next flush's.
//
// Mutation: publish under the ledger's lock, and the add waits for the
// broker — this case's hang detector goes red.
func TestAnAddIsNeverHeldByAPublish(t *testing.T) {
	t.Parallel()
	pub := &recorder{entered: make(chan struct{}, 1), hold: make(chan struct{})}
	l := coalesce.New(pub, tallies(&lines{}))
	add(l, "a", 1)
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		l.Flush(context.Background())
	}()
	<-pub.entered

	added := make(chan struct{})
	go func() {
		defer close(added)
		add(l, "b", 2)
	}()
	select {
	case <-added:
	case <-time.After(10 * time.Second):
		close(pub.hold)
		t.Fatal("an add waited for a publish the broker had not answered")
	}
	close(pub.hold)
	<-flushed
	if got := pub.records(t); len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("the flush in flight published %+v, want a's record alone", got)
	}
	pub.entered = nil
	l.Flush(t.Context())
	if got := pub.records(t); len(got) != 2 || got[1].Name != "b" || got[1].N != 2 {
		t.Fatalf("records = %+v, want b's two occurrences on the next flush", got)
	}
}

// A REFUSED PUBLISH IS PUBLISHED AGAIN, AS THE SAME EVENT.
//
// The record is the only account its readers will ever have of what it
// folded, so a broker that refused it is asked again by the next flush — with
// the id and the instant it was sealed with, so a publish reported failed
// that had in fact landed is collapsed by the store's (time, id) key and the
// live window's id index rather than counted twice.
func TestARefusedRecordIsPublishedAgainAsTheSameEvent(t *testing.T) {
	t.Parallel()
	pub := &recorder{refuse: true}
	log := &lines{}
	l := coalesce.New(pub, tallies(log))
	add(l, "a", 2)
	add(l, "b", 1)
	l.Flush(t.Context())
	// ONE REFUSAL ENDS THE FLUSH: a broker that refused one record is down
	// for the next, and a backlog asked record by record would spend every
	// flush of an outage on refusals.
	if pub.refused != 1 || len(pub.got) != 0 {
		t.Fatalf("refused %d, published %d — want one attempt that was refused",
			pub.refused, len(pub.got))
	}
	if attrs, ok := log.said("coalesce_tally_unpublished"); !ok || attrs["records"] != int64(2) ||
		attrs["occurrences"] != int64(3) {
		t.Errorf("the refusal was said as %v (said %v), want both records and their three occurrences",
			attrs, ok)
	}
	pub.setRefuse(false)
	add(l, "a", 5)
	l.Flush(t.Context())
	got := pub.records(t)
	if len(got) != 3 {
		t.Fatalf("records = %+v, want the two refused and the new one", got)
	}
	var as []int
	for _, r := range got {
		if r.Name == "a" {
			as = append(as, r.N)
		}
	}
	if len(as) != 2 || as[0] != 2 || as[1] != 5 {
		t.Errorf("a's records = %v, want the refused 2 first and unchanged — a later "+
			"occurrence is a new record, never added to one already sealed", as)
	}
	ids := map[string]bool{}
	for _, ev := range pub.got {
		if ids[ev.ID.String()] {
			t.Errorf("record %s was published twice", ev.ID)
		}
		ids[ev.ID.String()] = true
	}

	// AND THE RETRY IS THE SAME EVENT: refused twice and then let through, a
	// record keeps its id and its instant on every attempt.
	pub.setRefuse(true)
	add(l, "c", 1)
	l.Flush(t.Context())
	l.Flush(t.Context())
	pub.setRefuse(false)
	l.Flush(t.Context())
	l.Flush(t.Context())
	pub.mu.Lock()
	defer pub.mu.Unlock()
	if len(pub.got) != 4 {
		t.Fatalf("after two refusals c was published %d times, want once", len(pub.got)-3)
	}
	c := pub.got[3]
	var attempts int
	for _, ev := range pub.tried {
		if rec, _ := events.DataAs[*tally](ev); rec.Name != "c" {
			continue
		}
		attempts++
		if ev.ID != c.ID || !ev.Timestamp.Equal(c.Timestamp) {
			t.Errorf("an attempt to publish c was event %s at %s, want the one sealed: %s at %s",
				ev.ID, ev.Timestamp, c.ID, c.Timestamp)
		}
	}
	if attempts != 3 {
		t.Errorf("c was asked for %d times, want its two refusals and its publish", attempts)
	}
}

// THE BACKLOG IS BOUNDED, and what leaves it is the OLDEST: a broker that has
// refused for an hour costs the readers that hour's oldest records, never an
// unbounded heap — and the records dropped are said, with what they held, so
// the gap can be named.
//
// At [backlogBound] rather than MaxPending: the trim and the seal-order merge
// are the same branches at any bound. ONE FLUSH PER ADD, because the open
// buckets are a map: only a flush between adds seals records in the order
// they were added, which is the order the assertions read.
func TestARefusedBacklogIsBoundedOldestFirst(t *testing.T) {
	t.Parallel()
	pub := &recorder{refuse: true}
	log := &lines{}
	l := coalesce.NewBounded(pub, tallies(log), backlogBound)
	for i := range backlogBound + 3 {
		add(l, "t", i+1)
		l.Flush(t.Context())
	}
	// THE THIRD DROP, of the record of three occurrences.
	if attrs, ok := log.said("coalesce_tally_dropped"); !ok || attrs["records"] != int64(1) ||
		attrs["occurrences"] != int64(3) {
		t.Errorf("the last drop was said as %v (said %v), want one record of three occurrences",
			attrs, ok)
	}
	pub.setRefuse(false)
	l.Flush(t.Context())
	got := pub.records(t)
	if len(got) != backlogBound {
		t.Fatalf("published %d after the outage, want the %d the backlog holds",
			len(got), backlogBound)
	}
	for i, r := range got {
		if r.N != i+4 {
			t.Fatalf("record %d holds %d occurrences, want %d: the oldest three should "+
				"have been dropped and the rest published oldest first", i, r.N, i+4)
		}
	}
}

// A CHOSEN FLUSH'S REFUSED RECORDS JOIN THE BACKLOG AT ITS END, because they
// are the newest there are.
//
// A chosen flush (FlushWhere: a turn's end, its own buckets) never takes the
// backlog, so what it hands back is newer than everything already waiting.
// Put in front, the bound's trim dropped the turn's fresh records as "the
// oldest" at every turn's end of an outage while the hour-old backlog stayed,
// and the retry published newest first.
func TestARefusedChosenFlushIsTheNewestInTheBacklog(t *testing.T) {
	t.Parallel()
	pub := &recorder{refuse: true}
	l := coalesce.NewBounded(pub, tallies(&lines{}), backlogBound)
	for i := range backlogBound {
		add(l, "old", i+1)
		l.Flush(t.Context())
	}
	for i, name := range []string{"new-1", "new-2"} {
		add(l, name, 1000+i)
		l.FlushWhere(t.Context(), func(k string) bool { return k == name })
	}
	pub.setRefuse(false)
	l.Flush(t.Context())
	got := pub.records(t)
	if len(got) != backlogBound {
		t.Fatalf("published %d after the outage, want the %d the backlog holds",
			len(got), backlogBound)
	}
	if got[0].N != 3 {
		t.Errorf("the backlog's first record holds %d, want 3 — the two oldest dropped "+
			"and the rest published oldest first", got[0].N)
	}
	tail := got[len(got)-2:]
	if tail[0].Name != "new-1" || tail[1].Name != "new-2" {
		t.Errorf("the backlog ends with %s, %s — want the two chosen flushes' records, newest last",
			tail[0].Name, tail[1].Name)
	}
	for i := 1; i < len(got)-2; i++ {
		if got[i].N < got[i-1].N {
			t.Fatalf("record %d (%d) published after a newer one (%d)", i, got[i].N, got[i-1].N)
		}
	}
}

// A CHOSEN FLUSH PUBLISHES WHAT IT CHOSE, and nobody else's: every bucket it
// leaves, and the backlog, wait for the timer they were going to be flushed by.
func TestAChosenFlushPublishesWhatItChoseAlone(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := coalesce.New(pub, tallies(&lines{}))
	add(l, "t-1", 1)
	add(l, "t-2", 1)
	l.FlushWhere(t.Context(), func(k string) bool { return k == "t-1" })
	if got := pub.records(t); len(got) != 1 || got[0].Name != "t-1" {
		t.Fatalf("records = %+v, want t-1's alone", got)
	}
	l.Flush(t.Context())
	if got := pub.records(t); len(got) != 2 || got[1].Name != "t-2" {
		t.Fatalf("records = %+v, want t-2's on the next flush", got)
	}
}

// backlogBound is the backlog cases' bound: past the three records the first
// case overflows by and the two the second adds, so each trims from a backlog
// that keeps a run of older records whose order can be checked.
const backlogBound = 8

// A RUNNING NODE KEEPS 4096 REFUSED RECORDS, the figure MaxPending argues for:
// under three megabytes whichever records they are. The backlog cases run at a
// bound of their own, so this is what ties the figure they skip filling to the
// one every ledger New builds is held to.
func TestALedgerKeepsMaxPendingRefusedRecords(t *testing.T) {
	t.Parallel()
	if coalesce.MaxPending != 4096 {
		t.Errorf("MaxPending = %d, want 4096", coalesce.MaxPending)
	}
	l := coalesce.New(&recorder{}, tallies(&lines{}))
	if got := coalesce.PendingBound(l); got != coalesce.MaxPending {
		t.Errorf("New keeps %d refused records, want MaxPending (%d)", got, coalesce.MaxPending)
	}
}

// THE STOP FLUSHES WHAT THE TIMER HAS NOT: the last records a draining node's
// producers made are published before the stream they are published on
// closes — and a stopped ledger still takes records, which a second stop
// flushes. What a stop cannot publish is lost, and said with what it held.
func TestAStopFlushesWhatTheTimerHasNot(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	log := &lines{}
	l := coalesce.New(pub, tallies(log))
	l.Run(t.Context())
	add(l, "a", 2)
	l.Stop(t.Context())
	if got := pub.records(t); len(got) != 1 || got[0].N != 2 {
		t.Fatalf("records = %+v, want the occurrences made before the stop", got)
	}
	add(l, "a", 1)
	l.Stop(t.Context())
	if len(pub.records(t)) != 2 {
		t.Error("a second stop did not flush what arrived after the first")
	}
	pub.setRefuse(true)
	add(l, "b", 4)
	l.Stop(t.Context())
	if attrs, ok := log.said("coalesce_tally_lost_at_stop"); !ok || attrs["records"] != int64(1) ||
		attrs["occurrences"] != int64(4) {
		t.Errorf("the stop's loss was said as %v (said %v), want one record of four occurrences",
			attrs, ok)
	}
}

// A LEDGER FLUSHES ON THE USAGE DOMAIN'S CADENCE: every record it carries is
// counted by its node's usage day, derived on that tick, so another cadence
// would leave a record a day behind for longer than the tick it waits for.
func TestTheLedgerFlushesOnTheUsageCadence(t *testing.T) {
	t.Parallel()
	if coalesce.FlushInterval != usage.FlushInterval {
		t.Fatalf("coalesce.FlushInterval = %s, usage.FlushInterval = %s",
			coalesce.FlushInterval, usage.FlushInterval)
	}
}

// A NIL LEDGER, AND ONE WITH NO PUBLISHER, KEEP NOTHING AND PANIC NOWHERE: a
// node with no queue publishes no event of any kind, and producers call it
// unconditionally.
func TestANilLedgerIsInert(t *testing.T) {
	t.Parallel()
	var l *coalesce.Ledger[string, count]
	add(l, "a", 1)
	l.Flush(t.Context())
	l.FlushWhere(t.Context(), func(string) bool { return true })
	l.Run(t.Context())
	l.Stop(t.Context())
	folded := false
	none := coalesce.New[string, count](nil, tallies(&lines{}))
	none.Add("a", func(*count, bool) { folded = true })
	none.Run(t.Context())
	none.Stop(t.Context())
	if folded {
		t.Error("a ledger with no publisher folded an occurrence it can never publish")
	}
}
