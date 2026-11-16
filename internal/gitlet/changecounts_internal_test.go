package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"math/rand/v2"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// rederive is the applier's re-derivation of the estate's counts in one writer transaction,
// answering the rows it wrote.
func (e *estate) rederive() int {
	e.t.Helper()
	var n int
	if err := e.db.Tx(context.Background(), func(tx *sql.Tx) (err error) {
		n, err = NewApplier(nil).Rederive(context.Background(), tx, statelog.ApplyOptions{})
		return err
	}); err != nil {
		e.t.Fatalf("re-derive the change counts: %v", err)
	}
	return n
}

// forgetChangeCounts leaves the counts as a node upgraded to them holds them before its
// first re-derivation: every column zero and no change or repository tally row.
func (e *estate) forgetChangeCounts() {
	e.t.Helper()
	e.exec(`UPDATE code_repos SET draft_changes = 0, open_changes = 0, closed_changes = 0, merged_changes = 0`)
	e.exec(`DELETE FROM code_change_tallies`)
	e.exec(`DELETE FROM code_repo_tallies`)
}

// changeCountsMatchRows fails the test where the counts are not what the rows imply.
func (e *estate) changeCountsMatchRows() {
	e.t.Helper()
	if drift := e.changeCountsDrift(); drift != "" {
		e.t.Fatal(drift)
	}
}

// changeCountsDrift names the first difference between the counts and the rows, "" for none.
func (e *estate) changeCountsDrift() string {
	e.t.Helper()
	var drift string
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) (err error) {
		drift, err = changeCountsDrift(context.Background(), tx)
		return err
	}); err != nil {
		e.t.Fatalf("read the change counts: %v", err)
	}
	return drift
}

