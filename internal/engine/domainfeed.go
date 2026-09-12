package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
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
}

// Consume implements [tracker.DomainConsumer].
func (f domainFeed) Consume(ctx context.Context, group string) (changefeed.Records, error) {
	g, err := f.log.Group(ctx, group)
	if err != nil {
		return nil, err
	}
	return &domainRecords{group: g, stream: f.stream, envel: f.envel}, nil
}

// domainRecords is one open group, as the change feed reads it.
type domainRecords struct {
	group *jetstream.DomainGroup

	// stream is the LOG's name, not the consumer's. A wake carries the
	// stream a position is on, because a position alone is comparable only
	// against the same log — and the node that wins a message is rarely
	// the node that runs the woken seat.
	stream string

	envel func(payload []byte) (statelog.Envelope, error)
}

// Next blocks for the next record.
//
// THE OPERATION ID IS THE DELIVERY'S IDENTITY, taken from the envelope every
// build can read. It is what makes a redelivery collapse: a translator with no
// id of its own uses it, and the wake id derived from it is the same on every
// redelivery and on every node.
//
// A record whose envelope will not decode is ACKNOWLEDGED and skipped rather
// than retried for ever. It is not a transient failure — the bytes will not
// improve — and the framework's own contract is that a record no build can
// read is retained by the APPLIER, at its position, where a later build
// reprocesses it. Blocking the wake feed on it would stop every notification
// in the company behind one malformed record.
func (r *domainRecords) Next(ctx context.Context) (*changefeed.Message, error) {
	for {
		delivery, err := r.group.Next(ctx)
		if err != nil || delivery == nil {
			return nil, err
		}
		env, err := r.envel(delivery.Payload)
		if err != nil {
			log.WarnContext(ctx, "changefeed_record_undecodable",
				"stream", r.stream, "consumer", r.group.Name(), "seq", delivery.Seq,
				"error", err.Error(),
				"detail", "acknowledged and skipped: the bytes will not improve, "+
					"and the applier retains the record at its own position for "+
					"a build that can read it")
			if err := delivery.Ack(); err != nil {
				return nil, err
			}
			continue
		}
		return &changefeed.Message{
			Record: changefeed.Record{
				ID:       env.OpID,
				Position: delivery.Seq,
				Stream:   r.stream,
				Gen:      uint64(env.Gen),
				Key:      delivery.Subject,
				Payload:  delivery.Payload,
			},
			Ack: delivery.Ack,
			Nak: delivery.Nak,
		}, nil
	}
}

// Stop ends this process's consumption; the durable position survives.
func (r *domainRecords) Stop() error { return r.group.Stop() }

// trackerFeedSource is the tracker's own change feed over its log.
func trackerFeedSource(running *runningDomain) (tracker.FeedSource, error) {
	if running == nil {
		return tracker.FeedSource{}, fmt.Errorf("engine: the tracker domain is " +
			"not running on this node, so nothing derives a wake from a " +
			"committed record")
	}
	return tracker.FeedSource{Log: domainFeed{
		log:    running.log,
		stream: running.domain.Stream().Name,
		envel:  running.domain.Envelope,
	}}, nil
}

// pagesFeedSource is the knowledge base's own consumer over its log.
//
// The same shape [trackerFeedSource] has, and separate rather than generic
// because each domain declares its own group name — which IS the fleet's
// position, so a helper that derived one would be a rename waiting to happen.
func pagesFeedSource(running *runningDomain) (pages.FeedSource, error) {
	if running == nil {
		return pages.FeedSource{}, fmt.Errorf("engine: the pages domain is not " +
			"running on this node, so nothing derives a wake from a committed " +
			"record")
	}
	return pages.FeedSource{Log: domainFeed{
		log:    running.log,
		stream: running.domain.Stream().Name,
		envel:  running.domain.Envelope,
	}}, nil
}
