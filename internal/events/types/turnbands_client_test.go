package types

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/events"
)

// The Turn screen's bands, held against the wire keys this package freezes.
//
// A band entry is a PREDICATE against this package's wire contract. The
// dashboard's `lib/turnstory.ts` sorts one turn's non-phase events into the
// Turn screen's panels by naming types in sets declared in
// `contract/turnbands.ts`. Every row comes from ONE read, `EventLog.Turn`
// (`WHERE turn_id = ?`), whose column is filled from the event's own top-level
// `turn_id` FIELD (`store.ExtractTags`, `EventLog.Append`): a payload without
// that key never matches, so naming such a type is a promise the wire cannot
// keep. It fails EMPTY, the one way a panel cannot say it is broken.

// This is the `internal/clientsource` idiom: the dashboard is a separate build
// in another language that cannot import a Go identifier, so it carries its own
// copy of a set the engine owns, and the check belongs on the engine side.
//
// It is two-sided, plus a third. One direction stops a band naming a type that
// can never fill. The other stops a repair being forgotten: [keptOutOfTurnBands]
// records each decision with a reason, and an entry whose type GAINS a
// `turn_id` fails as stale, so somebody must decide to band it (a reason per
// entry, as `events.excluded` gives: an exclusion and an oversight look alike).
// The third, every stored, turn-scoped type is placed, is on the test itself.

// turnBands are the declarations in `contract/turnbands.ts` whose members are
// matched against a turn's rows.
//
// TURN_STOP rides along although it draws nothing: it is the subset the Turn
// screen counts problems with, read off the same query answer, so an entry
// that can never appear understates the count by exactly as much as it fails
// to match.
var turnBands = []string{"WENT_WRONG", "GIVEN", "DID", "LEFT_BEHIND", "TURN_STOP"}

// absorbedBand is the SIXTH classification, and it is a map rather than a set.
//
// A row in [ABSORBED] is drawn somewhere better, and the screen prints an
// inventory of them ("Already on this page"). That makes it as much a promise
// against the query as the five bands: a type here with no `turn_id` never
// reaches the browser, and the inventory omits it silently, the same EMPTY
// failure.
//
// Its own declaration, because its shape is its own: the bands are
// `new Set([...])` and this is `Record<string, string>` keyed on the type, so
// the bands are read as their strings and this as its KEYS.
const absorbedBand = "ABSORBED"

// notPersisted are absorbed types that never reach the event store at all, so
// the `turn_id` question does not arise for them.
//
// A SEPARATE ROSTER FROM [keptOutOfTurnBands]: that one is a repair owed for a
// row that could be on the screen; this one is a row not in the store at all,
// permanent by construction.
//
// IT IS CHECKED AGAINST THE ENGINE in both directions: the classification asks
// whether the type is appended BEFORE it asks about the turn id (see
// [turnScopedTypes]); asked the other way the clause was unreachable, since its
// one member declares `turn_id`, and emptying this map left the gate green. An
// entry whose type gained a category is stale.
var notPersisted = map[string]string{
	"agent_turn_progress": "stream-only — published to the live socket and " +
		"never appended to crewlet_events, so it is in no turn's answer by " +
		"construction. The Turn screen absorbs it into the live phase card, " +
		"which is the only place it exists",
}

// keptOutOfTurnBands are the types a band named and had to give up, with the
// reason each one cannot be there.
//
// Two shapes: events that never carry a turn id (no turn exists when they are
// published, or they describe many turns), which are permanent, and events
// published INSIDE a turn by code that held its id and did not stamp it, a
// defect the stale check announces when fixed.
//
// EVERY ENTRY LEFT IS OF THE FIRST KIND, so a NEW entry is either a genuinely
// turn-less event or a repair somebody decided to defer, and the two must not
// be added under the same silence.
var keptOutOfTurnBands = map[string]string{
	"task_assigned": "the SCHEDULER's cron fire (internal/schedule), published " +
		"to wake a seat. It is the TRIGGER of a turn rather than work a turn " +
		"did, so it precedes every turn id there could be — and the Turn screen " +
		"already renders the trigger, as the brief. Permanent",
	"work_pickup": "the work pickup's wake (internal/pickup), published to " +
		"hand an idle seat its next ready item. It is the TRIGGER of a turn, " +
		"as a schedule's fire is. Permanent",
	"turn_trigger_skipped": "the record that a trigger will NOT be worked " +
		"(internal/engine/turn.go) — it is addressed to the delivery, which is " +
		"why its payload carries trigger_id, and the whole of what it says is " +
		"that no turn ran on it. Permanent",
	"notification_skipped": "the notification routing gate dropping a delivery " +
		"(internal/notify/service.go), before any seat is woken. Permanent",
	"skill_promoted": "the curator duty promoting one unit's skill off a " +
		"CLUSTER of many seats' turns (internal/learning/promote.go). It names " +
		"no turn because there is no single right one to name — the same reason " +
		"skill_synthesized carries an empty turn id on its clustered path. " +
		"Permanent",
}