// changeCountsDrift names the first difference between the counts and the rows, "" for none:
// each repository's four columns against its changes counted by state, the tally rows against
// the changes of the repositories in use grouped by visibility and owner, no zero row, and the
// repository tallies ([repoTalliesDrift]).
func changeCountsDrift(ctx context.Context, tx *sql.Tx) (string, error) {
	type repo struct {
		id     string
		live   bool
		key    tallyKey
		kept   ChangeCounts
		counts ChangeCounts
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, status, visibility, owner_kind, owner, draft_changes,
		open_changes, closed_changes, merged_changes, kind FROM code_repos ORDER BY id`)
	if err != nil {
		return "", err
	}
	var repos []repo
	for rows.Next() {
		var r repo
		var s tallyStanding
		if s, err = scanStanding(rows, &r.id); err != nil {
			_ = rows.Close()
			return "", err
		}
		r.live, r.key, r.kept = s.live, s.key, s.counts
		repos = append(repos, r)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	_ = rows.Close()
	wanted := map[tallyKey]ChangeCounts{}
	for _, r := range repos {
		if r.counts, err = changesCountedBy(ctx, tx, r.id); err != nil {
			return "", err
		}
		if r.kept != r.counts {
			return fmt.Sprintf("%s counts its changes %+v and its rows hold %+v", r.id, r.kept, r.counts), nil
		}
		if r.live && !r.counts.zero() {
			wanted[r.key] = wanted[r.key].plus(r.counts)
		}
	}
	held := map[tallyKey]ChangeCounts{}
	if err = eachTally(ctx, tx, func(key tallyKey, c ChangeCounts) error {
		held[key] = c
		return nil
	}); err != nil {
		return "", err
	}
	if !maps.Equal(wanted, held) {
		return fmt.Sprintf("the change tallies hold %s and the repositories in use %s", tallyText(held),
			tallyText(wanted)), nil
	}
	return repoTalliesDrift(ctx, tx)
}

// changesCountedBy is repoID's changes counted by state with a statement of the test's own.
func changesCountedBy(ctx context.Context, tx *sql.Tx, repoID string) (c ChangeCounts, err error) {
	rows, err := tx.QueryContext(ctx, `SELECT state, COUNT(*) FROM code_changes WHERE repo_id = ? GROUP BY state`,
		repoID)
	if err != nil {
		return c, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var state string
		var n int64
		if err = rows.Scan(&state, &n); err != nil {
			return c, err
		}
		c.add(ChangeState(state), n)
	}
	return c, rows.Err()
}

// tallyText is tallies as a sorted line, for a failure's sentence.
func tallyText(t map[tallyKey]ChangeCounts) string {
	keys := slices.SortedFunc(maps.Keys(t), tallyKey.compare)
	parts := make([]string, len(keys))
	for i, k := range keys {
		c := t[k]
		parts[i] = fmt.Sprintf("%s/%s/%s=%d/%d/%d/%d", k.visibility, k.owner.Kind, k.owner.Ref, c.Draft, c.Open,
			c.Closed, c.Merged)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// tallies is the estate's tally rows as tallyText writes them.
func (e *estate) tallies() string {
	e.t.Helper()
	held := map[tallyKey]ChangeCounts{}
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		return eachTally(context.Background(), tx, func(key tallyKey, c ChangeCounts) error {
			held[key] = c
			return nil
		})
	}); err != nil {
		e.t.Fatalf("read the change tallies: %v", err)
	}
	return tallyText(held)
}

// changeCountsOf is repository id's four counts, draft, open, closed and merged.
func (e *estate) changeCountsOf(id string) [4]int64 {
	e.t.Helper()
	var c [4]int64
	for i, column := range []string{"draft_changes", "open_changes", "closed_changes", "merged_changes"} {
		c[i] = e.num(`SELECT `+column+` FROM code_repos WHERE id = ?`, id)
	}
	return c
}

// ccSeat is the seat every change record of these cases is made by.
var ccSeat = Principal{Kind: PrincipalAgent, Seat: "dev"}

// ccChange is the id of the n-th change a case opens, ccSource its source branch and
// ccHead the commit that branch starts at.
func ccChange(n int) string { return fmt.Sprintf("0192f6c1-0000-7000-8000-%012d", n) }
func ccSource(n int) string { return fmt.Sprintf("refs/heads/change-%d", n) }
func ccHead(n int) string   { return oid(fmt.Sprint("head-", n)) }

// ccMain is the branch every change of these cases targets.
const ccMain = "refs/heads/main"

// ccRecord is a record of op on repository repo carrying payload.
func ccRecord(t testing.TB, repo string, op OpKind, payload any) built {
	return rec(t, RepoSubject(repo), op, Scope{RepoPath(repo)}, payload)
}

// ccOpen is the open of change n on repo, from its source at its head into main.
func ccOpen(t testing.TB, repo string, n int, draft bool) built {
	return ccRecord(t, repo, OpChangeOpen, ChangeOpen{RepoID: repo, ChangeID: ccChange(n), SourceRef: ccSource(n),
		TargetRef: ccMain, Head: ccHead(n), Title: "Retry the upload", Draft: draft, Owner: ccSeat})
}

// ccBranches is a push of main at base and the source of each change n names at its head.
func ccBranches(t testing.TB, repo, pushID string, changes ...int) built {
	entry := pushEntry(pushID, move(ccMain, ZeroOID, oid("base")))
	for _, n := range changes {
		entry.Commands = append(entry.Commands, move(ccSource(n), ZeroOID, ccHead(n)))
	}
	return pushOf(t, repo, entry)
}

// ccSettings is the settings record of repo with s.
func ccSettings(t testing.TB, repo string, s RepoSettings) built {
	return ccRecord(t, repo, OpSettings, SettingsRecord{RepoID: repo, Settings: s})
}

// ccOwned is settingsFor's settings under another owner and visibility at epoch.
func ccOwned(owner Owner, v Visibility, epoch int64) RepoSettings {
	s := settingsFor("")
	s.Owner, s.Visibility, s.ConfigEpoch = owner, v, epoch
	return s
}

var (
	ccENG = Owner{Kind: OwnerUnit, Ref: "ENG"}
	ccOPS = Owner{Kind: OwnerUnit, Ref: "OPS"}
	ccQA  = Owner{Kind: OwnerUnit, Ref: "QA"}
)

// EACH RECORD MOVES THE COUNTS IT SHOULD: every writer of a change's state or a repository's
// standing leaves the counts and tally rows the case states, and those the rows imply.
// Mutation: drop a writer's arm, or a delete's, restore's or settings change's tally move.
func TestEachRecordMovesTheCountsItShould(t *testing.T) {
	t.Parallel()
	const repo = "repo-c"
	created := func(t *testing.T, e *estate) {
		e.apply(create(t, repo, "c"), ccBranches(t, repo, "p0", 1, 2, 3))
	}
	opened := func(changes ...int) func(t *testing.T, e *estate) {
		return func(t *testing.T, e *estate) {
			created(t, e)
			for _, n := range changes {
				e.apply(ccOpen(t, repo, n, false))
			}
		}
	}
	closed := func(n int) built {
		return ccRecord(t, repo, OpChangeClose, ChangeClose{RepoID: repo, ChangeID: ccChange(n), By: ccSeat})
	}
	deleted := func(t *testing.T, e *estate) {
		opened(1, 2)(t, e)
		e.apply(closed(2), ccRecord(t, repo, OpDelete, LifecycleRecord{RepoID: repo, ConfigEpoch: 1}))
	}
	restored := func(owner Owner, epoch int64) built {
		back := ccOwned(owner, VisibilityCompany, epoch)
		return ccRecord(t, repo, OpRestore, LifecycleRecord{RepoID: repo, ConfigEpoch: epoch, Settings: &back})
	}
	merge := func(n int, id string) built {
		return ccRecord(t, repo, OpMerge, Merge{RepoID: repo, MergeID: id, ChangeID: ccChange(n),
			Method: MergeMethodMerge, TargetRef: ccMain, BaseOID: oid("base"), HeadOID: ccHead(n),
			CommitOID: oid("merged-" + id), Pack: packOf(id), Node: "git-a", ReceivedAt: applyClock,
			Principal: ccSeat})
	}
	enqueued := func(n int) built {
		return ccRecord(t, repo, OpEnqueue, Enqueue{RepoID: repo, EntryID: ccChange(100 + n), ChangeID: ccChange(n),
			TargetRef: ccMain, HeadOID: ccHead(n), Method: MergeMethodMerge, Principal: ccSeat})
	}
	importing := func(t *testing.T, e *estate) {
		s := settingsFor("")
		e.apply(rec(t, NameSubject("c"), OpCreate, Scope{NamePath(NameToken("c")), RepoPath(repo)},
			CreateRepo{RepoID: repo, Name: "c", Settings: s, Import: &ImportSource{URL: "https://example.com/c.git"}}),
			ccBranches(t, repo, "p0", 1), ccOpen(t, repo, 1, false))
	}
	for _, tc := range []struct {
		name    string
		given   func(t *testing.T, e *estate)
		records func(t *testing.T) []built
		// counts is the repository's draft, open, closed and merged; tallies the tally rows.
		counts  [4]int64
		tallies string
	}{
		{"an open", created, func(t *testing.T) []built { return []built{ccOpen(t, repo, 1, false)} },
			[4]int64{0, 1, 0, 0}, "[company/unit/ENG=0/1/0/0]"},
		{"a draft open", created, func(t *testing.T) []built { return []built{ccOpen(t, repo, 1, true)} },
			[4]int64{1, 0, 0, 0}, "[company/unit/ENG=1/0/0/0]"},
		{"ready", func(t *testing.T, e *estate) { created(t, e); e.apply(ccOpen(t, repo, 1, true)) },
			func(t *testing.T) []built {
				return []built{ccRecord(t, repo, OpChangeDraft, ChangeDraft{RepoID: repo, ChangeID: ccChange(1),
					By: ccSeat})}
			}, [4]int64{0, 1, 0, 0}, "[company/unit/ENG=0/1/0/0]"},
		{"a close", opened(1, 2), func(t *testing.T) []built { return []built{closed(1)} },
			[4]int64{0, 1, 1, 0}, "[company/unit/ENG=0/1/1/0]"},
		{"a redelivered close", opened(1, 2), func(t *testing.T) []built {
			c := closed(1)
			return []built{c, c}
		}, [4]int64{0, 1, 1, 0}, "[company/unit/ENG=0/1/1/0]"},
		{"a reopen", func(t *testing.T, e *estate) { opened(1)(t, e); e.apply(closed(1)) },
			func(t *testing.T) []built {
				return []built{ccRecord(t, repo, OpChangeReopen, ChangeReopen{RepoID: repo, ChangeID: ccChange(1),
					By: ccSeat, Head: ccHead(1)})}
			}, [4]int64{0, 1, 0, 0}, "[company/unit/ENG=0/1/0/0]"},
		{"a landing", opened(1), func(t *testing.T) []built {
			return []built{ccRecord(t, repo, OpChangeLanded, ChangeLanded{RepoID: repo, ChangeID: ccChange(1),
				Revision: 1, HeadOID: ccHead(1), TargetRef: ccMain, TargetOID: ccHead(1), Node: "git-a"})}
		}, [4]int64{0, 0, 0, 1}, "[company/unit/ENG=0/0/0/1]"},
		{"a merge record", opened(1, 2), func(t *testing.T) []built { return []built{merge(1, ccChange(300))} },
			[4]int64{0, 1, 0, 1}, "[company/unit/ENG=0/1/0/1]"},
		{"a landing of two", opened(1, 2, 3), func(t *testing.T) []built {
			l := Land{RepoID: repo, LandID: ccChange(400), TargetRef: ccMain, BaseOID: oid("base"), Node: "git-a",
				ReceivedAt: applyClock, Entries: []LandEntry{
					{EntryID: ccChange(101), ChangeID: ccChange(1), Commit: oid("landed-1")},
					{EntryID: ccChange(102), ChangeID: ccChange(2), Commit: oid("landed-2")}}}
			l.TipOID = l.Entries[1].Commit
			return []built{enqueued(1), enqueued(2), ccRecord(t, repo, OpLand, l)}
		}, [4]int64{0, 1, 0, 2}, "[company/unit/ENG=0/1/0/2]"},
		{"a push merging two and closing one", opened(1, 2, 3), func(t *testing.T) []built {
			// Changes 1 and 2 start at one head here, so main set to it merges both.
			return []built{pushOf(t, repo, pushEntry("p1", move(ccSource(2), ccHead(2), ccHead(1)))),
				pushOf(t, repo, pushEntry("p2", move(ccMain, oid("base"), ccHead(1)),
					move(ccSource(3), ccHead(3), ZeroOID)))}
		}, [4]int64{0, 0, 1, 2}, "[company/unit/ENG=0/0/1/2]"},
		{"a push opening a draft", created, func(t *testing.T) []built {
			entry := pushEntry("p1", move("refs/heads/opened", ZeroOID, oid("opened")))
			entry.Principal = ccSeat
			entry.Change = &ChangeOption{Target: ccMain, Title: "Opened by its push", Draft: true}
			return []built{pushOf(t, repo, entry)}
		}, [4]int64{1, 0, 0, 0}, "[company/unit/ENG=1/0/0/0]"},
		{"a settings change to another owner", opened(1, 2), func(t *testing.T) []built {
			return []built{ccSettings(t, repo, ccOwned(ccOPS, VisibilityCompany, 1))}
		}, [4]int64{0, 2, 0, 0}, "[company/unit/OPS=0/2/0/0]"},
		{"a visibility change", opened(1), func(t *testing.T) []built {
			return []built{ccSettings(t, repo, ccOwned(ccENG, VisibilityUnit, 1))}
		}, [4]int64{0, 1, 0, 0}, "[unit/unit/ENG=0/1/0/0]"},
		{"an archive", opened(1), func(t *testing.T) []built {
			s := settingsFor("")
			s.Archived = true
			return []built{ccSettings(t, repo, s)}
		}, [4]int64{0, 1, 0, 0}, "[company/unit/ENG=0/1/0/0]"},
		{"a delete", opened(1), func(t *testing.T) []built {
			return []built{ccRecord(t, repo, OpDelete, LifecycleRecord{RepoID: repo, ConfigEpoch: 1})}
		}, [4]int64{0, 1, 0, 0}, "[]"},
		{"a restore under another owner", deleted, func(t *testing.T) []built {
			return []built{restored(ccOPS, 2)}
		}, [4]int64{0, 1, 1, 0}, "[company/unit/OPS=0/1/1/0]"},
		{"a deleted repository's owner moved, then its restore under a third", deleted,
			func(t *testing.T) []built {
				return []built{ccSettings(t, repo, ccOwned(ccOPS, VisibilityUnit, 1)), restored(ccQA, 2)}
			}, [4]int64{0, 1, 1, 0}, "[company/unit/QA=0/1/1/0]"},
		{"an import chunk closing a change", importing, func(t *testing.T) []built {
			return []built{ccRecord(t, repo, OpImportChunk, ImportChunk{RepoID: repo, ChunkID: "chunk-1",
				Node: "git-a", Seq: 1, Commands: []Command{move(ccSource(1), ccHead(1), ZeroOID)}})}
		}, [4]int64{0, 0, 1, 0}, "[company/unit/ENG=0/0/1/0]"},
		{"a mirror sync merging a change", func(t *testing.T, e *estate) {
			opened(1)(t, e)
			s := settingsFor("")
			s.MirrorFrom = &MirrorSource{URL: "https://example.com/c.git"}
			e.apply(ccSettings(t, repo, s), ccRecord(t, repo, OpImportDone, ImportDone{RepoID: repo, Node: "git-a"}))
		}, func(t *testing.T) []built {
			return []built{ccRecord(t, repo, OpMirrorSync, MirrorSync{RepoID: repo, SyncID: "sync-1", Node: "git-a",
				FetchedAt: applyClock, Commands: []Command{move(ccMain, oid("base"), ccHead(1))}})}
		}, [4]int64{0, 0, 0, 1}, "[company/unit/ENG=0/0/0/1]"},
		{"a restore of a repository whose import never finished", func(t *testing.T, e *estate) {
			importing(t, e)
			e.apply(ccRecord(t, repo, OpDelete, LifecycleRecord{RepoID: repo, ConfigEpoch: 1}))
		}, func(t *testing.T) []built { return []built{restored(ccENG, 2)} },
			[4]int64{0, 1, 0, 0}, "[company/unit/ENG=0/1/0/0]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := openApplyEstate(t)
			tc.given(t, e)
			e.changeCountsMatchRows()
			e.apply(tc.records(t)...)
			if got := e.changeCountsOf(repo); got != tc.counts {
				t.Errorf("the repository counts %v draft, open, closed and merged, want %v", got, tc.counts)
			}
			if got := e.tallies(); got != tc.tallies {
				t.Errorf("the tallies hold %s, want %s", got, tc.tallies)
			}
			e.changeCountsMatchRows()
		})
	}

	// A PURGE FOLLOWED TO ITS END: the part that cuts a change counts nothing out, the part that
	// takes it whole counts it out in the state it was in, and the final purge leaves no row.
	t.Run("a purge to its end", func(t *testing.T) {
		t.Parallel()
		e := openApplyEstate(t)
		e.apply(create(t, repo, "c"), ccRecord(t, repo, OpDelete, LifecycleRecord{RepoID: repo, ConfigEpoch: 1}))
		// THE FIRST CHANGE CARRIES MORE REVISIONS THAN A PART TAKES, so the first part cuts it.
		e.seedChangeRows(repo, 3, 1, 1, 0)
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 2 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, position, at)
			SELECT ?, i, printf('%040d', i), 'seat', 'agent', 1, 0 FROM k`, PurgePartRows+40, repo+"-change-000000")
		e.exec(`UPDATE code_changes SET state = 'closed' WHERE id = ?`, repo+"-change-000000")
		e.rederive()
		before := e.changeCountsOf(repo)
		e.apply(partOf(t, repo, 1))
		if got := e.num(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ?`, repo); got != 3 {
			t.Fatalf("the first part left %d changes, want the three it cut the first of", got)
		}
		if got := e.changeCountsOf(repo); got != before {
			t.Errorf("a part that cut a change moved the counts from %v to %v", before, got)
		}
		e.changeCountsMatchRows()
		for part := 2; e.num(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ?`, repo) > 0 && part < 10; part++ {
			e.apply(partOf(t, repo, part))
			e.changeCountsMatchRows()
		}
		if got := e.changeCountsOf(repo); got != [4]int64{} {
			t.Errorf("the parts that took every change left the counts %v", got)
		}
		e.apply(rec(t, RepoSubject(repo), OpPurge, Scope{NamePath(NameToken("c")), RepoPath(repo)},
			Purge{RepoID: repo, Name: "c", Reason: "test"}))
		if n := e.num(`SELECT COUNT(*) FROM code_repos WHERE id = ?`, repo); n != 0 {
			t.Fatalf("the purge left the repository's row")
		}
		if got := e.tallies(); got != "[]" {
			t.Errorf("the tallies hold %s after the purge, want none", got)
		}
	})
}

