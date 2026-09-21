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
// One number for both is wrong for one of them, which is the conclusion
// [internal/queue/jetstream]'s accept budget already reached: it was raised
// from the solo thirty seconds to two minutes for exactly this symptom, while
// the provisioning calls DOWNSTREAM of it kept the flat thirty. A create
// cannot defensibly be more impatient than the readiness wait that precedes
// it.
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
// — fourteen coordination buckets from [internal/coord/kv]'s OpenFleet, three
// more from its Open, and the engine's own streams beside them.
// Nothing bounded the sequence, so the real worst case was already the product
// rather than the term, and raising the term alone would have multiplied it.
// [SequenceBudget] is the wall-clock ceiling over a whole bring-up; a caller
// applies it once to the context it passes down, and because
// [context.WithTimeout] only ever shortens, each create inside then takes the
// lesser of its own budget and what is left of the sequence's.
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
	// TWO MINUTES, which is not a new number: it is what
	// [internal/queue/jetstream]'s clusterAcceptTimeout already spends on
	// the strictly EASIER problem of accepting a connection, and this call
	// sits downstream of both that and the sixty-second readiness wait.
	// The asymmetry is the accept budget's: failing here fails the whole
	// boot, so a budget that is too short turns a busy host into a node
	// that refuses to start and then works on the retry, while one that is
	// too long only reports a genuinely wedged cluster later — and the
	// wait is cancellable, so an operator who has seen enough gets their
	// prompt back immediately.
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
	// TWO MINUTES rather than something tighter, although every create in
	// it is local file-store setup measured in milliseconds and a solo
	// node has no readiness wait to sit behind at all. The asymmetry is
	// the one the whole package turns on: there was no aggregate ceiling
	// before this, so the only way a number here can do harm is by being
	// too SMALL — it would fail a boot that used to work, which is the
	// exact bug this change exists to fix, reintroduced on the path that
	// never had it. Two minutes is still a real bound (a quarter of what
	// the per-create budgets alone would allow) and leaves roughly a
	// thousandfold headroom over what the creates actually cost.
	soloSequenceBudget = 2 * time.Minute

	// clusterSequenceBudget bounds a whole clustered bring-up.
	//
	// FIVE MINUTES, sized so that one genuinely slow create can spend its
	// whole two-minute budget and the rest of the sequence still has room
	// — by then the metadata group has proven it works, so the others are
	// fast. Anchored above the three minutes of waiting a clustered boot
	// already tolerates before it provisions anything at all
	// (clusterAcceptTimeout plus clusterReadyTimeout), and below the point
	// where a wedged cluster stops being reported inside the patience of
	// whatever is watching the node come up.
	clusterSequenceBudget = 5 * time.Minute
)

// SequenceBudget is how long a whole bring-up of many creates gets.
func SequenceBudget(clustered bool) time.Duration {
	if clustered {
		return clusterSequenceBudget
	}
	return soloSequenceBudget
}

