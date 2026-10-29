package code_test

import (
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
)

// The activations the lifecycle cases run at, in order.
var (
	t1 = harnessAt.Add(-3 * time.Hour)
	t2 = harnessAt.Add(-2 * time.Hour)
	t3 = harnessAt.Add(-time.Hour)
)

// described is the suite's settings with a description.
func described(d string) code.RepoSettings {
	s := suiteSettings()
	s.Description = d
	return s
}

// repoRow is what a lifecycle case reads back of one repository.
type repoRow struct {
	id, status, description string
	configEpoch             int64
	deleted                 bool
}

// repo reads one repository's row by name; found is false when there is none.
func (n *node) repo(name string) (repoRow, bool) {
	n.t.Helper()
	var (
		r       repoRow
		deleted sql.NullInt64
	)
	err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(n.t.Context(), `
			SELECT id, status, description, config_epoch, deleted_at
			FROM code_repos WHERE name = ?`, name).
			Scan(&r.id, &r.status, &r.description, &r.configEpoch, &deleted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return repoRow{}, false
	case err != nil:
		n.t.Fatalf("read repository %q: %v", name, err)
	}
	r.deleted = deleted.Valid
	return r, true
}

// end is the log's last sequence.
func (n *node) end() uint64 {
	n.t.Helper()
	last, err := n.log.End(n.t.Context())
	if err != nil {
		n.t.Fatalf("read the log's end: %v", err)
	}
	return last
}

// A DECLARED REPOSITORY IS CREATED ONCE, SETTLED WHEN ITS SETTINGS CHANGE, AND
// COSTS NOTHING WHEN THEY DO NOT: a reconcile decides from rows, so a second
// call finds its own value already there.
func TestEnsureRepoCreatesSettlesAndIsFreeWhenUnchanged(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	first := n.ensure("app", described("one"), t1)
	if !first.Changed || first.Status != code.RepoStatusActive {
		t.Fatalf("the first ensure = %+v, want a created, active repository", first)
	}
	if id, err := uuid.Parse(first.RepoID); err != nil || id.Version() != 7 {
		t.Fatalf("the repository's id %q is not a version 7 uuid (%v)", first.RepoID, err)
	}

	before := n.end()
	again := n.ensure("app", described("one"), t1)
	if again.Changed || again.RepoID != first.RepoID || n.end() != before {
		t.Fatalf("the same declaration again = %+v and moved the log from %d to %d, "+
			"want nothing written and the same repository", again, before, n.end())
	}

	changed := n.ensure("app", described("two"), t1)
	if !changed.Changed || changed.RepoID != first.RepoID || n.end() != before+1 {
		t.Fatalf("a changed declaration at the same activation = %+v (log %d → %d), "+
			"want one settings record", changed, before, n.end())
	}
	if row, _ := n.repo("app"); row.description != "two" {
		t.Fatalf("the repository's description is %q, want the declared %q", row.description, "two")
	}
}

