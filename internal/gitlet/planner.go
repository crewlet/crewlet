package gitlet

// How a statement reaches its key on the pinned driver's planner, measured on
// this schema with no statistics (nothing in the engine, nor the plan fixture,
// runs ANALYZE), and held by TestEveryCodeIndexServesARegisteredQuery to the exact seek.
//
// A PRIMARY KEY SHARING ITS LEADING COLUMN WITH ANOTHER INDEX IS NOT CHOSEN
// FOR ITS WHOLE KEY: a lookup of one ref sought code_refs_changed_idx on
// repo_id alone, every ref the repository holds through the heap. So a
// statement comparing a key past its first column NAMES the key's index
// ([refsKey], [packsKey], [queueKey]), as internal/worklet's field filter
// does; the name is also a guard, failing with `no such index` on a rename.

// AN OR'D KEYSET COMPARISON SEEKS NOTHING PAST THE EQUALITY, so each keyset
// page also states the bound its first ordered column can seek
// (`position >= ?`), and the OR only sorts out the rows at the cursor.
//
// A COLUMN OF json_each IS NOT A SEEK KEY, BUT AN EXPRESSION OVER ONE IS:
// `CAST(value AS TEXT)` seeks where a bare `value` scanned. An IN list never
// seeks a key's second column, so a batch of refs or packs is DRIVEN from its
// list (`json_each … CROSS JOIN`, which fixes the order), one whole-key seek
// an element, and a batch deletion deletes the rowids that join found.

// A RANGE THAT STOPS AT ITS FIRST LIVE REF READS [liveRefsKey]: over
// [refsKey] the target test filters each row, so the range stepped over every
// tombstone (up to [MaxTombstonesPerRepo]) first. Such a statement repeats
// the index's predicate verbatim so the planner may use it at all.

// A PARTIAL INDEX'S IN-LIST IS MATCHED AS WRITTEN, NOT AS A SET: the indexes
// partial on a change's live states serve only a statement repeating
// [liveChanges]; another order, or `state = 'open'`, reads another index
// (TestTheLiveIndexesHoldTheLiveStates).
//
// TWO ORDERED LOOPS SORT: an ORDER BY naming a join's inner key after the
// outer's (`c.number, r.n`) sorts every row before its LIMIT, the outer index
// not being UNIQUE (the schema refuses one that is). So a bounded walk over a repository's changes orders by
// the change alone and takes each change whole ([purgeChanges]).
const (
	// refsKey is code_refs' PRIMARY KEY index, (repo_id, ref).
	refsKey = "sqlite_autoindex_code_refs_1"
	// liveRefsKey is code_refs_live_idx, (repo_id, ref) WHERE target != ''
	// (replicated migration 0082): the live refs alone, in key order.
	liveRefsKey = "code_refs_live_idx"
	// movedRefsKey is code_refs_moved_idx, (repo_id, at, ref) WHERE
	// [movedRefs] (replicated migration 0170): the live branches by when each
	// last moved.
	movedRefsKey = "code_refs_moved_idx"
	// movedRefs is the predicate code_refs_moved_idx is partial on, which every
	// statement over it repeats verbatim.
	movedRefs = `target != '' AND ref >= 'refs/heads/' AND ref < 'refs/heads0'`
	// changesSourceKey is code_changes_source_idx, (repo_id, source_ref,
	// number) (replicated migration 0170): the changes from one branch.
	changesSourceKey = "code_changes_source_idx"
	// protectedReposKey is code_repos_protected_idx, (name) WHERE
	// [protectedRepos] (replicated migration 0145): the repositories a rule
	// protects, in name order.
	protectedReposKey = "code_repos_protected_idx"
	// protectedRepos is the predicate code_repos_protected_idx is partial
	// on, which a statement over it repeats verbatim.
	protectedRepos = `protected = 1`
	// kindReposKey is code_repos_kind_idx, (kind, name): one kind's repositories in name order.
	kindReposKey = "code_repos_kind_idx"
	// pushedReposKey is code_repos_pushed_idx, (kind, last_push_at, name) WHERE [reposInUse]:
	// one kind's repositories in use by last push.
	pushedReposKey = "code_repos_pushed_idx"
	// reposInUse is the predicate code_repos_pushed_idx is partial on, every [RepoStatus.live]
	// status in [RepoStatuses]' order, which a statement over it repeats verbatim.
	reposInUse = `status IN ('importing', 'active', 'archived')`
	// packsKey is code_packs' PRIMARY KEY index, (repo_id, pack_id, part).
	packsKey = "sqlite_autoindex_code_packs_1"
	// changesNumberKey is code_changes_number_idx, (repo_id, number)
	// (replicated migrations 0086 and 0094): a repository's changes in number
	// order, the only way to its revisions, authors and bodies, which carry
	// no repo_id.
	changesNumberKey = "code_changes_number_idx"
	// changesRevertsKey is code_changes_reverts_idx, (repo_id, reverts, number)
	// WHERE reverts > 0 (replicated migration 0190): the changes reverting one.
	changesRevertsKey = "code_changes_reverts_idx"
	// liveChanges is the predicate code_changes_open_idx and
	// code_changes_target_idx are partial on: a change in a [ChangeState.Live]
	// state, spelled in [ChangeStates]' order.
	liveChanges = `state IN ('draft', 'open')`
	// retiredKey is code_packs_retired's PRIMARY KEY index, (repo_id,
	// pack_id, part), which shares its leading column with
	// code_packs_retired_repo_idx as packsKey does with code_packs_added_idx.
	retiredKey = "sqlite_autoindex_code_packs_retired_1"
	// dropsKey is code_ref_drops' PRIMARY KEY index, (repo_id, at, position,
	// ref, old) (replicated migration 0089): a repository's ref drops in the
	// order every statement over them reads. The table's only index, it is
	// named for the guard alone.
	dropsKey = "sqlite_autoindex_code_ref_drops_1"
	// queueKey is code_queue's PRIMARY KEY index, (repo_id, entry_id), which
	// shares its leading column with code_queue_enqueued_idx (replicated
	// migration 0141), so a seek or walk in its order names it.
	queueKey = "sqlite_autoindex_code_queue_1"
)
