// Package jsprovision is the one place that decides how long a replicated
// JetStream create gets, how often a forming cluster is re-asked, and what
// "the cluster is still forming" looks like on the wire.
//
// # Why this is a package rather than a constant beside each caller
//
// Two subsystems provision replicated JetStream objects at boot and they are
// the same call underneath: [internal/queue/jetstream] creates the engine's
// streams and durable consumers, and [internal/coord/kv] creates the
// coordination buckets — a bucket IS a stream. Written twice, the rule drifted
// in the way this repository has already paid for elsewhere: the two carried
// their own spelling of the same five decisions (the per-create budget, the
// read-back budget, the retry cadence, JetStream's "no suitable peers" code,
// and the predicate over it), each doc comment asserting it matched the other
// with nothing enforcing that it did. When the clustered case turned out to
// need a larger budget, both would have had to learn it separately.
//
// It imports the JetStream client and nothing from the rest of the engine, so
// either side can take it without taking the other.
//
// # Why the clustered budget is four times the solo one
//
// A SOLO create is local file-store setup: no peers, no raft, no election. A
// CLUSTERED one is a raft round trip against a metadata group whose members
// are themselves still booting, on a host bringing several of them up at once
// — and on a fleet every node issues the whole sequence at the same instant.
// One number for both is wrong for one of them, which is the conclusion the
// embedded broker's own start had already reached: [internal/queue/jetstream]
// gave a clustered member two minutes to a solo server's thirty seconds for
// exactly this symptom, while the provisioning calls DOWNSTREAM of it kept the
// flat thirty. (That start budget has since moved off the store recovery it
// was really bounding — see that package's listenTimeout — but the contention
// it measured is the one a create meets.)
//
// Thirty seconds was measured failing: over a two-day window the engine's own
// CI lost a cluster-start attempt in roughly two runs in five and failed the
// end-to-end job outright five times, every one of them a single create
// blowing this budget while its peer was still coming up — a different object
// each time, which is what says the object was never the problem. Reproduced
// on a four-core host under the race detector: the same create completes in
// seconds idle and exceeds thirty under ordinary CPU contention.
//
// # And why the sequence is bounded separately
//
// Each budget below bounds ONE create, and a boot makes many of them in a row
// — every coordination bucket [internal/coord/kv]'s OpenFleet and Open
// provision, and the engine's own streams beside them.
// Nothing bounded the sequence, so the real worst case was already the product
// rather than the term, and raising the term alone would have multiplied it.
// [SequenceBudget] is the wall-clock ceiling over a whole bring-up; a caller
// applies it once to the context it passes down, and because
// [context.WithTimeout] only ever shortens, each create inside then takes the
// lesser of its own budget and what is left of the sequence's.
//
// # Every duration in one value, so the mechanism can be run at any scale
//
// [Timing] carries every duration this package decides, and [Clustered.Timing]
// is the one place a topology is turned into them. [Settle], [Ask], [Read],
// [ReadBounded] and [Place] are [SettleWith], [Timing.Ask], [ReadWith],
// [ReadBoundedWith] and [Timing.Place] at the production numbers; a caller
// that holds a [Timing] of its own — a test, through the one seam its package
// keeps for it — runs the same branches in milliseconds.
//
// It exists because the branches that matter most are the ones only an
// EXHAUSTED budget reaches — a read-back after a create the server held, a
// probe nobody answered for its whole ceiling — and at the production numbers
// a case proving one waits out two minutes. The alternative was a seam per
// number, each spelled by whichever package needed it first: one exported
// field on the queue's configuration that nothing in the engine set, and
// nothing at all for the rest, so their cases waited the numbers out. The
// numbers themselves are pinned where they are decided, here, by tests that
// read them rather than wait for them.
//
// # A read is asked again with every request still out, a write is not
//
// A READ (does this stream, consumer or bucket exist) is answered when a
// worker takes it from the server's API queue, or never. [ReadWith] therefore
// presumes a request lost after [Timing.ReadTerm] and asks again after
// [Timing.ReAsk] WITH EVERY REQUEST STILL OUTSTANDING, taking the first answer:
// a request behind a backed-up queue is answered late rather than never, and
// one thrown away at its term turns a slow leader into a cliff. This holds for
// every read only a leader answers, because a member that still believes in a
// lost leader stays silent. A WRITE's reply is held until the object has a
// leader and its repeat is a second fact, so [Timing.Ask] waits [Timing.AskTerm]
// and abandons the one request before asking again. A read is never asked past
// [LookupBudget], and nothing is asked again once its deadline has passed,
// whether or not its context says so yet: a timer runs late on a stalled
// process, and a request re-sent past the bound is one its caller gave up on.
// A hedged state read opens a stream handle of its own per request: the
// client's handle caches its info with no lock, and two requests may be out.
// Hedging costs the server a request per re-ask, so a leader slower than the
// first re-ask meets more of them.
//
// [Settle] owns its read-back window and takes the context that bounds the
// BOOT: a read-back often exists because the create's deadline expired, so one
// gated on that deadline would be dead code on the path it was written for.
//
// # A group with no leader is waited out, not answered
//
// Silence is not the only sign of an election. A member that KNOWS its stream's
// group, or the metadata group, has no leader answers 503, err_code 10008
// (server/jetstream_api.go, isGroupLeaderless and isLeaderless), and a publish
// to a stream nobody leads is heard by nobody, since only the leader subscribes
// its subjects (server/stream.go, setLeader); the same answer to a stream that
// is not there is that absence, so a caller first finds the stream ([Unheard]).
// Both say an election is under way, not what the caller asked about, so
// [ThroughElection] asks again inside the caller's deadline with no count of
// its own ([Leaderless] is the one classification). A caller with no deadline is held to one [Election], the
// longest the pinned server's raft defaults leave a group without a leader it
// can be heard from. It ASKS AGAIN at [ReAsk] rather than waking on a signal:
// a stream's LEADER_ELECTED advisory is published in its account, but the
// metadata group's, which a consumer's create waits on, only in the system
// account (server/jetstream_cluster.go, sendDomainLeaderElectAdvisory), where
// no client of the engine listens; and the answer comes back in a round trip.
// A wait that ends is [*NoLeader], naming the stream.
package jsprovision

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// soloBudget bounds one create on a server with no peers: its own
	// file store, and nothing else.
	soloBudget = 30 * time.Second

	// clusterBudget bounds one create that is a raft round trip against a
	// metadata group whose members may still be booting.
	//
	// TWO MINUTES: what a clustered member's start gets for the same
	// contention (peers booting on one host), and this call sits downstream
	// of that start and the sixty-second readiness wait. Failing here fails
	// the whole boot, so too short turns a busy host into a node that
	// refuses to start and works on retry, while too long only reports a
	// wedged cluster later, and the wait is cancellable.
	clusterBudget = 2 * time.Minute
)