// LookupBudget is the CEILING over one existence probe — the "does this object
// already exist" read that decides create-versus-observe, including however
// many times [Ask] has to re-issue it.
//
// # Why a read is not sized like a create
//
// [Budget] sizes a raft round trip against a metadata group that may still be
// forming, and the provisioning paths handed that same number to the LOOKUP
// that precedes the create, because both went through one context. They are
// not the same operation. A create has to be agreed by a quorum; a lookup is a
// metadata READ, and the only reason it is ever slow is that the group cannot
// answer it yet.
//
// # THIRTY SECONDS, WHICH IS TWO ATTEMPTS AND THE GAP BETWEEN THEM
//
// A ceiling rather than one request's deadline, because an unanswered lookup
// is re-issued like any other metadata request: the reply is usually destroyed
// rather than late, and [Ask] carries the server's own three drop paths.
//
// THE SECOND ATTEMPT IS DELIBERATELY TRUNCATED, and the arithmetic says so
// rather than rounding it away. A full first attempt at [AskTerm]'s clustered
// fifteen seconds plus the [ReAsk] second between them leaves FOURTEEN of the
// thirty for the second, not another fifteen. Fourteen is still past both
// numbers the term is anchored to — maxElectionTimeout is 9s and
// lostQuorumInterval is 10s — so the second attempt still spans a complete
// election and still outlasts the point at which a leaderless group starts
// answering. It buys everything a full term would; it is one second short of
// symmetric, which is a property of the ceiling and not a defect in it.
//
// Thirty-one would make the two attempts equal and buy nothing: what the
// second attempt needs is to reach a group that has settled, and it does that
// at fourteen. A round ceiling that states its own truncation is better than
// an odd one chosen to hide it.
//
// A third attempt would add no evidence. What changes between attempts is
// which member holds the metadata group, and that is settled within one
// election; past it, a group still not answering is one the create's own
// budget goes on asking — with far more of that budget left than a longer
// lookup would have spent.
//
// The ceiling does not branch on topology, which is why this is one constant
// where [Budget] and [SequenceBudget] are two. A solo member's lookup is a
// local file-store read answered in microseconds, with no raft, no election
// and nothing to re-ask — [AskTerm] states that as one attempt — so for it
// thirty seconds is pure hang detection.
//
// # What it buys
//
// A lookup that used to spend the whole two-minute clustered [Budget] on ONE
// unanswered request and then fail the boot now spends thirty seconds asking
// twice, and if it is still told nothing it falls through to the create, which
// covers every outcome the lookup could have reported (see [Unanswered]).
//
// Both of the numbers this replaced were measured failing. The client's
// undeclared five-second default on this call failed a three-member cluster
// under load about one boot in six, reported as a bare "context deadline
// exceeded" with nothing to say which deadline; the two-minute budget that
// replaced it then failed an end-to-end cluster case on three attempts
// running, at ~120s each, on a different object every time.
const LookupBudget = 30 * time.Second

// Settle re-runs ask while it answers [NotYetVisible], for up to [ReadBack].
//
// # Why every read-back on this path needs it
//
// A clustered create is committed by the metadata leader and becomes visible
// to each member on its own next metadata update, so there is a window in
// which the member that just made an object is told it does not exist. Every
// read-back on the provisioning path runs inside that window BY CONSTRUCTION —
// each one is asking "did my create, or a peer's, actually land?" — and a
// single lookup answers the question at one arbitrary instant inside it.
//
// It was written out once, for the state log's own stream lookup, and the
// three sibling read-backs kept the one-shot form: the bucket create's, the
// lease bucket's status read, and the stream create's peer-race read. All four
// fail a clustered boot the same way and for the same reason, so the re-asking
// is here rather than in any of them — which is the argument this whole
// package exists for.
//
// # ctx IS THE BOOT'S, NEVER THE CREATE'S
//
// Two of these read-backs run because a create's own deadline EXPIRED — that
// is what the timeout half of the peer-race looks like — so handing this
// function that deadline is handing it a context that is already done. It
// would ask once, find the select below already closed, and return the
// not-found it exists to wait out; the retry would be dead code on exactly the
// paths it was written for, and only the two read-backs whose caller had a
// live context would ever re-ask at all. So a caller derives its per-create
// deadline into a SEPARATE variable and passes the one that bounds the boot.
//
// # And the window is this function's, not the caller's
//
// It owns the [ReadBack] deadline, which bounds the WHOLE read-back and not
// one attempt of it: what is being waited out is a single propagation delay
// and not N independent requests, so a window per attempt would multiply the
// time a genuinely absent object takes to be reported by however many times it
// was re-asked. A window the caller built separately would still be running
// when a lookup blocked past it, and a lookup with no deadline of its own
// would outlive the window entirely and hang until the boot's context expired.
//
// # An attempt nobody answered is not an answer, and gets its own cadence
//
// Inside that window each attempt gets its own short term ([SettleAsk]), for
// [Ask]'s reason arriving on the read-back path: a request put to a group that
// has no leader yet is DROPPED rather than refused, so handing one attempt the
// whole window spends all of it waiting for a reply nobody is going to send —
// and returning what that produced reports "the object is not there" on the
// strength of having heard nothing, which is the collapse [Unanswered] exists
// to prevent. Measured on three members creating one stream at three ceilings:
// the loser of each race read back once, was never answered, and failed its
// boot over a stream that existed a moment later.
//
// So this loop now serves TWO conditions, and they keep the two cadences
// [ReAsk] states rather than sharing one. A not-yet-visible is an ANSWER, and
// it clears on this member's next metadata update: [PlacementRetry], four
// times a second. A request nobody replied to was DESTROYED, and what has to
// change is which member holds the group: [ReAsk]. Merging them is not
// academic — `nats.ErrNoResponders` comes back in microseconds, so at the
// placement cadence a broker whose JetStream is not serving yet is asked four
// times a second, for every object a boot provisions, which is the load ReAsk
// exists to remove.
//
// The ERROR IT RETURNS IS THE ASK'S OWN, never a deadline of this function's:
// "stream not found" names the object and "deadline exceeded" does not. That
// holds for the last attempt too — the one the window interrupts — so the
// answer a caller wraps is what the object said, not what this function's
// patience did.
func Settle(ctx context.Context, ask func(context.Context) error) error {
	window, cancel := context.WithTimeout(ctx, ReadBack)
	defer cancel()

	var absent, unanswered error
	for {
		attempt, endAttempt := context.WithTimeout(window, SettleAsk)
		err := ask(attempt)
		endAttempt()
		// THE CADENCE FOLLOWS THE CONDITION, never the loop: see the
		// section above, and [ReAsk] for why the two must not merge.
		pause := PlacementRetry
		switch {
		case err == nil:
			return nil
		case NotYetVisible(err):
			absent = err
		case Unanswered(window, err):
			// NOBODY REPLIED, which is the third answer and not the
			// second: re-asked at a destroyed request's own
			// interval, and kept in case the window closes with the
			// object never having said anything at all.
			unanswered, pause = namedSilence(unanswered, err), ReAsk
		case heard(absent, unanswered) != nil && errors.Is(err, context.DeadlineExceeded):
			// THE WINDOW CLOSED MID-ASK, so what came back describes
			// this function's patience rather than the object. The
			// last thing that was heard is the honest answer, and it
			// is the one that names something.
			return heard(absent, unanswered)
		default:
			return err
		}
		select {
		case <-window.Done():
			return heard(absent, unanswered)
		case <-time.After(pause):
		}
	}
}

