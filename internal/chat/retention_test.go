package chat_test

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
)

// The prune's selection, which is the arithmetic that decides how long a
// company keeps what it said.
//
// It is PURE over values, so every case here runs without a store, a broker or
// a clock — which is the whole reason it is a value type at all: a rule that
// could only be exercised through a database is a rule nobody re-measures, and
// this one deletes conversations.

// A ROOM WHOSE RETENTION IS ZERO IS NEVER ELIGIBLE.
//
// Zero is a SETTING here — "keep this for ever" — which is why every field
// carrying it is a pointer, and it is the setting a company with a legal-hold
// policy comes to write. A selection that read zero as "no override" would
// delete that room's history on the company default a year later, and a
// selection that read an absent override as zero would never prune anything at
// all. Both are here, because each looks correct from the other's side.
func TestARoomWhoseRetentionIsZeroIsNeverEligible(t *testing.T) {
	t.Parallel()

	forever, ordinary := 0, 90
	got := chat.Eligible(chat.DefaultRetentionDays, []chat.ChannelRetention{
		{ChannelID: "room-keep", RetentionDays: &forever},
		{ChannelID: "room-90", RetentionDays: &ordinary},
		{ChannelID: "room-default"},
	})
	if ids := targetIDs(got); !slices.Equal(ids, []string{"room-90", "room-default"}) {
		t.Fatalf("the selection named %v, want the ninety-day room and the "+
			"one on the company default — a room set to %d is kept for ever",
			ids, chat.RetentionForever)
	}

	// THE COMPANY ITSELF MAY KEEP EVERYTHING, and then the sweep covers
	// nothing — including the rooms that stated a horizon of their own?
	// No: a room that asked to be kept for ninety days still is, which is
	// what makes the override an override in both directions.
	kept := chat.Eligible(chat.RetentionForever, []chat.ChannelRetention{
		{ChannelID: "room-default"},
		{ChannelID: "room-90", RetentionDays: &ordinary},
	})
	if ids := targetIDs(kept); !slices.Equal(ids, []string{"room-90"}) {
		t.Fatalf("with the company keeping everything the selection named %v, "+
			"want the room that set its own horizon", ids)
	}

	// A NEGATIVE HORIZON IS NOT A HORIZON. Both write-path validations
	// refuse one naming the field, so a negative here is a row some other
	// build wrote — and the only reading it has is a cutoff in the FUTURE,
	// which empties the room. There is no inverse for that.
	past := -30
	if got := chat.Eligible(chat.DefaultRetentionDays, []chat.ChannelRetention{
		{ChannelID: "room-broken", RetentionDays: &past},
	}); len(got) != 0 {
		t.Errorf("a room with a retention of %d days was selected for a "+
			"prune, at %v — a negative window is a cutoff in the future, "+
			"which deletes everything the room ever said", past, got)
	}
}

// A ROOM'S OWN RETENTION BEATS THE COMPANY DEFAULT, and the answer says which
// one decided it.
//
// The source is not decoration: an operator asking why a room was pruned at
// thirty days when the company keeps a year needs to be told where the thirty
// came from, and the alternative is comparing two config values by hand
// against a record that carries neither.
func TestARoomsOwnRetentionBeatsTheCompanyDefault(t *testing.T) {
	t.Parallel()

	short, long := 30, 730
	got := chat.Eligible(chat.DefaultRetentionDays, []chat.ChannelRetention{
		{ChannelID: "room-short", RetentionDays: &short},
		{ChannelID: "room-long", RetentionDays: &long},
		{ChannelID: "room-default"},
	})
	want := []chat.PruneTarget{
		{ChannelID: "room-default", Horizon: chat.Horizon{
			Days:     chat.DefaultRetentionDays,
			Duration: chat.DefaultRetentionDays * 24 * time.Hour,
			Source:   chat.RetentionCompany,
		}},
		{ChannelID: "room-long", Horizon: chat.Horizon{
			Days:     long,
			Duration: time.Duration(long) * 24 * time.Hour,
			Source:   chat.RetentionChannel,
		}},
		{ChannelID: "room-short", Horizon: chat.Horizon{
			Days:     short,
			Duration: time.Duration(short) * 24 * time.Hour,
			Source:   chat.RetentionChannel,
		}},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the selection is\n%v\nwant\n%v", got, want)
	}

	// THE HORIZON IS THE COMPANY'S OWN DEFAULT, not a second number that
	// happens to match today. The config field and this constant are one
	// setting seen from two sides, and a test that wrote 365 twice would
	// pass on the day somebody changed one of them.
	if chat.DefaultRetentionDays != config.DefaultMessageRetentionDays {
		t.Errorf("chat keeps messages for %d days by default and the config "+
			"field defaults to %d — the two are one setting, and a company "+
			"reading its own configuration would be told the wrong horizon",
			chat.DefaultRetentionDays, config.DefaultMessageRetentionDays)
	}

	// AND THE CUTOFF IS THE CALLER'S INSTANT, subtracted once. The WHEN
	// belongs to the writer's own snapshot; this half is arithmetic, which
	// is exactly why it can be checked here without a clock.
	at := time.Date(2031, 5, 6, 7, 8, 0, 0, time.UTC)
	if cutoff := got[2].Cutoff(at); !cutoff.Equal(at.AddDate(0, 0, -short)) {
		t.Errorf("a thirty-day room's cutoff at %s is %s, want %s",
			at, cutoff, at.AddDate(0, 0, -short))
	}
}

