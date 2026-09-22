// Package learning is the agent-learning subsystem: what a seat remembers
// about its own work, who it worked with, and what it has taught itself.
//
// Everything here is BEST EFFORT by design. A seat whose memory is
// unreachable is a seat with less context, never a seat that cannot work —
// so a failed write is logged and a failed read answers empty. The one
// exception is stated at the call site that makes it.
package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/store"
)

var log = logging.Get("learning")

// Kind tells the two row shapes in the episode table apart.
type Kind string

const (
	// KindRaw is one completed turn.
	KindRaw Kind = "raw"
	// KindCompacted is a cluster summary the lifecycle worker folds raw
	// rows into. Count is how many it replaces.
	KindCompacted Kind = "compacted"
)

// Episode is one completed agent turn, or one compacted cluster.
type Episode struct {
	ID        string
	Handle    string
	Role      string
	TaskID    string
	TurnID    string
	StartedAt time.Time
	EndedAt   time.Time

	PlanSummary   string
	TaskSummary   string
	ToolSequence  []string
	SkillsUsed    []string
	ReviewOutcome string
	Duration      time.Duration

	// Embeddings are the task summary's vectors, one per WINDOW of it, in
	// the order the windows were read — or nil when the embeddings provider
	// was unreachable at write time.
	//
	// SEVERAL, because a summary is not bounded and one vector represents
	// one subject: the episodist splits a long summary into overlapping
	// windows and embeds every one, so nothing is cut and every byte reaches
	// some vector. [Episodist.Reflect] fills it; see [EpisodeWindowBytes]
	// for the window and node migration 0030 for how the set is stored.
	//
	// A short summary — which is almost every one — is exactly one window,
	// so this holds a single vector and the row is byte-for-byte what it was
	// before windowing existed.
	//
	// Recall scores an episode as its NEAREST window (see [Episodes.Recall]),
	// never as a mean: a turn is worth recalling because one part of what it
	// did matches, not because all of it does.
	//
	// THE ORDINAL IS NOT AN ADDRESS. Nothing maps a window back to the slice
	// of text it came from, so a window the store refuses (a non-finite
	// vector) is dropped rather than held as a gap, and the rest still rank.
	//
	// Nil is a supported state, not a failure: recall skips such rows while
	// the time-window and outcome queries still surface them. A transient
	// outage must never cost an episode.
	Embeddings [][]float32

	Kind  Kind
	Count int

	// WorkKey is the identity of the work this turn did. Empty when the
	// turn had no ledgerable trigger — a scheduled fire, a sub-agent, a
	// sandbox resume.
	WorkKey string

	// ConversationKey is which conversation the turn served. Empty for
	// triggers with no derivable conversation, and always empty on a
	// compacted row, whose cluster spans conversations by construction.
	ConversationKey string

	// The compacted-row fields. All zero on a raw row.
	ExemplarTurnIDs   []string
	ConsolidatedInto  string
	CommonTaskPattern string
	CommonOutcome     string
	SuccessRate       float64
	SubjectsInvolved  []string
	NotablePatterns   string
}

// Episodes is the durable episode memory.
type Episodes struct{ db *store.DB }

// NewEpisodes wraps a database handle.
func NewEpisodes(db *store.DB) *Episodes { return &Episodes{db: db} }

const episodeInsertSQL = `
INSERT INTO episodes (
	id, agent_handle, agent_role, task_id, turn_id, started_at, ended_at,
	plan_summary, task_summary, tool_sequence, skills_used, review_outcome,
	duration_ms, embedding, embedding_windows, kind, count, exemplar_turn_ids,
	consolidated_into_skill_id, common_task_pattern, common_outcome,
	success_rate, subjects_involved, notable_patterns, work_key, conversation_key
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (agent_handle, work_key) DO NOTHING`