// Budget is how long ONE replicated create gets.
func Budget(clustered bool) time.Duration {
	if clustered {
		return clusterBudget
	}
	return soloBudget
}

const (
	// soloSequenceBudget bounds a whole bring-up with no peers.
	//
	// TWO MINUTES rather than tighter, although every create is local
	// file-store setup measured in milliseconds. A number here can only do
	// harm by being too SMALL (it would fail a boot that works), so it
	// leaves roughly a thousandfold headroom while staying a real bound,
	// a fraction of what the per-create budgets alone would allow.
	soloSequenceBudget = 2 * time.Minute

	// clusterSequenceBudget bounds a whole clustered bring-up.
	//
	// FIVE MINUTES: one slow create can spend its whole two-minute budget
	// and the rest still have room (the group has proven itself by then).
	// Above the ninety seconds of bounded waiting a clustered boot already
	// tolerates (thirty-second listener budget plus sixty-second readiness
	// wait), and below where a wedged cluster outlasts whatever watches
	// the node come up.
	clusterSequenceBudget = 5 * time.Minute
)

// SequenceBudget is how long a whole bring-up of many creates gets.
func SequenceBudget(clustered bool) time.Duration {
	if clustered {
		return clusterSequenceBudget
	}
	return soloSequenceBudget
}

// LookupBudget is the CEILING over one existence probe (the read that decides
// create-versus-observe, re-issued by [Read]) and over every other read this
// package hedges ([ReadWith], [ReadBoundedWith]): the longest silence the
// server keeps, unlike a create's [Budget] (slow only while the group cannot
// answer). An unanswered read never will be, so it bounds how long the REASON
// lasts, re-asked every [ReadTerm]/[ReAsk]: election maxElectionTimeout (9s)
// plus lostQuorumInterval and lostQuorumCheckInterval (10s each), after which
// a leaderless group ANSWERS with JSClusterNotAvailError. Past that sum, 29s,
// any state answers; a read unanswered at thirty loses nothing, the create
// decides ([Unanswered]). One constant: a solo lookup is hang detection.
const LookupBudget = 30 * time.Second

// Lookup asks one, a metadata read of an object that should exist (a bucket's,
// a stream's, a consumer's), the way every lookup is asked: through [Read],
// re-asked while nobody answers, for at most [LookupBudget] of ctx, and it
// returns the value of the request that answered.
//
// One request fails a lookup of an object that is there (only its leader
// answers, and nobody does during an election). THE ANSWER COMES BACK THROUGH
// THE READ, never a shared variable: a cancelled request ends AFTER the one
// that answered ([ReadWith]) and would write its nil over the answer.
func Lookup[T any](ctx context.Context, one func(context.Context) (T, error)) (T, error) {
	return lookupWith(ctx, Clustered(false).Timing(), one)
}

// lookupWith is [Lookup] at t's cadences: [ReadWith], which holds every read
// to t.Lookup of ctx, with nobody told of a request presumed lost.
func lookupWith[T any](ctx context.Context, t Timing, one func(context.Context) (T, error)) (T, error) {
	return ReadWith(ctx, t, one, nil)
}

// LookupBucket binds the key-value bucket named bucket through js, looked up
// as [Lookup] asks.
func LookupBucket(ctx context.Context, js jetstream.JetStream, bucket string) (jetstream.KeyValue, error) {
	return Lookup(ctx, func(ctx context.Context) (jetstream.KeyValue, error) {
		return js.KeyValue(ctx, bucket)
	})
}