// A RECORD WRITES NO MORE ROWS THAN ITS DECIDE COUNTS: each change record, merge, landing
// and push book at its widest writes exactly the rows its decide counts, its tally row with them.
// Mutation: drop tallyRows, a readiness row or the tip work row from a bound, and one row is over.
func TestARecordWritesNoMoreRowsThanItsDecideCounts(t *testing.T) {
	t.Parallel()
	const repo = "repo-c"
	// queued opens, enqueues and stores an answered readiness for each change n, which a close,
	// a merge and a landing clear.
	queued := func(t *testing.T, draft bool, changes ...int) *estate {
		e := openApplyEstate(t)
		e.apply(create(t, repo, "c"), ccBranches(t, repo, "p0", changes...))
		for _, n := range changes {
			e.apply(ccOpen(t, repo, n, draft), ccRecord(t, repo, OpEnqueue, Enqueue{RepoID: repo,
				EntryID: ccChange(100 + n), ChangeID: ccChange(n), TargetRef: ccMain, HeadOID: ccHead(n),
				Method: MergeMethodMerge, Principal: ccSeat}))
			e.exec(`INSERT INTO code_mergeability (change_id, revision, target_ref, target_oid,
				target_position, state, conflicts_count, node, bytes, position, at, change_version,
				readiness_version, checks_position, chart_epoch, ready, waits_person, conflicts)
				VALUES (?, 1, ?, ?, 1, 'clean', 0, 'node-a', 0, 1, 0, 1, 1, 0, 0, 1, 0, '')`,
				ccChange(n), ccMain, oid("base"))
		}
		return e
	}
	given := func(t *testing.T, draft bool) *estate { return queued(t, draft, 1) }
	body := "what it does"
	moved := oid("moved")
	for _, tc := range []struct {
		name     string
		draft    bool
		closed   bool
		proposal ChangeProposal
		op       OpKind
		payload  any
	}{
		{name: "an open with a description", op: OpChangeOpen,
			proposal: ChangeProposal{Open: &ChangeOpen{}},
			payload: ChangeOpen{RepoID: repo, ChangeID: ccChange(2), SourceRef: ccSource(2), TargetRef: ccMain,
				Head: ccHead(2), Title: "Another", Body: body, Owner: ccSeat}},
		{name: "a draft of a queued change", op: OpChangeDraft, proposal: ChangeProposal{Draft: &ChangeDraft{}},
			payload: ChangeDraft{RepoID: repo, ChangeID: ccChange(1), By: ccSeat, Draft: true}},
		{name: "a close of a queued change", op: OpChangeClose, proposal: ChangeProposal{Close: &ChangeClose{}},
			payload: ChangeClose{RepoID: repo, ChangeID: ccChange(1), By: ccSeat}},
		{name: "a landing of a queued change", op: OpChangeLanded,
			proposal: ChangeProposal{Landed: &ChangeLanded{}},
			payload: ChangeLanded{RepoID: repo, ChangeID: ccChange(1), Revision: 1, HeadOID: ccHead(1),
				TargetRef: ccMain, TargetOID: ccHead(1), Node: "git-a"}},
		{name: "a reopen whose source moved", closed: true, op: OpChangeReopen,
			proposal: ChangeProposal{Reopen: &ChangeReopen{}},
			payload:  ChangeReopen{RepoID: repo, ChangeID: ccChange(1), By: ccSeat, Head: moved}},
		{name: "an edit", op: OpChangeEdit, proposal: ChangeProposal{Edit: &ChangeEdit{}},
			payload: ChangeEdit{RepoID: repo, ChangeID: ccChange(1), By: ccSeat, Body: &body}},
		{name: "a retarget", op: OpChangeRetarget, proposal: ChangeProposal{Retarget: &ChangeRetarget{}},
			payload: ChangeRetarget{RepoID: repo, ChangeID: ccChange(1), By: ccSeat, TargetRef: "refs/heads/next"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := given(t, tc.draft)
			if tc.closed {
				e.apply(ccRecord(t, repo, OpChangeClose, ChangeClose{RepoID: repo, ChangeID: ccChange(1), By: ccSeat}))
			}
			if got, want := e.applyCounting(ccRecord(t, repo, tc.op, tc.payload)), tc.proposal.decidedRows(); got != want {
				t.Errorf("the record wrote %d rows and its decide counted %d", got, want)
			}
			e.changeCountsMatchRows()
		})
	}

	// A green merge or landing pays main's owed tip work. Each one's pushes write 4 rows beside its own:
	// a merge's ref, pack, push row and counters; a landing's two push rows, main's ref and the counters.
	const pushRows = 4
	t.Run("a green merge of a queued change", func(t *testing.T) {
		t.Parallel()
		e := given(t, false)
		e.exec(`INSERT INTO code_tips_due (repo_id, target_ref) VALUES (?, ?)`, repo, ccMain)
		got := e.applyCounting(ccRecord(t, repo, OpMerge, Merge{RepoID: repo, MergeID: ccChange(300),
			ChangeID: ccChange(1), Method: MergeMethodMerge, TargetRef: ccMain, BaseOID: oid("base"),
			HeadOID: ccHead(1), CommitOID: oid("merged"), Pack: packOf(ccChange(300)), Node: "git-a",
			ReceivedAt: applyClock, Principal: ccSeat, Protected: true, Green: true}))
		if own, want := got-pushRows, mergeDecidedRows-routingNoticeRows; own != want {
			t.Errorf("the merge wrote %d rows of its own and its decide counted %d", own, want)
		}
		e.changeCountsMatchRows()
	})
	t.Run("a landing of two queued changes", func(t *testing.T) {
		t.Parallel()
		e := queued(t, false, 1, 2)
		e.exec(`INSERT INTO code_tips_due (repo_id, target_ref) VALUES (?, ?)`, repo, ccMain)
		l := Land{RepoID: repo, LandID: ccChange(400), TargetRef: ccMain, BaseOID: oid("base"),
			Node: "git-a", ReceivedAt: applyClock, Entries: []LandEntry{
				{EntryID: ccChange(101), ChangeID: ccChange(1), Commit: oid("landed-1")},
				{EntryID: ccChange(102), ChangeID: ccChange(2), Commit: oid("landed-2")}}}
		l.TipOID = l.Entries[1].Commit
		got := e.applyCounting(ccRecord(t, repo, OpLand, l))
		// No candidates or queue history are seeded, so the bound's train and horizon terms are 0.
		if own, want := got-pushRows, landDecidedRows(2)-MaxQueueDepth-queueHorizonRows; own != want {
			t.Errorf("the landing wrote %d rows of its own and its decide counted %d", own, want)
		}
		e.changeCountsMatchRows()
	})

	t.Run("a push merging one change and closing another", func(t *testing.T) {
		t.Parallel()
		e := openApplyEstate(t)
		e.apply(create(t, repo, "c"), ccBranches(t, repo, "p0", 1, 2), ccOpen(t, repo, 1, false),
			ccOpen(t, repo, 2, false))
		pushes := []PushEntry{pushEntry("p1", move(ccMain, oid("base"), ccHead(1)), move(ccSource(2), ccHead(2), ZeroOID))}
		var decided int
		if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
			book, err := readChangeBook(context.Background(), tx, repo, pushes, 0, 0)
			if err != nil {
				return err
			}
			for _, p := range pushes {
				book.move(p)
			}
			decided = book.decidedRows()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var written, tallied int
		if err := e.db.Tx(context.Background(), func(tx *sql.Tx) (err error) {
			at := applyContext{packed: 1 << 40, maxVariables: e.db.Caps().MaxVariables}
			written, tallied, err = applyChangeMoves(context.Background(), tx, at, repo, pushes)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if written+tallied != decided || tallied != tallyRows {
			t.Errorf("the push's changes wrote %d rows and %d tally rows, and its decide counted %d", written,
				tallied, decided)
		}
		e.changeCountsMatchRows()
	})
}

// THE TALLY ROWS TAKE NOTHING FROM THE HORIZONS: under a full backlog a push and a merge sweep or
// forget what a build without counts does, touching MaxRecordRows and their tally row on top.
// Mutation: charge the tally rows to landPushes or a merge's spent, and each sweeps one fewer.
func TestTheTallyRowsTakeNothingFromTheHorizons(t *testing.T) {
	t.Parallel()
	const repo, waiting = "repo-c", MaxRecordRows + 500
	given := func(t *testing.T) *estate {
		e := openApplyEstate(t)
		e.apply(create(t, repo, "c"), ccBranches(t, repo, "p0", 1), ccOpen(t, repo, 1, false))
		return e
	}
	tombstones := func(e *estate) {
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_refs (repo_id, ref, target, position, at)
			SELECT ?, printf('refs/heads/gone-%06d', i), '', 1, 0 FROM k`, waiting, repo)
		e.exec(`UPDATE code_repos SET tombstones = tombstones + ? WHERE id = ?`, waiting, repo)
	}
	// own is the rows the merging push writes as a build without the counts counts them: the ref,
	// the pack, the push row, the counters, the change's row and its readiness row.
	const own = 6
	t.Run("a push merging a change", func(t *testing.T) {
		t.Parallel()
		e := given(t)
		tombstones(e)
		got := e.applyCounting(pushOf(t, repo, pushEntry("p1", move(ccMain, oid("base"), ccHead(1)))))
		left := e.num(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = ''`, repo)
		if want := int64(waiting - (MaxRecordRows - own - 1)); left != want || got != MaxRecordRows+tallyRows {
			t.Errorf("the push touched %d rows and left %d tombstones, want %d and %d", got, left,
				MaxRecordRows+tallyRows, want)
		}
		e.changeCountsMatchRows()
	})
	t.Run("a merge record", func(t *testing.T) {
		t.Parallel()
		e := given(t)
		tombstones(e)
		got := e.applyCounting(ccRecord(t, repo, OpMerge, Merge{RepoID: repo, MergeID: ccChange(300),
			ChangeID: ccChange(1), Method: MergeMethodMerge, TargetRef: ccMain, BaseOID: oid("base"),
			HeadOID: ccHead(1), CommitOID: oid("merged"), Pack: packOf(ccChange(300)), Node: "git-a",
			ReceivedAt: applyClock, Principal: ccSeat}))
		left := e.num(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = ''`, repo)
		// THE MERGE'S OWN ROW is its change, its spent (it holds no readiness or queue entry); its
		// push's are the ref, the pack, the push row and the counters; the sweep's mark one more.
		if want := int64(waiting - (MaxRecordRows - 1 - 4 - 1)); left != want || got != MaxRecordRows+tallyRows {
			t.Errorf("the merge touched %d rows and left %d tombstones, want %d and %d", got, left,
				MaxRecordRows+tallyRows, want)
		}
		e.changeCountsMatchRows()
	})
	t.Run("a push forgetting drops", func(t *testing.T) {
		t.Parallel()
		e := given(t)
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_ref_drops (repo_id, at, position, ref, old)
			SELECT ?, 0, 1, printf('refs/heads/gone-%06d', i), printf('%040d', i) FROM k`,
			waiting, repo)
		got := e.applyCounting(pushOf(t, repo, pushEntry("p1", move(ccMain, oid("base"), ccHead(1)))))
		left := e.num(`SELECT COUNT(*) FROM code_ref_drops WHERE repo_id = ?`, repo)
		if want := int64(waiting - (MaxRecordRows - own)); left != want || got != MaxRecordRows+tallyRows {
			t.Errorf("the push touched %d rows and left %d drops, want %d and %d", got, left,
				MaxRecordRows+tallyRows, want)
		}
		e.changeCountsMatchRows()
	})
}