// settingsOf reads back what a repository's rows hold of its settings — every
// field the comparison reads, ConfigEpoch left zero.
func (n *node) settingsOf(name string) code.RepoSettings {
	n.t.Helper()
	var s code.RepoSettings
	err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		var id, status string
		if err := tx.QueryRowContext(n.t.Context(), `
			SELECT id, managed_by, kind, owner_kind, owner, visibility, default_branch, description, status
			FROM code_repos WHERE name = ?`, name).Scan(&id, &s.ManagedBy, &s.Kind, &s.Owner.Kind,
			&s.Owner.Ref, &s.Visibility, &s.DefaultBranch, &s.Description, &status); err != nil {
			return err
		}
		s.Archived = status == string(code.RepoStatusArchived)
		rows, err := tx.QueryContext(n.t.Context(), `
			SELECT pattern, allow_force, allow_delete, direct_push FROM code_protections
			WHERE repo_id = ? ORDER BY pattern`, id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var p code.Protection
			if err := rows.Scan(&p.Pattern, &p.AllowForce, &p.AllowDelete, &p.DirectPush); err != nil {
				return err
			}
			s.Protections = append(s.Protections, p)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		owners, err := tx.QueryContext(n.t.Context(), `
			SELECT owner_kind, owner, paths FROM code_path_owners
			WHERE repo_id = ? ORDER BY owner_kind, owner`, id)
		if err != nil {
			return err
		}
		defer func() { _ = owners.Close() }()
		for owners.Next() {
			var po code.PathOwner
			var packed string
			if err := owners.Scan(&po.Owner.Kind, &po.Owner.Ref, &packed); err != nil {
				return err
			}
			po.Paths = strings.Split(packed, "\n")
			s.PathOwners = append(s.PathOwners, po)
		}
		return owners.Err()
	})
	if err != nil {
		n.t.Fatalf("read repository %q's settings: %v", name, err)
	}
	return s
}

// EVERY FIELD A DECLARATION CARRIES REACHES THE LOG WHEN IT CHANGES. Under the
// activation fence the settings comparison is the ONLY thing that decides
// whether a declared change is written — an unchanged declaration writes
// nothing whatever activation stamped the row — so a field the comparison
// skips is a change dropped on every activation, silently, for good: the
// reconcile sees "unchanged" and nothing ever re-sends it. Each change is
// declared at the activation the repository was created at, then reverted at
// a later one; each must write exactly one settings record (the later one
// beside its fence), and the rows must then hold what was declared.
func TestEveryDeclaredSettingsChangeWritesOneRecord(t *testing.T) {
	t.Parallel()
	rules := func(ps ...code.Protection) code.RepoSettings {
		s := suiteSettings()
		s.Protections = ps
		return s
	}
	main := code.Protection{Pattern: "refs/heads/main", DirectPush: code.DirectPersons}
	release := code.Protection{Pattern: "refs/heads/release/*", DirectPush: code.DirectNone}
	for name, changed := range map[string]code.RepoSettings{
		"a rule added":   rules(main, release),
		"a rule removed": rules(),
		"allow_force flipped": rules(code.Protection{Pattern: main.Pattern, AllowForce: true,
			DirectPush: main.DirectPush}),
		"allow_delete flipped": rules(code.Protection{Pattern: main.Pattern, AllowDelete: true,
			DirectPush: main.DirectPush}),
		"direct_push changed": rules(code.Protection{Pattern: main.Pattern,
			DirectPush: code.DirectNone}),
		"a rule's pattern changed": rules(code.Protection{Pattern: "refs/heads/trunk",
			DirectPush: main.DirectPush}),
		"the owner's kind changed": func() code.RepoSettings {
			s := suiteSettings()
			s.Owner.Kind = code.OwnerSeat
			return s
		}(),
		"the owner's ref changed": func() code.RepoSettings {
			s := suiteSettings()
			s.Owner.Ref = "OPS"
			return s
		}(),
		"archived": func() code.RepoSettings {
			s := suiteSettings()
			s.Archived = true
			return s
		}(),
		"the default branch changed": func() code.RepoSettings {
			s := suiteSettings()
			s.DefaultBranch = "trunk"
			return s
		}(),
		"the description changed": described("changed"),
		"the visibility changed": func() code.RepoSettings {
			s := suiteSettings()
			s.Visibility = code.VisibilityUnit
			return s
		}(),
		"path owners named": func() code.RepoSettings {
			s := suiteSettings()
			s.PathOwners = []code.PathOwner{
				{Owner: code.Owner{Kind: code.OwnerSeat, Ref: "dev"}, Paths: []string{"Makefile"}},
				{Owner: code.Owner{Kind: code.OwnerUnit, Ref: "ENG"}, Paths: []string{"docs/**", "web/**"}},
			}
			return s
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			base := suiteSettings()
			n.ensure("app", base, t1)
			if got := n.settingsOf("app"); !reflect.DeepEqual(got, base) {
				t.Fatalf("the premise: the created repository holds %+v, want %+v", got, base)
			}

			before := n.end()
			if got := n.ensure("app", changed, t1); !got.Changed {
				t.Fatalf("the change at the same activation = %+v, want a settings record", got)
			}
			if n.end() != before+1 {
				t.Fatalf("the change at the same activation moved the log from %d to %d, "+
					"want exactly one settings record", before, n.end())
			}
			if got := n.settingsOf("app"); !reflect.DeepEqual(got, changed) {
				t.Fatalf("after the change the rows hold %+v, want the declared %+v", got, changed)
			}

			before = n.end()
			if got := n.ensure("app", base, t2); !got.Changed {
				t.Fatalf("the revert at a later activation = %+v, want a settings record", got)
			}
			if n.end() != before+2 {
				t.Fatalf("the revert at a later activation moved the log from %d to %d, "+
					"want its fence and exactly one settings record", before, n.end())
			}
			if got := n.settingsOf("app"); !reflect.DeepEqual(got, base) {
				t.Fatalf("after the revert the rows hold %+v, want the declared %+v", got, base)
			}
		})
	}
}

