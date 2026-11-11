package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The log as a CHANGE FEED, which is a second consumer over the same stream.
//
// # Why a wake is derived here and not published by the writer
//
// The rule the change feed exists for is that a wake is derived by something
// that OUTLIVES the writer, never published by the writer's goroutine as a
// courtesy: the chat path already swallows a failed publish and the webhook
// path is safe only because the vendor retries. The durable record IS the
// retry — so the feed is its own fleet-wide consumer, at its own position,
// reading the same log the applier reads.
//
// # And why it is a GROUP rather than a duty
//
// A duty is held by one node under a lease, so a lease flap stalls the whole
// company's notifications for work that is stateless. A group has no holder to
// lose: whichever node gets there first handles the record.

// domainFeed adapts one domain's fleet-wide group to the change feed's own
// seam.
//
// A THIN ADAPTER AND DELIBERATELY SO. What it translates is a delivery's
// SHAPE, and the two shapes differ in exactly one interesting way: a log
// delivery carries a stream and a generation that a bucket delivery has
// neither of, and both must travel — the node that wins a message is rarely
// the node that runs the woken seat, and a wake stamped with a bare position
// is a number the woken node cannot safely compare with its own.
type domainFeed struct {
	log    *jetstream.DomainLog
	stream string
	envel  func(payload []byte) (statelog.Envelope, error)

	// verify opens the framework's signature frame, because THIS IS THE
	// SECOND READER OF A SIGNED LOG and the rule is stated for readers
	// rather than for appliers: the framework verifies before any domain
	// decodes. The applier's own loop does it in [statelog.Runner]; the
	// feed reads the same bytes off the same stream and has to do it too.
	//
	// Without it a domain's decoder is handed the frame — four magic
	// bytes, a version, a key id and a MAC in front of the record — and
	// every record in the company comes back undecodable, which the feed's
	// own rule then acknowledges and skips. Every notification stops, and
	// the only symptom is a log line blaming the record.
	verify *recordseal.Verifier

	// estate and applied are THIS NODE's replicated estate and the domain's
	// own applier, for a translator that derives a wake from what the
	// applier DECIDED about a record rather than from the record alone —
	// chat's, whose applier decides every message at its position
	// ([chatlet.Decisions]). Zero on a feed built where no core runs, which
	// is the register's own check of what each entry declares.
	estate  store.ReplicatedReader
	applied *statelog.Runner

	// marks is the fleet's record of how far each wake feed's group had
	// acknowledged, which a group created again starts after
	// ([coord.FeedMark]). Nil records and reads nothing, and a group
	// created then starts at its log's first retained record.
	marks coord.FeedMarkRegister
}

// One adapter serves every domain that declares a wake feed, so each domain's
// own consumer seam is asserted here rather than left to whichever register
// entry happens to hand it over.
var (
	_ worklet.DomainConsumer = domainFeed{}
	_ doclet.DomainConsumer  = domainFeed{}
	_ chatlet.DomainConsumer = domainFeed{}
	_ code.DomainConsumer    = domainFeed{}
)

// Consume implements each feed-declaring domain's DomainConsumer —
// [worklet.DomainConsumer], [doclet.DomainConsumer], [chatlet.DomainConsumer]
// and [code.DomainConsumer].
//
// A GROUP THIS OPEN CREATES starts after the fleet's recorded floor
// ([feedPosition.start]), and every open records the floor as it advances
// ([feedMarker]): see feedmark.go.
func (f domainFeed) Consume(ctx context.Context, group string) (changefeed.Records, error) {
	at, marked := f.position(group)
	var from uint64
	if marked {
		from = at.start(ctx)
	}
	g, created, err := f.log.Group(ctx, group, from)
	if err != nil {
		return nil, err
	}
	if created {
		log.InfoContext(ctx, "changefeed_group_created", "stream", f.stream, "group", group,
			"from", from, "detail", "the group did not exist and was created; it starts after "+
				"the fleet's recorded floor, or at the log's first retained record (from 0) "+
				"where none is recorded")
	}
	records := &domainRecords{group: g, stream: f.stream, envel: f.envel, verify: f.verify}
	if marked {
		records.marker = startFeedMarker(ctx, at)
	}
	return records, nil
}