// namedSilence keeps whichever of two unanswered errors NAMES something.
//
// An attempt that expires on [SettleAsk] is unanswered, and it is also a bare
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

// SettleAsk is how long ONE of [Settle]'s attempts waits for a reply before
// it is presumed dropped and re-issued inside the same window.
//
// ONE SECOND, the vendored server's hbInterval (server/raft.go) and [ReAsk]'s
// own anchor: the shortest interval over which the group's leadership can
// have changed, so an attempt shorter than it abandons a request just as the
// state that would answer it might change.
//
// WHAT IT BUYS IS THAT ONE HUNG ASK IS NOT THE WHOLE READ-BACK, which is what
// the single lookup this replaced amounted to. A hung ask costs this term and
// then waits [ReAsk], so two of them complete inside [ReadBack] and a third
// begins — stated as the relation rather than as a count, because all three
// constants are tuned against the server and a count restated here is the
// thing that goes stale. The test holds the relation, not the number.
//
// Deliberately NOT [AskTerm], and not because that term is for a different
// KIND of request — five of [Ask]'s call sites are lookups exactly like this
// one. It is that [AskTerm] is sized to span a complete election, at fifteen
// seconds clustered, and [ReadBack] is five: one such attempt would be the
// whole window three times over. The two windows differ because the QUESTIONS
// do. [Ask] is asking a group that may be electing, and waits that out. This
// is asking after an object somebody has just committed, against a group that
// has therefore just proven it works — so a reply either arrives in a round
// trip or is not being routed, and the wait is for the routing rather than
// for a leader.
const SettleAsk = time.Second

