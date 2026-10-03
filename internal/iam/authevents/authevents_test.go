package authevents

import (
	"context"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// recorder is a publisher that keeps what it was handed, and whether the
// context it was handed was still live.
type recorder struct {
	mu     sync.Mutex
	events []*events.Event
	topics []string
	dead   []bool
}

func (r *recorder) Publish(ctx context.Context, topic string, ev *events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	r.topics = append(r.topics, topic)
	r.dead = append(r.dead, ctx.Err() != nil)
	return nil
}

func (r *recorder) published() []*events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

// counter is a Counter that sums by attribute set.
type counter struct {
	mu   sync.Mutex
	seen map[string]uint64
}

func (c *counter) Add(name string, n uint64, attrs metrics.Attrs) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]uint64{}
	}
	c.seen[name+"|"+attrs["method"]+"|"+attrs["outcome"]] += n
}

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

var noon = time.Date(2026, 9, 23, 12, 0, 10, 0, time.UTC)

func newTrail(t *testing.T) (*Trail, *recorder, *clock, *counter) {
	t.Helper()
	pub, clk, count := &recorder{}, &clock{now: noon}, &counter{}
	trail, err := New(Options{Publisher: pub, Counter: count, Node: "node-a", Now: clk.Now})
	if err != nil {
		t.Fatal(err)
	}
	return trail, pub, clk, count
}

func failuresOf(t *testing.T, published []*events.Event) []types.IAMLoginFailures {
	t.Helper()
	var out []types.IAMLoginFailures
	for _, ev := range published {
		row, ok := events.DataAs[*types.IAMLoginFailures](ev)
		if !ok {
			t.Fatalf("published %s, want only iam_login_failures", ev.Type)
		}
		out = append(out, *row)
	}
	return out
}

// A FAILURE CARRIES NOTHING ABOUT WHO, AND NEITHER DOES ITS ROW.
//
// A failed attempt is authored by whoever can reach the listener, and what it
// presented is where a password typed into the login box lands. The tally
// this replaced kept HMAC digests of it to count distinct names, and the ids
// of the people the engine resolved failures to; neither is kept now, and the
// way to keep it that way is to hold the SHAPES: what a surface can hand the
// trail, and what the row can say. A field added to carry "just a hash of the
// subject" is a new field, and it is caught here whatever it is called.
//
// Mutation: add a Subject to [Failure], or a key to the row or to a source's
// entry, and its half goes red naming it.
func TestAFailureCarriesNothingAboutWho(t *testing.T) {
	t.Parallel()
	fieldsOf := func(rt reflect.Type, tagged bool) []string {
		var out []string
		for i := range rt.NumField() {
			name := rt.Field(i).Name
			if tagged {
				name = strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
			}
			out = append(out, name)
		}
		slices.Sort(out)
		return out
	}
	for _, shape := range []struct {
		what   string
		rt     reflect.Type
		tagged bool
		want   []string
	}{
		{"a failure a surface hands the trail", reflect.TypeFor[Failure](), false,
			[]string{"Method", "Source", "Throttled"}},
		{"iam_login_failures", reflect.TypeFor[types.IAMLoginFailures](), true,
			[]string{"minute", "overflow", "sources"}},
		{"a source's entry on it", reflect.TypeFor[types.SourceFailures](), true,
			[]string{"failures", "source"}},
	} {
		if got := fieldsOf(shape.rt, shape.tagged); !slices.Equal(got, shape.want) {
			t.Errorf("%s carries %v, want exactly %v: a new field on a fact "+
				"whose rate an unauthenticated caller authors is a new place "+
				"for what they presented, or whom it named, to land",
				shape.what, got, shape.want)
		}
	}
}

// ONE ROW PER NODE PER MINUTE, counting each source, published only once the
// minute has CLOSED.
//
// The row count is the engine's decision and the attempt count is the
// caller's; this is the property that keeps the second from becoming the
// first. A throttled attempt is a failure like any other on the row, and the
// metric is where the method and the outcome are told apart.
//
// Mutation: publish from Failed and the first assertion fails with a row per
// attempt; flush the open minute and the second does; publish a row per
// source and the closed minute has two.
func TestOneRowPerNodePerMinute(t *testing.T) {
	t.Parallel()
	trail, pub, clk, count := newTrail(t)
	ctx := context.Background()

	for range 40 {
		trail.Failed(ctx, Failure{Source: "203.0.113.9", Method: types.FailPassword})
	}
	trail.Failed(ctx, Failure{Source: "203.0.113.9", Method: types.FailBearer})
	trail.Failed(ctx, Failure{Source: "203.0.113.9", Method: types.FailPassword,
		Throttled: true})
	trail.Failed(ctx, Failure{Source: "198.51.100.4", Method: types.FailInvite})
	if got := len(pub.published()); got != 0 {
		t.Fatalf("%d rows published on the request path: a failed attempt must "+
			"never publish, or the caller who failed paces the estate", got)
	}

	trail.Flush(ctx)
	if got := len(pub.published()); got != 0 {
		t.Fatalf("%d rows published for a minute still open: its row would be "+
			"one of several for the same minute", got)
	}

	// The next minute opens; one more attempt lands in it.
	clk.Set(noon.Add(time.Minute))
	trail.Failed(ctx, Failure{Source: "203.0.113.9", Method: types.FailPassword})
	trail.Flush(ctx)
	rows := failuresOf(t, pub.published())
	if len(rows) != 1 {
		t.Fatalf("published %d rows for the closed minute, want one for the "+
			"node: %+v", len(rows), rows)
	}
	row := rows[0]
	if !row.Minute.Equal(noon.Truncate(time.Minute)) {
		t.Errorf("row minute = %v, want the minute the attempts fell in", row.Minute)
	}
	want := []types.SourceFailures{
		{Source: "203.0.113.9", Failures: 42}, {Source: "198.51.100.4", Failures: 1},
	}
	if !slices.Equal(row.Sources, want) || row.Overflow != 0 {
		t.Errorf("row = %+v, want %v — every source's count, most first", row, want)
	}

	// AND THE COUNTER SAW EVERY ONE, by method and outcome, which is where
	// the rate and the door live.
	if got := count.seen[metrics.AuthAttemptsFailed+"|password|refused"]; got != 41 {
		t.Errorf("password refusals counted = %d, want 41", got)
	}
	if got := count.seen[metrics.AuthAttemptsFailed+"|password|throttled"]; got != 1 {
		t.Errorf("password throttled counted = %d, want 1", got)
	}

	// The minute that is still open is published by a later flush, alone.
	clk.Set(noon.Add(2 * time.Minute))
	trail.Flush(ctx)
	if got := len(pub.published()); got != 2 {
		t.Errorf("after the second minute closed, %d rows in total, want 2", got)
	}
}