// NO RULES IS NO RULES, however the declaration spells it: a repository
// declared without protections and declared again with an empty list is
// unchanged, so the second activation writes its fence and nothing else.
func TestAnEmptyRuleListIsNoChange(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	none := suiteSettings()
	none.Protections = nil
	n.ensure("app", none, t1)

	empty := suiteSettings()
	empty.Protections = []code.Protection{}
	before := n.end()
	if got := n.ensure("app", empty, t2); got.Changed {
		t.Fatalf("an empty rule list where none was declared = %+v, want nothing written", got)
	}
	if n.end() != before+1 {
		t.Fatalf("the activation moved the log from %d to %d, want exactly its fence",
			before, n.end())
	}
}

// AN UNCHANGED DECLARATION AT A LATER ACTIVATION WRITES NOTHING BUT THE FENCE:
// the activation fence is what keeps an older activation from walking the
// rows back, so a row needs no re-stamp to be protected — and an activation
// over every repository the company has costs its one fence record rather than
// one settings record a repository.
func TestAnActivationThatChangesNothingWritesOneRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	names := []string{"api", "app", "web"}
	for _, name := range names {
		n.ensure(name, described(name), t1)
	}
	before := n.end()
	a := n.activate(t2)
	for _, name := range names {
		got, err := n.store.EnsureRepo(t.Context(), a, code.Declared{Name: name, Settings: described(name)})
		if err != nil || got.Changed {
			t.Fatalf("ensure %q unchanged at a later activation = %+v, %v", name, got, err)
		}
	}
	if after := n.end(); after != before+1 {
		t.Fatalf("an activation that changed nothing moved the log from %d to %d, "+
			"want exactly its fence record", before, after)
	}
	if row, _ := n.repo("app"); row.configEpoch != configplane.ActivationStamp(t1) {
		t.Fatalf("an unchanged repository was re-stamped to %d, want it left at %d",
			row.configEpoch, configplane.ActivationStamp(t1))
	}
}

