package statelog_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
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
// is zero. Five words that read like them are deliberately absent, and adding
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
//   - `chunk` survives on its own in four live senses — a JetStream snapshot
//     arrives in chunks, a statelog snapshot transfer is sent in chunks, a
//     batched SQL statement binds a chunk of ids (`slices.Chunk`), and the
//     dashboard's bundle is split into chunks. Only the names the
//     content-addressed file store spelled are gone.
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
	root := sourcetree.Root(t)

	// The matcher is exercised on strings whose verdict is known BEFORE it
	// is run over the tree. A guard asserting an absence passes identically
	// when the thing is absent and when the guard has gone inert.
	for _, positive := range []string{
		"tracker.partitions: 4",
		"the SearchPlan is computed from",
		"partitions_answered",
		"tracker_task_home",
		"a task's home partition is its filed project",
		"this record is a cross-partition write",
		"FiledProject string",
		"ControlDrainWait = time.Second",
		"the projection_keys table",
		"coord.FamilyPages",
		"a frozen read is served from",
		"func (Domain) PartitionOf(env Envelope) (PartitionID, bool)",
		"the partitionOf helper",
		"statelog.EstateStream(tracker.Domain{})",
		"the estate map names the holders",
		"CREWLET_L1_TRACKER_TRACKER_007",
		"crewlet.l1.tracker.007.tracker.>",
		"map_epoch",
		"DELETE FROM tracker_file_chunks WHERE project = ?",
		"store.LockChunk(ctx, hash, owner)",
		"defer store.UnlockChunk(ctx, hash, owner)",
		"coord.ChunkLockTTL",
		"memory.SetChunkLockTTL(time.Second)",
		`"missing_chunks": 3`,
		"data, err := store.GetChunk(ctx, h)",
		"if len(chunks) > tracker.MaxFileChunks {",
		"tracker.FileChunkReferences",
		"objstore.Split(r, sink)",
		"make([]byte, objstore.ChunkSize)",
		"var m objstore.Manifest",
		"the collector takes the chunk's lock under the chunk locks bucket",
		"a chunk lock held by the writer",
		"crewlet_chunk_locks",
		"Crewlet seat, presence and object-store membership leases",
		"nothing said so until the object store's membership, a lease,",
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
		// Live senses of the same words: an owed wake partitioning a
		// task's notices, the cluster harness's partitioned broker, the
		// notification key, a struct field named for its state, and the
		// engine's own environment variables.
		"func TestAnOwedWakeIsItsOwnPartitionOfTheTask(t *testing.T)",
		"jetstreamtest.PartitionDB(t, i)",
		"Prompt.PartitionKey",
		"ExecuteState json.RawMessage",
		"CREWLET_LOG_LEVEL",
		"crewlet.log",
		// The fleet's BROKER membership is live: an operator reads and
		// changes it through `crewlet fleet`.
		"serve the fleet's broker membership",
		"the object store's collector",
		// The live senses of `chunk`: a snapshot's and a transfer's
		// pieces, a batched statement and the dashboard's bundle.
		"a JetStream snapshot arrives in chunks",
		"the transfer sends the snapshot in 1 MiB chunks",
		"for chunk := range slices.Chunk(ids, ScanBatch) {",
		"a chunk per workspace, and the next one fetched",
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
	if !offends("internal/somewhere/live.go", `home_partition`) {
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
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
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
//
// # Two designs divided the estate, and both are gone
//
// A withdrawn design once divided the tracker's log, and a later one divided
// the whole replicated estate into numbered partitions under a layout, each
// with its own file, its own logs and an estate map saying which node held
// which. Every data node holds the ONE estate whole now, every domain names
// its own stream, and nothing maps an object, a record or a log to a part of
// it — so both vocabularies are listed here, each with the reason it stays
// gone.
var withdrawn = map[string]string{
	// THE ESTATE IS ONE, held whole by every data node: there is no count
	// of parts to configure, and a field for one would describe a division
	// nothing performs.
	`tracker\.partitions`: "the estate is not divided, so there is no partition count",
	// The four read levels are per LOG, and every log is read whole.
	`partition_linearizable`: "the read levels are per log, and a log is not divided",
	`partitions_answered`:    "the search fan-out's coverage is buckets_answered",
	`partitions_missing`:     "as above; buckets_missing survives",
	// Nothing homes an object anywhere: every row of the estate is on
	// every data node.
	`home_partition`:           "an object has no home; every data node holds every row",
	`tracker_task_home`:        "as above",
	`tracker_task_placement`:   "as above",
	`scoped_through_partition`: "scoped_through survives; its partition sibling does not",
	`\bhome partition\b`:       "prose for the same withdrawn idea",
	`\bhome stream\b`:          "prose for the same withdrawn idea",
	`\bmixed-home\b`:           "prose for the same withdrawn idea",
	// A record is on its domain's one log, and the estate it lands in is
	// not divided, so there is no second part for a write to cross into.
	`\bcross-partition\b`: "the estate is one; there is nothing for a write to cross",

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

	// THE PARTITIONED ESTATE: a layout of numbered partitions, each a file
	// with one log per domain, placed on nodes by an estate map. Every data
	// node holds the one estate whole, and a domain declares its own
	// stream ([Domain.Stream]), so nothing names a part of it.
	`\bpartitionOf\b`:     "nothing maps an object or a record to a part of the estate",
	`ScopePartition`:      "a record's scope is checked against its own log, which is the domain's",
	`OnlyPartition`:       "a domain has one log, so there is no partition to look one up by",
	`LogShare`:            "a domain's log carries its whole budget; nothing divides it",
	`RunPartitioned`:      "the engine runs one estate; there is no partitioned mode",
	`wrong_partition`:     "a copy is out of service or it serves; it is never another part's",
	`EvictionKindRelease`: "a node never releases a log: every data node keeps the estate whole",
	`ReleaseLog`:          "as above",
	`PartitionHandle`:     "the replicated estate is one file, reached through ReplicatedHandle",
	`PartitionReader`:     "as above; ReplicatedReader",
	`OpenPartition`:       "as above; OpenReplicated",
	`PartitionFile`:       "as above; ReplicatedFile",
	`EstateLayout`:        "there is no layout; a domain declares its own stream",
	`EstateStream`:        "as above; a domain's stream is Domain.Stream()",
	`LayoutZero`:          "as above",
	`DefaultLayoutOne`:    "as above",
	`\bestate map\b`:      "every data node holds the estate, so nothing records who holds what",
	`map_epoch`:           "as above",
	`CREWLET_L[0-9]`:      "a state log's stream is its domain's own name, never a layout's",
	`crewlet\.l[0-9]`:     "as above, for its subjects",

	// THE CONTENT-ADDRESSED FILE STORE: files cut into 1 MiB chunks named
	// by their SHA-256, a manifest listing them on the file's row, a table
	// of chunk rows beside it, and a coordination lock per chunk around a
	// deletion and a re-put. Every upload is ONE object under a key minted
	// for it and never reused now, so nothing is shared, nothing is locked
	// and a row names one object. What survives is the chunk table's
	// migration history, allowed by name below.
	`tracker_file_chunks`: "a file row names one object; migration 0038 dropped the chunk table",
	`chunk_locks`:         "no build opens or deletes the lock bucket",
	`LockChunk`:           "a deletion takes no lock (ADR-0027)",
	`UnlockChunk`:         "as above",
	`ChunkLockTTL`:        "as above; there is no lock to age",
	`missing_chunks`:      "the audit names missing and damaged FILES (missing_files)",
	`GetChunk`:            "a read streams one object through Store.Open",
	`MaxFileChunks`:       "a file is one object; MaxFileBytes is its only bound",
	`FileChunkReferences`: "the declaration is FileObjectReferences",
	`objstore\.Split\b`:   "the backend streams the whole upload; nothing cuts it",
	`objstore\.ChunkSize`: "as above; an object has no chunk size",
	`objstore\.Manifest`:  "a row names one objstore.Object, not a list of chunks",
	`\bchunk lock`:        "a deletion takes no lock (ADR-0027)",

	// THE PLACED OBJECT STORE: object bytes on chosen data nodes, a lease
	// per member saying which nodes kept them, and a repair duty that read
	// that membership. The store is one the fleet shares now (natsobj or
	// s3obj) and keeps its own copies, so no node claims a membership of
	// it and the seat lease bucket holds seats and presence alone. Two
	// literal spellings rather than one pattern with a class, because the
	// prefilter reads each pattern's literal core and a bracket in it is a
	// core no line holds — a guard that silently matches nothing.
	`\bobject-store membership`:   "no node is a member of the object store; it is one shared store",
	`\bobject store's membership`: "as above",
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
		`\b`, "\x00", `\s+`, "\x00", `\.`, ".", `.?`, "\x00", `[0-9]`, "\x00",
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

	// THE CHUNK TABLE'S MIGRATIONS, for the same reason: the three that
	// created and reshaped it and the one that dropped it, plus the test
	// that carries a database holding chunk rows through that drop.
	{"internal/store/schema/replicated/0031_a_project_keeps_files.sql", `tracker_file_chunks`}:            "the migration that created it",
	{"internal/store/schema/replicated/0032_a_chunk_row_names_its_slot.sql", `tracker_file_chunks`}:       "a migration that reshaped it",
	{"internal/store/schema/replicated/0036_a_chunk_row_names_only_its_chunk.sql", `tracker_file_chunks`}: "a migration that reshaped it",
	{"internal/store/schema/replicated/0038_a_file_row_names_one_object.sql", `tracker_file_chunks`}:      "the migration that dropped it",
	{"internal/store/fileobject_test.go", `tracker_file_chunks`}:                                          "the test of the migration that dropped it",
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

// skipDir names the trees this scan does not read, beyond what
// [sourcetree.Walk] never enters in any scan.
func skipDir(name string) bool {
	switch name {
	case "dist", "static":
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
