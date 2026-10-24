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
// whole of it — internal/tracker's field filter does the same, for the same
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
const (
	// refsKey is code_refs' PRIMARY KEY index, (repo_id, ref).
	refsKey = "sqlite_autoindex_code_refs_1"
	// liveRefsKey is code_refs_live_idx, (repo_id, ref) WHERE target != ''
	// (replicated migration 0052): the live refs alone, in key order.
	liveRefsKey = "code_refs_live_idx"
	// packsKey is code_packs' PRIMARY KEY index, (repo_id, pack_id, part).
	packsKey = "sqlite_autoindex_code_packs_1"
)
