package changefeed

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
)

// DocumentSource opens a coordination family's change feed as records.
//
// # Why the adapter is here and not in coord
//
// This package declares [Opener] because it is the consumer, and the estate
// that satisfies it must not have to know that. A coordination backend serves
// documents and feeds; what a wake needs from one is this package's business,
// and there is more than one backend — the KV estate and its in-memory twin —
// so an adapter written inside either would be a second implementation of a
// mapping the two must agree on exactly.
//
// # What a bucket cannot fill in
//
// [Record.Stream] and [Record.Gen] stay empty and zero: a bucket has neither,
// and inventing values would make a wake stamped from one comparable with a
// wake stamped from a log, which is the mistake those fields exist to prevent.
//
// The class must be a CREATE-ONLY key class. A bucket keeps one revision per
// key, so rewriting a key terminates any un-acked message already delivered
// for it — no redelivery, no error, and nothing anywhere saying a wake was
// lost. See [coord.Feed].
func DocumentSource(feeder coord.Feeder, family coord.Family, class string) Opener {
	return documents{feeder: feeder, family: family, class: class}
}

type documents struct {
	feeder coord.Feeder
	family coord.Family
	class  string
}

func (d documents) Open(ctx context.Context, group string) (Records, error) {
	if d.feeder == nil {
		return nil, fmt.Errorf("changefeed: the %s family has no feeder", d.family)
	}
	feed, err := d.feeder.FeedDocuments(ctx, d.family, d.class, group)
	if err != nil {
		return nil, err
	}
	return documentRecords{feed: feed}, nil
}

type documentRecords struct{ feed coord.Feed }

func (r documentRecords) Next(ctx context.Context) (*Message, error) {
	delivery, err := r.feed.Next(ctx)
	if err != nil || delivery == nil {
		return nil, err
	}
	return &Message{
		Record: Record{
			// THE KEY IS THE IDENTITY. A change key is create-only, so it
			// names exactly one record for the life of the family — and
			// the id inside the value is the domain's to read, not this
			// adapter's.
			ID:       delivery.Key,
			Position: delivery.Revision,
			Key:      delivery.Key,
			Payload:  delivery.Value,
			Removed:  delivery.Op == coord.OpPurge,
		},
		Ack: delivery.Ack,
		Nak: delivery.Nak,
	}, nil
}

func (r documentRecords) Stop() error { return r.feed.Stop() }