// LookupStream is the info of the stream named name AS ITS LEADER ANSWERED
// IT, looked up as [Lookup] asks.
//
// A replicated stream's info that names NO LEADER is a member's own copy,
// which a member answers once its group has been leaderless for a while: its
// last sequence and its state can be short of what the quorum committed. That
// answer is asked again like no answer at all ([leaderAnswered]), so what
// comes back is the leader's. A stream on one server always names it.
func LookupStream(ctx context.Context, js jetstream.JetStream, name string) (*jetstream.StreamInfo, error) {
	info, err := Lookup(ctx, func(ctx context.Context) (*jetstream.StreamInfo, error) {
		stream, err := js.Stream(ctx, name)
		if err != nil {
			return nil, err
		}
		info := stream.CachedInfo()
		if err := leaderAnswered(name, info); err != nil {
			return nil, err
		}
		return info, nil
	})
	if err != nil {
		return nil, err
	}
	return info, nil
}

// errNoLeaderAnswer is a stream's info that a member answered from its own
// copy, naming no leader. It wraps [nats.ErrTimeout], so [Unanswered] reads
// it as no answer and [ReadWith] asks again.
var errNoLeaderAnswer = fmt.Errorf("answered by a member while the stream has no leader: %w", nats.ErrTimeout)

// leaderAnswered is nil for info its leader answered (or a stream with no
// cluster to elect one), and [errNoLeaderAnswer] for a member's own copy.
func leaderAnswered(name string, info *jetstream.StreamInfo) error {
	if info == nil {
		return fmt.Errorf("stream %s: the answer carried no info", name)
	}
	if info.Cluster != nil && info.Cluster.Leader == "" {
		return fmt.Errorf("stream %s: %w", name, errNoLeaderAnswer)
	}
	return nil
}

// Timing is every duration a provisioning call waits on, for one topology.
//
// [Clustered.Timing] is the production value; a test builds its own at a
// scale it can afford, keeping the relations between fields.
//
// THE ZERO VALUE IS REFUSED: zero is a read-back that never asks, a loop that
// never waits, a deadline already past. EVERY METHOD REFUSES ANY UNSET FIELD,
// naming the first, Budget, Sequence and Lookup included (a caller derives
// contexts from them; a missing one ran on an expired deadline reported as
// the broker's answer). Progress's zero is a setting: nil is a read whose
// ceilings run on time alone ([Timing.Following]); Beyond is refused unset
// only beside a Progress.
type Timing struct {
	// Budget bounds one create — see [Budget].
	Budget time.Duration
	// Sequence bounds a whole bring-up of many creates — see
	// [SequenceBudget].
	Sequence time.Duration
	// Lookup is the ceiling over one read, re-asks included, whatever its
	// caller set — see [LookupBudget] and [ReadWith].
	Lookup time.Duration
	// AskTerm is how long one metadata WRITE waits before it is presumed
	// destroyed and re-issued — see [AskTerm].
	AskTerm time.Duration
	// ReAsk is the pause before a request nobody answered is sent again —
	// see [ReAsk].
	ReAsk time.Duration
	// ReadBack bounds a whole read-back — see [ReadBack].
	ReadBack time.Duration
	// ReadTerm is how long one metadata READ waits before it is presumed
	// dropped and the next is sent beside it — every request of a
	// read-back and of [ReadWith] — see [ReadTerm].
	ReadTerm time.Duration
	// PlacementRetry is how often an answered refusal is asked again — see
	// [PlacementRetry].
	PlacementRetry time.Duration

	// Progress, when set, is the link a read crosses, whose movement holds
	// the read's ceilings past their spans ([Timing.Ceiling]), by Beyond at
	// most — see [Timing.Following]. Nil for a read that crosses none, or
	// whose ceilings run on time alone: every timing [Clustered.Timing]
	// builds, and every one its caller does not hand a link.
	Progress Progress
	// Beyond is the most a moving link holds a ceiling past its span — see
	// [Timing.Following]. Required with Progress, and read with nothing else.
	Beyond time.Duration
}

// Timing is every provisioning duration for a broker of this topology.
func (c Clustered) Timing() Timing {
	return Timing{
		Budget:         c.Budget(),
		Sequence:       c.SequenceBudget(),
		Lookup:         LookupBudget,
		AskTerm:        c.AskTerm(),
		ReAsk:          ReAsk,
		ReadBack:       ReadBack,
		ReadTerm:       ReadTerm,
		PlacementRetry: PlacementRetry,
	}
}

// span is one of a [Timing]'s durations, named for the refusal [refuseUnset]
// gives when it is not set.
type span struct {
	name string
	d    time.Duration
}

// unset is the refusal every method gives a [Timing] with any field left
// unset — see [Timing]'s doc for why a method refuses fields it does not read.
func (t Timing) unset() error {
	if err := refuseUnset(span{"Budget", t.Budget}, span{"Sequence", t.Sequence},
		span{"Lookup", t.Lookup}, span{"AskTerm", t.AskTerm},
		span{"ReAsk", t.ReAsk}, span{"ReadBack", t.ReadBack},
		span{"ReadTerm", t.ReadTerm}, span{"PlacementRetry", t.PlacementRetry}); err != nil {
		return err
	}
	return t.unfollowed()
}

// Arriving is t for a read whose answer takes arrival to reach its reader
// whole once the broker has the request: a leader's reply of megabytes, built,
// carried slowly and decoded, none of it heard until its last byte is in
// (internal/jsapi's Arrival). A request is presumed lost only once arrival has
// passed beyond the read term, and the read runs no longer than the longest
// silence AND that arrival. A reply still arriving is not silence
// ([LookupBudget] bounds only NOTHING said; six megabytes on the slowest link
// takes four minutes), and one still being built is not lost: re-asking made
// the leader build, and the link carry, a second whole reply beside the first.
// So a read asks once while its reply can still be on its way: never build,
// nor carry, one reply twice (crewlet/crewlet#1149, #1153).
func (t Timing) Arriving(arrival time.Duration) Timing {
	t.ReadTerm += arrival
	t.Lookup += arrival
	return t
}

