package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chat"
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

// nativeFeed is one domain's wake feed: the translator that decides what a
// committed record means, and how to open a durable consumer over that
// domain's log.
type nativeFeed struct {
	// translator is what a record MEANS, and it is also where the durable
	// consumer's name is declared — see [changefeed.Translator.Source].
	translator changefeed.Translator

	// open builds the consumer over this node's running log.
	open func(running *runningDomain) (changefeed.Opener, error)
}

// nativeFeeds is every domain whose committed records wake somebody, keyed on
// the DOMAIN's own name.
//
// ONE DECLARATION, because two callers need the same answer and each used to
// hold its own. [Engine.startNativeFeeds] opens the consumer; the trim's feed
// term ([feedTermOf]) reads how far that same consumer has acknowledged before
// it lets the log be purged. The term spelled the TRACKER's group into itself,
// so on every other domain's log the lookup found no consumer at all: the term
// read that as a floor of zero, permitted nothing, and CREWLET_PAGES_LOG grew
// toward its ceiling with no error, no alarm and no line in any log. A domain
// listed here inherits both halves at once.
//
// THE GROUP NAME IS NEVER SPELLED HERE. It comes off the translator's own
// [changefeed.Source], which is where each domain declares it exactly once. A
// second list would be a second place for it to drift — and a drifted group is
// not an error but a fresh consumer at the head of the log, with everything
// the first had not yet handled silently abandoned.
//
// skills names the reserved tool-skills container the knowledge base's
// translator quiets; see [pages.NewTranslator]. It is nil wherever only a
// SOURCE is wanted, because a translator's source is its identity rather than
// its configuration.
func nativeFeeds(skills func() string) map[string]nativeFeed {
	return map[string]nativeFeed{
		tracker.Domain{}.Name(): {
			translator: tracker.NewTranslator(),
			open: func(running *runningDomain) (changefeed.Opener, error) {
				// THE LOG IS THE SOURCE, and it is the piece the
				// domain replaced outright: a bucket feed needs a
				// family and a key class, and a log delivery has
				// neither. Its own fleet-wide group over the same
				// stream the applier reads is what derives a wake
				// from a committed record.
				consumer, err := domainFeedFor(running, tracker.Domain{}.Name())
				if err != nil {
					return nil, err
				}
				return tracker.FeedSource{Log: consumer}, nil
			},
		},
		pages.Domain{}.Name(): {
			translator: pages.NewTranslator(skills),
			open: func(running *runningDomain) (changefeed.Opener, error) {
				consumer, err := domainFeedFor(running, pages.Domain{}.Name())
				if err != nil {
					return nil, err
				}
				return pages.FeedSource{Log: consumer}, nil
			},
		},
		chat.Domain{}.Name(): {
			// NO SKILLS ARGUMENT, and the asymmetry is the domain
			// rather than an omission: the reserved container quiets
			// PAGES whose edits are machinery, and chat has no
			// equivalent — every message in a company's own rooms is
			// somebody talking.
			translator: chat.NewTranslator(),
			open: func(running *runningDomain) (changefeed.Opener, error) {
				consumer, err := domainFeedFor(running, chat.Domain{}.Name())
				if err != nil {
					return nil, err
				}
				return chat.FeedSource{Log: consumer}, nil
			},
		},
	}
}

// domainFeedFor is the consumer half of one running domain's log.
//
// GENERIC WHERE THE TWO OPENERS ARE NOT, and the split is deliberate: what is
// shared is the delivery SHAPE — a stream name, a generation and an envelope
// decoder — and what is not is the group name, which each domain declares for
// itself and which this function never touches.
func domainFeedFor(running *runningDomain, domain string) (domainFeed, error) {
	if running == nil {
		return domainFeed{}, fmt.Errorf("engine: the %s domain is not running "+
			"on this node, so nothing derives a wake from a committed record",
			domain)
	}
	return domainFeed{
		log:    running.log,
		stream: running.domain.Stream().Name,
		envel:  running.domain.Envelope,
	}, nil
}

// feedGroup is the durable consumer one domain's wakes ride on, and whether
// that domain has a wake feed at all.
//
// THE REGISTRY IS THE AUTHORITY on both halves, rather than a predicate that
// merely correlates with one. The trim's feed term asked
// [statelog.Domain.ClaimsIdentity], which is true of every domain that has a
// feed today and is still a different question: a domain that claims identity
// and that nobody registered a feed for would be read as having one, and its
// term would block the trim for ever waiting on a consumer that is never
// created.
func feedGroup(domain string) (string, bool) {
	// NIL SKILLS, because a translator's [changefeed.Source] is its
	// identity rather than its configuration: the reserved container
	// quiets pages the feed would otherwise wake somebody about, and
	// changes neither the source name nor the group.
	feed, registered := nativeFeeds(nil)[domain]
	if !registered {
		return "", false
	}
	return feed.translator.Source().Group, true
}