// AN OLDER ACTIVATION NEVER WALKS A NEWER ONE BACK — neither one activated
// after it, which the fence refuses before anything is decided, nor one
// activated BEFORE it whose reconcile is still running, which every decide
// refuses from the fence its own snapshot holds.
func TestEnsureRepoNeverWalksBackALaterActivation(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	stale := n.activate(t1)
	n.ensure("app", described("new"), t2)

	before := n.end()
	if _, err := n.store.Activate(t.Context(), t1); !errors.Is(err, code.ErrSuperseded) {
		t.Fatalf("activating an older configuration = %v, want ErrSuperseded", err)
	}
	if _, err := n.store.EnsureRepo(t.Context(), stale, code.Declared{
		Name: "app", Settings: described("old"),
	}); !errors.Is(err, code.ErrSuperseded) {
		t.Fatalf("an older activation's reconcile still running = %v, want ErrSuperseded", err)
	}
	if _, err := n.store.DeleteRepo(t.Context(), stale, n.mustID("app")); !errors.Is(err, code.ErrSuperseded) {
		t.Fatalf("an older activation's delete = %v, want ErrSuperseded", err)
	}
	if n.end() != before {
		t.Fatalf("the older activation wrote %d record(s)", n.end()-before)
	}
	if row, _ := n.repo("app"); row.description != "new" || row.status != string(code.RepoStatusActive) {
		t.Fatalf("the repository is %+v, want the later activation's", row)
	}
	if _, err := n.store.EnsureRepo(t.Context(), code.Activation{}, code.Declared{
		Name: "app", Settings: described("x"),
	}); !errors.Is(err, code.ErrNoActivation) {
		t.Fatalf("an activation Activate never made = %v, want ErrNoActivation", err)
	}
}

// mustID is a repository's id by name.
func (n *node) mustID(name string) string {
	n.t.Helper()
	row, ok := n.repo(name)
	if !ok {
		n.t.Fatalf("no repository is named %q", name)
	}
	return row.id
}

// A RECORD AN OLDER ACTIVATION DECIDED AND APPENDED PAST THE FENCE IS REFUSED
// ON EVERY NODE — the case the fence exists for. A reconcile's decide reads
// the fence and publishes; the broker arbitrates only its own subject, so a
// settings, a delete or a create decided under an older activation can land
// after a newer one has raised the fence and settled the repository. Every
// node's applier then reads the fence in the record's own transaction and
// writes nothing, so no node walks the repository back and no two nodes
// disagree.
func TestAnOlderActivationsRecordPastTheFenceIsRefusedOnEveryNode(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	a.ensure("app", described("one"), t1)
	a.ensure("app", described("two"), t2)
	id := a.mustID("app")
	old := described("stale")
	old.ConfigEpoch = configplane.ActivationStamp(t1)

	for name, rec := range map[string]struct {
		subject code.Subject
		op      code.OpKind
		scope   code.Scope
		payload any
	}{
		"a settings record": {code.RepoSubject(id), code.OpSettings, code.Scope{code.RepoPath(id)},
			code.SettingsRecord{RepoID: id, Settings: old}},
		"a delete": {code.RepoSubject(id), code.OpDelete, code.Scope{code.RepoPath(id)},
			code.LifecycleRecord{RepoID: id, ConfigEpoch: old.ConfigEpoch, Reason: "stale"}},
		"a create of a name the newer activation does not declare": {
			code.NameSubject("gone"), code.OpCreate,
			code.Scope{code.NamePath(code.NameToken("gone")), code.RepoPath("stale-repo")},
			code.CreateRepo{RepoID: "stale-repo", Name: "gone", Settings: old}},
	} {
		if _, err := a.publish(rec.subject, rec.op, rec.scope, rec.payload); err != nil {
			t.Fatalf("%s: publish: %v", name, err)
		}
	}
	b.drain()
	for _, n := range []*node{a, b} {
		row, _ := n.repo("app")
		if row.description != "two" || row.status != string(code.RepoStatusActive) ||
			row.configEpoch != configplane.ActivationStamp(t2) {
			t.Errorf("%s holds %+v, want the later activation's settings, active — an "+
				"older activation's record walked it back", n.id, row)
		}
		if _, found := n.repo("gone"); found {
			t.Errorf("%s created a repository an older activation declared", n.id)
		}
	}
}