// refuseUnset names the first of spans that is not a positive duration.
func refuseUnset(spans ...span) error {
	for _, s := range spans {
		if s.d <= 0 {
			return fmt.Errorf("jsprovision: Timing.%s is %v — a provisioning timing "+
				"is built by Clustered.Timing, and one built by hand sets every "+
				"field", s.name, s.d)
		}
	}
	return nil
}

// withTerm is the production timing with term as one request's term, which is
// what the package-level [Ask] and [Place] run at.
//
// EITHER TOPOLOGY'S, because nothing else those two read branches on one: the
// cadences are the server's, and the term — the one duration that does — is
// the caller's argument.
func withTerm(term time.Duration) Timing {
	t := Clustered(false).Timing()
	t.AskTerm = term
	return t
}

// Settle is [SettleWith] at the production cadences, which no topology
// changes.
func Settle[T any](ctx context.Context, ask func(context.Context) (T, error)) (T, error) {
	return SettleWith(ctx, Clustered(false).Timing(), ask)
}

// SettleWith re-runs ask while it answers [NotYetVisible], for up to
// [ReadBack], at t's cadences.
//
// A create is visible to a member only on its next metadata update, so every
// read-back runs in a not-found window. ctx IS THE BOOT'S, NEVER THE
// CREATE'S: a read-back often runs because the create's deadline EXPIRED. The
// [ReadBack] window is this function's, over the WHOLE read-back. Questions go
// as every read ([ReadWith], [hedge]); an unanswered one is not "not there"
// ([Unanswered]). Not-yet-visible is an ANSWER, re-asked at [PlacementRetry];
// a DESTROYED request needs another member to hold the group, at [ReAsk]
// (merged, `nats.ErrNoResponders` is asked four times a second per object).
// Returns the ASK'S OWN error and asks nothing past the window ([pause]).
func SettleWith[T any](ctx context.Context, t Timing, ask func(context.Context) (T, error)) (T, error) {
	var zero T
	if err := t.unset(); err != nil {
		return zero, err
	}
	window, cancel := context.WithTimeout(ctx, t.ReadBack)
	defer cancel()

	var absent error
	for {
		// ONE QUESTION, asked until something answers it: a request
		// nobody replied to is the third answer and not the second, so
		// it never comes back here while the window is open — the hedge
		// re-asks it at a destroyed request's own interval ([ReAsk]),
		// and reports the silence only once the window closes.
		v, err := hedge(window, t, 0, ask, nil)
		switch {
		case err == nil:
			return v, nil
		case NotYetVisible(err):
			absent = err
		case window.Err() != nil && silent(err):
			// THE WINDOW CLOSED with the question unanswered, so what
			// came back describes the silence or this function's
			// patience rather than the object. What the object last
			// said outranks it, and the silence that names something
			// outranks the deadline ([heard]).
			return zero, heard(absent, err)
		default:
			return zero, err
		}
		// THE CADENCE FOLLOWS THE CONDITION, never the loop: a not-found
		// is an answer that clears on this member's next metadata update.
		// And none is asked past the window, though the window cannot say
		// yet that it has closed ([pause]).
		if !pause(window, t.PlacementRetry) {
			return zero, heard(absent, nil)
		}
	}
}

// namedSilence keeps whichever of two unanswered errors NAMES something.
//
// An attempt that expires on [ReadTerm] is unanswered, and it is also a bare
// [context.DeadlineExceeded] — which names neither the object nor the broker,
// and is the shape [Settle] promises never to report. [nats.ErrNoResponders]
// and [nats.ErrTimeout] say WHY nobody replied, so once one of those has been
// heard it is what the caller is told, however many terms expire after it.
//
// Without this the retained silence was overwritten by the next term to
// expire, and a read-back that began with "the broker's JetStream is not
// serving yet" ended as "context deadline exceeded" — the undiagnosable answer
// [LookupBudget] records having produced, arriving by a different route.
func namedSilence(held, err error) error {
	if held != nil && errors.Is(err, context.DeadlineExceeded) {
		return held
	}
	return err
}

// heard is what [Settle] reports when its window closes: what the object said,
// or the silence, in that order.
//
// AN ANSWER OUTRANKS SILENCE. A not-found is the object speaking and names it;
// an unanswered request names only the broker that would not route to it. Both
// beat this function's own deadline, which names neither, and BOTH EXITS TAKE
// THE SAME RULE from here — written at each of them, the mid-ask exit is
// exactly where it drifted.
func heard(absent, unanswered error) error {
	if absent != nil {
		return absent
	}
	return unanswered
}

