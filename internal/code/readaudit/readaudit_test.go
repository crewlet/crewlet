package readaudit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/readaudit"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// recorder is a publisher that keeps what it was handed, refusing while told
// to — the broker's two answers.
type recorder struct {
	mu     sync.Mutex
	got    []*events.Event
	refuse bool
}

func (r *recorder) Publish(_ context.Context, subject string, ev *events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if subject != "crewlet.events."+ev.Type {
		return errors.New("published on " + subject)
	}
	if r.refuse {
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

// records is every record published so far, in order, beside its envelope.
func (r *recorder) records(t *testing.T) ([]types.CodeFetched, []*events.Event) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]types.CodeFetched, 0, len(r.got))
	for _, ev := range r.got {
		rec, ok := events.DataAs[*types.CodeFetched](ev)
		if !ok {
			t.Fatalf("published a %s, want code_fetched", ev.Type)
		}
		out = append(out, *rec)
	}
	return out, append([]*events.Event(nil), r.got...)
}

var t0 = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// fetch is one clone of repo-1 by maya's token, sent bytes, at t0 plus at.
func fetch(at time.Duration, bytes int64) readaudit.Read {
	return readaudit.Read{
		Fetched: types.CodeFetched{RepoID: "repo-1", RepoName: "platform",
			PrincipalKind: types.CodeReaderOperator, Principal: "pat:7", Seat: "maya",
			Via: types.FetchedViaGit, Reads: 1, Bytes: bytes},
		At: t0.Add(at), Day: "2026-09-23",
	}
}

// ONE READER'S READS OF A FLUSH ARE ONE RECORD: a CI token fetching every few
// seconds is a record a flush, not a record a fetch. It counts the reads and
// sums their bytes, names the repository as its NEWEST read did — whatever
// order the reads were filed in — carries its first read's trace, and is
// stamped with its newest read's instant, the one the readers' answer reports
// as last read. A flush empties the ledger: the next publishes nothing again.
//
// Mutation: count a record's reads as one, and the three reads are a record
// of one.
func TestAReadersReadsOfAFlushAreOneRecord(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := readaudit.New(pub)
	first := fetch(0, 100)
	first.Trace = events.TraceContext{TraceID: "first"}
	newest := fetch(5*time.Second, 200)
	newest.Fetched.RepoName = "platform-core"
	newest.Trace = events.TraceContext{TraceID: "newest"}
	l.Add(first)
	l.Add(newest)
	l.Add(fetch(2*time.Second, 300))
	l.Flush(t.Context())
	l.Flush(t.Context())

	got, envs := pub.records(t)
	want := types.CodeFetched{RepoID: "repo-1", RepoName: "platform-core",
		PrincipalKind: types.CodeReaderOperator, Principal: "pat:7", Seat: "maya",
		Via: types.FetchedViaGit, Reads: 3, Bytes: 600}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("records = %+v, want the one %+v", got, want)
	}
	if !envs[0].Timestamp.Equal(t0.Add(5*time.Second)) || envs[0].TraceID != "first" || envs[0].Source != "" {
		t.Errorf("the record is stamped %s, traced %q and sourced %q — want its newest read's instant, "+
			"its first read's trace and no source", envs[0].Timestamp, envs[0].TraceID, envs[0].Source)
	}
}

// EVERY READER, WAY, KIND AND DAY IS A RECORD OF ITS OWN: a reader is its
// credential or seat AND the seat it acted as, so a token rebound to another
// seat reads anew; a server-side read of one kind is not one of another; and
// the day is in the key, so a record never straddles the company's midnight —
// each day's readers are that day's alone.
func TestEveryReaderWayKindAndDayIsARecordOfItsOwn(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := readaudit.New(pub)
	base := fetch(0, 10)
	l.Add(base)
	l.Add(base)
	for _, vary := range []func(r *readaudit.Read){
		func(r *readaudit.Read) { r.Fetched.RepoID = "repo-2" },
		func(r *readaudit.Read) { r.Fetched.Principal = "pat:8" },
		func(r *readaudit.Read) { r.Fetched.Seat = "jane" },
		func(r *readaudit.Read) { r.Day = "2026-09-24" },
		func(r *readaudit.Read) {
			r.Fetched.PrincipalKind, r.Fetched.Principal, r.Fetched.Seat = types.CodeReaderAgent, "dev", "dev"
		},
		func(r *readaudit.Read) { r.Fetched.Via, r.Fetched.Kind = types.FetchedViaTool, "blob" },
		func(r *readaudit.Read) { r.Fetched.Via, r.Fetched.Kind = types.FetchedViaTool, "tree" },
	} {
		r := base
		vary(&r)
		l.Add(r)
	}
	l.Flush(t.Context())
	got, _ := pub.records(t)
	if len(got) != 8 {
		t.Fatalf("records = %+v, want the base reader's and seven of their own", got)
	}
	for _, r := range got {
		want := int64(1)
		if r == (types.CodeFetched{RepoID: "repo-1", RepoName: "platform",
			PrincipalKind: types.CodeReaderOperator, Principal: "pat:7", Seat: "maya",
			Via: types.FetchedViaGit, Reads: 2, Bytes: 20}) {
			want = 2
		}
		if r.Reads != want {
			t.Errorf("record %+v counts %d reads, want %d", r, r.Reads, want)
		}
	}
}