// PAST THE PER-MINUTE CAP, SOURCES FOLD INTO ONE OVERFLOW COUNT.
//
// Without it a minute's row — and the memory it is counted in — grows with the
// attacker's address pool. A source already named keeps its name.
//
// Mutation: name every source regardless and the row names 74; fold a named
// source's later failure into the overflow and its count stays at one.
func TestSourcesPastTheCapFoldIntoTheOverflow(t *testing.T) {
	t.Parallel()
	trail, pub, clk, _ := newTrail(t)
	ctx := context.Background()
	const extra = 10
	for i := range MaxSourcesPerMinute + extra {
		trail.Failed(ctx, Failure{Source: "2001:db8::" + strconv.Itoa(i),
			Method: types.FailBearer})
	}
	trail.Failed(ctx, Failure{Source: "2001:db8::0", Method: types.FailBearer})
	clk.Set(noon.Add(time.Minute))
	trail.Flush(ctx)
	rows := failuresOf(t, pub.published())
	if len(rows) != 1 {
		t.Fatalf("%d rows, want one", len(rows))
	}
	row := rows[0]
	if len(row.Sources) != MaxSourcesPerMinute || row.Overflow != extra {
		t.Errorf("row names %d sources with overflow %d, want %d and %d",
			len(row.Sources), row.Overflow, MaxSourcesPerMinute, extra)
	}
	if first := row.Sources[0]; first.Source != "2001:db8::0" || first.Failures != 2 {
		t.Errorf("the busiest source is %+v, want the named one still counted "+
			"under its name", first)
	}
	if got := row.Total(); got != MaxSourcesPerMinute+extra+1 {
		t.Errorf("the row totals %d, want every attempt", got)
	}
}

// A STOPPING NODE PUBLISHES THE MINUTE IT IS HOLDING, open or not.
func TestRunFlushesWhatItHoldsWhenItStops(t *testing.T) {
	t.Parallel()
	trail, pub, _, _ := newTrail(t)
	trail.Failed(context.Background(), Failure{Source: "c", Method: types.FailPassword})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { trail.Run(ctx); close(done) }()
	cancel()
	<-done
	published := pub.published()
	if len(published) != 1 {
		t.Fatalf("%d rows after stop, want the open minute's one", len(published))
	}
	if pub.dead[0] {
		t.Error("the final flush published on the cancelled context, which a " +
			"real broker refuses — a teardown takes context.WithoutCancel")
	}
}

// AN EVENT OUTLIVES THE REQUEST THAT CAUSED IT, and carries this node's id.
//
// A client hanging up the moment its sign-in succeeds cancels the request's
// context; publishing on it would erase the row. Mutation: publish on ctx
// itself and dead[0] goes true.
func TestAnEventOutlivesTheRequestThatCausedIt(t *testing.T) {
	t.Parallel()
	trail, pub, _, _ := newTrail(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	trail.Emit(ctx, types.IAMSessionStarted{Person: "p-1", Method: types.SignInPassword})
	published := pub.published()
	if len(published) != 1 {
		t.Fatalf("published %d, want 1", len(published))
	}
	if pub.dead[0] {
		t.Error("the publish inherited the request's cancellation")
	}
	if published[0].Source != "node-a" {
		t.Errorf("source = %q, want this node's id", published[0].Source)
	}
	if pub.topics[0] != topics.Event("iam_session_started") {
		t.Errorf("topic = %q, want the ordinary event subject", pub.topics[0])
	}
}

// A TRAIL IS REFUSED WITHOUT WHAT IT CANNOT WORK WITHOUT.
func TestNewRefusesAMissingDependency(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{Node: "n"}); err == nil {
		t.Error("a trail with no publisher was built")
	}
	if _, err := New(Options{Publisher: &recorder{}}); err == nil {
		t.Error("a trail with no node id was built")
	}
}
