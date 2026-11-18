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
	"github.com/crewlet/crewlet/internal/textcut"

	"github.com/crewlet/crewlet/internal/statelog"
)

// DomainConsumer is one node's own reader of one domain's log.
//
// Durable and named after the node, never shared by the fleet: every data
// node applies every record, and a shared consumer would deliver each record
// to one node only, the opposite of replication.
//
// Its acknowledgement floor is not the checkpoint. The applier's checkpoint
// commits with the rows in SQL; the floor moves after that commit and can lag
// a crash, so the loop resumes from the SQL checkpoint and a lost ack costs a
// redelivery, never a hole. What the consumer already gave away cannot be
// asked back, so an open compares the two and rebuilds a consumer that
// disagrees with the rows: see [Queue.DomainConsumer].
type DomainConsumer struct {
	q      *Queue
	stream string
	name   string

	// cons is the broker-side consumer, under mu because [Reset] replaces it
	// while the applier holding this handle is between runs.
	//
	// NIL IS A REAL STATE: a reset that deleted and then failed to create
	// leaves the broker with no consumer, and a handle still naming the
	// deleted one would fail every fetch for the life of the process. Cleared
	// instead, and rebuilt from want by [DomainConsumer.consumerFor].
	mu   sync.Mutex
	cons jetstream.Consumer

	// want is this consumer's configuration, kept so a handle cleared by a
	// failed reset can be rebuilt.
	want jetstream.ConsumerConfig

	// pull is the consumer's standing pull (pullrequest.go): the one inbox
	// every fetch's request is answered on, and what it delivered that no
	// fetch took. Made by the first fetch, dropped by a reset (its deliveries
	// came from the deleted consumer), released by [Close] (the consumer
	// stays). Guarded by mu.
	pull *puller
	// closed is [DomainConsumer.Close] having run. Guarded by mu.
	closed bool
}

// domainConsumerMaxAckPending is how many records the broker may hand this
// node's applier before one is acknowledged.
//
// [statelog.FetchMessages], the applier's transaction budget. One pull
// already carries that count ([DomainConsumer.Fetch]); this bounds it across
// pulls, so records whose transaction has not committed hold back the next
// pull rather than piling up behind it. It is also how many barriers the
// applier can reach while their checkpoint's commit is stalled (they are
// acknowledged only after it); [statelog.FetchMessages] says what that covers.
const domainConsumerMaxAckPending = statelog.FetchMessages

// domainConsumerAckWait is how long the broker waits for an acknowledgement
// before redelivering.
//
// Thirty seconds, far above the row and time budgets bounding the applier's
// own transaction. A redelivery costs one idempotent re-apply, so the bound
// exists to release the broker's redelivery state, not to protect the applier.
const domainConsumerAckWait = 30 * time.Second

