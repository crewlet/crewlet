package memsync

import "slices"

// The tables a seat's memory is made of, and how each one travels.
//
// ONE REGISTRY, and it is the whole reason this package is not fifteen
// publish calls scattered across the learning subsystem. Memory is written
// from many places — the reflect worker, the persist decider, the skill
// synthesizer and refiner, the counterparty profiler, the onboarding claim —
// and a scheme that hooked each write site would need every one of them to
// remember. The diary's own retention sweep proves
// how that ends: it shipped, was documented, and for months nothing called
// it.
//
// So replication reads the tables instead of intercepting the writers. A new
// memory table is one entry here, and the day somebody adds one without an
// entry, TestEveryAgentKeyedTableTravels fails.

// table describes how one memory table is selected, keyed and carried.
type table struct {
	// name is the table.
	name string

	// seatCol names the seat. Two spellings exist in this schema and both
	// are stable across nodes: a handle, or the derived agent id.
	seatCol string

	// byAgentID says seatCol holds the derived UUIDv5 rather than the
	// handle. The derivation is a pure function of (org name, handle), so
	// every node computes the same value — which is what makes a row
	// written on one node addressable from another.
	byAgentID bool

	// key is the NATURAL key: the columns that identify this row the same
	// way on every node. It is what the replication subject is built from
	// and what an import conflicts on.
	//
	// Never a synthetic id. An AUTOINCREMENT `id` starts at 1 on every
	// node, so replicating one would collide two unrelated rows the moment
	// two nodes had both written one.
	key []string

	// columns is everything carried. The synthetic id is deliberately
	// absent where one exists — see key.
	columns []string

	// blobs names the columns whose values are bytes rather than text, so
	// a round trip through JSON restores them as bytes. Only embeddings
	// are, and getting it wrong would store a vector as its base64 text
	// and silently break recall for that row.
	blobs []string

	// wholeEachCycle republishes every row rather than only what is new.
	//
	// THE SPLIT IS BY SIZE AND BY MUTABILITY. A watermark ([table.watermark])
	// catches inserts and misses in-place updates, which is right for the
	// big append-only tables — a diary entry's retrieval counter moving is
	// bookkeeping, and a compaction flag that fails to travel self-heals
	// because the lifecycle re-derives it. It is wrong for the small ones,
	// where an update IS the content: a counterparty profile is rewritten
	// as the seat learns, a skill's state and use count move, an
	// onboarding marker flips. Those hold a handful of rows per seat, so
	// carrying all of them every cycle costs nothing and misses nothing.
	//
	// "A handful" is a claim about each table's BOUND, so each has one.
	// synthesized_skills is capped per agent by the curator's archive
	// horizon; agent_onboarding_markers is one row per seat by
	// construction; counterparty_profiles is one row per person a seat has
	// messaged and is swept at maintenance.CounterpartyRetention — it had
	// no bound at all, which made it the one table here whose cost grew
	// with the deployment's age rather than with its size.
	wholeEachCycle bool

	// watermark is the column an append-only table's incremental export
	// is over: a seat's rows past the highest value it published last
	// cycle, in that column's order. Empty for a wholeEachCycle table,
	// which has none.
	//
	// IT MUST NEVER BE REUSED, which is the whole of the requirement and
	// what the rowid failed. A TEXT-keyed table with no AUTOINCREMENT
	// hands out max(rowid) + 1, so after the newest row is deleted — the
	// diary's expiry or its trim, a lifecycle sweep, a skill's pruned
	// history — the next insert takes that rowid again, at or below a
	// mark that has already passed it, and is never published while this
	// node holds the seat. So those tables carry `change_seq`, stamped by
	// a trigger from a counter that only ever increments, on every insert
	// and on every vector set in place (node migration 0041).
	// TestNoAppendOnlyTableReusesItsWatermark holds every append-only entry
	// to that, so an entry added later cannot bring the reusable rowid
	// back.
	watermark string

	// fillsVector says the holder FILLS this append-only table's vector
	// after the insert — its `embedding` and the `embedding_model` it came
	// from — for rows written without one or under a model the company
	// has since left.
	//
	// The one in-place change an append-only row takes, and both halves
	// of carrying it are deliberate. Setting the vector stamps the row
	// with a fresh change sequence (the table's trigger, node migration
	// 0041), so the watermark carries it like an insert
	// (learning.Diary.FillEmbeddings); and a carry over a row
	// already here takes the carried vector WHEN THE STORED ONE IS OF NO
	// MODEL OR ANOTHER — see [table.onConflict] — because with DO NOTHING
	// a peer that already held the vectorless row would keep it, and when
	// the seat moved there that stale copy would be the holder's.
	//
	// The diary and the episodes: a note's vector is of its content, and an
	// episode's of its label, its ask and what it did — all stored on the
	// row — so the holder can make either again.
	fillsVector bool
}

// retiredTables are the memory tables this build no longer carries, which a
// seat's changelog may still hold rows of: nothing republishes or replaces
// them, so a hydration that meets one purges the seat's rows of it, once, for
// every node ([Syncer.Hydrate]). A table a NEWER build carries is not here: its
// rows are skipped, never purged, since that build still reads them.
var retiredTables = []string{
	// Moved to the executor log, where a conversation's sessions are kept
	// beside the turns that ran them (ADR-0073).
	"conversation_sessions",
}

// tables is every table a seat's memory lives in.
//
// Ordered so a hydrating node writes parents before children:
// synthesized_skill_versions references synthesized_skills, and foreign keys
// are ON in this store.
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

// isBlob reports whether a column carries bytes.
func (t table) isBlob(column string) bool {
	return slices.Contains(t.blobs, column)
}

// isKey reports whether a column is part of the natural key.
func (t table) isKey(column string) bool {
	return slices.Contains(t.key, column)
}
