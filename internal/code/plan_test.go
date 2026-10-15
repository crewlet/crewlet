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
// first and keyset forms, the purge's bounded count and the object store's
// two statements over code_packs — and holds each to the EXACT seek it names:
// the index and every column of it the statement bounds. Seeking an index is
// not enough — a lookup of one ref that sought repo_id alone read every ref
// its repository holds (planner.go). The statements are the package's own
// constants: there is no copy here to drift from what runs.
//
// NO ANALYZE, because no deployment runs one: statistics choose differently,
// and a fixture that gathered them certified plans no node ever ran.
//
// The reads bounded by what a push row WEIGHS are held to a COVERING seek:
// they read the widths code_pushes_repo_idx carries, so weighing a wide row
// never reads its overflow pages.
//
// Mutation: drop `INDEXED BY` from headSQL and the head seeks
// code_refs_changed_idx on repo_id alone; test `ref IN (SELECT value FROM
// json_each(?))` in refsNamedSQL and it seeks repo_id alone; drop `position >=
// ?` from refsDeltaAfterSQL and the page seeks repo_id alone; drop row_bytes
// from code_pushes_repo_idx and the purge's and the horizon's reads stop
// covering.
func TestEveryCodeIndexServesARegisteredQuery(t *testing.T) {
	t.Parallel()
	db := seededPlanEstate(t)
	covered := []string{"code_repos", "code_names", "code_refs", "code_packs",
		"code_pushes", "code_protections"}
	used := map[string]bool{}
	for name, st := range registeredStatements() {
		t.Run(name, func(t *testing.T) {
			plan := explain(t, db, st.sql, st.args)
			for _, index := range indexesIn(plan) {
				used[index] = true
			}
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
		})
	}
	for _, index := range indexesOn(t, db, covered) {
		if !used[index] {
			t.Errorf("no registered statement's plan reaches %s — an index is a write cost "+
				"on every push on every node, so one no plan uses is deleted", index)
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

// The PRIMARY KEY indexes the plans name, as the driver names them.
const (
	repoKey       = "sqlite_autoindex_code_repos_1"
	nameKey       = "sqlite_autoindex_code_names_1"
	pushKey       = "sqlite_autoindex_code_pushes_1"
	protectionKey = "sqlite_autoindex_code_protections_1"
	deletionKey   = "sqlite_autoindex_code_deletions_1"
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
	out := map[string]statement{
		"guard repo":          {guardRepoSQL, []any{"repo-1", "repo-1"}, []string{deletionKey + " (repo_id=?)", repoKey + " (id=?)"}},
		"guard name":          {guardNameSQL, []any{"token"}, []string{nameKey + " (token=?)"}},
		"marker":              {markerSQL, []any{"repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		"purged":              {purgedSQL, []any{"repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		"repo version":        {repoVersionSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"settings":            {settingsSQL, []any{"config", "unit", "ENG", "main", "", "active", 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"clear protections":   {clearProtectionsSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"delete repo":         {deleteRepoSQL, []any{1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"restore repo":        {restoreRepoSQL, []any{"config", "unit", "ENG", "main", "", "active", 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"pushes applied":      {pushesAppliedSQL, []any{ids}, []string{pushKey + " (push_id=?)"}},
		"refs named":          {refsNamedSQL, []any{"repo-1", keys}, []string{"SCAN json_each AS j", refsKey + " (repo_id=? AND ref=?)"}},
		"push counters":       {pushCountersSQL, []any{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, "repo-1"}, []string{repoKey + " (id=?)"}},
		"tombstones held":     {tombstonesHeldSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"expired pushes":      {expiredPushesSQL, []any{"repo-1", 100, HorizonDeletesPerRecord}, []string{"COVERING INDEX code_pushes_repo_idx (repo_id=? AND at<?)"}},
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
		"remaining":           {remainingSQL, []any{"repo-1", PurgePartRows + 1, "repo-1", PurgePartRows + 1, "repo-1", PurgePartRows + 1, "repo-1", PurgePartRows + 1}, []string{"COVERING INDEX code_pushes_repo_idx (repo_id=?)", refsKey + " (repo_id=?)", packsKey + " (repo_id=?)"}},
		"claim":               {claimSQL, []any{"token"}, []string{nameKey + " (token=?)"}},
		"claims":              {claimsSQL, []any{`["token","other"]`}, []string{nameKey + " (token=?)"}},
		"repos rows":          {reposRowsSQL, []any{`["repo-1","repo-2"]`}, []string{repoKey + " (id=?)"}},
		"repo row":            {repoRowSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"protections":         {protectionsSQL, []any{"repo-1"}, []string{protectionKey + " (repo_id=?)"}},
		"read back":           {readBackSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"repo view":           {repoViewSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"packs full first":    {packsFullFirstSQL, []any{"repo-1", 100}, []string{packsKey + " (repo_id=?)"}},
		"packs full after":    {packsFullAfterSQL, []any{"repo-1", "p", "p", "pack", 100}, []string{packsKey + " (repo_id=? AND pack_id>=?)"}},
		"packs added first":   {packsAddedFirstSQL, []any{"repo-1", 1, 100}, []string{"code_packs_added_idx (repo_id=? AND added_position>?)"}},
		"packs added after":   {packsAddedAfterSQL, []any{"repo-1", 1, 1, "p", "p", "pack", 100}, []string{"code_packs_added_idx (repo_id=? AND added_position>=?)"}},
		"refs full first":     {refsFullFirstSQL, []any{"repo-1", 100}, []string{refsKey + " (repo_id=?)"}},
		"refs full after":     {refsFullAfterSQL, []any{"repo-1", "refs/heads/r000100", 100}, []string{refsKey + " (repo_id=? AND ref>?)"}},
		"refs delta first":    {refsDeltaFirstSQL, []any{"repo-1", 400, 100}, []string{"code_refs_changed_idx (repo_id=? AND position>?)"}},
		"refs delta after":    {refsDeltaAfterSQL, []any{"repo-1", 400, 400, "refs/heads/r", 100}, []string{"code_refs_changed_idx (repo_id=? AND position>=?)"}},
		"delta rows":          {deltaRowsSQL, []any{"repo-1", 400, 551}, []string{"COVERING INDEX code_refs_changed_idx (repo_id=? AND position>?)", "COVERING INDEX code_packs_added_idx (repo_id=? AND added_position>?)"}},
		"push":                {pushSQL, []any{"repo-1-push-000001"}, []string{pushKey + " (push_id=?)"}},
		"pushes first":        {pushesFirstSQL, []any{"repo-1", 50}, []string{"code_pushes_repo_idx (repo_id=?)"}},
		"pushes after":        {pushesAfterSQL, []any{"repo-1", 100, 100, 100, 100, "p", 50}, []string{"code_pushes_repo_idx (repo_id=? AND at<=?)"}},
		"repos":               {reposSQL, []any{`["active","archived"]`, "", 100}, []string{"code_repos_name_idx (name>?)", refsKey + " (repo_id=? AND ref=?)"}},
		"protections of":      {protectionsOfSQL, []any{`["repo-1","repo-2"]`}, []string{protectionKey + " (repo_id=?)"}},
		"head":                {headSQL, []any{"repo-1", "refs/heads/main"}, []string{refsKey + " (repo_id=? AND ref=?)"}},
		"lives":               {livesSQL, []any{"", 1000}, []string{"code_repos_name_idx (name>?)"}},
		"due purging":         {duePurgingSQL, []any{-1, -1, "", 100}, []string{"code_repos_purging_idx (deleted_at>=?)"}},
		"due deleted":         {dueDeletedSQL, []any{100, -1, -1, "", 100}, []string{"code_repos_deleted_idx (deleted_at>=? AND deleted_at<=?)"}},
		"purge state":         {purgeStateSQL, []any{"repo-1", "repo-1"}, []string{repoKey + " (id=?)", deletionKey + " (repo_id=?)"}},
		"count a gate reject": {countRejectSQL, []any{1, "repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		"decide repo":         {decideRepoSQL, []any{"repo-1"}, []string{repoKey + " (id=?)"}},
		"decide deletion":     {decideDeletionSQL, []any{"repo-1"}, []string{deletionKey + " (repo_id=?)"}},
		"pushes named":        {pushesNamedSQL, []any{ids}, []string{pushKey + " (push_id=?)"}},
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
	return out
}

// seededPlanEstate is an estate holding five repositories of 500 refs, 50
// packs and 200 pushes each, with no statistics gathered — the state every
// node's estate is in.
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
