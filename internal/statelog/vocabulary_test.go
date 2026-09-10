package statelog_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestNoWithdrawnIdentifierSurvives fails the build when a name or a sentence
// this design retired is still written down anywhere in the repository.
//
// # Why an absence needs a test at all
//
// Every entry below was withdrawn from a design that was, at the time, partly
// implemented and thoroughly described — in code, in package docs, in the
// published documentation and in this file's own neighbours. What a
// withdrawal leaves behind is not a compile error. It is a paragraph that
// describes a mechanism nobody built, or a symbol nobody can find, and the
// next reader cannot tell it from a mechanism they simply have not located
// yet. That reader is often an agent, and the failure is worse than confusion:
// an accurate-sounding paragraph about a home partition is an instruction to
// build one.
//
// # What is on the list, and what is deliberately NOT
//
// The list is the names and phrases whose ONLY correct number of occurrences
// is zero. Four words that read like them are deliberately absent, and adding
// any of them would break working code:
//
//   - `partition` survives on its own, as the LexoRank shape partition and as
//     the batch loop's key partitioning. Only the compound forms are gone.
//   - `search_shard` is the surviving column, and is what a prefix grep for
//     the retired sharding vocabulary would otherwise catch.
//   - `scoped_through` survives unqualified; only its `_partition` sibling
//     left.
//   - `original_max_bytes` survives as a field of the capacity record, where
//     it is the value the pending classification compares against — deleting
//     it would delete the reconcile.
//
// # What it can and cannot see
//
// It is a text scan and it says so. A grep cannot see a PARAPHRASE: a
// paragraph that describes a home partition without using the words passes
// here. What keeps the retired designs from coming back is that there is one
// implementation of each surviving mechanism, not that this check will find
// every prose copy of a dead one. The list is a floor.
func TestNoWithdrawnIdentifierSurvives(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	// The matcher is exercised on strings whose verdict is known BEFORE it
	// is run over the tree. A guard asserting an absence passes identically
	// when the thing is absent and when the guard has gone inert.
	for _, positive := range []string{
		"tracker.partitions: 4",
		"the SearchPlan is computed from",
		"partitions_answered",
		"func partitionOf(project string) int",
		"tracker_task_home",
		"a task's home partition is its filed project",
		"this record is a cross-partition write",
		"FiledProject string",
		"ControlDrainWait = time.Second",
		"the projection_keys table",
		"coord.FamilyPages",
		"a frozen read is served from",
	} {
		if hits := matchWithdrawn(positive); len(hits) == 0 {
			t.Errorf("control: %q carries a withdrawn name and the matcher did "+
				"not flag it", positive)
		}
	}
	// THE FOUR NAMED NEAR-MISSES, plus the senses that legitimately
	// survive. Every one of these is working code or live prose, and a
	// matcher that flagged them would send somebody to delete it.
	for _, negative := range []string{
		"the shape partition a rank is minted under",
		"per-partition acking, and the two places a batch loop",
		"search_shard INTEGER NOT NULL",
		"scoped_through, which the applier writes",
		"original_max_bytes is what the pending classification compares",
		"the replacement record carries no bot user id",
		"a node's boot reconcile is O(keys)",
		"An item promotion marks its parent LAST",
		"LimitMarkerTTL: time.Minute",
		"tracker_rank_duplicates_cleared",
	} {
		if hits := matchWithdrawn(negative); len(hits) > 0 {
			t.Errorf("control: %q is live and the matcher flagged it on %v",
				negative, hits)
		}
	}

	// THE PREFILTER IS WHAT MAKES THIS CHECK AFFORDABLE, and a prefilter
	// that rejects a line its regex would have matched is a guard that has
	// silently stopped guarding. Every core must be non-empty, and short
	// enough to be a substring of what it filters for.
	for i, core := range cores() {
		if core == "" || len(core) < 4 {
			t.Fatalf("pattern %q reduced to the literal core %q — a core this "+
				"short filters nothing and a prefilter that admits every line "+
				"is the seventeen seconds this exists to avoid",
				patternOrder()[i], core)
		}
	}

	// AND THE SUPPRESSION PATH ITSELF, on inputs whose verdict is known.
	// The walk below only ever reaches it with real occurrences, so a
	// suppression that has swallowed everything would report a clean tree.
	if !offends("internal/somewhere/live.go", `partitionOf\b`) {
		t.Error("control: an unallowed occurrence is not reported as one, so " +
			"every hit in the tree is being suppressed and this guard reports " +
			"a clean repository whatever it finds")
	}
	if offends("internal/store/schema/node/0020_the_projection_lands.sql",
		`projection_keys`) {
		t.Error("control: the migration that created the projection's tables " +
			"is reported as a violation — an applied migration is history, " +
			"and editing one that already ran never re-runs it")
	}

	files, scanned := 0, 0
	var offences []string
	seen := map[withdrawalKey]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !scannedFile(path) {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		// THIS FILE IS THE LIST. Every withdrawn name appears here by
		// construction, so scanning it would report the inventory as the
		// inventory's own violation.
		if rel == listFile {
			return nil
		}
		files++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(body), "\n") {
			scanned++
			for _, hit := range matchWithdrawn(line) {
				seen[withdrawalKey{rel, hit}] = true
				if !offends(rel, hit) {
					continue
				}
				offences = append(offences, sprintOffence(rel, i+1, hit, line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if files == 0 || scanned == 0 {
		t.Fatalf("scanned %d files and %d lines — this guard was certifying "+
			"nothing", files, scanned)
	}

	sort.Strings(offences)
	for _, offence := range offences {
		t.Error(offence + "\n\tThis name or sentence was withdrawn. A paragraph " +
			"describing a mechanism nobody built reads exactly like one " +
			"describing a mechanism the reader has not found yet — and the next " +
			"reader is often an agent, for whom it is an instruction.")
	}

	// THE ALLOWANCE IS EXACT IN BOTH DIRECTIONS. An entry whose occurrence
	// has since been removed must be deleted, or the list becomes a place a
	// future occurrence can hide behind a stale excuse.
	for key, why := range allowedWithdrawal {
		if !seen[key] {
			t.Errorf("allowedWithdrawal still excuses %q in %s, but the walk no "+
				"longer finds it. Delete the entry — it was: %s",
				key.hit, key.file, why)
		}
	}
	t.Logf("scanned %d files / %d lines for %d withdrawn names",
		files, scanned, len(withdrawn))
}

// withdrawn is the list, with the reason each name is gone beside it.
//
// A pattern rather than a literal wherever a bare word would catch a live one:
// the compound forms of `partition` are gone and the word itself is not, so
// each is anchored to the compound that left.
var withdrawn = map[string]string{
	// The tracker's log was never partitioned: there is one mutation
	// stream, arbitrated per subject, so there is no partition count to
	// configure and nothing to route between.
	`tracker\.partitions`:      "the tracker's log is one stream, arbitrated per subject",
	`partition_linearizable`:   "a read level per partition, of a log that has one",
	`partitions_answered`:      "a fan-out over partitions; buckets_answered is the surviving name",
	`partitions_missing`:       "as above; buckets_missing survives",
	`partitionOf\b`:            "nothing maps an object to a partition",
	`home_partition`:           "an object has no home; every node holds the whole corpus",
	`tracker_task_home`:        "as above",
	`tracker_task_placement`:   "as above",
	`scoped_through_partition`: "scoped_through survives; its partition sibling does not",
	`\bhome partition\b`:       "prose for the same withdrawn idea",
	`\bhome stream\b`:          "prose for the same withdrawn idea",
	`\bmixed-home\b`:           "prose for the same withdrawn idea",
	`\bcross-partition\b`:      "there is one log, so no write crosses anything",

	// The search planner: withdrawn entirely, because every query
	// consults every bucket and there is nothing to plan.
	`SearchPlan`: "no query selects which part of the corpus to read",

	// Rank: the lattice mints from the value's own shape, with no counter
	// and no epoch beside it.
	`rank_epoch`: "the rank key carries its own magnitude",
	`rank_seq`:   "as above",

	// A source's bucket is a function of its identity, never of where it
	// is filed — the whole point of the shard being independent.
	`filed_project`: "a source's bucket does not follow its project",
	`FiledProject`:  "as above",

	// Withdrawn control-plane vocabulary.
	`ReadClosureRounds`: "a read is a barrier append, not a round of closure",
	`ControlDrainWait`:  "the capacity procedure has no drain wait",

	// The projection: deleted with the last family it served.
	`projection_keys`:    "the projection's key table is gone",
	`projection_cursor`:  "the projection's cursor table is gone",
	`coord\.Family`:      "coordination has no document families",
	`FamilyPages`:        "as above",
	`counter_high_water`: "the rank path reads no counter high-water mark",

	// Read levels: four, and `frozen` is not one of them.
	`\bfrozen\b\s+read|read\s+level\s+.?frozen`: "the level is consistent_prefix",
}

// listFile is this file, which cannot be its own violation.
const listFile = "internal/statelog/vocabulary_test.go"

// matchWithdrawn returns the withdrawn patterns one line carries.
//
// ONE COMBINED EXPRESSION over every pattern rather than a loop of matches:
// this runs on every line of the repository, and twenty-six separate scans of
// each was measured at half a minute where one pass is under a second.
func matchWithdrawn(line string) []string {
	// THE FAST PATH IS A SUBSTRING SCAN, not a regular expression. This
	// runs on every line of the repository — measured at 475 000 of them —
	// and a twenty-six-way case-insensitive alternation with word
	// boundaries costs about 36 µs per line, or seventeen seconds for the
	// tree. Every pattern below contains a literal core, so a lowercased
	// line that holds none of those cores cannot match any pattern, and
	// strings.Contains settles that in nanoseconds.
	lower := strings.ToLower(line)
	possible := false
	for _, core := range cores() {
		if strings.Contains(lower, core) {
			possible = true
			break
		}
	}
	if !possible {
		return nil
	}
	loc := combined().FindStringSubmatchIndex(line)
	if loc == nil {
		return nil
	}
	var hits []string
	for i, pattern := range patternOrder() {
		if loc[2*(i+1)] >= 0 {
			hits = append(hits, pattern)
		}
	}
	// A LINE MAY CARRY MORE THAN ONE, and the single pass above reports
	// only the leftmost alternation that matched. The rest are found by
	// re-running from just past it, which is bounded by the line.
	for at := loc[1]; at < len(line); {
		next := combined().FindStringSubmatchIndex(line[at:])
		if next == nil {
			break
		}
		for i, pattern := range patternOrder() {
			if next[2*(i+1)] >= 0 && !slices.Contains(hits, pattern) {
				hits = append(hits, pattern)
			}
		}
		if next[1] == 0 {
			break
		}
		at += next[1]
	}
	sort.Strings(hits)
	return hits
}

var (
	combinedRE    *regexp.Regexp
	combinedOrder []string
	combinedCores []string
)

// patternOrder is the withdrawn patterns in one stable order, which is what
// makes a submatch index mean a pattern.
func patternOrder() []string {
	build()
	return combinedOrder
}

func combined() *regexp.Regexp {
	build()
	return combinedRE
}

// cores are the literal substrings the patterns cannot match without.
//
// DERIVED from the patterns rather than typed out beside them, so a pattern
// added to the list is covered by adding nothing else — a hand-maintained
// second list is exactly how a prefilter starts rejecting lines its regex
// would have matched, which is a guard that has silently stopped guarding.
func cores() []string {
	build()
	return combinedCores
}

// literalCore is a pattern's longest run of characters that must appear
// literally, lowercased.
func literalCore(pattern string) string {
	plain := strings.NewReplacer(
		`\b`, "\x00", `\s+`, "\x00", `\.`, ".", `.?`, "\x00",
		"(", "\x00", ")", "\x00", "|", "\x00",
	).Replace(pattern)
	best := ""
	for _, run := range strings.Split(plain, "\x00") {
		if len(run) > len(best) {
			best = run
		}
	}
	return strings.ToLower(best)
}

func build() {
	if combinedRE != nil {
		return
	}
	combinedOrder = make([]string, 0, len(withdrawn))
	for pattern := range withdrawn {
		combinedOrder = append(combinedOrder, pattern)
	}
	sort.Strings(combinedOrder)
	parts := make([]string, 0, len(combinedOrder))
	for _, pattern := range combinedOrder {
		parts = append(parts, "("+pattern+")")
	}
	combinedRE = regexp.MustCompile("(?i)" + strings.Join(parts, "|"))
	combinedCores = make([]string, 0, len(combinedOrder))
	for _, pattern := range combinedOrder {
		combinedCores = append(combinedCores, literalCore(pattern))
	}
}

type withdrawalKey struct{ file, hit string }

// offends reports whether one occurrence is a violation rather than an
// allowed one.
//
// A FUNCTION rather than a condition inside the walk, so the controls above
// can exercise it directly. A guard whose suppression path is only ever
// reached with real input passes identically when nothing is wrong and when
// the suppression has swallowed everything — which is a check that has
// stopped checking and reports a clean tree while it does.
func offends(file, hit string) bool {
	return allowedWithdrawal[withdrawalKey{file, hit}] == ""
}

// allowedWithdrawal is where a withdrawn name legitimately still appears, with
// the reason. Every entry is a place the name is HISTORY rather than a live
// claim, and each says which.
var allowedWithdrawal = map[withdrawalKey]string{
	// AN APPLIED MIGRATION IS HISTORY, NOT SOURCE. schema_migrations keys
	// on the filename, so editing one that already ran silently never
	// re-runs it: every database that applied it keeps the old shape while
	// the code assumes the new one. These three name the projection's
	// tables because they created and dropped them.
	{"internal/store/schema/node/0020_the_projection_lands.sql", `projection_keys`}:      "the migration that created it",
	{"internal/store/schema/node/0020_the_projection_lands.sql", `projection_cursor`}:    "the migration that created it",
	{"internal/store/schema/node/0021_pages_normalised_titles.sql", `projection_keys`}:   "a migration that reshaped it",
	{"internal/store/schema/node/0021_pages_normalised_titles.sql", `projection_cursor`}: "a migration that reshaped it",
	{"internal/store/schema/node/0025_the_projection_leaves.sql", `projection_keys`}:     "the migration that dropped it",
	{"internal/store/schema/node/0025_the_projection_leaves.sql", `projection_cursor`}:   "the migration that dropped it",
}

func sprintOffence(file string, line int, hit, text string) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) > 120 {
		trimmed = trimmed[:120] + "…"
	}
	return file + ":" + itoa(line) + ": " + hit + "\n\t" + trimmed
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// skipDir names the trees this scan does not read.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "dist", "static":
		// static/ is a committed BUILD OUTPUT — never hand-edited, and
		// a minified bundle is not prose anybody reads.
		return true
	}
	return false
}

// scannedFile reports whether one path is prose or source this check reads.
func scannedFile(path string) bool {
	switch filepath.Ext(path) {
	case ".go", ".md", ".sql", ".yaml", ".yml", ".ts", ".tsx", ".json":
		return true
	}
	return false
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}
	return root
}
