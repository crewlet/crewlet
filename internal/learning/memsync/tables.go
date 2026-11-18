package memsync

import "slices"

// The tables a seat's memory is made of, and how each one travels.
//
// ONE REGISTRY: replication reads the tables rather than hooking each of the
// many writers (the reflect worker, the persist decider, the skill
// synthesizer and refiner, the counterparty profiler, the onboarding claim),
// every one of which would have to remember the hook. A new memory table is
// one entry here; TestEveryAgentKeyedTableTravels fails for one without it.

// table describes how one memory table is selected, keyed and carried.
type table struct {
	// name is the table.
	name string

	// seatCol names the seat: a handle, or the derived agent id. Both are
	// stable across nodes.
	seatCol string

	// byAgentID says seatCol holds the derived UUIDv5, not the handle. It is a
	// pure function of (org name, handle), so a row written on one node is
	// addressable from another.
	byAgentID bool

	// key is the NATURAL key: the columns identifying the row the same way on
	// every node, which the replication subject is built from and an import
	// conflicts on. Never a synthetic id: an AUTOINCREMENT `id` starts at 1 on
	// every node, so two nodes' unrelated rows would collide.
	key []string

	// columns is everything carried; the synthetic id, where one exists, is
	// deliberately absent (see key).
	columns []string

	// blobs names the byte columns, so a JSON round trip restores them as
	// bytes. Only embeddings are; got wrong, a vector is stored as its base64
	// text and recall silently breaks for that row.
	blobs []string

	// wholeEachCycle republishes every row rather than only what is new.
	// THE SPLIT IS BY MUTABILITY: a watermark misses in-place updates, which
	// is right for the append-only tables (a retrieval counter is bookkeeping,
	// a compaction flag the lifecycle re-derives) and wrong where an update IS
	// the content: a profile rewritten, a skill's state and use count, an
	// onboarding marker flipping.
	//
	// It costs each table whole, every publish: profiles return after a sweep
	// since deletes do not travel, and archived skills are never deleted (the
	// learning.DefaultMaxSkillsPerAgent cap counts only live ones). A cut
	// inside one keeps nothing of it, hence [ownPassOrder].
	wholeEachCycle bool

	// watermark is the column an append-only table's incremental export
	// pages over, per seat, table and range ([Syncer.PublishOwn],
	// [Syncer.PublishBacklog]), past the last row the stream acknowledged;
	// empty for a wholeEachCycle table.
	//
	// NEVER REUSED (a rowid is, once its newest row is deleted, and a reuse at
	// or below a passed mark is never published) AND UNIQUE ABOVE 0 within a
	// table (0, the default, no read returns; the mark moves per row, so a
	// cut between two rows of one value skips the second): `change_seq`,
	// stamped by trigger from an only-increasing counter on insert and vector
	// set (node migration 0041). Held by TestNoAppendOnlyTableReusesItsWatermark
	// and TestRowsWrittenTogetherCarryDistinctWatermarks.
	watermark string

	// fillsVector says the holder FILLS this append-only table's vector (its
	// `embedding` and `embedding_model`) after the insert, for rows written
	// without one or under a model the company has since left; the vector is
	// of content stored on the row, so the holder can make it again.
	//
	// Both halves of carrying that in-place change are deliberate: setting
	// the vector stamps a fresh change sequence, so the watermark carries it
	// like an insert (learning.Diary.FillEmbeddings); and a carry over a row
	// already here takes the carried vector when the stored one is of no
	// model or another ([table.onConflict]), since with DO NOTHING a peer
	// holding the vectorless row would keep it and hand it on as holder.
	fillsVector bool

	// parent is the carried table a row of this one belongs to (a skill
	// version's skill_id names a synthesized_skills id); nil for none.
	//
	// A hydration replays the parent's table first ([tables]) and LEAVES OUT
	// a row whose parent is still not in the store ([table.parentHeld]),
	// counted as an orphan: written, its foreign key refused the seat on every node
	// lacking the parent, over a row nothing reads alone. The parent is
	// missing after a publish cut between version and skill ([ownPassOrder]),
	// or when this store skipped it for another skill of that name on the
	// (agent_handle, name) index ([table.onConflict]).
	// TestEveryReferenceBetweenCarriedTablesNamesAParentReplayedFirst holds
	// every schema reference between carried tables to a parent listed first.
	parent *reference
}

// reference is a carried table's reference to another, as the schema
// declares it: the column holding the parent's key, the parent table, and the
// parent's key column.
type reference struct {
	column string
	table  string
	key    string
}

// retiredTables are the memory tables this build no longer carries, which a
// seat's changelog may still hold rows of: nothing republishes or replaces
// them, so a hydration that finds the seat's rows of one purges them, once,
// for every node ([Syncer.settleNotCarried]). A table a NEWER build carries is
// not here: no hydration of this build reads it, and its rows stay, since that
// build still reads them.
var retiredTables = []string{
	// Moved to the executor log, where a conversation's sessions are kept
	// beside the turns that ran them (ADR-0073).
	"conversation_sessions",
}