// Place runs create until the cluster stops refusing to place the object, for
// as long as ctx allows.
//
// # Why this is not each caller's own loop
//
// Because three of the four had one and the fourth did not, which is this
// package's founding argument arriving a second time. A replicated stream and a
// replicated KV bucket each retried [Unplaceable] at [PlacementRetry] until
// their budget ran out; a durable consumer — placed by the same metadata group,
// on the same stream, at the same moment of the same boot — returned on the
// first refusal. So the clustered budget bought the consumer creates nothing at
// all: the one condition the budget exists to wait out was the one condition
// they did not wait on, and a node could fail its boot on "no suitable peers"
// while the group it was asking was still forming.
//
// awaiting is called ONCE, before the first wait, and is where a caller says
// which object is being waited for. Once rather than per attempt for
// [WhenSlow]'s reason: the useful facts are that this started and whether it
// ended, and a line per poll answers the first question hundreds of times.
//
// THE ERROR IT RETURNS IS THE CREATE'S OWN, never the context's: "no suitable
// peers" says what is wrong and names the condition to go and look at, and
// "deadline exceeded" says neither.
func Place(ctx context.Context, term time.Duration,
	create func(context.Context) error, awaiting func()) error {

	for attempt := 0; ; attempt++ {
		// THROUGH [Ask], because a create is a metadata request like
		// any other and the server drops those. Without it a create
		// whose request was destroyed sat on this loop's single call
		// until the whole clustered budget expired, and the refusal
		// this loop waits for never arrived to be waited for — so the
		// budget bought the create the same nothing the missing
		// placement retry once bought the consumer.
		err := Ask(ctx, term, create, nil)
		if err == nil || !Unplaceable(err) {
			return err
		}
		if attempt == 0 && awaiting != nil {
			awaiting()
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(PlacementRetry):
		}
	}
}

// Ask runs one idempotent metadata request, RE-ISSUING it while the broker
// does not answer, for as long as ctx allows.
//
// # Why re-asking is the remedy and waiting is not
//
// Because the reply does not exist rather than being late. nats-server drops
// a routed metadata request outright in more than one place, and both are
// ordinary during a bring-up. A member that is not the metadata leader and has
// no assignment for the object RETURNS WITHOUT REPLYING unless the group is
// already leaderless (server/jetstream_api.go, the `sa == nil` arm of
// jsStreamInfoRequest — the leaderless branch sends a delayed error, the other
// branch sends nothing at all). And apiDispatch DRAINS its whole routed queue
// when it reaches JSDefaultRequestQueueLimit, discarding every request pending
// on it with an advisory and no replies.
//
// So the caller is waiting on a reply nobody is going to send, and a longer
// deadline buys strictly nothing: it was measured buying exactly nothing, at
// two minutes a time, on a different object every attempt. What gets an answer
// is ASKING AGAIN — at a leader that now exists, or past a queue that has
// drained.
//
// # Why every attempt gets its own context
//
// The premise is that the outstanding request is dead. An attempt handed the
// previous one's spent deadline would be dead on arrival, which is the same
// defect [Settle]'s doc records for read-backs handed the create's expired
// context: the retry would be dead code on precisely the path it was written
// for.
//
// # Any answer ends it, including a bad one
//
// [Unanswered] is the only condition re-asked. An answer — the object, a
// not-found, a placement refusal, an auth failure — is returned at once, so
// this never turns a configuration mistake into a loop, and [Place] can still
// see the refusal it exists to wait out.
//
// again, when non-nil, is called before each re-ask with the number of
// requests already sent, so a caller can say which object is not being
// answered. The error returned is the last attempt's own, never this
// function's patience.
func Ask(ctx context.Context, term time.Duration,
	one func(context.Context) error, again func(asks int)) error {

	for asks := 1; ; asks++ {
		attempt, cancel := context.WithTimeout(ctx, term)
		err := one(attempt)
		cancel()
		if !Unanswered(ctx, err) {
			return err
		}
		if again != nil {
			again(asks)
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(ReAsk):
		}
	}
}

