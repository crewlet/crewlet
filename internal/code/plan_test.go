package code

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// EVERY CODE INDEX SERVES A REGISTERED QUERY, AND EVERY REGISTERED QUERY SEEKS
// THE KEY IT IS WRITTEN FOR.
//
// A trailing comment naming an index's reader is a CLAIM; the planner is the
// only thing that knows whether the statement reaches the index. So this runs
// EXPLAIN QUERY PLAN over every statement the package issues against its
// tables — the guards, the applier's reads and writes (the tombstones a push
// sweeps and the ref drops it forgets past their grace, each oldest first,
// among them, and the fences it reads: an eviction's, the activation's), the
// lifecycle store's decides, the push decide's reads, a change's lifecycle's
// decide, apply and answer, the live changes a push record moves and the
// authors its pushers already are of them, every walk a listing of changes
// takes and one change's reads, the reader's pages in
// their first and keyset forms — a snapshot's retired phase and its layers
// phase among them — and its bounded counts, the purge's bounded count, its
// parts and its statements over a repository's changes and its published
// chain, a sweep's decide, apply and listing over the retired packs, a
// compaction's decide, apply and answer, a publication's reads of the chain
// and the live packs and its replacement of the chain's top, the compactor's
// and the publisher's due listings, the trim's listing of who was evicted from
// the log, and every statement the object store builds over code_packs,
// code_packs_retired and code_layer_parts, at every width its callers build
// it at ([planShapes]) — and holds each to the EXACT seek it names: the index
// and every column of it the statement bounds. Seeking an index is not
// enough — a lookup of one ref that sought repo_id alone read every ref its
// repository holds (planner.go). The statements are the package's own
// constants: there is no copy here to drift from what runs, and
// TestEveryCodeReadSeeksANamedIndex holds the set to the package's source and
// refuses a plan that reads any table by a scan, so this holds only what that
// one cannot — which index, which columns, in which order. An index no
// statement reads yet is held to the read its migration claims for it
// ([claimedReads]), until a statement does.
//
// NO ANALYZE, because no deployment runs one: statistics choose differently,
// and a fixture that gathered them certified plans no node ever ran.
//
// The reads bounded by what a push row WEIGHS are held to a COVERING seek:
// they read the widths code_pushes_repo_idx carries, so weighing a wide row
// never reads its overflow pages — and the purge's reads count a change's
// body from its key's index alone, a covering seek too, weighing it by the
// change's own narrow row. And every statement reads in the order of the
// index it seeks — a plan that sorts what it found reads all of it before its
// LIMIT takes a row — but those [sortingStatements] names, over a set
// something else bounds.
//
// Mutation: drop `INDEXED BY` from liveRefSQL and the head seeks
// code_refs_changed_idx on repo_id alone; test `ref IN (SELECT value FROM
// json_each(?))` in refsNamedSQL and it seeks repo_id alone; drop `position >=
// ?` from refsDeltaAfterSQL and the page seeks repo_id alone; drop row_bytes
// from code_pushes_repo_idx and the purge's and the horizon's reads stop
// covering; order the push listing by `at` again, which replicated 0056 moved
// out of code_pushes_repo_idx's key, and it sorts every push of the
// repository; order the purge's walk of a repository's changes by `c.number,
// r.n` and it sorts every revision the repository holds; write a claim's live
// states in another order and it seeks code_changes_state_idx; lead
// code_ref_drops' key with the position again (replicated 0061 moved the
// instant ahead of it) and the drops' expiry and a purge part sort what they
// seek rather than read it in order; key code_changes_work_idx by the item
// alone again (replicated 0069 ordered it) and a work item's listing sorts
// every change the item ever carried.
func TestEveryCodeIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	db := seededPlanEstate(t)
	// EVERY TABLE A RECORD REPRODUCES, from the inventory rather than a
	// list of this test's own, so an index on a table a later change adds
	// is held from the change that adds it. The log's machinery tables are
	// not among them: their indexes serve the framework's statements, and
	// this package spells none of those.
	covered := ReproducibleTables
	// held plans one statement and answers the indexes its plan reaches —
	// whether it reads a table by a scan is the read gate's to judge, of a
	// claim as of a registered statement (TestEveryCodeReadSeeksANamedIndex).
	held := func(t *testing.T, name string, st statement) []string {
		plan := explain(t, db, st.sql, st.args)
		if len(st.seeks) == 0 {
			t.Fatalf("%s names no seek — every registered statement is held to one", name)
		}
		for _, seek := range st.seeks {
			if !slices.ContainsFunc(plan, func(line string) bool { return strings.Contains(line, seek) }) {
				t.Errorf("%s does not seek %q:\n%s", name, seek, strings.Join(plan, "\n"))
			}
		}
		// AND THE ORDER IS THE SEEK'S: a statement whose ORDER BY the index
		// it seeks does not serve SORTS every row the seek finds before its
		// LIMIT takes one — a push listing still ordered by the instant
		// replicated 0056 moved out of code_pushes_repo_idx's key would read
		// every push of the repository, its 181 days, on every page.
		// Two-sided: a statement declared to sort that no longer does loses
		// the declaration.
		sorts := slices.ContainsFunc(plan, func(line string) bool {
			return strings.Contains(line, "USE SORTER") || strings.Contains(line, "TEMP B-TREE")
		})
		why, declared := sortingStatements[name]
		switch {
		case sorts && !declared:
			t.Errorf("%s sorts what it seeks rather than reading it in its index's order:\n%s",
				name, strings.Join(plan, "\n"))
		case !sorts && declared:
			t.Errorf("%s is declared to sort (%s) and its plan reads in its index's order: "+
				"drop the declaration:\n%s", name, why, strings.Join(plan, "\n"))
		}
		return indexesIn(plan)
	}
	used := map[string]bool{}
	for name, st := range everyStatement(t) {
		t.Run(name, func(t *testing.T) {
			for _, index := range held(t, name, st) {
				used[index] = true
			}
		})
	}
	claimed := map[string]bool{}
	for index, st := range claimedReads() {
		name := claimName(index)
		t.Run(name, func(t *testing.T) {
			if !slices.Contains(held(t, name, st), index) {
				t.Errorf("%s's claimed read reaches another index: the read its comment names "+
					"does not use it", index)
			}
			claimed[index] = true
		})
	}
	for _, index := range indexesOn(t, db, covered) {
		switch {
		case used[index] && claimed[index]:
			t.Errorf("%s is read by a registered statement and still carries a claimed read — "+
				"the statement holds the index now: drop the claim from claimedReads", index)
		case !used[index] && !claimed[index]:
			t.Errorf("no registered statement's plan reaches %s — an index is a write cost "+
				"on every push on every node, so one no plan uses is deleted", index)
		}
	}
}

