package learning

import (
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// THE STORE'S "NOTHING TO EMBED" IS THE EMBEDDER'S, rune for rune: the trim set
// a fill selects by is every rune unicode.IsSpace reports and no other, which
// is the set embeddings.Prepare drops — checked over the whole of Unicode,
// because a set copied by hand is right for the characters somebody thought of.
func TestTheFillsTrimSetIsWhatTheEmbedderDrops(t *testing.T) {
	t.Parallel()
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if got, want := slices.Contains(preparedSpace, r), unicode.IsSpace(r); got != want {
			t.Fatalf("U+%04X: in the fill's trim set %v, a space to unicode.IsSpace %v", r, got, want)
		}
	}
	for _, r := range preparedSpace {
		if embeddings.Prepare("x"+string(r)+"y") != "x y" || embeddings.Prepare(string(r)) != "" {
			t.Fatalf("U+%04X is in the trim set but the embedder keeps it", r)
		}
	}
	if !strings.HasPrefix(preparedSpaceSQL, "char(9, 10, 11, 12, 13, 32, 133, 160,") {
		t.Fatalf("the trim set renders as %s", preparedSpaceSQL)
	}
}
