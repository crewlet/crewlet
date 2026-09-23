package jetstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"

	"github.com/crewlet/crewlet/internal/statelog"
)

// DomainConsumer is one node's own reader of one domain's log.
//
// # Why it is DURABLE and per NODE, not per fleet
//
// Every node applies every record — that is what makes N identical SQL copies
// — so this is not a work queue and there is no group to share. Each node
// keeps its own consumer, named after itself, and its own position within it.
// A shared consumer would deliver each record to exactly one node, which is
// the opposite of replication.
//
// # And why the CONSUMER's position is not the checkpoint
//
// The applier's checkpoint lives in the same transaction as the rows, in SQL.
// This consumer's acknowledgement floor is a second, weaker number: it moves
// after the commit, it can lag behind a crash, and on a rebuilt consumer it
// starts wherever the delivery policy says. So the loop RESUMES from the SQL
// checkpoint and the acknowledgement exists to let the broker release
// redelivery state — which is why a lost ack costs a redelivery and never a
// hole.
//
// # But the checkpoint cannot ask for what the consumer already gave away
//
// Resuming from the checkpoint means DROPPING what arrives below it; nothing
// on the loop's side can make the broker hand over a record it has already
// delivered and been told was handled. So the two numbers are compared when
// the consumer is opened, and one that disagrees with the rows is rebuilt from
// them — see [Queue.DomainConsumer] for the directions they can disagree in
// and what each one costs a node that is left with it.
type DomainConsumer struct {
	q      *Queue
	stream string
	name   string

	// cons is the broker-side consumer, under a mutex because [Reset]
	// replaces it while the applier that holds this handle is between
	// runs — the handle stays, the consumer under it moves.
	//
	// NIL IS A REAL STATE: a reset deletes before it creates, so a create
	// that fails leaves the broker with no consumer at all, and a handle
	// that went on naming the deleted one would fail every fetch for the
	// life of the process with nothing able to repair it. Cleared instead,
	// and rebuilt from `want` by the next caller — see [DomainConsumer.consumerFor].
	mu   sync.Mutex
	cons jetstream.Consumer

	// want is the configuration this consumer should have, kept so that a
	// handle cleared by a failed reset can be rebuilt without the caller
	// knowing how one is configured.
	want jetstream.ConsumerConfig
}

// domainConsumerMaxAckPending is how many records the broker may hand this
// node's applier before one is acknowledged, and it is THE COUNT BOUND ON A
// PULL.
//
// The vendored client cannot bound one pull by both bytes and count, so the
// applier pulls by bytes and the count has to live somewhere the broker
// enforces it. This is that place: a pull hands over at most this many
// records however many bytes it asked for, and [DomainConsumer.Fetch] returns
// every one of them. The alternative — the applier taking a prefix of a
// byte-bounded batch — left the remainder delivered and unacknowledged, so
// the broker hit its own default cap of a thousand, handed over nothing more
// for a thirty-second ack window, and then redelivered the same prefix.
//
// [statelog.FetchMessages] is the number, and it is the applier's transaction
// budget: one pull is at most one transaction's worth of records in flight.
const domainConsumerMaxAckPending = statelog.FetchMessages

// domainConsumerAckWait is how long the broker waits for an acknowledgement
// before redelivering.
//
// THIRTY SECONDS, against an applier whose own transaction is bounded by a row
// budget and a time budget far below it. What a redelivery costs here is one
// re-apply, which is free by construction — the apply is idempotent at a
// position — so the bound exists to release the broker's redelivery state
// rather than to protect the applier from itself.
const domainConsumerAckWait = 30 * time.Second