// tables is every table a seat's memory lives in, in the order a hydration
// replays them; a publish's own pass reorders them ([ownPassOrder]) and its
// backlog carries only the watermark tables ([backlogPassOrder]).
//
// PARENTS BEFORE CHILDREN: synthesized_skill_versions references
// synthesized_skills with foreign keys ON, and the changelog keeps no order
// across subjects (ADR-0014), so a hydration replays each table on its own in
// this order ([Syncer.Hydrate]) and every skill is in the store before its
// versions. A child names its parent ([table.parent]).
var tables = []table{
	{
		name:      "agent_diary",
		seatCol:   "agent_id",
		byAgentID: true,
		key:       []string{"id"},
		columns: []string{
			"id", "agent_id", "kind", "content", "ttl_until", "source",
			"turn_id", "metadata", "retrieval_count", "last_retrieved_at",
			"embedding", "embedding_model", "created_at", "provenance", "engine_build",
		},
		blobs:       []string{"embedding"},
		watermark:   "change_seq",
		fillsVector: true,
	},
	{
		name:    "episodes",
		seatCol: "agent_handle",
		key:     []string{"id"},
		columns: []string{
			"id", "agent_handle", "agent_role", "work_item", "turn_id",
			"started_at", "ended_at", "plan_summary", "task_summary", "ask",
			"tool_sequence", "skills_used", "review_outcome", "duration_ms",
			"embedding", "embedding_model", "kind", "count", "exemplar_turn_ids",
			"consolidated_into_skill_id", "common_task_pattern",
			"common_outcome", "success_rate", "subjects_involved",
			"notable_patterns", "work_key", "conversation_key", "ask_provenance",
		},
		blobs:       []string{"embedding"},
		watermark:   "change_seq",
		fillsVector: true,
	},
	{
		name:    "counterparty_profiles",
		seatCol: "observer_handle",
		key: []string{
			"observer_handle", "subject_handle",
			"subject_external_id", "subject_platform",
		},
		columns: []string{
			"observer_handle", "subject_handle", "subject_external_id",
			"subject_platform", "subject_name", "traits", "first_seen_at",
			"last_updated_at", "last_corroborated_at", "interaction_count",
			"last_work_key",
		},
		wholeEachCycle: true,
	},
	{
		name:    "synthesized_skills",
		seatCol: "agent_handle",
		key:     []string{"id"},
		columns: []string{
			"id", "agent_handle", "name", "description", "content",
			"frontmatter", "tool_sequence", "source_episode_ids", "version",
			"created_at", "updated_at", "use_count", "last_used_at", "state",
			"pinned", "stale_at", "archived_at", "load_count",
		},
		wholeEachCycle: true,
	},
	{
		name:    "synthesized_skill_versions",
		seatCol: "agent_handle",
		key:     []string{"id"},
		columns: []string{
			"id", "skill_id", "agent_handle", "name", "description", "content",
			"frontmatter", "tool_sequence", "source_episode_ids", "version",
			"refinement_kind", "refinement_note", "archived_at",
		},
		watermark: "change_seq",
		parent:    &reference{column: "skill_id", table: "synthesized_skills", key: "id"},
	},
	{
		name:      "agent_onboarding_markers",
		seatCol:   "agent_id",
		byAgentID: true,
		key:       []string{"agent_id"},
		columns: []string{
			"agent_id", "chain_hash", "agent_handle", "role", "summary",
			"created_at", "updated_at", "in_progress_until",
		},
		wholeEachCycle: true,
	},
}

// ownPassOrder is the order a publish's own pass carries the tables in
// ([Syncer.PublishOwn]): the watermark tables (diary, episodes, skill
// versions), then the tables sent whole (profiles, skills, onboarding
// marker), each half in the registry's order.
//
// WHAT THE HOLDING WROTE GOES FIRST, because a cut should lose it last: a
// watermark table carries a few rows a cycle while the whole tables are
// unbounded, so a pass is far likelier cut inside those, and a seat whose
// profiles and skills outlast a cycle would never carry what sat behind them.
// A version thus goes ahead of its skill; a cut between them leaves the
// version with the skill's older copy or none, which a hydration tolerates
// by leaving it out until the skill arrives ([table.parent]).
var ownPassOrder = slices.Concat(
	slices.DeleteFunc(slices.Clone(tables), func(t table) bool { return t.wholeEachCycle }),
	slices.DeleteFunc(slices.Clone(tables), func(t table) bool { return !t.wholeEachCycle }))

// backlogPassOrder is the order a publish's backlog carries the tables in
// ([Syncer.PublishBacklog]): the tables with a watermark, in the registry's
// order. The tables sent whole are not in it: the own pass sends them.
var backlogPassOrder = slices.DeleteFunc(slices.Clone(tables), func(t table) bool { return t.wholeEachCycle })

// isBlob reports whether a column carries bytes.
func (t table) isBlob(column string) bool {
	return slices.Contains(t.blobs, column)
}

// isKey reports whether a column is part of the natural key.
func (t table) isKey(column string) bool {
	return slices.Contains(t.key, column)
}
