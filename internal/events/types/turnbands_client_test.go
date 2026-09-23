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
// # What a band entry actually is
//
// The dashboard's `lib/turnstory.ts` sorts one turn's non-phase events into
// the four panels the Turn screen draws, by naming event types in four sets.
// Every row it sorts came from ONE read — `EventLog.Turn`, which is
// `WHERE turn_id = ?` — and that column is filled from the event's own
// top-level `turn_id` FIELD: `store.ExtractTags` pulls it out of the
// marshalled envelope and `EventLog.Append` copies it onto the column. So a
// payload that declares no `turn_id` key writes an empty column, never matches
// the predicate, and cannot reach the client however it is banded.
//
// A band entry is therefore a PREDICATE against this package's wire contract,
// not a statement about where a row belongs. Naming a type with no `turn_id`
// is a promise the wire cannot keep.
//
// # Why it needed a gate
//
// Because it fails EMPTY, which is the one way a panel cannot say it is
// broken. Eight types were named that way at once. The three A2A audit records
// were the loudest — the "What else it did" band advertised "colleagues" on
// their behalf, and an A2A ask had never been drawn under that heading on any
// turn this engine has ever run — and the other five were quieter versions of
// the same thing. Nothing failed, nothing logged, and a turn that asked three
// colleagues rendered exactly like a turn that spoke to nobody.
//
// Four of the eight were repairs rather than facts, and all four have landed:
// the three A2A records now carry the publishing turn (internal/a2a/service.go)
// and skill_telemetry_write_failed carries the turn whose reflection tried the
// write (internal/learning/skilluse.go). They are back in their bands, and the
// stale check below is what made that a decision somebody took rather than one
// nobody got to.
//
// This is the `internal/clientsource` idiom for the reason that package's own
// doc gives: the dashboard is a separate build in a separate language that
// cannot import a Go identifier, so where it must know a set this engine owns
// it carries its own copy, and every such copy has drifted at least once. The
// check belongs on the engine side because the engine owns the value.
//
// # Two-sided, like the roster it is built on
//
// One direction stops a band naming a type that can never fill. The other
// stops the repair being forgotten: [keptOutOfTurnBands] records each decision
// with a reason, and an entry whose type GAINS a `turn_id` fails as stale — the
// fix landed, so the row can be banded, and somebody has to decide that rather
// than default to it. That half has already earned itself: it is what turned
// four of the original eight from a note nobody re-read into a failing build
// the day their payloads were repaired. A reason per entry rather than a
// set for the reason `events.excluded` gives about its own map: an exclusion
// and an oversight look identical from the outside, and writing the reason
// down is what lets the next reader tell them apart.

// turnBands are the declarations in `lib/turnstory.ts` whose members are
// matched against a turn's rows.
//
// TURN_STOP rides along although it draws nothing: it is the subset the Turn
// screen counts problems with, read off the same query answer, so an entry
// that can never appear understates the count by exactly as much as it fails
// to match.
var turnBands = []string{"WENT_WRONG", "GIVEN", "DID", "LEFT_BEHIND", "TURN_STOP"}

// absorbedBand is the SIXTH classification, and it is a map rather than a set.
//
// A row in [ABSORBED] is not dropped — it is drawn somewhere better, and the
// screen prints an inventory of them ("Already on this page") so a reader
// checking a cut turn's event total can see where each kind went. That makes
// it as much a promise against the query as the five bands are: a type here
// with no `turn_id` never reaches the browser, and the inventory omits it
// silently, which is the same EMPTY failure one band further along.
//
// Its own declaration, because its shape is its own: the bands are
// `new Set([...])` and this is `Record<string, string>` keyed on the type with
// the destination as the value, so the bands are read as their strings and
// this as its KEYS. Keeping them apart is also what lets the entry below be
// about the map rather than about a band nobody would find it in.
const absorbedBand = "ABSORBED"