// DomainConsumer opens (or creates) this node's durable reader of a domain's
// log, resuming at `after + 1`, past the SQL checkpoint (the beginning when
// `after` is zero): never the head, which skips what was published before the
// consumer existed, nor the beginning on a node with a checkpoint, which
// redelivers everything for the applier to drop.
//
// An existing consumer is kept only when it hands over exactly what a fresh
// one would ([driftOf]); any other is rebuilt through [DomainConsumer.Reset],
// because the broker will not move a start sequence and a create-or-update
// reports success without moving it. A failed rebuild is decided by
// [DomainConsumer.afterFailedRebuild]; package doc, "A domain consumer is held
// to its node's rows", has the four drifts and what each costs.
func (q *Queue) DomainConsumer(ctx context.Context, stream, nodeID string,
	after uint64) (*DomainConsumer, error) {

	if nodeID == "" {
		return nil, fmt.Errorf("jetstream: a domain consumer has no node id — " +
			"every data node reads the whole log, so the consumer is named " +
			"after the node and a shared name would deliver each record to " +
			"one of them")
	}
	name := domainConsumerName(stream, nodeID)
	config := domainConsumerConfig(name, after)
	// The handle exists before the broker-side consumer, so the rebuild below
	// is [DomainConsumer.Reset] itself, whose half-done state is repairable.
	handle := &DomainConsumer{q: q, stream: stream, name: name, want: config}

	// Only an absent consumer is created here, and whatever the broker ends up
	// holding is judged by [DomainConsumer.resumeAt] — not through
	// [Queue.ensureDurableConsumer], whose create-or-read-back keeps an
	// existing consumer unjudged. So the breadcrumb and the create's
	// provisioning budget are this site's own: the caller's ctx may carry no
	// deadline, and the client's five-second default would decide a clustered
	// boot. The breadcrumb spans the lookup and the create, stops where the
	// create ends (it must name the step this member is on), and watches ctx
	// because the two halves carry separate deadlines.
	stop := jsprovision.WhenSlow(ctx, func(after time.Duration) {
		q.log.WarnContext(ctx, "jetstream_consumer_slow", "stream", stream,
			"consumer", name, "waited", after,
			"detail", "this state-log consumer is still being provisioned — "+
				"looked up, and created if it was absent; on a fleet that is "+
				"a metadata group that has not settled")
	})

	// The lookup is a read, sized and re-asked as one ([askRead]) on its own
	// ceiling; the create's budget starts once it returns, so a probe the
	// group never answered cannot spend the create's budget.
	cons, err := handle.lookup(ctx)
	if jsprovision.Unanswered(ctx, err) {
		// Told nothing, which is not "absent" ([jsprovision.Unanswered]).
		// Reported as not-found: the create below settles it either way, as
		// it does a held create's timeout.
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
		// The create's budget is derived once the lookup has returned, as at
		// every create site ([Queue.createOrObserveStream]): derived before,
		// an unanswered lookup spends it, and on a solo broker (equal
		// ceilings) hands the create an expired context.
		//
		// Not shadowing ctx, for [jsprovision.Settle]'s reason: the
		// read-back below must not inherit a deadline this create may
		// have spent.
		createCtx, cancel := context.WithTimeout(ctx, q.provisioning().Budget)
		defer cancel()
		// Waited out, for [Queue.ensureDurableConsumer]'s reason: placed by
		// the same metadata group as its stream, so "no suitable peers" is
		// transient here too.
		err = q.provisioning().Place(createCtx, func(ctx context.Context) error {
			q.noteConsumerProposal()
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
			// A create can answer with a consumer already there: nats.go
			// returns the existing one when its config matches, and a node
			// whose rows are gone asks for its first boot's config
			// (DeliverAll). So it is judged like any other, on the state the
			// create returned; a really fresh one agrees by construction.
			err = handle.resumeAt(ctx, cons, after)
		case jsprovision.NoApplicableLimit(err):
			// No limit applies to this consumer at all (the account's tiered
			// limits carry none for the class `stream.replicas` puts this
			// node in; server/consumer.go, acc.selectLimits). Never a lost
			// race: read back, it would report a reader this node lost
			// rather than an account with no limit for it.
			err = fmt.Errorf("%w%s", err,
				jsprovision.NoApplicableLimitDetail(q.cfg.Replicas))
		case !jsprovision.Refused(err):
			// The lookup fell inside the propagation window, so the create
			// met a consumer THIS node made on an earlier boot (the name
			// carries the node id). Every error but a refusal: nats.go
			// returns ErrConsumerExists only when the configs differ, and a
			// create the server held while the group settled comes back a
			// timeout with the consumer there, the shape a clustered boot
			// produces. A refusal ([jsprovision.Refused]) placed nothing.
			//
			// Read back and held to the checkpoint, like the lookup's found
			// branch below.
			createErr := err
			cons, err = jsprovision.SettleWith(ctx, q.provisioning(),
				func(ctx context.Context) (jetstream.Consumer, error) {
					return q.js.Consumer(ctx, stream, name)
				})
			switch {
			case err == nil:
				err = handle.resumeAt(ctx, cons, after)
			default:
				// The create's error is reported; the read-back's only
				// confirms the consumer is absent.
				err = fmt.Errorf("%w (and it is not there: %w)", createErr, err)
			}
		}
	case err == nil:
		// Stopped before the judgement: the breadcrumb names the lookup and
		// the create, and a rebuild carries its own in [DomainConsumer.Reset].
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
// positioned to resume after `after`. One function for the open and the
// reset, so a rebuilt consumer cannot carry a ceiling or ack wait the boot's
// never had.
func domainConsumerConfig(name string, after uint64) jetstream.ConsumerConfig {
	config := jetstream.ConsumerConfig{
		Durable:       name,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       domainConsumerAckWait,
		MaxAckPending: domainConsumerMaxAckPending,
		// No MaxDeliver: a record this node cannot apply is a transient
		// failure the loop retries or a version this build defers, never
		// poison, and a spent budget would leave the node silently past it.
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
// broker-side consumer and creating it again there: the broker treats a start
// sequence as immutable. An adoption calls it when its checkpoint jumps to a
// snapshot's position (left alone, the consumer would deliver everything
// between for the applier to drop), and an open when its consumer disagrees
// with the rows ([Queue.DomainConsumer]). An adoption's applier resumes from
// the checkpoint whatever the consumer says, so a failed reset there costs
// only redeliveries; an open decides in [DomainConsumer.afterFailedRebuild].
//
// A delete that lands and a create that fails leave no consumer on the
// broker, and the caller only logs this error, so the handle is CLEARED then
// and the next fetch rebuilds it ([DomainConsumer.consumerFor]).
func (c *DomainConsumer) Reset(ctx context.Context, after uint64) error {
	config := domainConsumerConfig(c.name, after)
	c.mu.Lock()
	defer c.mu.Unlock()
	// One breadcrumb for both halves: a member stalled on either waits on the
	// same metadata group, and an open reaches this on the boot path, where a
	// silent stall is what [jsprovision.WhenSlow] exists to name.
	stop := jsprovision.WhenSlow(ctx, func(waited time.Duration) {
		c.q.log.WarnContext(ctx, "jetstream_consumer_slow", "stream", c.stream,
			"consumer", c.name, "waited", waited, "resume_at", after+1,
			"detail", "this state-log consumer is still being rebuilt at its "+
				"checkpoint — deleted and created again; on a fleet that is a "+
				"metadata group that has not settled")
	})
	defer stop()
	// The delete is budgeted and re-asked like the create: an adoption's ctx
	// has no deadline, leaving the request term (jsapi.RequestTerm, five
	// seconds) to fail a request the group dropped. Asking twice is safe: a
	// delete that landed is not-found the second time, the answer wanted.
	deleteCtx, cancelDelete := context.WithTimeout(ctx, c.q.provisioning().Budget)
	defer cancelDelete()
	if err := c.q.provisioning().Ask(deleteCtx,
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
	// The delete has landed: the handle must not name a consumer the broker
	// no longer has, and the standing pull's records came from it, so they go
	// with its inbox; the new consumer hands them over again.
	c.cons = nil
	if c.pull != nil {
		c.pull.close()
		c.pull = nil
	}
	// Through [jsprovision.Place], with a budget of its own: placed by the
	// same metadata group as the durable consumers beside it, it meets the
	// same placement refusals and unanswered requests.
	createCtx, cancelCreate := context.WithTimeout(ctx, c.q.provisioning().Budget)
	defer cancelCreate()
	var cons jetstream.Consumer
	err := c.q.provisioning().Place(createCtx,
		func(ctx context.Context) error {
			c.q.noteConsumerProposal()
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
		// want is left as it was: a rebuild restores the consumer at its
		// PREVIOUS position, not one the broker just refused. An adoption
		// accepts the redeliveries that costs; an open never returns a
		// handle in this state ([DomainConsumer.afterFailedRebuild]).
		return fmt.Errorf("jetstream: recreate the domain consumer %s on %s at "+
			"%d: %w", c.name, c.stream, after, err)
	}
	c.cons, c.want = cons, config
	return nil
}

// consumerFor is the current broker-side consumer, created if a failed reset
// left none: here because this is where the absence is found, and every
// caller wants the last reset's position. Idempotent: a consumer the broker
// still has comes back from CreateConsumer unchanged.
func (c *DomainConsumer) consumerFor(ctx context.Context) (jetstream.Consumer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cons != nil {
		return c.cons, nil
	}
	// The same budget and re-ask as Reset's create: the identical call,
	// facing the identical transient conditions.
	createCtx, cancelCreate := context.WithTimeout(ctx, c.q.provisioning().Budget)
	defer cancelCreate()
	var cons jetstream.Consumer
	err := c.q.provisioning().Place(createCtx,
		func(ctx context.Context) error {
			c.q.noteConsumerProposal()
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

// resumeAt leaves the handle holding a consumer that resumes at `after + 1`:
// cons when it agrees with the checkpoint, a rebuilt one when it does not, and
// whatever the broker holds when the rebuild fails for a drift that only costs
// time ([DomainConsumer.afterFailedRebuild]).
//
// It judges the state the lookup or create returned, not a second request:
// nothing reads this consumer while it is opened. Acknowledgements a departed
// reader sent may still be queued on the server (an ack is fire-and-forget,
// and a lookup is answered by another worker), showing as in flight; that
// costs at most an unneeded rebuild, and never keeps a consumer that needed
// one, since only a consumer with nothing in flight is kept.
func (c *DomainConsumer) resumeAt(ctx context.Context, cons jetstream.Consumer,
	after uint64) error {

	info := cons.CachedInfo()
	if info == nil {
		// A read, sized and re-asked as one ([askRead]), through a handle
		// per request: a handle's Info caches its reply unlocked, and a read
		// may have two requests out at once.
		var err error
		info, err = askRead(ctx, c.q, func(ctx context.Context) (*jetstream.ConsumerInfo, error) {
			fresh, lookupErr := c.q.js.Consumer(ctx, c.stream, c.name)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if held := fresh.CachedInfo(); held != nil {
				return held, nil
			}
			return fresh.Info(ctx)
		})
		if err != nil {
			return fmt.Errorf("read the consumer's state: %w", err)
		}
	}
	// Raw positions are judged first, and the stream's first sequence read
	// only when it can change the answer ([consumerDrift.firstMatters]), so
	// the in-flight consumer a busy restart leaves costs no request. When that
	// read fails the skip is not credited and the consumer is rebuilt, which
	// is always correct, so the failure does not fail the open.
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
		// Kept as it is: every consumer of this name came from
		// [domainConsumerConfig]. The rebuild below takes the boot's ctx, as
		// [DomainConsumer.Reset] sets its own terms.
		c.mu.Lock()
		c.cons = cons
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
	// A warning only for acknowledged-past, which says the rows went
	// backwards under this node id (a deletion or a restore); the other three
	// are what an unclean stop or an adoption leaves, and a warning on every
	// busy restart gets filtered out. Still a warning when the first sequence
	// was unreadable, the detail saying it may instead be a node below the
	// trim floor: demoted, the case it exists for would go unread.
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
// disagrees with its checkpoint fails. A drift that only costs time
// ([consumerDrift.onlyCostsTime]) keeps whatever consumer the broker holds,
// with a warning: failing a boot over an optimisation's metadata write would
// leave it worse off than not rebuilding. Any other drift fails the open, as
// does a broker holding no consumer or unable to say which, since a handle
// kept on a guess fails every fetch while the boot reports success. It reports
// rebuilt when the held consumer is not the judged one: the create placed it
// before reporting failure.
func (c *DomainConsumer) afterFailedRebuild(ctx context.Context, drift consumerDrift,
	judged *jetstream.ConsumerInfo, after uint64, resetErr error) (rebuilt bool, err error) {

	if !drift.onlyCostsTime() {
		return false, fmt.Errorf("this node's checkpoint is %d and its consumer "+
			"disagrees with it (%s) and could not be rebuilt there — left "+
			"reading it, %s: %w", after, drift, drift.leftCosts(), resetErr)
	}
	// What the broker holds is asked, never inferred from the error: a
	// refused delete left the consumer, a landed one left none, and an
	// unanswered one may have done either.
	held, err := c.lookup(ctx)
	switch {
	case err == nil:
		// The name carries the node id, so this is the old consumer (delete
		// refused) or the rebuilt one (placed before the create failed):
		// neither gives a record away, and both came from [domainConsumerConfig].
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
	// records past the checkpoint. It will never deliver them again. The
	// rows going backwards under the same node id leave it: the replicated
	// database deleted, or restored from a backup (which copies the store
	// before the broker estate).
	driftAcknowledged consumerDrift = "acknowledged_past_checkpoint"

	// driftDelivered is a consumer that has delivered records past the
	// checkpoint to a reader that is gone (a stop mid-drain, a pull answered
	// after its caller left), and redelivers them only when their ack window
	// expires; a strict log waits behind the first of them until then.
	driftDelivered consumerDrift = "delivered_past_checkpoint"

	// driftInFlight is a consumer holding deliveries at or below the
	// checkpoint for a reader that is gone, each one a slot of the
	// in-flight ceiling until its ack window expires: a full ceiling, from a
	// process that committed its pull and died before acknowledging, hands
	// over nothing new until then.
	driftInFlight consumerDrift = "in_flight_for_a_gone_reader"

	// driftBehind is a consumer below the checkpoint, which would deliver
	// the records between for the applier to drop: after a crash, or a
	// snapshot adoption at boot that moved the checkpoint.
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
// this drift when its rebuild fails: behind and in flight, which delivered
// nothing past the checkpoint. Not delivered-past, though its records come
// back: the ack floor is only a lower bound ([driftOf]), so it can hide
// records acknowledged one by one, which never do.
func (d consumerDrift) onlyCostsTime() bool {
	return d == driftBehind || d == driftInFlight
}

// firstMatters reports whether the stream's first sequence can change what
// [driftOf] answers for a consumer whose RAW positions judge as d.
//
// The skip ([nextDelivery]) raises every position to at least the same first
// sequence, so it only brings positions together: agreement, and in-flight
// (no position past the checkpoint; the count is not a position), hold on any
// stream. Only a position strictly past the checkpoint or short of it can be
// one a trimmed prefix explains.
// [TestTheFirstSequenceIsReadOnlyWhenItCanChangeTheAnswer] holds this in both
// directions.
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
// stream whose first surviving sequence is `first` (zero when unknown). Pure
// over values, so which consumers are kept is tested without a broker.
//
// The order is the order of harm: acknowledged-past no waiting repairs,
// delivered-past stalls a strict log for the ack window, in-flight holds
// ceiling slots for it, behind costs redeliveries. Delivered-past is rebuilt
// rather than waited out because the ack floor is only a lower bound: records
// acknowledged one by one above it are never redelivered, and from outside
// look merely delivered.
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
// The broker skips what the stream no longer holds (a consumer created below
// the first survivor is placed there and reports its predecessor delivered),
// so positions differing only below it are the SAME position; treating them as
// different would rebuild a below-floor node's consumer on every boot into the
// place it already was.
func nextDelivery(through, first uint64) uint64 {
	return max(through+1, first)
}

// firstSequence is the first sequence the stream still holds, or zero for one
// nothing has been written to. It is a READ, sized and re-asked as one — see
// [askRead].
func (q *Queue) firstSequence(ctx context.Context, stream string) (uint64, error) {
	s, err := askRead(ctx, q, func(ctx context.Context) (jetstream.Stream, error) {
		return q.js.Stream(ctx, stream)
	})
	if err != nil {
		return 0, fmt.Errorf("read %s's first sequence: %w", stream, err)
	}
	return s.CachedInfo().State.FirstSeq, nil
}

// lookup asks the broker for this node's consumer, as a read — see
// [askRead].
func (c *DomainConsumer) lookup(ctx context.Context) (jetstream.Consumer, error) {
	return askRead(ctx, c.q, func(ctx context.Context) (jetstream.Consumer, error) {
		return c.q.js.Consumer(ctx, c.stream, c.name)
	})
}

// askRead runs one metadata READ on a state log's bring-up (its stream's
// ceiling when the logs are sized, and every read a domain consumer's open
// makes): at [Queue.provisioning]'s cadences, re-issued by
// [jsprovision.ReadWith] while nobody answers, every request kept outstanding,
// never past the lookup ceiling. Not sized like the writes beside it, for
// [jsprovision.LookupBudget]'s reason: a slow read's request was dropped, not
// delayed, and asking again is what gets an answer.
//
// It runs on the caller's ctx, so no earlier read or write decides how long
// this one may take. A function only because a method cannot take a type
// parameter; it returns what it read because [jsprovision.ReadWith] may have
// several requests out at once.
func askRead[T any](ctx context.Context, q *Queue, read func(context.Context) (T, error)) (T, error) {
	return jsprovision.ReadWith(ctx, q.provisioning(), read, nil)
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
		// On a character boundary: a byte slice through a multi-byte
		// node id or group name is a consumer name that is not valid
		// UTF-8, which the broker stores mangled.
		readable = textcut.Bytes(readable, max)
	}
	return readable + "__" + id
}

// Fetch implements [statelog.Fetcher]: the next burst of records, up to
// maxMessages of them and, when maxBytes is positive, maxBytes of them,
// waiting up to wait for the first.
//
// Through the consumer's standing pull, whose one request carries both bounds
// and whose one inbox answers every request (package doc, "A pull allocates
// for what it receives, never for what it may"). It returns once a burst is
// complete (the newest delivery reports nothing pending) or a bound is met,
// not when the request ends, which one record does not do. What a call does
// not return stays buffered for the next, as does everything when ctx ends:
// returned beside the error, a stopping caller would drop records the broker
// then holds in flight until the ack window.
func (c *DomainConsumer) Fetch(ctx context.Context, maxMessages, maxBytes int,
	wait time.Duration) ([]statelog.Message, error) {

	if maxMessages <= 0 {
		return nil, nil
	}
	// The standing pull first: it refuses a closed handle before anything is
	// rebuilt for it, and sends nothing.
	p, err := c.standing()
	if err != nil {
		return nil, err
	}
	// Ensured before a request is sent, so a handle a failed reset cleared is
	// rebuilt before a request names a consumer the broker does not hold.
	if _, err = c.consumerFor(ctx); err != nil {
		return nil, err
	}
	msgs, err := p.fetch(ctx, maxMessages, max(maxBytes, 0), wait)
	switch {
	case ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil:
		return nil, fmt.Errorf("jetstream: fetch from %s: %w", c.name, err)
	}
	out := make([]statelog.Message, 0, len(msgs))
	for _, msg := range msgs {
		meta, unreadable := msg.Metadata()
		if unreadable != nil {
			// Dropped, and not acknowledged: its sequence is the
			// applier's checkpoint and contiguity check, so passing a
			// zero would move the cursor backwards, and an ack would move
			// the broker's floor past a record nothing applied.
			continue
		}
		out = append(out, statelog.Message{
			Seq:      meta.Sequence.Stream,
			StoredAt: meta.Timestamp,
			Payload:  msg.Data,
			Ack:      func() error { return msg.Ack() },
			// A nak is redelivered at once; an unacknowledged delivery
			// waits out the ack window.
			Release: func() error { return msg.Nak() },
		})
	}
	return out, nil
}

// standing is the handle's standing pull, made on first use.
func (c *DomainConsumer) standing() (*puller, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, fmt.Errorf("jetstream: %s's handle was closed; open the "+
			"consumer again to read it: %w", c.name, errHandleClosed)
	}
	if c.pull != nil {
		return c.pull, nil
	}
	p, err := c.q.newPuller(c.stream, c.name)
	if err != nil {
		return nil, err
	}
	c.pull = p
	return p, nil
}

// Close gives up this handle for good, leaving the consumer on the broker for
// whoever opens it next (a stopped engine's successor in the same process, on
// the same connection). Call it once nothing will fetch through the handle; a
// later fetch is refused. Idempotent.
//
// What the handle holds is handed back: its buffer, and whatever a request it
// left standing still delivers (pullrequest.go's head). Left, the successor's
// request, served behind the abandoned one, would wait out the ack window.
func (c *DomainConsumer) Close() {
	c.mu.Lock()
	p := c.pull
	c.pull, c.closed = nil, true
	c.mu.Unlock()
	if p != nil {
		p.release()
	}
}

// errHandleClosed is a fetch through a [DomainConsumer] that was closed.
var errHandleClosed = errors.New("jetstream: state-log consumer handle closed")

// Pending implements [statelog.Fetcher]: how many records this consumer has
// not handed to a fetch, which tells a partial batch whether waiting buys
// anything.
//
// Counted from what the pull delivered ([puller.undelivered]: the newest
// delivery's own pending count plus what no fetch has taken), never asked of
// the broker: the applier asks on every committed run, so a consumer info
// request would sit on the commit path, answered only by the consumer's
// leader and dropped silently by a member that lost it. A hint, which can
// trail an append (the server acknowledges one before telling its consumers).
// Before any delivery it returns [errNotCounted]: the applier then commits on
// an unknown count, where a zero would claim it level with the log.
func (c *DomainConsumer) Pending(context.Context) (uint64, error) {
	c.mu.Lock()
	p := c.pull
	c.mu.Unlock()
	if p != nil {
		if n, counted := p.undelivered(); counted {
			return n, nil
		}
	}
	return 0, fmt.Errorf("jetstream: %s's pull has delivered no record whose metadata "+
		"counts what follows it: %w", c.name, errNotCounted)
}

// errNotCounted is a pending count no delivery has made yet.
var errNotCounted = errors.New("jetstream: nothing counted yet")

// Name is what this consumer is called on the broker, for an operator tracing
// a stalled applier back to the thing that is not delivering.
func (c *DomainConsumer) Name() string { return c.name }

// DomainGroup is a FLEET-WIDE pull consumer over one domain's log: one
// consumer shared by the fleet, each record handled by whichever node gets
// there first, as a change feed needs (the opposite of [DomainConsumer]'s
// replication).
//
// A group rather than a duty: a duty's lease flap would stall the whole
// company's notifications for stateless work, and a group has no holder to
// lose.
type DomainGroup struct {
	cons     jetstream.Consumer
	messages jetstream.MessagesContext
	stream   string
	name     string

	// closed ends the context watcher, and once makes a caller's Stop and a
	// cancelled context's the same Stop. The iterator has no context:
	// [jetstream.MessagesContext.Next] blocks until a message or a stop, for
	// ever on a quiet log, so a caller that only cancelled a context would
	// hang on the goroutine it joins (the tracker's wake feed sits in it).
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

	// Delivered is how many times the broker has delivered this record to
	// the group, this delivery included: what a record handed back for a
	// peer backs off by (changefeed.DeferDelay).
	Delivered uint64

	Payload []byte

	// Ack marks the record handled, and Nak returns it for redelivery
	// after a delay. A handler that could not reach something it needs
	// naks; one that decided the record means nothing to it ACKS, because
	// a decision is handling.
	Ack func() error
	Nak func(time.Duration) error
}

// Group opens (or creates) a fleet-wide consumer over this log, and reports
// whether this call created it.
//
// An existing group is opened where it stands, whatever from says: the
// durable consumer is the fleet's position. One created here starts at from,
// the record after the last the caller knows the group acknowledged, or at the
// first retained record when from is zero. Never at the head, which drops
// every wake owed while nothing watched, nor at the first record when the
// caller knows better, which replays the log's whole retention as wakes.
func (l *DomainLog) Group(ctx context.Context, name string, from uint64) (*DomainGroup, bool, error) {
	if name == "" {
		return nil, false, fmt.Errorf("jetstream: a domain group has no name, and it is " +
			"the durable consumer's identity: an unnamed one would be a " +
			"fresh consumer per process that replays the whole log on restart")
	}
	safe := domainGroupName(l.name, name)
	cfg := jetstream.ConsumerConfig{
		Durable:       safe,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       domainConsumerAckWait,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		// No MaxDeliver, for [domainConsumerConfig]'s reason: a spent
		// budget would drop a wake silently.
		MaxDeliver: -1,
	}
	if from > 0 {
		// The broker clamps a start below the first record to it and one
		// past the last to the next publish, so a mark from before a trim
		// starts where it means to.
		cfg.DeliverPolicy, cfg.OptStartSeq = jetstream.DeliverByStartSequencePolicy, from
	}
	// Through [Queue.ensureDurableConsumer], so a group every node ensures
	// at boot survives a peer winning the race, and an existing one is
	// opened as it stands.
	cons, created, err := l.q.ensureDurableConsumer(ctx, l.name, cfg)
	if err != nil {
		return nil, false, fmt.Errorf("jetstream: open the group %s on %s: %w",
			safe, l.name, err)
	}
	messages, err := cons.Messages()
	if err != nil {
		return nil, false, fmt.Errorf("jetstream: consume %s on %s: %w", safe, l.name, err)
	}
	group := &DomainGroup{
		cons: cons, messages: messages, stream: l.name, name: safe,
		closed: make(chan struct{}),
	}
	// ctx becomes a Stop through a goroutine ([DomainGroup.closed] says
	// why), one per group rather than per read: it exits with the group.
	go func() {
		select {
		case <-ctx.Done():
			group.stop()
		case <-group.closed:
		}
	}()
	return group, created, nil
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
		readable = textcut.Bytes(readable, max) // on a character boundary; see domainConsumerName
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
		// A cancelled caller gets the closed-iterator answer: the group's
		// goroutine turns ctx.Done into a Drain, so whichever lands first
		// decides whether Next sees ErrMsgIteratorClosed or a read failing
		// mid-drain, and a clean shutdown must not log the second.
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
		// Not acknowledged: without its place in the log no stable wake id
		// can be derived, and an ack would drop the wake outright.
		return nil, fmt.Errorf("jetstream: read %s's metadata: %w", g.name, err)
	}
	return &DomainDelivery{
		Subject: msg.Subject(), Seq: meta.Sequence.Stream,
		StoredAt: meta.Timestamp, Delivered: meta.NumDelivered, Payload: msg.Data(),
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
// Drain rather than Stop, as [internal/coord/kv]'s feed does: a drain naks
// what it pulled ahead and has not handed over, so a peer gets those records
// at once rather than after the ack window.
func (g *DomainGroup) stop() {
	g.once.Do(func() {
		close(g.closed)
		g.messages.Drain()
	})
}

// Name is what this group is called on the broker.
func (g *DomainGroup) Name() string { return g.name }

// GroupAckFloor is how far a fleet-wide group has acknowledged, WITHOUT
// attaching to it: the trim duty's node need not run the feed, so the answer
// comes from the consumer's name, which [DomainLog.Group] derives from the
// stream and the group.
//
// It reports (0, false, nil) when no such consumer exists: a fleet that never
// opened the group has a feed that has seen nothing, which must not look like
// a term nobody could read. A group name this log never had returns the same,
// for ever, so the caller passes the group its domain declares, having asked
// first whether the domain has a feed at all.
func (l *DomainLog) GroupAckFloor(ctx context.Context, group string) (uint64, bool, error) {
	// The errors name the durable: an operator looks the consumer up on the
	// broker, where the group's own spelling finds nothing.
	durable := domainGroupName(l.name, group)
	// Asked as a read ([jsprovision.ReadBoundedWith]), once: the lookup is the
	// info request itself. Only the consumer's leader answers, and a member
	// that lost one drops it silently, so the trim tick (no deadline of its
	// own) keeps the client's default term and asks again inside it rather
	// than leaving the log's trim blocked for the tick. A handle per request,
	// since requests can be out together.
	info, err := jsprovision.ReadBoundedWith(ctx, l.q.provisioning(), l.js.Options().DefaultTimeout,
		func(ctx context.Context) (*jetstream.ConsumerInfo, error) {
			cons, err := l.js.Consumer(ctx, l.name, durable)
			if err != nil {
				return nil, err
			}
			return cons.CachedInfo(), nil
		}, nil)
	switch {
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("jetstream: read the group %q (consumer %s) on %s: %w",
			group, durable, l.name, err)
	}
	// The ack floor, not the delivered sequence: the trim must keep a record
	// whose wake the feed fetched and has not finished acting on.
	return info.AckFloor.Stream, true, nil
}
