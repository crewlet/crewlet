package learning_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
)

func counterparties(t *testing.T) *learning.Counterparties {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "c.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return learning.NewCounterparties(db)
}

func mustRecord(t *testing.T, c *learning.Counterparties, o learning.Observation) bool {
	t.Helper()
	counted, err := c.Record(context.Background(), o)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	return counted
}

func mustGet(t *testing.T, c *learning.Counterparties, observer string, s learning.Subject) learning.Profile {
	t.Helper()
	p, ok, err := c.Get(context.Background(), observer, s)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("no profile for %+v", s)
	}
	return p
}

func TestAProfileIsCreatedThenMergedInPlace(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	alice := learning.Subject{Handle: "alice", Name: "Alice"}

	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: alice, At: base,
		Traits: map[string]any{"prefers": "bullet points"},
	})
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: alice, At: base.Add(time.Hour),
		Traits: map[string]any{"timezone": "CET"},
	})

	p := mustGet(t, c, "ceo", alice)
	if p.Traits["prefers"] != "bullet points" || p.Traits["timezone"] != "CET" {
		t.Errorf("traits = %v, want both merged", p.Traits)
	}
	if p.InteractionCount != 2 {
		t.Errorf("interactions = %d, want 2", p.InteractionCount)
	}
	if !p.FirstSeenAt.Equal(base) {
		t.Errorf("first seen = %v, want the first observation", p.FirstSeenAt)
	}
}

func TestTheTwoTimestampsMeasureDifferentThings(t *testing.T) {
	t.Parallel()
	// last_updated moves on every upsert, so it measures INTERACTION
	// cadence. last_corroborated moves only when the traits actually
	// changed, so it measures trait-CHANGE cadence — a counterparty seen
	// daily whose traits have not moved in months is one the observer has
	// stopped learning about, and that is a different fact from not having
	// seen them.
	c := counterparties(t)
	bob := learning.Subject{Handle: "bob"}
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base,
		Traits: map[string]any{"style": "terse"},
	})

	// An interaction that learned nothing new.
	later := base.Add(24 * time.Hour)
	mustRecord(t, c, learning.Observation{Observer: "ceo", Subject: bob, At: later})
	p := mustGet(t, c, "ceo", bob)
	if !p.LastUpdatedAt.Equal(later) {
		t.Errorf("last updated = %v, want the latest interaction", p.LastUpdatedAt)
	}
	if !p.LastCorroboratedAt.Equal(base) {
		t.Errorf("last corroborated = %v, want the last real change", p.LastCorroboratedAt)
	}

	// A patch that repeats a trait it already holds is not a corroboration
	// either: treating it as one makes every interaction look like fresh
	// learning and the staleness signal stops meaning anything.
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: later.Add(time.Hour),
		Traits: map[string]any{"style": "terse"},
	})
	if p := mustGet(t, c, "ceo", bob); !p.LastCorroboratedAt.Equal(base) {
		t.Errorf("an unchanged trait moved the corroboration stamp to %v", p.LastCorroboratedAt)
	}

	// A real change does move it.
	changed := later.Add(2 * time.Hour)
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: changed,
		Traits: map[string]any{"style": "expansive"},
	})
	if p := mustGet(t, c, "ceo", bob); !p.LastCorroboratedAt.Equal(changed) {
		t.Errorf("a real change left the corroboration stamp at %v", p.LastCorroboratedAt)
	}
}

func TestOneWorkKeyCountsOneInteraction(t *testing.T) {
	t.Parallel()
	// Two nodes racing the same turn, or a redelivery. Counting it twice
	// inflates a relationship the seat has had once.
	c := counterparties(t)
	bob := learning.Subject{Handle: "bob"}
	if !mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base, WorkKey: "wk-1",
	}) {
		t.Error("the first observation was not counted")
	}
	if mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base, WorkKey: "wk-1",
	}) {
		t.Error("a repeat of the same work key was counted again")
	}
	if p := mustGet(t, c, "ceo", bob); p.InteractionCount != 1 {
		t.Errorf("interactions = %d, want 1", p.InteractionCount)
	}
	// A different key counts.
	if !mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base, WorkKey: "wk-2",
	}) {
		t.Error("a new work key was not counted")
	}
}

func TestUnkeyedObservationsAlwaysCount(t *testing.T) {
	t.Parallel()
	// Without a key a redelivery is indistinguishable from a second
	// interaction, and the two errors are not symmetric: suppressing real
	// interactions makes a seat believe it has never met someone it works
	// with daily.
	c := counterparties(t)
	bob := learning.Subject{Handle: "bob"}
	for range 3 {
		if !mustRecord(t, c, learning.Observation{Observer: "ceo", Subject: bob, At: base}) {
			t.Error("an unkeyed observation was suppressed")
		}
	}
	if p := mustGet(t, c, "ceo", bob); p.InteractionCount != 3 {
		t.Errorf("interactions = %d, want 3", p.InteractionCount)
	}
}