// AskTerm is how long ONE metadata request waits before it is presumed
// destroyed and re-issued by [Ask].
//
// # THE CLUSTERED TERM IS FIFTEEN SECONDS, AND THE SERVER CHOSE IT
//
// An attempt has to be long enough that a group which is GOING to answer has
// answered, and no longer — because past that point the request is gone and
// the time is spent waiting for nothing. Three of the vendored server's own
// constants bound that (server/raft.go, server/jetstream_api.go in the pinned
// nats-server):
//
//   - maxElectionTimeout is 9s, so an attempt that spans it covers a complete
//     election: a request that arrived mid-election is re-asked at a leader
//     that now exists rather than at one that never did.
//   - lostQuorumInterval is 10s, which is when a leaderless group stops being
//     silent and starts ANSWERING JSClusterNotAvailError. An attempt shorter
//     than this abandons the request just before the server would have replied
//     to it, and the reply is the thing that ends the loop.
//   - errRespDelay is 500ms, the delay on that answer once it is decided, so
//     the term has to clear 10s by more than a rounding margin.
//
// Fifteen covers all three with room, and it is an eighth of the clustered
// [Budget] that used to be one attempt — so a create now asks eight times
// inside the budget that once bought a single unanswered request.
//
// # AND THE SOLO TERM IS THE SOLO BUDGET, BECAUSE THERE IS NOTHING TO RE-ASK
//
// A solo broker has no metadata group, no election and no leader that can
// change, so none of the drop paths above exists: a request that has not been
// answered is one the local file store is still working on, and asking a
// second time adds load rather than a leader. Stating it as one attempt at the
// full budget keeps that a decision rather than an accident of sharing the
// clustered number.
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
// ONE SECOND, which is the vendored server's hbInterval (server/raft.go) — the
// shortest interval over which the metadata group's leadership can have
// changed. Re-asking sooner puts the same question to a group in the same
// state and answers it the same way, so it spends requests on a queue that may
// itself be draining.
//
// Deliberately NOT [PlacementRetry], although both are "try again in a
// moment". That one re-asks a refusal the server ANSWERED, which clears the
// instant a peer joins and is worth polling for four times a second; this one
// re-sends a request that was destroyed, where the thing that has to change is
// which member holds the group. Two conditions, two cadences, and merging them
// would tie each to the other's evidence.
const ReAsk = time.Second

