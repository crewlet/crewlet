package authevents

import (
	"context"
	"crypto/md5"  //nolint:gosec // the digests a leak would take the shape of, computed to be searched for
	"crypto/sha1" //nolint:gosec // the same
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

// NOTHING AN ATTEMPT PRESENTED LEAVES THE PROCESS — not the value, and not an
// unsalted hash of it.
//
// Two halves, because they fail differently. The TYPE half walks the payload's
// wire keys against the list of things this row is allowed to say: a field
// added to carry "just a hash of the subject" is a new key, and it is caught
// here whatever it is called. The INSTANCE half publishes a real row from
// attempts whose presented values are distinctive — one of them a password
// typed into the login box, which is exactly where a real one ends up — and
// searches the bytes that went to the publisher for each value and for every
// unsalted digest of it an implementation would plausibly reach for.
//
// Mutation: put `hex(sha256(subject))` into the row's people and the instance
// half goes red naming the sha256 digest; add a `subject_digest` field to the
// payload and the type half goes red naming it.
func TestLoginFailuresCarryNoPresentedString(t *testing.T) {
	t.Parallel()

	// THE TYPE: every key the row can carry is one of these, and none of
	// them is a place the presented value could go.
	allowed := []string{"attempts", "client", "clients", "methods", "minute",
		"people", "subjects", "throttled"}
	rt := reflect.TypeFor[types.IAMLoginFailures]()
	var keys []string
	for i := range rt.NumField() {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		keys = append(keys, tag)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, allowed) {
		t.Errorf("iam_login_failures carries keys %v, want exactly %v: a new "+
			"key on a row whose rate an unauthenticated caller authors is a "+
			"new place for what they typed to land", keys, allowed)
	}

	// THE INSTANCE.
	trail, pub, clk, _ := newTrail(t)
	presented := []string{
		"jane.doe@example.com",
		"Tr0ub4dor&3-correct-horse",         // a password in the login box
		"cwl_pat_0192f00d_Zm9vYmFyYmF6cXV4", // a bearer somebody sprayed
	}
	for i, value := range presented {
		trail.Failed(context.Background(), Failure{
			Client: "203.0.113.9", Method: types.FailPassword, Subject: value,
			Person: "p-" + strconv.Itoa(i),
		})
	}
	clk.Set(noon.Add(time.Minute))
	trail.Flush(context.Background())
	published := pub.published()
	if len(published) != 1 {
		t.Fatalf("published %d rows, want one for the client's minute", len(published))
	}
	raw, err := json.Marshal(published[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range presented {
		for form, needle := range leakedForms(value) {
			if strings.Contains(string(raw), needle) {
				t.Errorf("the published row carries the presented value as %s "+
					"(%q): %s", form, needle, raw)
			}
		}
	}
	if got := failuresOf(t, published)[0].Subjects; got != len(presented) {
		t.Errorf("subjects = %d, want %d: the count is the one thing the "+
			"presented values may contribute", got, len(presented))
	}
}

// leakedForms is every shape a presented value could reach a row in: itself,
// its lower-cased form, and the unsalted digests a careless implementation
// would reach for, in the encodings they are usually written in.
func leakedForms(value string) map[string]string {
	out := map[string]string{"cleartext": value, "lower-cased": strings.ToLower(value)}
	sums := map[string][]byte{}
	s256 := sha256.Sum256([]byte(value))
	sums["sha256"] = s256[:]
	s1 := sha1.Sum([]byte(value)) //nolint:gosec // searched for, never used
	sums["sha1"] = s1[:]
	m5 := md5.Sum([]byte(value)) //nolint:gosec // searched for, never used
	sums["md5"] = m5[:]
	for name, sum := range sums {
		out[name+" hex"] = hex.EncodeToString(sum)
		out[name+" hex prefix"] = hex.EncodeToString(sum)[:16]
		out[name+" base64"] = base64.StdEncoding.EncodeToString(sum)
		out[name+" base64url"] = base64.RawURLEncoding.EncodeToString(sum)
	}
	return out
}

// ONE ROW PER CLIENT PER MINUTE, published only once the minute has CLOSED.
//
// The row count is the engine's decision and the attempt count is the
// caller's; this is the property that keeps the second from becoming the
// first. Mutation: publish from Failed and the first assertion fails with a
// row per attempt; flush the open minute and the second does.
func TestOneRowPerClientPerMinute(t *testing.T) {
	t.Parallel()
	trail, pub, clk, count := newTrail(t)
	ctx := context.Background()

	for range 40 {
		trail.Failed(ctx, Failure{Client: "203.0.113.9", Method: types.FailPassword,
			Subject: "jane.doe"})
	}
	trail.Failed(ctx, Failure{Client: "203.0.113.9", Method: types.FailBearer,
		Subject: "a-wrong-token"})
	trail.Failed(ctx, Failure{Client: "203.0.113.9", Method: types.FailPassword,
		Throttled: true})
	trail.Failed(ctx, Failure{Client: "198.51.100.4", Method: types.FailOIDC})
	if got := len(pub.published()); got != 0 {
		t.Fatalf("%d rows published on the request path: a failed attempt must "+
			"never publish, or the caller who failed paces the estate", got)
	}

	trail.Flush(ctx)
	if got := len(pub.published()); got != 0 {
		t.Fatalf("%d rows published for a minute still open: its row would be "+
			"one of several for the same client and minute", got)
	}

	// The next minute opens; one more attempt lands in it.
	clk.Set(noon.Add(time.Minute))
	trail.Failed(ctx, Failure{Client: "203.0.113.9", Method: types.FailPassword,
		Subject: "jane.doe"})
	trail.Flush(ctx)
	rows := failuresOf(t, pub.published())
	if len(rows) != 2 {
		t.Fatalf("published %d rows for the closed minute, want one per client "+
			"(2): %+v", len(rows), rows)
	}
	byClient := map[string]types.IAMLoginFailures{}
	for _, row := range rows {
		byClient[row.Client] = row
		if !row.Minute.Equal(noon.Truncate(time.Minute)) {
			t.Errorf("row minute = %v, want the minute the attempts fell in", row.Minute)
		}
	}
	guesser := byClient["203.0.113.9"]
	if guesser.Attempts != 41 || guesser.Throttled != 1 {
		t.Errorf("attempts/throttled = %d/%d, want 41/1", guesser.Attempts, guesser.Throttled)
	}
	if guesser.Subjects != 2 {
		t.Errorf("subjects = %d, want 2 (one login, one bearer)", guesser.Subjects)
	}
	if want := []types.FailureMethod{types.FailBearer, types.FailPassword}; !slices.Equal(guesser.Methods, want) {
		t.Errorf("methods = %v, want %v, sorted and distinct", guesser.Methods, want)
	}
	if byClient["198.51.100.4"].Attempts != 1 {
		t.Errorf("the second client's row = %+v", byClient["198.51.100.4"])
	}

	// AND THE COUNTER SAW EVERY ONE, which is where the rate lives.
	if got := count.seen[metrics.AuthAttemptsFailed+"|password|refused"]; got != 41 {
		t.Errorf("password refusals counted = %d, want 41", got)
	}
	if got := count.seen[metrics.AuthAttemptsFailed+"|password|throttled"]; got != 1 {
		t.Errorf("password throttled counted = %d, want 1", got)
	}

	// The minute that is still open is published by a later flush, alone.
	clk.Set(noon.Add(2 * time.Minute))
	trail.Flush(ctx)
	if got := len(pub.published()); got != 3 {
		t.Errorf("after the second minute closed, %d rows in total, want 3", got)
	}
}

// PAST THE PER-MINUTE CAP, CLIENTS FOLD INTO ONE ROW THAT SAYS HOW MANY.
//
// Without it the rows a minute writes are bounded by the attacker's address
// pool. Mutation: give every client its own row regardless and the count goes
// to 74.
func TestClientsPastTheCapFoldIntoOneRow(t *testing.T) {
	t.Parallel()
	trail, pub, clk, _ := newTrail(t)
	ctx := context.Background()
	const extra = 10
	for i := range MaxClientsPerMinute + extra {
		trail.Failed(ctx, Failure{Client: "2001:db8::" + strconv.Itoa(i),
			Method: types.FailBearer, Subject: "x" + strconv.Itoa(i)})
	}
	clk.Set(noon.Add(time.Minute))
	trail.Flush(ctx)
	rows := failuresOf(t, pub.published())
	if len(rows) != MaxClientsPerMinute+1 {
		t.Fatalf("%d rows, want %d named clients and one folded row",
			len(rows), MaxClientsPerMinute)
	}
	folded := rows[0] // "*" sorts before every digit and letter
	if folded.Client != "*" || folded.Clients != extra || folded.Attempts != extra {
		t.Errorf("folded row = %+v, want client * covering %d clients", folded, extra)
	}
}

// A COUNT AN ATTACKER DRIVES SATURATES AT ITS CAP rather than growing the
// memory it is counted in.
func TestCountsSaturateAtTheirCaps(t *testing.T) {
	t.Parallel()
	trail, pub, clk, _ := newTrail(t)
	ctx := context.Background()
	for i := range MaxSubjectsCounted + 50 {
		trail.Failed(ctx, Failure{Client: "c", Method: types.FailBearer,
			Subject: "token-" + strconv.Itoa(i), Person: "p-" + strconv.Itoa(i)})
	}
	clk.Set(noon.Add(time.Minute))
	trail.Flush(ctx)
	row := failuresOf(t, pub.published())[0]
	if row.Subjects != MaxSubjectsCounted {
		t.Errorf("subjects = %d, want it saturated at %d", row.Subjects, MaxSubjectsCounted)
	}
	if len(row.People) != MaxPeopleNamed {
		t.Errorf("people named = %d, want at most %d", len(row.People), MaxPeopleNamed)
	}
	if row.Attempts != MaxSubjectsCounted+50 {
		t.Errorf("attempts = %d: a saturated subject set must not stop the count",
			row.Attempts)
	}
}

// A STOPPING NODE PUBLISHES THE MINUTE IT IS HOLDING, open or not.
func TestRunFlushesWhatItHoldsWhenItStops(t *testing.T) {
	t.Parallel()
	trail, pub, _, _ := newTrail(t)
	trail.Failed(context.Background(), Failure{Client: "c", Method: types.FailPassword})
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

// ONCE PER KEY PER WINDOW, and the caller is told which call published.
func TestEmitOnceIsOncePerWindow(t *testing.T) {
	t.Parallel()
	trail, pub, clk, _ := newTrail(t)
	ctx := context.Background()
	payload := types.IAMTokenFirstUse{Token: "ops"}

	if !trail.EmitOnce(ctx, OnceTokenUse, "ops", time.Hour, payload) {
		t.Fatal("the first use in a window did not publish")
	}
	clk.Set(noon.Add(59 * time.Minute))
	if trail.EmitOnce(ctx, OnceTokenUse, "ops", time.Hour, payload) {
		t.Error("a second use inside the window published again")
	}
	if !trail.EmitOnce(ctx, OnceTokenUse, "ci", time.Hour, types.IAMTokenFirstUse{Token: "ci"}) {
		t.Error("a different key was suppressed by another's window")
	}
	if !trail.EmitOnce(ctx, OnceTokenOverreach, "ops", time.Hour, payload) {
		t.Error("one key in a DIFFERENT class was suppressed by the first " +
			"class's window: a use and an overreach are two facts")
	}
	clk.Set(noon.Add(time.Hour))
	if !trail.EmitOnce(ctx, OnceTokenUse, "ops", time.Hour, payload) {
		t.Error("the first use after the window closed did not publish")
	}
	if got := len(pub.published()); got != 4 {
		t.Errorf("published %d, want 4", got)
	}

	// A ZERO WINDOW IS THE LIFE OF THE PROCESS.
	if !trail.EmitOnce(ctx, OnceSessionEnded, "k", 0, payload) {
		t.Fatal("first call on a zero window did not publish")
	}
	clk.Set(noon.Add(24 * 365 * time.Hour))
	if trail.EmitOnce(ctx, OnceSessionEnded, "k", 0, payload) {
		t.Error("a zero window expired")
	}

	// A CLASS THIS BUILD KEEPS NO BOUND FOR PUBLISHES NOTHING, rather than
	// growing a set nothing bounds.
	if trail.EmitOnce(ctx, OnceClass("mystery"), "k", time.Hour, payload) {
		t.Error("a class with no bound published")
	}
}

// EVERY CLASS HAS A BOUND, and the bound is what the set holds to.
func TestEveryOnceClassIsBounded(t *testing.T) {
	t.Parallel()
	for _, class := range OnceClasses {
		if !class.Valid() || OnceBound(class) <= 0 {
			t.Errorf("class %q has no bound", class)
		}
	}
	if OnceClass("mystery").Valid() {
		t.Error("a class this build does not name reports itself valid")
	}
	trail, _, _, _ := newTrail(t)
	ctx := context.Background()
	payload := types.IAMTokenFirstUse{Token: "ops"}
	bound := OnceBound(OnceTokenUse)
	for i := range bound + 10 {
		trail.EmitOnce(ctx, OnceTokenUse, "t-"+strconv.Itoa(i), time.Hour, payload)
	}
	if got := trail.held(OnceTokenUse); got != bound {
		t.Errorf("the token class holds %d keys, want its bound of %d", got, bound)
	}
}

// ONE CLASS AT ITS BOUND NEVER EVICTS ANOTHER'S KEY.
//
// The dedupe was one set, evicting whichever entry's window ended soonest —
// which is never a key remembered for the life of the process. So a node that
// had seen as many deadline endings as the set held evicted a REPLAY's key on
// every further ending, and the next presentation of the replayed cookie
// published another reuse row and revoked again. Mutation: share one set
// between the classes and the replay publishes twice.
func TestAFullClassEvictsOnlyItsOwnKeys(t *testing.T) {
	t.Parallel()
	trail, _, clk, _ := newTrail(t)
	ctx := context.Background()
	payload := types.IAMSessionEnded{Reason: types.EndAbsolute}
	for i := range OnceBound(OnceSessionEnded) - 1 {
		trail.EmitOnce(ctx, OnceSessionEnded, "ended-"+strconv.Itoa(i), 0, payload)
	}
	replay := types.IAMSessionReuseDetected{Lineage: "A"}
	if !trail.EmitOnce(ctx, OnceSessionReuse, "A", 8*time.Hour, replay) {
		t.Fatal("the replay's first presentation did not publish")
	}
	if !trail.EmitOnce(ctx, OnceTokenUse, "ops", time.Hour,
		types.IAMTokenFirstUse{Token: "ops"}) {
		t.Fatal("the token's first use did not publish")
	}
	// A WHOLE BOUND'S WORTH MORE, so no eviction order that shared one set
	// between the classes could have kept the replay's key.
	for i := range OnceBound(OnceSessionEnded) + 1 {
		clk.Set(noon.Add(time.Duration(i) * time.Millisecond))
		trail.EmitOnce(ctx, OnceSessionEnded, "later-"+strconv.Itoa(i), 0, payload)
	}
	if trail.EmitOnce(ctx, OnceSessionReuse, "A", 8*time.Hour, replay) {
		t.Error("a deadline ending evicted the replay's key, so the replay " +
			"published — and revoked — a second time")
	}
	if trail.EmitOnce(ctx, OnceTokenUse, "ops", time.Hour,
		types.IAMTokenFirstUse{Token: "ops"}) {
		t.Error("a deadline ending evicted the token's hour")
	}
	if got, bound := trail.held(OnceSessionEnded), OnceBound(OnceSessionEnded); got != bound {
		t.Errorf("the ended class holds %d, want its bound of %d", got, bound)
	}
}

// AT ITS BOUND A CLASS FORGETS WHAT HAS EXPIRED FIRST, and only then the key
// it claimed longest ago.
func TestAFullClassForgetsTheExpiredBeforeTheOldest(t *testing.T) {
	t.Parallel()
	trail, _, clk, _ := newTrail(t)
	ctx := context.Background()
	payload := types.IAMSessionReuseDetected{}
	bound := OnceBound(OnceSessionReuse)
	trail.EmitOnce(ctx, OnceSessionReuse, "oldest", 8*time.Hour, payload)
	trail.EmitOnce(ctx, OnceSessionReuse, "short", time.Minute, payload)
	for i := range bound - 2 {
		trail.EmitOnce(ctx, OnceSessionReuse, "long-"+strconv.Itoa(i), 8*time.Hour, payload)
	}
	clk.Set(noon.Add(2 * time.Minute))
	trail.EmitOnce(ctx, OnceSessionReuse, "one-more", 8*time.Hour, payload)
	if trail.EmitOnce(ctx, OnceSessionReuse, "oldest", 8*time.Hour, payload) {
		t.Error("at the bound the class evicted a live key while an expired " +
			"one was there to go")
	}
	// Full again, nothing expired: the key claimed longest ago goes.
	trail.EmitOnce(ctx, OnceSessionReuse, "and-another", 8*time.Hour, payload)
	if !trail.EmitOnce(ctx, OnceSessionReuse, "oldest", 8*time.Hour, payload) {
		t.Error("with nothing expired, the class kept its oldest claim")
	}
	if got := trail.held(OnceSessionReuse); got > bound {
		t.Errorf("the class holds %d keys, past its bound of %d", got, bound)
	}
}

// A CLAIM HANDED BACK CAN BE TAKEN AGAIN, and handing back is the claim's own.
func TestAReleasedClaimCanBeTakenAgain(t *testing.T) {
	t.Parallel()
	trail, pub, clk, _ := newTrail(t)
	ctx := context.Background()
	release, claimed := trail.Claim(ctx, OnceSessionEnded, "L", 0)
	if !claimed {
		t.Fatal("the first claim was not taken")
	}
	if _, again := trail.Claim(ctx, OnceSessionEnded, "L", 0); again {
		t.Error("a held key was claimed twice")
	}
	release()
	release()
	if _, again := trail.Claim(ctx, OnceSessionEnded, "L", 0); !again {
		t.Error("a released key could not be claimed again")
	}
	if len(pub.published()) != 0 {
		t.Error("a claim published something")
	}
	// A stale release never undoes a LATER claim of the same key.
	stale, _ := trail.Claim(ctx, OnceTokenUse, "ops", time.Minute)
	clk.Set(noon.Add(2 * time.Minute))
	if _, fresh := trail.Claim(ctx, OnceTokenUse, "ops", time.Minute); !fresh {
		t.Fatal("an expired key could not be claimed again")
	}
	stale()
	if _, again := trail.Claim(ctx, OnceTokenUse, "ops", time.Minute); again {
		t.Error("an earlier claim's release handed back a later one")
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