// DomainConsumer opens (or creates) this node's durable reader of a domain's
// log, resuming from `after`.
//
// # DeliverPolicy is derived from the SQL checkpoint, and that is the whole
// of the resume rule
//
// A consumer created fresh must not start at the stream's head — it would
// silently skip everything published before it existed, which on a state log
// is every record the node has not applied. It must not blindly start at the
// beginning either, on a node that has applied a million records: that is a
// million redeliveries the applier drops one at a time.
//
// So a node with a checkpoint starts at `after + 1` and a node with none
// starts at the beginning. Both are the same rule — resume from what the ROWS
// say — and the rows are the only durable statement of it.
//
// # An EXISTING consumer is held to the same rule
//
// A consumer this node made on an earlier run is kept only when it would hand
// over exactly what a fresh one at `after + 1` would: nothing past the
// checkpoint delivered, nothing in flight, nothing between the two left to
// deliver. Anything else is DELETED AND CREATED AGAIN at `after + 1` — the
// same operation an adoption performs through [DomainConsumer.Reset], because
// the broker treats a start sequence as immutable and a create-or-update
// would refuse to move it while reporting success.
//
// It used to be taken as it was, on the reasoning that a consumer sitting
// lower than the rows only costs redeliveries the applier drops. That covers
// one of the four ways the two can disagree, and the other three stall the
// node — one of them for ever:
//
//   - AHEAD, ACKNOWLEDGED. The broker has been told records past the
//     checkpoint were handled, so it will never deliver them again — and the
//     loop, which resumes at the checkpoint, waits for them for ever. This is
//     what a node's rows going BACKWARDS under the same node id produces: the
//     replicated database deleted and rebuilt, or restored from a copy older
//     than the broker estate beside it (a backup copies the store FIRST, so a
//     restore of one is exactly this). On a strict log the node stays
//     unhydrated, every write on the missing subjects refuses `behind`, and
//     the heartbeat's below-floor check never fires because the records are
//     still on the log — measured on a node whose replicated database was
//     deleted between two boots on one broker: `applying: 2 record(s) behind
//     the log's head` at every sample to forty-five seconds, past any
//     redelivery window. On a compacted log it is quieter and worse: nothing
//     there checks contiguity, so the node applies what comes next and
//     reports itself caught up with the skipped records' rows absent.
//   - AHEAD, DELIVERED. Records past the checkpoint went to a process that is
//     gone — a stop mid-drain, a pull the server answered after its caller
//     left — and come back only when their ack window expires. On a strict
//     log every later record waits in the reorder buffer behind the first of
//     them, so a restarted node sat unhydrated for the whole
//     [domainConsumerAckWait].
//   - IN FLIGHT, at or below the checkpoint. Harmless records, but each one
//     holds a slot of [domainConsumerMaxAckPending] for a reader that will
//     never acknowledge it, and a ceiling full of them — a process that
//     committed its whole pull and died before the acknowledgements — hands
//     over nothing new for the same thirty seconds.
//   - BEHIND. Correct, and costs the redeliveries between for the applier to
//     drop under the in-flight ceiling. Small after a crash, but a node that
//     ADOPTED a snapshot at boot opens its consumers after the adoption moved
//     its checkpoint to the artefact's position, with everything from the
//     log's first surviving record up to it still to be delivered, and
//     nothing else on the boot path moves them — the runtime adoption calls
//     Reset for exactly this cost, and the boot's was the same cost with no
//     Reset.
//
// A rebuild is one replicated delete and create, which is what every node
// pays for its consumers on its first boot; a consumer left disagreeing costs
// between thirty seconds and for ever. And it touches nothing but this node:
// the consumer is named after the node, so on a fleet no peer reads through
// it, and no retention term reads its acknowledgements — the trim takes the
// SQL positions the heartbeat publishes, never this.
//
// # A rebuild that fails is decided by what a node LEFT with the consumer pays
//
// The delete and the create are metadata writes, and on a fleet whose group
// is not answering either can fail. BEHIND and IN FLIGHT cost time and never
// records — nothing past the checkpoint was given away — so there the open
// asks the broker which consumer it now holds and keeps that one, with a
// warning. That is what every open did before the rebuild existed, and a boot
// failed over an optimisation's metadata write would be worse off than the
// boot the optimisation improves. AHEAD, acknowledged or delivered, can cost a
// strict log for ever (the ack floor is only a lower bound — see [driftOf]),
// so there the open fails. So it does when the broker holds NO consumer after
// the failure — the delete landed and the create did not, which is the state
// a failed create on the absent path above fails the open on too — or cannot
// say which it holds: a handle kept on a guess names a consumer that may not
// exist, or leaves nothing where one does, and either way fails every fetch
// for the life of the process while the boot reports success.
func (q *Queue) DomainConsumer(ctx context.Context, stream, nodeID string,
	after uint64) (*DomainConsumer, error) {

	if nodeID == "" {
		return nil, fmt.Errorf("jetstream: a domain consumer has no node id — " +
			"every node reads the whole log, so the consumer is named after " +
			"the node and a shared name would deliver each record to one of them")
	}
	name := domainConsumerName(stream, nodeID)
	config := domainConsumerConfig(name, after)
	// THE HANDLE EXISTS BEFORE THE BROKER-SIDE CONSUMER DOES, so the
	// rebuild below is [DomainConsumer.Reset] itself rather than a second
	// copy of a delete-then-create whose half-done state that method
	// already knows how to leave repairable.
	handle := &DomainConsumer{q: q, stream: stream, name: name, want: config}

	// ONLY AN ABSENT CONSUMER IS CREATED HERE, and whatever consumer the
	// broker ends up holding is judged against the checkpoint by
	// [DomainConsumer.resumeAt] — see the doc comment for why the obvious
	// create-or-update cannot do either job.
	//
	// THE PROVISIONING BUDGET AND ITS BREADCRUMB, because this is a
	// replicated create on the boot path like every other one — and it does
	// not come through [Queue.ensureDurableConsumer], whose create-or-
	// read-back shape is wrong here (see above: an existing consumer is
	// judged, not merely found). Without them the caller's context
	// reached nats.go with no deadline and the client's five-second default
	// decided a clustered boot, silently.
	//
	// NOT SHADOWING ctx, for [jsprovision.Settle]'s reason: the read-back
	// below must not inherit a deadline this create may have spent.
	createCtx, cancel := context.WithTimeout(ctx, q.provisionBudget())
	defer cancel()
	// IT SPANS THE LOOKUP AND THE CREATE, AND SAYS SO — and it stops where
	// the create ends rather than where this function does.
	//
	// Both halves were wrong in the same way, at opposite ends: armed
	// before the lookup while saying "created", and left armed across the
	// read-back and the alignment after it. Either way the line names a
	// step this member is not on, which is the one thing it exists to get
	// right.
	//
	// ON ctx rather than on either term, because the two halves it spans
	// carry separate deadlines now and a watcher armed on one would stop
	// reporting the moment that half gave up.
	stop := jsprovision.WhenSlow(ctx, func(after time.Duration) {
		q.log.WarnContext(ctx, "jetstream_consumer_slow", "stream", stream,
			"consumer", name, "waited", after,
			"detail", "this state-log consumer is still being provisioned — "+
				"looked up, and created if it was absent; on a fleet that is "+
				"a metadata group that has not settled")
	})

	// THE LOOKUP IS SIZED AS A READ AND RE-ASKED, like its three siblings —
	// see [Queue.askRead]. It shared the create's term, so a probe the
	// metadata group never answered spent the whole clustered budget and
	// left none of it for the create that would have settled the question.
	cons, err := handle.lookup(ctx)
	if jsprovision.Unanswered(ctx, err) {
		// TOLD NOTHING, which is not "it is not there" — see
		// [jsprovision.Unanswered]. The create below answers it either
		// way, so a boot no longer fails on a question nobody got
		// round to. Reported as not-found so the one create path
		// handles both, which is exactly what it already does with a
		// held create's timeout a few lines down.
		q.log.WarnContext(ctx, "jetstream_consumer_lookup_unanswered",
			"stream", stream, "consumer", name, "error", err.Error(),
			"detail", "the broker did not say whether this state-log consumer "+
				"exists, so the create below decides it: absent and it is "+
				"made, present and it is read back and held to this node's "+
				"checkpoint")
		err = jetstream.ErrConsumerNotFound
	}
	switch {
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		// WAITED OUT, for the reason [Queue.ensureDurableConsumer]
		// gives: this consumer is placed by the same metadata group as
		// the stream it reads, so "no suitable peers" is transient here
		// too and the budget above is worth nothing without the retry.
		err = jsprovision.Place(createCtx, q.Clustered().AskTerm(), func(ctx context.Context) error {
			var e error
			cons, e = q.js.CreateConsumer(ctx, stream, config)
			return e
		}, func() {
			q.log.Info("jetstream_consumer_awaiting_peers", "stream", stream,
				"consumer", name,
				"detail", "the cluster has not yet seen enough members to "+
					"place this state-log consumer; retrying until the "+
					"provisioning deadline")
		})
		stop()
		switch {
		case err == nil:
			// A CREATE CAN ANSWER WITH A CONSUMER THAT WAS ALREADY THERE:
			// nats.go hands back the existing one when its configuration
			// matches what was asked for. And a node whose rows are gone
			// asks for exactly what it asked for on its first boot —
			// DeliverAll, from nothing — so when the lookup above went
			// unanswered or fell inside the propagation window, the
			// consumer this create returns may be the very one that is
			// ahead. It is judged like any other; a consumer that really
			// is fresh agrees with the checkpoint by construction, and
			// the judgement reads the state this create already returned.
			err = handle.resumeAt(ctx, cons, after)
		case jsprovision.NoApplicableLimit(err):
			// NO LIMIT APPLIES TO THIS CONSUMER AT ALL, which is not
			// a peer having won the race and never becomes one: the
			// account's limits are tiered and carry none for the class
			// `stream.replicas` puts this node in, so the broker
			// refused before it looked at anything else
			// (server/consumer.go, acc.selectLimits). Read back like
			// any other create error it came back as a consumer that
			// is "not there", which reads as a state-log reader this
			// node lost rather than as an account with no limit for
			// it.
			err = fmt.Errorf("%w%s", err,
				jsprovision.NoApplicableLimitDetail(q.cfg.Replicas))
		case !jsprovision.Unplaceable(err):
			// THE LOOKUP ABOVE WAS INSIDE THE PROPAGATION WINDOW, so
			// the consumer this create met is one THIS NODE made on an
			// earlier boot and has not been told about yet — the name
			// carries the node id, so no peer can have made it.
			//
			// EVERY ERROR BUT AN UNPLACEABLE ONE, not just
			// ErrConsumerExists, because the create announces this in
			// two shapes and the tidy one is the rarer. nats.go returns
			// the existing consumer when the configs MATCH and
			// ErrConsumerExists when they differ — but a create the
			// server HELD while the metadata group settled outlives the
			// deadline and comes back a timeout, with the consumer
			// there all the same. That is the shape a clustered boot
			// actually produces, and it is the one
			// [Queue.ensureDurableConsumer] has always read back.
			// Unplaceable is excluded for its own reason: nothing was
			// placed, so there is nothing to become visible.
			//
			// Read it back and hold it to the checkpoint, which is the
			// same rule the lookup's own found branch below applies: a
			// consumer an earlier boot made is one whose position this
			// node's rows may no longer agree with.
			createErr := err
			err = jsprovision.Settle(ctx, func(ctx context.Context) error {
				var e error
				cons, e = q.js.Consumer(ctx, stream, name)
				return e
			})
			switch {
			case err == nil:
				err = handle.resumeAt(ctx, cons, after)
			default:
				// THE CREATE'S ERROR IS WHAT IS REPORTED, with the
				// read-back's beside it: the first says what went
				// wrong and the second only confirms the consumer
				// really is absent.
				err = fmt.Errorf("%w (and it is not there: %w)", createErr, err)
			}
		}
	case err == nil:
		// STOPPED BEFORE THE JUDGEMENT, because the breadcrumb names the
		// lookup and the create and this is neither: a rebuild carries
		// its own, inside [DomainConsumer.Reset].
		stop()
		err = handle.resumeAt(ctx, cons, after)
	default:
		stop()
	}
	if err != nil {
		return nil, fmt.Errorf("jetstream: open the domain consumer %s on %s: %w",
			name, stream, err)
	}
	return handle, nil
}