// THE CHANGE COUNTS REWRITE NO INDEX: the statement every state move runs deletes no entry of
// any code_repos index, while a write of a column one indexes is seen to.
// Mutation: an index on open_changes, and every state move rewrites it.
func TestTheChangeCountsRewriteNoIndex(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-c", "c"))
	ctx := context.Background()
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		roots := indexRootsOf(ctx, t, tx, "code_repos")
		if got := rewrites(t, program(ctx, t, tx, `UPDATE code_repos SET name = ? WHERE id = ?`,
			[]any{"d", "repo-c"}), roots); !slices.Contains(got, "code_repos_name_idx") {
			t.Errorf("a rename rewrites %q, want the name's index: a reading that sees none passes everything", got)
		}
		if got := rewrites(t, program(ctx, t, tx, changeCountsSQL, []any{1, -1, 0, 0, "repo-c"}), roots); len(got) > 0 {
			t.Errorf("a state move rewrites %q of code_repos' indexes, want none", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// indexRootsOf is every index of table by its root page.
func indexRootsOf(ctx context.Context, t *testing.T, tx *sql.Tx, table string) map[int64]string {
	t.Helper()
	rows, err := tx.QueryContext(ctx, `SELECT name, rootpage FROM sqlite_master WHERE type = 'index' AND tbl_name = ?`,
		table)
	if err != nil {
		t.Fatalf("read %s's indexes: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]string{}
	for rows.Next() {
		var name string
		var root int64
		if err := rows.Scan(&name, &root); err != nil {
			t.Fatalf("read %s's indexes: %v", table, err)
		}
		out[root] = name
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s's indexes: %v", table, err)
	}
	return out
}

// countedWriters is every function of the package that writes a change's row or state, or a
// repository's status, owner or visibility, and how it keeps the counts: the counting function
// its body calls, callersCount where each caller counts, or "none: why".
var countedWriters = map[string]string{
	"(*Applier).applyChangeOpen":  "countChanges",
	"(*Applier).applyChangeWrite": "countChanges",
	"applyChangeMoves":            "countChanges",
	"(*Applier).applyMerge":       "countChanges",
	"(*Applier).applyLand":        "countChanges",
	"(*Applier).purgeChanges":     "countChanges",
	"(*Applier).applySettings":    "retally",
	"(*Applier).applyDelete":      "retally",
	"(*Applier).applyRestore":     "retally",
	"writeChanges":                callersCount,
	"(*changeBook).write":         callersCount,
	"(*Applier).applyCreate":      "retally",
	"(*Applier).applyImportDone":  "retally",
	"(*Applier).applyPurgePart":   "retally",
	"(*Applier).applyPurge":       "retally",
}

// callersCount marks a writer whose every caller counts what it writes.
const callersCount = "its callers count"

// writesCounted is a statement that writes what the counts are kept of: a change's row whole or
// its state, or a repository's row whole or its status, owner or visibility.
var writesCounted = regexp.MustCompile(`(?s)(INSERT INTO|DELETE FROM) code_(changes|repos)([^_a-z]|$)|` +
	`UPDATE code_changes SET\s((?:[^W]|W[^H])*?)\bstate\s*=|` +
	`UPDATE code_repos SET\s((?:[^W]|W[^H])*?)\b(status|owner|owner_kind|visibility)\s*=`)

// EVERY WRITER OF A COUNTED ROW SAYS HOW IT COUNTS: the functions writing a change's state or a
// repository's standing are exactly countedWriters, each calling the counting function it names.
// Mutation: a new uncounted writer, a stale entry or a dropped countChanges call fails.
func TestEveryWriterOfACountedRowSaysHowItCounts(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	// THE PACKAGE TYPED ON ITS OWN, its imports empty: only its own methods are resolved here.
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: emptyImports{}, Error: func(error) {}}
	_, _ = conf.Check("github.com/crewlet/crewlet/internal/gitlet", fset, files, info)

	matched := func(lit *ast.BasicLit) bool {
		if lit.Kind != token.STRING {
			return false
		}
		v, err := strconv.Unquote(lit.Value)
		return err == nil && writesCounted.MatchString(v)
	}
	// THE CONSTANTS AND VARIABLES WHOSE VALUE HOLDS SUCH A STATEMENT.
	statements := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || (g.Tok != token.CONST && g.Tok != token.VAR) {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, value := range vs.Values {
					ast.Inspect(value, func(n ast.Node) bool {
						if lit, ok := n.(*ast.BasicLit); ok && matched(lit) && i < len(vs.Names) {
							statements[vs.Names[i].Name] = true
						}
						return true
					})
				}
			}
		}
	}
	// EVERY FUNCTION, what it calls, and whether it writes such a statement itself.
	writes := map[string]bool{}
	calls := map[string]map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := declKey(fn)
			calls[key] = map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BasicLit:
					if matched(n) {
						writes[key] = true
					}
				case *ast.Ident:
					if statements[n.Name] {
						writes[key] = true
					}
				case *ast.CallExpr:
					if callee := calleeKey(info, n); callee != "" {
						calls[key][callee] = true
					}
				}
				return true
			})
		}
	}
	for key, called := range calls {
		for callee := range called {
			if countedWriters[callee] == callersCount {
				writes[key] = true
			}
		}
	}
	for _, key := range slices.Sorted(maps.Keys(writes)) {
		how, declared := countedWriters[key]
		switch {
		case !declared:
			t.Errorf("%s writes a change's state or a repository's standing and does not say how it keeps the "+
				"counts: name it in countedWriters", key)
		case how == callersCount, strings.HasPrefix(how, "none: "):
		case !calls[key][how]:
			t.Errorf("%s is declared to count through %s and never calls it", key, how)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(countedWriters)) {
		if !writes[key] {
			t.Errorf("countedWriters names %s, which writes no counted row", key)
		}
	}
}