// THE RANGES A ROUND ASKS ABOUT ARE READ OVER THE LIVE REFS ALONE.
//
// The push decide asks, under every create, whether a live ref lies in a
// range of code_refs' key ([gapsHoldingSQL], then [firstLiveInSQL] for a range
// that holds one) — and a range under a create holds the tombstones under it
// too. Read over the key, the seek stepped over each tombstone, a lookup of its
// row apiece, before it reached a live ref or the range's end, and a tombstone
// was stepped over once for every create above it: one push of the 118 nested
// creates of the deepest chain over a repository's [MaxTombstonesPerRepo]
// tombstones held its round 7.7–9.9 s (BenchmarkDecideABatch's buried arm).
// So each statement reads code_refs through [liveRefsKey] and through nothing
// else, and that index is PARTIAL on the live refs — the half the plan cannot
// show, since a rebuild of the index without its predicate would plan the
// same seek and step over every tombstone again.
//
// Mutation: name [refsKey] in either statement, or declare the index without
// its predicate on the empty target, and this fails.
func TestTheRefConflictProbesStepOverNoTombstone(t *testing.T) {
	t.Parallel()
	db := seededPlanEstate(t)
	gaps := `[["refs/heads/r/","refs/heads/r0"],["refs/heads/r000001/","refs/heads/r0000010"]]`
	for name, probe := range map[string]string{"gaps holding": gapsHoldingSQL, "first live in": firstLiveInSQL} {
		plan := explain(t, db, probe, []any{"repo-1", gaps})
		reads := 0
		for _, line := range plan {
			if !strings.Contains(line, "code_refs") {
				continue
			}
			reads++
			if !strings.Contains(line, "INDEX "+liveRefsKey+" (repo_id=? AND ref>? AND ref<?)") {
				t.Errorf("%s reads code_refs other than by a range of the live refs' index:\n%s",
					name, strings.Join(plan, "\n"))
			}
		}
		if reads == 0 {
			t.Errorf("%s's plan names no read of code_refs:\n%s", name, strings.Join(plan, "\n"))
		}
	}

	var declared string
	if err := db.Read(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(),
			`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, liveRefsKey).Scan(&declared)
	}); err != nil {
		t.Fatalf("read %s's declaration: %v", liveRefsKey, err)
	}
	const want = "CREATE INDEX code_refs_live_idx ON code_refs (repo_id, ref) WHERE target != ''"
	if got := strings.Join(strings.Fields(declared), " "); got != want {
		t.Fatalf("%s is declared\n\t%s\nwant\n\t%s\n— the live refs alone, keyed as the probes seek them",
			liveRefsKey, got, want)
	}
}

// THE LIVE INDEXES HOLD THE LIVE STATES, SPELLED AS THEIR READERS SPELL THEM.
// code_changes_open_idx and code_changes_target_idx are each declared partial
// on [liveChanges] exactly, and [liveChanges] lists every [ChangeState.Live]
// state, in [ChangeStates]' order, and no other — so a state added to the set
// is a decision about the two indexes, and a statement that repeats the
// constant reaches them, since the pinned planner matches the list as it is
// written (planner.go): a statement listing the states in another order, or
// naming one of them, reads code_changes_state_idx or the table instead.
//
// Mutation: declare either index on `state IN ('open', 'draft')`, or make a
// closed change live, and this fails.
func TestTheLiveIndexesHoldTheLiveStates(t *testing.T) {
	t.Parallel()
	var live []string
	for _, s := range ChangeStates() {
		if s.Live() {
			live = append(live, "'"+string(s)+"'")
		}
	}
	if want := "state IN (" + strings.Join(live, ", ") + ")"; liveChanges != want {
		t.Fatalf("liveChanges is %q, and the live states in ChangeStates' order spell %q", liveChanges, want)
	}
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	for _, index := range []string{"code_changes_open_idx", "code_changes_target_idx"} {
		var declared string
		if err := db.Read(t.Context(), func(tx *sql.Tx) error {
			return tx.QueryRowContext(t.Context(),
				`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, index).Scan(&declared)
		}); err != nil {
			t.Fatalf("read %s's declaration: %v", index, err)
		}
		if got := strings.Join(strings.Fields(declared), " "); !strings.HasSuffix(got, " WHERE "+liveChanges) {
			t.Errorf("%s is declared\n\t%s\nand its readers repeat\n\tWHERE %s\n— a predicate they do "+
				"not spell is one the planner never matches", index, got, liveChanges)
		}
	}
}

// statement is one registered statement, arguments that make it plannable,
// and the seeks its plan must contain — each an index and the columns of it the
// statement bounds, as the plan prints them, or for a table [scanOK] lets a
// statement read whole, the scan.
type statement struct {
	sql   string
	args  []any
	seeks []string
}

// shape is a statement a BUILDER assembles at run time, which no constant
// holds whole, as the builder spells it for one input: planned and held to
// its seeks like any registered statement.
type shape struct {
	statement
	// builder is what assembles it: one of this package's functions, named as
	// the read gate names the function a statement is spelled in
	// ("Reader.page", "page"), or a statement builder of
	// objstore.ReferenceTable over one of [referenceTables]
	// ("objstore.ReferenceTable.ObjectsAmong").
	builder string
}

// everyStatement is every registered statement and every shape, by name —
// what the plan tests explain — refusing a name the two registries share.
func everyStatement(t *testing.T) map[string]statement {
	t.Helper()
	out := registeredStatements()
	for name, sh := range planShapes() {
		if _, taken := out[name]; taken {
			t.Fatalf("%q names a registered statement and a shape: one name, one statement", name)
		}
		out[name] = sh.statement
	}
	return out
}

// sortingStatements is every registered statement whose plan may SORT what it
// seeks, by name, and why: a sort is allowed only over a set something other
// than a repository's size bounds. Every other statement reads in the order
// of the index it seeks.
var sortingStatements = map[string]string{
	"protections of": "one listing page's rules, at most MaxReposPerPage of MaxProtectionsPerRepo " +
		"each: a seek a repository of the page, whose union no one index orders",
}

// The PRIMARY KEY indexes the plans name, as the driver names them.
const (
	repoKey       = "sqlite_autoindex_code_repos_1"
	nameKey       = "sqlite_autoindex_code_names_1"
	pushKey       = "sqlite_autoindex_code_pushes_1"
	protectionKey = "sqlite_autoindex_code_protections_1"
	pathOwnerKey  = "sqlite_autoindex_code_path_owners_1"
	deletionKey   = "sqlite_autoindex_code_deletions_1"
	changeKey     = "sqlite_autoindex_code_changes_1"
	authorKey     = "sqlite_autoindex_code_change_authors_1"
	revisionKey   = "sqlite_autoindex_code_revisions_1"
	bodyKey       = "sqlite_autoindex_code_change_bodies_1"
	// A change's conversation's keys.
	conversationKey = "sqlite_autoindex_code_conversations_1"
	reviewerKey     = "sqlite_autoindex_code_reviewers_1"
	participantKey  = "sqlite_autoindex_code_participants_1"
	commentKey      = "sqlite_autoindex_code_comments_1"
	findingKey      = "sqlite_autoindex_code_findings_1"
	findingTextKey  = "sqlite_autoindex_code_finding_texts_1"
	activationKey   = "sqlite_autoindex_code_activation_1"
	evictionKey     = "sqlite_autoindex_code_evictions_1"
	layersKey       = "sqlite_autoindex_code_layers_1"
	layerPartsKey   = "sqlite_autoindex_code_layer_parts_1"
	seatDaysKey     = "sqlite_autoindex_code_seat_days_1"
	repoKindsKey    = "sqlite_autoindex_code_repo_kinds_1"
)

