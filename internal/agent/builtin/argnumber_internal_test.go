package builtin

import (
	"encoding/json"
	"testing"
)

// EVERY DECODER ON THIS PATH READS THROUGH json.Number (internal/toolsession,
// which the bridge serves the executor's run through too), so that is the shape
// a model's number arrives in. A reader that misses it fails SILENTLY: a
// `limit` falls to its fallback and a `points` is refused as "not a number",
// and nothing logs it, since "not sent" and "sent in a shape nobody reads" are
// the same branch.
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
	// A NUMBER WHERE A STRING BELONGS is common from models, and is read as
	// its text in either spelling.
	if got := argString(args, "name"); got != "42" {
		t.Errorf("argString = %q, want %q", got, "42")
	}
}

// AN ID PAST 2^53 is why the decoders read through json.Number: a reader via
// float64 would hand back the rounded one.
func TestAWideIDReadsExactly(t *testing.T) {
	args := map[string]any{"id": json.Number("9007199254740993")}
	if got := argInt(args, "id", 0); got != 9007199254740993 {
		t.Errorf("argInt = %d, want 9007199254740993", got)
	}
	if got := argString(args, "id"); got != "9007199254740993" {
		t.Errorf("argString = %q, want the literal the model sent", got)
	}
}

// THE PRESENCE-REPORTING READERS KEEP THEIR OWN DISCIPLINE in either spelling:
// a fraction is not a whole number of minutes or decimal places, and NaN and
// the infinities are not sizes. A json.Number is read as its decimal text, by
// the rule a string is.
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