// emptyImports answers every import as an empty package, so the package types on its own.
type emptyImports struct{}

func (emptyImports) Import(p string) (*types.Package, error) {
	pkg := types.NewPackage(p, path.Base(p))
	pkg.MarkComplete()
	return pkg, nil
}

// declKey names a function as calleeKey does: its name, or (*T).name and T.name for a method.
func declKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv, star := fn.Recv.List[0].Type, ""
	if s, ok := recv.(*ast.StarExpr); ok {
		recv, star = s.X, "*"
	}
	if g, ok := recv.(*ast.IndexExpr); ok {
		recv = g.X
	}
	id, _ := recv.(*ast.Ident)
	if id == nil {
		return fn.Name.Name
	}
	if star != "" {
		return "(*" + id.Name + ")." + fn.Name.Name
	}
	return id.Name + "." + fn.Name.Name
}

// calleeKey is the package function or method a call names, "" for any other.
func calleeKey(info *types.Info, call *ast.CallExpr) string {
	var obj types.Object
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		obj = info.Uses[fn]
	case *ast.SelectorExpr:
		if sel := info.Selections[fn]; sel != nil {
			obj = sel.Obj()
		}
	}
	f, ok := obj.(*types.Func)
	if !ok {
		return ""
	}
	sig, _ := f.Type().(*types.Signature)
	if sig == nil || sig.Recv() == nil {
		return f.Name()
	}
	recv, star := sig.Recv().Type(), ""
	if p, ok := recv.(*types.Pointer); ok {
		recv, star = p.Elem(), "*"
	}
	named, ok := recv.(*types.Named)
	if !ok {
		return ""
	}
	if star != "" {
		return "(*" + named.Obj().Name() + ")." + f.Name()
	}
	return named.Obj().Name() + "." + f.Name()
}

