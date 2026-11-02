package code

// HOW A STATEMENT REACHES ITS KEY ON THIS DRIVER.
//
// Three rules of the pinned driver's planner, each measured on this schema
// with no statistics — nothing in the engine runs ANALYZE, so neither does the
// plan fixture, and TestEveryCodeIndexServesARegisteredQuery holds every
// statement here to the exact seek it is written for.
//
// A PRIMARY KEY THAT SHARES ITS LEADING COLUMN WITH ANOTHER INDEX IS NOT
// CHOSEN FOR ITS WHOLE KEY. code_refs' key (repo_id, ref) and
// code_refs_changed_idx (repo_id, position, ref) both lead with repo_id, and a
// lookup of one ref sought code_refs_changed_idx on repo_id alone — every ref
// the repository holds, through the heap: 3 000 such lookups against a
// 100 000-ref repository took 29.8 s. code_packs' key and code_packs_added_idx
// are the same pair. So a statement that compares a key past its first column
// NAMES the key's index ([refsKey], [packsKey]), and the planner then seeks the
// whole of it — internal/worklet's field filter does the same, for the same
// reason. The name is the driver's own for a PRIMARY KEY's index, and naming it
// is also a GUARD: a schema change that renames it fails every such statement
// with `no such index` at its first run, rather than leaving a push apply
// quietly a thousand times slower.
//
// AN OR'D KEYSET COMPARISON SEEKS NOTHING PAST THE EQUALITY. A page after
// `(position > ? OR (position = ? AND ref > ?))` sought repo_id alone and
// walked the repository from its first row on every page, so each keyset page
// also states the bound its first ordered column can seek (`position >= ?`),
// and the OR only sorts out the rows at the cursor itself.
//
// A COLUMN OF json_each IS NOT A SEEK KEY, BUT AN EXPRESSION OVER ONE IS.
// `push_id IN (SELECT value FROM json_each(?))` scanned every push the company
// holds, and a join on `r.ref = j.value` sought repo_id alone; `CAST(value AS
// TEXT)` — which is what every key here already is — seeks each one. And an IN
// list never seeks a key's SECOND column, bound or derived: a batch of refs or
// packs is DRIVEN from its list (`json_each … CROSS JOIN`, which fixes the
// order), one seek of the whole key per element, and a batch deletion deletes
// the rowids that join found.
//
// A RANGE THAT STOPS AT ITS FIRST LIVE REF READS AN INDEX OF LIVE REFS. Over
// [refsKey], the test of the target is a filter on each row the range walks,
// so a range holding tombstones stepped over every one of them, a lookup of
// its row each, before it found a live ref or ran out — the push decide's
// probe under a create cost 7.7–9.9 s a round where a repository held its
// [MaxTombstonesPerRepo] under a deep chain of creates. Such a statement
// names [liveRefsKey] instead, whose rows are the live refs alone, and repeats
// its predicate verbatim so the planner may use it at all.
//
// A PARTIAL INDEX'S IN-LIST IS MATCHED AS WRITTEN, NOT AS A SET. The two
// indexes partial on a change's live states are used only by a statement that
// repeats [liveChanges] — the states in its order: `state IN ('open', 'draft')`
// sought code_changes_state_idx instead, and `state = 'open'`, which implies
// the predicate, sought neither (measured on the pinned driver). So every
// statement over them is written with the constant rather than with a
// spelling of its own.
//
// TWO ORDERED LOOPS SORT. An ORDER BY over a join that names the inner table's
// key after the outer's (`c.number, r.n`) is sorted whole, every row the join
// finds before its LIMIT takes one — the outer index is not UNIQUE, and the
// schema refuses one that is — while the outer's order alone is read in index
// order. So a bounded walk over a repository's changes orders by the change
// and takes each change whole ([purgeChanges]).
const (
	// refsKey is code_refs' PRIMARY KEY index, (repo_id, ref).
	refsKey = "sqlite_autoindex_code_refs_1"
	// liveRefsKey is code_refs_live_idx, (repo_id, ref) WHERE target != ''
	// (replicated migration 0077): the live refs alone, in key order.
	liveRefsKey = "code_refs_live_idx"
	// packsKey is code_packs' PRIMARY KEY index, (repo_id, pack_id, part).
	packsKey = "sqlite_autoindex_code_packs_1"
	// changesNumberKey is code_changes_number_idx, (repo_id, number)
	// (replicated migrations 0081 and 0089): a repository's changes in number
	// order, which is the only way to a repository's revisions, authors and
	// bodies — they carry no repo_id of their own.
	changesNumberKey = "code_changes_number_idx"
	// liveChanges is the predicate code_changes_open_idx and
	// code_changes_target_idx are partial on: a change in a [ChangeState.Live]
	// state, spelled in [ChangeStates]' order.
	liveChanges = `state IN ('draft', 'open')`
	// retiredKey is code_packs_retired's PRIMARY KEY index, (repo_id,
	// pack_id, part), which shares its leading column with
	// code_packs_retired_repo_idx as packsKey does with code_packs_added_idx.
	retiredKey = "sqlite_autoindex_code_packs_retired_1"
	// dropsKey is code_ref_drops' PRIMARY KEY index, (repo_id, at, position,
	// ref, old) (replicated migration 0084): a repository's ref drops by the
	// instant their records landed, the one order every statement over them
	// reads — the push apply's expiry, a purge part's and the final purge's
	// removal, and the purge's bounded count. It is the table's only index,
	// so nothing shares its leading column; it is named for the guard, as
	// every key index here is.
	dropsKey = "sqlite_autoindex_code_ref_drops_1"
)
