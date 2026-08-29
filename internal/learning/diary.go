package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// DiaryKind is how long an observation is meant to live.
type DiaryKind string

const (
	// DiaryLong is a durable observation with no expiry.
	DiaryLong DiaryKind = "diary_long"
	// DiaryShort is an observation with a deadline — something true for
	// this sprint, this incident, this quarter.
	DiaryShort DiaryKind = "diary_short"
)

// DiaryEntry is one private observation a seat made about its own work.
//
// PRIVATE is the operative word, and the reason the table is called a diary
// rather than a memory: it is the seat's own log, not knowledge other seats
// can query. Every read is scoped to one agent id, and there is no cross-agent
// surface at all.
type DiaryEntry struct {
	ID string

	// AgentID is the DERIVED uuid (org name plus handle), not the handle.
	// Renaming a handle then cleanly orphans the old rows rather than
	// mixing them into the new identity's memory.
	AgentID string

	Kind    DiaryKind
	Content string

	// TTLUntil is the deadline for a short entry, zero for a long one.
	TTLUntil time.Time

	Source   string
	TurnID   string
	Metadata map[string]any

	// RetrievalCount and LastRetrievedAt are how often the entry has been
	// recalled. They are what lets a compaction pass tell a memory that
	// keeps proving useful from one written once and never read.
	RetrievalCount  int
	LastRetrievedAt time.Time

	Embedding []float32
	CreatedAt time.Time
}

// Expired reports whether a short entry's deadline has passed.
func (d DiaryEntry) Expired(now time.Time) bool {
	return !d.TTLUntil.IsZero() && !now.Before(d.TTLUntil)
}

// Diary is a seat's private observation log.
type Diary struct{ db *store.DB }

// NewDiary wraps a database handle.
func NewDiary(db *store.DB) *Diary { return &Diary{db: db} }