// CHANGE COUNTS COUNT EVERY STATE: each state moves exactly its own field and, where live,
// Live; a move to the same state is nothing; and counts plus their negation are nothing.
// Mutation: a state added to ChangeStates and not counted.
func TestChangeCountsCountEveryState(t *testing.T) {
	t.Parallel()
	for _, s := range ChangeStates() {
		var c ChangeCounts
		c.add(s, 1)
		moved := 0
		for _, n := range []int64{c.Draft, c.Open, c.Closed, c.Merged} {
			moved += int(n)
		}
		if moved != 1 || (c.Live == 1) != s.Live() || c.zero() {
			t.Errorf("one change %s counts %+v, want exactly its own state, and live only where it is", s, c)
		}
		var same ChangeCounts
		if same.move(s, s); !same.zero() || same.Live != 0 {
			t.Errorf("a move from %s to itself counts %+v", s, same)
		}
		if sum := c.plus(c.negated()); sum != (ChangeCounts{}) {
			t.Errorf("%+v and its negation sum to %+v", c, sum)
		}
	}
}

// THE CHANGE COUNTS ARE THE ROWS: random records leave the counts what the rows imply after each
// one, through a re-derivation midway, a replay from zero and a re-derivation of the end.
// Mutation: drop a tally move, a draft check, a purge's decrement or retally's prune, and one differs.
func TestTheChangeCountsAreTheRows(t *testing.T) {
	t.Parallel()
	applied := map[string]bool{}
	for seed := uint64(1); seed <= 4; seed++ {
		w := newCountsWorld(t, seed)
		upgradeAt := 20 + w.rng.IntN(160)
		for step := range 200 {
			if step == upgradeAt {
				w.e.forgetChangeCounts()
				w.e.rederive()
				w.e.changeCountsMatchRows()
			}
			op := w.step()
			if drift := w.e.changeCountsDrift(); drift != "" {
				t.Fatalf("seed %d step %d, %s: %s", seed, step, op, drift)
			}
		}
		maps.Copy(applied, w.applied)

		// THE REPLAY FROM ZERO writes the same counts.
		replayed := openApplyEstate(t)
		replayed.apply(w.records...)
		if got, want := countsTables(replayed), countsTables(w.e); got != want {
			t.Errorf("seed %d: the replay from zero holds\n%s\nand the estate it replays\n%s", seed, got, want)
		}
		// THE RE-DERIVATION of rows kept writes nothing, and of the rows forgotten the same rows.
		kept := countsTables(w.e)
		if n := w.e.rederive(); n != 0 {
			t.Errorf("seed %d: re-deriving the counts the applies kept wrote %d rows", seed, n)
		}
		w.e.forgetChangeCounts()
		w.e.rederive()
		if got := countsTables(w.e); got != kept {
			t.Errorf("seed %d: the re-derivation holds\n%s\nand the applies kept\n%s", seed, got, kept)
		}
	}
	for _, op := range countsOps {
		if !applied[op.name] {
			t.Errorf("no seed applied %s: the property holds nothing of it", op.name)
		}
	}
}

// countsTables is the counts' rows whole: every repository's four columns, every change tally
// row and every repository tally row.
func countsTables(e *estate) string {
	e.t.Helper()
	return e.str(`SELECT COALESCE((SELECT group_concat(r, ',') FROM (SELECT id || ':' || draft_changes || '/' ||
			open_changes || '/' || closed_changes || '/' || merged_changes AS r FROM code_repos ORDER BY id)), '')
		|| ' | ' || COALESCE((SELECT group_concat(r, ',') FROM (SELECT visibility || '/' || owner_kind || '/' ||
			owner || ':' || draft_changes || '/' || open_changes || '/' || closed_changes || '/' || merged_changes
			AS r FROM code_change_tallies ORDER BY visibility, owner_kind, owner)), '')
		|| ' | ' || COALESCE((SELECT group_concat(r, ',') FROM (SELECT visibility || '/' || owner_kind || '/' ||
			owner || '/' || kind || ':' || importing_repos || '/' || active_repos || '/' || archived_repos || '/' ||
			deleted_repos || '/' || purging_repos AS r FROM code_repo_tallies
			ORDER BY visibility, owner_kind, owner, kind)), '')`)
}

// countsWorld is one seed's estate, the records applied to it in order, and the ops that
// changed it.
type countsWorld struct {
	t       *testing.T
	e       *estate
	rng     *rand.Rand
	records []built
	applied map[string]bool
	next    int
}

func newCountsWorld(t *testing.T, seed uint64) *countsWorld {
	return &countsWorld{t: t, e: openApplyEstate(t), rng: rand.New(rand.NewPCG(seed, 13)),
		applied: map[string]bool{}}
}

// countsOp is one kind of record the property draws: nil where the estate holds nothing it applies to.
type countsOp struct {
	name string
	make func(w *countsWorld) []built
}

// id is a fresh id for a record's own row.
func (w *countsWorld) id() string {
	w.next++
	return fmt.Sprintf("0192f6c1-0000-7000-8000-%012d", w.next)
}

// pick is one of the rows query answers, each its columns as text; nil where it answers none.
func (w *countsWorld) pick(query string, args ...any) []string {
	w.t.Helper()
	var all [][]string
	if err := w.e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		columns, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			values := make([]sql.NullString, len(columns))
			into := make([]any, len(columns))
			for i := range values {
				into[i] = &values[i]
			}
			if err = rows.Scan(into...); err != nil {
				return err
			}
			row := make([]string, len(columns))
			for i, v := range values {
				row[i] = v.String
			}
			all = append(all, row)
		}
		return rows.Err()
	}); err != nil {
		w.t.Fatalf("%q: %v", query, err)
	}
	if len(all) == 0 {
		return nil
	}
	return all[w.rng.IntN(len(all))]
}

