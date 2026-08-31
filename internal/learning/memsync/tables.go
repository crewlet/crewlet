package memsync

// The tables a seat's memory is made of, and how each one travels.
//
// ONE REGISTRY, and it is the whole reason this package is not fifteen
// publish calls scattered across the learning subsystem. Memory is written
// from many places — the reflect worker, the persist decider, the skill
// synthesizer and refiner, the counterparty profiler, the onboarding claim,
// the conversation ledger — and a scheme that hooked each write site would
// need every one of them to remember. The diary's own retention sweep proves
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
	// Never a synthetic id. conversation_sessions carries an AUTOINCREMENT
	// `id` that starts at 1 on every node, so replicating it would collide
	// two unrelated rows the moment two nodes had both written one.
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
	// THE SPLIT IS BY SIZE AND BY MUTABILITY. A watermark over the rowid
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
			"embedding", "created_at",
		},
		blobs: []string{"embedding"},
	},
	{
		name:    "episodes",
		seatCol: "agent_handle",
		key:     []string{"id"},
		columns: []string{
			"id", "agent_handle", "agent_role", "task_id", "turn_id",
			"started_at", "ended_at", "plan_summary", "task_summary",
			"tool_sequence", "skills_used", "review_outcome", "duration_ms",
			"embedding", "kind", "count", "exemplar_turn_ids",
			"consolidated_into_skill_id", "common_task_pattern",
			"common_outcome", "success_rate", "subjects_involved",
			"notable_patterns", "work_key", "conversation_key",
		},
		blobs: []string{"embedding"},
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
			"pinned", "stale_at", "archived_at",
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
	{
		name:    "conversation_sessions",
		seatCol: "agent_handle",
		// NOT `id`: it is an AUTOINCREMENT that starts at 1 on every
		// node, so replicating it would collide two unrelated entries
		// the moment two nodes had both written one. entry_id is the
		// name the writing node minted for the row — see schema/0017.
		//
		// And NOT (agent_handle, conversation_key, work_key, turn_id)
		// either, which reads like the natural key and is not one: both
		// work_key and turn_id are '' for a turn with no ledgerable
		// trigger, so that quadruple collapses every unkeyed turn in a
		// conversation onto one row. The table's own dedupe index is
		// PARTIAL over `work_key <> ''` for exactly that reason.
		key: []string{"entry_id"},
		columns: []string{
			"entry_id", "agent_handle", "conversation_key", "work_key",
			"turn_id", "entry", "created_at",
		},
	},
}

// isBlob reports whether a column carries bytes.
func (t table) isBlob(column string) bool {
	for _, name := range t.blobs {
		if name == column {
			return true
		}
	}
	return false
}

// isKey reports whether a column is part of the natural key.
func (t table) isKey(column string) bool {
	for _, name := range t.key {
		if name == column {
			return true
		}
	}
	return false
}