// domainRecords is one open group, as the change feed reads it.
type domainRecords struct {
	group *jetstream.DomainGroup

	// stream is the LOG's name, not the consumer's. A wake carries the
	// stream a position is on, because a position alone is comparable only
	// against the same log — and the node that wins a message is rarely
	// the node that runs the woken seat.
	stream string

	envel  func(payload []byte) (statelog.Envelope, error)
	verify *recordseal.Verifier

	// marker records the group's floor while it is open. Nil where no
	// register is wired.
	marker *feedMarker
}

// Next blocks for the next record.
//
// THE FRAME IS OPENED FIRST, before the envelope and before anything
// downstream is handed a byte of it — the framework's rule for every reader of
// a signed log, and the feed is the second one. What travels on
// [changefeed.Record.Payload] is therefore the BODY: the wake's own
// translators decode it again ([worklet.DecodeEnvelope], [doclet.Decode]), so a
// framed payload here would fail in a package that never heard of a frame. The
// frame itself travels beside it, untouched ([changefeed.Message.Frame]), for
// the one reader that needs it: a dead letter, which keeps the record whole.
//
// THE OPERATION ID IS THE DELIVERY'S IDENTITY, taken from the envelope every
// build can read. It is what makes a redelivery collapse: a translator with no
// id of its own uses it, and the wake id derived from it is the same on every
// redelivery and on every node.
//
// A record this node cannot read meets one of three ends here, and only one
// is an acknowledgement:
//
//   - A FRAME SEALED UNDER A KEY THIS NODE DOES NOT HOLD, and AN ENVELOPE A
//     NEWER BUILD WRITES, are DEFERRED to a peer ([changefeed.Message.Deferred]):
//     handed to the feed with no payload, which returns them for redelivery
//     with a backoff and dead-letters them past [changefeed.DeadLetterAge].
//     "The bytes will not improve" is true on THIS node and false on a peer
//     that holds the key or runs the newer build — and every node pulls the
//     feed from ONE durable consumer, so a record acked here was one no peer
//     would ever see: its wake was gone fleet-wide (#838).
//   - A TAMPERED FRAME — a MAC that fails under a key this node holds, or
//     bytes that are not a frame at all — is ACKNOWLEDGED and skipped: no node
//     should act on a frame this fleet did not write, and the applier is where
//     a forged record stops the fleet.
//
// Either way the APPLIER retains the record at its position, where a later
// build or keyring reprocesses it; what is decided here is only the wake.
//
// NONE OF THE THREE DERIVES A WAKE ON THIS NODE, which is the half that
// matters: a notification derived from bytes this node could not authenticate
// is the engine acting on an instruction it cannot attribute.
func (r *domainRecords) Next(ctx context.Context) (*changefeed.Message, error) {
	for {
		delivery, err := r.group.Next(ctx)
		if err != nil || delivery == nil {
			return nil, err
		}
		msg, err := r.read(delivery.Payload, placeOf(r.stream, delivery))
		if err != nil {
			// TAMPERED, and louder than a deferral: a MAC that fails under
			// a key this node holds is something that is not this fleet
			// writing to its log.
			log.ErrorContext(ctx, "changefeed_record_unverified",
				"stream", r.stream, "consumer", r.group.Name(), "seq", delivery.Seq,
				"error", err.Error(), "holds", r.verify.KeyIDs(),
				"detail", "acknowledged and skipped, and NO wake is derived from "+
					"it: the applier is what stops the node on a record this "+
					"fleet did not write")
			if err := delivery.Ack(); err != nil {
				return nil, err
			}
			continue
		}
		msg.Ack, msg.Nak = delivery.Ack, delivery.Nak
		msg.Progress, msg.ProgressEvery = delivery.Progress, delivery.ProgressEvery
		return msg, nil
	}
}

// Reopen implements [changefeed.Records]: a dead letter's frame read exactly as
// [domainRecords.Next] reads a delivery — through this node's verifier, which
// is built once at boot and never changes, so a key that arrived with a
// restart is the key this reads it with — at the place the dead letter kept.
func (r *domainRecords) Reopen(frame []byte, at changefeed.Record) (*changefeed.Message, error) {
	return r.read(frame, at)
}