// fingerprint is what any record that applies moves: repositories, changes, refs and queue entries.
func (w *countsWorld) fingerprint() string {
	return w.e.str(`SELECT COALESCE((SELECT group_concat(id || status || owner || visibility || version, ',')
			FROM (SELECT * FROM code_repos ORDER BY id)), '') || '|' ||
		COALESCE((SELECT group_concat(id || state || version || head_oid, ',')
			FROM (SELECT * FROM code_changes ORDER BY id)), '') || '|' ||
		(SELECT COUNT(*) || '/' || COALESCE(SUM(length(target)), 0) FROM code_refs) || '|' ||
		(SELECT COUNT(*) FROM code_queue)`)
}

// step applies one drawn op's records and answers its name.
func (w *countsWorld) step() string {
	for {
		op := countsOps[w.rng.IntN(len(countsOps))]
		records := op.make(w)
		if records == nil {
			continue
		}
		before := w.fingerprint()
		for _, r := range records {
			w.e.apply(r)
			w.records = append(w.records, r)
		}
		if op.name == "a redelivery" || w.fingerprint() != before {
			w.applied[op.name] = true
		}
		return op.name
	}
}

// The property's repositories in use and their branches, and the live changes and their rows.
const (
	ccInUse      = `SELECT id FROM code_repos WHERE status IN ('importing', 'active', 'archived')`
	ccLiveChange = `SELECT c.id, c.repo_id, c.state, c.source_ref, c.head_oid, c.revision, c.target_ref,
		COALESCE((SELECT target FROM code_refs WHERE repo_id = c.repo_id AND ref = c.target_ref), '')
		FROM code_changes c JOIN code_repos r ON r.id = c.repo_id
		WHERE c.state IN ('draft', 'open') AND r.status IN ('importing', 'active', 'archived')`
)

// changeMoves are the three moves a push, an import chunk or a mirror sync makes of a live change
// of repo: its source moved on, its target set to its head (a merge), or its source deleted (a
// close); nil where repo holds no live change.
func (w *countsWorld) changeMoves(repo string) []Command {
	c := w.pick(ccLiveChange+` AND c.repo_id = ?`, repo)
	if c == nil {
		return nil
	}
	switch w.rng.IntN(3) {
	case 0:
		tip := w.e.str(`SELECT COALESCE((SELECT target FROM code_refs WHERE repo_id = ? AND ref = ?), '')`, repo, c[3])
		if tip == "" {
			return nil
		}
		return []Command{move(c[3], tip, oid(w.id()))}
	case 1:
		return []Command{move(c[6], cmpOr(c[7], ZeroOID), c[4])}
	}
	tip := w.e.str(`SELECT COALESCE((SELECT target FROM code_refs WHERE repo_id = ? AND ref = ?), '')`, repo, c[3])
	if tip == "" {
		return nil
	}
	return []Command{move(c[3], tip, ZeroOID)}
}

// repoSettings is the settings a property's record carries for repo: its row's own, so a record
// moves only what the op means to.
func (w *countsWorld) repoSettings(repo string) RepoSettings {
	row := w.pick(`SELECT owner_kind, owner, visibility, config_epoch, status, mirror_from FROM code_repos
		WHERE id = ?`, repo)
	s := settingsFor("")
	s.Owner = Owner{Kind: OwnerKind(row[0]), Ref: row[1]}
	s.Visibility = Visibility(row[2])
	s.ConfigEpoch, _ = strconv.ParseInt(row[3], 10, 64)
	s.Archived = row[4] == string(RepoStatusArchived)
	if row[5] != "" {
		s.MirrorFrom = &MirrorSource{URL: "https://example.com/mirror.git"}
	}
	return s
}

var ccOwners = []Owner{ccENG, ccOPS, {Kind: OwnerSeat, Ref: "dev"}}

