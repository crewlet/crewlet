package engine

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
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
	verify *statelog.Verifier
}

// Consume implements [tracker.DomainConsumer] and [pages.DomainConsumer].
func (f domainFeed) Consume(ctx context.Context, group string) (changefeed.Records, error) {
	g, err := f.log.Group(ctx, group)
	if err != nil {
		return nil, err
	}
	return &domainRecords{group: g, stream: f.stream, envel: f.envel, verify: f.verify}, nil
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
	verify *statelog.Verifier
}

// Next blocks for the next record.
//
// THE FRAME IS OPENED FIRST, before the envelope and before anything
// downstream is handed a byte of it — the framework's rule for every reader of
// a signed log, and the feed is the second one. What travels on
// [changefeed.Record.Payload] is therefore the BODY: the wake's own
// translators decode it again ([tracker.DecodeEnvelope], [pages.Decode]), so a
// framed payload here would fail in a package that never heard of a frame.
//
// THE OPERATION ID IS THE DELIVERY'S IDENTITY, taken from the envelope every
// build can read. It is what makes a redelivery collapse: a translator with no
// id of its own uses it, and the wake id derived from it is the same on every
// redelivery and on every node.
//
// A record this node cannot read is ACKNOWLEDGED and skipped rather than
// retried for ever, and all three reasons meet here: a frame whose key this
// node does not hold, a frame that is not this fleet's at all, and an envelope
// from a build that writes a version this one does not. None is transient —
// the bytes will not improve — and the framework's own contract is that a
// record no build can read is retained by the APPLIER, at its position, where
// a later build or a later keyring reprocesses it. Blocking the wake feed on
// it would stop every notification in the company behind one record.
//
// NONE OF THE THREE DERIVES A WAKE, which is the half that matters: a
// notification derived from bytes this node could not authenticate is the
// engine acting on an instruction it cannot attribute, and the applier is
// where a forged record stops the fleet.
func (r *domainRecords) Next(ctx context.Context) (*changefeed.Message, error) {
	for {
		delivery, err := r.group.Next(ctx)
		if err != nil || delivery == nil {
			return nil, err
		}
		env, body, verdict, err := openRecord(r.verify, r.envel, delivery.Payload)
		switch {
		case verdict != statelog.Verified:
			// TAMPERED IS LOUDER THAN UNKNOWN because the two are
			// different events: a key this node has not been given is
			// an operator's pending change, and a MAC that fails under
			// a key it does hold is something that is not this fleet
			// writing to its log.
			level := slog.LevelWarn
			detail := "acknowledged and skipped: the applier retains the record " +
				"at its own position, and reprocesses it when this node is given " +
				"the key it names"
			if verdict == statelog.Tampered {
				level = slog.LevelError
				detail = "acknowledged and skipped, and NO wake is derived from " +
					"it: the applier is what stops the node on a record this " +
					"fleet did not write"
			}
			log.Log(ctx, level, "changefeed_record_unverified",
				"stream", r.stream, "consumer", r.group.Name(), "seq", delivery.Seq,
				"verdict", string(verdict), "holds", r.verify.KeyIDs(),
				"detail", detail)
		case err != nil:
			// The frame opened and the ENVELOPE did not: a record from
			// a build that writes a shape this one cannot read.
			log.WarnContext(ctx, "changefeed_record_undecodable",
				"stream", r.stream, "consumer", r.group.Name(), "seq", delivery.Seq,
				"error", err.Error(),
				"detail", "acknowledged and skipped: the bytes will not improve, "+
					"and the applier retains the record at its own position for "+
					"a build that can read it")
		default:
			return &changefeed.Message{
				Record: changefeed.Record{
					ID:       env.OpID,
					Position: delivery.Seq,
					Stream:   r.stream,
					Gen:      uint64(env.Gen),
					Key:      delivery.Subject,
					Payload:  body,
				},
				Ack: delivery.Ack,
				Nak: delivery.Nak,
			}, nil
		}
		if err := delivery.Ack(); err != nil {
			return nil, err
		}
	}
}

// Stop ends this process's consumption; the durable position survives.
func (r *domainRecords) Stop() error { return r.group.Stop() }

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
func (e *Engine) feedFor(running *runningDomain) (changefeed.Translator, changefeed.Opener, error) {
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
			stream: running.domain.Stream().Name,
			envel:  running.domain.Envelope,
			// THROUGH THE DOMAIN'S OWN VERIFIER: the feed is the log's
			// second reader, and a domain decoder handed the frame reads
			// every record in the company as undecodable. See
			// [domainFeed.verify].
			verify: running.verifier,
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
// reader that skipped [statelog.Verifier.Open] would get wrong, silently, by
// handing the frame's magic bytes to a JSON decoder.
//
// The verdict is the caller's disposition and the error is the ENVELOPE's, so
// the two are separate results: a [statelog.Verified] record with an error is
// one this build cannot decode, and an unverified one is never decoded at all,
// so it can never carry one.
func openRecord(verify *statelog.Verifier, envel func([]byte) (statelog.Envelope, error),
	payload []byte) (statelog.Envelope, []byte, statelog.Verdict, error) {

	body, verdict := verify.Open(payload)
	if verdict != statelog.Verified {
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