// episodeInsertArgs binds [episodeInsertSQL], once, for every writer of the
// table.
//
// ONE ARGUMENT LIST, and it is not tidiness: the statement is bound
// POSITIONALLY, so a second hand-written list is a second chance to pair a
// column with the wrong value — and the failure is silent wherever the two
// types agree, which for this table's fourteen TEXT columns is most of it. The
// lifecycle's compacted-row insert used to keep its own copy under a comment
// promising it matched; the promise is now the code.
//
// blob and windows come from [Episodes.encodeEmbedding] together, because they
// are one fact about one value: a blob of N packed vectors and a count that
// said anything but N would make recall read somebody else's bytes as a vector.
func episodeInsertArgs(ep Episode, blob any, windows int) []any {
	return []any{
		ep.ID, ep.Handle, ep.Role, ep.TaskID, ep.TurnID,
		store.EncodeTime(ep.StartedAt), store.EncodeTime(ep.EndedAt),
		ep.PlanSummary, ep.TaskSummary, jsonList(ep.ToolSequence), jsonList(ep.SkillsUsed),
		ep.ReviewOutcome, ep.Duration.Milliseconds(), blob, windows,
		string(ep.Kind), ep.Count, jsonList(ep.ExemplarTurnIDs),
		store.NullText(ep.ConsolidatedInto), ep.CommonTaskPattern, ep.CommonOutcome,
		ep.SuccessRate, jsonList(ep.SubjectsInvolved), ep.NotablePatterns,
		store.NullText(ep.WorkKey), store.NullText(ep.ConversationKey),
	}
}