// A SEAT'S READS ARE SOURCED TO THE SEAT, as every event of a seat's is, and a
// credential's to nobody: its reader is the record's own principal.
func TestASeatsReadsAreSourcedToTheSeat(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := readaudit.New(pub)
	seat := fetch(0, 10)
	seat.Fetched.PrincipalKind, seat.Fetched.Principal, seat.Fetched.Seat = types.CodeReaderAgent, "dev", "dev"
	seat.Fetched.Via, seat.Fetched.Kind = types.FetchedViaTool, "blob"
	l.Add(seat)
	l.Flush(t.Context())
	l.Add(fetch(time.Second, 10))
	l.Flush(t.Context())
	_, envs := pub.records(t)
	if len(envs) != 2 || envs[0].Source != "dev" || envs[1].Source != "" {
		t.Fatalf("sources = %v, want the seat's handle and then none", sources(envs))
	}
}

// A READ COUNTS WHAT IT SAYS, and one where it says none: a read is never
// filed as no read.
func TestAReadCountsWhatItSays(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := readaudit.New(pub)
	none := fetch(0, 10)
	none.Fetched.Reads = 0
	three := fetch(time.Second, 10)
	three.Fetched.Reads = 3
	l.Add(none)
	l.Add(three)
	l.Flush(t.Context())
	if got, _ := pub.records(t); len(got) != 1 || got[0].Reads != 4 {
		t.Fatalf("records = %+v, want one of four reads", got)
	}
}

// A REFUSED RECORD IS PUBLISHED BY THE NEXT FLUSH, whole, and a read filed in
// between is a record of its own rather than added to one already sealed: the
// broker's outage costs who read a repository nothing it comes back from.
func TestARefusedRecordIsPublishedByTheNextFlush(t *testing.T) {
	t.Parallel()
	pub := &recorder{refuse: true}
	l := readaudit.New(pub)
	l.Add(fetch(0, 100))
	l.Add(fetch(time.Second, 100))
	l.Flush(t.Context())
	if got, _ := pub.records(t); len(got) != 0 {
		t.Fatalf("a refused flush published %+v", got)
	}
	pub.setRefuse(false)
	l.Add(fetch(2*time.Second, 100))
	l.Flush(t.Context())
	got, _ := pub.records(t)
	if len(got) != 2 || got[0].Reads != 2 || got[1].Reads != 1 {
		t.Fatalf("records = %+v, want the refused record of two reads and then the new one", got)
	}
}

// THE STOP PUBLISHES WHAT THE TIMER HAS NOT: a draining node's last reads are
// in its last day.
func TestAStopPublishesTheLastReads(t *testing.T) {
	t.Parallel()
	pub := &recorder{}
	l := readaudit.New(pub)
	l.Run(t.Context())
	l.Add(fetch(0, 100))
	l.Stop(t.Context())
	if got, _ := pub.records(t); len(got) != 1 || got[0].Reads != 1 {
		t.Fatalf("records = %+v, want the read made before the stop", got)
	}
}

// A NIL LEDGER, AND ONE WITH NO PUBLISHER, KEEP NOTHING AND PANIC NOWHERE: a
// node with no queue records no event of any kind, and its producers file
// every read unconditionally.
func TestANilLedgerIsInert(t *testing.T) {
	t.Parallel()
	var l *readaudit.Ledger
	l.Add(fetch(0, 1))
	l.Flush(t.Context())
	l.Run(t.Context())
	l.Stop(t.Context())
	none := readaudit.New(nil)
	none.Add(fetch(0, 1))
	none.Flush(t.Context())
	none.Stop(t.Context())
}

func sources(envs []*events.Event) []string {
	out := make([]string, len(envs))
	for i, ev := range envs {
		out[i] = ev.Source
	}
	return out
}