// countsOps are the records the property draws.
var countsOps = []countsOp{
	{"a create", func(w *countsWorld) []built {
		id := w.id()
		name := fmt.Sprintf("r%d", w.next)
		s := ccOwned(ccOwners[w.rng.IntN(len(ccOwners))], []Visibility{VisibilityCompany, VisibilityUnit}[w.rng.IntN(2)], 1)
		c := CreateRepo{RepoID: id, Name: name, Settings: s}
		if w.rng.IntN(4) == 0 {
			c.Import = &ImportSource{URL: "https://example.com/" + name + ".git"}
		}
		return []built{rec(w.t, NameSubject(name), OpCreate, Scope{NamePath(NameToken(name)), RepoPath(id)}, c)}
	}},
	{"a push of a branch", func(w *countsWorld) []built {
		r := w.pick(ccInUse)
		if r == nil {
			return nil
		}
		return []built{pushOf(w.t, r[0], pushEntry(w.id(), move("refs/heads/b-"+w.id(), ZeroOID, oid(w.id()))))}
	}},
	{"an open", func(w *countsWorld) []built {
		b := w.pick(`SELECT f.repo_id, f.ref, f.target FROM code_refs f JOIN code_repos r ON r.id = f.repo_id
			WHERE f.target != '' AND f.ref LIKE 'refs/heads/b-%' AND r.status IN ('importing', 'active', 'archived')
			AND NOT EXISTS (SELECT 1 FROM code_changes c WHERE c.repo_id = f.repo_id AND c.source_ref = f.ref
				AND c.state IN ('draft', 'open'))`)
		if b == nil {
			return nil
		}
		return []built{ccRecord(w.t, b[0], OpChangeOpen, ChangeOpen{RepoID: b[0], ChangeID: w.id(), SourceRef: b[1],
			TargetRef: ccMain, Head: b[2], Title: "A change", Draft: w.rng.IntN(2) == 0, Owner: ccSeat})}
	}},
	{"a push moving a change", func(w *countsWorld) []built { return w.pushMove(0) }},
	{"a push merging a change", func(w *countsWorld) []built { return w.pushMove(1) }},
	{"a push closing a change", func(w *countsWorld) []built { return w.pushMove(2) }},
	{"a push opening a change", func(w *countsWorld) []built {
		r := w.pick(ccInUse)
		if r == nil {
			return nil
		}
		entry := pushEntry(w.id(), move("refs/heads/b-"+w.id(), ZeroOID, oid(w.id())))
		entry.Principal = ccSeat
		entry.Change = &ChangeOption{Target: ccMain, Title: "Opened by its push", Draft: w.rng.IntN(2) == 0}
		return []built{pushOf(w.t, r[0], entry)}
	}},
	{"a draft's state", func(w *countsWorld) []built {
		c := w.pick(ccLiveChange)
		if c == nil {
			return nil
		}
		return []built{ccRecord(w.t, c[1], OpChangeDraft, ChangeDraft{RepoID: c[1], ChangeID: c[0], By: ccSeat,
			Draft: c[2] != string(ChangeStateDraft)})}
	}},
	{"a close", func(w *countsWorld) []built {
		c := w.pick(ccLiveChange)
		if c == nil {
			return nil
		}
		return []built{ccRecord(w.t, c[1], OpChangeClose, ChangeClose{RepoID: c[1], ChangeID: c[0], By: ccSeat})}
	}},
	{"a reopen", func(w *countsWorld) []built {
		c := w.pick(`SELECT id, repo_id, head_oid FROM code_changes WHERE state = 'closed'`)
		if c == nil {
			return nil
		}
		return []built{ccRecord(w.t, c[1], OpChangeReopen, ChangeReopen{RepoID: c[1], ChangeID: c[0], By: ccSeat,
			Head: c[2]})}
	}},
	{"a landing", func(w *countsWorld) []built {
		c := w.pick(ccLiveChange)
		if c == nil {
			return nil
		}
		revision, _ := strconv.ParseInt(c[5], 10, 64)
		return []built{ccRecord(w.t, c[1], OpChangeLanded, ChangeLanded{RepoID: c[1], ChangeID: c[0],
			Revision: revision, HeadOID: c[4], TargetRef: c[6], TargetOID: c[4], Node: "git-a"})}
	}},
	{"a merge", func(w *countsWorld) []built {
		c := w.pick(ccLiveChange + ` AND c.state = 'open' AND EXISTS (SELECT 1 FROM code_refs
			WHERE repo_id = c.repo_id AND ref = c.target_ref AND target != '')`)
		if c == nil {
			return nil
		}
		id := w.id()
		m := Merge{RepoID: c[1], MergeID: id, ChangeID: c[0], Method: MergeMethodMerge, TargetRef: c[6],
			BaseOID: c[7], HeadOID: c[4], CommitOID: oid(id), Pack: packOf(id), Node: "git-a",
			ReceivedAt: applyClock, Principal: ccSeat}
		tip := w.e.str(`SELECT COALESCE((SELECT target FROM code_refs WHERE repo_id = ? AND ref = ?), '')`, c[1], c[3])
		if tip == c[4] && w.rng.IntN(2) == 0 {
			m.DeleteSource, m.SourceRef = true, c[3]
		}
		return []built{ccRecord(w.t, c[1], OpMerge, m)}
	}},
	{"a landing of the queue", func(w *countsWorld) []built {
		c := w.pick(ccLiveChange + ` AND c.state = 'open' AND EXISTS (SELECT 1 FROM code_refs
			WHERE repo_id = c.repo_id AND ref = c.target_ref AND target != '') AND NOT EXISTS (SELECT 1
			FROM code_queue q WHERE q.change_id = c.id AND q.state IN ('waiting', 'testing'))`)
		if c == nil {
			return nil
		}
		entry, landID := w.id(), w.id()
		l := Land{RepoID: c[1], LandID: landID, TargetRef: c[6], BaseOID: c[7], Node: "git-a",
			ReceivedAt: applyClock, Entries: []LandEntry{{EntryID: entry, ChangeID: c[0], Commit: oid(landID)}}}
		l.TipOID = l.Entries[0].Commit
		return []built{ccRecord(w.t, c[1], OpEnqueue, Enqueue{RepoID: c[1], EntryID: entry, ChangeID: c[0],
			TargetRef: c[6], HeadOID: c[4], Method: MergeMethodMerge, Principal: ccSeat}),
			ccRecord(w.t, c[1], OpLand, l)}
	}},
	{"an import chunk", func(w *countsWorld) []built {
		r := w.pick(`SELECT id, import_seq FROM code_repos WHERE status = 'importing'`)
		if r == nil {
			return nil
		}
		seq, _ := strconv.ParseInt(r[1], 10, 64)
		commands := w.changeMoves(r[0])
		if commands == nil {
			commands = []Command{move("refs/heads/b-"+w.id(), ZeroOID, oid(w.id()))}
		}
		return []built{ccRecord(w.t, r[0], OpImportChunk, ImportChunk{RepoID: r[0], ChunkID: w.id(), Node: "git-a",
			Seq: seq + 1, Commands: commands})}
	}},
	{"an import's end", func(w *countsWorld) []built {
		r := w.pick(`SELECT id, import_seq FROM code_repos WHERE status = 'importing'`)
		if r == nil {
			return nil
		}
		seq, _ := strconv.ParseInt(r[1], 10, 64)
		return []built{ccRecord(w.t, r[0], OpImportDone, ImportDone{RepoID: r[0], Node: "git-a", Seq: seq})}
	}},
	{"a mirror sync", func(w *countsWorld) []built {
		r := w.pick(`SELECT id FROM code_repos WHERE status = 'active' AND mirror_from != ''`)
		if r == nil {
			return nil
		}
		commands := w.changeMoves(r[0])
		if commands == nil {
			return nil
		}
		return []built{ccRecord(w.t, r[0], OpMirrorSync, MirrorSync{RepoID: r[0], SyncID: w.id(), Node: "git-a",
			FetchedAt: applyClock, Commands: commands})}
	}},
	{"a settings change", func(w *countsWorld) []built {
		r := w.pick(`SELECT id, status FROM code_repos`)
		if r == nil {
			return nil
		}
		s := w.repoSettings(r[0])
		switch w.rng.IntN(4) {
		case 0:
			s.Owner = ccOwners[w.rng.IntN(len(ccOwners))]
		case 1:
			s.Visibility = []Visibility{VisibilityCompany, VisibilityUnit}[w.rng.IntN(2)]
		case 2:
			s.Archived = !s.Archived
			s.MirrorFrom = nil
		default:
			if r[1] != string(RepoStatusActive) || s.MirrorFrom != nil {
				return nil
			}
			s.MirrorFrom = &MirrorSource{URL: "https://example.com/mirror.git"}
		}
		return []built{ccSettings(w.t, r[0], s)}
	}},
	{"a delete", func(w *countsWorld) []built {
		r := w.pick(ccInUse)
		if r == nil {
			return nil
		}
		s := w.repoSettings(r[0])
		return []built{ccRecord(w.t, r[0], OpDelete, LifecycleRecord{RepoID: r[0], ConfigEpoch: s.ConfigEpoch})}
	}},
	{"a restore", func(w *countsWorld) []built {
		r := w.pick(`SELECT id FROM code_repos WHERE status = 'deleted'`)
		if r == nil {
			return nil
		}
		s := w.repoSettings(r[0])
		s.ConfigEpoch++
		s.Owner = ccOwners[w.rng.IntN(len(ccOwners))]
		return []built{ccRecord(w.t, r[0], OpRestore, LifecycleRecord{RepoID: r[0], ConfigEpoch: s.ConfigEpoch,
			Settings: &s})}
	}},
	{"a purge part", func(w *countsWorld) []built {
		r := w.pick(`SELECT id, purge_parts FROM code_repos WHERE status IN ('deleted', 'purging')`)
		if r == nil {
			return nil
		}
		parts, _ := strconv.Atoi(r[1])
		return []built{partOf(w.t, r[0], parts+1)}
	}},
	{"a purge", func(w *countsWorld) []built {
		r := w.pick(`SELECT id, name FROM code_repos WHERE status IN ('deleted', 'purging')`)
		if r == nil {
			return nil
		}
		return []built{rec(w.t, RepoSubject(r[0]), OpPurge, Scope{NamePath(NameToken(r[1])), RepoPath(r[0])},
			Purge{RepoID: r[0], Name: r[1], Reason: "test"})}
	}},
	{"a redelivery", func(w *countsWorld) []built {
		if len(w.records) == 0 {
			return nil
		}
		return []built{w.records[w.rng.IntN(len(w.records))]}
	}},
}

// pushMove is a push of one of changeMoves' moves, by kind: 0 a source moved, 1 a merge, 2 a close.
func (w *countsWorld) pushMove(kind int) []built {
	c := w.pick(ccLiveChange)
	if c == nil {
		return nil
	}
	tip := w.e.str(`SELECT COALESCE((SELECT target FROM code_refs WHERE repo_id = ? AND ref = ?), '')`, c[1], c[3])
	var cmd Command
	switch {
	case kind == 1:
		cmd = move(c[6], cmpOr(c[7], ZeroOID), c[4])
	case tip == "":
		return nil
	case kind == 0:
		cmd = move(c[3], tip, oid(w.id()))
	default:
		cmd = move(c[3], tip, ZeroOID)
	}
	return []built{pushOf(w.t, c[1], pushEntry(w.id(), cmd))}
}