// domainConsumerConfig is the one configuration a domain consumer has,
// positioned to resume after `after`.
//
// ONE FUNCTION for the open and the reset, because they are the same object
// made at two moments: two literals are how a consumer rebuilt by an adoption
// comes to carry a ceiling or an ack window the boot never gave it.
func domainConsumerConfig(name string, after uint64) jetstream.ConsumerConfig {
	config := jetstream.ConsumerConfig{
		Durable:       name,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       domainConsumerAckWait,
		MaxAckPending: domainConsumerMaxAckPending,
		// NO MaxDeliver. A record this node cannot apply is not a
		// poison message to be given up on: it is either a transient
		// failure the loop retries or a version this build retains
		// deliberately, and a delivery budget that ran out would leave
		// the node silently past a record it never applied.
		MaxDeliver: -1,
	}
	if after > 0 {
		config.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		config.OptStartSeq = after + 1
	} else {
		config.DeliverPolicy = jetstream.DeliverAllPolicy
	}
	return config
}

// Reset moves this node's reader to resume from `after`, by deleting the
// broker-side consumer and creating it again there.
//
// # Why a delete rather than an update
//
// The broker treats a consumer's start sequence as immutable, so the only way
// to move one is to make a new one. Two callers need one moved. An ADOPTION: a
// node that fell below the trim floor installs a peer's snapshot, and its
// checkpoint jumps to the artefact's position — thousands or millions of
// records past where its consumer stopped. Left where it was, the consumer
// would deliver every record in between for the applier to drop one at a
// time, under an in-flight ceiling, for as long as that took. And an OPEN that
// finds a consumer disagreeing with the rows it is opened for — see
// [Queue.DomainConsumer] — where the direction that matters is the other one:
// a consumer AHEAD of the rows never hands the missing records over at all.
//
// What a reset that fails costs depends on who asked. For an adoption the
// applier resumes from the checkpoint whatever the consumer says, so a reset
// that fails there costs the redeliveries and is reported. An open reads back
// which consumer the broker now holds and keeps it only where the drift costs
// time rather than records — behind, or in flight — and otherwise fails, as
// it does when the broker holds none or cannot say: see [Queue.DomainConsumer].
//
// # What a HALF-done reset leaves, and why the handle is cleared
//
// The delete and the create are two calls and the broker can take the first
// and refuse the second — a connection lost in between, a server that went
// away. That leaves no consumer on the stream at all, and a handle still
// naming the deleted one: every later fetch fails against something that does
// not exist, for the life of the process, and no later reset is ever attempted
// because the caller only logs this error. The handle is therefore CLEARED on
// that path, which makes the next fetch rebuild it — the repair happens where
// the damage is noticed rather than needing a second failure to trigger it.
func (c *DomainConsumer) Reset(ctx context.Context, after uint64) error {
	config := domainConsumerConfig(c.name, after)
	c.mu.Lock()
	defer c.mu.Unlock()
	// ONE BREADCRUMB FOR BOTH HALVES, for the reason the open's spans its
	// lookup and its create: a member stalled on either is waiting on the
	// same metadata group, and the line has to say which object it is on.
	// An open reaches this on the boot path, where a silent stall is the
	// failure [jsprovision.WhenSlow] exists to name.
	stop := jsprovision.WhenSlow(ctx, func(waited time.Duration) {
		c.q.log.WarnContext(ctx, "jetstream_consumer_slow", "stream", c.stream,
			"consumer", c.name, "waited", waited, "resume_at", after+1,
			"detail", "this state-log consumer is still being rebuilt at its "+
				"checkpoint — deleted and created again; on a fleet that is a "+
				"metadata group that has not settled")
	})
	defer stop()
	// THE DELETE IS BUDGETED AND RE-ASKED LIKE THE CREATE AFTER IT. It is a
	// request to the same metadata group, and on the caller's context it
	// had neither: an adoption's context carries no deadline, which hands
	// nats.go its undeclared five-second default, and a request the group
	// dropped failed the reset when that expired rather than being asked
	// again. Asking twice is safe — a delete that landed the first time is
	// not-found the second, and not-found is the answer this wants.
	deleteCtx, cancelDelete := context.WithTimeout(ctx, c.q.provisionBudget())
	defer cancelDelete()
	if err := jsprovision.Ask(deleteCtx, c.q.Clustered().AskTerm(),
		func(ctx context.Context) error {
			err := c.q.js.DeleteConsumer(ctx, c.stream, c.name)
			if errors.Is(err, jetstream.ErrConsumerNotFound) {
				return nil
			}
			return err
		}, nil); err != nil {
		return fmt.Errorf("jetstream: reset the domain consumer %s on %s: %w",
			c.name, c.stream, err)
	}
	// THE DELETE HAS LANDED, so from here the broker has no consumer and
	// the handle must not go on naming one.
	c.cons = nil
	// THROUGH [jsprovision.Place] like every other replicated create,
	// which this one was not. A domain consumer is placed by the same
	// metadata group as the durable consumers beside it, so it meets the
	// same two transient conditions — a placement refusal while the group
	// is short of members, and a request the group never answers — and it
	// met them with no budget at all, which is nats.go's undeclared
	// five-second default on whatever context the applier happened to hold.
	createCtx, cancelCreate := context.WithTimeout(ctx, c.q.provisionBudget())
	defer cancelCreate()
	var cons jetstream.Consumer
	err := jsprovision.Place(createCtx, c.q.Clustered().AskTerm(),
		func(ctx context.Context) error {
			var e error
			cons, e = c.q.js.CreateConsumer(ctx, c.stream, config)
			return e
		}, func() {
			c.q.log.Info("jetstream_domain_consumer_awaiting_peers",
				"stream", c.stream, "consumer", c.name,
				"detail", "the cluster has not yet seen enough members to place "+
					"this consumer; retrying until the provisioning deadline")
		})
	if err != nil {
		// `want` IS LEFT AS IT WAS, deliberately: the rebuild then
		// restores the consumer at its PREVIOUS position rather than at
		// one this broker has just refused. For an adoption that costs
		// redeliveries and not correctness, which is the trade its
		// caller makes when it logs this error and carries on. An open
		// never returns a handle in this state: it installs the
		// consumer a second lookup finds, or fails — see
		// [Queue.DomainConsumer] for which, and why it does not leave the
		// repair to the next fetch.
		return fmt.Errorf("jetstream: recreate the domain consumer %s on %s at "+
			"%d: %w", c.name, c.stream, after, err)
	}
	c.cons, c.want = cons, config
	return nil
}