// notPersisted are absorbed types that never reach the event store at all, so
// the `turn_id` question does not arise for them.
//
// A SEPARATE ROSTER FROM [keptOutOfTurnBands], because it answers a different
// question and the two must not be confused: that roster is "this row could be
// on the screen and is not, here is the repair owed", and this one is "this row
// is not in the store to begin with". An entry here is permanent by
// construction rather than by judgement — a stream-only event has no row for a
// query to return however its payload is shaped.
//
// AND IT IS CHECKED AGAINST THE ENGINE, in both directions, because a roster
// nothing contradicts is a comment. The classification asks whether the type
// is appended at all BEFORE it asks about the turn id — see [turnScopedTypes]
// — so an entry here is the only thing standing between a live-only absorbed
// type and a silent pass, and an entry whose type gained a category is stale
// the day it did. Asked in the other order the clause was unreachable: its one
// member declares `turn_id` like every other phase event, so the turn-id
// branch answered first and emptying this map left the gate green.
var notPersisted = map[string]string{
	"agent_turn_progress": "stream-only — published to the live socket and " +
		"never appended to crewlet_events, so it is in no turn's answer by " +
		"construction. The Turn screen absorbs it into the live phase card, " +
		"which is the only place it exists",
}

// keptOutOfTurnBands are the types a band named and had to give up, with the
// reason each one cannot be there.
//
// Two shapes, and the difference is the whole value of writing them down. Some
// of these will never carry a turn id, because no turn exists when they are
// published or because they describe many turns at once — those entries are
// permanent. The rest were events published INSIDE a turn by code that already
// held its id and simply did not stamp it; those were a defect in this package,
// and the stale check below is what announced the day each one was fixed.
//
// EVERY ENTRY LEFT IS OF THE FIRST KIND, which is a state worth naming rather
// than a coincidence to notice later: the four repairs are done, so a NEW entry
// here is either a genuinely turn-less event or a repair somebody has decided
// to defer, and the two must not be added under the same silence.
var keptOutOfTurnBands = map[string]string{
	"task_assigned": "the SCHEDULER's cron fire (internal/schedule), published " +
		"to wake a seat. It is the TRIGGER of a turn rather than work a turn " +
		"did, so it precedes every turn id there could be — and the Turn screen " +
		"already renders the trigger, as the brief. Permanent",
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
func TestTurnBandsNameOnlyTurnScopedEvents(t *testing.T) {
	t.Parallel()
	stamped, persisted, known := turnScopedTypes(t)

	for _, band := range turnBands {
		// `internal/events/types` is one level deeper than the package
		// directory [clientsource.Tree] is written against, so it joins
		// the extra step itself — the same as internal/api/configapi.
		body, err := clientsource.Literal("../"+clientsource.Tree, band)
		if err != nil {
			t.Errorf("%s: %v", band, err)
			continue
		}
		names := clientsource.Strings(body)
		if len(names) == 0 {
			t.Errorf("the Turn screen's %s band names no event type, so this "+
				"gate certifies nothing for it", band)
			continue
		}
		for _, name := range names {
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
	absorbed, err := clientsource.Literal("../"+clientsource.Tree, absorbedBand)
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
	} else {
		if len(names) == 0 {
			t.Errorf("the Turn screen's %s map names no event type, so this "+
				"gate certifies nothing for it", absorbedBand)
		}
		for _, name := range names {
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
				"its band in dashboard/src/lib/turnstory.ts and drop the entry, "+
				"or say here why it stays out anyway. It was kept out because: "+
				"%s", name, reason)
		}
	}
}

// turnScopedTypes reports the two conditions a row has to meet to be in a
// turn's answer, and the whole set of registered types beside them.
//
// TWO, BECAUSE THE QUERY HAS TWO. `EventLog.Turn` is `WHERE turn_id = ?` over
// `crewlet_events`, so a row reaches the Turn screen only if it was written at
// all and carries the key the predicate reads. A gate that asked the second
// question alone passes a LIVE-ONLY type — and `agent_turn_progress` is
// exactly one: it stamps `turn_id` like every other phase event and
// `observe.Record` still refuses it, so the absorbed map's roster clause for
// it sat behind a branch that answered first and never ran. Asked apart, each
// failure names its own repair: give the payload a turn id, or say why the
// type has no row.
//
// `stamped` is OFF THE MARSHALLED PAYLOAD, through the same `filled` the
// wire-contract test uses, rather than off a struct field name or off
// [wireTags]. The key is what `store.ExtractTags` reads and what the column is
// filled from, so the key is what decides it — a field named TurnID under a
// different tag would answer the Go question and the wrong one.
//
// `persisted` is the CATEGORY, which is the same value [observe.Record]
// branches on when it decides whether an event becomes a row at all. Asked of
// internal/events rather than restated here, for the reason that package's own
// doc gives about a second copy of a placement map: two lists of which types
// are stored disagree silently, and the half a test exercises is never the
// half production writes through.
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