func TestOnlyTheIMMEDIATELYPrecedingKeyIsRemembered(t *testing.T) {
	t.Parallel()
	// One column rather than a side table: the duplicate this guards
	// against is always the immediately preceding write, never one from
	// last week — so a key returning after another key does count.
	c := counterparties(t)
	bob := learning.Subject{Handle: "bob"}
	for _, key := range []string{"wk-1", "wk-2"} {
		mustRecord(t, c, learning.Observation{Observer: "ceo", Subject: bob, At: base, WorkKey: key})
	}
	if !mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base, WorkKey: "wk-1",
	}) {
		t.Error("a key seen two writes ago was treated as the immediate duplicate")
	}
}

func TestAResolvedSeatAndAnExternalHumanCoexistUnderOneObserver(t *testing.T) {
	t.Parallel()
	// The composite key is what makes this work, and it is why the empty
	// fields are '' and not NULL: NULL would make every such row distinct
	// from every other, which is the opposite of what the key is for.
	c := counterparties(t)
	seat := learning.Subject{Handle: "bob"}
	human := learning.Subject{ExternalID: "U123", Platform: "slack", Name: "Bob (external)"}

	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: seat, At: base, Traits: map[string]any{"kind": "seat"}})
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: human, At: base, Traits: map[string]any{"kind": "human"}})

	if got := mustGet(t, c, "ceo", seat); got.Traits["kind"] != "seat" {
		t.Errorf("seat profile = %v", got.Traits)
	}
	if got := mustGet(t, c, "ceo", human); got.Traits["kind"] != "human" {
		t.Errorf("human profile = %v", got.Traits)
	}
}

func TestProfilesAreScopedToTheObserver(t *testing.T) {
	t.Parallel()
	// What one seat learned about someone is not what another seat learned.
	c := counterparties(t)
	bob := learning.Subject{Handle: "bob"}
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base, Traits: map[string]any{"seen": "by ceo"}})

	_, ok, err := c.Get(context.Background(), "cto", bob)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("one observer read another's profile")
	}
}

func TestAnUnknownSubjectIsNotAnError(t *testing.T) {
	t.Parallel()
	// The first time a seat meets anyone. It must be distinguishable from a
	// store that could not be read.
	c := counterparties(t)
	_, ok, err := c.Get(context.Background(), "ceo", learning.Subject{Handle: "stranger"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("an unmet subject reported a profile")
	}
}

func TestAMissingDisplayNameDoesNotEraseTheKnownOne(t *testing.T) {
	t.Parallel()
	// A chat payload that omits the name is common, and blanking the
	// profile makes every later prompt say "someone" about a person the
	// seat has met a dozen times.
	c := counterparties(t)
	named := learning.Subject{Handle: "bob", Name: "Bob Smith"}
	mustRecord(t, c, learning.Observation{Observer: "ceo", Subject: named, At: base})
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: learning.Subject{Handle: "bob"}, At: base.Add(time.Hour)})

	if p := mustGet(t, c, "ceo", named); p.Subject.Name != "Bob Smith" {
		t.Errorf("name = %q, want the one already known", p.Subject.Name)
	}
	// A new name still replaces the old: people do rename themselves.
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: learning.Subject{Handle: "bob", Name: "Robert"},
		At: base.Add(2 * time.Hour)})
	if p := mustGet(t, c, "ceo", named); p.Subject.Name != "Robert" {
		t.Errorf("name = %q, want the update", p.Subject.Name)
	}
}

func TestAnObservationNeedsSomebodyOnBothEnds(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	for name, bad := range map[string]learning.Observation{
		"no observer":     {Subject: learning.Subject{Handle: "bob"}, At: base},
		"no subject":      {Observer: "ceo", At: base},
		"half a stranger": {Observer: "ceo", Subject: learning.Subject{ExternalID: "U1"}, At: base},
	} {
		if _, err := c.Record(context.Background(), bad); err == nil {
			t.Errorf("%s: recorded cleanly", name)
		}
	}
}