// Clustered is whether this node's broker has peers, which is what decides
// every budget above.
//
// A TOPOLOGY FACT, and deliberately not a replica count. Inferring it from
// replicas was wrong on a configuration this repository permits: a member that
// names peers, or an external NATS cluster, with `stream.replicas: 1`. Only
// `replicas > 1` with no peers is refused (see config.Bootstrap.Validate), so
// a single-replica clustered broker is a real deployment — and every create on
// it still waits on the same metadata group, while the replica proxy handed it
// the solo budget and the solo sequence ceiling.
//
// It is a named type rather than a bare bool so a caller cannot pass one of
// the several other booleans in scope at these call sites by accident.
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
// # Why a provisioning call needs this at all
//
// Because a stalled one is COMPLETELY SILENT, and that is what made a failed
// boot undiagnosable. A node opens nineteen buckets across its two coordination
// stores, and several streams, in a row; if one of them hangs, nothing is
// logged between the line before it and the failure a budget later — so the
// log cannot say which object it was on,
// how many it had already done, or whether it was moving slowly or not moving
// at all. In one CI run a member emitted nothing whatsoever for 28 seconds and
// then failed, and the only way to learn which bucket it died on was the error
// text at the end.
//
// One line PER OBJECT is deliberately all this gives. It is enough to answer
// both questions a person actually has: which object, and — by whether more
// lines follow — wedged or merely slow. A repeating tick would answer the same
// question eleven times per hung create.
//
// The returned stop WAITS for the watcher, so report never fires after it has
// returned: a caller that logs into a test's sink must not have a line arrive
// after the case it belonged to finished.
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
// Every other create failure — a bad TTL, a conflicting replica count, an auth
// failure — clears by nobody waiting, so retrying it would turn a
// configuration mistake into a two-minute hang ending in the same message.
//
// A STORAGE CLAUSE INSIDE THIS CODE IS STILL WAITED OUT, which is what keeps
// it disjoint from [OutOfCapacity] rather than overlapping it. A clustered
// create that no member can place reports this one code with every reason its
// peer selection accumulated flattened into the description, and the room it
// weighed is somebody else's disk — a figure this node cannot read and so
// cannot name in a terminal message.
//
// AND THE REASONS ARE THE GROUP'S CURRENT MEMBERS' ONLY. selectPeerGroup walks
// the metadata group's peers, sets one shared flag for each member it discards
// for want of room, and never sees a member that has not joined at all — so
// during a bring-up, which is the one moment this whole package exists to be
// patient through, a refusal whose every reason is storage can be the verdict
// of the single member that has come up so far. Waiting is right for that, and
// for a peer an operator is about to give room to; the cost is that a cluster
// which really has no room says so when the budget runs out rather than at
// once.
func Unplaceable(err error) bool {
	apiErr, ok := apiError(err)
	return ok && apiErr.ErrorCode == errCodeNoPeers
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
// THE ACCOUNT APPLIES TO IT AT ALL.
//
// # Why it needs a predicate of its own, beside [OutOfCapacity]
//
// Because it is terminal in the same way and remedied in a different one, and
// a caller that cannot tell them apart sends an operator to the wrong lever.
// OutOfCapacity means the ceiling asked for does not fit inside a limit that
// exists: what moves is the limit, or what is already reserved against it.
// This means the account states no limit that COULD be fitted into — its
// limits are tiered and it carries none for the replica class this node's
// objects land in — so nothing here fits by being made smaller. What moves is
// `stream.replicas`, or the account's own tier declarations.
//
// Folded in as a third arm of OutOfCapacity it would have inherited that
// refusal's sentence, which names a limit, a usage and
// `stream.store_max_bytes`: three numbers and a field that do not exist on
// this account, offered as the thing to change.
//
// # And unclassified it was reported as an object that is not there
//
// Which is what it was. Neither OutOfCapacity nor [Unplaceable] matched, so
// every create path fell through to its read-back, spent [ReadBack] asking
// after an object the broker had refused to make, and appended `(and it is not
// there: stream not found)` to the one sentence that said what was actually
// wrong. An operator reading that goes looking for a missing stream on a
// cluster whose account never carried a limit for it.
//
// TERMINAL, and never waited out: no member arriving changes a limit table, so
// unlike [Unplaceable] there is nothing here for the placement retry to wait
// for.
func NoApplicableLimit(err error) bool {
	apiErr, ok := apiError(err)
	return ok && apiErr.ErrorCode == errCodeNoLimits
}

// NoApplicableLimitDetail is the clause a caller attaches to that refusal, in
// the shape [Unplaceable]'s and [OutOfCapacity]'s callers already use: a
// leading-space sentence appended to the broker's own words.
//
// ONE WORDING FOR FOUR CALLERS — the stream create, the bucket create and the
// two consumer creates — because the remedy is the same at each and the server
// makes no distinction between them either. Written per caller it would drift
// the way every other pair in this tree has.
//
// IT NAMES THE CLASS RATHER THAN THE OBJECT, because the class is the whole
// fact: the account's limits are per replica class and this node's number is
// `stream.replicas`. replicas is normalised the way the server normalises it
// (server/jetstream.go, tierName reads 0 as 1), so the class named here is the
// class the refusal was decided against.
//
// It states that the account IS tiered rather than guessing: selectLimits can
// only answer not-ok when there is no default limit, and an account with no
// limits at all is not a shape the server permits — EnableJetStream installs
// defaultJSAccountTiers for one. It also says an embedded broker never answers
// this, because that is the first thing a reader will wonder and the answer
// saves them looking at Tier A for a field that is not the lever.
//
// A TIER THAT IS THERE AND CARRIES NO LIMIT reaches this same refusal, which
// is why the sentence says "carries none for" rather than "declares no tier
// for": the account's report lists every class it holds objects in, and
// [internal/queue/jetstream]'s budget tells the two apart for the operator who
// reads it there.
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
// # Why this is named here rather than beside either caller
//
// Two subsystems provision replicated objects and both meet this refusal —
// [internal/queue/jetstream] creating streams and [internal/coord/kv] creating
// buckets, a bucket BEING a stream — which is the same reason [Unplaceable],
// [Budget] and [ReadBack] live here. Written twice the two spellings drift,
// exactly as this package's own doc records for every other rule it holds.
//
// # Why PLACEMENT is deliberately not one of these codes
//
// A clustered create that no member can place reports [errCodeNoPeers], whose
// code is shared by every placement failure and whose storage clause is prose
// accumulated across the peers the metadata group HAS. What it weighed is
// another member's disk, which this node cannot read — so a message calling it
// terminal could only quote a budget that is not the one that refused. And the
// accumulation is over the group's CURRENT membership rather than over the
// fleet, so during a bring-up that verdict can be the single member that has
// come up so far speaking for peers still starting: read as terminal it would
// refuse a boot on a cluster where nothing is wrong. It stays [Unplaceable]. `insufficient resources` (10023) is excluded for the
// opposite reason: the server answers a publish, a catch-up or a consumer's
// placement with it, never a stream's create, so naming it here would read
// some other failure as a ceiling nobody reserved.
//
// Like [Unplaceable] it is TERMINAL — nothing frees a limit by being waited
// for — and, like Unplaceable, a caller must not read back afterwards: nothing
// was placed, and a not-found appended to the message only obscures what is
// actually wrong.
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

// apiError unwraps the broker's own answer out of whatever a caller wrapped it
// in, which is how both predicates above are asked the question.
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
// # Why this is not the same question as Unplaceable
//
// "No suitable peers" is the metadata leader REFUSING to place an object,
// answered by waiting for more members. This is the opposite: the object was
// placed, the create returned no error, and the immediately following lookup
// on the same member still says not found because the metadata update has not
// reached it. Waiting is the answer to both, but the errors do not overlap and
// a lookup that waited out a placement failure would be waiting for something
// nobody is going to do.
//
// It is bounded by [ReadBack] rather than [Budget] wherever it is used: a
// create that has already returned successfully makes this a propagation
// delay, not a provisioning one. Treating a not-found as terminal here is what
// turned one CI run's clustered boot into `open the log
// "CREWLET_TRACKER_VECTORS": stream not found` immediately after the create of
// that same stream had succeeded.
func NotYetVisible(err error) bool {
	return errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrBucketNotFound) ||
		errors.Is(err, jetstream.ErrConsumerNotFound)
}