// THE SELECTION IS PURE: the same inputs answer the same thing, in the same
// order, with no clock in it.
//
// Both halves matter and neither implies the other. A function that sorted its
// answer would still be impure if it read the time; one that read no clock
// would still make two nodes publish different sweeps if it answered in the
// caller's own order — and "the caller's own order" is whatever a listing
// happened to return, which is not an ordering unless somebody says so.
func TestThePruneSelectionIsPure(t *testing.T) {
	t.Parallel()

	ninety, forever := 90, 0
	rooms := []chat.ChannelRetention{
		{ChannelID: "room-c", RetentionDays: &ninety},
		{ChannelID: "room-a"},
		{ChannelID: "room-keep", RetentionDays: &forever},
		{ChannelID: "room-b"},
	}
	first := chat.Eligible(chat.DefaultRetentionDays, rooms)
	second := chat.Eligible(chat.DefaultRetentionDays, rooms)
	if !slices.Equal(first, second) {
		t.Fatalf("two calls over one input answered\n%v\nand\n%v", first, second)
	}
	// THE SAME ROOMS IN ANOTHER ORDER, which is what two nodes reading one
	// company's rows actually have.
	shuffled := []chat.ChannelRetention{
		rooms[3], rooms[2], rooms[1], rooms[0],
	}
	if third := chat.Eligible(chat.DefaultRetentionDays, shuffled); !slices.Equal(
		first, third) {

		t.Fatalf("the same rooms in another order answered\n%v\nwant\n%v",
			third, first)
	}
	if ids := targetIDs(first); !slices.Equal(ids,
		[]string{"room-a", "room-b", "room-c"}) {

		t.Fatalf("the selection is ordered %v, want the rooms by id", ids)
	}

	// AND NO CLOCK IS READ INSIDE IT — structurally, because the defect is
	// structural. A behavioural test for it would have to catch two calls a
	// day apart disagreeing, which is a test nobody runs.
	//
	// THE MATCHER IS EXERCISED ON INPUT WHOSE VERDICT IS KNOWN, because a
	// guard asserting an absence passes identically when the thing is
	// absent and when the guard has gone inert.
	for _, positive := range []string{"time.Now()", "time.Since(at)"} {
		if !readsTheClock(positive) {
			t.Errorf("control: %q reads the clock and the matcher did not "+
				"flag it", positive)
		}
	}
	if readsTheClock("now.UTC().Add(-t.Horizon)") {
		t.Error("control: subtracting from an instant the CALLER passed in " +
			"was flagged, so this guard would fail on the correct shape")
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "retention.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse retention.go: %v", err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var buf strings.Builder
		if err := printer.Fprint(&buf, fset, call.Fun); err != nil {
			return true
		}
		if readsTheClock(buf.String()) {
			t.Errorf("%s reads the clock inside the selection — the cutoff is "+
				"a decision that already happened, so the instant belongs to "+
				"the writer's own snapshot and this half has to answer the "+
				"same thing whenever it is asked",
				fset.Position(call.Pos()).String())
		}
		return true
	})
}

// readsTheClock reports whether an expression asks what time it is.
func readsTheClock(expr string) bool {
	return strings.Contains(expr, "time.Now") ||
		strings.Contains(expr, "time.Since") ||
		strings.Contains(expr, "time.Until")
}

// targetIDs is a selection as the rooms it names.
func targetIDs(targets []chat.PruneTarget) []string {
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, target.ChannelID)
	}
	return out
}