func TestNestedTraitValuesCompareByContent(t *testing.T) {
	t.Parallel()
	// Trait values come off a model as arbitrary decoded JSON, nested maps
	// and slices included, and Go's == panics on those. Comparing their
	// renderings also makes 1 and 1.0 the same trait, which is what a
	// reader means by unchanged.
	c := counterparties(t)
	bob := learning.Subject{Handle: "bob"}
	nested := map[string]any{"channels": []any{"eng", "design"}}
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base, Traits: nested})

	// The same nested value again is not a corroboration.
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base.Add(time.Hour),
		Traits: map[string]any{"channels": []any{"eng", "design"}}})
	if p := mustGet(t, c, "ceo", bob); !p.LastCorroboratedAt.Equal(base) {
		t.Errorf("an identical nested value moved the corroboration stamp")
	}
	// A different one is.
	changed := base.Add(2 * time.Hour)
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: changed,
		Traits: map[string]any{"channels": []any{"eng"}}})
	if p := mustGet(t, c, "ceo", bob); !p.LastCorroboratedAt.Equal(changed) {
		t.Errorf("a changed nested value did not move the corroboration stamp")
	}
}

func TestTraitsOfEqualLengthAreStillCompared(t *testing.T) {
	t.Parallel()
	// The comparison is on CONTENT, not on size. Every other case in this
	// file happens to change the rendering's length as well, so a
	// comparison that only measured length would pass all of them — found
	// by mutation.
	c := counterparties(t)
	bob := learning.Subject{Handle: "bob"}
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base,
		Traits: map[string]any{"style": "terse"}})

	changed := base.Add(time.Hour)
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: changed,
		Traits: map[string]any{"style": "brisk"}}) // same length, different trait
	p := mustGet(t, c, "ceo", bob)
	if !p.LastCorroboratedAt.Equal(changed) {
		t.Errorf("a same-length change did not move the corroboration stamp")
	}
	if p.Traits["style"] != "brisk" {
		t.Errorf("style = %v, want the update", p.Traits["style"])
	}
}

func TestFirstSeenIsWrittenOnceAndNeverMoves(t *testing.T) {
	t.Parallel()
	// It is how long the observer has known the subject, so a later
	// interaction must not restart the clock. The upsert holds this by
	// omitting the column from its update clause.
	c := counterparties(t)
	bob := learning.Subject{Handle: "bob"}
	mustRecord(t, c, learning.Observation{Observer: "ceo", Subject: bob, At: base})
	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base.Add(90 * 24 * time.Hour)})
	if p := mustGet(t, c, "ceo", bob); !p.FirstSeenAt.Equal(base) {
		t.Errorf("first seen = %v, want the first interaction", p.FirstSeenAt)
	}
}

func TestSubjectPredicates(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		s               learning.Subject
		valid, resolved bool
	}{
		"seat":          {learning.Subject{Handle: "bob"}, true, true},
		"external":      {learning.Subject{ExternalID: "U1", Platform: "slack"}, true, false},
		"half external": {learning.Subject{ExternalID: "U1"}, false, false},
		"name only":     {learning.Subject{Name: "Bob"}, false, false},
		"nobody":        {learning.Subject{}, false, false},
	} {
		if got := tc.s.Valid(); got != tc.valid {
			t.Errorf("%s: Valid = %v", name, got)
		}
		if got := tc.s.Resolved(); got != tc.resolved {
			t.Errorf("%s: Resolved = %v", name, got)
		}
	}
}

// AN UNKEYED OBSERVATION MUST NOT DISARM THE GUARD.
//
// Unkeyed observations count (see TestUnkeyedObservationsAlwaysCount), but the
// remembered key is what the redelivery guard compares against — and writing
// "" into it left the NEXT observation with nothing to match. A redelivery of
// a keyed interaction arriving after an unkeyed one then compared its real key
// against "", differed, and counted a second time: exactly the double count
// the guard exists to prevent, reachable through an ordinary interleaving.
func TestAnUnkeyedObservationDoesNotDisarmTheRedeliveryGuard(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	bob := learning.Subject{Handle: "bob"}

	mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base, WorkKey: "wk-1",
	})
	// An unkeyed observation lands in between — a chat message with no
	// work item behind it, which is the ordinary case.
	mustRecord(t, c, learning.Observation{Observer: "ceo", Subject: bob, At: base})

	// Now the redelivery of wk-1. It must still be recognised.
	if mustRecord(t, c, learning.Observation{
		Observer: "ceo", Subject: bob, At: base, WorkKey: "wk-1",
	}) {
		t.Error("a redelivery of the last keyed interaction was counted again: " +
			"the unkeyed observation in between overwrote the remembered key")
	}
	if p := mustGet(t, c, "ceo", bob); p.InteractionCount != 2 {
		t.Errorf("interactions = %d, want 2 (one keyed, one unkeyed)", p.InteractionCount)
	}
	if p := mustGet(t, c, "ceo", bob); p.LastWorkKey != "wk-1" {
		t.Errorf("last work key = %q, want the last KEYED unit of work", p.LastWorkKey)
	}
}