// ReadTerm is how long ONE metadata READ — a stream's or a consumer's info —
// waits before it is presumed dropped and the next is sent ([SettleWith],
// [ReadWith]); the request stays outstanding ([hedge]). A read is answered
// when the server PROCESSES it or never (jsStreamInfoRequest,
// jsConsumerInfoRequest), and an object in flight during a boot is answered
// by no member but the one preferred to lead it, so a term spanning an
// election waits for a reply that does not exist. ONE SECOND is the server's
// hbInterval (server/raft.go), the shortest interval over which a group's
// state can change: a sooner re-send is dropped again, and a dropped read
// costs a second and a [ReAsk], not an election. No topology branch, unlike
// [AskTerm]: a solo broker drops no read. The test holds that two hung asks
// fit inside [ReadBack], not a count.
const ReadTerm = time.Second

// Place runs create until the cluster stops refusing to place the object, or a
// consumer of its name stops closing ([ConsumerClosing]), while ctx allows.
//
// One loop for every caller: a consumer create that returned on the first
// [Unplaceable] refusal, while streams and buckets retried at
// [PlacementRetry], failed a boot on "no suitable peers" while the group was
// still forming. awaiting is called ONCE, on the first placement refusal, to
// name the object ([WhenSlow]'s reason: not a line per poll); a closing
// consumer awaits no peer. THE ERROR IT RETURNS IS THE CREATE'S OWN, never the
// context's. Each attempt is [Timing.Ask]'d at t.AskTerm, a refusal re-asked
// every t.PlacementRetry, and never once ctx's deadline has passed ([pause]).
func (t Timing) Place(ctx context.Context, create func(context.Context) error, awaiting func()) error {
	if err := t.unset(); err != nil {
		return err
	}
	announced := false
	for {
		// THROUGH [Ask], because a create is a metadata request like
		// any other and the server drops those. Without it a create
		// whose request was destroyed sat on this loop's single call
		// until the whole clustered budget expired, and the refusal
		// this loop waits for never arrived to be waited for — so the
		// budget bought the create the same nothing the missing
		// placement retry once bought the consumer.
		err := t.Ask(ctx, create, nil)
		if err == nil || (!Unplaceable(err) && !ConsumerClosing(err)) {
			return err
		}
		if !announced && awaiting != nil && Unplaceable(err) {
			awaiting()
			announced = true
		}
		if !pause(ctx, t.PlacementRetry) {
			return err
		}
	}
}

// Place is [Timing.Place] at the production cadences, each attempt asked for
// term.
func Place(ctx context.Context, term time.Duration,
	create func(context.Context) error, awaiting func()) error {

	return withTerm(term).Place(ctx, create, awaiting)
}

// Ask runs one idempotent metadata request, RE-ISSUING it while the broker
// does not answer, for as long as ctx allows. The reply does not exist, not
// late: nats-server drops a routed request when
// a member that is not the metadata leader has no assignment for the object
// (the `sa == nil` arm of jsStreamInfoRequest), and apiDispatch DRAINS its
// queue at JSDefaultRequestQueueLimit. ASKING AGAIN helps, a longer deadline
// does not; each attempt gets its own context (a spent one is dead on
// arrival). Only [Unanswered] is re-asked: any answer returns at once, so a
// configuration mistake never loops and [Place] sees its refusal. again, if
// non-nil, gets the requests sent. A request waits t.AskTerm (a WRITE's term,
// [AskTerm]) and is abandoned before the next, a second create being a second
// write. None is sent once ctx's deadline has passed ([pause]).
func (t Timing) Ask(ctx context.Context, one func(context.Context) error, again func(asks int)) error {
	if err := t.unset(); err != nil {
		return err
	}
	for asks := 1; ; asks++ {
		attempt, cancel := context.WithTimeout(ctx, t.AskTerm)
		err := one(attempt)
		cancel()
		if !Unanswered(ctx, err) {
			return err
		}
		if again != nil {
			again(asks)
		}
		if !pause(ctx, t.ReAsk) {
			return err
		}
	}
}

// Ask is [Timing.Ask] at the production cadence, each request asked for term.
func Ask(ctx context.Context, term time.Duration,
	one func(context.Context) error, again func(asks int)) error {

	return withTerm(term).Ask(ctx, one, again)
}

// AskTerm is how long ONE metadata WRITE waits before it is presumed destroyed
// and re-issued by [Ask]. CLUSTERED, FIFTEEN SECONDS: a write's reply is HELD
// until the object it made has a LEADER (server/jetstream_cluster.go,
// processStreamLeaderChange), so the term spans the metadata commit and a full
// election (maxElectionTimeout 9s, server/raft.go). Shorter abandons an
// accepted create just before its leader answers; longer waits for a request
// already gone. An eighth of the clustered [Budget]. A READ is not held and
// waits [ReadTerm] ([ReadWith]); sized by this term, one dropped lookup cost
// sixteen idle seconds. SOLO, THE SOLO BUDGET: no metadata group or election,
// so an unanswered request is the file store still working and a second ask
// adds load, not a leader.
func AskTerm(clustered bool) time.Duration {
	if clustered {
		return clusterAskTerm
	}
	return soloBudget
}

// AskTerm is how long one metadata request gets before it is re-issued.
func (c Clustered) AskTerm() time.Duration { return AskTerm(bool(c)) }

const clusterAskTerm = 15 * time.Second

// ReAsk is how long [Ask] waits before re-issuing a request nobody answered.
//
// ONE SECOND, the vendored server's hbInterval (server/raft.go): the shortest
// interval over which the metadata group's leadership can have changed;
// sooner asks the same group in the same state and spends requests on a queue
// that may be draining. Deliberately NOT [PlacementRetry]: that re-asks a
// refusal the server ANSWERED, which clears when a peer joins and is worth
// polling four times a second; this re-sends a DESTROYED request, where what
// must change is which member holds the group. Two conditions, two cadences.
const ReAsk = time.Second

