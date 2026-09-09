package work

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/coord"
)

// Source is the notification source name the native tracker registers under,
// and the party registry's namespace for a handle on it.
//
// BARE, not "crewlet-work" or a prefixed spelling: it appears in every log
// line, every event's source column and every dashboard filter, and a
// first-party source has nothing to disambiguate itself from.
const Source = "work"

// Translator turns a change record into the delivery the parser reads.
//
// It implements [changefeed.Translator]. What it contributes is the decision
// nothing above it can make: whether a change should wake anybody at all.
type Translator struct{}

// NewTranslator builds the tracker's feed translator.
func NewTranslator() *Translator { return &Translator{} }

// Source is the estate this translator serves: the notification source name
// parsers register under, and the tracker family's durable consumer.
//
// THE GROUP NAME COMES FROM [changefeed.Group], which is what keeps it
// byte-identical to the one the fleet is already positioned on — the name IS
// the position, and a rename starts a second consumer at the head.
func (t *Translator) Source() changefeed.Source {
	return changefeed.Source{Name: Source, Group: changefeed.Group(coord.FamilyWork)}
}

// Translate decides whether a change wakes anybody, and hands the parser the
// whole record.
//
// THE RECORD TRAVELS IN THE BODY. The node that wins a feed message is rarely
// the one running the recipient and is often behind on its projection, so
// routing from a local read would either use a stale head or block the feed
// until it caught up. The change carries its own routing snapshot precisely
// so neither is necessary.
func (t *Translator) Translate(ctx context.Context, rec changefeed.Record) (changefeed.Delivery, bool, error) {
	record, err := DecodeChange(rec.Payload)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf(
			"work: read the change on %s: %w", rec.Key, err)
	}
	if record.Quiet {
		// AN IMPORT. The flag is on the RECORD rather than a parameter to
		// the feed, so a redelivery months later still knows not to wake
		// anybody — which a runtime flag could not.
		log.DebugContext(ctx, "work_change_quiet", "change", record.ID,
			"item", record.Snapshot.Key)
		return changefeed.Delivery{}, false, nil
	}

	body, err := changeBody(record)
	if err != nil {
		return changefeed.Delivery{}, false, err
	}
	return changefeed.Delivery{
		Body:  body,
		ID:    record.ID,
		Actor: record.Actor,
	}, true, nil
}

// changeBody renders a change as the map a parser reads.
//
// THROUGH JSON rather than field by field, so the body is exactly the record
// — including the fields this build carries but does not understand. A parser
// on a newer node reading a change an older node relayed sees everything the
// writer wrote.
func changeBody(record Change) (map[string]any, error) {
	data, err := EncodeChange(record)
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("work: render the change body: %w", err)
	}
	return body, nil
}
