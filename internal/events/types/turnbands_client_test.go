package types

import (
	"encoding/json"
	"fmt"
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
// This is the `internal/clientsource` idiom for the reason that package's own
// doc gives: the dashboard is a separate build in a separate language that
// cannot import a Go identifier, so where it must know a set this engine owns
// it carries its own copy, and every such copy has drifted at least once. The
// check belongs on the engine side because the engine owns the value.
//
// # Two-sided, like the roster it is built on
//
// One direction stops a band naming a type that can never fill. The other
// stops the repair being forgotten: [keptOutOfTurnBands] records the eight
// decisions with a reason each, and an entry whose type GAINS a `turn_id`
// fails as stale — the fix landed, so the row can be banded, and somebody has
// to decide that rather than default to it. A reason per entry rather than a
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

// keptOutOfTurnBands are the types a band named and had to give up, with the
// reason each one cannot be there.
//
// Two shapes, and the difference is the whole value of writing them down. Some
// of these will never carry a turn id, because no turn exists when they are
// published or because they describe many turns at once — those entries are
// permanent. The rest are events published INSIDE a turn by code that already
// holds its id and simply does not stamp it; those are a defect in this
// package, and the stale check below is what announces the day it is fixed.
var keptOutOfTurnBands = map[string]string{
	"a2a_channel_opened": "published by internal/a2a's service from inside the " +
		"ASKING turn's tool loop, which holds turnctx.Turn.RunID and passes it " +
		"to a2a.Ask.ParentTurnID already — the audit record just does not carry " +
		"it. Add TurnID/WorkKey to the payload and thread them off the Ask, and " +
		"this goes back in DID",
	"a2a_message_sent": "the same publisher and the same omission; the brief's " +
		"copy is published inside the asking turn and the reply's inside the " +
		"answering turn (internal/engine/a2a.go, which holds req.RunID)",
	"a2a_channel_closed": "the same, with one honest gap: the answering turn " +
		"closes the channel and knows its id, but the maintenance sweep closing " +
		"an idle channel belongs to no turn and would carry an empty one",
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
	"skill_telemetry_write_failed": "published by the SkillUse reflection worker " +
		"(internal/learning/skilluse.go), which holds t.Event.TurnID and stamps " +
		"it onto its siblings — episode_written, skill_refined and the rest all " +
		"carry it. This one does not. Add TurnID/WorkKey and it goes back in " +
		"WENT_WRONG",
}

// TestTurnBandsNameOnlyTurnScopedEvents holds the Turn screen's four bands
// against the `turn_id` key, both ways.
func TestTurnBandsNameOnlyTurnScopedEvents(t *testing.T) {
	t.Parallel()
	scoped, known := turnScopedTypes(t)

	for _, band := range turnBands {
		// `internal/events/types` is one level deeper than the package
		// directory [clientsource.Tree] is written against, so it joins
		// the extra step itself — the same as internal/api/configapi.
		body, err := clientsource.Declaration("../"+clientsource.Tree,
			fmt.Sprintf(`(?s)const %s = new Set\(\[(.*?)\]\)`, band))
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
			case !scoped[name]:
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
		if scoped[name] {
			t.Errorf("%q now declares a `turn_id` key, so the Turn screen CAN "+
				"draw it and this roster entry is stale. Put the type back in "+
				"its band in dashboard/src/lib/turnstory.ts and drop the entry, "+
				"or say here why it stays out anyway. It was kept out because: "+
				"%s", name, reason)
		}
	}
}

// turnScopedTypes reports which registered payloads publish a `turn_id` key,
// and the whole set of registered types beside it.
//
// OFF THE MARSHALLED PAYLOAD, through the same `filled` the wire-contract test
// uses, rather than off a struct field name or off [wireTags]. The key is what
// `store.ExtractTags` reads and what the column is filled from, so the key is
// what decides this — a field named TurnID under a different tag would answer
// the Go question and the wrong one.
func turnScopedTypes(t *testing.T) (scoped, known map[string]bool) {
	t.Helper()
	scoped = map[string]bool{}
	known = map[string]bool{}
	for _, prototype := range catalogue() {
		known[prototype.EventType()] = true
		raw, err := json.Marshal(filled(prototype))
		if err != nil {
			t.Fatalf("marshal %s: %v", prototype.EventType(), err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("remap %s: %v", prototype.EventType(), err)
		}
		if _, ok := keys["turn_id"]; ok {
			scoped[prototype.EventType()] = true
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
	return scoped, known
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