// pause waits d before a request is asked again, and reports whether it may
// be: not once ctx has ended, and not once ctx's deadline has passed, whether
// or not ctx says so yet ([overdue]). Then it waits for ctx's ending, which is
// due, so its caller returns what it returns when ctx ends, with ctx's own
// error set beside it for [Unanswered] to read, as it would be on a runtime
// that ran every timer on time.
func pause(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
	}
	if overdue(ctx) {
		<-ctx.Done()
		return false
	}
	return true
}

// overdue reports whether ctx's deadline has passed, WHETHER OR NOT CTX SAYS
// SO YET. Every loop here that asks again decides it with this, never with
// ctx.Done alone. A context ends at its deadline via a timer whose function
// ([time.AfterFunc]) the runtime runs in its own goroutine once it reaches
// that timer; until then ctx.Err() is nil. On a process the machine has not
// run for a while a
// loop's own timer and ctx's are both due, with select choosing at random, so
// it went on as though its caller still waited. nats.go refuses a request
// only on a context whose Err is set, so one went out anyway: a read nobody
// would hear (the load [hedge] counts), or a write the broker may carry out
// after its caller gave up. A deadline AT this instant has passed: a request
// then has no time left to be answered in.
func overdue(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

// Clustered is whether this node's broker has peers, which is what decides
// every budget above.
//
// A TOPOLOGY FACT, not a replica count: a member that names peers, or an
// external NATS cluster, with `stream.replicas: 1` is a real deployment (only
// `replicas > 1` with no peers is refused, config.Bootstrap.Validate), and
// every create on it waits on the same metadata group. A named type so a
// caller cannot pass one of the other booleans in scope by accident.
type Clustered bool

// Budget is how long ONE replicated create gets.
func (c Clustered) Budget() time.Duration { return Budget(bool(c)) }

// SequenceBudget is how long a whole bring-up of many creates gets.
func (c Clustered) SequenceBudget() time.Duration { return SequenceBudget(bool(c)) }

// SlowAfter is how long one create may take before it is worth a line in the
// log.
//
// TEN SECONDS, which is the only interesting band: below it every healthy
// create on every topology finishes (a solo one in milliseconds, a clustered
// one in a round trip or two), and above it nothing else on this path says
// anything at all until the budget expires. A threshold much lower would put a
// line under ordinary load; much higher and the window it exists to describe
// is mostly over.
const SlowAfter = 10 * time.Second

// WhenSlow calls report once if the work outlives [SlowAfter], and returns the
// function that ends the watch.
//
// A stalled provisioning call is COMPLETELY SILENT: a node opens many buckets
// and streams in a row, and a hang logs nothing until the failure a budget
// later, so the log cannot say which object it was on or whether it was slow
// or wedged. One line PER OBJECT answers both (more lines follow or not); a
// repeating tick would repeat the question. The returned stop WAITS for the
// watcher, so report never fires after it returns: a test's sink must not get
// a line after its case finished.
func WhenSlow(ctx context.Context, report func(after time.Duration)) (stop func()) {
	return whenSlowAfter(ctx, SlowAfter, report)
}

// whenSlowAfter is the mechanism, with the threshold as an argument.
//
// SEPARATED so the firing half can be exercised in milliseconds. The
// alternative is a unit test that really waits [SlowAfter], and a ten-second
// case is one somebody eventually deletes — which would leave the half that
// only runs when something is wrong as the half nothing covers.
func whenSlowAfter(ctx context.Context, after time.Duration,
	report func(after time.Duration)) (stop func()) {

	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		timer := time.NewTimer(after)
		defer timer.Stop()
		select {
		case <-done:
		case <-ctx.Done():
		case <-timer.C:
			report(after)
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// ReadBack bounds the one read that asks whether a peer won a create race, and
// the one that asks whether a create this node made is visible yet.
//
// SHORT, and deliberately not [Budget]: this is an ordinary metadata read
// against a group that has just proven it is working — it either answers in a
// round trip or the cluster has gone away, and inheriting the provisioning
// budget would multiply a failing boot's time to say so.
const ReadBack = 5 * time.Second

// PlacementRetry is how often a forming cluster is re-asked.
//
// Short enough that a cluster which forms quickly is not held back by the poll
// itself, and it runs at most a few hundred times inside a provisioning
// budget.
const PlacementRetry = 250 * time.Millisecond

// errCodeNoPeers is JetStream's "no suitable peers for placement".
//
// A NUMBER rather than a description match, because nats.go names only a
// handful of its codes and this is not one of them — and matching the text
// would break the moment the server reworded it.
//
// It is ONE code over several unrelated facts, which is why the description is
// read below after all: the server flattens every reason its peer selection
// accumulated into this code's `{err}` slot (server/jetstream_errors_generated.go,
// JSClusterNoPeersErrF).
const errCodeNoPeers jetstream.ErrorCode = 10005

// Unplaceable reports the transient "the cluster is still forming" error, and
// it is the ONLY one worth waiting out.
//
// Every other create failure clears by nobody waiting; retrying would turn a
// configuration mistake into a two-minute hang. A STORAGE CLAUSE INSIDE THIS
// CODE IS STILL WAITED OUT, keeping it disjoint from [OutOfCapacity]: the room
// weighed is other members' disks, which this node cannot read. The reasons
// cover the group's CURRENT members only (selectPeerGroup never sees one not
// yet joined), so mid bring-up an all-storage refusal can be the verdict of
// the one member up so far. A cluster really out of room says so when the
// budget runs out rather than at once.
func Unplaceable(err error) bool {
	apiErr, ok := apiError(err)
	return ok && apiErr.ErrorCode == errCodeNoPeers
}

// ConsumerClosing reports a consumer create answered "consumer does not exist":
// the broker still holds a consumer of that name that is being deleted, and a
// create meeting it updates it, which a closing consumer refuses
// (server/consumer.go, updateConfig). The only other answer of that code is to
// an update, which no create site sends. WAITED OUT like [Unplaceable]: the
// delete ends in milliseconds and the next create makes the consumer, where a
// read-back ([SettleWith]) asks after one nobody is making until [ReadBack]
// runs out.
func ConsumerClosing(err error) bool {
	apiErr, ok := apiError(err)
	return ok && apiErr.ErrorCode == jetstream.JSErrCodeConsumerDoesNotExist
}

// The broker's codes for a create it refused because the reservation would not
// fit inside the storage limit in force.
//
// BOTH, because which one a broker answers with is decided by where its
// streams live rather than by what went wrong: a file-backed server reports
// the first and a memory-backed one the second, for the identical event.
// Naming only one leaves the other as the silent half nobody notices is
// missing.
//
// A NUMBER rather than a description match, for [errCodeNoPeers]'s reason:
// nats.go names neither, and the text is the server's to reword.
const (
	errCodeOutOfStore  jetstream.ErrorCode = 10047
	errCodeOutOfMemory jetstream.ErrorCode = 10028
)

// errCodeNoLimits is JetStream's "no JetStream default or applicable tiered
// limit present", a NUMBER for [errCodeNoPeers]'s reason.
//
// It is the one refusal in this family that is about the LIMIT TABLE rather
// than about a quantity: the server resolves an object's limits through
// jsAccount.selectLimits, which answers not-ok when the account carries
// neither a default limit nor one for this object's replica class, and every
// create path returns this code at that point — before it compares a byte
// (server/stream.go and server/jetstream_cluster.go for a stream,
// server/consumer.go for a consumer, in the pinned nats-server).
const errCodeNoLimits jetstream.ErrorCode = 10120

// NoApplicableLimit reports a create the broker refused because NO LIMIT IN
// THE ACCOUNT APPLIES TO IT AT ALL. Beside [OutOfCapacity]: terminal alike,
// remedied differently. There the ceiling does not fit a limit that exists;
// here the account is tiered and carries none for this node's replica class,
// so nothing fits by being smaller: what moves is `stream.replicas` or the
// tier declarations. As an arm of OutOfCapacity it would inherit a sentence
// naming limits that do not exist here. TERMINAL, never waited out: no member
// arriving changes a limit table, unlike [Unplaceable].
func NoApplicableLimit(err error) bool {
	apiErr, ok := apiError(err)
	return ok && apiErr.ErrorCode == errCodeNoLimits
}

// NoApplicableLimitDetail is the clause a caller attaches to that refusal, in
// the shape of [Unplaceable]'s and [OutOfCapacity]'s: a leading-space sentence
// appended to the broker's own words. ONE WORDING FOR ALL FIVE CALLERS: the
// remedy is the same at each, and per-caller wording drifted. IT NAMES THE
// CLASS, NOT THE OBJECT: limits are per replica class and this node's number
// is `stream.replicas`, normalised as the server does (tierName reads 0 as 1).
// The account IS tiered (selectLimits answers not-ok only without a default
// limit, and EnableJetStream installs tiers). A tier present with no limit
// reaches the same refusal, hence "carries none for". It says an embedded
// broker never answers this, so no one looks at Tier A.
func NoApplicableLimitDetail(replicas int) string {
	if replicas < 1 {
		replicas = 1
	}
	return fmt.Sprintf(" — this account's storage limits are TIERED and it "+
		"carries none for R%d, which is the replica class `stream.replicas` "+
		"puts this node's streams, consumers and buckets in. The broker "+
		"refuses every create in this state before it compares a byte, so "+
		"nothing here fits by being made smaller: set `stream.replicas` to a "+
		"class the account carries a limit for, or have whoever runs that "+
		"cluster declare one for R%d. An embedded broker never answers this — "+
		"the account is an external cluster's, and its limits are its "+
		"operator's", replicas, replicas)
}

// OutOfCapacity reports a create the broker refused because the byte ceiling
// asked for does not fit inside the storage limit in force.
//
// Named here because [internal/queue/jetstream] and [internal/coord/kv] (a
// bucket BEING a stream) both meet it. PLACEMENT is deliberately not one of
// these codes: [errCodeNoPeers]'s storage clause covers the group's CURRENT
// members and disks this node cannot read, so read as terminal it would
// refuse a boot where nothing is wrong. It stays [Unplaceable].
// `insufficient resources` (10023) is excluded the other way: the server
// answers a publish, catch-up or consumer placement with it, never a stream
// create. Like Unplaceable it is TERMINAL, and a caller must not read back
// afterwards: nothing was placed.
func OutOfCapacity(err error) bool {
	apiErr, ok := apiError(err)
	if !ok {
		return false
	}
	switch apiErr.ErrorCode {
	case errCodeOutOfStore, errCodeOutOfMemory:
		return true
	}
	return false
}

// The broker's codes for a create refused ON THE REQUEST ALONE (before it
// looked for a same-name assignment, or with that name not counted), so no
// peer creating the same object can have produced one. NUMBERS for
// [errCodeNoPeers]'s reason; nats.go names only the last. In the pinned
// nats-server (jsClusteredStreamRequest, jsClusteredConsumerRequest):
//   - INVALID CONFIGURATION (checkStreamCfg, the sealed-create refusal) depends
//     on the configuration sent, run before the name-assigned check.
//   - SUBJECTS OVERLAPPING is decided after a same-name stream is answered
//     "name already in use" or excluded from the comparison.
//   - THE MAXIMUM NUMBER OF STREAMS, and of CONSUMERS, are counted without the
//     name being created (tieredStreamAndReservationCount skips it).
const (
	errCodeInvalidConfig  jetstream.ErrorCode = 10052
	errCodeSubjectOverlap jetstream.ErrorCode = 10065
	errCodeMaxStreams     jetstream.ErrorCode = 10027
	errCodeMaxConsumers                       = jetstream.JSErrCodeMaximumConsumersLimit
)

// Refused reports a create the broker ANSWERED BY PLACING NOTHING, for a
// reason no peer racing the same create can have produced, so a read-back
// would ask after an object nobody made. One predicate for every create site:
// an unlisted refusal fell through to a [ReadBack] poll and appended `(and it
// is not there: stream not found)`. A LIST, not "everything but a race's
// shapes": a code wrongly listed turns a peer that won the race into a node
// refusing to boot; one wrongly left out costs a read-back and a misleading
// suffix. So only codes proven not a race's, and still read back: a name in
// use, an unanswered create, an unavailable group, the generic consumer-create
// failure (the consumer may be placed). Includes [Unplaceable] and
// [ConsumerClosing] once [Place] has waited them out, [OutOfCapacity] and
// [NoApplicableLimit]; a caller wording one differently asks for it first.
func Refused(err error) bool {
	if Unplaceable(err) || ConsumerClosing(err) || OutOfCapacity(err) || NoApplicableLimit(err) {
		return true
	}
	apiErr, ok := apiError(err)
	if !ok {
		return false
	}
	switch apiErr.ErrorCode {
	case errCodeInvalidConfig, errCodeSubjectOverlap, errCodeMaxStreams, errCodeMaxConsumers:
		return true
	}
	return false
}

// InvalidConfig reports a create the broker refused as an INVALID
// CONFIGURATION: checkStreamCfg's answer (see the constants above), one of
// [Refused]'s, asked on its own by a caller that can say what in the
// configuration it sent a server may refuse. ONE CODE FOR EVERY SUCH
// REFUSAL — a window above the server's limit, an age below the window, a bad
// subject — so it says the configuration was refused and never which part of
// it; the broker's words say which, and are no contract to match.
func InvalidConfig(err error) bool {
	apiErr, ok := apiError(err)
	return ok && apiErr.ErrorCode == errCodeInvalidConfig
}

// apiError unwraps the broker's own answer out of whatever a caller wrapped it
// in, which is how every predicate here is asked the question.
func apiError(err error) (*jetstream.APIError, bool) {
	// TWO STATEMENTS, because `return apiErr, errors.As(err, &apiErr)` reads
	// a variable the call it sits beside WRITES, and Go orders the function
	// call against the other operand's evaluation for nobody.
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) {
		return nil, false
	}
	return apiErr, true
}

// NotYetVisible reports a create that landed at the metadata layer but is not
// yet readable by the member that made it.
//
// Not [Unplaceable]'s question: there the leader REFUSED to place, and more
// members fix it. Here the object was placed and the create returned no
// error, but the next lookup on the same member says not found because the
// metadata update has not reached it. Waiting answers both, but the errors do
// not overlap. Bounded by [ReadBack], not [Budget]: after a successful create
// it is a propagation delay, and a terminal not-found failed a boot right
// after that same stream's create succeeded.
func NotYetVisible(err error) bool {
	return errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrBucketNotFound) ||
		errors.Is(err, jetstream.ErrConsumerNotFound)
}

// Unanswered reports a request the broker never replied to: the THIRD value
// of an existence probe, beside "there" and "not" ([internal/coord]'s three
// facts). A [jetstream.ErrStreamNotFound] TOLD the object is absent; an
// expired deadline told NOTHING, and read as one a slow group failed boots on
// a healthy cluster. Falling through to the create is the answer: it makes an
// absent object, and "already in use" means a peer won the race, read back
// through [Settle]. Counts: [context.DeadlineExceeded], [nats.ErrTimeout] and
// [nats.ErrNoResponders] (JetStream not serving yet); NOT [context.Canceled],
// the caller giving up. It takes the PARENT because DeadlineExceeded comes
// from a term THIS package set (the request is gone, the next is answered) or
// the CALLER's deadline (nobody waits; a request would be doomed and hide the
// ceiling). nats.go's two cannot come from a caller's deadline.
func Unanswered(parent context.Context, err error) bool {
	if errors.Is(err, nats.ErrTimeout) || errors.Is(err, nats.ErrNoResponders) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil
}