// consumerFor is the current broker-side consumer, created if a failed reset
// left none.
//
// THE REBUILD IS HERE rather than at the call site because this is where the
// absence is discovered, and because every caller wants the same thing: a
// consumer at the position the last reset asked for. It is idempotent — a
// consumer the broker still has comes back from CreateConsumer unchanged.
func (c *DomainConsumer) consumerFor(ctx context.Context) (jetstream.Consumer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cons != nil {
		return c.cons, nil
	}
	// THE SAME BUDGET AND THE SAME RE-ASK as Reset's own create: this is
	// the identical call, reached when that one failed, so it faces the
	// identical transient conditions.
	createCtx, cancelCreate := context.WithTimeout(ctx, c.q.provisionBudget())
	defer cancelCreate()
	var cons jetstream.Consumer
	err := jsprovision.Place(createCtx, c.q.Clustered().AskTerm(),
		func(ctx context.Context) error {
			var e error
			cons, e = c.q.js.CreateConsumer(ctx, c.stream, c.want)
			return e
		}, nil)
	if err != nil {
		return nil, fmt.Errorf("jetstream: rebuild the domain consumer %s on "+
			"%s after a reset that deleted it and could not create it again: %w",
			c.name, c.stream, err)
	}
	c.cons = cons
	return cons, nil
}

// resumeAt leaves the handle holding a broker-side consumer that resumes at
// `after + 1`: cons itself when it agrees with the checkpoint, a rebuilt one
// when it does not, and — when the rebuild fails for a drift that costs only
// time — whichever consumer the broker still holds. [Queue.DomainConsumer] is
// where the directions, their costs and what a failed rebuild does are set
// out.
//
// # The state judged is the one the broker last reported
//
// The lookup and the create each return the consumer's state with the
// consumer, and that is what is read rather than a second request: nothing
// READS this node's consumer while it is being opened — the process that last
// read it is gone, which is the whole premise of every in-flight delivery
// counting as dead.
//
// That is not the same as nothing having been SENT to it. An acknowledgement
// is a fire-and-forget publish, and the server only queues it for the
// consumer's own goroutine while a lookup is answered by a separate API
// worker, so acknowledgements a reader sent just before it went — a stop, or
// an earlier engine in this same process — can still be queued when the state
// is read, and show as deliveries in flight that are already acknowledged.
// What that costs is a rebuild that was not needed, which is always correct.
// It can never keep a consumer that should have been rebuilt: a consumer is
// kept only when its state shows nothing in flight, and with nothing in flight
// there is nothing a queued acknowledgement could still change.
func (c *DomainConsumer) resumeAt(ctx context.Context, cons jetstream.Consumer,
	after uint64) error {

	info := cons.CachedInfo()
	if info == nil {
		// A READ, sized and re-asked as one — see [Queue.askRead].
		if err := c.q.askRead(ctx, func(ctx context.Context) error {
			var e error
			info, e = cons.Info(ctx)
			return e
		}); err != nil {
			return fmt.Errorf("read the consumer's state: %w", err)
		}
	}
	// THE RAW POSITIONS ARE JUDGED FIRST, and the stream's first sequence is
	// read only when it can change the answer — which is not whenever the
	// consumer disagrees with the checkpoint. The broker's skip over what the
	// stream no longer holds (see [nextDelivery]) can only bring positions
	// together, so it changes what is kept, and which drift is reported at
	// which level, for exactly the three raw drifts
	// [consumerDrift.firstMatters] names. A consumer that agrees, or that
	// holds deliveries in flight at or below the checkpoint, is what it is on
	// any stream — so the in-flight consumer a busy restart leaves costs no
	// request here.
	//
	// UNKNOWN IS THE RAW ANSWER: the skip is then not credited, so a consumer
	// it would have brought into agreement is rebuilt anyway. A rebuild is
	// always correct, so failing this read must not fail the open.
	drift := driftOf(info.Delivered.Stream, info.AckFloor.Stream,
		info.NumAckPending, 0, after)
	var first uint64
	var firstErr error
	if drift.firstMatters() {
		if first, firstErr = c.q.firstSequence(ctx, c.stream); firstErr == nil {
			drift = driftOf(info.Delivered.Stream, info.AckFloor.Stream,
				info.NumAckPending, first, after)
		}
	}
	if drift == driftNone {
		// THE IN-FLIGHT CEILING IS BROUGHT UP TO DATE on a consumer that
		// is kept, because it is the count bound on every pull and a
		// consumer created by an earlier build carries the broker's own
		// default. A rebuilt one needs nothing: it was made from this
		// build's configuration.
		//
		// ITS OWN BUDGET, derived here from the caller's context rather
		// than taken from it or from what the read above left, because
		// none of those is right for it. The per-create context may be the
		// deadline that just expired — that is what puts the read-back
		// path here at all, and handed it this call fails instantly on a
		// consumer that was just found. The boot's has no deadline of its
		// own, so it would reach nats.go under the client's five-second
		// default, which is what every other replicated call on this path
		// was fixed for: an UpdateConsumer is a write against the same
		// metadata group as the create. And what a read leaves is sized
		// for a read, not for a write.
		//
		// So the caller passes the context that bounds the BOOT and this
		// owns the term, which is [jsprovision.Settle]'s arrangement for
		// the same reason. The rebuild below takes the boot's context too,
		// because [DomainConsumer.Reset] owns its terms the same way.
		alignCtx, cancel := context.WithTimeout(ctx, c.q.provisionBudget())
		defer cancel()
		aligned, err := c.q.alignDomainConsumer(alignCtx, c.stream, cons, info.Config)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.cons = aligned
		c.mu.Unlock()
		return nil
	}

	attrs := []any{"stream", c.stream, "consumer", c.name, "drift", string(drift),
		"checkpoint", after, "delivered", info.Delivered.Stream,
		"ack_floor", info.AckFloor.Stream, "in_flight", info.NumAckPending,
		"stream_first", first}
	if firstErr != nil {
		attrs = append(attrs, "stream_first_error", firstErr.Error())
	}
	resetErr := c.Reset(ctx, after)
	rebuilt := resetErr == nil
	if !rebuilt {
		var err error
		if rebuilt, err = c.afterFailedRebuild(ctx, drift, info, after, resetErr); err != nil {
			return err
		}
	}
	if !rebuilt {
		c.q.log.WarnContext(ctx, "jetstream_domain_consumer_rebuild_failed",
			append(attrs, "error", resetErr.Error(),
				"detail", "this node's reader disagrees with its checkpoint and "+
					"could not be rebuilt there, so it is kept as the broker "+
					"holds it, which costs time and no records — "+
					drift.leftCosts()+"; the next open rebuilds it")...)
		return nil
	}
	// A WARNING FOR THE ONE DIRECTION THAT SAYS SOMETHING HAPPENED TO THE
	// ROWS. The broker holding acknowledgements this node's rows do not
	// cover means the replicated database went backwards under the same
	// node id — a deletion or a restore — and an operator who did that is
	// told what it would have cost. The other three are what an unclean
	// stop or an adoption leaves, and a warning on every busy restart is
	// one that gets filtered out.
	//
	// STILL A WARNING when the stream's first sequence could not be read,
	// and the detail says why it may not be what it names: a raw
	// acknowledged-past is then either that deletion or restore, or a node
	// below the trim floor whose reader the broker placed at the first
	// surviving record. Demoting it would put the one case this warning
	// exists for among the lines nobody reads.
	level := slog.LevelInfo
	if drift == driftAcknowledged {
		level = slog.LevelWarn
	}
	detail := drift.detail()
	if firstErr != nil {
		detail += "; the stream's first sequence could not be read, so this " +
			"was judged on raw positions — which is also what a reader the " +
			"broker placed at a trimmed stream's first surviving record looks " +
			"like, on a node below the trim floor"
	}
	c.q.log.Log(ctx, level, "jetstream_domain_consumer_rebuilt",
		append(attrs, "detail", detail)...)
	return nil
}