// registeredStatements is every statement this package spells against its
// tables that reads one, by name — TestEveryCodeReadSeeksANamedIndex holds
// the set to the package's source both ways, so a statement lands here or
// fails there.
func registeredStatements() map[string]statement {
	keys := `["refs/heads/r000001","refs/heads/r000002"]`
	// THE SWEEP'S DELETE AT ITS LARGEST: every tombstone a record's whole
	// row budget may remove, which is still one seek a ref.
	stones := make([]string, MaxRecordRows)
	for i := range stones {
		stones[i] = fmt.Sprintf("%q", fmt.Sprintf("refs/heads/r%06d", i))
	}
	sweep := "[" + strings.Join(stones, ",") + "]"
	ids := `["repo-1-push-000001","repo-1-push-000002"]`
	changeIDs := `["repo-1-change-000001","repo-1-change-000002"]`
	// A COMPACTION'S INPUTS AT THEIR MOST, which is still one seek a pack.
	inputs := make([]string, MaxCompactInputs)
	for i := range inputs {
		inputs[i] = fmt.Sprintf("%q", fmt.Sprintf("pack-%06d", i))
	}
	compactInputs := "[" + strings.Join(inputs, ",") + "]"
	out := map[string]statement{
		"guard repo":          {guardRepoSQL, []any{"repo-1", "repo-1"}, []string{deletionKey + " (repo_id=?)", repoKey + " (id=?)"}},
		"guard name":          {guardNameSQL, []any{"token"}, []string{nameKey + " (token=?)"}},
		"marker":              {markerSQL, []any{"repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		"purged":              {purgedSQL, []any{"repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		"repo version":        {repoVersionSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"settings":            {settingsSQL, []any{"config", "unit", "ENG", "company", "main", "", "active", 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"clear protections":   {clearProtectionsSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"path owners":         {pathOwnersSQL, []any{"repo-1"}, []string{pathOwnerKey + " (repo_id=?)"}},
		"path owners of":      {pathOwnersOfSQL, []any{`["repo-1","repo-2"]`}, []string{pathOwnerKey + " (repo_id=?)"}},
		"clear path owners":   {clearPathOwnersSQL, []any{"repo-1"}, []string{pathOwnerKey + " (repo_id=?)"}},
		"delete repo":         {deleteRepoSQL, []any{1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"restore repo":        {restoreRepoSQL, []any{"config", "unit", "ENG", "company", "main", "", "active", 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"pushes applied":      {pushesAppliedSQL, []any{ids}, []string{pushKey + " (push_id=?)"}},
		"refs named":          {refsNamedSQL, []any{"repo-1", keys}, []string{"SCAN json_each AS j", refsKey + " (repo_id=? AND ref=?)"}},
		"push counters":       {pushCountersSQL, []any{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"tombstones held":     {tombstonesHeldSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"expired pushes":      {expiredPushesSQL, []any{"repo-1", HorizonDeletesPerRecord}, []string{"COVERING INDEX code_pushes_repo_idx (repo_id=?)"}},
		"delete pushes":       {deletePushesSQL, []any{ids}, []string{pushKey + " (push_id=?)"}},
		"oldest tombstones":   {tombstonesOldestSQL, []any{"repo-1", MaxRecordRows}, []string{"code_refs_tombstones_idx (repo_id=?)"}},
		"delete refs":         {deleteRefsSQL, []any{"repo-1", sweep}, []string{refsKey + " (repo_id=? AND ref=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"swept tombstones":    {sweptTombstonesSQL, []any{1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"purge parts":         {purgePartsSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"purge pushes":        {purgePushesSQL, []any{"repo-1", PurgePartRows}, []string{"COVERING INDEX code_pushes_repo_idx (repo_id=?)"}},
		"purge refs":          {purgeRefsSQL, []any{"repo-1", PurgePartRows}, []string{refsKey + " (repo_id=?)"}},
		"purge packs":         {purgePacksSQL, []any{"repo-1", PurgePartRows}, []string{packsKey + " (repo_id=?)"}},
		"delete packs":        {deletePacksSQL, []any{"repo-1", `["p1"]`}, []string{packsKey + " (repo_id=? AND pack_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge counters":      {purgeCountersSQL, []any{1, 1, 1, 1, 1, 1, 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"release name":        {releaseNameSQL, []any{"token", "repo-1"}, []string{nameKey + " (token=?)"}},
		"purge all pushes":    {purgeAllPushesSQL, []any{"repo-1"}, []string{"code_pushes_repo_idx (repo_id=?)"}},
		"purge all refs":      {purgeAllRefsSQL, []any{"repo-1"}, []string{refsKey + " (repo_id=?)"}},
		"purge all packs":     {purgeAllPacksSQL, []any{"repo-1"}, []string{packsKey + " (repo_id=?)"}},
		"remove repo":         {removeRepoSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"repo name":           {repoNameSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"remaining":           {remainingSQL, []any{"repo-1", PurgePartRows + 1}, []string{"COVERING INDEX code_pushes_repo_idx (repo_id=?)", refsKey + " (repo_id=?)", packsKey + " (repo_id=?)", retiredKey + " (repo_id=?)", dropsKey + " (repo_id=?)", changesNumberKey + " (repo_id=?)", revisionKey + " (change_id=?)", authorKey + " (change_id=?)", "COVERING INDEX " + bodyKey + " (change_id=?)", reviewerKey + " (change_id=?)", participantKey + " (change_id=?)", "COVERING INDEX code_findings_raiser_idx (change_id=?)", "COVERING INDEX code_comments_thread_idx (change_id=?)", "COVERING INDEX " + findingTextKey + " (change_id=?)", "COVERING INDEX code_reviews_change_idx (change_id=?)", conversationKey + " (change_id=?)"}},
		"claim":               {claimSQL, []any{"token"}, []string{nameKey + " (token=?)"}},
		"claims":              {claimsSQL, []any{`["token","other"]`}, []string{nameKey + " (token=?)"}},
		"repos rows":          {reposRowsSQL, []any{`["repo-1","repo-2"]`}, []string{repoKey + " (id=?)"}},
		"repo row":            {repoRowSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"protections":         {protectionsSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"read back":           {readBackSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"repo view":           {repoViewSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"repo grant":          {repoGrantSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"packs full first":    {packsFullFirstSQL, []any{"repo-1", 100}, []string{packsKey + " (repo_id=?)"}},
		"packs full after":    {packsFullAfterSQL, []any{"repo-1", "p", "p", "pack", 100}, []string{packsKey + " (repo_id=? AND pack_id>=?)"}},
		"packs added first":   {packsAddedFirstSQL, []any{"repo-1", 1, 100}, []string{"code_packs_added_idx (repo_id=? AND added_position>?)"}},
		"packs added after":   {packsAddedAfterSQL, []any{"repo-1", 1, 1, "p", "p", "pack", 100}, []string{"code_packs_added_idx (repo_id=? AND added_position>=?)"}},
		"refs full first":     {refsFullFirstSQL, []any{"repo-1", 100}, []string{refsKey + " (repo_id=?)"}},
		"refs full after":     {refsFullAfterSQL, []any{"repo-1", "refs/heads/r000100", 100}, []string{refsKey + " (repo_id=? AND ref>?)"}},
		"refs delta first":    {refsDeltaFirstSQL, []any{"repo-1", 400, 100}, []string{"code_refs_changed_idx (repo_id=? AND position>?)"}},
		"refs delta after":    {refsDeltaAfterSQL, []any{"repo-1", 400, 400, "refs/heads/r", 100}, []string{"code_refs_changed_idx (repo_id=? AND position>=?)"}},
		"delta rows":          {deltaRowsSQL, []any{"repo-1", 400, 551}, []string{"COVERING INDEX code_refs_changed_idx (repo_id=? AND position>?)", "COVERING INDEX code_packs_added_idx (repo_id=? AND added_position>?)"}},
		"retired above":       {retiredAboveSQL, []any{"repo-1", 1005, 551}, []string{"COVERING INDEX " + retiredRepoIdx + " (repo_id=? AND retired_position>?)"}},
		"retired first":       {retiredFirstSQL, []any{"repo-1", 1005, 1015, 100}, []string{retiredRepoIdx + " (repo_id=? AND retired_position>? AND retired_position<=?)"}},
		"retired after":       {retiredAfterSQL, []any{"repo-1", 1005, 1015, 1005, "retired-000050", "retired-000050", "pack", 100}, []string{retiredRepoIdx + " (repo_id=? AND retired_position>=? AND retired_position<=?)"}},
		"push":                {pushSQL, []any{"repo-1-push-000001"}, []string{pushKey + " (push_id=?)"}},
		"pushes first":        {pushesFirstSQL, []any{"repo-1", 50}, []string{"code_pushes_repo_idx (repo_id=?)"}},
		"pushes after":        {pushesAfterSQL, []any{"repo-1", 100, 100, "p", 50}, []string{"code_pushes_repo_idx (repo_id=? AND position<=?)"}},
		"pushes above":        {pushesAboveSQL, []any{"repo-1", 10, 100, 100}, []string{"code_pushes_repo_idx (repo_id=? AND position>? AND position<=?)"}},
		"pushes above after":  {pushesAboveAfterSQL, []any{"repo-1", 10, 100, 10, "p", 100}, []string{"code_pushes_repo_idx (repo_id=? AND position>=? AND position<=?)"}},
		"pushes below":        {pushesBelowSQL, []any{"repo-1", 100, 100}, []string{"code_pushes_repo_idx (repo_id=? AND position<?)"}},
		"repos":               {reposSQL, []any{`["active","archived"]`, "", 100}, []string{"code_repos_name_idx (name>?)", refsKey + " (repo_id=? AND ref=?)"}},
		"protections of":      {protectionsOfSQL, []any{`["repo-1","repo-2"]`}, []string{protectionKey + " (repo_id=?)"}},
		"live ref":            {liveRefSQL, []any{"repo-1", "refs/heads/main"}, []string{refsKey + " (repo_id=? AND ref=?)"}},
		"refs under":          {refsUnderSQL, []any{"repo-1", "refs/heads/", "refs/heads0", 200}, []string{"INDEX " + liveRefsKey + " (repo_id=? AND ref>? AND ref<?)"}},
		"lives":               {livesSQL, []any{"", 1000}, []string{"code_repos_name_idx (name>?)"}},
		"due purging":         {duePurgingSQL, []any{-1, -1, "", 100}, []string{"code_repos_purging_idx (deleted_at>=?)"}},
		"due deleted":         {dueDeletedSQL, []any{100, -1, -1, "", 100}, []string{"code_repos_deleted_idx (deleted_at>=? AND deleted_at<=?)"}},
		"purge state":         {purgeStateSQL, []any{"repo-1", "repo-1"}, []string{repoKey + " (id=?)", deletionKey + " (repo_id=?)"}},
		"count a gate reject": {countRejectSQL, []any{1, "repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		"decide repo":         {decideRepoSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"decide deletion":     {decideDeletionSQL, []any{"repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		// A SEAT'S DAY IS ONE ROW, read by its key for every seat a round
		// names, and its oldest day beyond its two newest forgotten by the
		// same key, read newest first — never a count over the push history,
		// and never a sort of the seat's days.
		"seat days": {seatDaysSQL, []any{"2031-04-02", `["seat-1","seat-2"]`},
			[]string{"SCAN json_each AS j", seatDaysKey + " (seat=? AND day=?)"}},
		"forget seat days": {forgetSeatDaysSQL, []any{`["seat-1","seat-2"]`},
			[]string{"SCAN json_each AS j", seatDaysKey + " (seat=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		// A CODING RUN'S PUSHES TO ONE REPOSITORY, above its launch in log
		// order: the repository's pushes by position, each row tested for
		// the run — which no index carries (replicated 0070) — never a sort.
		"run pushes first": {runPushesFirstSQL, []any{"repo-1", "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", 10, 50},
			[]string{"code_pushes_repo_idx (repo_id=? AND position>?)"}},
		"run pushes after": {runPushesAfterSQL, []any{"repo-1", "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", 10, 10, "p", 50},
			[]string{"code_pushes_repo_idx (repo_id=? AND position>=?)"}},
		"pushes named": {pushesNamedSQL, []any{ids}, []string{pushKey + " (push_id=?)"}},
		// THE LIFECYCLE'S DECIDES AND APPLIES, each one row by its key — a
		// name's claim with its redirect, a repository's row, the count a
		// kind's repositories keep — but a seat's scratch count, a seek on
		// its owner of at most code.scratch.max_per_seat entries, never a
		// count over every repository.
		"claim redirect":   {claimRedirectSQL, []any{"token"}, []string{nameKey + " (token=?)"}},
		"scratch count":    {scratchCountSQL, []any{"dev"}, []string{"COVERING INDEX code_repos_scratch_idx (owner=?)"}},
		"kind count":       {kindCountSQL, []any{"project"}, []string{repoKindsKey + " (kind=?)"}},
		"uncount a kind":   {uncountKindSQL, []any{"project"}, []string{repoKindsKey + " (kind=?)"}},
		"repo kind":        {repoKindSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"rename row":       {renameRowSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"keep name":        {keepNameSQL, []any{"token", "repo-1"}, []string{nameKey + " (token=?)"}},
		"redirect name":    {redirectNameSQL, []any{1, "token", "repo-1"}, []string{nameKey + " (token=?)"}},
		"rename repo":      {renameRepoSQL, []any{"web", 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"release redirect": {releaseRedirectSQL, []any{"token", "repo-1", 1}, []string{nameKey + " (token=?)"}},
		"hold":             {holdSQL, []any{1, "repo-1"}, []string{repoKey + " (id=?)"}},
		// THE RETENTION'S TWO NEW LISTINGS, keyset pages over their partial
		// indexes in the index's order: the redirects whose window ended,
		// and the active scratch repositories idle past the cutoff.
		"redirects due": {redirectsDueSQL, []any{100, -1, -1, "", retentionPage},
			[]string{"code_names_redirect_idx (redirect_until>=? AND redirect_until<=?)"}},
		"idle scratch": {idleScratchSQL, []any{100, -1, -1, "", retentionPage},
			[]string{"code_repos_idle_idx (last_activity_at>=? AND last_activity_at<=?)"}},
		"gaps holding": {gapsHoldingSQL, []any{"repo-1", `[["refs/heads/r/","refs/heads/r0"],["refs/heads/r000001/","refs/heads/r0000010"]]`},
			[]string{"SCAN json_each AS j", liveRefsKey + " (repo_id=? AND ref>? AND ref<?)"}},
		"first live in": {firstLiveInSQL, []any{"repo-1", `[["refs/heads/r/","refs/heads/r0"],["refs/heads/r000001/","refs/heads/r0000010"]]`},
			[]string{"SCAN json_each AS j", liveRefsKey + " (repo_id=? AND ref>? AND ref<?)"}},
		"retired oldest":    {retiredOldestSQL, []any{"repo-1", RetireSweepRows + 1}, []string{retiredRepoIdx + " (repo_id=?)"}},
		"sweep retired":     {sweepRetiredSQL, []any{"repo-1", 1010, RetireSweepRows}, []string{retiredRepoIdx + " (repo_id=? AND retired_position<=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"swept retired":     {sweptRetiredSQL, []any{1010, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"retired due":       {retiredDueSQL, []any{15, -1, -1, "", retentionPage}, []string{"code_packs_retired_due_idx (retired_at>=? AND retired_at<=?)"}},
		"purge retired":     {purgeRetiredSQL, []any{"repo-1", PurgePartRows}, []string{retiredRepoIdx + " (repo_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all retired": {purgeAllRetiredSQL, []any{"repo-1"}, []string{retiredRepoIdx + " (repo_id=?)"}},
		// THE DROPS' EXPIRY AT ITS LARGEST LIMIT, a whole record's budget,
		// past a horizon half the seeded drops are older than.
		"expire drops":    {expireDropsSQL, []any{"repo-1", 100, MaxRecordRows}, []string{"COVERING INDEX " + dropsKey + " (repo_id=? AND at<?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge drops":     {purgeDropsSQL, []any{"repo-1", PurgePartRows}, []string{"COVERING INDEX " + dropsKey + " (repo_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all drops": {purgeAllDropsSQL, []any{"repo-1"}, []string{dropsKey + " (repo_id=?)"}},
		// A PURGE'S CHANGES: the walk in number order, each change's
		// dependents counted by its key — its body from the key's index
		// alone, never the body's row, whose overflow pages a read would
		// walk; the whole changes' rows by the list of their ids; the cut
		// change's in its key's order; and the final purge's, the dependents
		// through their changes.
		"purge change walk": {purgeChangeWalkSQL, []any{"repo-1", PurgePartRows},
			[]string{changesNumberKey + " (repo_id=?)", revisionKey + " (change_id=?)", authorKey + " (change_id=?)",
				reviewerKey + " (change_id=?)", participantKey + " (change_id=?)",
				"COVERING INDEX code_findings_raiser_idx (change_id=?)",
				"COVERING INDEX code_comments_thread_idx (change_id=?)",
				"COVERING INDEX " + findingTextKey + " (change_id=?)",
				"COVERING INDEX code_reviews_change_idx (change_id=?)", "COVERING INDEX " + bodyKey + " (change_id=?)",
				conversationKey + " (change_id=?)"}},
		"purge changes' reviewers": {purgeChangeReviewersSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", reviewerKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' participants": {purgeChangeParticipantsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", participantKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' comments": {purgeChangeCommentsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", "code_comments_thread_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' findings": {purgeChangeFindingsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", "code_findings_raiser_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' finding texts": {purgeChangeFindingTextsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", findingTextKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' reviews": {purgeChangeReviewsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", "code_reviews_change_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' conversations": {purgeChangeConversationsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", conversationKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge a cut change's reviewers": {purgeCutReviewersSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{reviewerKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge a cut change's participants": {purgeCutParticipantsSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{participantKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge a cut change's findings": {purgeCutFindingsSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{"code_findings_raiser_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"weigh a cut change's finding texts": {purgeCutFindingTextWidthsSQL, []any{"repo-1-change-000001",
			PurgePartRows}, []string{findingTextKey + " (change_id=?)"}},
		"purge a cut change's finding texts": {purgeCutFindingTextsSQL, []any{"repo-1-change-000001", 3},
			[]string{findingTextKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"weigh a cut change's reviews": {purgeCutReviewWidthsSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{"code_reviews_change_idx (change_id=?)"}},
		"purge a cut change's reviews": {purgeCutReviewsSQL, []any{"repo-1-change-000001", 3},
			[]string{"code_reviews_change_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"weigh a cut change's comments": {purgeCutCommentWidthsSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{"code_comments_thread_idx (change_id=?)"}},
		"purge a cut change's comments": {purgeCutCommentsSQL, []any{"repo-1-change-000001", 3},
			[]string{"code_comments_thread_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"weigh a cut conversation again": {unweighConversationSQL, []any{1024, "repo-1-change-000001"},
			[]string{conversationKey + " (change_id=?)"}},
		"purge all reviewers": {purgeAllReviewersSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", reviewerKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all participants": {purgeAllParticipantsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", participantKey + " (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all comments": {purgeAllCommentsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", "code_comments_thread_idx (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all findings": {purgeAllFindingsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", "code_findings_raiser_idx (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all finding texts": {purgeAllFindingTextsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", findingTextKey + " (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all reviews": {purgeAllReviewsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", "code_reviews_change_idx (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all conversations": {purgeAllConversationsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", conversationKey + " (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' revisions": {purgeChangeRevisionsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", revisionKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' authors": {purgeChangeAuthorsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", authorKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' bodies": {purgeChangeBodiesSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", bodyKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' rows": {purgeChangeRowsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", changeKey + " (id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge a cut change's revisions": {purgeCutRevisionsSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{revisionKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge a cut change's authors": {purgeCutAuthorsSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{authorKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all revisions": {purgeAllRevisionsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", revisionKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all authors": {purgeAllAuthorsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", authorKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all bodies": {purgeAllBodiesSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", bodyKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all changes": {purgeAllChangesSQL, []any{"repo-1"},
			[]string{"COVERING INDEX " + changesNumberKey + " (repo_id=?)"}},
		// A CHANGE'S LIFECYCLE: a change by its key — a decide's, an
		// apply's and an answer's — and its row written back by the same
		// key; the live change a source branch heads, by the index partial
		// on the live states, its predicate as the index spells it; a
		// change's body read and removed by its key; and the repository's
		// next number and version by its own.
		"change row": {changeRowSQL, []any{"repo-1-change-000001"}, []string{changeKey + " (id=?)"}},
		"write change": {writeChangeSQL, []any{"a change", 10, "open", "refs/heads/main", oid("head-1-4"), 4,
			"", "", "", "", nil, "", nil, "", "", 4, 3000, "repo-1-change-000002"},
			[]string{changeKey + " (id=?)"}},
		"live change on": {liveChangeOnSQL, []any{"repo-1", "refs/heads/f000001"},
			[]string{"code_changes_open_idx (repo_id=? AND source_ref=?)"}},
		// THE LIVE CHANGES A PUSH RECORD MOVES — a decide's and an apply's —
		// driven from the list of its branches, and from the list of the
		// targets and heads its commands set: one seek of an index partial on
		// the live states an element; and the authors its pushers already are
		// of them, driven from the list of the pairs, one seek of the
		// authors' key a pair.
		"live changes by source": {liveBySourceSQL, []any{"repo-1", `["refs/heads/f000001","refs/heads/f000002"]`},
			[]string{"SCAN json_each AS j", "code_changes_open_idx (repo_id=? AND source_ref=?)"}},
		"live changes by target": {liveByTargetSQL, []any{"repo-1",
			`[["refs/heads/main","` + oid("head-1-3") + `"],["refs/heads/main","` + oid("head-2-3") + `"]]`},
			[]string{"SCAN json_each AS j", "code_changes_target_idx (repo_id=? AND target_ref=? AND head_oid=?)"}},
		"held authors": {heldAuthorsSQL,
			[]any{`[["repo-1-change-000001","seat-0"],["repo-1-change-000002","seat-2"]]`},
			[]string{"SCAN json_each AS j", authorKey + " (change_id=? AND author=?)"}},
		// A CHANGE'S CONVERSATION: its row, a comment and a thread's root by
		// their keys — a decide's, an apply's and an answer's; the authors and
		// the reviewers a write names, driven from the list of their handles,
		// one seek of the key a handle; how many reviewers a change asks,
		// counted over the reviewers' key; the change's repository, which the
		// apply holds a record to; a withdrawn ask and a resolved thread
		// written back by their keys.
		"conversation":        {conversationSQL, []any{"repo-1-change-000001"}, []string{conversationKey + " (change_id=?)"}},
		"comment":             {commentSQL, []any{"comment-1"}, []string{commentKey + " (id=?)"}},
		"change's repository": {changeRepoSQL, []any{"repo-1-change-000001"}, []string{changeKey + " (id=?)"}},
		"authors among": {authorsAmongSQL, []any{"repo-1-change-000001", `["seat-0","seat-2"]`},
			[]string{"SCAN json_each AS j", authorKey + " (change_id=? AND author=?)"}},
		"reviewers named": {reviewersNamedSQL, []any{"repo-1-change-000001", `["seat-0","seat-2"]`},
			[]string{"SCAN json_each AS j", reviewerKey + " (change_id=? AND reviewer=?)"}},
		"reviewers asked": {reviewersAskedSQL, []any{"repo-1-change-000001", MaxReviewersPerChange + 1},
			[]string{reviewerKey + " (change_id=?)"}},
		"withdraw reviewer": {withdrawReviewerSQL, []any{string(ReviewerWithdrawn), 3000, "repo-1-change-000001",
			"seat-0"}, []string{reviewerKey + " (change_id=? AND reviewer=?)"}},
		"resolve thread": {resolveThreadSQL, []any{true, "seat-0", "agent", "comment-1", "repo-1-change-000001"},
			[]string{commentKey + " (id=?)"}},
		// A CHANGE'S REVIEWS AND FINDINGS: a finding by its key — a decide's
		// and an answer's; a raiser's standing findings, driven from the list
		// of the standing states, one seek of the raiser's index a state; a
		// reviewer's standing verdict, by their participant row's key; and a
		// review's supersession, driven from the list of the findings' ids,
		// one seek of the key an id.
		"finding": {findingSQL, []any{"finding-1"}, []string{findingKey + " (id=?)"}},
		"answer finding": {answerFindingSQL, []any{string(FindingResolved), "", "seat-0", "agent", "", 3000,
			"finding-1"}, []string{findingKey + " (id=?)"}},
		"standing findings": {standingFindingsSQL, []any{"repo-1-change-000001", "seat-0", "agent", standingStates},
			[]string{"SCAN json_each AS j", "code_findings_raiser_idx (change_id=? AND raiser=? AND state=?)"}},
		"standing verdict": {standingVerdictSQL, []any{"repo-1-change-000001", "seat-0"},
			[]string{participantKey + " (change_id=? AND handle=?)"}},
		"supersede findings": {supersedeFindingsSQL, []any{string(FindingSuperseded), "seat-0", "agent", 3000,
			`["finding-1","finding-2"]`}, []string{"SCAN json_each AS j", findingKey + " (id=?)",
			"INTEGER PRIMARY KEY (rowid=?)"}},
		"change body":        {changeBodySQL, []any{"repo-1-change-000002"}, []string{bodyKey + " (change_id=?)"}},
		"delete change body": {deleteBodySQL, []any{"repo-1-change-000002"}, []string{bodyKey + " (change_id=?)"}},
		"next change":        {nextChangeSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"opened":             {openedSQL, []any{3000, "repo-1"}, []string{repoKey + " (id=?)"}},
		"touched":            {touchedSQL, []any{3000, "repo-1"}, []string{repoKey + " (id=?)"}},
		// THE READS OF CHANGES: each walk a listing's filters choose
		// (changeread.go's walkOf), a first page and a page after a cursor,
		// newest first in its index's own order — a repository's by
		// number, every other by the opening record's position and the id —
		// one change by its number, its authors and its revisions by their
		// keys, and the repositories a page across repositories names,
		// driven from their list.
		"changes of a repository": {changesOfRepoSQL, []any{"repo-1", 50},
			[]string{changesNumberKey + " (repo_id=?)"}},
		"changes of a repository after": {changesOfRepoAfterSQL, []any{"repo-1", 30, 50},
			[]string{changesNumberKey + " (repo_id=? AND number<?)"}},
		"changes of a repository in a state": {changesInRepoStateSQL, []any{"repo-1", "open", 50},
			[]string{changesRepoStateKey + " (repo_id=? AND state=?)"}},
		"changes of a repository in a state after": {changesInRepoStateAfterSQL, []any{"repo-1", "open", 30, 50},
			[]string{changesRepoStateKey + " (repo_id=? AND state=? AND number<?)"}},
		"changes in a state": {changesInStateSQL, []any{"open", 50},
			[]string{changesStateKey + " (state=?)"}},
		"changes in a state after": {changesInStateAfterSQL,
			[]any{"open", 2030, 2030, "repo-1-change-000031", 50},
			[]string{changesStateKey + " (state=? AND opened_position<=?)"}},
		"changes of an owner": {changesOfOwnerSQL, []any{"seat-1", "open", 50},
			[]string{changesOwnerKey + " (owner=? AND state=?)"}},
		"changes of an owner after": {changesOfOwnerAfterSQL,
			[]any{"seat-1", "open", 2030, 2030, "repo-1-change-000031", 50},
			[]string{changesOwnerKey + " (owner=? AND state=? AND opened_position<=?)"}},
		"changes of a work item": {changesOfWorkSQL, []any{"native", "work-3", 50},
			[]string{changesWorkKey + " (work_backend=? AND work_id=?)"}},
		"changes of a work item after": {changesOfWorkAfterSQL,
			[]any{"native", "work-3", 2030, 2030, "repo-1-change-000004", 50},
			[]string{changesWorkKey + " (work_backend=? AND work_id=? AND opened_position<=?)"}},
		"change by number": {changeByNumberSQL, []any{"repo-1", 3},
			[]string{changesNumberKey + " (repo_id=? AND number=?)"}},
		"change authors": {changeAuthorsSQL, []any{"repo-1-change-000001"}, []string{authorKey + " (change_id=?)"}},
		"revisions": {revisionsSQL, []any{"repo-1-change-000001", 200},
			[]string{revisionKey + " (change_id=?)"}},
		"revisions after": {revisionsAfterSQL, []any{"repo-1-change-000001", 2, 200},
			[]string{revisionKey + " (change_id=? AND n<?)"}},
		"listed repos": {listedReposSQL, []any{`["repo-1","repo-2"]`}, []string{repoKey + " (id=?)"}},
		// THE READS OF A CHANGE'S CONVERSATION (conversationread.go): a
		// reviewer's standing asks newest first, a first page and a page
		// after a cursor, and the changes they name driven from their list;
		// which of a page's changes await their owner's fix, and which of
		// those ask a reviewer's review, each driven from the page's list,
		// one seek of a key a change; one change's counters, its reviewers,
		// its comments, its reviews and its findings by their keys, each a
		// first page and a page after a cursor — the comments' >= beside the
		// OR keeping the seek past the thread, and the findings' texts' key
		// walked, each finding sought by its own.
		"changes asked": {changesAskedSQL, []any{"seat-1", 50},
			[]string{reviewerAsksKey + " (reviewer=? AND state=?)"}},
		"changes asked after": {changesAskedAfterSQL, []any{"seat-1", "repo-1-change-000031", 50},
			[]string{reviewerAsksKey + " (reviewer=? AND state=? AND change_id<?)"}},
		"changes by id": {changesByIDSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{changeKey + " (id=?)"}},
		"changes needing a fix": {needingFixSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", conversationKey + " (change_id=?)"}},
		"changes asking a reviewer": {askedAmongSQL,
			[]any{`["repo-1-change-000001","repo-1-change-000002"]`, "seat-0"},
			[]string{"SCAN json_each AS j", reviewerKey + " (change_id=? AND reviewer=?)"}},
		"conversation counts": {conversationCountsSQL, []any{"repo-1-change-000001"},
			[]string{conversationKey + " (change_id=?)"}},
		"reviews of a change": {reviewsOfSQL, []any{"repo-1-change-000001", 50},
			[]string{"code_reviews_change_idx (change_id=?)"}},
		"reviews of a change after": {reviewsOfAfterSQL, []any{"repo-1-change-000001", 3000, 50},
			[]string{"code_reviews_change_idx (change_id=? AND position>?)"}},
		"findings of a change": {findingsOfSQL, []any{"repo-1-change-000001", 50},
			[]string{findingTextKey + " (change_id=?)", findingKey + " (id=?)"}},
		"findings of a change after": {findingsOfAfterSQL, []any{"repo-1-change-000001", "finding-1", 50},
			[]string{findingTextKey + " (change_id=? AND finding_id>?)", findingKey + " (id=?)"}},
		"reviewers of a change": {reviewersOfSQL, []any{"repo-1-change-000001", 200},
			[]string{reviewerKey + " (change_id=?)"}},
		"reviewers of a change after": {reviewersOfAfterSQL, []any{"repo-1-change-000001", "seat-0", 200},
			[]string{reviewerKey + " (change_id=? AND reviewer>?)"}},
		"comments of a change": {commentsOfSQL, []any{"repo-1-change-000001", 50},
			[]string{"code_comments_thread_idx (change_id=?)"}},
		"comments of a change after": {commentsOfAfterSQL, []any{"repo-1-change-000001", "comment-1", "comment-1",
			3000, 50}, []string{"code_comments_thread_idx (change_id=? AND thread_id>=?)"}},
		// THE FENCES the applier and a writer read — a node's eviction
		// window and the readmission that closes one, and the activation
		// fence every config-managed record is held to — each a point read
		// of its key.
		"eviction":      {evictionSQL, []any{"node-1"}, []string{evictionKey + " (node_id=?)"}},
		"readmit":       {readmitSQL, []any{1, 1, "node-1", 1}, []string{evictionKey + " (node_id=?)"}},
		"guard company": {guardCompanySQL, []any{activationID}, []string{activationKey + " (id=?)"}},
		"fence":         {fenceSQL, []any{activationID}, []string{activationKey + " (id=?)"}},
		// THE TRIM'S LISTING of who was evicted from the log: every row, in
		// its key's order — the one scan scanOK allows, bounded by the
		// nodes the fleet has evicted rather than by anything a repository
		// holds.
		"evictions": {evictionsSQL, nil, []string{"SCAN code_evictions USING INDEX " + evictionKey}},
		// A COMPACTION'S: the rows of the packs a list names, a decide's
		// inputs, an apply's inputs and outputs and an answer's outputs, at
		// the most inputs one compaction names; the retired rows of the
		// outputs an answer did not find live; the counters its apply
		// moves; and the compactor's due listing, most live packs first.
		"named packs":          {namedPacksSQL, []any{"repo-1", compactInputs}, []string{"SCAN json_each AS j", packsKey + " (repo_id=? AND pack_id=?)"}},
		"retired added":        {retiredAddedSQL, []any{"repo-1", `["retired-000001","retired-000002"]`}, []string{"SCAN json_each AS j", retiredKey + " (repo_id=? AND pack_id=?)"}},
		"compact counters":     {compactCountersSQL, []any{1, 1, 1, 1, 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"compaction due first": {compactionDueFirstSQL, []any{CompactMinMerge, MaxSnapshotItems}, []string{"code_repos_packs_idx (live_packs>=?)"}},
		"compaction due after": {compactionDueAfterSQL, []any{CompactMinMerge, 30, 30, "repo-2", MaxSnapshotItems}, []string{"code_repos_packs_idx (live_packs<=? AND live_packs>=?)"}},
		// A PUBLICATION'S: the chain whole and the live packs, each by the
		// key — the decide's, the apply's guard and recount, an answer's
		// and a snapshot's — the replaced top by the key's range above its
		// base and its parts by theirs, driven from the list of the layers'
		// ids, the counters with the recount, the final purge's two, a
		// snapshot's parts, and the publisher's due listing, least recently
		// published first.
		"layers of":             {layersOfSQL, []any{"repo-1"}, []string{layersKey + " (repo_id=?)"}},
		"live packs":            {livePacksSQL, []any{"repo-1"}, []string{packsKey + " (repo_id=?)"}},
		"delete layers above":   {deleteLayersAboveSQL, []any{"repo-1", 1}, []string{layersKey + " (repo_id=? AND seq>?)"}},
		"delete layer parts":    {deleteLayerPartsSQL, []any{"repo-1", `["layer-000001","layer-000002"]`}, []string{"SCAN json_each AS j", layerPartsKey + " (repo_id=? AND layer_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"publish counters":      {publishCountersSQL, []any{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"purge all layer parts": {purgeAllLayerPartsSQL, []any{"repo-1"}, []string{layerPartsKey + " (repo_id=?)"}},
		"purge all layers":      {purgeAllLayersSQL, []any{"repo-1"}, []string{layersKey + " (repo_id=?)"}},
		"layer parts of":        {layerPartsOfSQL, []any{"repo-1"}, []string{layerPartsKey + " (repo_id=?)"}},
		"publication due first": {publicationDueFirstSQL, []any{MaxSnapshotItems}, []string{"code_repos_unpublished_idx (published_at>=?)"}},
		"publication due after": {publicationDueAfterSQL, []any{30, 30, "repo-2", MaxSnapshotItems}, []string{"code_repos_unpublished_idx (published_at>=?)"}},
		// AN EXPUNGE'S: a commit's read of the repository's dropping packs
		// by the key's prefix; the applies' read of the repository's
		// expunge and their counters by its key; whether a pack of the
		// repository dates its content from before a request, over the
		// packs' content index from the repository's start, never reading a
		// row; and the maintainer's listing of the expunges owed, by id
		// after a cursor.
		"dropping packs":       {droppingPacksSQL, []any{"repo-1"}, []string{packsKey + " (repo_id=?)"}},
		"expunge state":        {expungeStateSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"request pending":      {requestPendingSQL, []any{"repo-1", 25}, []string{"COVERING INDEX code_packs_content_idx (repo_id=? AND content_position<?)"}},
		"request":              {requestSQL, []any{"r", 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"begin counters":       {beginCountersSQL, []any{1, 1, 1, 1, 1, 1, 1, "x", 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"begin dropped layers": {beginDroppedLayersSQL, []any{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, "x", 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"commit counters":      {commitCountersSQL, []any{1, 1, 1, 1, 1, 1, "", 0, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"expunge due":          {expungeDueSQL, []any{"", MaxSnapshotItems}, []string{"code_repos_expunging_idx (id>?)"}},
	}
	return out
}

// referenceBuilders is every statement objstore.ReferenceTable builds over a
// declaration, by its method's name — what the collector, its audit, a
// backup and a store move read a declared table's rows by. The read gate
// holds the list to the type's methods both ways, so a builder objstore adds
// is one this package's shapes answer for.
var referenceBuilders = map[string]func(objstore.ReferenceTable, int) (string, error){
	"ObjectsAmong":         objstore.ReferenceTable.ObjectsAmong,
	"ReferencesAfter":      objstore.ReferenceTable.ReferencesAfter,
	"ReferencesAmong":      objstore.ReferenceTable.ReferencesAmong,
	"DeferredRecordsAfter": objstore.ReferenceTable.DeferredRecordsAfter,
}

// referenceTables is every objstore.ReferenceTable this package declares, by
// the variable that holds it — what objstore builds statements over. The
// read gate holds the list to the package's source both ways.
var referenceTables = map[string]objstore.ReferenceTable{
	"PackReferences":        PackReferences,
	"RetiredPackReferences": RetiredPackReferences,
	"LayerReferences":       LayerReferences,
}

// referenceWidths is every width each of objstore's statement builders is
// built at, by the builder's name: the widest, from its callers' own
// constant, and for a question of a list of keys the one key a short last
// batch narrows it to. The read gate holds the list to [referenceBuilders]
// both ways.
//
// EACH WIDTH IS A STATEMENT TEXT OF ITS OWN — n placeholders, a LIMIT of n —
// which the driver plans on its own, so a shape planned at a width no caller
// builds certifies a statement nothing runs. Between a list's two ends the
// plan is a seek of the key's index a key apiece, a cost that grows with the
// list while a scan's does not, so the widest is where a planner would first
// trade the seeks for a scan of the table; and one key is the form the
// planner may read as a plain equality.
var referenceWidths = map[string][]int{
	// The collector's question of a batch of the keys a listing named
	// (collect's Referenced): a full batch, and the short last one.
	"ObjectsAmong": {1, collect.JudgeBatch},
	// A page of a walk — the audit's, a backup's, a store move's — whose
	// LIMIT is always the whole page.
	"ReferencesAfter": {objstore.WalkPage},
	// A backup's question of the keys the records past its copy name, a
	// walk's page of them at a time: a full page, and the short last one.
	"ReferencesAmong": {1, objstore.WalkPage},
	// A backup's page of the records its copy's appliers deferred: the page
	// the framework walks the same table in, which the backup takes as its
	// own.
	"DeferredRecordsAfter": {statelog.ReprocessPage},
}

// referenceShape names the shape of objstore's builder over a table at a
// width.
func referenceShape(builder, table string, width int) string {
	return fmt.Sprintf("%s %s at %d", builder, table, width)
}

// planShapes is every statement a builder assembles at run time, by name:
// the statements no constant here spells whole, registered by what the
// builder spells. Today they are objstore's, built over this package's
// declarations — so whether each finds its rows by an index is this
// package's schema's to answer: the collector's batch question of every
// table; of the live one the walk the audit, a backup and a store move read,
// a backup's question of the keys its log names and its walk of the records
// that log deferred; and of the derived one a store move's walk. A retired
// declaration's standing refuses the rest of them, and a derived one's the
// backup's two. One shape for each builder each declaration's standing
// admits, at each width its callers build it at ([referenceWidths]), keyed
// by [referenceShape].
func planShapes() map[string]shape {
	out := map[string]shape{}
	reference := func(builder string, table objstore.ReferenceTable, args func(width int) []any,
		seeks ...string) {
		for _, width := range referenceWidths[builder] {
			text, err := referenceBuilders[builder](table, width)
			if err != nil {
				panic(fmt.Sprintf("objstore builds no %s over %s at %d: %v", builder, table.Table, width, err))
			}
			out[referenceShape(builder, table.Table, width)] = shape{statement{text, args(width), seeks},
				objstoreBuilder + builder}
		}
	}
	// A KEY FOR EACH PLACEHOLDER of a question of a list of keys.
	keys := func(width int) []any {
		among := make([]any, width)
		for i := range among {
			among[i] = fmt.Sprintf("repo-1-object-%06d", i)
		}
		return among
	}
	// A PAGE'S ONE ARGUMENT, where the page before ended, whatever its LIMIT.
	after := func(from any) func(int) []any { return func(int) []any { return []any{from} } }
	reference("ObjectsAmong", PackReferences, keys, "code_packs_object_idx (object=?)")
	reference("ReferencesAfter", PackReferences, after(""), "code_packs_object_idx (object>?)")
	reference("ReferencesAmong", PackReferences, keys, "code_packs_object_idx (object=?)")
	// A PAGE OF THE DEFERRED RECORDS, a seek of the table's INTEGER PRIMARY
	// KEY — which the driver prints as rowid=? whatever the comparison: its
	// `position > ?` here and a `rowid = ?` print alike.
	reference("DeferredRecordsAfter", PackReferences, after(0),
		"SEARCH code_log_deferred USING INTEGER PRIMARY KEY (rowid=?)")
	// THE COLLECTOR'S ONE QUESTION OF THE RETIRED TABLE — the audit's and a
	// backup's walk, and a backup's question, are refused for a retired
	// declaration, so this is all the object store asks of it.
	reference("ObjectsAmong", RetiredPackReferences, keys, "code_packs_retired_object_idx (object=?)")
	// THE DERIVED TABLE'S TWO: the collector's question, and a store move's
	// walk — the one pass that walks a derived table, which the audit and a
	// backup never do.
	reference("ObjectsAmong", LayerReferences, keys, "code_layer_parts_object_idx (object=?)")
	reference("ReferencesAfter", LayerReferences, after(""), "code_layer_parts_object_idx (object>?)")
	return out
}

// claimedReads is every index on a covered table that no statement this
// package issues reads YET, by name, with the read its migration's `-- <reader>`
// comment claims for it — planned here, so the claim is the planner's from the
// migration that declares the index rather than from whichever change first
// issues the read. A change's indexes land with its tables (replicated 0058),
// one migration for the rows rather than one an index, and their reads — the
// lifecycle's, the listings', a commit's page — are each a change's of its own:
// the listings' are registered now (changeread.go), and a commit's page's are
// still claims.
//
// TWO-SIDED, as every allowance here is: a claim whose index a registered
// statement reaches fails, because the change that issues the read registers
// its own statement and a claim beside it would be a second copy of what runs;
// and an index that neither a statement nor a claim reaches fails as it always
// has. Each read is written in the claim's own terms — the live states as
// [liveChanges] spells them, a listing newest first by the order its index
// keeps — and is held to the exact seek and no sort, like any registered
// statement, and judged by the read gate as the statement that will issue it
// will be: no table read by a scan. The gate's match against the source never
// sees a claim, which no code issues yet.
func claimedReads() map[string]statement {
	return map[string]statement{
		"code_changes_merge_idx": {`SELECT id FROM code_changes WHERE repo_id = ? AND merge_oid = ?
			AND merge_oid != ''`, []any{"repo-1", oid("head-3-3")},
			[]string{"code_changes_merge_idx (repo_id=? AND merge_oid=?)"}},
		"code_revisions_head_idx": {`SELECT change_id, n FROM code_revisions WHERE head_oid = ?`,
			[]any{oid("head-1-1")}, []string{"code_revisions_head_idx (head_oid=?)"}},
		// THE WEEKLY EXPUNGE'S LISTING, least recently expunged first over
		// the repositories an expunge may be due in, its partial predicate
		// repeated verbatim.
		"code_repos_expunge_due_idx": {`SELECT id, name FROM code_repos INDEXED BY code_repos_expunge_due_idx
			WHERE status IN ('active', 'archived') AND expunged_at <= ? ORDER BY expunged_at, id LIMIT ?`,
			[]any{2, MaxSnapshotItems}, []string{"code_repos_expunge_due_idx (expunged_at<=?)"}},
	}
}

// claimName is what the plan tests call the read claimed for an index.
func claimName(index string) string { return "the read claimed for " + index }

// retiredRepoIdx is code_packs_retired_repo_idx, (repo_id, retired_position,
// pack_id, part): the order every statement over a repository's retired rows
// names, so a sweep and a purge remove them alike on every node.
const retiredRepoIdx = "code_packs_retired_repo_idx"

// seededPlanEstate is an estate holding five repositories of 500 refs, 50
// packs, 200 pushes, 200 retired packs, 200 ref drops, 50 changes — forty of
// them with a body — and a published chain of three layers of two parts
// each, and fifty seats' push counts of three days each, with no statistics
// gathered — the state every node's estate is in.
func seededPlanEstate(t *testing.T) store.ReplicatedHandle {
	t.Helper()
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	ctx := context.Background()
	vars := db.Caps().MaxVariables
	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		for r := range 5 {
			id := fmt.Sprintf("repo-%d", r)
			// ONE OF EACH STATUS the retention pass lists, so both of its
			// partial indexes hold a row.
			status := map[int]RepoStatus{3: RepoStatusDeleted, 4: RepoStatusPurging}[r]
			if status == "" {
				status = RepoStatusActive
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO code_repos (id, name, status, managed_by,
				owner_kind, owner, default_branch, created_at, deleted_at) VALUES (?, ?, ?,
				'config', 'unit', 'ENG', 'main', 0, ?)`, id, id, status,
				sql.NullInt64{Int64: int64(r), Valid: r >= 3}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO code_names (token, name, repo_id)
				VALUES (?, ?, ?)`, NameToken(id), id, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO code_protections (repo_id, pattern,
				direct_push) VALUES (?, 'refs/heads/main', 'persons')`, id); err != nil {
				return err
			}
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_refs (repo_id, ref, target, position, at) VALUES `, `(?, ?, ?, ?, ?)`, ``,
				500, func(i int) []any {
					target := fmt.Sprintf("%040d", i)
					if i%50 == 0 {
						target = ""
					}
					return []any{id, fmt.Sprintf("refs/heads/r%06d", i), target, i, i}
				}); err != nil {
				return err
			}
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
					reverse_bytes, git_checksum, objects, kind, state, added_position) VALUES `,
				`(?, ?, 'pack', ?, 'h', 1, 0, 0, 'c', 1, 'push', 'live', ?)`, ``, 50,
				func(i int) []any {
					return []any{id, fmt.Sprintf("pack-%06d", i), fmt.Sprintf("%s-object-%06d", id, i), i}
				}); err != nil {
				return err
			}
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, via, commands,
					received_at, row_bytes) VALUES `, `(?, ?, ?, ?, 'op', 'operator', 'git', '[]', 0, 2)`, ``, 200,
				func(i int) []any { return []any{fmt.Sprintf("%s-push-%06d", id, i), id, i, i} }); err != nil {
				return err
			}
			// TWO HUNDRED RETIRED PACKS ACROSS TWENTY RETIRING RECORDS, each
			// record's rows sharing its position and its instant, as a
			// record's retirements do.
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_packs_retired (repo_id, pack_id, part, object, hash, size, index_bytes,
					reverse_bytes, git_checksum, objects, kind, added_position, retired_position,
					retired_at) VALUES `,
				`(?, ?, 'pack', ?, 'h', 1, 0, 0, 'c', 1, 'push', ?, ?, ?)`, ``, 200,
				func(i int) []any {
					return []any{id, fmt.Sprintf("retired-%06d", i),
						fmt.Sprintf("%s-retired-object-%06d", id, i), i, 1000 + i/10, i / 10}
				}); err != nil {
				return err
			}
			// TWO HUNDRED REF DROPS ACROSS FORTY RECORDS, each record's
			// sharing its position and its instant, as a record's drops do.
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_ref_drops (repo_id, position, ref, old, at) VALUES `,
				`(?, ?, ?, ?, ?)`, ``, 200,
				func(i int) []any {
					return []any{id, 2000 + i/5, fmt.Sprintf("refs/heads/r%06d", i), fmt.Sprintf("%040d", i),
						(i / 5) * 5}
				}); err != nil {
				return err
			}
			// FIFTY CHANGES ACROSS EVERY STATE, each of three revisions and
			// two authors, a work item on every third, every merged one
			// naming the commit it was merged at, and a body on all but
			// every fifth — which has none, and so no body row.
			states := ChangeStates()
			body := strings.Repeat("what this change does, and why. ", 32)
			var bodied []int
			for i := range 50 {
				if i%5 != 0 {
					bodied = append(bodied, i)
				}
			}
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref,
					target_ref, head_oid, revision, owner, owner_kind, work_backend, work_id, opened_at,
					opened_position, closed_at, merged_at, merged_via, merge_oid, version, position) VALUES `,
				`(?, ?, ?, 'a change', ?, ?, ?, 'refs/heads/main', ?, 3, ?, 'agent', ?, ?, ?, ?, ?, ?, ?, ?, 3, ?)`,
				``, 50, func(i int) []any {
					state := states[i%len(states)]
					var backend, work, via, merge string
					if i%3 == 0 {
						backend, work = "native", fmt.Sprintf("work-%d", i)
					}
					closed := sql.NullInt64{Int64: int64(i), Valid: state == ChangeStateClosed}
					merged := sql.NullInt64{Int64: int64(i), Valid: state == ChangeStateMerged}
					if merged.Valid {
						via, merge = string(MergedViaPush), oid(fmt.Sprintf("head-%d-3", i))
					}
					weighs := 0
					if slices.Contains(bodied, i) {
						weighs = len(body)
					}
					return []any{fmt.Sprintf("%s-change-%06d", id, i+1), id, i + 1, weighs, string(state),
						fmt.Sprintf("refs/heads/f%06d", i), oid(fmt.Sprintf("head-%d-3", i)),
						fmt.Sprintf("seat-%d", i%7), backend, work, i, 2000 + i, closed, merged, via, merge,
						2000 + i}
				}); err != nil {
				return err
			}
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_change_bodies (change_id, body) VALUES `, `(?, ?)`, ``, len(bodied),
				func(i int) []any { return []any{fmt.Sprintf("%s-change-%06d", id, bodied[i]+1), body} }); err != nil {
				return err
			}
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, push_id, position,
					at) VALUES `, `(?, ?, ?, ?, 'agent', ?, ?, ?)`, ``, 150,
				func(i int) []any {
					change, n := i/3, i%3+1
					return []any{fmt.Sprintf("%s-change-%06d", id, change+1), n,
						oid(fmt.Sprintf("head-%d-%d", change, n)), fmt.Sprintf("seat-%d", change%7),
						fmt.Sprintf("%s-push-%06d", id, i), 2000 + i, i}
				}); err != nil {
				return err
			}
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_change_authors (change_id, author, author_kind, first_revision,
					position) VALUES `, `(?, ?, 'agent', ?, ?)`, ``, 100,
				func(i int) []any {
					return []any{fmt.Sprintf("%s-change-%06d", id, i/2+1), fmt.Sprintf("seat-%d", i), i%2 + 1,
						2000 + i}
				}); err != nil {
				return err
			}
			// A CHAIN OF THREE LAYERS OVER THE FIRST THIRTY PACKS, ten each,
			// each layer's bytes in two parts.
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_layers (repo_id, seq, layer_id, base_id, graph_id, packs, midx_bytes,
					bitmap_bytes, graph_bytes, objects, commits, git, took_ms, added_position) VALUES `,
				`(?, ?, ?, ?, '', '["c"]', 10, 10, 0, 10, 0, '2.56.0', 100, ?)`, ``, 3,
				func(i int) []any {
					base := ""
					if i > 0 {
						base = fmt.Sprintf("layer-%06d", i)
					}
					return []any{id, i + 1, fmt.Sprintf("layer-%06d", i+1), base, 3000 + i}
				}); err != nil {
				return err
			}
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_layer_parts (repo_id, layer_id, part, object, hash, size) VALUES `,
				`(?, ?, ?, ?, 'h', 10)`, ``, 6,
				func(i int) []any {
					return []any{id, fmt.Sprintf("layer-%06d", i/2+1), i % 2,
						fmt.Sprintf("%s-layer-object-%06d", id, i)}
				}); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE code_repos SET published_packs = 30,
				published_objects = 30, published_at = ?, live_packs = 50 WHERE id = ?`, r, id); err != nil {
				return err
			}
			// AN EXPUNGE OPEN ON ONE, with its dropping packs, A PURGE
			// REQUESTED OF ANOTHER, and every repository expunged at an
			// instant of its own — so both of the expunge's partial indexes
			// hold rows, and the rest of each table does not.
			expunge, request := map[int]string{1: "expunge-1"}[r], map[int]string{2: "request-2"}[r]
			if _, err := tx.ExecContext(ctx, `UPDATE code_repos SET expunge_id = ?, expunge_request = ?,
				expunge_request_position = ?, expunged_at = ? WHERE id = ?`, expunge, request, 20*r, r, id); err != nil {
				return err
			}
			if expunge != "" {
				if _, err := tx.ExecContext(ctx, `UPDATE code_packs SET state = 'dropping', kind = 'cruft'
					WHERE repo_id = ? AND added_position >= 45`, id); err != nil {
					return err
				}
			}
		}
		// FIFTY SEATS' COUNTS OF THREE DAYS EACH: the counter a seat's day's
		// quota is read from, a row a seat and day.
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_seat_days (seat, day, pushes) VALUES `, `(?, ?, ?)`, ``, 150,
			func(i int) []any {
				return []any{fmt.Sprintf("seat-%d", i/3), fmt.Sprintf("2031-04-%02d", i%3+1), i}
			}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the plan estate: %v", err)
	}
	return db
}

// explain is a statement's query plan, one detail line a step.
func explain(t *testing.T, db store.ReplicatedHandle, statement string, args []any) []string {
	t.Helper()
	var plan []string
	// A WRITER'S TRANSACTION, because a read's refuses to plan a write — and
	// EXPLAIN executes nothing it plans.
	if err := db.Tx(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+statement, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				return err
			}
			plan = append(plan, detail)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN:\n%s\n%v", statement, err)
	}
	return plan
}

// indexesIn reads the index names out of a plan.
func indexesIn(plan []string) []string {
	var found []string
	for _, line := range plan {
		_, tail, ok := strings.Cut(line, "USING INDEX ")
		if !ok {
			_, tail, ok = strings.Cut(line, "USING COVERING INDEX ")
		}
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(tail, " ")
		found = append(found, strings.TrimSpace(name))
	}
	return found
}

// indexesOn is every index the schema declares on these tables.
func indexesOn(t *testing.T, db store.ReplicatedHandle, tables []string) []string {
	t.Helper()
	var found []string
	if err := db.Read(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), `SELECT name, tbl_name FROM sqlite_master
			WHERE type = 'index' AND sql IS NOT NULL ORDER BY name`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var name, table string
			if err := rows.Scan(&name, &table); err != nil {
				return err
			}
			if slices.Contains(tables, table) {
				found = append(found, name)
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the indexes: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the schema declares no index on any covered table")
	}
	return found
}