// Append records one episode, at most once per (seat, work key).
//
// EXACTLY-ONCE ON THE WORK KEY, and it is a real unique index rather than a
// read-then-write: two writers racing inside one process cannot both see "not
// there" and both insert. A turn can legitimately be worked twice — a
// redelivery, or an honest re-run after the completion ledger fails open — and
// an episode keyed on nothing simply lands twice, then feeds every later
// recall and skill synthesis, weighting the agent's behaviour with an event
// that happened once.
//
// THE INDEX IS THIS NODE'S, like the table. That is the whole scope of the
// guarantee and it is the right one: episodes are a seat's memory, read by the
// node running that seat and never by a peer, so a duplicate written on two
// DIFFERENT nodes is two rows in two databases neither of which the other
// reads — no recall sees both. What is collapsed here is the duplicate one
// reader would otherwise be shown twice.
//
// An empty work key maps to SQL NULL, which the index treats as distinct from
// every other NULL — so an unkeyed turn is never deduped against another. That
// is the whole reason the column is nullable: ” would collide every unkeyed
// turn a seat ever ran onto one row.
//
// Reports whether the row was WRITTEN. False means a duplicate was collapsed,
// which is the guard working and not a failure.
func (e *Episodes) Append(ctx context.Context, ep Episode) (bool, error) {
	if ep.ID == "" || ep.Handle == "" {
		return false, fmt.Errorf("learning: an episode needs an id and a seat")
	}
	if ep.Kind == "" {
		ep.Kind = KindRaw
	}
	if ep.Count < 1 {
		ep.Count = 1
	}
	blob, windows, err := e.encodeEmbedding(ep.Embeddings)
	if err != nil {
		return false, err
	}
	res, err := e.db.SQL().ExecContext(ctx, episodeInsertSQL,
		episodeInsertArgs(ep, blob, windows)...)
	if err != nil {
		return false, fmt.Errorf("learning: append episode %s: %w", ep.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("learning: append episode %s: %w", ep.ID, err)
	}
	return n > 0, nil
}

// encodeEmbedding packs the window vectors END TO END and reports how many
// landed, refusing one of the wrong width and DEGRADING one that is not finite.
//
// The count is returned beside the bytes rather than derived from them, and
// node migration 0030 is where that argument is written down: the width is a
// runtime property, so `len(blob) / (4 * width)` can divide exactly under a
// model the row was not embedded with and hand recall a window count that is
// arithmetically clean and entirely wrong.
//
// The width is checked because the column is a plain BLOB: Turso does not
// enforce a declared vector width (measured), so a mismatched vector stores
// happily and then makes every distance query against it return nothing — a
// seat whose recall silently stops working, with no error anywhere. That is a
// configuration fault, and the write fails.
//
// A NaN or an infinity is a different thing and gets the opposite answer: that
// WINDOW is dropped and the rest are kept, exactly as if the provider had been
// unreachable for it. Failing the write instead would cost the episode, and an
// episode is the record of a turn that really happened — the one thing this
// subsystem may not lose to a bad response from an embeddings API. Storing it
// anyway is not an option either: [store.ErrVectorNotFinite] says why. Dropping
// one window rather than the whole set is what [Episode.Embeddings] means by
// the ordinal not being an address: nothing maps a vector back to its slice of
// text, so a shorter set still ranks the row on everything that did embed.
func (e *Episodes) encodeEmbedding(windows [][]float32) (any, int, error) {
	if len(windows) == 0 {
		return nil, 0, nil
	}
	var packed []byte
	kept := 0
	for at, window := range windows {
		if len(window) == 0 {
			continue
		}
		blob, err := e.db.EncodeVector(window)
		switch {
		case errors.Is(err, store.ErrVectorNotFinite):
			log.Warn("episode_embedding_window_discarded",
				"window", at, "windows", len(windows), "error", err.Error())
			continue
		case err != nil:
			// A WIDTH FAULT FAILS THE WRITE, because it is the same
			// fault for every window: the vectors came from one call
			// to one provider, so a second window would report the
			// same mismatch and the row must not land half-embedded
			// under a configuration nobody has noticed is wrong.
			return nil, 0, fmt.Errorf("learning: encode embedding window %d of %d: %w",
				at, len(windows), err)
		}
		packed = append(packed, blob...)
		kept++
	}
	if kept == 0 {
		// Every window was refused, which is the same outcome as an
		// unreachable provider: the row lands, recall skips it, and the
		// time-window queries still surface it.
		return nil, 0, nil
	}
	return packed, kept, nil
}

const episodeColumns = `id, agent_handle, agent_role, task_id, turn_id,
	started_at, ended_at, plan_summary, task_summary, tool_sequence,
	skills_used, review_outcome, duration_ms, embedding, embedding_windows,
	kind, count,
	exemplar_turn_ids, consolidated_into_skill_id, common_task_pattern,
	common_outcome, success_rate, subjects_involved, notable_patterns,
	work_key, conversation_key`

func scanEpisode(rows interface{ Scan(...any) error }) (Episode, error) {
	var (
		ep                                     Episode
		started, ended, durationMS             int64
		embedding                              []byte
		windows                                int
		kind                                   string
		toolSeq, skills, exemplars, subjects   string
		consolidated, workKey, conversationKey sql.NullString
	)
	if err := rows.Scan(
		&ep.ID, &ep.Handle, &ep.Role, &ep.TaskID, &ep.TurnID,
		&started, &ended, &ep.PlanSummary, &ep.TaskSummary, &toolSeq,
		&skills, &ep.ReviewOutcome, &durationMS, &embedding, &windows,
		&kind, &ep.Count,
		&exemplars, &consolidated, &ep.CommonTaskPattern,
		&ep.CommonOutcome, &ep.SuccessRate, &subjects, &ep.NotablePatterns,
		&workKey, &conversationKey,
	); err != nil {
		return Episode{}, err
	}
	ep.StartedAt = store.DecodeTime(started)
	ep.EndedAt = store.DecodeTime(ended)
	ep.Duration = time.Duration(durationMS) * time.Millisecond
	ep.Kind = Kind(kind)
	ep.ToolSequence = parseList(toolSeq)
	ep.SkillsUsed = parseList(skills)
	ep.ExemplarTurnIDs = parseList(exemplars)
	ep.SubjectsInvolved = parseList(subjects)
	ep.ConsolidatedInto = store.Text(consolidated)
	ep.WorkKey = store.Text(workKey)
	ep.ConversationKey = store.Text(conversationKey)
	if len(embedding) > 0 {
		vectors, err := splitWindows(embedding, windows)
		if err != nil {
			// A row whose vectors cannot be read is still a usable
			// episode for the time-window and outcome queries. Losing
			// the whole row over its recall vector would be a worse
			// trade than losing the recall.
			log.Warn("episode_embedding_undecodable", "episode", ep.ID,
				"windows", windows, "bytes", len(embedding), "error", err)
		} else {
			ep.Embeddings = vectors
		}
	}
	return ep, nil
}

// splitWindows unpacks the blob into the window vectors it holds.
//
// THE COUNT IS ASKED FOR, NOT INFERRED. The blob is the windows packed end to
// end with no separator, so the only thing that can say where one ends is the
// `embedding_windows` column written beside it — and node migration 0030 has
// the arithmetic showing why guessing from the length is not a weaker answer
// but a wrong one.
//
// A row whose length is not a whole multiple of its count is REFUSED rather
// than split on the nearest boundary that works: the two disagree only if the
// row was written by something that did not keep them together, and a plausible
// split of an implausible row is a vector made of two half-vectors, which
// ranks against every query and means nothing.
func splitWindows(blob []byte, windows int) ([][]float32, error) {
	if windows < 1 {
		return nil, fmt.Errorf("learning: %d bytes of embedding are labelled %d windows",
			len(blob), windows)
	}
	if len(blob)%windows != 0 {
		return nil, fmt.Errorf("learning: %d bytes of embedding do not divide into %d windows",
			len(blob), windows)
	}
	width := len(blob) / windows
	out := make([][]float32, 0, windows)
	for at := 0; at < len(blob); at += width {
		vector, err := store.DecodeVector(blob[at : at+width])
		if err != nil {
			return nil, err
		}
		out = append(out, vector)
	}
	return out, nil
}

// defaultEpisodeListing and defaultConversationListing are what [Episodes.Recent]
// and [Episodes.ForConversation] return for a caller that names no limit.
//
// FLOORS, NOT POLICY, for the same reason [defaultDiaryListing] is: every
// production caller passes an explicit limit. The conversation default is the
// smaller of the two on purpose — "the previous turns on this same ticket" is
// read to reconstruct one thread, where the useful answer is the last handful
// and the rest is prompt weight.
const (
	defaultEpisodeListing      = 10
	defaultConversationListing = 5
)

// Recent returns a seat's most recent episodes, newest first.
func (e *Episodes) Recent(ctx context.Context, handle string, limit int) ([]Episode, error) {
	if limit <= 0 {
		limit = defaultEpisodeListing
	}
	rows, err := e.db.SQL().QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes
		 WHERE agent_handle = ? ORDER BY ended_at DESC, id DESC LIMIT ?`,
		handle, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: recent episodes for %s: %w", handle, err)
	}
	return collectEpisodes(rows)
}

// ForConversation returns a seat's episodes on one conversation, newest first
// — "the previous turn on this same ticket".
func (e *Episodes) ForConversation(ctx context.Context, handle, conversation string, limit int) ([]Episode, error) {
	if conversation == "" {
		// A short-circuit, not a safety guard, and the difference is worth
		// stating: SQL already refuses to match '' against the NULLs that
		// mark turns with no conversation, so the dangerous reading — a
		// seat reading unrelated work as this thread's history — is
		// impossible either way. This just skips a round trip that can
		// only come back empty.
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultConversationListing
	}
	rows, err := e.db.SQL().QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes
		 WHERE agent_handle = ? AND conversation_key = ?
		 ORDER BY ended_at DESC, id DESC LIMIT ?`,
		handle, conversation, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: conversation episodes for %s: %w", handle, err)
	}
	return collectEpisodes(rows)
}