// TestTurnBandsNameOnlyTurnScopedEvents holds the Turn screen's four bands
// against the `turn_id` key, both ways.
//
// AND EVERY ROW THE TURN QUERY CAN RETURN HAS A PLACE, which is the third
// direction and the one a new event type trips. The residual band is for a
// type a NEWER build publishes — the registry is additive-only, so it must
// still render — and not a default for this build's own: a turn-scoped type
// that is stored and banded nowhere is drawn under "everything else" on every
// turn, which is the flat list the bands were introduced to end. So a type
// this build registers, stores and stamps with a turn id is placed in a band
// or absorbed, and the decision lands in the same change as the type.
func TestTurnBandsNameOnlyTurnScopedEvents(t *testing.T) {
	t.Parallel()
	stamped, persisted, known := turnScopedTypes(t)
	placed := map[string]bool{}
	// unread is whether any declaration could not be read at all — a band
	// or the absorbed map. The third direction below asks where every type
	// is PLACED, and a declaration nobody read placed nothing, so asking it
	// then would report every type that declaration holds as unplaced: a
	// wall of wrong findings under the one real one.
	unread := false

	for _, band := range turnBands {
		body, err := clientsource.Literal(clientsource.Tree(t), band)
		if err != nil {
			t.Errorf("%s: %v", band, err)
			unread = true
			continue
		}
		names := clientsource.Strings(body)
		if len(names) == 0 {
			t.Errorf("the Turn screen's %s band names no event type, so this "+
				"gate certifies nothing for it", band)
			continue
		}
		for _, name := range names {
			placed[name] = true
			switch {
			case !known[name]:
				t.Errorf("the Turn screen's %s band names %q, which this build "+
					"registers no payload for — a misspelled type is a band "+
					"entry that matches nothing, and it fails EMPTY. The "+
					"catalogue is %v", band, name, sortedKeys(known))
			case !persisted[name]:
				t.Errorf("the Turn screen's %s band names %q, which this build "+
					"never APPENDS: it carries no category, so observe.Record "+
					"refuses it and it drives the live projection alone. A row "+
					"that was never written cannot be in `WHERE turn_id = ?`'s "+
					"answer however its payload is shaped, so the band fails "+
					"EMPTY exactly as a missing turn id does", band, name)
			case !stamped[name]:
				reason, roster := keptOutOfTurnBands[name]
				if !roster {
					reason = "it is not on the kept-out roster in this file, so " +
						"nothing has written down why it was expected to work"
				}
				t.Errorf("the Turn screen's %s band names %q, whose payload "+
					"declares no `turn_id` key. The turn query is "+
					"`WHERE turn_id = ?` (internal/store/eventlog.go), so that "+
					"row can never be in the answer and the band fails EMPTY, "+
					"which reads as a quiet turn rather than as a broken panel. "+
					"Give the payload a turn id or take it out of the band: %s",
					band, name, reason)
			}
		}
	}

	// AND THE ABSORBED MAP, on the same terms. Read separately because its
	// declaration has a different shape, checked identically because it makes
	// the identical promise: the screen draws these rows, so a type here that
	// the turn query cannot return is an inventory line that never appears.
	absorbed, err := clientsource.Literal(clientsource.Tree(t), absorbedBand)
	var names []string
	if err == nil {
		// KEYS, NOT EVERY QUOTED STRING. The map's values are prose — "the
		// phase card it opens" — so [clientsource.Strings] would read a
		// destination as an event type and report the whole map as
		// unregistered. Quoted keys are keys: one holding a dot cannot be
		// written bare, so the first such type absorbed arrives quoted.
		names, err = clientsource.Keys(absorbed)
	}
	if err != nil {
		t.Errorf("%s: %v", absorbedBand, err)
		unread = true
	} else {
		if len(names) == 0 {
			t.Errorf("the Turn screen's %s map names no event type, so this "+
				"gate certifies nothing for it", absorbedBand)
		}
		for _, name := range names {
			placed[name] = true
			switch {
			case !known[name]:
				t.Errorf("the Turn screen's %s map names %q, which this build "+
					"registers no payload for. The catalogue is %v",
					absorbedBand, name, sortedKeys(known))
			case !persisted[name]:
				// In no answer because it is in no table. Declared, or
				// this is the first anyone has said so.
				if notPersisted[name] == "" {
					t.Errorf("the Turn screen's %s map names %q, which this "+
						"build never APPENDS: it carries no category, so "+
						"observe.Record refuses it. The screen's \"Already on "+
						"this page\" inventory therefore omits it silently, "+
						"which is the same EMPTY failure a band has one "+
						"classification back. Add it to notPersisted with the "+
						"reason it has no row, or give it a category",
						absorbedBand, name)
				}
			case stamped[name]:
				// Appended, and carrying a turn id: it is in the answer, and
				// the screen draws it wherever the map says.
			default:
				t.Errorf("the Turn screen's %s map names %q, whose payload "+
					"declares no `turn_id` key. The turn query is "+
					"`WHERE turn_id = ?` (internal/store/eventlog.go), so the "+
					"row can never be in the answer and the screen's "+
					"\"Already on this page\" inventory omits it silently — "+
					"the same EMPTY failure a band has, one classification "+
					"further along. Give the payload a turn id",
					absorbedBand, name)
			}
		}
		// And the roster stops describing the build in either of two ways:
		// the type leaves the map, or it starts being written.
		for name, reason := range notPersisted {
			switch {
			case !slices.Contains(names, name):
				t.Errorf("the not-persisted roster names %q, which the %s map "+
					"no longer carries — a stale entry is how a roster stops "+
					"describing the build. It was listed because: %s",
					name, absorbedBand, reason)
			case persisted[name]:
				t.Errorf("the not-persisted roster names %q, which this build "+
					"DOES append — it carries a category, so observe.Record "+
					"writes a row for it and the turn query can return one. "+
					"The excuse no longer applies: drop the entry and let the "+
					"turn-id question decide, or say here what still keeps the "+
					"row out. It was listed because: %s", name, reason)
			}
		}
	}

	// THE THIRD DIRECTION: every row the turn query can return has a place.
	// Asked only when every declaration was read — see `unread`.
	if !unread {
		for _, name := range sortedKeys(known) {
			if stamped[name] && persisted[name] && !placed[name] {
				t.Errorf("%q is stored and carries a `turn_id`, so the Turn screen's "+
					"query returns it — and no band in "+
					"dashboard/src/contract/turnbands.ts places it, so it is drawn "+
					"under \"everything else\" on every turn it appears in. Put it in "+
					"the band whose question it answers, or in ABSORBED naming where "+
					"the screen already draws it", name)
			}
		}
	}

	// THE OTHER DIRECTION. A roster entry is a repair somebody still owes, or
	// a permanent fact about the event; either way an entry that stopped being
	// true is an entry that has stopped describing this build.
	for name, reason := range keptOutOfTurnBands {
		if !known[name] {
			t.Errorf("the kept-out roster names %q, which the catalogue no "+
				"longer carries — a stale entry is how a roster stops "+
				"describing the build. It was kept out because: %s", name, reason)
			continue
		}
		if stamped[name] && persisted[name] {
			t.Errorf("%q now declares a `turn_id` key, so the Turn screen CAN "+
				"draw it and this roster entry is stale. Put the type back in "+
				"its band in dashboard/src/contract/turnbands.ts and drop the entry, "+
				"or say here why it stays out anyway. It was kept out because: "+
				"%s", name, reason)
		}
	}
}