// Unanswered reports a request the broker never replied to — the THIRD value
// an existence probe can return, beside "it is there" and "it is not".
//
// # Why this is a value and not a failure
//
// [internal/coord]'s lesson, in the provisioning path: "held", "definitively
// not held" and "the store could not be reached" are three different facts,
// and collapsing the last two is the most expensive mistake this engine has
// made. An existence probe has exactly the same three answers. A lookup that
// returned [jetstream.ErrStreamNotFound] has been TOLD the object is absent; a
// lookup whose deadline expired has been told NOTHING, and the difference is
// the whole of this function.
//
// Every provisioning path here read the second and third as one. The switch
// was `case err == nil: observe` / `case errors.Is(err, ErrNotFound): create`
// / `default: fail the boot` — so a metadata group that was merely slow to
// answer produced `ensure stream CREWLET_CONFIG: context deadline exceeded`
// and a node that refused to start, on a cluster where nothing was wrong with
// the stream, the config or the peer. That is a node declining to boot because
// it could not hear, reported as though it had heard "no".
//
// # Why falling through to the create is the answer
//
// Because the create already covers both of the things the lookup failed to
// say. If the object is absent the create makes it; if it exists the create
// returns "already in use", which these paths have ALWAYS treated as a peer
// having won the race, read back through [Settle] and compared exactly as a
// lookup's own hit would be. So the lookup is an optimisation — it saves one
// round trip when it answers — and a failed optimisation is not a failed boot.
// Nothing downstream needs a distinction the probe could not supply.
//
// # What counts
//
// [context.DeadlineExceeded] is the budget above expiring while the request
// was in flight; nats.go's [nats.ErrTimeout] is the same event on its own
// non-context path; [nats.ErrNoResponders] is the request reaching a server
// whose JetStream is not serving yet, which on a boot means "ask again in a
// moment" rather than "there is no such object".
//
// [context.Canceled] is deliberately NOT here. A cancelled context is the
// caller giving up — a shutdown, an operator's Ctrl-C — and falling through to
// a create on it would spend a second doomed round trip arguing with a
// decision that has already been made.
//
// # Why it takes the PARENT and not the error alone
//
// Because [context.DeadlineExceeded] arrives from two deadlines that mean
// opposite things, and the value is identical. From a term THIS package set,
// it means the request is gone and the NEXT one will be answered — the
// condition this predicate exists to name. From the CALLER's own deadline —
// the boot's sequence ceiling, an operator's Ctrl-C deadline — it means time
// is up and nobody is waiting for the answer any more. Reading the second as
// the first sends a doomed second request on a context that is already done,
// and reports whatever that produces instead of the ceiling that actually
// expired.
//
// nats.go's own two are unconditional: neither [nats.ErrTimeout] nor
// [nats.ErrNoResponders] can be produced by a caller's deadline, so for them
// the parent has nothing to say.
func Unanswered(parent context.Context, err error) bool {
	if errors.Is(err, nats.ErrTimeout) || errors.Is(err, nats.ErrNoResponders) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) && parent.Err() == nil
}