// read is one frame as the feed reads it, at the place at names: the frame
// verified ([openRecord]), the envelope read, and the record handed over — or
// DEFERRED where this node cannot read it and a peer can, with no payload (the
// envelope's op id and generation unread, and no byte of an unopened frame
// travelling on), or [changefeed.ErrForeignFrame] for a frame this fleet did
// not write. The frame rides on every message, a deferred one included.
func (r *domainRecords) read(frame []byte, at changefeed.Record) (*changefeed.Message, error) {
	env, body, verdict, err := openRecord(r.verify, r.envel, frame)
	switch {
	case verdict == recordseal.KeyUnknown:
		// AN OPERATOR'S PENDING CHANGE — a key the rotation runbook adds on
		// every node before anything is sealed under it, and this node has
		// not restarted onto yet — which a peer that holds the key reads.
		return deferredAt(frame, at, changefeed.DeferUnknownKey,
			fmt.Errorf("the frame is sealed under a key this node does not hold (it holds %s)",
				words.And(r.verify.KeyIDs()))), nil
	case verdict != recordseal.Verified:
		return nil, fmt.Errorf("%w: the frame is %s under this node's keyring",
			changefeed.ErrForeignFrame, verdict)
	case err != nil:
		// The frame opened and the ENVELOPE did not: a record from a build
		// that writes a shape this one cannot read, which a peer on that
		// build reads.
		return deferredAt(frame, at, changefeed.DeferNewerEnvelope, err), nil
	}
	return &changefeed.Message{Record: feedRecord(at, env, body), Frame: frame}, nil
}

// deferredAt is a frame this node cannot open and a peer can, handed to the
// feed to defer: its place in the log and the broker's instant and count, and
// NO payload.
func deferredAt(frame []byte, at changefeed.Record, reason changefeed.DeferReason,
	err error) *changefeed.Message {

	return &changefeed.Message{
		Record:   at,
		Frame:    frame,
		Deferred: &changefeed.Deferral{Reason: reason, Err: err},
	}
}

// Stop ends this process's consumption; the durable position survives, and
// the fleet's mark is raised to it once more.
func (r *domainRecords) Stop() error {
	err := r.group.Stop()
	r.marker.close()
	return err
}

// placeOf is where a delivery was delivered from, as a record carries it: its
// place on stream, its subject, the broker's instant for it — what the wake it
// becomes is timed from — and how many times the broker has delivered it, which
// a deferral backs off by. What Next knows of a delivery before its frame is
// read, and what a dead letter keeps of it.
func placeOf(stream string, delivery *jetstream.DomainDelivery) changefeed.Record {
	return changefeed.Record{
		Position:  delivery.Seq,
		Stream:    stream,
		Key:       delivery.Subject,
		StoredAt:  delivery.StoredAt,
		Delivered: delivery.Delivered,
	}
}

// feedRecord is the record at at whose frame opened to env and body: the
// envelope's operation id and generation, and the opened body.
func feedRecord(at changefeed.Record, env statelog.Envelope, body []byte) changefeed.Record {
	at.ID, at.Gen, at.Payload = env.OpID, uint64(env.Gen), body
	return at
}

// feedFor is the wake feed one running domain declares: the translator that
// reads its records and the opener over its own log, or a nil translator for a
// domain that declares none.
//
// # Off the register, and held to the declaration
//
// THE REGISTER'S ENTRY BUILDS IT ([registration.Feed]), the one place a
// domain is declared, rather than a switch here: a translator is not part of
// the declaration — the knowledge base's reads Tier B on every change — and a
// switch that did not know a domain answered "no feed" for it indistinguishably
// from one that meant it. What the declaration DOES own is the group
// ([statelog.Domain.FeedGroup]), which the trim waits on from a node that
// builds no feed at all, so the feed is held to it ([checkFeed]) — here, where
// it starts, and before that at boot ([checkFeedEntry]), so a domain whose feed
// disagrees with its declaration fails naming itself rather than silently.
func (e *Engine) feedFor(running *runningLog) (changefeed.Translator, changefeed.Opener, error) {
	var (
		translator changefeed.Translator
		opener     changefeed.Opener
	)
	if entry, found := registrationFor(running.domain.Name()); found && entry.Feed != nil {
		// THE LOG IS THE SOURCE: the domain's own fleet-wide group over
		// the same stream its applier reads, which is what derives a wake
		// from a committed record rather than the writer's goroutine.
		translator, opener = entry.Feed(e, domainFeed{
			log:    running.log,
			stream: running.spec.Name,
			envel:  running.domain.Envelope,
			// THROUGH THE DOMAIN'S OWN VERIFIER: the feed is the log's
			// second reader, and a domain decoder handed the frame reads
			// every record in the company as undecodable. See
			// [domainFeed.verify].
			verify:  running.verifier,
			estate:  e.feedEstate(),
			applied: running.runner,
			marks:   e.feedMarks(),
		})
	}
	if err := checkFeed(running.domain, translator); err != nil {
		return nil, nil, err
	}
	if translator == nil {
		return nil, nil, nil
	}
	return translator, opener, nil
}