// afterFailedRebuild is what an open does when the rebuild of a consumer that
// disagrees with its checkpoint fails, by the rule [Queue.DomainConsumer] sets
// out. It reports rebuilt when the consumer the broker holds is not the one
// that was judged: the create placed it before reporting failure.
func (c *DomainConsumer) afterFailedRebuild(ctx context.Context, drift consumerDrift,
	judged *jetstream.ConsumerInfo, after uint64, resetErr error) (rebuilt bool, err error) {

	if !drift.onlyCostsTime() {
		return false, fmt.Errorf("this node's checkpoint is %d and its consumer "+
			"disagrees with it (%s) and could not be rebuilt there — left "+
			"reading it, %s: %w", after, drift, drift.leftCosts(), resetErr)
	}
	// WHAT THE BROKER HOLDS NOW IS ASKED, never inferred from the error. A
	// delete that was refused left the consumer where it was, one that
	// landed left none, and one nobody answered for its whole budget may
	// have done either — so the handle is given what a lookup finds, and
	// the open fails when the lookup cannot say.
	held, err := c.lookup(ctx)
	switch {
	case err == nil:
		// WHICHEVER CONSUMER THE BROKER HOLDS UNDER THIS NAME, and the
		// name carries the node id, so it is one of two: the old one, when
		// the delete was refused, or the rebuilt one, when the create
		// placed it before reporting failure. Neither gives a record away.
		//
		// NOT ALIGNED: its ceiling is raised by an UpdateConsumer, a write
		// to the metadata group that has just refused this node one, and a
		// ceiling left at an earlier build's default makes a pull smaller
		// rather than wrong. The next open or reset makes it this build's.
		c.mu.Lock()
		c.cons = held
		c.mu.Unlock()
		info := held.CachedInfo()
		return info != nil && !info.Created.Equal(judged.Created), nil
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		return false, fmt.Errorf("this node's checkpoint is %d and its consumer "+
			"disagreed with it (%s); the rebuild deleted it and could not "+
			"create it again, so the broker holds no reader for this node: %w",
			after, drift, resetErr)
	default:
		return false, fmt.Errorf("this node's checkpoint is %d and its consumer "+
			"disagrees with it (%s) and could not be rebuilt there (%w) — and "+
			"whether the broker still holds it could not be read, so no handle "+
			"can say what this node would be reading: %w",
			after, drift, resetErr, err)
	}
}

// consumerDrift is how an existing consumer disagrees with the checkpoint it
// is opened for. Every drift is rebuilt the same way; what the drift decides
// is how the rebuild is REPORTED, and what an open does when the rebuild
// fails ([consumerDrift.onlyCostsTime]).
type consumerDrift string

const (
	// driftNone is a consumer whose next delivery is the record after the
	// checkpoint, with nothing held for anybody.
	driftNone consumerDrift = ""

	// driftAcknowledged is a consumer the broker has been told has handled
	// records past the checkpoint. It will never deliver them again.
	driftAcknowledged consumerDrift = "acknowledged_past_checkpoint"

	// driftDelivered is a consumer that has delivered records past the
	// checkpoint to a reader that is gone, and redelivers them only when
	// their ack window expires.
	driftDelivered consumerDrift = "delivered_past_checkpoint"

	// driftInFlight is a consumer holding deliveries at or below the
	// checkpoint for a reader that is gone, each one a slot of the
	// in-flight ceiling until its ack window expires.
	driftInFlight consumerDrift = "in_flight_for_a_gone_reader"

	// driftBehind is a consumer below the checkpoint, which would deliver
	// the records between for the applier to drop.
	driftBehind consumerDrift = "behind_checkpoint"
)

// detail is the operator's sentence for a rebuild.
func (d consumerDrift) detail() string {
	switch d {
	case driftAcknowledged:
		return "the broker was told records past this node's checkpoint were " +
			"applied, so it would never deliver them again — this node's " +
			"replicated database is older than its reader on the log, which is " +
			"what a deleted or restored database leaves; the reader is rebuilt " +
			"at the checkpoint and the node replays what its rows are missing"
	case driftDelivered:
		return "records past this node's checkpoint were delivered to a process " +
			"that is gone and would come back only after the ack window; the " +
			"reader is rebuilt at the checkpoint so they are delivered now"
	case driftInFlight:
		return "deliveries held for a process that is gone occupy the reader's " +
			"in-flight ceiling until the ack window expires; the reader is " +
			"rebuilt at the checkpoint without them"
	case driftBehind:
		return "the reader is below this node's checkpoint — an adoption or a " +
			"restore moved the rows without it — and would deliver every record " +
			"between for the applier to drop; it is rebuilt at the checkpoint"
	}
	return ""
}

