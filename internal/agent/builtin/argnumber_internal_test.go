package builtin

import (
	"encoding/json"
	"testing"
)

// EVERY DECODER ON THIS PATH READS THROUGH json.Number, so that is the shape a
// model's number actually arrives in — `httpapi.DecodeArgs` has done it for
// the HTTP providers all along, `mcpbridge` does it for a bridged run, and the
// subscription CLI backend does it now too.
//
// These readers only knew float64, and the failure was SILENT in both
// directions: a `limit` fell through to its fallback, so a search quietly
// returned the default page rather than the one the model asked for, and a
// `points` was refused as "not a number" on a call that carried one. Nothing
// logged either, because "the model did not send it" and "the model sent it in
// a shape nobody reads" are the same branch.
func TestNumericArgumentsArriveAsJSONNumbers(t *testing.T) {
	args := map[string]any{
		"limit":  json.Number("10"),
		"points": json.Number("2.5"),
		"name":   json.Number("42"),
	}
	if got := argInt(args, "limit", 20); got != 10 {
		t.Errorf("argInt = %d, want 10 — the fallback means it read nothing", got)
	}
	if got := argFloat(args, "points"); got != 2.5 {
		t.Errorf("argFloat = %v, want 2.5", got)
	}
	// A NUMBER WHERE A STRING BELONGS is a thing models do constantly, and
	// this reader already tolerated the float64 spelling of it.
	if got := argString(args, "name"); got != "42" {
		t.Errorf("argString = %q, want %q", got, "42")
	}
}

// AN ID PAST 2^53 is the whole reason the decoders read through json.Number,
// and a reader that went via float64 would hand the rounded one back — the
// exact defect the text form of a recorded call exists to prevent.
func TestAWideIDReadsExactly(t *testing.T) {
	args := map[string]any{"id": json.Number("9007199254740993")}
	if got := argInt(args, "id", 0); got != 9007199254740993 {
		t.Errorf("argInt = %d, want 9007199254740993", got)
	}
	if got := argString(args, "id"); got != "9007199254740993" {
		t.Errorf("argString = %q, want the literal the model sent", got)
	}
}

// THE PRESENCE-REPORTING READERS KEEP THEIR OWN DISCIPLINE, whichever spelling
// the number arrived in: a fraction is not a whole number of minutes — nor of
// decimal places — and NaN and the infinities are not sizes a total can be
// summed from. The json.Number arm routes back through the float arm rather
// than restating any of it.
func TestPresenceReportingReadersKeepTheirDisciplineOnJSONNumbers(t *testing.T) {
	if n, ok := argIntValue(json.Number("15")); !ok || n != 15 {
		t.Errorf("argIntValue(15) = %d, %v; want 15, true", n, ok)
	}
	if _, ok := argIntValue(json.Number("2.5")); ok {
		t.Error("argIntValue(2.5) accepted a fraction of a minute")
	}
	if f, ok := argFloatValue(json.Number("3")); !ok || f != 3 {
		t.Errorf("argFloatValue(3) = %v, %v; want 3, true", f, ok)
	}
	// Not a number json can even encode, so not a size either.
	if _, ok := argFloatValue(json.Number("1e400")); ok {
		t.Error("argFloatValue(1e400) accepted an overflow as a size")
	}
	if _, ok := argIntValue(json.Number("not a number")); ok {
		t.Error("argIntValue accepted a malformed number")
	}
}