// feedMarks is the fleet's register of wake feed marks, or nil where no fleet
// store is wired.
func (e *Engine) feedMarks() coord.FeedMarkRegister {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return e.backends.Fleet
}

// feedEstate is this node's replicated estate as a wake feed reads it, or the
// zero reader where no core runs.
func (e *Engine) feedEstate() store.ReplicatedReader {
	if c := e.core.Load(); c != nil && c.log != nil {
		return c.log.estate().Reader()
	}
	return store.ReplicatedReader{}
}

// checkFeed refuses a wake feed that disagrees with its domain's declaration,
// naming the domain. Each disagreement is silent anywhere else:
//
//   - declared and never built: the trim waits on a consumer no node ever
//     opens, and that log grows until its ceiling refuses appends;
//   - built and not declared: the trim never waits for the feed, and purges
//     records nobody has been woken for yet;
//   - built under another group: the trim waits on a consumer the wakes never
//     advance — which is where the knowledge base's log stood while the trim
//     read the tracker's group on it.
func checkFeed(domain statelog.Domain, translator changefeed.Translator) error {
	declared := domain.FeedGroup()
	switch {
	case translator == nil && declared == "":
		return nil
	case translator == nil:
		return fmt.Errorf("engine: domain %q declares the wake feed %q "+
			"and this build runs none over its log, so its trim would wait for "+
			"ever on a consumer nobody opens", domain.Name(), declared)
	case declared == "":
		return fmt.Errorf("engine: domain %q declares no wake feed and "+
			"this build runs one as %q, so its trim would purge records that "+
			"feed has not woken anybody for", domain.Name(),
			translator.Source().Group)
	case translator.Source().Group != declared:
		return fmt.Errorf("engine: domain %q declares the wake feed %q "+
			"and its feed runs as %q — the trim would wait on one consumer while "+
			"the wakes advance the other", domain.Name(), declared,
			translator.Source().Group)
	}
	return nil
}

// openRecord is the feed's whole reading of one delivery's bytes: the
// framework's frame first, the domain's envelope second, and the BODY between
// them.
//
// IT IS A FUNCTION RATHER THAN THREE LINES INSIDE THE LOOP because the order
// is the invariant and the loop is unreachable without a broker. A test can
// hand this a signed record, a record signed under a key it does not hold and
// a record that is not this fleet's, and pin that a domain decoder is handed
// the body on the first and nothing at all on the other two — which is what a
// reader that skipped [recordseal.Verifier.Open] would get wrong, silently, by
// handing the frame's magic bytes to a JSON decoder.
//
// The verdict is the caller's disposition and the error is the ENVELOPE's, so
// the two are separate results: a [recordseal.Verified] record with an error is
// one this build cannot decode, and an unverified one is never decoded at all,
// so it can never carry one.
func openRecord(verify *recordseal.Verifier, envel func([]byte) (statelog.Envelope, error),
	payload []byte) (statelog.Envelope, []byte, recordseal.Verdict, error) {

	body, verdict := verify.Open(payload)
	if verdict != recordseal.Verified {
		// NO BODY TRAVELS OUT OF AN UNVERIFIED RECORD even where the
		// frame kept one: an unknown key leaves the bytes readable so
		// the APPLIER can file the record under its own scope, and the
		// feed has nowhere to file anything — it would only be handing
		// a decoder bytes this node cannot attribute.
		return statelog.Envelope{}, nil, verdict, nil
	}
	env, err := envel(body)
	if err != nil {
		return statelog.Envelope{}, nil, verdict, err
	}
	return env, body, verdict, nil
}
