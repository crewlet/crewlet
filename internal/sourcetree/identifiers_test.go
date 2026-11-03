package sourcetree

import "testing"

// AN IDENTIFIER PREFILTER IS ON IDENTIFIERS ONLY.
func TestIdentifiersRefusesWhatIsNotAnIdentifier(t *testing.T) {
	t.Parallel()
	ids := MustIdentifiers("UpdateStream", "Auxiliary")
	if !ids.In([]byte("js.CreateOrUpdateStream(ctx, c)")) || !ids.In([]byte("e.Auxiliary(")) {
		t.Error("a file spelling one of the identifiers was skipped")
	}
	if ids.In([]byte("js.CreateStream(ctx, c)")) {
		t.Error("a file spelling none of the identifiers was admitted")
	}
	for _, bad := range [][]string{nil, {"net/http"}, {"func"}, {"a b"}, {""}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("MustIdentifiers(%q) accepted a name a prefilter is not "+
						"exact for", bad)
				}
			}()
			MustIdentifiers(bad...)
		}()
	}
}

// THE ZERO VALUE NAMES NOTHING AND ADMITS EVERY FILE, the one answer that
// cannot hide a construct.
func TestTheZeroIdentifiersAdmitsEveryFile(t *testing.T) {
	t.Parallel()
	if !(Identifiers{}).In([]byte("package p")) || !(Identifiers{}).In(nil) {
		t.Error("the zero Identifiers skips a file; it must admit everything")
	}
}
