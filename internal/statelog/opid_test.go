package statelog_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// AN OPERATION ID CARRIES THE INSTANT IT WAS MINTED, so a retry that reuses the
// id reuses the instant — and nothing a retry does can move it later.
//
// This is the property the pre-adoption arm rests on. It was a field beside the
// id once, filled by every writer from its own clock at the call, so a retry
// after an adoption stated an instant after the adoption for an operation
// minted before it.
func TestAnOperationIDCarriesTheInstantItWasMinted(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 23, 10, 11, 12, 345_678_901, time.UTC)
	truncated := at.Truncate(time.Millisecond)

	t.Run("a fresh id answers its own instant, truncated rather than rounded", func(t *testing.T) {
		t.Parallel()
		id := statelog.NewOpID(at, "update-task-1")
		got, ok := statelog.OpMintedAt(id)
		if !ok {
			t.Fatalf("OpMintedAt(%q) found no instant in an id NewOpID minted", id)
		}
		if !got.Equal(truncated) {
			t.Fatalf("OpMintedAt = %s, want %s — the instant must never read LATER "+
				"than the mint, which rounding up would", got, truncated)
		}
		if again := statelog.NewOpID(at, "update-task-1"); again == id {
			t.Fatal("two fresh ids minted at one instant under one name are the " +
				"same id — a fresh id is one operation, and two operations " +
				"sharing one would have the ledger collapse the second into the " +
				"first")
		}
	})

	t.Run("an id names what it is, for whoever reads the ledger", func(t *testing.T) {
		t.Parallel()
		for name, id := range map[string]string{
			"fresh":   statelog.NewOpID(at, "update-task-1"),
			"derived": statelog.DeriveOpID(at, "update-task-1", "seed"),
		} {
			if !strings.HasSuffix(id, ".update-task-1") {
				t.Errorf("the %s id %q does not name its verb and object — a "+
					"stuck operation in the ledger is found by what it did", name, id)
			}
		}
		if bare := statelog.NewOpID(at, ""); len(bare) != len(uuid.NewString()) {
			t.Errorf("an unnamed id is %q, want a bare uuid — it is the id a "+
				"page comment is addressed by", bare)
		}
	})

	t.Run("a derived id is reproduced by a retry and still carries its instant", func(t *testing.T) {
		t.Parallel()
		one := statelog.DeriveOpID(at, "update-task-1", "work-key", "update", "task-1")
		two := statelog.DeriveOpID(at, "update-task-1", "work-key", "update", "task-1")
		if one != two {
			t.Fatalf("DeriveOpID gave %q then %q for one unit of work — a re-run "+
				"that derives a different id is a second operation", one, two)
		}
		got, ok := statelog.OpMintedAt(one)
		if !ok || !got.Equal(truncated) {
			t.Fatalf("OpMintedAt(derived) = (%s, %v), want %s", got, ok, truncated)
		}
		for name, other := range map[string]string{
			"another object":  statelog.DeriveOpID(at, "update-task-1", "work-key", "update", "task-2"),
			"another verb":    statelog.DeriveOpID(at, "update-task-1", "work-key", "comment", "task-1"),
			"parts regrouped": statelog.DeriveOpID(at, "update-task-1", "work-ke", "yupdate", "task-1"),
			"another name":    statelog.DeriveOpID(at, "update-task-2", "work-key", "update", "task-1"),
			"another instant": statelog.DeriveOpID(at.Add(time.Second), "update-task-1", "work-key", "update", "task-1"),
		} {
			if other == one {
				t.Errorf("%s derived the same id — two operations the ledger would "+
					"collapse into one", name)
			}
		}
	})

	t.Run("a step inherits its gesture's instant", func(t *testing.T) {
		t.Parallel()
		gesture := statelog.NewOpID(at, "create-task-1")
		step := statelog.StepOpID(statelog.StepOpID(gesture, "counter"), "retry")
		if step == gesture {
			t.Fatal("a step shares its gesture's id, so the second append would " +
				"resolve against the first's ledger row")
		}
		got, ok := statelog.OpMintedAt(step)
		if !ok || !got.Equal(truncated) {
			t.Fatalf("OpMintedAt(step) = (%s, %v), want the gesture's %s", got, ok, truncated)
		}
	})

	t.Run("an instant before the epoch reads as the epoch, the conservative end", func(t *testing.T) {
		t.Parallel()
		for name, instant := range map[string]time.Time{
			"the zero time":     {},
			"before the epoch":  time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC),
			"exactly the epoch": time.Unix(0, 0),
		} {
			got, ok := statelog.OpMintedAt(statelog.DeriveOpID(instant, "x"))
			if !ok || !got.Equal(time.Unix(0, 0).UTC()) {
				t.Errorf("%s: OpMintedAt = (%s, %v), want the epoch — an instant "+
					"the layout cannot hold must read as early as it can, never later",
					name, got, ok)
			}
		}
	})

	t.Run("an id the engine did not mint carries no instant", func(t *testing.T) {
		t.Parallel()
		for name, id := range map[string]string{
			"a literal":            "op-1",
			"empty":                "",
			"a random uuid":        uuid.NewString(),
			"a name-based uuid":    uuid.NewSHA1(uuid.NameSpaceURL, []byte("x")).String(),
			"a suffix with no dot": statelog.NewOpID(at, "") + "x",
			"a v7 in braces":       "{" + statelog.NewOpID(at, "") + "}",
		} {
			if got, ok := statelog.OpMintedAt(id); ok {
				t.Errorf("%s (%q) answered an instant %s — an id that carries none "+
					"must say so, or it is read as minted whenever its bytes happen "+
					"to decode to", name, id, got)
			}
		}
	})
}