// EPISODES HAVE NO Purge, deliberately, and this note is here so the next
// reader does not add one.
//
// Every other short-horizon table gets a range delete on a single horizon,
// wired into the maintenance sweep. Episodes cannot: their retention is
// [Lifecycle.Pass], which applies FOUR different horizons to four different
// row states — mid-state rows go early, rows a skill absorbed go after an
// audit grace, exemplars of a compacted cluster stay raw, and the compacted
// summaries themselves are kept for years or forever. A single
// `DELETE WHERE ended_at < cutoff` would collapse all four, and the row it
// would take first is the compacted summary — the only record of a whole era
// of a seat's work, standing in for hundreds of turns that are already gone.

func collectEpisodes(rows *sql.Rows) ([]Episode, error) {
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		ep, err := scanEpisode(rows)
		if err != nil {
			return nil, fmt.Errorf("learning: scan episode: %w", err)
		}
		out = append(out, ep)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: read episodes: %w", err)
	}
	return out, nil
}

// jsonList renders a string slice as a JSON array, never as null.
//
// A nil slice marshals to `null`, and the columns are NOT NULL with a '[]'
// default — so a nil would be stored as the four characters "null" and read
// back as a parse failure on every subsequent read of that row.
func jsonList(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	blob, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(blob)
}

// parseList reads a JSON array column, tolerating anything else.
//
// A row written by a different version, or hand-edited, costs its list rather
// than the whole episode: the plan summary and the outcome are what recall is
// for, and they are still readable.
func parseList(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

// ErrNoEmbedding reports a recall asked for without a query vector.
var ErrNoEmbedding = errors.New("learning: recall needs a query embedding")
