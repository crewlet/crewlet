package gitlet

import (
	"errors"
	"testing"
)

// A REFUSAL BY ONE FIELD CARRIES THE FIELD AS A VALUE, read from the call that
// formed it (its format's field, or the argument a "%s" field takes), never
// from the sentence; its sentence and its sentinel stay as they were. A
// format whose field is no plain name carries none.
//
// Mutation: take the field from the error's text and the "%s" case names the
// sentence's first word; drop the wrapping and errors.Is(ErrInvalid) is false.
func TestARefusalByOneFieldNamesItAsAValue(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err   error
		field string
		says  string
	}{
		{refusedf("%w: change.title: %d bytes", ErrInvalid, 300), "change.title",
			"gitlet: refused: change.title: 300 bytes"},
		{refusedf("%w: %s: %s", ErrInvalid, "review.findings[0]", "lines 0 to 0"), "review.findings[0]",
			"gitlet: refused: review.findings[0]: lines 0 to 0"},
		{refusedf("%w: %d bytes past the bound: trim it", ErrInvalid, 9), "",
			"gitlet: refused: 9 bytes past the bound: trim it"},
		{checkField("report.state", "%q is not one a CI reports", "skipped"), "report.state",
			`gitlet: refused: report.state: "skipped" is not one a CI reports`},
	} {
		var named *FieldRefused
		got := ""
		if errors.As(c.err, &named) {
			got = named.Field
		}
		if got != c.field || c.err.Error() != c.says || !errors.Is(c.err, ErrInvalid) {
			t.Errorf("%q names field %q (ErrInvalid %v), want %q saying %q", c.err, got,
				errors.Is(c.err, ErrInvalid), c.field, c.says)
		}
	}
}