// leftCosts is what a node LEFT reading a consumer with this drift pays, for
// the error an open fails with and the warning it keeps one under.
func (d consumerDrift) leftCosts() string {
	switch d {
	case driftAcknowledged:
		return "the broker will never deliver the records past this node's " +
			"checkpoint again, so the node waits for them for ever"
	case driftDelivered:
		return "the records past this node's checkpoint come back only when " +
			"their ack window expires, and any acknowledged one by one above " +
			"the ack floor never do, so a strict log can wait for ever"
	case driftInFlight:
		return "its deliveries for a process that is gone hold slots of the " +
			"in-flight ceiling until their ack window expires"
	case driftBehind:
		return "every record between it and this node's checkpoint is " +
			"delivered for the applier to drop"
	}
	return ""
}

// onlyCostsTime reports whether a node may be LEFT reading a consumer with
// this drift when its rebuild fails.
//
// BEHIND AND IN FLIGHT, because neither has delivered a record past the
// checkpoint: the worst either does is redeliver what the applier drops, or
// hold ceiling slots for one ack window. The two drifts ahead of the rows are
// not, and the delivered one is no exception although its records do come
// back — the ack floor is only a lower bound (see [driftOf]), so what looks
// merely delivered can hide records acknowledged one by one, which never do.
func (d consumerDrift) onlyCostsTime() bool {
	return d == driftBehind || d == driftInFlight
}

// firstMatters reports whether the stream's first sequence can change what
// [driftOf] answers for a consumer whose RAW positions judge as d.
//
// THE SKIP ONLY EVER BRINGS POSITIONS TOGETHER: [nextDelivery] raises every
// position it is given to at least the same first sequence, so positions that
// were equal stay equal and one that was no later than another stays no later.
// A consumer that agrees therefore agrees on any stream, and one holding
// deliveries at or below the checkpoint holds them on any stream, since
// neither of its positions was past the checkpoint and the in-flight count is
// not a position. Only a position strictly past the checkpoint (acknowledged,
// delivered) or strictly short of it (behind) can be one a trimmed prefix
// explains. [TestTheFirstSequenceIsReadOnlyWhenItCanChangeTheAnswer] holds
// this against the arithmetic in both directions.
func (d consumerDrift) firstMatters() bool {
	switch d {
	case driftAcknowledged, driftDelivered, driftBehind:
		return true
	}
	return false
}

// driftOf is how a consumer that has delivered through `delivered`,
// acknowledged every record through `ackFloor` and holds `inFlight`
// unacknowledged deliveries stands against a checkpoint at `after`, on a
// stream whose first surviving sequence is `first` (zero when unknown).
//
// PURE OVER VALUES, so the judgement is tested without a broker: which
// consumers are kept is the part of this that is easy to get subtly wrong, and
// a rule reachable only through a live stream is one nobody re-checks.
//
// # The order is the order of harm
//
// Acknowledged-past is checked first because it is the one no amount of
// waiting repairs. Delivered-past is next because it stalls a strict log for
// the whole ack window. In-flight below it costs ceiling slots for the same
// window. Behind costs only redeliveries.
//
// # The ack floor is a lower bound on what was acknowledged
//
// Records above it may have been acknowledged one by one, and those are never
// redelivered either. That is why delivered-past is rebuilt too rather than
// waited out: from outside the broker a record acknowledged above the floor
// and one merely delivered look the same.
func driftOf(delivered, ackFloor uint64, inFlight int, first, after uint64) consumerDrift {
	want := nextDelivery(after, first)
	switch {
	case nextDelivery(ackFloor, first) > want:
		return driftAcknowledged
	case nextDelivery(delivered, first) > want:
		return driftDelivered
	case inFlight > 0:
		return driftInFlight
	case nextDelivery(delivered, first) < want:
		return driftBehind
	}
	return driftNone
}

// nextDelivery is the first sequence a consumer positioned after `through`
// hands over, on a stream whose first surviving sequence is `first`.
//
// THE BROKER SKIPS WHAT THE STREAM NO LONGER HOLDS: a consumer whose next
// sequence was trimmed resumes at the first survivor, and one created below it
// is placed there outright and reports the survivor's predecessor as already
// delivered. So two positions that differ only below the first survivor are
// the SAME position, and treating them as different would rebuild the
// consumer of a node that is below the floor on every boot — into exactly the
// place it already was.
func nextDelivery(through, first uint64) uint64 {
	return max(through+1, first)
}

// firstSequence is the first sequence the stream still holds, or zero for one
// nothing has been written to. It is a READ, sized and re-asked as one — see
// [Queue.askRead].
func (q *Queue) firstSequence(ctx context.Context, stream string) (uint64, error) {
	var s jetstream.Stream
	if err := q.askRead(ctx, func(ctx context.Context) error {
		var e error
		s, e = q.js.Stream(ctx, stream)
		return e
	}); err != nil {
		return 0, fmt.Errorf("read %s's first sequence: %w", stream, err)
	}
	return s.CachedInfo().State.FirstSeq, nil
}

// lookup asks the broker for this node's consumer, as a read — see
// [Queue.askRead].
func (c *DomainConsumer) lookup(ctx context.Context) (jetstream.Consumer, error) {
	var cons jetstream.Consumer
	err := c.q.askRead(ctx, func(ctx context.Context) error {
		var e error
		cons, e = c.q.js.Consumer(ctx, c.stream, c.name)
		return e
	})
	return cons, err
}

// askRead runs one metadata READ on a domain consumer's open: under
// [Queue.lookupBudget] as its ceiling, re-issued by [jsprovision.Ask] while
// nobody answers.
//
// # Why a read on this path is not sized like the writes beside it
//
// For [jsprovision.LookupBudget]'s reason: a create has to be agreed by a
// quorum, and a read is slow only because the group cannot answer it yet —
// in which case its request was destroyed rather than delayed, and what gets
// an answer is asking again. The lookup went through this idiom already; the
// stream's first sequence was read once, on the provisioning budget — two
// minutes on a fleet — so one request the group dropped stalled its domain
// that long with nothing in the log, and three of them, one per domain, spent
// more than the ceiling the three domains' consumers share.
//
// ON THE CALLER'S CONTEXT, which is the one that bounds the boot, and never on
// what an earlier read or write left: each gets its own term, so no one of
// them decides how long the next may take.
func (q *Queue) askRead(ctx context.Context, read func(context.Context) error) error {
	readCtx, cancel := context.WithTimeout(ctx, q.lookupBudget())
	defer cancel()
	return jsprovision.Ask(readCtx, q.Clustered().AskTerm(), read, nil)
}