// Write records one observation.
func (d *Diary) Write(ctx context.Context, e DiaryEntry) error {
	switch {
	case e.ID == "" || e.AgentID == "":
		return fmt.Errorf("learning: a diary entry needs an id and an agent")
	case e.Kind != DiaryLong && e.Kind != DiaryShort:
		// The column has a CHECK constraint, so an unknown kind would fail
		// at the driver with a message naming the constraint and not the
		// caller. Refusing here says which field and which values.
		return fmt.Errorf("learning: diary kind must be %q or %q, got %q",
			DiaryLong, DiaryShort, e.Kind)
	case e.Kind == DiaryShort && e.TTLUntil.IsZero():
		// A short entry with no deadline never expires, which makes it a
		// long entry wearing the wrong label — and one the expiry sweep
		// will never look at, because that scan is indexed on a non-NULL
		// ttl.
		return fmt.Errorf("learning: a %q entry needs a deadline", DiaryShort)
	case e.Kind == DiaryLong && !e.TTLUntil.IsZero():
		return fmt.Errorf("learning: a %q entry must not carry a deadline", DiaryLong)
	}

	var blob any
	if len(e.Embedding) > 0 {
		packed, err := d.db.EncodeVector(e.Embedding)
		if err != nil {
			return fmt.Errorf("learning: encode diary embedding: %w", err)
		}
		blob = packed
	}
	if _, err := d.db.SQL().ExecContext(ctx, `
		INSERT INTO agent_diary (id, agent_id, kind, content, ttl_until, source,
			turn_id, metadata, retrieval_count, last_retrieved_at, embedding, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, NULL, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		e.ID, e.AgentID, string(e.Kind), e.Content, store.NullTime(e.TTLUntil),
		e.Source, e.TurnID, jsonObject(e.Metadata), blob, store.EncodeTime(e.CreatedAt),
	); err != nil {
		return fmt.Errorf("learning: write diary entry %s: %w", e.ID, err)
	}
	return nil
}

const diaryColumns = `id, agent_id, kind, content, ttl_until, source, turn_id,
	metadata, retrieval_count, last_retrieved_at, embedding, created_at`

func scanDiary(rows interface{ Scan(...any) error }) (DiaryEntry, error) {
	var (
		e                  DiaryEntry
		kind, metadata     string
		created            int64
		ttl, lastRetrieved sql.NullInt64
		embedding          []byte
	)
	if err := rows.Scan(&e.ID, &e.AgentID, &kind, &e.Content, &ttl, &e.Source,
		&e.TurnID, &metadata, &e.RetrievalCount, &lastRetrieved, &embedding, &created,
	); err != nil {
		return DiaryEntry{}, err
	}
	e.Kind = DiaryKind(kind)
	e.TTLUntil = store.TimeAt(ttl)
	e.LastRetrievedAt = store.TimeAt(lastRetrieved)
	e.CreatedAt = store.DecodeTime(created)
	if err := json.Unmarshal([]byte(metadata), &e.Metadata); err != nil {
		// The observation itself is what the seat needs; its metadata is
		// provenance. Losing the second must not cost the first.
		log.Warn("diary_metadata_undecodable", "entry", e.ID, "error", err)
	}
	if len(embedding) > 0 {
		if vec, err := store.DecodeVector(embedding); err == nil {
			e.Embedding = vec
		} else {
			log.Warn("diary_embedding_undecodable", "entry", e.ID, "error", err)
		}
	}
	return e, nil
}

// Recent returns a seat's most recent LIVE entries, newest first.
//
// Live means unexpired. An expired short entry is filtered on READ as well as
// swept in the background, because the sweep runs on a timer and a memory that
// has just passed its deadline is exactly as wrong as one that passed it a
// week ago.
func (d *Diary) Recent(ctx context.Context, agentID string, now time.Time, limit int) ([]DiaryEntry, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.db.SQL().QueryContext(ctx,
		`SELECT `+diaryColumns+` FROM agent_diary
		 WHERE agent_id = ? AND (ttl_until IS NULL OR ttl_until > ?)
		 ORDER BY created_at DESC, id DESC LIMIT ?`,
		agentID, store.EncodeTime(now), limit)
	if err != nil {
		return nil, fmt.Errorf("learning: recent diary for %s: %w", agentID, err)
	}
	return collectDiary(rows)
}

// Recall returns a seat's most similar live entries.
//
// Same shape as episode recall and for the same reasons — a scan the database
// ranks, a relevance floor, a total order, a Go re-score over the rows that
// survive — but scoped to one agent id, which is what makes the diary private.
// See [Episodes.Recall] for why each of those is the way it is; the only thing
// this adds is the liveness predicate.
//
// Live means unexpired, applied here as well as by the background sweep,
// because the sweep runs on a timer and a memory that has just passed its
// deadline is exactly as wrong as one that passed it a week ago.
func (d *Diary) Recall(ctx context.Context, agentID string, q RecallQuery, now time.Time) ([]DiaryHit, error) {
	if agentID == "" {
		return nil, fmt.Errorf("learning: diary recall needs an agent")
	}
	if len(q.Embedding) == 0 {
		return nil, ErrNoEmbedding
	}
	limit, floor := q.Limit, q.MinSimilarity
	if limit <= 0 {
		limit = defaultRecallLimit
	}
	if floor == 0 {
		floor = defaultMinSimilarity
	}
	probe, width, err := vectorProbe(d.db, q.Embedding)
	if err != nil {
		return nil, fmt.Errorf("learning: diary recall for %s: %w", agentID, err)
	}
	rows, err := d.db.SQL().QueryContext(ctx,
		`SELECT `+diaryColumns+` FROM (
		    SELECT `+diaryColumns+`,
		           vector_distance_cos(embedding, ?) AS distance
		    FROM agent_diary
		    WHERE agent_id = ?
		      AND embedding IS NOT NULL
		      AND length(embedding) = ?
		      AND (ttl_until IS NULL OR ttl_until > ?)
		 )
		 WHERE distance <= ?
		 ORDER BY distance ASC, created_at DESC, id DESC
		 LIMIT ?`,
		probe, agentID, width, store.EncodeTime(now), 1-floor, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: diary recall for %s: %w", agentID, err)
	}
	entries, err := collectDiary(rows)
	if err != nil {
		return nil, err
	}
	var hits []DiaryHit
	for _, e := range entries {
		sim, ok := cosine(q.Embedding, e.Embedding)
		if !ok || sim < floor {
			continue
		}
		hits = append(hits, DiaryHit{Entry: e, Similarity: sim})
	}
	rankDiary(hits)
	return hits, nil
}

// DiaryHit is one recalled observation with its similarity.
type DiaryHit struct {
	Entry      DiaryEntry
	Similarity float64
}

// rankDiary orders hits most-similar first, then newest, then by id — the
// same total order episode recall uses, for the same reason.
func rankDiary(hits []DiaryHit) {
	slices.SortFunc(hits, func(a, b DiaryHit) int {
		switch {
		case a.Similarity > b.Similarity:
			return -1
		case a.Similarity < b.Similarity:
			return 1
		case a.Entry.CreatedAt.After(b.Entry.CreatedAt):
			return -1
		case a.Entry.CreatedAt.Before(b.Entry.CreatedAt):
			return 1
		default:
			return -1 * compareStrings(a.Entry.ID, b.Entry.ID)
		}
	})
}

// MarkRetrieved records that entries were recalled.
//
// Best effort and deliberately not part of Recall's return path: a seat that
// recalled a memory has had the benefit whether or not the counter moved, and
// failing a recall because its bookkeeping failed would trade the useful half
// of the operation for the statistical half.
func (d *Diary) MarkRetrieved(ctx context.Context, ids []string, at time.Time) {
	for _, id := range ids {
		if _, err := d.db.SQL().ExecContext(ctx,
			`UPDATE agent_diary SET retrieval_count = retrieval_count + 1,
			 last_retrieved_at = ? WHERE id = ?`, store.EncodeTime(at), id); err != nil {
			log.Warn("diary_retrieval_not_recorded", "entry", id, "error", err)
			return
		}
	}
}

// Expire deletes short entries whose deadline has passed.
func (d *Diary) Expire(ctx context.Context, now time.Time) (int64, error) {
	res, err := d.db.SQL().ExecContext(ctx,
		`DELETE FROM agent_diary WHERE ttl_until IS NOT NULL AND ttl_until <= ?`,
		store.EncodeTime(now))
	if err != nil {
		return 0, fmt.Errorf("learning: expire diary: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("learning: expire diary: %w", err)
	}
	return n, nil
}

func collectDiary(rows *sql.Rows) ([]DiaryEntry, error) {
	defer rows.Close()
	var out []DiaryEntry
	for rows.Next() {
		e, err := scanDiary(rows)
		if err != nil {
			return nil, fmt.Errorf("learning: scan diary entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: read diary: %w", err)
	}
	return out, nil
}

// jsonObject renders a map as a JSON object, never as null.
//
// Same reason as jsonList: the column is NOT NULL with a '{}' default, and a
// nil map marshals to "null", which then fails every JSON query against it.
func jsonObject(v map[string]any) string {
	if len(v) == 0 {
		return "{}"
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(blob)
}
