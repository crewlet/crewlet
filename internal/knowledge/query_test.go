package knowledge

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// THE QUERY BOUND IS ONE RULE: past MaxQueryBytes of the text a searcher reads
// is refused naming the limit and the size, at it is a query, and whitespace
// at the ends — which no searcher reads — is not counted against it.
//
// Every surface asks this whether to refuse, so a rule that drifted here would
// drift on all of them at once rather than on one, which is the point of
// declaring it once.
func TestAQueryIsRefusedPastTheBoundAndOnlyPastIt(t *testing.T) {
	t.Parallel()
	at := strings.Repeat("q", MaxQueryBytes)
	for _, ok := range []string{"", "rollback runbook", at, "  " + at + "\n\t"} {
		if err := CheckQuery(ok); err != nil {
			t.Errorf("a %d-byte query was refused: %v", len(strings.TrimSpace(ok)), err)
		}
	}
	err := CheckQuery(at + "é")
	if !errors.Is(err, ErrQueryTooLong) {
		t.Fatalf("a query past the bound answered %v, want ErrQueryTooLong", err)
	}
	var tooLong *QueryTooLongError
	if !errors.As(err, &tooLong) || tooLong.Bytes != MaxQueryBytes+2 {
		t.Fatalf("the refusal carries %+v, want the %d bytes measured", tooLong, MaxQueryBytes+2)
	}
	for _, want := range []string{strconv.Itoa(MaxQueryBytes), strconv.Itoa(MaxQueryBytes + 2)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %s", err, want)
		}
	}
}