// turnScopedTypes reports the two conditions a row meets to be in a turn's
// answer (written, and carrying the key), and the set of registered types.
// Asking only the second passes a LIVE-ONLY type such as `agent_turn_progress`,
// which stamps `turn_id` while `observe.Record` refuses it.
//
// `stamped` is OFF THE MARSHALLED PAYLOAD, through the wire-contract test's
// `filled`, because the key is what `store.ExtractTags` reads; a field named
// TurnID under another tag would answer the wrong question. `persisted` is the
// CATEGORY [observe.Record] branches on, asked of internal/events since two
// lists of stored types disagree silently.
func turnScopedTypes(t *testing.T) (stamped, persisted, known map[string]bool) {
	t.Helper()
	stamped = map[string]bool{}
	persisted = map[string]bool{}
	known = map[string]bool{}
	for _, prototype := range catalogue() {
		known[prototype.EventType()] = true
		if category, _ := events.Category(prototype.EventType()); category != "" {
			persisted[prototype.EventType()] = true
		}
		raw, err := json.Marshal(filled(prototype))
		if err != nil {
			t.Fatalf("marshal %s: %v", prototype.EventType(), err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("remap %s: %v", prototype.EventType(), err)
		}
		if _, ok := keys["turn_id"]; ok {
			stamped[prototype.EventType()] = true
		}
	}
	// The catalogue is checked against the registry by
	// TestCatalogueCoversRegistry, so this reads the registry only to
	// refuse a run where the two have not been reconciled yet — a gate that
	// derived "known" from a short catalogue would report a band entry as a
	// typo when the type exists.
	if registered := events.RegisteredTypes(); len(registered) != len(known) {
		t.Fatalf("the catalogue holds %d types and the registry %d, so this "+
			"gate cannot tell a misspelled band entry from a real type — fix "+
			"TestCatalogueCoversRegistry first", len(known), len(registered))
	}
	return stamped, persisted, known
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
