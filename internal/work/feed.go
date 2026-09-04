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

// Family is the family this translator serves.
func (t *Translator) Family() coord.Family { return coord.FamilyWork }

// Class is the key class the feed filters on.
//
// THE CHANGE CLASS, never the head. A bucket keeps one revision per key, so
// rewriting a key terminates any un-acked message already delivered for it —
// no redelivery, no error, nothing anywhere saying a wake was lost. Change
// keys are create-only for exactly this reason.
func (t *Translator) Class() string { return ClassChange }

// Source is the notification source name.
func (t *Translator) Source() string { return Source }

// Translate decides whether a change wakes anybody, and hands the parser the
// whole record.
//
// THE RECORD TRAVELS IN THE BODY. The node that wins a feed message is rarely
// the one running the recipient and is often behind on its projection, so
// routing from a local read would either use a stale head or block the feed
// until it caught up. The change carries its own routing snapshot precisely
// so neither is necessary.
func (t *Translator) Translate(ctx context.Context, change coord.Change) (changefeed.Delivery, bool, error) {
	if change.Op == coord.OpPurge {
		// A change key was swept or removed. Nothing to tell anybody: the
		// wake it once produced was delivered a year ago.
		return changefeed.Delivery{}, false, nil
	}
	record, err := DecodeChange(change.Value)
	if err != nil {
		return changefeed.Delivery{}, false, fmt.Errorf(
			"work: read the change on %s: %w", change.Key, err)
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
