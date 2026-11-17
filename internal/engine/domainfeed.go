package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The log as a CHANGE FEED: a second consumer over the same stream.
//
// A wake is derived by something that OUTLIVES the writer, never published by
// the writer's goroutine, since the durable record IS the retry: the feed is
// its own fleet-wide consumer at its own position on the log the applier reads.
//
// A GROUP rather than a duty: a duty's lease flap would stall the whole
// company's notifications for stateless work, and a group has no holder to
// lose; whichever node gets there first handles the record.

// domainFeed adapts one domain's fleet-wide group to the change feed's own
// seam.
//
// A thin adapter of a delivery's SHAPE: a log delivery's stream and
// generation must travel with it, because the node that wins a message is
// rarely the one running the woken seat, which cannot compare a bare position
// with its own.
type domainFeed struct {
	log    *jetstream.DomainLog
	stream string
	envel  func(payload []byte) (statelog.Envelope, error)

	// verify opens the signature frame before any domain decodes, as
	// [statelog.Runner] does for the applier: the feed is the log's SECOND
	// READER. Without it every decoder is handed the frame's magic bytes,
	// every record comes back undecodable and is skipped, and every
	// notification stops with only a log line blaming the record.
	verify *recordseal.Verifier

	// estate and applied are THIS NODE's replicated estate and the domain's
	// applier, for a translator that wakes on what the applier DECIDED about
	// a record (chat's, [chatlet.Decisions]). Zero where no core runs, which
	// the register checks against what each entry declares.
	estate  store.ReplicatedReader
	applied *statelog.Runner

	// marks is how far each wake feed's group had acknowledged, which a
	// group created again starts after ([coord.FeedMark]). Nil reads and
	// records nothing: a new group starts at the first retained record.
	marks coord.FeedMarkRegister
}

// One adapter serves every feed-declaring domain, so each one's consumer seam
// is asserted here rather than left to whichever register entry hands it over.
var (
	_ worklet.DomainConsumer         = domainFeed{}
	_ doclet.DomainConsumer          = domainFeed{}
	_ chatlet.DomainConsumer         = domainFeed{}
	_ gitlet.DomainConsumer          = domainFeed{}
	_ artifactsdomain.DomainConsumer = domainFeed{}
)

// Consume implements each feed-declaring domain's DomainConsumer —
// [worklet.DomainConsumer], [doclet.DomainConsumer], [chatlet.DomainConsumer],
// [gitlet.DomainConsumer] and [artifactsdomain.DomainConsumer].
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

	// stream is the LOG's name, not the consumer's: a position is
	// comparable only against the same log (see [domainFeed]).
	stream string

	envel  func(payload []byte) (statelog.Envelope, error)
	verify *recordseal.Verifier

	// marker records the group's floor while it is open. Nil where no
	// register is wired.
	marker *feedMarker
}

// Next blocks for the next record.
//
// The payload is the BODY ([domainRecords.read]), and the envelope's operation
// id the delivery's identity, so a redelivery derives the same wake id on
// every node. A record under a key this node lacks, or in a newer build's
// envelope, is DEFERRED rather than acked (redelivered with backoff,
// dead-lettered past [changefeed.DeadLetterAge]), since every node pulls ONE
// durable consumer and an ack here hides it from the peer that can read it. A
// tampered one is ACKED and skipped: the applier stops the fleet on a forged
// record. The applier keeps the record either way; only the wake is decided
// here, and none derives on an instruction this node cannot attribute.
func (r *domainRecords) Next(ctx context.Context) (*changefeed.Message, error) {
	for {
		delivery, err := r.group.Next(ctx)
		if err != nil || delivery == nil {
			return nil, err
		}
		msg, err := r.read(delivery.Payload, placeOf(r.stream, delivery))
		if err != nil {
			// TAMPERED, and louder than a deferral: something other than
			// this fleet wrote to its log.
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
		return msg, nil
	}
}

// Reopen implements [changefeed.Records]: a dead letter's frame read exactly as
// [domainRecords.Next] reads a delivery, at the place the dead letter kept,
// through this node's boot-time verifier (a key arrives only with a restart).
func (r *domainRecords) Reopen(frame []byte, at changefeed.Record) (*changefeed.Message, error) {
	return r.read(frame, at)
}

// read is one frame as the feed reads it, at the place at names: opened first
// ([openRecord]), so [changefeed.Record.Payload] carries the BODY the
// translators decode again in packages that know no frame. The record is
// handed over, DEFERRED with no payload ([changefeed.Message.Deferred]) where
// a peer can read it and this node cannot, or [changefeed.ErrForeignFrame]
// for a frame this fleet did not write. The frame rides on every message
// ([changefeed.Message.Frame]), for a dead letter.
func (r *domainRecords) read(frame []byte, at changefeed.Record) (*changefeed.Message, error) {
	env, body, verdict, err := openRecord(r.verify, r.envel, frame)
	switch {
	case verdict == recordseal.KeyUnknown:
		// A rotation in progress: a key added on every node before anything
		// is sealed under it, which this node has not restarted onto yet.
		return deferredAt(frame, at, changefeed.DeferUnknownKey,
			fmt.Errorf("the frame is sealed under a key this node does not hold (it holds %s)",
				words.And(r.verify.KeyIDs()))), nil
	case verdict != recordseal.Verified:
		return nil, fmt.Errorf("%w: the frame is %s under this node's keyring",
			changefeed.ErrForeignFrame, verdict)
	case err != nil:
		// The ENVELOPE did not open: a newer build's shape, which a peer on
		// that build reads.
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

// placeOf is a delivery's place, as a record carries it before its frame is
// read and as a dead letter keeps it: its position on stream, its subject, the
// broker's instant (what the wake is timed from) and its delivery count (what a
// deferral backs off by).
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
// THE REGISTER'S ENTRY BUILDS IT ([registration.Feed]), not a switch here,
// which would answer "no feed" for a domain it did not know as if meant. The
// declaration owns the group ([statelog.Domain.FeedGroup]) the trim waits on,
// so the feed is held to it ([checkFeed]) here and at boot ([checkFeedEntry]).
func (e *Engine) feedFor(running *runningLog) (changefeed.Translator, changefeed.Opener, error) {
	var (
		translator changefeed.Translator
		opener     changefeed.Opener
	)
	if entry, found := registrationFor(running.domain.Name()); found && entry.Feed != nil {
		translator, opener = entry.Feed(e, domainFeed{
			log:    running.log,
			stream: running.spec.Name,
			envel:  running.domain.Envelope,
			// The domain's own verifier: see [domainFeed.verify].
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
//     advance.
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
// A function rather than lines inside the loop, which no test reaches without
// a broker, so a test can pin the order: a decoder gets the body of a verified
// record and nothing at all otherwise.
//
// The verdict and the ENVELOPE's error are separate results: a
// [recordseal.Verified] record with an error is one this build cannot decode,
// and an unverified one is never decoded, so never carries one.
func openRecord(verify *recordseal.Verifier, envel func([]byte) (statelog.Envelope, error),
	payload []byte) (statelog.Envelope, []byte, recordseal.Verdict, error) {

	body, verdict := verify.Open(payload)
	if verdict != recordseal.Verified {
		// NO BODY TRAVELS OUT OF AN UNVERIFIED RECORD, even where an
		// unknown key left it readable for the APPLIER to file: the feed
		// files nothing, and would only hand a decoder unattributable bytes.
		return statelog.Envelope{}, nil, verdict, nil
	}
	env, err := envel(body)
	if err != nil {
		return statelog.Envelope{}, nil, verdict, err
	}
	return env, body, verdict, nil
}