// A RECORD AT AN OLDER ACTIVATION APPLIED BEFORE THE NEWER FENCE IS CORRECTED
// BY THE NEWER RECONCILE: the fence refuses nothing it did not stand over, so
// the newer activation's own decide finds the stale settings and settles them.
func TestARecordBelowTheFenceAppliedBeforeItIsCorrected(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	id := n.mustID("app")
	stale := described("stale")
	stale.ConfigEpoch = configplane.ActivationStamp(t1)
	if _, err := n.publish(code.RepoSubject(id), code.OpSettings, code.Scope{code.RepoPath(id)},
		code.SettingsRecord{RepoID: id, Settings: stale}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if row, _ := n.repo("app"); row.description != "stale" {
		t.Fatalf("the premise: a record at the fence's own activation applies, and the "+
			"repository holds %q", row.description)
	}
	if got := n.ensure("app", described("two"), t2); !got.Changed {
		t.Fatalf("the newer reconcile = %+v, want it to settle the stale settings", got)
	}
	if row, _ := n.repo("app"); row.description != "two" {
		t.Fatalf("the repository holds %q after the newer reconcile, want %q", row.description, "two")
	}
}

// A DELETED REPOSITORY IS RESTORED ONLY BY A LATER ACTIVATION: the one that
// deleted it declaring it again is the deleting configuration's predecessor
// applied late, and it must not bring the repository back.
func TestADeletedRepositoryIsRestoredByALaterActivation(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	id := n.mustID("app")
	n.push(id, entry("push-1", set("refs/heads/main", code.ZeroOID, oidOf("c1"))))

	a2 := n.activate(t2)
	deleted, err := n.store.DeleteRepo(t.Context(), a2, id)
	if err != nil || !deleted {
		t.Fatalf("DeleteRepo = %v, %v", deleted, err)
	}
	if row, _ := n.repo("app"); row.status != string(code.RepoStatusDeleted) || !row.deleted {
		t.Fatalf("after the delete the repository is %+v", row)
	}
	if got, err := n.store.EnsureRepo(t.Context(), a2, code.Declared{
		Name: "app", Settings: described("one"),
	}); err != nil || got.Changed || got.Status != code.RepoStatusDeleted {
		t.Fatalf("the deleting activation declaring it again = %+v, %v; want nothing written", got, err)
	}
	got := n.ensure("app", described("one"), t3)
	if !got.Changed || got.Status != code.RepoStatusActive || got.RepoID != id {
		t.Fatalf("a later activation declaring it = %+v, want a restore of %s", got, id)
	}
	if row, _ := n.repo("app"); row.status != string(code.RepoStatusActive) || row.deleted {
		t.Fatalf("after the restore the repository is %+v", row)
	}
	if refs := n.count(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target != ''`, id); refs != 1 {
		t.Fatalf("the restored repository holds %d refs, want its one", refs)
	}
}

// A REPOSITORY ITS CONFIGURATION DOES NOT MANAGE IS NEVER SETTLED BY ONE — the
// rule a tools-managed repository relies on. Hand-built, because no writer of
// this build makes one.
func TestEnsureRepoRefusesARepositoryItDoesNotManage(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.activate(t1)
	n.exec(`INSERT INTO code_names (token, name, repo_id) VALUES (?, 'tooled', 'tooled-id')`,
		code.NameToken("tooled"))
	n.exec(`INSERT INTO code_repos (id, name, status, managed_by, owner_kind, owner,
		default_branch, created_at) VALUES ('tooled-id', 'tooled', 'active', 'tools', 'unit',
		'ENG', 'main', 0)`)
	before := n.end()
	_, err := n.store.EnsureRepo(t.Context(), n.activate(t1), code.Declared{
		Name: "tooled", Settings: described("mine"),
	})
	if !errors.Is(err, code.ErrNotConfigManaged) || n.end() != before {
		t.Fatalf("EnsureRepo of a repository tools manage = %v (log %d → %d), "+
			"want ErrNotConfigManaged and nothing written", err, before, n.end())
	}
}

// A REPOSITORY BEING PURGED IS WAITED OUT: its name returns when the purge
// completes, and nothing is written meanwhile.
func TestEnsureRepoWaitsOutAPurge(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	n.exec(`UPDATE code_repos SET status = 'purging' WHERE name = 'app'`)
	before := n.end()
	_, err := n.store.EnsureRepo(t.Context(), n.activate(t1), code.Declared{
		Name: "app", Settings: described("two"),
	})
	if !errors.Is(err, code.ErrPurging) || n.end() != before {
		t.Fatalf("EnsureRepo of a purging repository = %v (log %d → %d), want ErrPurging "+
			"and nothing written", err, before, n.end())
	}
}

// TWO NODES ENSURING ONE NAME AT ONCE MAKE ONE REPOSITORY: the name's subject
// arbitrates the two creates, and the loser's next round finds the winner's
// claim — so both calls answer the same id and both nodes hold one row.
func TestTwoEnsuresOfOneNameCreateOneRepository(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	a.activate(t1)
	b.release()
	var (
		wg  sync.WaitGroup
		ids [2]string
	)
	for i, n := range []*node{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			act, err := n.store.Activate(t.Context(), t1)
			if err != nil {
				t.Errorf("%s: activate: %v", n.id, err)
				return
			}
			got, err := n.store.EnsureRepo(t.Context(), act, code.Declared{
				Name: "app", Settings: described("one"),
			})
			if err != nil {
				t.Errorf("%s: ensure: %v", n.id, err)
				return
			}
			ids[i] = got.RepoID
		}()
	}
	wg.Wait()
	a.drain()
	b.drain()
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("the two ensures answered %q and %q, want one repository", ids[0], ids[1])
	}
	for _, n := range []*node{a, b} {
		if names := n.count(`SELECT COUNT(*) FROM code_names`); names != 1 {
			t.Errorf("%s holds %d name claims, want one", n.id, names)
		}
		if repos := n.count(`SELECT COUNT(*) FROM code_repos`); repos != 1 {
			t.Errorf("%s holds %d repositories, want one", n.id, repos)
		}
	}
}

// seedRows writes rows of a repository's history straight into a node's
// estate — the bulk a purge has to clear, which publishing push by push would
// spend minutes on and assert nothing more by.
func (n *node) seedRows(repoID string, pushes, refs, packs int) {
	n.t.Helper()
	ctx := n.t.Context()
	vars := n.db.Caps().MaxVariables
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, via,
				commands, received_at, row_bytes) VALUES `, `(?, ?, ?, 0, 'seed', 'operator', 'git', '[]', 0, 2)`, ``,
			pushes, func(i int) []any { return []any{uuid.NewString(), repoID, i + 1} }); err != nil {
			return err
		}
		if _, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_refs (repo_id, ref, target, position, at) VALUES `,
			`(?, ?, ?, 1, 0)`, ``, refs, func(i int) []any {
				return []any{repoID, "refs/heads/seed-" + uuid.NewString(), oidOf(repoID)}
			}); err != nil {
			return err
		}
		_, err := store.InsertRows(ctx, tx, vars,
			`INSERT INTO code_packs (repo_id, pack_id, part, object, hash, size, index_bytes,
				reverse_bytes, git_checksum, objects, kind, state, added_position) VALUES `,
			`(?, ?, 'pack', ?, ?, 100, 0, 0, ?, 1, 'push', 'live', 1)`, ``, packs,
			func(i int) []any {
				pack := uuid.NewString()
				return []any{repoID, pack, keyOf(pack).String(), string(objstore.HashOf([]byte(pack))),
					oidOf(pack)}
			})
		return err
	}); err != nil {
		n.t.Fatalf("seed %s's rows: %v", repoID, err)
	}
}

// A PURGE IS BOUNDED BY WHAT ITS PUSHES WEIGH: a repository with few push
// rows, each wide, is purged in parts until what is left weighs no more than
// PurgePartBytes — the final purge's statements are unbounded, so it is
// formed only once the pushes fit a part's bytes as well as its rows.
func TestAPurgeIsBoundedByWhatItsPushesWeigh(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	id := n.mustID("app")
	const retention = 7 * 24 * time.Hour
	n.storedAt = func(seq uint64, broker time.Time) time.Time { return harnessAt.Add(-time.Hour) }
	if deleted, err := n.store.DeleteRepo(t.Context(), n.activate(t2), id); err != nil || !deleted {
		t.Fatalf("DeleteRepo = %v, %v", deleted, err)
	}
	n.storedAt = nil
	// TEN ROWS, far under PurgePartRows, of 8 MiB each: 80 MiB in all.
	const width, rows = 8 << 20, 10
	n.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, via, commands,
			received_at, row_bytes)
		SELECT printf('wide-%02d', i), ?, i + 1, 0, 'seed', 'operator', 'git', '[]', 0, ? FROM k`,
		rows, id, width)
	late, err := code.NewStore(code.StoreOptions{
		Publisher: n.pub, DB: n.db.Reader(), NodeID: n.id,
		Now: func() time.Time { return harnessAt.Add(time.Hour + retention) },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	pushes := func() int { return n.count(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, id) }
	var steps []int
	for range rows {
		before := pushes()
		res, err := late.PurgeStep(t.Context(), id, retention)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		n.drain()
		steps = append(steps, before-pushes())
		if res.Done {
			break
		}
	}
	perPart := code.PurgePartBytes / width
	if want := []int{perPart, perPart, rows - 2*perPart}; !slices.Equal(steps, want) {
		t.Fatalf("the purge removed %v push rows a record, want %v: parts of %d rows of %d "+
			"bytes (PurgePartBytes %d) until what is left fits one, then the purge", steps, want,
			perPart, width, code.PurgePartBytes)
	}
	if _, found := n.repo("app"); found {
		t.Fatal("the repository still has a row after its purge")
	}
}

// A PURGE IS BOUNDED PER RECORD: a deleted repository past its retention is
// removed a part at a time — no record deletes more than PurgePartRows of its
// rows, and the last one removes the repository and leaves its marker — and a
// purge step on what is already purged writes nothing.
func TestAPurgeIsBoundedPerRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("one"), t1)
	id := n.mustID("app")
	const retention = 7 * 24 * time.Hour

	if _, err := n.store.PurgeStep(t.Context(), id, retention); !errors.Is(err, code.ErrNotDeleted) {
		t.Fatalf("PurgeStep on an active repository = %v, want ErrNotDeleted", err)
	}
	// THE DELETE LANDS AT THE STORE'S OWN CLOCK, so it is inside its
	// retention until the case moves the clock past it.
	deletedAt := harnessAt.Add(-time.Hour)
	n.storedAt = func(seq uint64, broker time.Time) time.Time { return deletedAt }
	if deleted, err := n.store.DeleteRepo(t.Context(), n.activate(t2), id); err != nil || !deleted {
		t.Fatalf("DeleteRepo = %v, %v", deleted, err)
	}
	n.storedAt = nil
	if _, err := n.store.PurgeStep(t.Context(), id, retention); !errors.Is(err, code.ErrNotYet) {
		t.Fatalf("PurgeStep within the retention = %v, want ErrNotYet", err)
	}

	n.seedRows(id, 3000, 3000, 1000)
	total := func() int {
		return n.count(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, id) +
			n.count(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ?`, id) +
			n.count(`SELECT COUNT(*) FROM code_packs WHERE repo_id = ?`, id)
	}
	if total() != 7000 {
		t.Fatalf("the premise: the repository holds %d rows, want 7000", total())
	}
	past := time.Hour + retention // the purge pass runs past the retention
	late, err := code.NewStore(code.StoreOptions{
		Publisher: n.pub, DB: n.db.Reader(), NodeID: n.id,
		Now: func() time.Time { return harnessAt.Add(past) },
	})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	var steps []int
	for range 5 {
		before := total()
		res, err := late.PurgeStep(t.Context(), id, retention)
		if err != nil {
			t.Fatalf("PurgeStep: %v", err)
		}
		n.drain()
		removed := before - total()
		if removed > code.PurgePartRows {
			t.Fatalf("one purge record removed %d rows, past PurgePartRows (%d)", removed, code.PurgePartRows)
		}
		steps = append(steps, removed)
		if res.Done {
			break
		}
	}
	if len(steps) != 3 || steps[0] != code.PurgePartRows || steps[1] != code.PurgePartRows {
		t.Fatalf("the purge removed %v rows a record, want two parts of %d and the purge", steps,
			code.PurgePartRows)
	}
	if _, found := n.repo("app"); found || total() != 0 {
		t.Fatalf("after the purge the repository has a row (%v) or %d rows of history", found, total())
	}
	if marked := n.count(`SELECT COUNT(*) FROM code_deletions WHERE repo_id = ?`, id); marked != 1 {
		t.Fatalf("the purge left %d markers, want one", marked)
	}
	before := n.end()
	if res, err := late.PurgeStep(t.Context(), id, retention); err != nil || !res.Done || !res.Held() ||
		!res.Position.IsZero() || n.end() != before {
		t.Fatalf("PurgeStep after the purge = %+v, %v (log %d → %d), want done with nothing written",
			res, err, before, n.end())
	}
	if _, err := late.PurgeStep(t.Context(), "never-was", retention); !errors.Is(err, code.ErrRepoNotFound) {
		t.Fatalf("PurgeStep of a repository nobody made = %v, want ErrRepoNotFound", err)
	}
}

// A VISIBILITY CHANGE IS A SETTINGS RECORD LIKE ANY OTHER: narrowing a
// repository writes one record, the rows and both reads then answer the new
// visibility, and widening it again writes one more — so no reconcile ever
// finds a narrowed repository "unchanged" and leaves every seat reading it.
//
// Mutation: the settings comparison skipping Visibility → the narrowing
// writes nothing and the rows still say company.
func TestAVisibilityChangeIsASettingsRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", suiteSettings(), t1)
	visible := func(want code.Visibility) {
		t.Helper()
		if got := n.settingsOf("app").Visibility; got != want {
			t.Fatalf("the rows hold visibility %q, want %q", got, want)
		}
		one, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepo, Repo: "app"})
		if err != nil || one.Repo == nil || one.Repo.Visibility != want {
			t.Fatalf("Read(repo app) = %+v, %v; want visibility %q", one.Repo, err, want)
		}
		all, err := n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadRepos})
		if err != nil || len(all.Repos) != 1 || all.Repos[0].Visibility != want {
			t.Fatalf("Read(repos) = %+v, %v; want app at visibility %q", all.Repos, err, want)
		}
	}
	visible(code.VisibilityCompany)

	narrowed := suiteSettings()
	narrowed.Visibility = code.VisibilityUnit
	before := n.end()
	if got := n.ensure("app", narrowed, t1); !got.Changed || n.end() != before+1 {
		t.Fatalf("narrowing the repository = %+v (log %d → %d), want one settings record",
			got, before, n.end())
	}
	visible(code.VisibilityUnit)
	if got := n.ensure("app", narrowed, t1); got.Changed || n.end() != before+1 {
		t.Fatalf("the narrowed declaration again = %+v (log %d → %d), want nothing written",
			got, before, n.end())
	}

	// A later activation writes its fence beside the settings record.
	if got := n.ensure("app", suiteSettings(), t2); !got.Changed || n.end() != before+3 {
		t.Fatalf("widening it at a later activation = %+v (log %d → %d), want its fence and "+
			"one settings record", got, before, n.end())
	}
	visible(code.VisibilityCompany)
}
