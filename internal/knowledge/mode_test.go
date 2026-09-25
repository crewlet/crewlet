package knowledge

import "testing"

// ONE VOCABULARY FOR EVERY SURFACE: the three wire values, the empty value as
// the hybrid default, and a refusal — never a silent default — for anything
// else. "meaning" is the dashboard's LABEL for semantic and never a wire value,
// which is exactly the confusion the refusal exists to catch.
func TestTheSearchModesAreOneVocabulary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		wire     string
		want     Mode
		lexical  bool
		semantic bool
	}{
		{"", ModeHybrid, true, true},
		{"hybrid", ModeHybrid, true, true},
		{" keyword ", ModeKeyword, true, false},
		{"semantic", ModeSemantic, false, true},
	} {
		got, err := ParseMode(tc.wire)
		if err != nil || got != tc.want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q", tc.wire, got, err, tc.want)
		}
		if got.Lexical() != tc.lexical || got.Semantic() != tc.semantic {
			t.Errorf("%q ranks lexical=%v semantic=%v", got, got.Lexical(), got.Semantic())
		}
	}
	for _, bad := range []string{"meaning", "Hybrid", "both", "bm25"} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) accepted a mode this build does not have", bad)
		}
	}
	for _, d := range []Degradation{NotDegraded, DegradedNoEmbeddings,
		DegradedEmbeddingFailed, DegradedSemanticPartial, DegradedUnsupported} {
		if !d.Valid() {
			t.Errorf("degradation %q is emitted and not valid", d)
		}
	}
	if Degradation("slow").Valid() {
		t.Error("an unknown degradation is valid")
	}
}
