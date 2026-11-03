package engine

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A HELD VALUE IS SERVED, AND REFRESHED OFF THE READ'S PATH, ONE READ AT A
// TIME.
//
// Nothing held: the first read waits for the value. Younger than the recheck:
// answered as held, nothing asked. Older: answered as held at once while ONE
// read replaces it, however many reads find it old — none of them waits on a
// broker that has stopped answering. Past the stale bound with nobody able to
// refresh it: no answer at all, the failure the refresh met. And a value that
// does not describe what is asked (a log's ends read at another generation) is
// never answered: the read waits for one that does. The stale bound is the
// VALUE'S, so the same held value is answered under a long bound and refused
// under a short one.
func TestAHeldValueIsServedAndRefreshedOffTheReadsPath(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(time.Unix(1_700_000_000, 0).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	advance := func(d time.Duration) { clock.Add(int64(d)) }
	m := &memo[int]{now: now}

	var mu sync.Mutex
	value, fail := 1, error(nil)
	gate := make(chan struct{})
	close(gate) // open: reads answer at once
	read := func(ctx context.Context) (int, error) {
		mu.Lock()
		g := gate
		mu.Unlock()
		select {
		case <-g:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		mu.Lock()
		defer mu.Unlock()
		return value, fail
	}
	always := func(int) bool { return true }
	served := func(usable func(int) bool) (int, error) {
		t.Helper()
		return m.serve(t.Context(), statelog.FloorCacheStale, usable, read)
	}

	if v, err := served(always); err != nil || v != 1 || m.reads.Load() != 1 {
		t.Fatalf("an empty memo served (%d, %v) after %d reads, want 1 after one", v, err, m.reads.Load())
	}
	for range 50 {
		if v, err := served(always); err != nil || v != 1 {
			t.Fatalf("a fresh value served (%d, %v)", v, err)
		}
	}
	if m.reads.Load() != 1 {
		t.Fatalf("a value younger than the recheck was read again: %d reads", m.reads.Load())
	}

	// OLDER THAN THE RECHECK, and the broker has stopped answering: every
	// read is answered the held value AT ONCE, and one read is in flight.
	mu.Lock()
	gate, value = make(chan struct{}), 2
	held := gate
	mu.Unlock()
	advance(servingRecheck)
	started := time.Now()
	for range 50 {
		if v, err := served(always); err != nil || v != 1 {
			t.Fatalf("an old value served (%d, %v), want the held one", v, err)
		}
	}
	if took := time.Since(started); took > servingRead/2 {
		t.Fatalf("reads of an old value waited %s on a broker that does not answer", took)
	}
	if got := m.reads.Load(); got != 2 {
		t.Fatalf("fifty reads of an old value made %d reads, want one more", got-1)
	}
	close(held)
	waitUntil(t, 5*time.Second, "the refresh to land", func() bool {
		v, err := served(always)
		return err == nil && v == 2
	})

	// PAST THE STALE BOUND with nobody able to refresh it: no answer. The
	// bound is the caller's, so a value inside a minute's bound is still
	// answered while the ends' few seconds have already refused it.
	mu.Lock()
	fail = errors.New("the broker does not answer")
	mu.Unlock()
	advance(endsStale + time.Second)
	if _, err := m.serve(t.Context(), endsStale, always, read); err == nil ||
		err.Error() != "the broker does not answer" {
		t.Fatalf("a value nobody refreshed for the ends' bound served err=%v, want the "+
			"refresh's failure", err)
	}
	if v, err := served(always); err != nil || v != 2 {
		t.Fatalf("a value inside the floor's bound served (%d, %v), want the held one", v, err)
	}
	advance(statelog.FloorCacheStale)
	if _, err := served(always); err == nil || err.Error() != "the broker does not answer" {
		t.Fatalf("a value nobody refreshed for the stale bound served err=%v, want the "+
			"refresh's failure", err)
	}

	// AND A VALUE THAT DESCRIBES SOMETHING ELSE is never answered.
	mu.Lock()
	fail, value = nil, 3
	mu.Unlock()
	if v, err := served(func(v int) bool { return v == 3 }); err != nil || v != 3 {
		t.Fatalf("an unusable held value: served (%d, %v), want the one read for it", v, err)
	}
}

// A READ ASKS THE BROKER NOTHING, on a real node: a few hundred reads of the
// tracker's read authority, at the stale level that promises "no broker call
// at all", send next to nothing on the node's broker connection and read the
// published floors and the log's ends at most once a recheck. Before, every
// read listed the coordination bucket's floors and asked the stream's leader
// for its info — a few hundred reads were a few hundred of each.
func TestAReadAsksTheBrokerNothing(t *testing.T) {
	t.Parallel()
	e, _, q := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	s := e.core.Load().log
	running := s.Domain(trackerName)
	read := func() {
		t.Helper()
		if _, err := running.reader.Read(t.Context(), statelog.Query{Level: statelog.ReadStale},
			func(*sql.Tx) error { return nil }); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	read() // the memos are filled

	const reads = 300
	floors, ends := s.floorMemos.of(trackerName).reads.Load(), running.ends.reads.Load()
	sent := q.Conn().Stats().OutMsgs
	started := time.Now()
	for range reads {
		read()
	}
	took := time.Since(started)
	rechecks := int64(took/servingRecheck) + 2
	sent = q.Conn().Stats().OutMsgs - sent
	if got := s.floorMemos.of(trackerName).reads.Load() - floors; got > rechecks {
		t.Errorf("%d reads listed the published floors %d times, want at most %d", reads, got, rechecks)
	}
	if got := running.ends.reads.Load() - ends; got > rechecks {
		t.Errorf("%d reads asked the log for its ends %d times, want at most %d", reads, got, rechecks)
	}
	// THE CONNECTION'S OWN COUNT, which no wrapper chose what to count, set
	// against what the same connection sends with nobody reading, over as
	// long again. The node's appliers and feeds pull on it whatever anybody
	// reads, about a dozen messages a second when idle, so a bound on the
	// reads' count alone measured how long they took: on a loaded machine
	// whose reads took a few seconds the background alone passed it (65
	// messages against 60, with every read served from memory). What is
	// left past the background is the reads' own traffic and the start-up
	// burst the reads may overlap; a request per read would be hundreds.
	//
	// HALF THE READS of allowance for that burst, because the burst is a
	// start-up's and grows with the logs a node runs, while what this case
	// exists to catch, a request per read, sends at least reads — twice the
	// allowance. A fifth of the reads was a score of messages over eight
	// logs; over the ten a node runs with the assistant's and chat's, CI run
	// 41343149799 measured 68 sent against 0 idle, every read served from
	// memory, and the fifth failed it.
	idle := q.Conn().Stats().OutMsgs
	time.Sleep(took)
	idle = q.Conn().Stats().OutMsgs - idle
	if sent > idle+reads/2 {
		t.Errorf("%d reads sent %d messages on the broker connection, against %d sent "+
			"over as long with nobody reading", reads, sent, idle)
	}
}

// BenchmarkAReadsReadiness measures what one read of the tracker's read
// authority on a single node costs the broker: the messages its connection
// sent, the reads of the published floors and the stream-info reads, each per
// read. All three are near zero — the floors and the ends are served from
// memory, refreshed at most once a recheck however many reads arrive.
func BenchmarkAReadsReadiness(b *testing.B) {
	e, _, q := bootRejoinNode(b)
	waitUntil(b, 20*time.Second, "the node to admit seats", hydrated(b, e))
	s := e.core.Load().log
	running := s.Domain(trackerName)
	ctx := b.Context()
	query := statelog.Query{Level: statelog.ReadStale}
	noop := func(*sql.Tx) error { return nil }
	if _, err := running.reader.Read(ctx, query, noop); err != nil {
		b.Fatal(err)
	}
	floors, ends := s.floorMemos.of(trackerName).reads.Load(), running.ends.reads.Load()
	sent := q.Conn().Stats().OutMsgs
	b.ResetTimer()
	for b.Loop() {
		if _, err := running.reader.Read(ctx, query, noop); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	per := func(n int64) float64 { return float64(n) / float64(b.N) }
	b.ReportMetric(per(int64(q.Conn().Stats().OutMsgs-sent)), "broker-msgs/op")
	b.ReportMetric(per(s.floorMemos.of(trackerName).reads.Load()-floors), "floor-reads/op")
	b.ReportMetric(per(running.ends.reads.Load()-ends), "stream-infos/op")
}

// A BOUNDED READ IS NEVER CHECKED AGAINST ENDS NOBODY COULD REFRESH, on a real
// node. The tracker's ends are held, the node's clock moves past the oldest a
// working refresh leaves them, and the one refresh in flight fails — a stream
// leader that stopped answering. A read declaring `max_lag_seq` must refuse:
// served the held ends, it would be checked against a lag nobody had measured
// since, which under a partial partition is a node thousands of records
// behind served as inside its caller's bound. Before the ends had a bound of
// their own they were served for the floor's whole minute.
//
// And it refuses `broker_unreachable`, naming the refresh's failure: what
// could not be read is the stream's leader, never the trim floor — folded into
// a zero health, it reached the caller as a floor "never read on this node".
func TestABoundedReadIsNeverCheckedAgainstEndsNobodyCouldRefresh(t *testing.T) {
	t.Parallel()
	e, _, _ := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	running := e.core.Load().log.Domain(trackerName)
	bounded := statelog.Query{Level: statelog.ReadStale, MaxLagSeq: 1_000}

	refusal := refusedOverAFailedRefresh(t, running, &running.ends, endsStale, bounded,
		"the stream's leader does not answer")
	if refusal.Code != statelog.RefuseBrokerUnreachable ||
		!strings.Contains(refusal.Detail, "the stream's leader does not answer") {
		t.Fatalf("a bounded read over ends nobody could refresh refused %q (%s), want %q "+
			"naming the refresh's failure", refusal.Code, refusal.Detail,
			statelog.RefuseBrokerUnreachable)
	}
}

// A FLOOR NOBODY COULD REFRESH IS REFUSED AS UNKNOWN, NAMING ITS AGE, on a real
// node. The published floors are held, the node's clock moves past
// [statelog.FloorCacheStale], and the refresh in flight fails — coordination
// that stopped answering. A read refuses `floor_unknown`, exactly as the alarm
// of that name says, and the detail says how long ago the floor was last
// read. Before, a floors failure was an error folded into a zero health,
// whose floor read as never read at all.
func TestAFloorNobodyCouldRefreshIsRefusedAsUnknownNamingItsAge(t *testing.T) {
	t.Parallel()
	e, _, _ := bootRejoinNode(t)
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	s := e.core.Load().log
	running := s.Domain(trackerName)

	refusal := refusedOverAFailedRefresh(t, running, s.floorMemos.of(trackerName), statelog.FloorCacheStale,
		statelog.Query{Level: statelog.ReadStale}, "coordination does not answer")
	if refusal.Code != statelog.RefuseFloorUnknown {
		t.Fatalf("a read over a floor nobody could refresh refused %q (%s), want %q",
			refusal.Code, refusal.Detail, statelog.RefuseFloorUnknown)
	}
	said := regexp.MustCompile(`last read (\S+) ago`).FindStringSubmatch(refusal.Detail)
	if said == nil {
		t.Fatalf("the refusal says %q, want how long ago the floor was last read", refusal.Detail)
	}
	if age, err := time.ParseDuration(said[1]); err != nil || age < statelog.FloorCacheStale {
		t.Fatalf("the refusal names an age of %q, want at least the %s past which a "+
			"floor stops counting as read", said[1], statelog.FloorCacheStale)
	}
}

// refusedOverAFailedRefresh holds m's value older than stale with one refresh
// in flight, starts a read of running's read authority, checks it waits for
// that refresh rather than answering from the old value, and then fails the
// refresh with why. It returns the read's refusal.
//
// THE MEMO IS FROZEN FOR THE CASE, because the node goes on reading it: every
// read of its health may start a refresh, and every fresh read made elsewhere
// (the heartbeat's, the copy's verdict) stores what it read. So the held
// instant and the memo's clock both move an hour AHEAD of the wall, the value
// exactly as old as the case says by the memo's own clock, and a store stamped
// with the wall clock landed before it and replaces nothing ([memo.store]);
// and the refresh's slot stays the case's, failed, until the case ends, so no
// other refresh starts in its place. The slot used to be given back with the
// failure: a real refresh the node's next read started then landed at the
// moved clock before the case's read looked again, and that read was answered
// from it, or refused naming an age of nothing — four runs in sixty of the
// two cases on one CPU.
func refusedOverAFailedRefresh[T any](t *testing.T, running *runningLog, m *memo[T],
	stale time.Duration, q statelog.Query, why string) *statelog.Refused {

	t.Helper()
	noop := func(*sql.Tx) error { return nil }
	if _, err := running.reader.Read(t.Context(), q, noop); err != nil {
		t.Fatalf("a read of a caught-up node: %v", err)
	}
	// Only once no real refresh is in flight, whose landing would otherwise
	// end the one this case holds open.
	landed := make(chan struct{})
	var heldAt time.Time
	waitUntil(t, 5*time.Second, "no refresh in flight", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.reading != nil {
			return false
		}
		ahead := time.Now().Add(time.Hour)
		heldAt, m.at = m.at, ahead
		m.now = func() time.Time { return ahead.Add(stale + time.Second) }
		m.reading = landed
		return true
	})
	t.Cleanup(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.now, m.at = nil, heldAt
		if m.reading == landed {
			m.reading = nil
		}
	})
	refused := make(chan error, 1)
	go func() {
		_, err := running.reader.Read(t.Context(), q, noop)
		refused <- err
	}()
	select {
	case err := <-refused:
		t.Fatalf("a read answered (err=%v) before the refresh had landed, from a "+
			"value older than %s", err, stale)
	case <-time.After(50 * time.Millisecond):
	}
	// AND THE REFRESH FAILS, recorded as [memo.refresh] records a read that
	// did not answer — its slot kept (above).
	m.mu.Lock()
	m.failed = errors.New(why)
	m.mu.Unlock()
	close(landed)

	var refusal *statelog.Refused
	if err := <-refused; !errors.As(err, &refusal) {
		t.Fatalf("a read over a value nobody could refresh: err=%v, want a refusal", err)
	}
	return refusal
}
