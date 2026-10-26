package code

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// EVERY CODE INDEX SERVES A REGISTERED QUERY, AND EVERY REGISTERED QUERY SEEKS
// THE KEY IT IS WRITTEN FOR.
//
// A trailing comment naming an index's reader is a CLAIM; the planner is the
// only thing that knows whether the statement reaches the index. So this runs
// EXPLAIN QUERY PLAN over every statement the package issues against the
// tables a repository's history lives in — the guards, the applier's reads
// and writes (the tombstones a push sweeps, oldest first, among them), the
// lifecycle store's decides, the push decide's reads, the reader's pages in their
// first and keyset forms — a snapshot's retired phase among them — and its
// bounded counts, the purge's bounded count and its statements over a
// repository's changes, a sweep's decide, apply and listing over the retired
// packs, and the object store's statements over code_packs and
// code_packs_retired — and holds each to the EXACT seek it names: the index
// and every column of it the statement bounds. Seeking an index is not enough
// — a lookup of one ref that sought repo_id alone read every ref its
// repository holds (planner.go). The statements are the package's own
// constants: there is no copy here to drift from what runs. An index no
// statement reads yet is held to the read its migration claims for it
// ([claimedReads]), until a statement does.
//
// NO ANALYZE, because no deployment runs one: statistics choose differently,
// and a fixture that gathered them certified plans no node ever ran.
//
// The reads bounded by what a push row WEIGHS are held to a COVERING seek:
// they read the widths code_pushes_repo_idx carries, so weighing a wide row
// never reads its overflow pages. And every statement reads in the order of
// the index it seeks — a plan that sorts what it found reads all of it before
// its LIMIT takes a row — but those [sortingStatements] names, over a set
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
// states in another order and it seeks code_changes_state_idx.
func TestEveryCodeIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	db := seededPlanEstate(t)
	covered := []string{"code_repos", "code_names", "code_refs", "code_packs",
		"code_pushes", "code_protections", "code_packs_retired",
		"code_changes", "code_change_authors", "code_revisions"}
	// held plans one statement and answers the indexes its plan reaches.
	held := func(t *testing.T, name string, st statement) []string {
		plan := explain(t, db, st.sql, st.args)
		for _, table := range covered {
			if scansHeap(plan, table) {
				t.Errorf("%s reads %s with no index:\n%s", name, table, strings.Join(plan, "\n"))
			}
		}
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
	for name, st := range registeredStatements() {
		t.Run(name, func(t *testing.T) {
			for _, index := range held(t, name, st) {
				used[index] = true
			}
		})
	}
	claimed := map[string]bool{}
	for index, st := range claimedReads() {
		name := "the read claimed for " + index
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
// statement bounds, as the plan prints them.
type statement struct {
	sql   string
	args  []any
	seeks []string
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
	deletionKey   = "sqlite_autoindex_code_deletions_1"
	retiredKey    = "sqlite_autoindex_code_packs_retired_1"
	changeKey     = "sqlite_autoindex_code_changes_1"
	authorKey     = "sqlite_autoindex_code_change_authors_1"
	revisionKey   = "sqlite_autoindex_code_revisions_1"
)

// registeredStatements is every statement this package issues against the
// covered tables with a WHERE, by name.
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
	out := map[string]statement{
		"guard repo":          {guardRepoSQL, []any{"repo-1", "repo-1"}, []string{deletionKey + " (repo_id=?)", repoKey + " (id=?)"}},
		"guard name":          {guardNameSQL, []any{"token"}, []string{nameKey + " (token=?)"}},
		"marker":              {markerSQL, []any{"repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		"purged":              {purgedSQL, []any{"repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		"repo version":        {repoVersionSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"settings":            {settingsSQL, []any{"config", "unit", "ENG", "company", "main", "", "active", 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"clear protections":   {clearProtectionsSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"delete repo":         {deleteRepoSQL, []any{1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"restore repo":        {restoreRepoSQL, []any{"config", "unit", "ENG", "company", "main", "", "active", 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"pushes applied":      {pushesAppliedSQL, []any{ids}, []string{pushKey + " (push_id=?)"}},
		"refs named":          {refsNamedSQL, []any{"repo-1", keys}, []string{"SCAN json_each AS j", refsKey + " (repo_id=? AND ref=?)"}},
		"push counters":       {pushCountersSQL, []any{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
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
		"remaining":           {remainingSQL, []any{"repo-1", PurgePartRows + 1}, []string{"COVERING INDEX code_pushes_repo_idx (repo_id=?)", refsKey + " (repo_id=?)", packsKey + " (repo_id=?)", retiredKey + " (repo_id=?)", changesNumberKey + " (repo_id=?)", revisionKey + " (change_id=?)", authorKey + " (change_id=?)"}},
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
		"delta rows":          {deltaRowsSQL, []any{"repo-1", 400, 551}, []string{"COVERING INDEX code_refs_changed_idx (repo_id=? AND position>?)", "COVERING INDEX code_packs_added_idx (repo_id=? AND added_position>?)", "COVERING INDEX " + retiredRepoIdx + " (repo_id=? AND retired_position>?)"}},
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
		"pushes named":        {pushesNamedSQL, []any{ids}, []string{pushKey + " (push_id=?)"}},
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
		// A PURGE'S CHANGES: the walk in number order, each change's
		// dependents counted by its key; the whole changes' rows by the list
		// of their ids; the cut change's in its key's order; and the final
		// purge's, the dependents through their changes.
		"purge change walk": {purgeChangeWalkSQL, []any{"repo-1", PurgePartRows},
			[]string{changesNumberKey + " (repo_id=?)", revisionKey + " (change_id=?)", authorKey + " (change_id=?)"}},
		"purge changes' revisions": {purgeChangeRevisionsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", revisionKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' authors": {purgeChangeAuthorsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", authorKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
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
		"purge all changes": {purgeAllChangesSQL, []any{"repo-1"},
			[]string{"COVERING INDEX " + changesNumberKey + " (repo_id=?)"}},
	}
	among, err := PackReferences.ObjectsAmong(3)
	if err != nil {
		panic(err)
	}
	out["objects among"] = statement{among, []any{"k1", "k2", "k3"},
		[]string{"code_packs_object_idx (object=?)"}}
	after, err := PackReferences.ReferencesAfter(100)
	if err != nil {
		panic(err)
	}
	out["references after"] = statement{after, []any{""},
		[]string{"code_packs_object_idx (object>?)"}}
	// THE COLLECTOR'S ONE QUESTION OF THE RETIRED TABLE — the audit's and the
	// backup's walk is refused for a retired declaration, so this is all of
	// what the object store asks of it.
	retired, err := RetiredPackReferences.ObjectsAmong(3)
	if err != nil {
		panic(err)
	}
	out["retired objects among"] = statement{retired, []any{"k1", "k2", "k3"},
		[]string{"code_packs_retired_object_idx (object=?)"}}
	return out
}

// claimedReads is every index on a covered table that no statement this
// package issues reads YET, by name, with the read its migration's `-- <reader>`
// comment claims for it — planned here, so the claim is the planner's from the
// migration that declares the index rather than from whichever change first
// issues the read. A change's indexes land with its tables (replicated 0058),
// one migration for the rows rather than one an index, and their reads — the
// lifecycle's, the listings', a commit's page — are each a change's of its own.
//
// TWO-SIDED, as every allowance here is: a claim whose index a registered
// statement reaches fails, because the change that issues the read registers
// its own statement and a claim beside it would be a second copy of what runs;
// and an index that neither a statement nor a claim reaches fails as it always
// has. Each read is written in the claim's own terms — the live states as
// [liveChanges] spells them, a listing newest first by the order its index
// keeps — and is held to the exact seek, no heap scan and no sort, like any
// registered statement.
func claimedReads() map[string]statement {
	return map[string]statement{
		"code_changes_open_idx": {`SELECT id FROM code_changes WHERE repo_id = ? AND source_ref = ? AND ` +
			liveChanges, []any{"repo-1", "refs/heads/f000001"},
			[]string{"code_changes_open_idx (repo_id=? AND source_ref=?)"}},
		"code_changes_target_idx": {`SELECT id FROM code_changes WHERE repo_id = ? AND target_ref = ?
			AND head_oid = ? AND ` + liveChanges, []any{"repo-1", "refs/heads/main", oid("head-1-1")},
			[]string{"code_changes_target_idx (repo_id=? AND target_ref=? AND head_oid=?)"}},
		"code_changes_state_idx": {`SELECT id FROM code_changes WHERE state = ?
			ORDER BY opened_position DESC, id DESC LIMIT ?`, []any{string(ChangeStateOpen), 50},
			[]string{"code_changes_state_idx (state=?)"}},
		"code_changes_owner_idx": {`SELECT id FROM code_changes WHERE owner = ? AND state = ?
			ORDER BY opened_position DESC, id DESC LIMIT ?`, []any{"seat-1", string(ChangeStateOpen), 50},
			[]string{"code_changes_owner_idx (owner=? AND state=?)"}},
		"code_changes_work_idx": {`SELECT id FROM code_changes WHERE work_backend = ? AND work_id = ?
			AND work_id != ''`, []any{"native", "work-3"},
			[]string{"code_changes_work_idx (work_backend=? AND work_id=?)"}},
		"code_changes_merge_idx": {`SELECT id FROM code_changes WHERE repo_id = ? AND merge_oid = ?
			AND merge_oid != ''`, []any{"repo-1", oid("head-3-3")},
			[]string{"code_changes_merge_idx (repo_id=? AND merge_oid=?)"}},
		"code_revisions_head_idx": {`SELECT change_id, n FROM code_revisions WHERE head_oid = ?`,
			[]any{oid("head-1-1")}, []string{"code_revisions_head_idx (head_oid=?)"}},
	}
}

// retiredRepoIdx is code_packs_retired_repo_idx, (repo_id, retired_position,
// pack_id, part): the order every statement over a repository's retired rows
// names, so a sweep and a purge remove them alike on every node.
const retiredRepoIdx = "code_packs_retired_repo_idx"

// seededPlanEstate is an estate holding five repositories of 500 refs, 50
// packs, 200 pushes, 200 retired packs and 50 changes each, with no statistics
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
				`INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, commands,
					received_at, row_bytes) VALUES `, `(?, ?, ?, ?, 'op', 'operator', '[]', 0, 2)`, ``, 200,
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
			// FIFTY CHANGES ACROSS EVERY STATE, each of three revisions and
			// two authors, a work item on every third and every merged one
			// naming the commit it was merged at.
			states := ChangeStates()
			if _, err := store.InsertRows(ctx, tx, vars,
				`INSERT INTO code_changes (id, repo_id, number, title, state, source_ref, target_ref,
					head_oid, revision, owner, owner_kind, work_backend, work_id, opened_at,
					opened_position, closed_at, merged_at, merged_via, merge_oid, version, position) VALUES `,
				`(?, ?, ?, 'a change', ?, ?, 'refs/heads/main', ?, 3, ?, 'agent', ?, ?, ?, ?, ?, ?, ?, ?, 3, ?)`,
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
					return []any{fmt.Sprintf("%s-change-%06d", id, i+1), id, i + 1, string(state),
						fmt.Sprintf("refs/heads/f%06d", i), oid(fmt.Sprintf("head-%d-3", i)),
						fmt.Sprintf("seat-%d", i%7), backend, work, i, 2000 + i, closed, merged, via, merge,
						2000 + i}
				}); err != nil {
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

// scansHeap reports a plan line that reads a whole table with no index — an
// index SCAN walks in the index's order and a LIMIT stops it; a bare one reads
// every row through the heap.
func scansHeap(plan []string, table string) bool {
	for _, line := range plan {
		if (strings.HasPrefix(line, "SCAN "+table+" ") || line == "SCAN "+table) &&
			!strings.Contains(line, "INDEX") && !strings.Contains(line, "PRIMARY KEY") {
			return true
		}
	}
	return false
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
