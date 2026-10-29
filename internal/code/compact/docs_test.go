package compact_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE PAGES STATE THE PUBLICATION'S CADENCE: docs/concepts/code-host.md and
// docs/concepts/scaling.md tell an operator when a repository is published —
// the tail's packs, its share of the objects and its objects, the debounce,
// the ceiling, the pace and the duty cycle, the chain's split and repack
// rules — in the constants' own figures, held here, so the re-derivation
// BenchmarkMidx's arms make of the provisional ones (M3-decisions M3-O39)
// cannot leave a page saying what the code no longer does.
//
// Mutation: change PublishPacks or PublishTailObjects, and neither page says
// any longer when a repository is published.
func TestThePagesStateThePublicationsCadence(t *testing.T) {
	t.Parallel()
	tail := fmt.Sprintf("%d packs, or a %s of its objects, or %s objects", compact.PublishPacks,
		fractionWord(t, compact.PublishShare), spaced(compact.PublishTailObjects))
	pace := fmt.Sprintf("%s since the last publication, or %s times that publication's build",
		spell(t, compact.PublishMinInterval), numberWord(t, compact.PublishDutyCycle))
	for page, wants := range map[string][]string{
		"code-host.md": {
			"its tail holds " + tail,
			fmt.Sprintf("the tail has stood still %s", spell(t, compact.PublishDebounce)),
			fmt.Sprintf("it has waited %s since the maintainer first listed it", spell(t, compact.PublishMaxWait)),
			pace,
			fmt.Sprintf("regardless while the chain would pass %d layers", code.MaxPublishedLayers),
			fmt.Sprintf("Where more than %d of the new layer's packs are not full", compact.LayerRepackPacks),
		},
		"scaling.md": {
			fmt.Sprintf("tail holds %d packs, a %s of the repository's objects, or %s objects", compact.PublishPacks,
				fractionWord(t, compact.PublishShare), spaced(compact.PublishTailObjects)),
			fmt.Sprintf("at most every %s or every %s times its last publication's build",
				spell(t, compact.PublishMinInterval), numberWord(t, compact.PublishDutyCycle)),
			fmt.Sprintf("about %s large repositories at that duty cycle", numberWord(t, compact.PublishDutyCycle)),
		},
	} {
		path := filepath.Join(sourcetree.Root(t), "docs", "concepts", page)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q", path, want)
			}
		}
	}
}

// spaced is n as the docs write a number: its thousands set apart by a space
// (90 909).
func spaced(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}

// spell is d as the pages write a duration: whole minutes, or whole seconds.
func spell(t *testing.T, d time.Duration) string {
	t.Helper()
	switch {
	case d%time.Minute == 0:
		return fmt.Sprintf("%d minutes", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%d seconds", d/time.Second)
	}
	t.Fatalf("the pages write %v in whole minutes or seconds: say it there, and here", d)
	return ""
}

// numberWord is n as the pages spell a small count in words.
func numberWord(t *testing.T, n int) string {
	t.Helper()
	words := map[int]string{2: "two", 4: "four", 5: "five", 8: "eight", 10: "ten", 16: "sixteen", 20: "twenty"}
	w, ok := words[n]
	if !ok {
		t.Fatalf("the pages spell %d in words: add it here as they write it", n)
	}
	return w
}

// fractionWord is 1/n as the pages spell a share in words.
func fractionWord(t *testing.T, n int) string {
	t.Helper()
	words := map[int]string{2: "half", 4: "quarter", 8: "eighth", 16: "sixteenth", 32: "thirty-second"}
	w, ok := words[n]
	if !ok {
		t.Fatalf("the pages spell a 1/%d share in words: add it here as they write it", n)
	}
	return w
}
