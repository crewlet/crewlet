package compact_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/compact"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE PAGES STATE THE PUBLICATION'S CADENCE: docs/concepts/gitlet.md and
// docs/concepts/scaling.md tell an operator when a repository is published —
// the tail's packs, its share of the objects and its objects, the debounce,
// the ceiling, the pace and the duty cycle, the chain's split and repack
// rules — in the constants' own figures, held here, so a re-derivation by
// BenchmarkMidx's arms (internal/gitlet/cache), on this pin or the next, cannot
// leave a page saying what the code no longer does.
//
// Mutation: change PublishPacks, PublishShare or PublishTailObjects, and
// neither page says any longer when a repository is published.
func TestThePagesStateThePublicationsCadence(t *testing.T) {
	t.Parallel()
	tail := fmt.Sprintf("%d packs, or a %s of its objects, or %s objects", compact.PublishPacks,
		fractionWord(t, compact.PublishShare), spaced(compact.PublishTailObjects))
	pace := fmt.Sprintf("%s since the last publication, or %s times that publication's build",
		spell(t, compact.PublishMinInterval), numberWord(t, compact.PublishDutyCycle))
	for page, wants := range map[string][]string{
		"gitlet.md": {
			"its tail holds " + tail,
			fmt.Sprintf("the tail has stood still %s", spell(t, compact.PublishDebounce)),
			fmt.Sprintf("it has waited %s since the maintainer first listed it", spell(t, compact.PublishMaxWait)),
			pace,
			fmt.Sprintf("regardless while the chain would pass %d layers", gitlet.MaxPublishedLayers),
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

// THE PAGES STATE THE EXPUNGE'S BOUNDS: docs/concepts/gitlet.md,
// docs/guides/gitlet.md and docs/concepts/object-store.md (what a hosting
// company's bucket grows by) tell an operator when an expunge is due (its
// share and its least), how often a repository is asked (its interval), how
// long what a force-push left is kept (the prune grace), how many packs a
// begin takes and writes, how long the object store keeps what it dropped
// (the retire grace) and what its builds are admitted at — in the constants'
// own figures, held here, so moving one (ExpungeMinBytes is provisional until
// BenchmarkExpunge decides it) cannot leave a page saying what the code no
// longer does.
//
// Mutation: change gitlet.ExpungeShare, gitlet.ExpungeMinBytes, gitlet.PruneGrace,
// gitlet.ExpungeInterval, gitlet.MaxExpungeInputs, gitlet.MaxExpungeOutputs,
// gitlet.RetireGrace, cache.CompactBytesPerObject,
// gitproc.TransferBytesPerObject or cache.CompactWindowBytes's multiple, and
// a page no longer says what that one bounds.
func TestThePagesStateTheExpungesBounds(t *testing.T) {
	t.Parallel()
	share := fractionWord(t, int(gitlet.ExpungeShare))
	least := mebibytes(t, int64(gitlet.ExpungeMinBytes))
	interval := span(t, gitlet.ExpungeInterval)
	grace, graceDays := span(t, gitlet.PruneGrace), days(t, gitlet.PruneGrace)
	retired := span(t, gitlet.RetireGrace)
	windowBlobs := cache.CompactWindowBytes(gitlet.Limits{MaxBlobBytes: 1})
	window := mebibytes(t, cache.CompactWindowBytes(gitlet.Limits{MaxBlobBytes: gitlet.DefaultMaxBlobBytes}))
	for page, wants := range map[string][]string{
		filepath.Join("concepts", "gitlet.md"): {
			fmt.Sprintf("is **due** once that is more than a %s of the repository's live pack bytes and at least %s",
				share, least),
			fmt.Sprintf("The %s is provisional", least),
			fmt.Sprintf("due past a %s and %s", share, least),
			fmt.Sprintf("last expunged or found clean %s ago or never", interval),
			fmt.Sprintf("takes it off every Gitlet node's list for %s", interval),
			fmt.Sprintf("so a clean repository's history is walked at most once %s", interval),
			fmt.Sprintf("for **%s**, git's own `gc.pruneExpire` default: every object", graceDays),
			fmt.Sprintf("stays fetchable by its id for %s, and no push in flight", grace),
			fmt.Sprintf("kept **%s**, git's own `gc.pruneExpire` default, because the weekly", graceDays),
			fmt.Sprintf("fetchable by its id for %s after the push", grace),
			fmt.Sprintf("keeps it for %s after the deletion", graceDays),
			fmt.Sprintf("Restore a branch you need within %s of its deletion", grace),
			fmt.Sprintf("A begin takes at most %s packs and writes at most %d", spaced(int64(gitlet.MaxExpungeInputs)),
				gitlet.MaxExpungeOutputs),
			fmt.Sprintf("each %s apart on the schedule", interval),
			fmt.Sprintf("by **the object store, for %s** after the commit", retired),
			fmt.Sprintf("the schedule waits for a %s of the repository to be unreachable", share),
			fmt.Sprintf("%d bytes an object of the whole repository", gitproc.TransferBytesPerObject),
			fmt.Sprintf("%s times `code.limits.max_blob_bytes` (%s by default)", numberWord(t, int(windowBlobs)), window),
			fmt.Sprintf("and %d bytes an object of the packs it sorts", cache.CompactBytesPerObject),
		},
		filepath.Join("guides", "gitlet.md"): {
			fmt.Sprintf("every %s once a %s of the repository, and at least %s, is unreachable, keeping what a "+
				"force-push or a deletion left for %s", strings.TrimPrefix(interval, "a "), share, least, grace),
			fmt.Sprintf("rather than %s on", grace),
			fmt.Sprintf("within %s of its deletion", grace),
			fmt.Sprintf("the object store for %s,", retired),
		},
		filepath.Join("concepts", "object-store.md"): {
			fmt.Sprintf("within `gitlet.PruneGrace` (%s)", grace),
			fmt.Sprintf("once more than a %s of the repository's live bytes and at least %s is unreachable",
				share, least),
		},
	} {
		path := filepath.Join(sourcetree.Root(t), "docs", page)
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

// mebibytes is n as the pages write a size of whole mebibytes (64 MiB).
func mebibytes(t *testing.T, n int64) string {
	t.Helper()
	if n <= 0 || n%(1<<20) != 0 {
		t.Fatalf("the pages write %d bytes in whole mebibytes: say it there, and here", n)
	}
	return fmt.Sprintf("%d MiB", n>>20)
}

// days is d as the pages write a grace in days (14 days).
func days(t *testing.T, d time.Duration) string {
	t.Helper()
	const day = 24 * time.Hour
	if d <= 0 || d%day != 0 {
		t.Fatalf("the pages write %v in whole days: say it there, and here", d)
	}
	return fmt.Sprintf("%d days", d/day)
}

// span is d as the pages spell a span in words (a day, a week, two weeks).
func span(t *testing.T, d time.Duration) string {
	t.Helper()
	const day = 24 * time.Hour
	words := map[time.Duration]string{day: "a day", 7 * day: "a week", 14 * day: "two weeks"}
	w, ok := words[d]
	if !ok {
		t.Fatalf("the pages spell %v in words: add it here as they write it", d)
	}
	return w
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
	words := map[int]string{2: "two", 4: "four", 5: "five", 6: "six", 8: "eight", 10: "ten", 16: "sixteen",
		20: "twenty"}
	w, ok := words[n]
	if !ok {
		t.Fatalf("the pages spell %d in words: add it here as they write it", n)
	}
	return w
}

// fractionWord is 1/n as the pages spell a share in words.
func fractionWord(t *testing.T, n int) string {
	t.Helper()
	words := map[int]string{2: "half", 4: "quarter", 8: "eighth", 16: "sixteenth", 32: "thirty-second",
		64: "sixty-fourth"}
	w, ok := words[n]
	if !ok {
		t.Fatalf("the pages spell a 1/%d share in words: add it here as they write it", n)
	}
	return w
}