// alignDomainConsumer updates a kept consumer's updatable bounds to this
// build's, leaving its position alone.
//
// It is handed the configuration the consumer REPORTED rather than building
// one from this build's defaults, because the update carries every field: a
// config built fresh would reset the start policy, which the broker refuses to
// move and which is the checkpoint's to decide.
func (q *Queue) alignDomainConsumer(ctx context.Context, stream string,
	cons jetstream.Consumer, config jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	if config.MaxAckPending == domainConsumerMaxAckPending &&
		config.AckWait == domainConsumerAckWait {
		return cons, nil
	}
	config.MaxAckPending = domainConsumerMaxAckPending
	config.AckWait = domainConsumerAckWait
	updated, err := q.js.UpdateConsumer(ctx, stream, config)
	if err != nil {
		return nil, fmt.Errorf("raise the consumer's in-flight ceiling to %d: %w",
			domainConsumerMaxAckPending, err)
	}
	return updated, nil
}

// domainConsumerName is what this node's reader is called on the broker.
//
// A node id can hold characters a consumer name may not, and two node ids can
// differ only in one of them — so the readable half is escaped and a digest
// over the exact pair is appended, exactly as the mailbox consumer's name is
// built. Truncation cannot reintroduce an alias, because the digest is taken
// over the full pair and appended after it.
func domainConsumerName(stream, nodeID string) string {
	safe := func(s string) string {
		return strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_").Replace(s)
	}
	sum := sha256.Sum256([]byte(nodeID + "\x00" + stream))
	id := hex.EncodeToString(sum[:6])

	readable := "statelog__" + safe(stream) + "__" + safe(nodeID)
	if max := consumerNameMax - len(id) - 2; len(readable) > max {
		readable = readable[:max]
	}
	return readable + "__" + id
}

