package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// EVERY CODE INDEX SERVES A REGISTERED QUERY, AND EVERY REGISTERED QUERY SEEKS
// THE KEY IT IS WRITTEN FOR.
//
// A trailing comment naming an index's reader is a CLAIM; only the planner
// knows whether the statement reaches the index. So this runs EXPLAIN QUERY
// PLAN over every statement the package issues against its tables, at every
// width its callers build it at ([planShapes]), and holds each to the EXACT
// seek it names: the index and every column of it the statement bounds. A ref
// lookup that sought repo_id alone read every ref its repository holds
// (planner.go). TestEveryCodeReadSeeksANamedIndex refuses a scan; this holds
// which index, columns and order. An unread index is held to its migration's
// claim ([claimedReads]).
func TestEveryCodeIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	// NO ANALYZE, because no deployment runs one: statistics choose
	// differently. Reads bounded by what a push row WEIGHS are held to a
	// COVERING seek (code_pushes_repo_idx carries the widths), and every
	// statement reads in its index's order, since a sorting plan reads all
	// before LIMIT, except [sortingStatements]. Mutations that must go red:
	// drop `INDEXED BY` from liveRefSQL, `position >= ?` from
	// refsDeltaAfterSQL or row_bytes from code_pushes_repo_idx; order the push
	// listing by `at`; lead code_ref_drops' key with the position; key
	// code_changes_work_idx by the item alone; test `ref IN (SELECT ...)` in
	// refsNamedSQL; order the purge's change walk by `c.number, r.n`.
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
		plan := storetest.Plan(t, db, st.sql, st.args)
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
		// replicated 0084 moved out of code_pushes_repo_idx's key would read
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
		return storetest.IndexesIn(plan)
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
	for name, st := range searchReads() {
		t.Run(name, func(t *testing.T) {
			for _, index := range held(t, name, st) {
				used[index] = true
			}
		})
	}
	for _, index := range storetest.IndexesOn(t, db, covered) {
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

// A HISTORY PAGE'S STATEMENTS READ NO UNNESTED AGGREGATE, which would read every
// attempt of every commit; a control the driver does unnest proves the check sees it.
// Mutation: ignore Decorrelated's answer, or plan the control as a page statement.
func TestTheCommitMarksStatementsReadNoUnnestedAggregate(t *testing.T) {
	t.Parallel()
	db := seededPlanEstate(t)
	control := storetest.Plan(t, db, `SELECT k.name, (SELECT MAX(n.attempt) FROM code_checks AS n
		WHERE n.repo_id = k.repo_id AND n.commit_oid = k.commit_oid AND n.target_ref = k.target_ref
			AND n.name = k.name) FROM code_checks AS k`, nil)
	if _, unnested := storetest.Decorrelated(control); !unnested {
		t.Fatalf("the control, a per-row maximum of every attempt, is not seen unnested, so this check "+
			"passes every statement blind:\n%s", strings.Join(control, "\n"))
	}
	for name, st := range map[string]statement{
		"a commit's latest checks, every branch": {sql: latestChecksOfCommitSQL,
			args: []any{"repo-1", oid("c"), MaxMarkTargets*maxRollupChecks + 1}},
		"a commit's latest checks for a branch": {sql: latestChecksOfTargetSQL,
			args: []any{"repo-1", oid("c"), "refs/heads/main", maxRollupChecks + 1}},
		"a merged change's approvals": {sql: changeApprovalsSQL,
			args: []any{"repo-1-change-000001", 1, MaxGateApprovals + 1}},
		"when a merged revision was pushed": {sql: revisionPushedSQL, args: []any{"repo-1-change-000001", 1}},
		"a change's landing":                {sql: changeLandingSQL, args: []any{"repo-1-change-000001"}},
		"a card's newest queue entry": {sql: cardEjectionsSQL,
			args: []any{`[["repo-1","repo-1-change-000001"]]`}},
		"changes a commit merged": {sql: commitMergedSQL,
			args: []any{"repo-1", oid("head-3-3"), MaxCommitChanges + 1}},
		"revisions a commit heads": {sql: commitHeadsSQL,
			args: []any{oid("head-1-1"), "repo-1", MaxCommitChanges + 1}},
		"when a page's reviewers last moved": {sql: reviewersMovedAtSQL,
			args: []any{"repo-1-change-000001", "[3000,3001]"}},
		"a standing page's facts": {sql: standingFactsSQL,
			args: []any{`[["repo-1","` + oid("c") + `","refs/heads/main","unit",1]]`}},
		"a standing page's entries": {sql: standingEntriesSQL, args: []any{`[["repo-1","entry-1"]]`}},
	} {
		plan := storetest.Plan(t, db, st.sql, st.args)
		if line, unnested := storetest.Decorrelated(plan); unnested {
			t.Errorf("%s answers a per-row aggregate from its whole child table (%q):\n%s", name, line,
				strings.Join(plan, "\n"))
		}
	}
}

// THE RANGES A ROUND ASKS ABOUT ARE READ OVER THE LIVE REFS ALONE.
//
// The push decide asks, under every create, whether a live ref lies in a
// range of code_refs' key ([gapsHoldingSQL], then [firstLiveInSQL]), and a
// range holds the tombstones under it too. Read over the key, the seek stepped
// over each tombstone once per create above it: 118 nested creates over
// [MaxTombstonesPerRepo] tombstones held a round 7.7-9.9 s
// (BenchmarkDecideABatch's buried arm). So each statement reads code_refs
// through [liveRefsKey] only, an index PARTIAL on the live refs: the half the
// plan cannot show, since the index rebuilt without its predicate plans the
// same seek. Mutation: name [refsKey] in either statement, or declare the
// index without its predicate on the empty target.
func TestTheRefConflictProbesStepOverNoTombstone(t *testing.T) {
	t.Parallel()
	db := seededPlanEstate(t)
	gaps := `[["refs/heads/r/","refs/heads/r0"],["refs/heads/r000001/","refs/heads/r0000010"]]`
	for name, probe := range map[string]string{"gaps holding": gapsHoldingSQL, "first live in": firstLiveInSQL} {
		plan := storetest.Plan(t, db, probe, []any{"repo-1", gaps})
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

// THE MOVED INDEX HOLDS THE LIVE BRANCHES ALONE, its predicate [movedRefs]:
// the planner prints the same seek without it, so only the declaration shows it.
// Mutation: declare it without the namespace bounds.
func TestTheMovedIndexHoldsTheLiveBranchesAlone(t *testing.T) {
	t.Parallel()
	db := seededPlanEstate(t)
	var declared string
	if err := db.Read(context.Background(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(context.Background(),
			`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, movedRefsKey).Scan(&declared)
	}); err != nil {
		t.Fatalf("read %s's declaration: %v", movedRefsKey, err)
	}
	want := "CREATE INDEX " + movedRefsKey + " ON code_refs (repo_id, at, ref) WHERE " + movedRefs
	if got := strings.Join(strings.Fields(declared), " "); got != want {
		t.Fatalf("%s is declared\n\t%s\nwant\n\t%s", movedRefsKey, got, want)
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

// THE PUSHED INDEX HOLDS THE REPOSITORIES IN USE, SPELLED AS ITS READERS SPELL THEM:
// [reposInUse] lists every [RepoStatus.live] status in [RepoStatuses]' order, and
// code_repos_pushed_idx is declared partial on it exactly, so a listing by last push lists
// what a listing by name of the same statuses lists.
//
// Mutation: declare the index on `status IN ('active', 'archived')`, or make a deleted
// repository live, and this fails.
func TestThePushedIndexHoldsTheRepositoriesInUse(t *testing.T) {
	t.Parallel()
	var live []string
	for _, s := range RepoStatuses() {
		if s.live() {
			live = append(live, "'"+string(s)+"'")
		}
	}
	if want := "status IN (" + strings.Join(live, ", ") + ")"; reposInUse != want {
		t.Fatalf("reposInUse is %q, and the statuses in use in RepoStatuses' order spell %q", reposInUse, want)
	}
	node, db := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "node.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = node.Close() })
	var declared string
	if err := db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`,
			pushedReposKey).Scan(&declared)
	}); err != nil {
		t.Fatalf("read %s's declaration: %v", pushedReposKey, err)
	}
	want := "CREATE INDEX " + pushedReposKey + " ON code_repos (kind, last_push_at, name) WHERE " + reposInUse
	if got := strings.Join(strings.Fields(declared), " "); got != want {
		t.Errorf("%s is declared\n\t%s\nwant\n\t%s", pushedReposKey, got, want)
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
	"push rules of": "one listing page's rules, at most MaxReposPerPage of MaxProtectionsPerRepo " +
		"each: a seek a repository of the page, whose union no one index orders",
	"protections of": "the rules of the repositories a reconcile's declaration names, at most " +
		"config.CodeMaxRepos of MaxProtectionsPerRepo each: a seek a repository of the declaration, whose " +
		"union no one index orders",
	"due rechecks": "one repository's live changes behind a moved target, at most one a live branch " +
		"(code_changes_open_idx), so MaxRefsPerRepo bounds them: no index orders a repository's changes by " +
		"when their merge was recorded without one written by the records of two subjects (changesdue.go)",
	"drop a person's oldest viewed marks": "one person's own marks, at most MaxViewedFiles + MaxViewedPerMark, " +
		"sorted only when a record takes them past the cap: no index orders a person's marks by when they were " +
		"made without an entry paid on every mark",
	"every repository's ejections counted": "at most MaxQueueCounted index entries of the day's ejections: " +
		"the inner LIMIT bounds what the group sorts",
	"every repository's live entries counted": "at most MaxQueueCounted index entries of the live entries: " +
		"the inner LIMIT bounds what the group sorts",
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
	talliesKey    = "sqlite_autoindex_code_change_tallies_1"
	// repoTalliesKey is code_repo_tallies' PRIMARY KEY index, (visibility, owner_kind, owner, kind).
	repoTalliesKey = "sqlite_autoindex_code_repo_tallies_1"
	// untestedKey and dequeuedKey are replicated 0140's indexes as a
	// given-up attempt's tests seek them: its commit among the ones its
	// target's trains test or landed, and its candidate among the dequeued.
	untestedKey = "code_queue_commit_idx (repo_id=? AND target_ref=? AND commit_oid=?)"
	dequeuedKey = "code_queue_dequeued_idx (repo_id=? AND candidate_id=?)"
	// A change's conversation's keys.
	conversationKey = "sqlite_autoindex_code_conversations_1"
	reviewerKey     = "sqlite_autoindex_code_reviewers_1"
	participantKey  = "sqlite_autoindex_code_participants_1"
	commentKey      = "sqlite_autoindex_code_comments_1"
	findingKey      = "sqlite_autoindex_code_findings_1"
	findingTextKey  = "sqlite_autoindex_code_finding_texts_1"
	reviewKey       = "sqlite_autoindex_code_reviews_1"
	factsKey        = "sqlite_autoindex_code_revision_facts_1"
	mergeabilityKey = "sqlite_autoindex_code_mergeability_1"
	activationKey   = "sqlite_autoindex_code_activation_1"
	evictionKey     = "sqlite_autoindex_code_evictions_1"
	layersKey       = "sqlite_autoindex_code_layers_1"
	layerPartsKey   = "sqlite_autoindex_code_layer_parts_1"
	checkLogsKey    = "sqlite_autoindex_code_check_logs_1"
	seatDaysKey     = "sqlite_autoindex_code_seat_days_1"
	pushDaysKey     = "sqlite_autoindex_code_push_days_1"
	pusherDaysKey   = "sqlite_autoindex_code_pusher_days_1"
	repoKindsKey    = "sqlite_autoindex_code_repo_kinds_1"
	viewedFilesKey  = "sqlite_autoindex_code_viewed_files_1"
	viewedKeysKey   = "sqlite_autoindex_code_viewed_keys_1"
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
		"settings":            {settingsSQL, []any{"config", "unit", "ENG", "company", "indexed", "main", "", "", "", strings.Repeat("a", 64), strings.Repeat("b", 64), 1, "active", 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"mirror import":       {mirrorImportSQL, []any{`{"url":"https://forge.example.com/a/web.git"}`, "repo-1"}, []string{repoKey + " (id=?)"}},
		"clear protections":   {clearProtectionsSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"path owners":         {pathOwnersSQL, []any{"repo-1"}, []string{pathOwnerKey + " (repo_id=?)"}},
		"path owners of":      {pathOwnersOfSQL, []any{`["repo-1","repo-2"]`}, []string{pathOwnerKey + " (repo_id=?)"}},
		"clear path owners":   {clearPathOwnersSQL, []any{"repo-1"}, []string{pathOwnerKey + " (repo_id=?)"}},
		"delete repo":         {deleteRepoSQL, []any{1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"restore repo":        {restoreRepoSQL, []any{"config", "unit", "ENG", "company", "indexed", "main", "", "", "", strings.Repeat("a", 64), strings.Repeat("b", 64), 1, "active", 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"pushes applied":      {pushesAppliedSQL, []any{ids}, []string{pushKey + " (push_id=?)"}},
		"refs named":          {refsNamedSQL, []any{"repo-1", keys}, []string{"SCAN json_each AS j", refsKey + " (repo_id=? AND ref=?)"}},
		"push counters":       {pushCountersSQL, []any{1, 1, 1, 1, 1, 1, 1, 1, false, 1, false, 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
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
		"remaining":           {remainingSQL, []any{"repo-1", PurgePartRows + 1}, []string{"COVERING INDEX code_pushes_repo_idx (repo_id=?)", refsKey + " (repo_id=?)", packsKey + " (repo_id=?)", retiredKey + " (repo_id=?)", dropsKey + " (repo_id=?)", changesNumberKey + " (repo_id=?)", revisionKey + " (change_id=?)", authorKey + " (change_id=?)", movesKey + " (change_id=?)", "COVERING INDEX " + bodyKey + " (change_id=?)", reviewerKey + " (change_id=?)", participantKey + " (change_id=?)", "COVERING INDEX " + findingPathsKey + " (change_id=?)", "COVERING INDEX code_comments_thread_idx (change_id=?)", "COVERING INDEX " + findingTextKey + " (change_id=?)", "COVERING INDEX code_reviews_change_idx (change_id=?)", factsKey + " (change_id=?)", mergeabilityKey + " (change_id=?)", conversationKey + " (change_id=?)", "COVERING INDEX " + checksEndedKey + " (repo_id=?)", checkLogsKey + " (repo_id=?)", "sqlite_autoindex_code_queue_1 (repo_id=?)", "sqlite_autoindex_code_candidates_1 (repo_id=?)", "sqlite_autoindex_code_landings_1 (repo_id=?)", "sqlite_autoindex_code_green_1 (repo_id=?)", "sqlite_autoindex_code_tips_due_1 (repo_id=?)", "COVERING INDEX " + pushDaysKey + " (repo_id=?)", "COVERING INDEX " + pusherDaysKey + " (repo_id=?)"}},
		"claim":               {claimSQL, []any{"token"}, []string{nameKey + " (token=?)"}},
		"claims":              {claimsSQL, []any{`["token","other"]`}, []string{nameKey + " (token=?)"}},
		"repos rows":          {reposRowsSQL, []any{`["repo-1","repo-2"]`}, []string{repoKey + " (id=?)"}},
		"repo row":            {repoRowSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"repo remotes":        {remotesSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"repo detail row":     {detailRowSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"protections":         {protectionsSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"push rules":          {pushRulesSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"gates":               {gatesSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"rule details":        {ruleDetailsSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"sourced rules":       {sourcedRulesSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"rule":                {ruleSQL, []any{"repo-1", "refs/heads/main"}, []string{protectionKey + " (repo_id=? AND pattern=?)"}},
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
		"latest pushes":       {latestPushesSQL, []any{"repo-1", MaxLastPushWalk}, []string{"code_pushes_repo_idx (repo_id=?)"}},
		"pushes after":        {pushesAfterSQL, []any{"repo-1", 100, 100, "p", 50}, []string{"code_pushes_repo_idx (repo_id=? AND position<=?)"}},
		"pushes above":        {pushesAboveSQL, []any{"repo-1", 10, 100, 100}, []string{"code_pushes_repo_idx (repo_id=? AND position>? AND position<=?)"}},
		"pushes above after":  {pushesAboveAfterSQL, []any{"repo-1", 10, 100, 10, "p", 100}, []string{"code_pushes_repo_idx (repo_id=? AND position>=? AND position<=?)"}},
		"pushes below":        {pushesBelowSQL, []any{"repo-1", 100, 100}, []string{"code_pushes_repo_idx (repo_id=? AND position<?)"}},
		"repos":               {reposSQL, []any{`["active","archived"]`, "", 100}, []string{"code_repos_name_idx (name>?)", refsKey + " (repo_id=? AND ref=?)"}},
		"repos protected":     {protectedReposSQL, []any{`["active","archived"]`, "", 100}, []string{protectedReposKey + " (name>?)", refsKey + " (repo_id=? AND ref=?)"}},
		"protections of":      {protectionsOfSQL, []any{`["repo-1","repo-2"]`}, []string{protectionKey + " (repo_id=?)"}},
		"push rules of":       {pushRulesOfSQL, []any{`["repo-1","repo-2"]`}, []string{protectionKey + " (repo_id=?)"}},
		"live ref":            {liveRefSQL, []any{"repo-1", "refs/heads/main"}, []string{refsKey + " (repo_id=? AND ref=?)"}},
		"refs under":          {refsUnderSQL, []any{"repo-1", "refs/heads/", "refs/heads0", 200}, []string{"INDEX " + liveRefsKey + " (repo_id=? AND ref>? AND ref<?)"}},
		"lives":               {livesSQL, []any{"", 1000}, []string{"code_repos_name_idx (name>?)"}},
		"due purging":         {duePurgingSQL, []any{-1, -1, "", 100}, []string{"code_repos_purging_idx (deleted_at>=?)"}},
		"due deleted":         {dueDeletedSQL, []any{100, -1, -1, "", 100}, []string{"code_repos_deleted_idx (deleted_at>=? AND deleted_at<=?)"}},
		"purge state":         {purgeStateSQL, []any{"repo-1", "repo-1"}, []string{repoKey + " (id=?)", deletionKey + " (repo_id=?)"}},
		"count a gate reject": {countRejectSQL, []any{1, "repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		"decide repo":         {decideRepoSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"decide deletion":     {decideDeletionSQL, []any{"repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		// THE NAMES A PREFIX BEGINS, in a listing's order: one range of its
		// index, from the prefix on a first page and past the cursor after
		// one, up to the prefix's end — never a walk of the names before it.
		"repos under first": {reposUnderFirstSQL, []any{`["active","archived"]`, "r01", "r02", 100},
			[]string{"code_repos_name_idx (name>=? AND name<?)", refsKey + " (repo_id=? AND ref=?)"}},
		"repos under after": {reposUnderAfterSQL, []any{`["active","archived"]`, "r0150", "r02", 100},
			[]string{"code_repos_name_idx (name>? AND name<?)", refsKey + " (repo_id=? AND ref=?)"}},
		// A LISTING OF ONE KIND, AND BY LAST PUSH: one range of a kind's index, the dated walked
		// backwards and the never-pushed after it by name; and a total's size over one kind's range.
		"repos of a kind": {kindReposSQL, []any{`["active","archived"]`, "project", "", 100},
			[]string{kindReposKey + " (kind=? AND name>?)", refsKey + " (repo_id=? AND ref=?)"}},
		"repos pushed": {pushedReposSQL, []any{`["active","archived"]`, "project", int64(math.MinInt64), 100},
			[]string{pushedReposKey + " (kind=? AND last_push_at>?)", refsKey + " (repo_id=? AND ref=?)"}},
		"repos pushed after": {pushedReposAfterSQL, []any{`["active","archived"]`, "project", 9, 9, "r05", 100},
			[]string{pushedReposKey + " (kind=? AND last_push_at<=?)", refsKey + " (repo_id=? AND ref=?)"}},
		"repos never pushed": {neverPushedReposSQL, []any{`["active","archived"]`, "project", "", 100},
			[]string{pushedReposKey + " (kind=? AND last_push_at=? AND name>?)", refsKey + " (repo_id=? AND ref=?)"}},
		"repos sized": {reposSizedSQL, []any{"project", `["active","archived"]`, MaxReposSized + 1},
			[]string{kindReposKey + " (kind=?)"}},
		"protected repos under first": {protectedReposUnderFirstSQL,
			[]any{`["active","archived"]`, "r01", "r02", 100},
			[]string{protectedReposKey + " (name>=? AND name<?)", refsKey + " (repo_id=? AND ref=?)"}},
		"protected repos under after": {protectedReposUnderAfterSQL,
			[]any{`["active","archived"]`, "r0150", "r02", 100},
			[]string{protectedReposKey + " (name>? AND name<?)", refsKey + " (repo_id=? AND ref=?)"}},
		// A SEAT'S DAY IS ONE ROW, read by its key for every seat a round
		// names, and its oldest day beyond its two newest forgotten by the
		// same key, read newest first — never a count over the push history,
		// and never a sort of the seat's days.
		"seat days": {seatDaysSQL, []any{"2031-04-02", `["seat-1","seat-2"]`},
			[]string{"SCAN json_each AS j", seatDaysKey + " (seat=? AND day=?)"}},
		"forget seat days": {forgetSeatDaysSQL, []any{`["seat-1","seat-2"]`},
			[]string{"SCAN json_each AS j", seatDaysKey + " (seat=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		// AN EXECUTOR'S PUSHES TO ONE REPOSITORY, above its launch in log
		// order: the window a page may read, weighed from the index alone —
		// its count and weight, its weights one by one, its last push's
		// place, each a COVERING seek, so weighing a wide row never reads it
		// — and then the run's own in exactly that window, the same index's
		// range with every row it reaches tested for the run, which no index
		// carries (replicated 0098); never a sort.
		"run weigh": {runWeighSQL, []any{"repo-1", 11, "", 50},
			[]string{"COVERING INDEX code_pushes_repo_idx (repo_id=? AND position>=?)"}},
		"run weights": {runWeightsSQL, []any{"repo-1", 11, "", 50},
			[]string{"COVERING INDEX code_pushes_repo_idx (repo_id=? AND position>=?)"}},
		"run last": {runLastSQL, []any{"repo-1", 11, "", 49},
			[]string{"COVERING INDEX code_pushes_repo_idx (repo_id=? AND position>=?)"}},
		"run pushes in": {runPushesInSQL, []any{"repo-1", "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", 11, "", 20, "q"},
			[]string{"code_pushes_repo_idx (repo_id=? AND position>=? AND position<=?)"}},
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
		// A SEAT BRANCH'S HOLD: its decide's read of the ref and its apply's
		// mark, each one row by code_refs' key, and the repository's version
		// by its own.
		"ref hold":         {refHoldSQL, []any{"repo-1", "refs/heads/seats/dev/x"}, []string{refsKey + " (repo_id=? AND ref=?)"}},
		"ref held":         {refHeldSQL, []any{1, "repo-1", "refs/heads/seats/dev/x"}, []string{refsKey + " (repo_id=? AND ref=?)"}},
		"ref hold version": {refHoldVersionSQL, []any{1, "repo-1"}, []string{repoKey + " (id=?)"}},
		// THE REF LIFECYCLE: rule 4b's rows of an expiry's refs, driven from
		// the list, with a seek of each live-change index; the step's keyset
		// page over its partial index, oldest first, each row it reads
		// sought in both live-change indexes so a live change's branches are
		// skipped (M4-D32); and the page's repositories by their key.
		"expiry refs": {expiryRefsSQL, []any{"repo-1", keys}, []string{"SCAN json_each AS j",
			refsKey + " (repo_id=? AND ref=?)", "code_changes_open_idx (repo_id=? AND source_ref=?)",
			"code_changes_target_idx (repo_id=? AND target_ref=?)"}},
		"expiry listing": {expiryListingSQL, []any{100, -1, "", "", MaxCommandsPerPush},
			[]string{"code_refs_expiry_idx (at>=? AND at<=?)", "code_changes_open_idx (repo_id=? AND source_ref=?)",
				"code_changes_target_idx (repo_id=? AND target_ref=?)"}},
		"expiry repos": {expiryReposSQL, []any{`["repo-1","repo-2"]`}, []string{"SCAN json_each AS j",
			repoKey + " (id=?)"}},
		// code_refs_near_limit's reading: the candidates by a keyset walk of
		// the name index, as the reconcile's own walk is, and each one's refs
		// set within RefExpiry by its key's repo_id prefix.
		"refs candidates": {refsCandidatesSQL, []any{"", refsCandidateFloor, 1000},
			[]string{"code_repos_name_idx (name>?)"}},
		"refs set since": {refsSetSinceSQL, []any{"repo-1", 100}, []string{refsKey + " (repo_id=?)"}},
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
				movesKey + " (change_id=?)", reviewerKey + " (change_id=?)", participantKey + " (change_id=?)",
				"COVERING INDEX " + findingPathsKey + " (change_id=?)",
				"COVERING INDEX code_comments_thread_idx (change_id=?)",
				"COVERING INDEX " + findingTextKey + " (change_id=?)",
				"COVERING INDEX code_reviews_change_idx (change_id=?)", factsKey + " (change_id=?)",
				"COVERING INDEX " + bodyKey + " (change_id=?)", mergeabilityKey + " (change_id=?)",
				conversationKey + " (change_id=?)"}},
		"purge changes' reviewers": {purgeChangeReviewersSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", reviewerKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' participants": {purgeChangeParticipantsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", participantKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' comments": {purgeChangeCommentsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", "code_comments_thread_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' findings": {purgeChangeFindingsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", findingPathsKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' finding texts": {purgeChangeFindingTextsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", findingTextKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' reviews": {purgeChangeReviewsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", "code_reviews_change_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' facts": {purgeChangeFactsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", factsKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' mergeability": {purgeChangeMergeabilitySQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", mergeabilityKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
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
		"weigh a cut change's facts": {purgeCutFactWidthsSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{factsKey + " (change_id=?)"}},
		"purge a cut change's facts": {purgeCutFactsSQL, []any{"repo-1-change-000001", 3},
			[]string{factsKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"count a cut change's notices again": {uncountNoticesSQL, []any{PurgePartRows, "repo-1-change-000001"},
			[]string{changeKey + " (id=?)"}},
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
			[]string{changesNumberKey + " (repo_id=?)", findingPathsKey + " (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all finding texts": {purgeAllFindingTextsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", findingTextKey + " (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all reviews": {purgeAllReviewsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", "code_reviews_change_idx (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all facts": {purgeAllFactsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", factsKey + " (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all mergeability": {purgeAllMergeabilitySQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", mergeabilityKey + " (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all conversations": {purgeAllConversationsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", conversationKey + " (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' revisions": {purgeChangeRevisionsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", revisionKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' authors": {purgeChangeAuthorsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", authorKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' moves": {purgeChangeMovesSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", movesKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' bodies": {purgeChangeBodiesSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", bodyKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge changes' rows": {purgeChangeRowsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", changeKey + " (id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge a cut change's revisions": {purgeCutRevisionsSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{revisionKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge a cut change's authors": {purgeCutAuthorsSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{authorKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge a cut change's moves": {purgeCutMovesSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{movesKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all revisions": {purgeAllRevisionsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", revisionKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all authors": {purgeAllAuthorsSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", authorKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all moves": {purgeAllMovesSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", movesKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all bodies": {purgeAllBodiesSQL, []any{"repo-1"},
			[]string{changesNumberKey + " (repo_id=?)", bodyKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all changes": {purgeAllChangesSQL, []any{"repo-1"},
			[]string{"COVERING INDEX " + changesNumberKey + " (repo_id=?)"}},
		// A CHANGE'S LIFECYCLE: a change by its key — a decide's, an
		// apply's and an answer's; the live change a source branch heads, by
		// the index partial on the live states, its predicate as the index
		// spells it; a change's body read and removed by its key; and the
		// repository's next number and version by its own. Its row is
		// written back by an upsert, which reads no row to find the one it
		// writes (changewrite_internal_test.go holds what it rewrites).
		"change row": {changeRowSQL, []any{"repo-1-change-000001"}, []string{changeKey + " (id=?)"}},
		// A REVIEW'S ANNOUNCEMENT reads its row by its key (announce.go).
		"review row": {reviewRowSQL, []any{"review-1"}, []string{"sqlite_autoindex_code_reviews_1 (id=?)"}},
		"live change on": {liveChangeOnSQL, []any{"repo-1", "refs/heads/f000001"},
			[]string{"code_changes_open_idx (repo_id=? AND source_ref=?)"}},
		// A REFS LISTING BY WHEN EACH BRANCH MOVED, its active count, and its
		// detail's reads: a ref's movers at its position and a branch's change.
		"refs moved": {refsMovedSQL, []any{"repo-1", 0, int64(math.MaxInt64), 200},
			[]string{"INDEX " + movedRefsKey + " (repo_id=? AND at<=? AND at>=?)"}},
		"refs moved after": {refsMovedAfterSQL, []any{"repo-1", 0, 9, 9, "refs/heads/b", 200},
			[]string{"INDEX " + movedRefsKey + " (repo_id=? AND at<=? AND at>=?)"}},
		"active branches": {activeBranchesSQL, []any{"repo-1", 0, "refs/heads/main", 1001},
			[]string{"INDEX " + movedRefsKey + " (repo_id=? AND at>=?)"}},
		"pushes at a position": {pushesAtSQL, []any{"repo-1", 7},
			[]string{"code_pushes_repo_idx (repo_id=? AND position=?)"}},
		"live change of a source": {liveChangeOfSourceSQL, []any{"repo-1", "refs/heads/topic"},
			[]string{"code_changes_open_idx (repo_id=? AND source_ref=?)"}},
		"newest change of a source": {newestChangeOfSourceSQL, []any{"repo-1", "refs/heads/topic"},
			[]string{changesSourceKey + " (repo_id=? AND source_ref=?)"}},
		// A FAILED CHECK A PERSON'S CI REPORTED finds the live change at
		// its head on its target (checkswriter.go's headChangeIn).
		"live change at head": {liveChangeAtHeadSQL, []any{"repo-1", "refs/heads/main", oid("head-1-3")},
			[]string{"code_changes_target_idx (repo_id=? AND target_ref=? AND head_oid=?)"}},
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
		// apply holds a record to; and a resolved thread written back by its
		// key (an ask withdrawn is moved with the rest, "move asks").
		"conversation":        {conversationSQL, []any{"repo-1-change-000001"}, []string{conversationKey + " (change_id=?)"}},
		"comment":             {commentSQL, []any{"comment-1"}, []string{commentKey + " (id=?)"}},
		"change's repository": {changeRepoSQL, []any{"repo-1-change-000001"}, []string{changeKey + " (id=?)"}},
		"authors among": {authorsAmongSQL, []any{"repo-1-change-000001", `["seat-0","seat-2"]`},
			[]string{"SCAN json_each AS j", authorKey + " (change_id=? AND author=?)"}},
		"reviewers named": {reviewersNamedSQL, []any{"repo-1-change-000001", `["seat-0","seat-2"]`},
			[]string{"SCAN json_each AS j", reviewerKey + " (change_id=? AND reviewer=?)"}},
		"reviewers asked": {reviewersAskedSQL, []any{"repo-1-change-000001", MaxReviewersPerChange + 1},
			[]string{reviewerKey + " (change_id=?)"}},
		// A CHANGE'S ASKS MOVED: those in one state, by the reviewers' key's
		// prefix — a settling's decide; a list of them moved from one state
		// to another, driven from the list of their handles, one seek of the
		// key a handle; and the waiting and the ended counted onto the
		// conversation's row by its key, each a range of the reviewers' key.
		"asks in state": {asksInStateSQL, []any{"repo-1-change-000001", string(ReviewerRequested), MaxAsksSettled},
			[]string{reviewerKey + " (change_id=?)"}},
		"move asks": {moveAsksSQL, []any{string(ReviewerReviewed), 3000, "repo-1-change-000001",
			string(ReviewerRequested), `["seat-0","seat-2"]`}, []string{"SCAN json_each AS j",
			reviewerKey + " (change_id=? AND reviewer=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"count asks": {countAsksSQL, []any{"repo-1-change-000001"},
			[]string{conversationKey + " (change_id=?)", reviewerKey + " (change_id=?)"}},
		"resolve thread": {resolveThreadSQL, []any{true, "seat-0", "agent", "comment-1", "repo-1-change-000001"},
			[]string{commentKey + " (id=?)"}},
		// A COMMENT'S EDIT AND DELETE: the comment read and written back by its
		// key, a decide's and an apply's.
		"comment state": {commentStateSQL, []any{"comment-1"}, []string{commentKey + " (id=?)"}},
		"comment width": {commentWidthSQL, []any{"comment-1", "repo-1-change-000001"},
			[]string{commentKey + " (id=?)"}},
		"edit comment":   {editCommentSQL, []any{"Why?", 4, 3000, "comment-1"}, []string{commentKey + " (id=?)"}},
		"delete comment": {deleteCommentSQL, []any{3000, "seat-0", "agent", "comment-1"}, []string{commentKey + " (id=?)"}},
		// A CHANGE'S REVIEWS AND FINDINGS: a finding by its key — a decide's
		// and an answer's; the findings an answer names, driven from the list
		// of their ids, one seek of the key an id; a raiser's standing
		// findings, driven from the list of the standing states, one seek of
		// the raiser's index a state; a
		// reviewer's standing verdict, by their participant row's key; and a
		// review's supersession, driven from the list of the findings' ids,
		// one seek of the key an id.
		"finding": {findingSQL, []any{"finding-1"}, []string{findingKey + " (id=?)"}},
		"findings among": {findingsAmongSQL, []any{`["finding-1","finding-2"]`},
			[]string{"SCAN json_each AS j", findingKey + " (id=?)"}},
		"answer finding": {answerFindingSQL, []any{string(FindingResolved), "", "seat-0", "agent", "", 3000,
			"finding-1"}, []string{findingKey + " (id=?)"}},
		"standing findings": {standingFindingsSQL, []any{"repo-1-change-000001", "seat-0", "agent", standingStates},
			[]string{"SCAN json_each AS j", "code_findings_raiser_idx (change_id=? AND raiser=? AND state=?)"}},
		"standing verdict": {standingVerdictSQL, []any{"repo-1-change-000001", "seat-0"},
			[]string{participantKey + " (change_id=? AND handle=?)"}},
		// THE HOME'S DUE LISTING (changesdue.go): the repositories holding a
		// due change, after a cursor, walking the index partial on the live
		// states in its order, each change checked against its facts' key and
		// its mergeability's, its target's row by the refs' key and its
		// repository's state by its key; one repository's state by its key;
		// one repository's due changes after a source branch, the same
		// index's range, each joined to its target's row by the refs' key and
		// checked against its facts' key and its mergeability's; and one
		// repository's due rechecks, the same index's range joined to its
		// target's row and its mergeability by their keys, sorted by when the
		// merge was recorded (sortingStatements says why).
		"due repositories": {dueReposSQL, []any{"", MaxSnapshotItems, int64(3000), int64(5000)},
			[]string{"code_changes_open_idx (repo_id>?)", factsKey + " (change_id=? AND n=?)",
				mergeabilityKey + " (change_id=?)", conversationKey + " (change_id=?)",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=?)",
				refsKey + " (repo_id=? AND ref=?)", repoKey + " (id=?)"}},
		// THE ASKS DUE A SETTLING: the repositories holding an ended change
		// whose asks still wait, after a cursor, walking the index partial on
		// a waiting ask in its order, each change's state and its
		// repository's by their keys; one repository's such changes, the
		// same index's range; and its live changes holding asks their end
		// left, the live changes' index's range, each conversation by its
		// key.
		"due asked repositories": {dueAskedReposSQL, []any{"", MaxSnapshotItems},
			[]string{askedKey + " (repo_id>?)", changeKey + " (id=?)", repoKey + " (id=?)"}},
		"ended asks": {endedAsksSQL, []any{"repo-1", MaxAskSettlesPerRead},
			[]string{askedKey + " (repo_id=?)", changeKey + " (id=?)"}},
		"reopened asks": {reopenedAsksSQL, []any{"repo-1", MaxAskSettlesPerRead},
			[]string{"code_changes_open_idx (repo_id=?)", conversationKey + " (change_id=?)"}},
		"due repository": {dueRepoSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"due changes": {dueChangesSQL, []any{"repo-1", "refs/heads/f000001", MaxSnapshotItems, int64(5000),
			strings.Repeat("a", 64), strings.Repeat("b", 64)},
			[]string{"code_changes_open_idx (repo_id=? AND source_ref>?)", refsKey + " (repo_id=? AND ref=?)",
				factsKey + " (change_id=? AND n=?)", mergeabilityKey + " (change_id=?)",
				conversationKey + " (change_id=?)",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=?)"}},
		"due rechecks": {dueRechecksSQL, []any{"repo-1", int64(3000), MaxSnapshotItems},
			[]string{"code_changes_open_idx (repo_id=?)", refsKey + " (repo_id=? AND ref=?)",
				mergeabilityKey + " (change_id=?)"}},
		// A CHANGE'S MERGEABILITY: its row by its key, a decide's and an
		// apply's, and a read's whole; a refresh's readiness written back by
		// the same key; and the readiness of the changes a record ended
		// cleared, driven from the list of their ids, a seek of the key an
		// element.
		"mergeability": {mergeabilitySQL, []any{"repo-1-change-000001"},
			[]string{mergeabilityKey + " (change_id=?)"}},
		"mergeability row": {mergeabilityRowSQL, []any{"repo-1-change-000001"},
			[]string{mergeabilityKey + " (change_id=?)"}},
		"refresh mergeability": {refreshMergeabilitySQL, []any{4, 3, 5000, 7000, true, false, "repo-1-change-000001",
			2, "refs/heads/main", 3000}, []string{mergeabilityKey + " (change_id=?)"}},
		"keep mergeability": {keptMergeabilitySQL, []any{true, oid("t"), 6000, 7000, true, 5, 4, 5000,
			"repo-1-change-000001", 2, 1_762_000_000_000}, []string{mergeabilityKey + " (change_id=?)"}},
		"clear readiness": {clearReadinessSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", mergeabilityKey + " (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		// THE POSITION OF A HEAD'S CHECKS a mergeability's decide stamps: every
		// attempt at the commit for the change's target, a range of
		// code_checks' key.
		"head checks position": {headChecksPositionSQL, []any{"repo-1", oid("c"), "refs/heads/main"},
			[]string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref=?)"}},
		// A CHANGE'S READINESS (readiness.go): the counters, the authors, the
		// standing verdicts that approve, the standing asks and the
		// mergeabilities of a page of changes, each driven from the list of
		// their ids, one seek of a key an element; the facts of a list of
		// (change, revision) pairs, one seek of their key a pair; the cut of
		// each of a list of changes, a walk of the partial index that holds
		// their standing blockers alone; and the standing blockers of a list
		// of (change, cut) bounds, a range of that index up to each cut.
		"readiness counters": {readinessCountersSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", conversationKey + " (change_id=?)"}},
		"readiness authors": {readinessAuthorsSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", authorKey + " (change_id=?)"}},
		"readiness approvals": {readinessApprovalsSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", participantKey + " (change_id=?)"}},
		"readiness asks": {readinessAsksSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", reviewerKey + " (change_id=?)"}},
		"readiness mergeabilities": {readinessMergesSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", mergeabilityKey + " (change_id=?)"}},
		"readiness facts": {readinessFactsSQL, []any{`[["repo-1-change-000001",3],["repo-1-change-000001",1]]`},
			[]string{"SCAN json_each AS j", factsKey + " (change_id=? AND n=?)"}},
		// THE MERGE GATE (gatejudge.go), beside the readiness rows and the
		// repositories' rules ("protections of"): the head revisions' facts a
		// gate reaches, driven from the list of (change, revision) pairs, one
		// seek of their key a pair; what a list of repositories' matches are
		// held to, one seek of each one's key, its list's members a set; and
		// every attempt of a required check on
		// a head commit checked for the change's target, driven from the list
		// of (repository, commit, target, check) keys, one seek of the checks'
		// key a key.
		"gate heads": {gateHeadsSQL, []any{`[["repo-1-change-000001",3],["repo-1-change-000002",1]]`},
			[]string{"SCAN json_each AS j", factsKey + " (change_id=? AND n=?)"}},
		"gate match digests": {matchDigestsOfSQL, []any{`["repo-1","repo-2"]`}, []string{repoKey + " (id=?)"}},
		// THE MERGE QUEUE (queue.go, apply_queue.go, queueread.go): entries by
		// key, driven from a list of changes by one seek of
		// code_queue_change_idx a change, counted and read in order over
		// code_queue_live_idx; ejections by the index partial on ejections
		// that leads with what the read names, counted by the same indexes,
		// covering; the merge rate's enqueues by code_queue_enqueued_idx; a
		// target's trains by code_candidates_train_idx; landings and green
		// rows by their keys; the purge part's deletes by their table's key.
		// Each read runs in its index's order.
		"queue entry": {entrySQL, []any{"repo-1", "entry-1"},
			[]string{"sqlite_autoindex_code_queue_1 (repo_id=? AND entry_id=?)"}},
		"live entries of changes": {liveEntriesSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", "code_queue_change_idx (change_id=?)"}},
		"queued changes": {queuedChangesSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", "code_queue_change_idx (change_id=?)"}},
		"waiting entries": {waitingSQL, []any{"repo-1", "refs/heads/main", MaxQueueEntries},
			[]string{"code_queue_live_idx (repo_id=? AND target_ref=?)"}},
		"an entry's place": {placeSQL, []any{"repo-1", "refs/heads/main", 1000},
			[]string{"code_queue_live_idx (repo_id=? AND target_ref=? AND seq<?)"}},
		"a target's live entries up to a seq": {liveSeqsSQL, []any{"repo-1", "refs/heads/main", 1000},
			[]string{"code_queue_live_idx (repo_id=? AND target_ref=? AND seq<=?)"}},
		"end an entry": {endEntrySQL, []any{"dequeued", 1, 1000, "asked", "repo-1", "entry-1"},
			[]string{"sqlite_autoindex_code_queue_1 (repo_id=? AND entry_id=?)"}},
		"end a change's entry": {endChangeEntrySQL, []any{"dequeued", 1, 1000, "closed", "repo-1-change-000001"},
			[]string{"code_queue_change_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"end changes' entries": {endChangesEntriesSQL, []any{"ejected", 1, 1000, "head_moved",
			`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", "code_queue_change_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"a target's queue": {queueOfTargetSQL, []any{"repo-1", "refs/heads/main", 0, 50},
			[]string{"code_queue_live_idx (repo_id=? AND target_ref=? AND seq>?)"}},
		"a repository's queues": {queueOfRepoSQL, []any{"repo-1", 50},
			[]string{"code_queue_live_idx (repo_id=?)"}},
		"a repository's queues next": {queueOfRepoAfterSQL, []any{"repo-1", "refs/heads/main", "refs/heads/main",
			"refs/heads/main", 1000, 50}, []string{"code_queue_live_idx (repo_id=? AND target_ref>=?)"}},
		"every queue": {queueAllSQL, []any{"", 50}, []string{"code_queue_live_idx (repo_id>=?)"}},
		"every queue next": {queueAllAfterSQL, []any{"repo-1", "repo-1", "refs/heads/main", "refs/heads/main", 1000,
			50}, []string{"code_queue_live_idx (repo_id>=?)"}},
		"queue due repositories": {queueDueReposSQL, []any{"", MaxSnapshotItems},
			[]string{"code_queue_live_idx (repo_id>?)", repoKey + " (id=?)"}},
		"a target's entries enqueued since": {enqueuedSQL, []any{"repo-1", "refs/heads/main", 1000,
			MergeRateScan}, []string{"COVERING INDEX code_queue_enqueued_idx (repo_id=? AND target_ref=? AND " +
			"enqueued_at>=?)"}},
		"a repository's ejections": {ejectedOfRepoSQL, []any{"repo-1", 1000, MaxEjectionsShown},
			[]string{"code_queue_ejected_idx (repo_id=? AND ended_at>=?)"}},
		"a target's ejections": {ejectedOfTargetSQL, []any{"repo-1", "refs/heads/main", 1000, MaxEjectionsShown},
			[]string{"code_queue_target_ejected_idx (repo_id=? AND target_ref=? AND ended_at>=?)"}},
		"a repository's ejections counted": {ejectedCountOfRepoSQL, []any{"repo-1", 1000, MaxQueueCounted},
			[]string{"COVERING INDEX code_queue_ejected_idx (repo_id=? AND ended_at>=?)"}},
		"a target's ejections counted": {ejectedCountOfTargetSQL, []any{"repo-1", "refs/heads/main", 1000,
			MaxQueueCounted}, []string{"COVERING INDEX code_queue_target_ejected_idx (repo_id=? AND target_ref=? " +
			"AND ended_at>=?)"}},
		"every repository's ejections counted": {ejectedReposSQL, []any{1000, MaxQueueCounted},
			[]string{"COVERING INDEX code_queue_ejected_recent_idx (ended_at>=?)"}},
		"every repository's ejections": {ejectedEverywhereSQL, []any{1000, `["repo-1","repo-2"]`,
			MaxEjectionsShown}, []string{"SCAN json_each", "code_queue_ejected_recent_idx (ended_at>=?)"}},
		"a repository's live entries counted": {queueCountOfRepoSQL, []any{"repo-1", MaxQueueCounted},
			[]string{"COVERING INDEX code_queue_live_idx (repo_id=?)"}},
		"every repository's live entries counted": {queueCountsSQL, []any{MaxQueueCounted},
			[]string{"COVERING INDEX code_queue_live_idx (repo_id>=?)"}},
		"a target's trains": {trainsSQL, []any{"repo-1", "refs/heads/main"},
			[]string{"code_candidates_train_idx (repo_id=? AND target_ref=?)"}},
		// A GIVEN-UP TRAIN'S CHECKS (queuework.go): a repository's attempts
		// a queue queued, not yet ended, by the index partial on exactly
		// those, each commit sought among the ones its target's trains test
		// or landed and each candidate among the dequeued entries; and the
		// repositories holding one, by its leading column.
		"a repository's given-up train checks": {abandonedChecksSQL, []any{"repo-1", MaxAbandonedChecks},
			[]string{"code_checks_train_idx (repo_id=?)", untestedKey, dequeuedKey}},
		"repositories holding a given-up train's checks": {abandonedReposSQL, []any{"", MaxSnapshotItems},
			[]string{"code_checks_train_idx (repo_id>?)", untestedKey, repoKey + " (id=?)"}},
		// AN OWED CLOSE (queuework.go, decide_queue.go): a repository's landed
		// entries owing their work item's close, and the repositories holding
		// one, by the index partial on exactly those; an entry's owed close
		// read and settled by its key.
		"a repository's owed closes": {owedClosesSQL, []any{"repo-1", MaxOwedCloses},
			[]string{"code_queue_close_owed_idx (repo_id=?)"}},
		"repositories owing a close": {closeOwedReposSQL, []any{"", MaxSnapshotItems},
			[]string{"code_queue_close_owed_idx (repo_id>?)", repoKey + " (id=?)"}},
		"entries named, with their owed closes": {owedNamedSQL, []any{`["entry-1","entry-2"]`, "repo-1"},
			[]string{"SCAN json_each AS j", "sqlite_autoindex_code_queue_1 (repo_id=? AND entry_id=?)"}},
		"settle entries' closes": {settleClosesSQL, []any{`["entry-1","entry-2"]`, "repo-1"},
			[]string{"SCAN json_each AS j", "sqlite_autoindex_code_queue_1 (repo_id=? AND entry_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"a target's landings": {landingsSQL, []any{"repo-1", "refs/heads/main", 1000, 1000, -1, 200},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position>=?)"}},
		"a target's green": {greenSQL, []any{"repo-1", "refs/heads/main"},
			[]string{"sqlite_autoindex_code_green_1 (repo_id=? AND target_ref=?)"}},
		"a repository's greens": {greensSQL, []any{"repo-1"},
			[]string{"sqlite_autoindex_code_green_1 (repo_id=?)"}},
		"purge queue entries": {purgeQueueSQL, []any{"repo-1", PurgePartRows},
			[]string{"sqlite_autoindex_code_queue_1 (repo_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge candidates": {purgeCandidatesSQL, []any{"repo-1", PurgePartRows},
			[]string{"sqlite_autoindex_code_candidates_1 (repo_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge landings": {purgeLandingsSQL, []any{"repo-1", PurgePartRows},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge greens": {purgeGreenSQL, []any{"repo-1", PurgePartRows},
			[]string{"sqlite_autoindex_code_green_1 (repo_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge tip work": {purgeTipsDueSQL, []any{"repo-1", PurgePartRows},
			[]string{"sqlite_autoindex_code_tips_due_1 (repo_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all queue entries": {purgeAllQueueSQL, []any{"repo-1"},
			[]string{"sqlite_autoindex_code_queue_1 (repo_id=?)"}},
		"purge all candidates": {purgeAllCandidatesSQL, []any{"repo-1"},
			[]string{"sqlite_autoindex_code_candidates_1 (repo_id=?)"}},
		"purge all landings": {purgeAllLandingsSQL, []any{"repo-1"},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=?)"}},
		"purge all greens": {purgeAllGreenSQL, []any{"repo-1"},
			[]string{"sqlite_autoindex_code_green_1 (repo_id=?)"}},
		"purge all tip work": {purgeAllTipsDueSQL, []any{"repo-1"},
			[]string{"sqlite_autoindex_code_tips_due_1 (repo_id=?)"}},
		// THE QUEUE'S OWN RECORDS (queuerecords.go, decide_queue.go,
		// apply_queuerecords.go, queue_sweep.go): lists of entries and of their
		// changes by key, each driven from the list (upserts on a key have no
		// plan); an abandoned train over code_queue_live_idx and
		// code_candidates_train_idx; a train's pack counters by the
		// repository's key; landings by their key, and those between a green
		// row and a landing counted and marked over the key's range; the
		// queue's history, ended entries walked in key order and weighed from
		// code_queue_ended_idx alone, removed over the key's range up to the
		// last one walked; and the sweep's listing, two seeks a repository.
		"entries named": {entriesNamedSQL, []any{`["entry-1","entry-2"]`, "repo-1"},
			[]string{"SCAN json_each AS j", "sqlite_autoindex_code_queue_1 (repo_id=? AND entry_id=?)"}},
		// THE ENTRIES A PUSH EJECTED, which its committer announces
		// (decide_queue.go's announcePushEjections).
		"entries a push ejected": {pushEjectedSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`,
			1000, "ejected", "head_moved"},
			[]string{"SCAN json_each AS j", "code_queue_change_idx (change_id=?)"}},
		"changes named": {changesNamedSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", changeKey + " (id=?)"}},
		"changes moved past a position": {changesMovedPastSQL, []any{int64(0), 101},
			[]string{changesPositionKey + " (position>?)"}},
		// A QUEUE BUILD'S DESCRIPTIONS (queuework.go): its changes' bodies
		// and authors, each driven from the list by its table's key.
		"queued changes' bodies": {changeBodiesNamedSQL,
			[]any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", bodyKey + " (change_id=?)"}},
		"queued changes' authors": {changeAuthorsNamedSQL,
			[]any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", authorKey + " (change_id=?)"}},
		"abandon a train's entries": {resetTrainSQL, []any{"repo-1", "refs/heads/main"},
			[]string{"code_queue_live_idx (repo_id=? AND target_ref=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"abandon a train's candidates": {dropTrainSQL, []any{"repo-1", "refs/heads/main"},
			[]string{"code_candidates_train_idx (repo_id=? AND target_ref=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"a train's landed candidates": {dropLandedSQL, []any{"repo-1", "refs/heads/main", "train-1", 2},
			[]string{"code_candidates_train_idx (repo_id=? AND target_ref=? AND train=? AND ordinal<=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"a train at a position": {trainAtSQL, []any{"repo-1", "refs/heads/main", "train-1", 1000},
			[]string{"code_candidates_train_idx (repo_id=? AND target_ref=? AND train=?)"}},
		"a train's pack's counters": {queuePackCountersSQL, []any{1, 100, 1, 1000, "repo-1"},
			[]string{repoKey + " (id=?)"}},
		"a landing record's tip": {landingTipSQL, []any{"repo-1", "refs/heads/main", 1000},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position=?)"}},
		"who pushed a failed head": {headPusherSQL, []any{"change-1", 2},
			[]string{revisionKey + " (change_id=? AND n=?)"}},
		"where a landed revision was pushed": {revisionPositionSQL, []any{"change-1", 2},
			[]string{revisionKey + " (change_id=? AND n=?)"}},
		"the push a landing names": {landingPushSQL, []any{"repo-1-push-000001", "repo-1", 1000},
			[]string{pushKey + " (push_id=?)"}},
		"a landing": {landingSQL, []any{"repo-1", "refs/heads/main", 1000, 1},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position=? AND ordinal=?)"}},
		"a landing broken": {landingBrokenSQL, []any{"repo-1", "refs/heads/main", 1000, 1},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position=? AND ordinal=?)"}},
		"a broken landing announced": {landingNamedSQL, []any{"repo-1", "refs/heads/main", 1000, 1},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position=? AND ordinal=?)"}},
		"break a landing": {brokenLandingSQL, []any{"repo-1", "refs/heads/main", 1000, 1},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position=? AND ordinal=?)"}},
		// A TARGET'S ROW OF TIP WORK (apply_merge.go): cleared by its key as a
		// green moves past every landing, and settled where no unbroken
		// landing lies above a place — the landings' key's range above it.
		"tip work paid": {tipPaidSQL, []any{"repo-1", "refs/heads/main"},
			[]string{"sqlite_autoindex_code_tips_due_1 (repo_id=? AND target_ref=?)"}},
		"tip work settled": {settleTipSQL, []any{"repo-1", "refs/heads/main", 1000},
			[]string{"sqlite_autoindex_code_tips_due_1 (repo_id=? AND target_ref=?)",
				"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position>?)"}},
		"ended entries within": {endedWithinSQL, []any{"repo-1", 1000, QueueSweepRows + 1},
			[]string{"COVERING INDEX code_queue_ended_idx (repo_id=? AND ended_at<?)"}},
		"sweep ended entries": {sweepEndedSQL, []any{"repo-1", 1000, 1000, "entry-1"},
			[]string{"code_queue_ended_idx (repo_id=? AND ended_at<=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"landings below green": {landingsBelowSQL, []any{"repo-1", "refs/heads/main", 1000, QueueSweepRows + 1},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position<?)"}},
		"sweep landings": {sweepLandingsSQL, []any{"repo-1", "refs/heads/main", 1000, 1000, 2},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position<=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"queue sweep listing": {queueSweepDueSQL, []any{"", 1000, retentionPage},
			[]string{repoKey + " (id>?)", "code_queue_ended_idx (repo_id=? AND ended_at<?)",
				"sqlite_autoindex_code_green_1 (repo_id=?)",
				"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position<?)"}},
		"gate checks": {gateChecksSQL, []any{`[["repo-1","` + oid("c") + `","refs/heads/main","unit"],["repo-1","` +
			oid("c") + `","refs/heads/main","lint"]]`}, []string{"SCAN json_each AS j",
			checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=?)"}},
		"blocker cuts": {blockerCutsSQL, []any{changeIDs},
			[]string{"SCAN json_each AS j", blockersKey + " (change_id=?)"}},
		"standing blockers": {standingBlockersSQL,
			[]any{`[["repo-1-change-000001",3000],["repo-1-change-000002",9223372036854775807]]`},
			[]string{"SCAN json_each AS j", blockersKey + " (change_id=? AND position<=?)"}},
		// A REVISION'S FACTS: one revision's by their key, a decide's and an
		// apply's, and what its repository's row says they are matched
		// against now, by its key; whether a change holds any, by the key's
		// prefix; who
		// stands on request_changes before a revision, a range of the
		// participants' key; and the reviewers a change asks now, a range of
		// the reviewers' key.
		"facts of a revision": {factsSQL, []any{"repo-1-change-000001", 1},
			[]string{factsKey + " (change_id=? AND n=?)"}},
		"what facts are matched against": {matchDigestsSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"whether a change holds facts": {anyFactsSQL, []any{"repo-1-change-000001"},
			[]string{factsKey + " (change_id=?)"}},
		"changes requested before": {changesRequestedBeforeSQL, []any{"repo-1-change-000001", 3, MaxReReviewers},
			[]string{participantKey + " (change_id=?)"}},
		"standing asks": {standingAsksSQL, []any{"repo-1-change-000001", MaxReviewersPerChange},
			[]string{reviewerKey + " (change_id=?)"}},
		// WHOM A RECORD CONCERNS (concerns.go): a change's authors and its
		// participants, a range of each key in its order; and a thread's
		// newest comments, its range of the thread index read back from its
		// end.
		"routing authors": {routingAuthorsSQL, []any{"repo-1-change-000001", MaxRoutedParticipants},
			[]string{authorKey + " (change_id=?)"}},
		"routing watchers": {routingWatchersSQL, []any{"repo-1-change-000001", MaxRoutedParticipants},
			[]string{participantKey + " (change_id=?)"}},
		"thread participants": {threadParticipantsSQL, []any{"repo-1-change-000001", "comment-1",
			threadParticipantRows}, []string{"code_comments_thread_idx (change_id=? AND thread_id=?)"}},
		// WHOM A RECORD REACHED (apply_notices.go, noticesweep.go): a
		// change's count of its notices, by its key; the notices of a list
		// of changes a purge removes, deleted by the change index one seek
		// an element, and the oldest of the change a part cuts, a range of
		// it; a kind's sweep, a range of the swept index; and the changes a
		// repository still holds, for its final purge.
		"count notices": {countNoticesSQL, []any{3, "repo-1-change-000001"}, []string{changeKey + " (id=?)"}},
		"remove notices": {removeNoticesSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", "code_notifications_change_idx (change_id=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"remove a cut change's notices": {removeCutNoticesSQL, []any{"repo-1-change-000001", PurgePartRows},
			[]string{"code_notifications_change_idx (change_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"unquote notices": {unquoteNoticesSQL, []any{"repo-1-change-000001", 3000},
			[]string{"code_notifications_change_idx (change_id=? AND log_seq=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"sweep notices": {sweepNoticesSQL, []any{string(PartyAgent), 3000},
			[]string{"code_notifications_swept_idx (recipient_kind=? AND created_at<?)"}},
		"purged changes": {purgedChangesSQL, []any{"repo-1"}, []string{"code_changes_number_idx (repo_id=?)"}},
		"supersede findings": {supersedeFindingsSQL, []any{string(FindingSuperseded), "seat-0", "agent", 3000,
			`["finding-1","finding-2"]`}, []string{"SCAN json_each AS j", findingKey + " (id=?)",
			"INTEGER PRIMARY KEY (rowid=?)"}},
		"change body":        {changeBodySQL, []any{"repo-1-change-000002"}, []string{bodyKey + " (change_id=?)"}},
		"delete change body": {deleteBodySQL, []any{"repo-1-change-000002"}, []string{bodyKey + " (change_id=?)"}},
		"next change":        {nextChangeSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"opened":             {openedSQL, []any{3000, "repo-1"}, []string{repoKey + " (id=?)"}},
		"touched":            {touchedSQL, []any{3000, "repo-1"}, []string{repoKey + " (id=?)"}},
		// A REPOSITORY'S CHANGES COUNTED BY STATE (apply_counts.go): its row by its key, a tally
		// row by its key, the re-derivation's page of repositories and one repository's changes
		// counted over one range of its index.
		"count changes":  {changeCountsSQL, []any{1, -1, 0, 0, "repo-1"}, []string{repoKey + " (id=?)"}},
		"tally standing": {tallyStandingSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"untally": {untallySQL, []any{"company", "unit", "ENG"},
			[]string{talliesKey + " (visibility=? AND owner_kind=? AND owner=?)"}},
		"rederive repos": {rederiveReposSQL, []any{"", 1000}, []string{repoKey + " (id>?)"}},
		"unrepo tally": {unrepoTallySQL, []any{"company", "unit", "ENG", "project"},
			[]string{repoTalliesKey + " (visibility=? AND owner_kind=? AND owner=? AND kind=?)"}},
		"changes by state": {changesByStateSQL, []any{"repo-1"}, []string{changesRepoStateKey + " (repo_id=?)"}},
		// A CHANGE A PUSH OPENS: the repository's next number moved past
		// the opens of a push record, by its key; and the numbers of the
		// changes a batch's answers name, or a queue page's entries, and
		// their titles and source branches, driven from their list, one seek
		// of the changes' key an element.
		"next change after": {nextChangeAfterSQL, []any{4, "repo-1"}, []string{repoKey + " (id=?)"}},
		"change numbers": {changeNumbersSQL, []any{"repo-1", `["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", changeKey + " (id=?)"}},
		"change labels": {changeLabelsSQL, []any{"repo-1", `["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", changeKey + " (id=?)"}},
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
		"reverted by": {revertedBySQL, []any{"repo-1", 3},
			[]string{changesRevertsKey + " (repo_id=? AND reverts=?)"}},
		"change authors": {changeAuthorsSQL, []any{"repo-1-change-000001"}, []string{authorKey + " (change_id=?)"}},
		"revisions": {revisionsSQL, []any{"repo-1-change-000001", 200},
			[]string{revisionKey + " (change_id=?)", factsKey + " (change_id=? AND n=?)"}},
		"revisions after": {revisionsAfterSQL, []any{"repo-1-change-000001", 2, 200},
			[]string{revisionKey + " (change_id=? AND n<?)", factsKey + " (change_id=? AND n=?)"}},
		// A WINDOW'S WALK (changewindow.go): the changes whose lifecycle
		// moved within it and those somebody spoke of within it, each a
		// first page and a page after a cursor, newest first, the
		// conversation's partial index walked and each change sought by its
		// key; and the conversations of a page's changes, driven from their
		// list.
		"changes moved": {changesMovedSQL, []any{1000, 50}, []string{changesMovedKey + " (updated_at>=?)"}},
		"changes moved after": {changesMovedAfterSQL, []any{1000, 2030, 2030, "repo-1-change-000031", 50},
			[]string{changesMovedKey + " (updated_at<=? AND updated_at>=?)"}},
		"changes spoken of": {conversationsMovedSQL, []any{1000, 50},
			[]string{conversationsMovedKey + " (updated_at>=?)", changeKey + " (id=?)"}},
		"changes spoken of after": {conversationsMovedAfterSQL,
			[]any{1000, 2030, 2030, "repo-1-change-000031", 50},
			[]string{conversationsMovedKey + " (updated_at<=? AND updated_at>=?)", changeKey + " (id=?)"}},
		// A LIVE STATE BY ITS LAST MOVES (changesort.go): its changes by their lifecycle, every
		// repository's or one's, and the conversations by when somebody spoke, each a first page and
		// a page after a cursor.
		"live changes moved": {liveMovedSQL, []any{"open", 1000, 50},
			[]string{changesLiveMovedKey + " (state=? AND updated_at>=?)"}},
		"live changes moved after": {liveMovedAfterSQL, []any{"open", 1000, 2030, 2030, "repo-1-change-000031", 50},
			[]string{changesLiveMovedKey + " (state=? AND updated_at<=? AND updated_at>=?)"}},
		"repo live changes moved": {repoLiveMovedSQL, []any{"repo-1", "open", 1000, 50},
			[]string{changesRepoLiveMovedKey + " (repo_id=? AND state=? AND updated_at>=?)"}},
		"repo live changes moved after": {repoLiveMovedAfterSQL,
			[]any{"repo-1", "open", 1000, 2030, 2030, "repo-1-change-000031", 50},
			[]string{changesRepoLiveMovedKey + " (repo_id=? AND state=? AND updated_at<=? AND updated_at>=?)"}},
		"live changes spoken of": {spokenOfSQL, []any{1000, 1000},
			[]string{conversationsMovedKey + " (updated_at>=?)", changeKey + " (id=?)"}},
		"live changes spoken of after": {spokenOfAfterSQL, []any{1000, 2030, 2030, "repo-1-change-000031", 1000},
			[]string{conversationsMovedKey + " (updated_at<=? AND updated_at>=?)", changeKey + " (id=?)"}},
		"repo live changes spoken of": {repoSpokenOfSQL, []any{"repo-1", 1000, 1000},
			[]string{conversationsRepoMovedKey + " (repo_id=? AND updated_at>=?)", changeKey + " (id=?)"}},
		"repo live changes spoken of after": {repoSpokenOfAfterSQL,
			[]any{"repo-1", 1000, 2030, 2030, "repo-1-change-000031", 1000},
			[]string{conversationsRepoMovedKey + " (repo_id=? AND updated_at<=? AND updated_at>=?)",
				changeKey + " (id=?)"}},
		"conversations of changes": {conversationsOfSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", conversationKey + " (change_id=?)"}},
		"listed repos": {listedReposSQL, []any{`["repo-1","repo-2"]`}, []string{repoKey + " (id=?)"}},
		// THE COUNTS' READS (counts.go): every tally row, and the facts of a page of repositories.
		"change tallies": {changeTalliesSQL, nil, []string{"SCAN code_change_tallies"}},
		"repo tallies":   {repoTalliesSQL, nil, []string{"SCAN code_repo_tallies"}},
		"repo facts":     {repoFactsOfSQL, []any{`["repo-1","repo-2"]`}, []string{repoKey + " (id=?)"}},
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
		"changes waiting": {changesWaitingSQL, []any{50}, []string{waitsKey + " (waits_person=?)"}},
		"changes waiting after": {changesWaitingAfterSQL, []any{"repo-1-change-000031", 50},
			[]string{waitsKey + " (waits_person=? AND change_id<?)"}},
		"changes ready": {changesReadySQL, []any{50}, []string{readyKey + " (ready=?)"}},
		"changes ready after": {changesReadyAfterSQL, []any{"repo-1-change-000031", 50},
			[]string{readyKey + " (ready=? AND change_id<?)"}},
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
		// ONE REVIEWER'S REVIEWS newest first (the `reviews` listing), a
		// range of the reviewer's index walked backward, and WHOM ONE
		// CHANGE'S RECORDS REACHED (the `change_routing` read), a range of
		// the change's notices walked backward, each first and after a
		// cursor.
		// A COMMIT'S CHANGES (the `commit` read): those it merged, a range
		// of the merge index on the repository and the commit, and the
		// revisions it heads, a range of the heads index on the commit, each
		// kept to the repository by its change's key.
		"changes a commit merged": {commitMergedSQL, []any{"repo-1", oid("head-3-3"), MaxCommitChanges + 1},
			[]string{commitMergedKey + " (repo_id=? AND merge_oid=?)"}},
		"revisions a commit heads": {commitHeadsSQL, []any{oid("head-1-1"), "repo-1", MaxCommitChanges + 1},
			[]string{commitHeadsKey + " (head_oid=?)", changeKey + " (id=?)"}},
		// A HISTORY PAGE'S MARKS AND A MERGE'S GATE (commitmarks.go, commitread.go,
		// checkrollup.go): each check's latest attempt, a merged change's approvals
		// and the landing a queue made of it, each one walk or seek.
		"a commit's latest checks, every branch": {latestChecksOfCommitSQL,
			[]any{"repo-1", oid("c"), MaxMarkTargets*maxRollupChecks + 1},
			[]string{checksKey + " (repo_id=? AND commit_oid=?)",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt>?)"}},
		"a commit's latest checks for a branch": {latestChecksOfTargetSQL,
			[]any{"repo-1", oid("c"), "refs/heads/main", maxRollupChecks + 1},
			[]string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref=?)",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt>?)"}},
		"a merged change's approvals": {changeApprovalsSQL, []any{"repo-1-change-000001", 1, MaxGateApprovals + 1},
			[]string{participantKey + " (change_id=?)"}},
		"when a merged revision was pushed": {revisionPushedSQL, []any{"repo-1-change-000001", 1},
			[]string{revisionKey + " (change_id=? AND n=?)"}},
		"a change's landing": {changeLandingSQL, []any{"repo-1-change-000001"},
			[]string{changeLandingsKey + " (change_id=?)"}},
		// A LISTING'S CARDS (changecards.go): its conversations' counters, standing verdicts and
		// waiting asks, and each live change's newest queue entry, driven from the page's ids.
		"card counts": {cardCountsSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", conversationKey + " (change_id=?)"}},
		"card verdicts": {cardVerdictsSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", participantKey + " (change_id=?)"}},
		"card asks": {cardAsksSQL, []any{`["repo-1-change-000001","repo-1-change-000002"]`},
			[]string{"SCAN json_each AS j", reviewerKey + " (change_id=?)"}},
		"card ejections": {cardEjectionsSQL, []any{`[["repo-1","repo-1-change-000001"]]`},
			[]string{"SCAN json_each AS j", queueKey + " (repo_id=? AND entry_id=?)",
				"code_queue_change_idx (change_id=?)"}},
		"reviews given": {reviewsGivenSQL, []any{"seat-0", 50},
			[]string{reviewsGivenKey + " (reviewer=?)"}},
		"reviews given after": {reviewsGivenAfterSQL, []any{"seat-0", 3000, 50},
			[]string{reviewsGivenKey + " (reviewer=? AND position<?)"}},
		// THE PUSHES COUNTED BY DAY AND BY PUSHER (apply_activity.go, activity.go): the moves' prunes, the
		// re-derivation's walks and reads, a purge's deletes, and the folds at the asker's bounds.
		"prune push days": {prunePushDaysSQL, []any{"repo-1", "[1,2]"},
			[]string{"SCAN json_each AS j", pushDaysKey + " (repo_id=? AND day=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"prune pusher days": {prunePusherDaysSQL, []any{"repo-1", `[[1,"agent","dev"]]`},
			[]string{"SCAN json_each AS j", pusherDaysKey + " (repo_id=? AND day=? AND pusher_kind=? AND pusher=?)",
				"INTEGER PRIMARY KEY (rowid=?)"}},
		"push rows counted":           {pushRowsCountedSQL, []any{"repo-1"}, []string{"code_pushes_repo_idx (repo_id=?)"}},
		"held push days":              {heldPushDaysSQL, []any{"repo-1"}, []string{pushDaysKey + " (repo_id=?)"}},
		"held pusher days":            {heldPusherDaysSQL, []any{"repo-1"}, []string{pusherDaysKey + " (repo_id=?)"}},
		"next pushed repository":      {nextPushedRepoSQL, []any{""}, []string{"COVERING INDEX code_pushes_repo_idx (repo_id>?)"}},
		"next push days repository":   {nextPushDaysRepoSQL, []any{""}, []string{"COVERING INDEX " + pushDaysKey + " (repo_id>?)"}},
		"next pusher days repository": {nextPusherDaysRepoSQL, []any{""}, []string{"COVERING INDEX " + pusherDaysKey + " (repo_id>?)"}},
		"purge left push days": {purgeLeftPushDaysSQL, []any{"repo-1", PurgePartRows},
			[]string{"COVERING INDEX " + pushDaysKey + " (repo_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge left pusher days": {purgeLeftPusherDaysSQL, []any{"repo-1", PurgePartRows},
			[]string{"COVERING INDEX " + pusherDaysKey + " (repo_id=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all push days":   {purgeAllPushDaysSQL, []any{"repo-1"}, []string{pushDaysKey + " (repo_id=?)"}},
		"purge all pusher days": {purgeAllPusherDaysSQL, []any{"repo-1"}, []string{pusherDaysKey + " (repo_id=?)"}},
		"push days of": {pushDaysOfSQL, []any{`["repo-1","repo-2"]`, 0, 900000000},
			[]string{"SCAN json_each AS j", pushDaysKey + " (repo_id=? AND day>=? AND day<?)"}},
		"pusher days of": {pusherDaysOfSQL, []any{"repo-1", 0, 86400000000},
			[]string{pusherDaysKey + " (repo_id=? AND day>=? AND day<?)"}},
		// A PUSH HISTORY NARROWED AND ITS CHANGES (pushfilter.go, pushmerged.go): a pusher's days, a push at or below a
		// place, a window and a stretch weighed from the index alone, a window's rows, and the changes pushes merged.
		"a pusher's days": {pusherGroupsSQL, []any{"repo-1", "agent", "dev", 0, int64(math.MaxInt64)},
			[]string{"COVERING INDEX " + pushDaysKey + " (repo_id=? AND day>=? AND day<?)",
				pusherDaysKey + " (repo_id=? AND day=? AND pusher_kind=? AND pusher=?)"}},
		"a jump's next pushes": {jumpNearSQL, []any{"repo-1", 100, 0, narrowWindowRows},
			[]string{"COVERING INDEX code_pushes_repo_idx (repo_id=? AND position<?)"}},
		"a push at or below": {pushAtOrBelowSQL, []any{"repo-1", 100},
			[]string{"COVERING INDEX code_pushes_repo_idx (repo_id=? AND position<=?)"}},
		"a narrowed window weighed": {narrowWeighSQL, []any{"repo-1", 100, "p", narrowWindowRows, 0},
			[]string{"COVERING INDEX code_pushes_repo_idx (repo_id=? AND position<=?)"}},
		"a narrowed window's weights": {narrowWeightsSQL, []any{"repo-1", 100, "p", narrowWindowRows},
			[]string{"COVERING INDEX code_pushes_repo_idx (repo_id=? AND position<=?)"}},
		"a narrowed window's last": {narrowLastSQL, []any{"repo-1", 100, "p", narrowWindowRows - 1},
			[]string{"COVERING INDEX code_pushes_repo_idx (repo_id=? AND position<=?)"}},
		"a narrowed stretch weighed": {narrowSpanSQL, []any{"repo-1", 100, "p", 10, ""},
			[]string{"COVERING INDEX code_pushes_repo_idx (repo_id=? AND position>=? AND position<=?)"}},
		"narrowed to a window": {narrowedWindowSQL, []any{"repo-1", 100, "p", 10, "", 0, int64(math.MaxInt64)},
			[]string{"code_pushes_repo_idx (repo_id=? AND position<=? AND position>=?)"}},
		"narrowed to a seat or a person": {narrowedPartySQL, append([]any{"repo-1", 100, "p", 10, "", 0,
			int64(math.MaxInt64), "agent", "dev"}, viaSlots(PusherAgent)...),
			[]string{"code_pushes_repo_idx (repo_id=? AND position<=? AND position>=?)"}},
		"narrowed to Gitlet's": {narrowedGitletSQL, append([]any{"repo-1", 100, "p", 10, "", 0,
			int64(math.MaxInt64)}, viaSlots(PusherGitlet)...),
			[]string{"code_pushes_repo_idx (repo_id=? AND position<=? AND position>=?)"}},
		"the changes pushes merged": {pushMergedSQL, []any{"repo-1", `["` + oid("m") + `"]`},
			[]string{"SCAN json_each AS j", commitMergedKey + " (repo_id=? AND merge_oid=?)"}},
		// ONE PERSON'S CODE INBOX (inboxread.go, marks.go): a range of their
		// notices newest first, every filter bound — under `unread` its lower
		// bound the seen-through position — and the notices an unread mark
		// brings back below it, one seek of the notice's key a pair; each
		// change the page names is changesByIDSQL's row (its readiness the
		// listing's statements), its description by the body's key; each one's open
		// agent findings, a covering seek of the index partial on them alone
		// (replicated 0137); the person's marks by
		// their key; and where the notices of theirs a mark names sit, one
		// seek of the notice's key a pair of the gesture's list.
		"an inbox page": {inboxScanSQL, []any{"founder", int64(1) << 62, 0, 0, "[]", 0, "[]", "[]", "", 0,
			"exclude", "[]", "[]", 50},
			[]string{"code_notifications_inbox_idx (recipient=? AND log_seq>? AND log_seq<?)"}},
		"the notices an unread page brings back below the position": {inboxMarkedSQL,
			[]any{`[["record-1","repo-1-change-000001"]]`, "founder", int64(1) << 62, 0, 0, "[]", "exclude", "[]",
				"[]"},
			[]string{"SCAN json_each AS j",
				"sqlite_autoindex_code_notifications_1 (record_id=? AND change_id=? AND recipient=?)"}},
		"one notice of an inbox, by its key": {inboxRecordSQL, []any{"record-1", "repo-1-change-000001", "founder",
			int64(1) << 62, 0, 0, "[]", 1, "[]", "[]", "", 0, "exclude", "[]", "[]"},
			[]string{"sqlite_autoindex_code_notifications_1 (record_id=? AND change_id=? AND recipient=?)"}},
		"the descriptions an inbox page names": {noticeBodiesSQL, []any{`["repo-1-change-000001"]`},
			[]string{"SCAN json_each AS j", bodyKey + " (change_id=?)"}},
		"the agent findings an inbox page names": {noticeFindingsSQL, []any{`["repo-1-change-000001"]`},
			[]string{"SCAN json_each AS j", "COVERING INDEX " + openAgentFindingsKey + " (change_id=?)"}},
		"a person's code marks": {personMarksSQL, []any{"founder"},
			[]string{"sqlite_autoindex_code_person_marks_1 (handle=?)"}},
		"where the notices a mark names sit": {noticePositionsSQL,
			[]any{`[["record-1","repo-1-change-000001"],["record-2","repo-1-change-000002"]]`, "founder"},
			[]string{"SCAN json_each AS j",
				"sqlite_autoindex_code_notifications_1 (record_id=? AND change_id=? AND recipient=?)"}},
		"a repository's viewed key": {viewedKeySQL, []any{"repo-1"}, []string{viewedKeysKey + " (repo_id=?)"}},
		"a person's viewed marks a gesture names": {viewedNamedSQL,
			[]any{"founder", "repo-1-change-000001", `["digest-a","digest-b"]`},
			[]string{"SCAN json_each AS j", viewedFilesKey + " (handle=? AND change_id=? AND path_digest=?)"}},
		"the ended changes a person's marks name": {viewedEndedSQL, []any{"founder", MaxViewedForget},
			[]string{"COVERING INDEX " + viewedFilesKey + " (handle=?)", changeKey + " (id=?)"}},
		"a change's viewed marks": {viewedPageSQL, []any{"founder", "repo-1-change-000001", 500},
			[]string{viewedFilesKey + " (handle=? AND change_id=?)"}},
		"a change's viewed marks after": {viewedPageAfterSQL,
			[]any{"founder", "repo-1-change-000001", "digest-a", 500},
			[]string{viewedFilesKey + " (handle=? AND change_id=? AND path_digest>?)"}},
		"forget a person's marks of ended changes": {forgetViewedSQL,
			[]any{"founder", `["repo-1-change-000001"]`},
			[]string{"SCAN json_each AS j", viewedFilesKey + " (handle=? AND change_id=?)"}},
		"take back a person's viewed marks": {unviewSQL, []any{"founder", "repo-1-change-000001", `["digest-a"]`},
			[]string{"SCAN json_each AS j", viewedFilesKey + " (handle=? AND change_id=? AND path_digest=?)"}},
		"count a person's viewed marks": {viewedCountSQL, []any{"founder"},
			[]string{"COVERING INDEX " + viewedFilesKey + " (handle=?)"}},
		"drop a person's oldest viewed marks": {dropOldestViewedSQL, []any{"founder", 3},
			[]string{viewedFilesKey + " (handle=?)"}},
		"whom a change reached": {changeNoticesSQL, []any{"repo-1-change-000001"},
			[]string{"code_notifications_change_idx (change_id=?)"}},
		"whom a change reached after": {changeNoticesAfterSQL, []any{"repo-1-change-000001", 3000},
			[]string{"code_notifications_change_idx (change_id=? AND log_seq<?)"}},
		"findings of a change": {findingsOfSQL, []any{"repo-1-change-000001", 50},
			[]string{findingTextKey + " (change_id=?)", findingKey + " (id=?)", reviewKey + " (id=?)"}},
		"findings of a change after": {findingsOfAfterSQL, []any{"repo-1-change-000001", "finding-1", 50},
			[]string{findingTextKey + " (change_id=? AND finding_id>?)", findingKey + " (id=?)",
				reviewKey + " (id=?)"}},
		// THE FINDINGS A WRITE NAMES, an apply's: each id one seek of the
		// texts' whole key and one of the row's, whatever the change holds.
		"findings a read names": {findingsNamedSQL,
			[]any{`["finding-1","finding-2"]`, "repo-1-change-000001"},
			[]string{"SCAN json_each AS j", findingTextKey + " (change_id=? AND finding_id=?)",
				findingKey + " (id=?)", reviewKey + " (id=?)"}},
		"reviewers of a change": {reviewersOfSQL, []any{"repo-1-change-000001", 200},
			[]string{reviewerKey + " (change_id=?)"}},
		"reviewers of a change after": {reviewersOfAfterSQL, []any{"repo-1-change-000001", "seat-0", 200},
			[]string{reviewerKey + " (change_id=? AND reviewer>?)"}},
		// A CHANGE'S QUEUE ENTRIES (changequeue.go), in queue order on its index.
		"queue entries of a change": {queueOfChangeSQL, []any{"repo-1-change-000001", int64(0), 50},
			[]string{"code_queue_change_idx (change_id=? AND seq>?)"}},
		// A CHANGE'S MOVES (changemoves.go), oldest first on the table's key.
		"moves of a change": {movesOfSQL, []any{"repo-1-change-000001", 100},
			[]string{movesKey + " (change_id=?)"}},
		"moves of a change after": {movesOfAfterSQL, []any{"repo-1-change-000001", int64(7), "closed", 100},
			[]string{movesKey + " (change_id=? AND position>=?)"}},
		// A CHANGE'S PAGE (changepage.go): its participants and their count, one notice of each
		// record behind a page of its reviewers, and its findings by path.
		"participants of a change": {participantsOfSQL, []any{"repo-1-change-000001", MaxDetailParticipants + 1},
			[]string{participantKey + " (change_id=?)"}},
		"participants counted": {participantsCountSQL, []any{"repo-1-change-000001"},
			[]string{"COVERING INDEX " + participantKey + " (change_id=?)"}},
		"when a page's reviewers last moved": {reviewersMovedAtSQL, []any{"repo-1-change-000001", "[3000,3001]"},
			[]string{"SCAN json_each AS j", "code_notifications_change_idx (change_id=? AND log_seq=?)"}},
		"findings by path": {findingPathsSQL, []any{"repo-1-change-000001", 500},
			[]string{"COVERING INDEX " + findingPathsKey + " (change_id=?)"}},
		"findings by path after": {findingPathsAfterSQL, []any{"repo-1-change-000001", "a.go", "open", 1, 500},
			[]string{"COVERING INDEX " + findingPathsKey + " (change_id=? AND path>=?)"}},
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
		"purge the viewed key":  {purgeViewedKeySQL, []any{"repo-1"}, []string{viewedKeysKey + " (repo_id=?)"}},
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
		"commit counters":      {commitCountersSQL, []any{1, 1, 1, 1, 1, 1, 1, "", 0, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"scan":                 {scanSQL, []any{1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"expunge due":          {expungeDueSQL, []any{"", MaxSnapshotItems}, []string{"code_repos_expunging_idx (id>?)"}},
		// THE WEEKLY ARM of the same listing, least recently judged first
		// up to an instant, from its start and after a cursor's row.
		"expunge weekly": {expungeWeeklySQL, []any{2, MaxSnapshotItems},
			[]string{"code_repos_expunge_due_idx (expunge_scanned_at<=?)"}},
		"expunge weekly after": {expungeWeeklyAfterSQL, []any{2, 1, 1, "repo-1", MaxSnapshotItems},
			[]string{"code_repos_expunge_due_idx (expunge_scanned_at>=? AND expunge_scanned_at<=?)"}},
		// THE REPOSITORIES SEARCHED BY INDEX, a Gitlet node's indexer's
		// listing, by name over code_repos_search_idx.
		"indexed": {indexedSQL, []any{string(SearchIndexed), "", MaxSnapshotItems},
			[]string{"code_repos_search_idx (search=? AND name>?)"}},
		// THE ACTIVE PROJECT REPOSITORIES' DEFAULT TIPS, the atlas's scan's
		// listing by name, and one of them by its key.
		"default tips": {defaultTipsSQL, []any{"", string(RepoProject), MaxSnapshotItems},
			[]string{"code_repos_name_idx (name>?)", refsKey + " (repo_id=? AND ref=?)"}},
		"default tip": {defaultTipSQL, []any{"repo-1", string(RepoProject)},
			[]string{repoKey + " (id=?)", refsKey + " (repo_id=? AND ref=?)"}},
		// THE HOME'S LISTINGS OF WHAT IT OWES OTHER HOSTS (replicated 0123's
		// indexes): the repositories being imported by id, the push mirrors
		// by id and the pull mirrors least recently synced first — each a
		// keyset page, its partial predicate repeated verbatim.
		"imports due": {importsDueSQL, []any{"", MaxSnapshotItems},
			[]string{"code_repos_importing_idx (id>?)"}},
		"push mirrors due": {pushMirrorsSQL, []any{"", MaxSnapshotItems},
			[]string{"code_repos_mirror_to_idx (id>?)"}},
		"pull mirrors due": {pullMirrorsSQL, []any{0, 0, "", MaxSnapshotItems},
			[]string{"code_repos_mirrored_idx (mirror_synced_at>=?)"}},
		// AN IMPORT'S AND A MIRROR'S APPLY: the repository's row by its key.
		"remote state":  {remoteStateSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"import seq":    {importSeqSQL, []any{1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"import done":   {importDoneSQL, []any{1, "", "", "", "", 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"mirror synced": {mirrorSyncedSQL, []any{1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		// A REPOSITORY'S CHECKS': the join's read of one row by its whole
		// key, and its log's by the same; the oldest rows past a cutoff —
		// the horizon's, a sweep's decide's and a purge part's — weighed by
		// a covering read of code_checks_finished_idx and deleted in its
		// order, their logs through them; the final purge's two; and the
		// retention duty's listing, one row an instant and a repository.
		"check row":            {readCheckSQL, []any{"repo-1", oid("c"), "refs/heads/main", "unit", 1}, []string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)"}},
		"drop check log":       {dropCheckLogSQL, []any{"repo-1", oid("c"), "refs/heads/main", "unit", 1}, []string{checkLogsKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)"}},
		"checks oldest":        {checksOldestSQL, []any{"repo-1", 1, 1000, CheckHorizonRows + 1}, []string{"COVERING INDEX " + checksEndedKey + " (repo_id=? AND finished_at>=? AND finished_at<?)"}},
		"delete oldest logs":   {deleteOldestCheckLogsSQL, []any{"repo-1", 1, 1000, CheckHorizonRows}, []string{"COVERING INDEX " + checksEndedKey + " (repo_id=? AND finished_at>=? AND finished_at<?)", checkLogsKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"delete oldest checks": {deleteOldestChecksSQL, []any{"repo-1", 1, 1000, CheckHorizonRows}, []string{"COVERING INDEX " + checksEndedKey + " (repo_id=? AND finished_at>=? AND finished_at<?)", "INTEGER PRIMARY KEY (rowid=?)"}},
		"purge all check logs": {purgeAllCheckLogsSQL, []any{"repo-1"}, []string{checkLogsKey + " (repo_id=?)"}},
		"purge all checks":     {purgeAllChecksSQL, []any{"repo-1"}, []string{checksKey + " (repo_id=?)"}},
		"checks due":           {checksDueSQL, []any{1000, -1, -1, "", 100}, []string{"COVERING INDEX code_checks_recent_idx (finished_at>=? AND finished_at<?)"}},
		// A REPOSITORY'S CHECKS, READ (checksread.go): an attempt and its log
		// by their keys; the latest attempt of a check read backwards over the
		// key's prefix; a commit's attempts after a place on the key, keeping
		// each check's latest by one seek of its later attempts; a
		// repository's newest ended first on code_checks_finished_idx; the
		// window on code_checks_recent_idx; a narrowed listing's places, each
		// a covering seek of the index leading with its narrowing (state, or
		// check and state); a standing listing's unended places, a covering
		// seek of the partial index; the facts, rows, later attempts, live
		// changes, entries and tips a page's lists drive, by their keys; and
		// the home's due work, a head tested by its dispatch's and target's keys.
		"check listed":       {checkRowSQL, []any{"repo-1", oid("c"), "refs/heads/main", "unit", 1}, []string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)"}},
		"check whole":        {checkWholeSQL, []any{"repo-1", oid("c"), "refs/heads/main", "unit", 1}, []string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)"}},
		"check log":          {checkLogSQL, []any{"repo-1", oid("c"), "refs/heads/main", "unit", 1}, []string{checkLogsKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)"}},
		"latest check":       {latestCheckSQL, []any{"repo-1", oid("c"), "refs/heads/main", "unit"}, []string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=?)"}},
		"checks of a commit": {checksOfCommitSQL, []any{"repo-1", oid("c"), "", "", 0, 50, "unit", "failed"}, []string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref>=?)"}},
		"checks of a target": {checksOfTargetSQL, []any{"repo-1", oid("c"), "refs/heads/main", "", 0, 50, "", ""}, []string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name>=?)"}},
		"latest of a commit": {latestOfCommitSQL, []any{"repo-1", oid("c"), "", "", 0, 64, "", "failed"},
			[]string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref>=?)",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt>?)"}},
		"latest of a target": {latestOfTargetSQL, []any{"repo-1", oid("c"), "refs/heads/main", "", 0, 64, "unit", ""},
			[]string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name>=?)",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt>?)"}},
		"checks of a repo":      {checksOfRepoSQL, []any{"repo-1", 50}, []string{checksEndedKey + " (repo_id=?)"}},
		"checks of a repo next": {checksOfRepoAfterSQL, []any{"repo-1", 1000, oid("c"), "refs/heads/main", "unit", 1, 50}, []string{checksEndedKey + " (repo_id=? AND finished_at<=?)"}},
		"checks window":         {checksWindowSQL, []any{1, 1000, 50}, []string{"code_checks_recent_idx (finished_at<? AND finished_at>=?)"}},
		"checks window next":    {checksWindowAfterSQL, []any{1, 1000, "repo-1", oid("c"), "refs/heads/main", "unit", 1, 50}, []string{"code_checks_recent_idx (finished_at<=? AND finished_at>=?)"}},
		"repo checks in a state": {repoStatePlacesSQL, []any{"repo-1", "running", int64(math.MaxInt64), "", "", "", 0, 50},
			[]string{"COVERING INDEX code_checks_state_idx (repo_id=? AND state=? AND finished_at<=?)"}},
		"repo checks of a name": {repoNamePlacesSQL, []any{"repo-1", "unit", "failed", 1000, oid("c"), "refs/heads/main", 1, 50},
			[]string{"COVERING INDEX code_checks_name_idx (repo_id=? AND name=? AND state=? AND finished_at<=?)"}},
		"window checks in a state": {windowStatePlacesSQL, []any{"failed", 1, 1000, "", "", "", "", 0, 50},
			[]string{"COVERING INDEX code_checks_state_recent_idx (state=? AND finished_at<=? AND finished_at>=?)"}},
		"window checks of a name": {windowNamePlacesSQL, []any{"unit", "passed", 1, 1000, "repo-1", oid("c"), "refs/heads/main", 1, 50},
			[]string{"COVERING INDEX code_checks_name_recent_idx (name=? AND state=? AND finished_at<=? AND finished_at>=?)"}},
		"window checks not yet ended": {windowUnendedPlacesSQL, []any{"running", int64(math.MaxInt64), "", "", "", "", 0, "", 50},
			[]string{"COVERING INDEX code_checks_unended_idx (state=? AND finished_at<=?)"}},
		"standing checks' facts": {standingFactsSQL, []any{`[["repo-1","` + oid("c") + `","refs/heads/main","unit",1]]`},
			[]string{"SCAN json_each AS j",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt>?)"}},
		"standing checks' rows": {standingRowsSQL, []any{`[["repo-1","` + oid("c") + `","refs/heads/main","unit",1]]`},
			[]string{"SCAN json_each AS j",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)"}},
		"standing checks' heads": {standingHeadsSQL, []any{`[["repo-1","refs/heads/main","` + oid("c") + `"]]`},
			[]string{"SCAN json_each AS j", "code_changes_target_idx (repo_id=? AND target_ref=? AND head_oid=?)"}},
		"standing checks' entries": {standingEntriesSQL, []any{`[["repo-1","entry-1"]]`},
			[]string{"SCAN json_each AS j", queueKey + " (repo_id=? AND entry_id=?)", "code_queue_change_idx (change_id=? AND seq>?)"}},
		"standing checks' tips": {standingTipsSQL, []any{`[["repo-1","refs/heads/main"]]`},
			[]string{"SCAN json_each AS j", refsKey + " (repo_id=? AND ref=?)"}},
		// THE HOME'S CHECK WORK (checksread.go, checksdispatch.go): a head's
		// dispatch by the two rows' keys, its requeue by the partial index
		// holding only the attempts their infrastructure ended and the next
		// attempt by code_checks' key, in the listing of repositories and in
		// one repository's; then what a listed head owes — its attempts for
		// its target, a range of code_checks' key; its change's attempts not
		// yet ended, by the partial index holding only those, each kept unless
		// a live change into its target heads its commit; and the next
		// attempt of each its infrastructure ended.
		"due check repositories": {dueCheckReposSQL, []any{"", MaxSnapshotItems, 1000, AutoRequeueAttempts},
			[]string{"code_changes_open_idx (repo_id>?)", refsKey + " (repo_id=? AND ref=?)",
				checkDispatchesKey + " (repo_id=? AND commit_oid=? AND target_ref=?)",
				checkTargetsKey + " (repo_id=? AND target_ref=?)",
				"code_checks_requeue_idx (repo_id=? AND commit_oid=? AND target_ref=?)",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)",
				repoKey + " (id=?)"}},
		"which of a schedule's turns moved code": {turnsMovedSQL, []any{`["t-1","t-2"]`},
			[]string{"code_pushes_turn_idx (turn_id=?)", "code_changes_turn_idx (created_turn=?)",
				"code_reviews_turn_idx (turn_id=?)"}},
		"checks backlog": {checksBacklogSQL, []any{1000, BacklogScan + 1}, []string{"code_checks_overdue_idx (overdue_at>? AND overdue_at<?)", repoKey + " (id=?)"}},
		// A RUNNER'S CLAIM WALK and A HOME'S SWEEP (checksread.go): the
		// queued attempts oldest queued first after a place, each tested and
		// named by its repository's key; the repositories holding a running
		// attempt after an id; and one repository's running attempts.
		"queued checks": {queuedChecksSQL, []any{0, "", "", "", "", 0, MaxSnapshotItems},
			[]string{"code_checks_queued_idx (queued_at>=?)", repoKey + " (id=?)"}},
		"running check repositories": {runningReposSQL, []any{"", MaxSnapshotItems},
			[]string{"code_checks_running_idx (repo_id>?)"}},
		"running checks": {runningChecksSQL, []any{"repo-1", MaxSnapshotItems},
			[]string{"code_checks_running_idx (repo_id=?)"}},
		"due check heads": {dueCheckChangesSQL, []any{"repo-1", "refs/heads/f000001", MaxSnapshotItems,
			AutoRequeueAttempts},
			[]string{"code_changes_open_idx (repo_id=? AND source_ref>?)", refsKey + " (repo_id=? AND ref=?)",
				checkDispatchesKey + " (repo_id=? AND commit_oid=? AND target_ref=?)",
				checkTargetsKey + " (repo_id=? AND target_ref=?)",
				"code_checks_requeue_idx (repo_id=? AND commit_oid=? AND target_ref=?)",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)"}},
		"touched paths": {touchedPathsSQL, []any{"repo-1-change-000001", 1, "refs/heads/main"},
			[]string{factsKey + " (change_id=? AND n=?)"}},
		"head attempts": {headAttemptsSQL, []any{"repo-1", oid("c"), "refs/heads/main"},
			[]string{checksKey + " (repo_id=? AND commit_oid=? AND target_ref=?)"}},
		"stale checks": {staleChecksSQL, []any{"repo-1-change-000001", oid("c"), "refs/heads/main", "repo-1"},
			[]string{"code_checks_live_idx (change_id=?)",
				"code_changes_target_idx (repo_id=? AND target_ref=? AND head_oid=?)"}},
		"requeues": {requeuesSQL, []any{"repo-1", oid("c"), "refs/heads/main", AutoRequeueAttempts},
			[]string{"code_checks_requeue_idx (repo_id=? AND commit_oid=? AND target_ref=?)",
				checksKey + " (repo_id=? AND commit_oid=? AND target_ref=? AND name=? AND attempt=?)"}},
		"due check targets": {dueCheckTargetsSQL, []any{"repo-1", 1000, MaxSnapshotItems},
			[]string{checkTargetsKey + " (repo_id=?)", refsKey + " (repo_id=? AND ref=?)",
				"code_changes_target_idx (repo_id=? AND target_ref=?)"}},
		// A PROTECTED TARGET'S TIP WORK (tipwork.go): the repositories, and
		// one repository's targets, owing tip work, by the key of its rows —
		// one a target — each repository tested by its key; a target's
		// window above its green, a range of the landings' key, and its
		// newest, the key read backwards; and its round in flight, by the
		// index partial on its tip attempts not yet ended.
		"due tip repositories": {dueTipReposSQL, []any{"", MaxSnapshotItems},
			[]string{"sqlite_autoindex_code_tips_due_1 (repo_id>?)", repoKey + " (id=?)"}},
		"due tip targets": {dueTipTargetsSQL, []any{"repo-1", "refs/heads/main", MaxSnapshotItems},
			[]string{"sqlite_autoindex_code_tips_due_1 (repo_id=? AND target_ref>?)"}},
		"tip landings": {tipLandingsSQL, []any{"repo-1", "refs/heads/main", 1000, 1000, -1, MaxTipLandings + 1},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=? AND position>=?)"}},
		"newest landing": {newestLandingSQL, []any{"repo-1", "refs/heads/main"},
			[]string{"sqlite_autoindex_code_landings_1 (repo_id=? AND target_ref=?)"}},
		"a tip round in flight": {tipInFlightSQL, []any{"repo-1", "refs/heads/main", oid("c")},
			[]string{"code_checks_tip_live_idx (repo_id=? AND target_ref=?)"}},
		// THE HOME'S DISPATCHES (checksdispatch.go): a target's reading and
		// a head's dispatch, each by its whole key — the join's read and the
		// decide's — and a purge's, by the repository's prefix of each key.
		"check target":            {readCheckTargetSQL, []any{"repo-1", "refs/heads/main"}, []string{checkTargetsKey + " (repo_id=? AND target_ref=?)"}},
		"check target defs":       {checkTargetDefsSQL, []any{"repo-1", "refs/heads/main"}, []string{checkTargetsKey + " (repo_id=? AND target_ref=?)"}},
		"check dispatch":          {readCheckDispatchSQL, []any{"repo-1", oid("c"), "refs/heads/main"}, []string{checkDispatchesKey + " (repo_id=? AND commit_oid=? AND target_ref=?)"}},
		"check dispatch defs":     {checkDispatchDefsSQL, []any{"repo-1", oid("c"), "refs/heads/main"}, []string{checkDispatchesKey + " (repo_id=? AND commit_oid=? AND target_ref=?)"}},
		"purge some dispatches":   {purgeSomeDispatchesSQL, []any{"repo-1", PurgePartRows}, []string{checkDispatchesKey + " (repo_id=?)"}},
		"purge some targets":      {purgeSomeCheckTargetsSQL, []any{"repo-1", PurgePartRows}, []string{checkTargetsKey + " (repo_id=?)"}},
		"purge all dispatches":    {purgeAllDispatchesSQL, []any{"repo-1"}, []string{checkDispatchesKey + " (repo_id=?)"}},
		"purge all check targets": {purgeAllCheckTargetsSQL, []any{"repo-1"}, []string{checkTargetsKey + " (repo_id=?)"}},
		// AN EXPUNGE'S TIPS: a repository's ref drops from an instant, the
		// key's own covering seek in its order, first and keyset.
		"expunge tips": {expungeTipsSQL, []any{"repo-1", 100, MaxSnapshotItems},
			[]string{"COVERING INDEX " + dropsKey + " (repo_id=? AND at>=?)"}},
		"expunge tips after": {expungeTipsAfterSQL, []any{"repo-1", 100, 3, "refs/heads/x", "abc", MaxSnapshotItems},
			[]string{"COVERING INDEX " + dropsKey + " (repo_id=? AND at>=?)"}},
		// AND WHAT ROWS HOLD IN FLIGHT (M6-D2), each phase from its start and
		// after a place in its index's order: the live changes over
		// code_changes_open_idx, then each one's revisions by their key; the live entries
		// over the index partial on the live states; the candidates over
		// code_candidates_train_idx; and the green rows by their key.
		"live change tips": {liveChangeTipsSQL, []any{"repo-1", MaxSnapshotItems},
			[]string{"code_changes_open_idx (repo_id=?)"}},
		"live change tips from": {liveChangeTipsFromSQL, []any{"repo-1", "refs/heads/f000001", MaxSnapshotItems},
			[]string{"code_changes_open_idx (repo_id=? AND source_ref>=?)"}},
		"live change tips after": {liveChangeTipsAfterSQL, []any{"repo-1", "refs/heads/f000001", MaxSnapshotItems},
			[]string{"code_changes_open_idx (repo_id=? AND source_ref>?)"}},
		"change revision tips": {changeRevisionTipsSQL, []any{"c-1", 2, MaxSnapshotItems},
			[]string{revisionKey + " (change_id=? AND n>?)"}},
		"queue tips": {queueTipsSQL, []any{"repo-1", MaxSnapshotItems},
			[]string{"code_queue_live_idx (repo_id=?)"}},
		"queue tips after": {queueTipsAfterSQL, []any{"repo-1", "refs/heads/main", 1000, MaxSnapshotItems},
			[]string{"code_queue_live_idx (repo_id=? AND target_ref>=?)"}},
		"candidate tips": {candidateTipsSQL, []any{"repo-1", MaxSnapshotItems},
			[]string{"code_candidates_train_idx (repo_id=?)"}},
		"candidate tips after": {candidateTipsAfterSQL, []any{"repo-1", "refs/heads/main", "train-1", 2,
			MaxSnapshotItems}, []string{"code_candidates_train_idx (repo_id=? AND target_ref>=?)"}},
		"green tips": {greenTipsSQL, []any{"repo-1", MaxSnapshotItems},
			[]string{"sqlite_autoindex_code_green_1 (repo_id=?)"}},
		"green tips after": {greenTipsAfterSQL, []any{"repo-1", "refs/heads/main", MaxSnapshotItems},
			[]string{"sqlite_autoindex_code_green_1 (repo_id=? AND target_ref>?)"}},
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
	"CheckLogReferences":    CheckLogReferences,
}

// referenceWidths is every width each of objstore's statement builders is
// built at, by the builder's name: the widest, from its callers' own
// constant, and for a question of a list of keys the one key a short last
// batch narrows it to. The read gate holds the list to [referenceBuilders]
// both ways.
//
// EACH WIDTH IS A STATEMENT TEXT OF ITS OWN (n placeholders, a LIMIT of n),
// planned on its own, so a width no caller builds certifies nothing. The
// widest is where a planner would first trade the seeks, whose cost grows
// with the list, for a scan; one key is the form it may read as an equality.
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
	// THE CHECKS' LOGS, a live table like the packs: the collector's
	// question, the walk the audit, a backup and a store move read, a
	// backup's question of the keys its log names, and its page of the
	// records the log deferred.
	reference("ObjectsAmong", CheckLogReferences, keys, "code_check_logs_object_idx (object=?)")
	reference("ReferencesAfter", CheckLogReferences, after(""), "code_check_logs_object_idx (object>?)")
	reference("ReferencesAmong", CheckLogReferences, keys, "code_check_logs_object_idx (object=?)")
	reference("DeferredRecordsAfter", CheckLogReferences, after(0),
		"SEARCH code_log_deferred USING INTEGER PRIMARY KEY (rowid=?)")
	return out
}

// claimedReads is every index on a covered table that no statement this
// package issues reads YET, by name, with the read its migration's `-- <reader>`
// comment claims for it, planned here so the claim is the planner's from the
// migration that declares the index, not from whichever change first issues
// the read.
//
// TWO-SIDED, as every allowance here is: a claim whose index a registered
// statement reaches fails (a second copy of what runs), and so does an index
// neither reaches. Each read is written in the claim's own terms ([liveChanges]
// for the live states, newest first by the index's order), held to the exact
// seek and no sort, and judged by the read gate for scans. The gate's source
// match never sees a claim, which no code issues yet.
func claimedReads() map[string]statement {
	return map[string]statement{
		// THE SEARCH MODE'S INDEX ITS LATER READERS SEEK (replicated 0107):
		// the indexed repositories in name order, a keyset page at a time —
		// the walk of a Gitlet node that keeps indexes, and of a search across
		// the company's repositories — its partial predicate repeated
		// verbatim. Nothing in this build reads a repository by its mode.
		// A PERSON'S CODE INBOX (replicated 0109), newest first: the
		// composed inbox's code half reads it.
		"code_notifications_inbox_idx": {`SELECT record_id FROM code_notifications
			WHERE recipient = ? ORDER BY log_seq DESC LIMIT ?`, []any{"founder", 50},
			[]string{"code_notifications_inbox_idx (recipient=?)"}},
	}
}

// searchReads is internal/search's reads of this package's tables, by name: each the exported
// statement the search package runs (its rule for a read of another domain's table), held to its exact
// seek and no sort here and judged for scans by the read gate, as a registered statement is.
func searchReads() map[string]statement {
	reach, reachArgs := search.ChangeReachRead(0, search.ChangeIndexBatchRows)
	batch, batchArgs := search.ChangeBatchRead(0, 1<<40)
	repos, reposArgs := search.ChangeReposRead([]string{"repo-1", "repo-2"})
	backReach, backReachArgs := search.ChangeBackReachRead("~", search.ChangeIndexBatchRows)
	backBatch, backBatchArgs := search.ChangeBackBatchRead("~", "", 1<<40)
	return map[string]statement{
		"the index of changes' words' reach": {reach, reachArgs,
			[]string{changesPositionKey + " (position>?)"}},
		"the index of changes' words' batch": {batch, batchArgs,
			[]string{changesPositionKey + " (position>? AND position<=?)", bodyKey + " (change_id=?)"}},
		"the index of changes' words' repositories": {repos, reposArgs, []string{repoKey + " (id=?)"}},
		"the first build's reach":                   {backReach, backReachArgs, []string{changeKey + " (id<?)"}},
		"the first build's batch": {backBatch, backBatchArgs,
			[]string{changeKey + " (id<? AND id>=?)", bodyKey + " (change_id=?)"}},
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
