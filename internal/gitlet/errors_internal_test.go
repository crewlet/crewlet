package gitlet

import (
	"errors"
	"testing"
)

// A REFUSAL BY ONE FIELD CARRIES THE FIELD AS A VALUE, read from the call that formed it (its
// format's field, or a "%s" field's argument), never the sentence; sentence and sentinel are
// unchanged, and a format whose field is no plain name carries none.
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