// Fetch implements [statelog.Fetcher].
//
// A message that cannot report its own sequence is DROPPED with its
// acknowledgement withheld, rather than passed on with a zero: the sequence is
// the applier's checkpoint, its contiguity check and the next writer's
// expectation, so a record delivered as sequence zero would move the cursor
// backwards on every node that applied it.
//
// # Everything a pull delivered is returned
//
// A byte-bounded pull cannot also be count-bounded by this client, and the
// count is enforced by the consumer's own in-flight ceiling instead — see
// [domainConsumerMaxAckPending]. So maxMessages is passed to the broker only
// on the count-bounded path, and on the byte-bounded one the batch is drained
// whole: a message the broker delivered and this call did not return is one it
// holds against that ceiling and redelivers after the ack window, which is a
// hole in the applier's run for thirty seconds.
func (c *DomainConsumer) Fetch(ctx context.Context, maxMessages, maxBytes int,
	wait time.Duration) ([]statelog.Message, error) {

	if maxMessages <= 0 {
		return nil, nil
	}
	opts := []jetstream.FetchOpt{jetstream.FetchMaxWait(wait)}
	var batch jetstream.MessageBatch
	var err error
	cons, err := c.consumerFor(ctx)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 {
		batch, err = cons.FetchBytes(maxBytes, opts...)
	} else {
		batch, err = cons.Fetch(maxMessages, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("jetstream: fetch from %s: %w", c.name, err)
	}

	// THE CONTEXT ENDS THE DRAIN, and until it did this function took a
	// context and used it for nothing but its own return value.
	//
	// A batch closes when it is FULL or when `wait` expires — one record
	// arriving does not end it — so a caller that wanted the records
	// already in hand had no way to say so and paid the whole wait. The
	// applier's [statelog.Runner] is exactly that caller: a barrier
	// appended onto a quiet log sat in a batch nobody had finished
	// collecting for five seconds, against a two second read budget, so
	// every linearizable read on an idle company refused `behind`.
	//
	// WHAT IS COLLECTED IS RETURNED. A cancelled drain is this caller
	// deciding it has waited long enough, not a failure — the messages
	// already taken are real, and the ones still in the batch are
	// redelivered because they were never acknowledged.
	var out []statelog.Message
	msgs := batch.Messages()
	for {
		var msg jetstream.Msg
		var open bool
		select {
		case msg, open = <-msgs:
			if !open {
				msg = nil
			}
		case <-ctx.Done():
		}
		if msg == nil {
			break
		}
		meta, err := msg.Metadata()
		if err != nil {
			// NOT ACKNOWLEDGED. A message whose metadata is
			// unreadable is one this node cannot place in the log,
			// and acknowledging it would move the broker's floor
			// past a record nothing applied.
			continue
		}
		out = append(out, statelog.Message{
			Seq:      meta.Sequence.Stream,
			StoredAt: meta.Timestamp,
			Payload:  msg.Data(),
			Ack:      msg.Ack,
		})
	}
	if err := batch.Error(); err != nil && !errors.Is(err, context.Canceled) {
		return out, fmt.Errorf("jetstream: fetch from %s: %w", c.name, err)
	}
	return out, ctx.Err()
}

// Pending implements [statelog.Fetcher]: how many records this consumer has
// not delivered, which is what tells a partially filled batch whether waiting
// would buy anything.
func (c *DomainConsumer) Pending(ctx context.Context) (uint64, error) {
	cons, err := c.consumerFor(ctx)
	if err != nil {
		return 0, err
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("jetstream: read %s: %w", c.name, err)
	}
	return info.NumPending, nil
}

// Name is what this consumer is called on the broker, for an operator tracing
// a stalled applier back to the thing that is not delivering.
func (c *DomainConsumer) Name() string { return c.name }

// DomainGroup is a FLEET-WIDE pull consumer over one domain's log.
//
// # Why this exists beside the per-node one
//
// [DomainConsumer] is replication: every node reads every record, so each has
// its own. This is the opposite shape — one consumer SHARED by the fleet,
// where a record is handled by whichever node gets there first — and it is
// what a change feed needs.
//
// A GROUP RATHER THAN A DUTY, and the difference is a real outage: a duty is
// held by one node under a lease, so a lease flap stalls the whole company's
// notifications for work that is stateless. A group has no holder to lose.
type DomainGroup struct {
	cons     jetstream.Consumer
	messages jetstream.MessagesContext
	stream   string
	name     string

	// closed ends the context watcher, and once guards the iterator's
	// stop so a Stop from the caller and one from a cancelled context are
	// the same Stop.
	//
	// THE ITERATOR HAS NO CONTEXT OF ITS OWN. [jetstream.MessagesContext.Next]
	// blocks until a message arrives or the iterator is stopped, and a
	// quiet log means it blocks for ever — so a caller that only
	// cancelled a context would hang on the goroutine it was joining.
	// That is not a theoretical shutdown wart: the tracker's wake feed
	// sits in exactly this call, and it wedged every engine test that
	// stopped before its own context expired.
	closed chan struct{}
	once   sync.Once
}

// DomainDelivery is one record delivered to a group.
type DomainDelivery struct {
	// Subject is the wire subject, which is what a translator reads the
	// object's kind and id out of.
	Subject string

	// Seq is the broker's sequence and StoredAt its own timestamp — the
	// instant every node reads identically.
	Seq      uint64
	StoredAt time.Time

	Payload []byte

	// Ack marks the record handled, and Nak returns it for redelivery
	// after a delay. A handler that could not reach something it needs
	// naks; one that decided the record means nothing to it ACKS, because
	// a decision is handling.
	Ack func() error
	Nak func(time.Duration) error
}

// Group opens (or creates) a fleet-wide consumer over this log.
//
// DELIVER ALL, always. A group created at the head exists and still discards
// everything published before its first consumer — which for a wake feed is
// every notification the company owed while nothing was watching.
func (l *DomainLog) Group(ctx context.Context, name string) (*DomainGroup, error) {
	if name == "" {
		return nil, fmt.Errorf("jetstream: a domain group has no name — it is " +
			"the durable consumer's identity, and an unnamed one would be a " +
			"fresh consumer per process that replays the whole log on restart")
	}
	safe := domainGroupName(l.name, name)
	// THROUGH THE PACKAGE'S ONE DURABLE-CONSUMER PATH, so a group every
	// node of a fleet ensures at boot survives a peer winning the race —
	// see [Queue.ensureDurableConsumer].
	cons, _, err := l.q.ensureDurableConsumer(ctx, l.name, jetstream.ConsumerConfig{
		Durable:       safe,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       domainConsumerAckWait,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		// NO MaxDeliver, for the reason the per-node consumer gives: a
		// record nobody could handle yet is a retry rather than a poison
		// message, and a budget that ran out would drop a wake silently.
		MaxDeliver: -1,
	})
	if err != nil {
		return nil, fmt.Errorf("jetstream: open the group %s on %s: %w",
			safe, l.name, err)
	}
	messages, err := cons.Messages()
	if err != nil {
		return nil, fmt.Errorf("jetstream: consume %s on %s: %w", safe, l.name, err)
	}
	group := &DomainGroup{
		cons: cons, messages: messages, stream: l.name, name: safe,
		closed: make(chan struct{}),
	}
	// The context is turned into a Stop by a goroutine, for the reason
	// [DomainGroup.closed] gives. It exits with the group, so a
	// long-lived process holds one goroutine per group rather than one
	// per read.
	go func() {
		select {
		case <-ctx.Done():
			group.stop()
		case <-group.closed:
		}
	}()
	return group, nil
}

// domainGroupName escapes a group's name the way every derived consumer name
// here is escaped, and appends a digest over the exact pair.
func domainGroupName(stream, group string) string {
	safe := func(s string) string {
		return strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_").Replace(s)
	}
	sum := sha256.Sum256([]byte(group + "\x00" + stream))
	id := hex.EncodeToString(sum[:6])
	readable := safe(group) + "__" + safe(stream)
	if max := consumerNameMax - len(id) - 2; len(readable) > max {
		readable = readable[:max]
	}
	return readable + "__" + id
}

// Next blocks for the next delivery.
//
// A NIL DELIVERY WITH A NIL ERROR MEANS THE CONSUMER CLOSED, which is how a
// caller tells a shutdown from a failure — the two have opposite responses,
// and a shutdown reported as a failure is a log line on every clean stop.
func (g *DomainGroup) Next(ctx context.Context) (*DomainDelivery, error) {
	msg, err := g.messages.Next()
	switch {
	case errors.Is(err, jetstream.ErrMsgIteratorClosed):
		return nil, nil
	case err != nil:
		// A CANCELLED CALLER IS THE SAME ANSWER AS A CLOSED ITERATOR. The
		// goroutine this group started turns ctx.Done into a Drain, so a
		// cancellation reaches the iterator by two routes at once and
		// whichever lands first decides what Next returns — the tidy
		// ErrMsgIteratorClosed above, or the underlying read failing
		// mid-drain. Reporting the second as a failure would make a clean
		// shutdown log an error on the race's losing half.
		if ctx.Err() != nil {
			//nolint:nilerr // Swallowing it IS the contract: a nil delivery
			// with a nil error means the consumer closed, which is how the
			// feed above tells a shutdown from a failure.
			return nil, nil
		}
		return nil, fmt.Errorf("jetstream: read %s: %w", g.name, err)
	}
	meta, err := msg.Metadata()
	if err != nil {
		// NOT ACKNOWLEDGED, and not returned: a delivery this node
		// cannot place in the log is one it cannot derive a stable wake
		// id from, and acknowledging it would drop the wake outright.
		return nil, fmt.Errorf("jetstream: read %s's metadata: %w", g.name, err)
	}
	return &DomainDelivery{
		Subject: msg.Subject(), Seq: meta.Sequence.Stream,
		StoredAt: meta.Timestamp, Payload: msg.Data(),
		Ack: msg.Ack,
		Nak: func(delay time.Duration) error { return msg.NakWithDelay(delay) },
	}, nil
}

// Stop ends this process's consumption. THE DURABLE POSITION SURVIVES, which
// is what makes a restart resume rather than replay: it is the fleet's
// position, not this process's.
func (g *DomainGroup) Stop() error {
	g.stop()
	return nil
}

// stop ends the iterator once, however it was reached.
//
// DRAIN RATHER THAN Stop, which is the choice [internal/coord/kv]'s feed makes
// for the same reason: a drain naks what it has pulled ahead and not yet
// handed over, so a peer receives those records at once rather than after the
// full ack window. This node is going away and has done nothing with them.
func (g *DomainGroup) stop() {
	g.once.Do(func() {
		close(g.closed)
		g.messages.Drain()
	})
}

// Name is what this group is called on the broker.
func (g *DomainGroup) Name() string { return g.name }

// GroupAckFloor is how far a fleet-wide group has acknowledged, WITHOUT
// attaching to it.
//
// # Why the retention gate cannot just ask the group
//
// The trim's feed term is "a record the wake feed has not seen is one nobody
// has been told about", and the node evaluating it is whichever one holds the
// trim duty — which is not necessarily a node running the feed at all. So the
// question has to be answerable from the consumer's NAME, which is stable by
// construction ([DomainLog.Group] derives it from the stream and the group),
// rather than from a handle.
//
// It reports (0, false, nil) when no such consumer exists. That is an ordinary
// answer and a load-bearing one: a fleet that has never run the feed has not
// failed to read it, and the two must not look alike — an unreadable term
// blocks the trim, while an absent feed is a domain that does not have one.
func (l *DomainLog) GroupAckFloor(ctx context.Context, group string) (uint64, bool, error) {
	cons, err := l.stream.Consumer(ctx, domainGroupName(l.name, group))
	switch {
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("jetstream: read the group %q on %s: %w",
			group, l.name, err)
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("jetstream: read the group %q on %s: %w",
			group, l.name, err)
	}
	// THE ACK FLOOR RATHER THAN THE DELIVERED SEQUENCE. Delivered says a
	// record left the broker; the floor says every record below it was
	// handled. The trim needs the second, because a wake the feed fetched
	// and had not finished acting on is one the record still has to be
	// there for.
	return info.AckFloor.Stream, true, nil
}
