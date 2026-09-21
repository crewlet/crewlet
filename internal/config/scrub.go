package config

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/redact"
)

// ERASING PERSONAL DATA FROM A STORED REVISION.
//
// # What this is for
//
// A revision is an immutable snapshot of the whole Tier B document, and every
// node that ever met one keeps its own copy in an append-only table — which
// is in every backup of every node, for the life of the deployment. For a
// configuration that is exactly right: the history is what makes a revert
// possible and an audit row meaningful.
//
// For a PERSON it is not. A human seat carries `email` and a `contact` block
// of external account ids, so a company that has ever named somebody in its
// chart has archived their personal data in a table nothing deletes from, on
// every machine, for ever. Removing the seat does not reach it, because the
// removal writes a NEW revision and every older one still holds them.
//
// Revisions written after the org chart moved onto its own log carry no chart
// at all, so nothing new enters the archive. What is already there is what
// this reaches, once, when an operator runs `crewlet config scrub`.
//
// # Why it rewrites JSON rather than the Go type
//
// A revision may have been written by a LATER build than the one scrubbing
// it. Decoding into [Company] and re-encoding would silently drop every field
// this build does not know, turning a privacy operation into data loss —
// which is the one outcome worse than the problem. So the walk is over the
// generic document: it finds the seats, rewrites the two fields it is here
// for, and leaves every byte it does not recognise exactly where it was.
//
// # It is not reversible, and that is the point
//
// [redact.ScrubMask] is not [redact.FieldMask]: a masked credential is
// restored from the row behind it by every write path, while a scrubbed field
// has nothing behind it. See that constant for what reading the two as one
// marker would cost.

// ScrubPersonalData rewrites a stored revision's document, replacing every
// seat's email and external account ids with [redact.ScrubMask].
//
// It reports how many FIELDS it rewrote, which is what tells a scrub that did
// something from one that found a revision already clean — an operator
// running this over a hundred revisions needs the difference, and "no error"
// does not carry it.
//
// IDEMPOTENT: a field already holding the mask is left alone and not counted,
// so re-running costs nothing and reports nothing.
func ScrubPersonalData(document []byte) ([]byte, int, error) {
	var root map[string]any
	if err := json.Unmarshal(document, &root); err != nil {
		return nil, 0, fmt.Errorf("config: read the revision to scrub: %w", err)
	}
	n := scrubSeats(root["roles"]) + scrubUnits(root["units"])
	if n == 0 {
		// THE ORIGINAL BYTES, not a re-encoding of them. A document that
		// needs no scrub must not be rewritten at all: re-encoding would
		// reorder every object's keys, so a diff across the run would show
		// the whole file changing to say that nothing did.
		return document, 0, nil
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, 0, fmt.Errorf("config: write the scrubbed revision: %w", err)
	}
	return out, n, nil
}

// scrubUnits walks a `units:` list, at any depth.
//
// RECURSIVE THROUGH `children`, because most of a company's people are not in
// the top-level list — a walk that stopped there would report success over an
// archive it had barely touched, which is the worst outcome a privacy
// operation has.
func scrubUnits(value any) int {
	units, ok := value.([]any)
	if !ok {
		return 0
	}
	n := 0
	for _, entry := range units {
		unit, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		n += scrubSeats(unit["roles"])
		n += scrubUnits(unit["children"])
	}
	return n
}

// scrubSeats walks a `roles:` list.
func scrubSeats(value any) int {
	seats, ok := value.([]any)
	if !ok {
		return 0
	}
	n := 0
	for _, entry := range seats {
		seat, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		n += erase(seat, "email")
		// THE WHOLE CONTACT BLOCK, key by key, rather than the named six.
		// Every value in it is an account id identifying a person on some
		// external surface — that is what the block IS — so a walk over
		// whatever keys the document carries reaches a field a later build
		// added, which a list of names written here could not.
		if contact, ok := seat["contact"].(map[string]any); ok {
			for key := range contact {
				n += erase(contact, key)
			}
		}
	}
	return n
}

// erase replaces one string field, reporting whether it changed anything.
//
// A NON-STRING IS LEFT ALONE rather than replaced: it is not a value this
// walk understands, and overwriting it would be guessing about a shape a
// later build gave the field.
func erase(object map[string]any, key string) int {
	value, isString := object[key].(string)
	if !isString || value == "" || value == redact.ScrubMask {
		return 0
	}
	object[key] = redact.ScrubMask
	return 1
}
