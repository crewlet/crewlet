package sandbox

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// A RUN'S TITLE IS NEVER CUT INSIDE A MARKER. The launching turn's title is
// redacted (internal/engine's turnTitle) and the row keeps its opening within
// MaxRunTitleBytes: a credential where the cut falls, its marker begun at
// every offset across the cut, is kept whole or not at all, never in part,
// and one inside the kept opening stays a marker there.
//
// Mutation: cut the title where its share ends (keptTitle), and the row keeps
// a marker in part.
func TestARunsTitleIsNeverCutInsideAMarker(t *testing.T) {
	t.Parallel()
	var key string
	for _, s := range redacttest.Shapes() {
		if s.Name == "anthropic-key" {
			key = s.Sample
		}
	}
	marker := redact.Secrets(key)
	if marker == key || !strings.HasPrefix(marker, "[REDACTED:") {
		t.Fatalf("redact's corpus no longer has a provider's key: %q", marker)
	}
	// WHERE A PLAIN TITLE IS CUT, so the sweep below puts the marker across it.
	plain, ok := rowRecord.Opening(keptTitle(strings.Repeat("a", 2*MaxRunTitleBytes)), 2*MaxRunTitleBytes)
	if !ok {
		t.Fatal("a title twice the row's share was kept whole")
	}
	head := "ship it with " + key + " then "
	rest := strings.Repeat(" and the rest of the ask", 200)
	for at := len(plain) - len(marker) - 8; at <= len(plain)+8; at++ {
		pad := strings.Repeat("a", at-len("ship it with "+marker+" then ")-1)
		title := redact.Secrets(head + pad + " " + key + " and" + rest)
		if strings.Index(title, marker+" and") != at {
			t.Fatalf("the second marker begins at %d, want %d", strings.Index(title, marker+" and"), at)
		}
		got := keptTitle(title)
		if _, cut := rowRecord.Opening(got, len(title)); !cut {
			t.Fatalf("marker at %d: the title was kept whole; this test is of one the row cuts", at)
		}
		if kept.Quoted(got) > MaxRunTitleBytes {
			t.Fatalf("marker at %d: the row keeps %d bytes of title, past its %d", at, kept.Quoted(got),
				MaxRunTitleBytes)
		}
		if !strings.HasPrefix(got, "ship it with "+marker+" then ") {
			t.Errorf("marker at %d: the kept title opens %.60q, want the first credential as its marker", at, got)
		}
		for i := 0; i+6 <= len(key); i++ {
			if strings.Contains(got, key[i:i+6]) {
				t.Fatalf("marker at %d: the kept title holds %q of the key", at, key[i:i+6])
			}
		}
		for _, words := range redact.OwnWords(got) {
			if strings.Contains(words, "REDACTED") {
				t.Fatalf("marker at %d: the kept title holds a marker cut in part: …%s", at,
					words[max(0, len(words)-60):])
			}
		}
	}
}
