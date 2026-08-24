package learning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/store"
)

// SkillState is where a synthesized skill sits in the curator's lifecycle.
//
// The machine is active -> stale -> archived on disuse and stale -> active on
// revival. ARCHIVED IS NOT DELETED: the row stays readable so an operator can
// restore one, and nothing in this file removes a skill.
type SkillState string

const (
	// SkillActive is a skill the Plan prefetch offers and the loader loads.
	SkillActive SkillState = "active"
	// SkillStale is one going unused. It is still listed and still
	// loadable — the prefetch renders it with a marker so the agent knows
	// it is ageing — and loading it revives it.
	SkillStale SkillState = "stale"
	// SkillArchived is one aged out of the catalogue. Listings hide it by
	// default and the curator never brings it back on its own.
	SkillArchived SkillState = "archived"
)

// RefinementKind says why a skill's prior body was archived. The set is fixed
// and the store refuses anything outside it.
//
// The column carries no CHECK constraint — the schema names the six kinds in a
// comment — so this is the only thing standing between a typo and a history
// row that groups under a kind nobody named. A dashboard grouping by kind
// cannot tell such a row from a legitimate one; it just shows a seventh
// category with one member.
type RefinementKind string

const (
	// RefineObserved and RefineCounterExample are the post-turn auto-refiner's
	// two notes: the turn went well and confirmed the skill, or it did not.
	RefineObserved       RefinementKind = "observed_in_practice"
	RefineCounterExample RefinementKind = "counter_example"
	// RefineTool is the model editing its own skill through the refine_skill
	// builtin, rather than the refiner deciding for it.
	RefineTool RefinementKind = "refine_skill_tool"
	// RefineReplace is a re-synthesis writing over a name the seat already
	// has. It is the reason the (seat, name) unique index does not simply
	// refuse the second draft: the new body replaces the old one THROUGH the
	// archive, so the superseded skill is recoverable rather than lost.
	RefineReplace RefinementKind = "replace"
	// RefinePromotion is a body rewritten because the cross-agent pass
	// distilled it into a knowledge-base draft.
	RefinePromotion RefinementKind = "promotion"
	// RefineRollback is [Skills.Rollback] archiving the state it is undoing.
	// A rollback is a forward step, so it archives like any other edit and a
	// rollback of a rollback works.
	RefineRollback RefinementKind = "rollback"
)

func (k RefinementKind) valid() bool {
	switch k {
	case RefineObserved, RefineCounterExample, RefineTool,
		RefineReplace, RefinePromotion, RefineRollback:
		return true
	}
	return false
}

func validState(s SkillState) bool {
	return s == SkillActive || s == SkillStale || s == SkillArchived
}

// Errors callers branch on.
var (
	// ErrSkillExists reports the (seat, name) unique index refusing a second
	// draft of a skill the seat already has. Overwriting one is
	// [Skills.Update] with [RefineReplace], which archives first.
	ErrSkillExists = errors.New("learning: the seat already has a skill with that name")
	// ErrUnknownSkill reports an edit aimed at a row that is not there.
	ErrUnknownSkill = errors.New("learning: no such skill")
	// ErrRefinementKind and ErrSkillState report a value outside the fixed set.
	ErrRefinementKind = errors.New("learning: unknown refinement kind")
	ErrSkillState     = errors.New("learning: unknown skill state")
)

const (
	// defaultVersionsKept caps one skill's archived history. Carried from the
	// Python engine's max_versions_kept, which is 10 at every call site.
	//
	// The table has no retention sweep of its own — unlike the five tables
	// the maintenance worker walks — so this prune is the only bound on it. A
	// skill the auto-refiner annotates after every turn would otherwise grow
	// one full copy of its body per turn, forever.
	defaultVersionsKept = 10

	// updateAttempts bounds the retry around one refine transaction.
	//
	// The transaction reads the live row and writes it in one shot, so a
	// competing writer does not corrupt anything — it makes our write
	// conflict with its snapshot, and SQLite answers that with an error
	// rather than an interleave. Retrying re-reads and re-applies. Carried
	// from Python's _UPDATE_MAX_ATTEMPTS: concurrent refines of the SAME
	// skill need the refine_skill builtin to overlap the post-turn
	// auto-refiner, which is rare, so a handful of attempts is headroom.
	updateAttempts = 5

	// useAttempts bounds the retry around a use-telemetry write.
	//
	// One retry, because the failure it exists for is a write conflict that
	// clears as soon as the other writer commits — a local single-file
	// commit, not a network round trip. A second failure means something
	// durable is wrong and hammering it does not help; the caller is told
	// instead, so it can publish SkillTelemetryWriteFailed before the curator
	// archives a skill whose last-used stamp never refreshed.
	useAttempts = 2

	// The disuse schedule, carried from the Python curator's
	// stale_after_days=30 / archive_after_days=90.
	defaultStaleAfter   = 30 * 24 * time.Hour
	defaultArchiveAfter = 90 * 24 * time.Hour

	// defaultVersionListing is how many archived versions a listing returns
	// when the caller does not say. Carried from Python's limit=20.
	defaultVersionListing = 20
)

// Reasons recorded on a curator transition. They land in the log line and on
// the change the worker publishes, so an operator reading "why did my skill
// disappear" gets the window that expired rather than "the curator did it".
const (
	reasonIdlePastStale   = "curator:idle_past_stale_window"
	reasonIdlePastArchive = "curator:idle_past_archive_window"
	reasonRecentUse       = "curator:recent_use_after_stale"
)

// Skill is one auto-drafted skill belonging to one seat.
//
// Agent-scope only. The cross-agent promotion pass does not write rows here —
// it drafts a knowledge-base page under the unit's auto-drafted parent for a
// lead to review — so this store is single-scope and keyed by (seat, name).
type Skill struct {
	ID          string
	AgentHandle string
	Name        string

	Description string
	Content     string
	// Frontmatter is the YAML header and Content the Markdown body of the
	// SKILL.md the loader reconstructs.
	Frontmatter map[string]any

	// ToolSequence is the tool run the skill encodes; the synthesizer reads
	// the seat's existing sequences to avoid drafting the same skill twice.
	ToolSequence []string
	// SourceEpisodeIDs are the turns the skill was distilled from.
	SourceEpisodeIDs []string

	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time

	// UseCount and LastUsedAt are the use telemetry. Without them the
	// question "do agents actually load the skills the synthesizer drafts, or
	// is the apparent benefit just extra sampling?" has no answer at all.
	// LastUsedAt is zero until the skill is first loaded, which is a real,
	// distinct value from "used long ago" — every guard here keeps them apart.
	UseCount   int
	LastUsedAt time.Time

	State SkillState
	// Pinned exempts the row from EVERY automatic transition. Operators set
	// it by hand; nothing promotes a skill to pinned.
	Pinned bool
	// StaleAt is when the row last went stale, ArchivedAt when it was
	// archived. Archiving deliberately leaves StaleAt standing (see
	// [Skills.Transition]).
	StaleAt    time.Time
	ArchivedAt time.Time
}

// Revision is the part of a skill an edit may change: its body, not its
// identity and not its lifecycle.
//
// A separate type rather than passing a mutated [Skill], because an edit that
// accepted the whole row would carry five fields the store owns and ignores —
// version, state, pinned and the two use counters — and a caller cannot tell
// an ignored field from an honoured one by looking.
type Revision struct {
	Description      string
	Content          string
	Frontmatter      map[string]any
	ToolSequence     []string
	SourceEpisodeIDs []string
}

// Revision returns the skill's current body as an editable copy.
//
// A copy: the maps and slices are cloned, so a caller that appends a bullet to
// the copy has not also mutated the Skill it read.
func (sk Skill) Revision() Revision {
	return Revision{
		Description:      sk.Description,
		Content:          sk.Content,
		Frontmatter:      maps.Clone(sk.Frontmatter),
		ToolSequence:     slices.Clone(sk.ToolSequence),
		SourceEpisodeIDs: slices.Clone(sk.SourceEpisodeIDs),
	}
}

// SkillVersion is a body this skill used to have, archived by the edit that
// replaced it.
type SkillVersion struct {
	ID      string
	SkillID string

	AgentHandle      string
	Name             string
	Description      string
	Content          string
	Frontmatter      map[string]any
	ToolSequence     []string
	SourceEpisodeIDs []string

	// Version is the version number of the ARCHIVED body, one less than the
	// live row carried after the edit.
	Version int
	Kind    RefinementKind
	Note    string

	ArchivedAt time.Time
}

// Revision returns the archived body, ready to be written back over the live
// row. See [Skills.Rollback], which does exactly that.
func (v SkillVersion) Revision() Revision {
	return Revision{
		Description:      v.Description,
		Content:          v.Content,
		Frontmatter:      maps.Clone(v.Frontmatter),
		ToolSequence:     slices.Clone(v.ToolSequence),
		SourceEpisodeIDs: slices.Clone(v.SourceEpisodeIDs),
	}
}

// Refinement is why and when a skill's body changed.
type Refinement struct {
	Kind RefinementKind
	// Note is free text an operator reads next to the archived body.
	Note string
	// KeepVersions caps the archived history for this skill; 0 means
	// defaultVersionsKept. There is no "keep everything": the table has no
	// other bound, so unbounded history is how one hot skill fills the file.
	KeepVersions int
	At           time.Time
}

// ListOptions narrows a skill listing by lifecycle state.
//
// Both fields are inverted relative to the Python flags they carry
// (include_archived / include_stale), so the zero value is the answer the
// engine wants everywhere: archived hidden, stale shown. A prefetch that
// forgets to pass options offers exactly the skills the loader will accept.
type ListOptions struct {
	// IncludeArchived surfaces rows the curator aged out — the operator's
	// restore path, not the agent's.
	IncludeArchived bool
	// ExcludeStale drops ageing rows. Off by default: stale is a marker on a
	// skill that still works and still revives on use, not a hidden state.
	ExcludeStale bool
}

// filter renders the exclusions as SQL.
//
// The state names are compile-time constants of this package and never caller
// input, so they are spelled inline. Binding them would thread a
// variable-length argument list through every read for no safety a constant
// does not already have.
func (o ListOptions) filter() string {
	switch {
	case o.IncludeArchived && o.ExcludeStale:
		return " AND state <> 'stale'"
	case o.IncludeArchived:
		return ""
	case o.ExcludeStale:
		return " AND state NOT IN ('archived', 'stale')"
	default:
		return " AND state <> 'archived'"
	}
}

// Skills is a seat's synthesized-skill library, and the curator's write side.
type Skills struct{ db *store.DB }

// NewSkills wraps a database handle.
func NewSkills(db *store.DB) *Skills { return &Skills{db: db} }

const skillColumns = `id, agent_handle, name, description, content, frontmatter,
	tool_sequence, source_episode_ids, version, created_at, updated_at,
	use_count, last_used_at, state, pinned, stale_at, archived_at`

const versionColumns = `id, skill_id, agent_handle, name, description, content,
	frontmatter, tool_sequence, source_episode_ids, version, refinement_kind,
	refinement_note, archived_at`

// Insert writes a new skill.
//
// Reports [ErrSkillExists] when the seat already has one under that name. The
// unique index makes a duplicate row impossible, so a re-synthesis has exactly
// two honest outcomes: leave the existing skill alone, or replace its body
// through [Skills.Update] with [RefineReplace], which archives what it
// overwrites. Silently upserting here would be the third: an LLM redraft
// landing on top of a refined, version-counted skill with no history row and
// nothing to roll back to.
func (s *Skills) Insert(ctx context.Context, sk Skill) error {
	switch {
	case sk.ID == "" || sk.AgentHandle == "" || sk.Name == "":
		return fmt.Errorf("learning: a skill needs an id, a seat and a name")
	case sk.CreatedAt.IsZero() || sk.UpdatedAt.IsZero():
		// A zero time encodes to year 1, and every consumer reads it as a
		// real instant: the curator ages the skill from it and archives it
		// on the first tick, and the health rollup reports an average skill
		// age of some 700,000 days.
		return fmt.Errorf("learning: skill %q needs created and updated times", sk.Name)
	}
	if sk.Version <= 0 {
		sk.Version = 1
	}
	if sk.State == "" {
		sk.State = SkillActive
	}
	if !validState(sk.State) {
		return fmt.Errorf("%w: %q", ErrSkillState, sk.State)
	}

	res, err := s.db.SQL().ExecContext(ctx, `
		INSERT INTO synthesized_skills (`+skillColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (agent_handle, name) DO NOTHING`,
		sk.ID, sk.AgentHandle, sk.Name, sk.Description, sk.Content,
		jsonObject(sk.Frontmatter), jsonList(sk.ToolSequence),
		jsonList(sk.SourceEpisodeIDs), sk.Version,
		store.EncodeTime(sk.CreatedAt), store.EncodeTime(sk.UpdatedAt),
		sk.UseCount, store.NullTime(sk.LastUsedAt), string(sk.State),
		sk.Pinned, store.NullTime(sk.StaleAt), store.NullTime(sk.ArchivedAt),
	)
	if err != nil {
		return fmt.Errorf("learning: insert skill %q: %w", sk.Name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("learning: insert skill %q: %w", sk.Name, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s/%s", ErrSkillExists, sk.AgentHandle, sk.Name)
	}
	log.InfoContext(ctx, "synthesized_skill_stored",
		"seat", sk.AgentHandle, "skill", sk.Name,
		"tools", len(sk.ToolSequence), "sources", len(sk.SourceEpisodeIDs))
	return nil
}

// Get returns a seat's skill by name, hiding archived rows.
//
// This is the LOADER's question — "may I load this?" — and the answer for an
// archived skill is no. An operator looking for one to restore lists with
// [ListOptions.IncludeArchived] instead.
func (s *Skills) Get(ctx context.Context, handle, name string) (Skill, bool, error) {
	if handle == "" || name == "" {
		return Skill{}, false, nil
	}
	return s.one(ctx, s.db.SQL(),
		`WHERE agent_handle = ? AND name = ? AND state <> 'archived'`, handle, name)
}

// ByID returns any row, archived included.
//
// The refiner's and the operator's question — "what is this row?" — where the
// id came from a history entry or an event and the state is what the caller is
// trying to find out.
func (s *Skills) ByID(ctx context.Context, id string) (Skill, bool, error) {
	if id == "" {
		return Skill{}, false, nil
	}
	return s.one(ctx, s.db.SQL(), `WHERE id = ?`, id)
}

// List returns a seat's skills, newest first.
func (s *Skills) List(ctx context.Context, handle string, opts ListOptions) ([]Skill, error) {
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT `+skillColumns+` FROM synthesized_skills
		 WHERE agent_handle = ?`+opts.filter()+`
		 ORDER BY created_at DESC, id DESC`, handle)
	if err != nil {
		return nil, fmt.Errorf("learning: list skills for %s: %w", handle, err)
	}
	return collectSkills(rows)
}

// ListFor returns every skill owned by any of the seats, newest first.
//
// The cross-agent promotion pass assembles a unit's candidate set with this,
// so it deliberately spans owners where every other read is single-seat.
func (s *Skills) ListFor(ctx context.Context, handles []string, opts ListOptions) ([]Skill, error) {
	if len(handles) == 0 {
		// An empty IN () is a syntax error rather than an empty answer, so
		// the empty unit short-circuits here.
		return nil, nil
	}
	args := make([]any, len(handles))
	for i, h := range handles {
		args[i] = h
	}
	// Only the placeholder COUNT is built from the input; every handle is
	// still bound.
	list := strings.TrimSuffix(strings.Repeat("?, ", len(handles)), ", ")
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT `+skillColumns+` FROM synthesized_skills
		 WHERE agent_handle IN (`+list+`)`+opts.filter()+`
		 ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("learning: list skills for %d seat(s): %w", len(handles), err)
	}
	return collectSkills(rows)
}

// Count reports how many skills a seat has.
//
// It takes the same options as the listings, and the caller has to say which
// question it is asking. The synthesizer's per-seat cap is the one that
// matters: counting archived rows against it means a long-lived seat that has
// aged out its library stops being able to learn anything new, forever, while
// every listing shows it holding almost nothing.
func (s *Skills) Count(ctx context.Context, handle string, opts ListOptions) (int, error) {
	var n int
	err := s.db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM synthesized_skills
		 WHERE agent_handle = ?`+opts.filter(), handle).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("learning: count skills for %s: %w", handle, err)
	}
	return n, nil
}

// ToolSequences returns the tool runs a seat's skills already encode, for the
// synthesizer's near-duplicate check.
func (s *Skills) ToolSequences(ctx context.Context, handle string, opts ListOptions) ([][]string, error) {
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT tool_sequence FROM synthesized_skills
		 WHERE agent_handle = ?`+opts.filter(), handle)
	if err != nil {
		return nil, fmt.Errorf("learning: tool sequences for %s: %w", handle, err)
	}
	defer rows.Close()
	var out [][]string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("learning: scan tool sequence: %w", err)
		}
		out = append(out, parseList(raw))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: read tool sequences: %w", err)
	}
	return out, nil
}

// Update replaces a live skill's body, archiving what it replaced.
//
// ONE TRANSACTION, and that is the load-bearing part. The archive row and the
// live row must never be able to disagree, and doing them as two statements
// gives two ways to disagree: archive-then-fail leaves a history entry for a
// body that is still live, and update-then-fail loses the superseded body
// permanently — which is the one thing the history table exists to prevent.
// (The Python store took the second shape: it updated the live row first and
// inserted the version row afterwards.) A failed attempt here rolls back to
// both-unchanged, so the retry below cannot leave a stray version row either.
//
// Last writer wins on the body, deliberately: a competing refinement is not
// lost, it is demoted to history and recoverable through [Skills.Rollback].
//
// The refinement does not touch the lifecycle: refining a stale skill leaves
// it stale. Only use revives (see [Skills.MarkUsed]).
func (s *Skills) Update(ctx context.Context, skillID string, rev Revision, r Refinement) (Skill, error) {
	if skillID == "" {
		return Skill{}, fmt.Errorf("%w: no id given", ErrUnknownSkill)
	}
	if !r.Kind.valid() {
		return Skill{}, fmt.Errorf("%w: %q", ErrRefinementKind, r.Kind)
	}
	if r.At.IsZero() {
		return Skill{}, fmt.Errorf("learning: a refinement needs a timestamp")
	}

	var (
		out Skill
		err error
	)
	for attempt := range updateAttempts {
		var updated Skill
		err = s.db.Tx(ctx, func(tx *sql.Tx) error {
			current, ok, err := s.one(ctx, tx, `WHERE id = ?`, skillID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: %s", ErrUnknownSkill, skillID)
			}
			if err := archiveVersion(ctx, tx, current, r); err != nil {
				return fmt.Errorf("learning: archive skill %s v%d: %w",
					skillID, current.Version, err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE synthesized_skills
				SET description = ?, content = ?, frontmatter = ?,
					tool_sequence = ?, source_episode_ids = ?,
					version = ?, updated_at = ?
				WHERE id = ?`,
				rev.Description, rev.Content, jsonObject(rev.Frontmatter),
				jsonList(rev.ToolSequence), jsonList(rev.SourceEpisodeIDs),
				current.Version+1, store.EncodeTime(r.At), skillID,
			); err != nil {
				return fmt.Errorf("learning: update skill %s: %w", skillID, err)
			}
			// Read back inside the transaction rather than assembling the
			// answer from the arguments: the returned value is then what is
			// stored, including the columns this edit did not write.
			updated, ok, err = s.one(ctx, tx, `WHERE id = ?`, skillID)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: %s vanished mid-update", ErrUnknownSkill, skillID)
			}
			return nil
		})
		if err == nil {
			out = updated
			break
		}
		if errors.Is(err, ErrUnknownSkill) {
			// A definitive answer. Retrying re-reads the same absence.
			return Skill{}, err
		}
		// Anything else is retried, because telling a write conflict from a
		// dead store means reading driver error strings and the two certified
		// drivers do not word them the same. A bounded retry costs one extra
		// transaction on a store that is genuinely down, and recovers the
		// case that is actually common.
		log.WarnContext(ctx, "synthesized_skill_update_retry",
			"skill", skillID, "attempt", attempt, "error", err)
		sleep(ctx, retryBeat())
	}
	if err != nil {
		return Skill{}, fmt.Errorf("learning: refine skill %s after %d attempts: %w",
			skillID, updateAttempts, err)
	}

	// Pruning is deliberately OUTSIDE the transaction. It is bookkeeping over
	// rows the edit just wrote, and rolling a good refinement back because
	// the trim failed would trade the model's work for a bounded amount of
	// extra history. It is also self-healing: the delete is offset-based, so
	// the next edit trims whatever this one left behind.
	s.pruneVersions(ctx, skillID, r.KeepVersions)

	log.InfoContext(ctx, "synthesized_skill_updated",
		"skill", skillID, "kind", string(r.Kind), "version", out.Version)
	return out, nil
}

// archiveVersion copies a skill's current body into the history table.
func archiveVersion(ctx context.Context, tx *sql.Tx, sk Skill, r Refinement) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO synthesized_skill_versions (`+versionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), sk.ID, sk.AgentHandle, sk.Name, sk.Description,
		sk.Content, jsonObject(sk.Frontmatter), jsonList(sk.ToolSequence),
		jsonList(sk.SourceEpisodeIDs), sk.Version, string(r.Kind), r.Note,
		store.EncodeTime(r.At))
	return err
}

// pruneVersions trims a skill's history to the newest keep rows.
//
// The order is (archived_at DESC, version DESC), and the tiebreaker is load
// bearing rather than decorative: the archive stamp comes from the caller, so
// two edits made in one pass carry the SAME instant, and without the version
// tiebreaker the pair sorts arbitrarily and the trim can keep the older body.
func (s *Skills) pruneVersions(ctx context.Context, skillID string, keep int) {
	if keep <= 0 {
		keep = defaultVersionsKept
	}
	if _, err := s.db.SQL().ExecContext(ctx, `
		DELETE FROM synthesized_skill_versions
		WHERE id IN (
			SELECT id FROM synthesized_skill_versions
			WHERE skill_id = ?
			ORDER BY archived_at DESC, version DESC
			LIMIT -1 OFFSET ?)`, skillID, keep); err != nil {
		log.WarnContext(ctx, "synthesized_skill_history_not_pruned",
			"skill", skillID, "error", err)
	}
}

// Versions returns a skill's archived bodies, newest first.
func (s *Skills) Versions(ctx context.Context, skillID string, limit int) ([]SkillVersion, error) {
	if limit <= 0 {
		limit = defaultVersionListing
	}
	rows, err := s.db.SQL().QueryContext(ctx,
		`SELECT `+versionColumns+` FROM synthesized_skill_versions
		 WHERE skill_id = ?
		 ORDER BY archived_at DESC, version DESC
		 LIMIT ?`, skillID, limit)
	if err != nil {
		return nil, fmt.Errorf("learning: list versions of %s: %w", skillID, err)
	}
	defer rows.Close()
	var out []SkillVersion
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, fmt.Errorf("learning: scan skill version: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: read versions of %s: %w", skillID, err)
	}
	return out, nil
}

// Rollback writes an archived body back over the live row.
//
// A forward step, not a rewind: the state being undone is itself archived
// first, with [RefineRollback], and the version number goes UP. Rolling back a
// rollback therefore works, and nothing an operator undoes is unrecoverable.
//
// Reports false when the version id is unknown.
func (s *Skills) Rollback(ctx context.Context, versionID string, at time.Time, keepVersions int) (Skill, bool, error) {
	if versionID == "" {
		return Skill{}, false, nil
	}
	row := s.db.SQL().QueryRowContext(ctx,
		`SELECT `+versionColumns+` FROM synthesized_skill_versions WHERE id = ?`, versionID)
	v, err := scanVersion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Skill{}, false, nil
	}
	if err != nil {
		return Skill{}, false, fmt.Errorf("learning: read version %s: %w", versionID, err)
	}

	// The skill this version belongs to must exist: the foreign key is ON
	// DELETE CASCADE and the store runs with foreign keys on, so a deleted
	// skill takes its history with it. An ErrUnknownSkill out of Update here
	// would mean that enforcement is off.
	out, err := s.Update(ctx, v.SkillID, v.Revision(), Refinement{
		Kind:         RefineRollback,
		Note:         fmt.Sprintf("rolled back to version %d", v.Version),
		KeepVersions: keepVersions,
		At:           at,
	})
	if err != nil {
		return Skill{}, false, err
	}
	return out, true, nil
}

// Use is what one recorded skill load did. The zero value is "nothing was
// recorded", which is what a caller gets from a store that could not be
// written.
type Use struct {
	// Recorded says the counters moved.
	Recorded bool
	// Revived says the row was stale and is active again.
	Revived bool
}

// MarkUsed records that a skill was loaded, and revives it if it was stale.
//
// IT CANNOT FAIL A LOAD, and the signature is how that is guaranteed rather
// than promised: there is no error to propagate, so no caller can turn a
// telemetry write into a failed skill load. A false Recorded is for the caller
// to publish SkillTelemetryWriteFailed on — operators need to see a degraded
// health rollup before the curator archives a skill whose last-used stamp
// stopped refreshing.
//
// The revival is part of the same transaction as the bump, deliberately: a
// skill used again is visible to the very next Plan prefetch, not only after
// the curator's next tick, which on the default schedule is up to a day later.
//
// An archived row's counters still move and it stays archived. Loading one is
// refused upstream, so reaching here with one means a caller bypassed that —
// and silently resurrecting a row the curator aged out, on a path whose whole
// contract is that it cannot fail anything, is not the place to decide that.
func (s *Skills) MarkUsed(ctx context.Context, skillID string, at time.Time) Use {
	if skillID == "" || at.IsZero() {
		return Use{}
	}
	var lastErr error
	for attempt := range useAttempts {
		var use Use
		err := s.db.Tx(ctx, func(tx *sql.Tx) error {
			// The prior state is read in the same transaction as the bump so
			// that Revived answers about the row this write actually moved.
			var state string
			if err := tx.QueryRowContext(ctx,
				`SELECT state FROM synthesized_skills WHERE id = ?`, skillID,
			).Scan(&state); err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `
				UPDATE synthesized_skills
				SET use_count = use_count + 1,
					last_used_at = ?,
					state = CASE WHEN state = 'stale' THEN 'active' ELSE state END,
					stale_at = CASE WHEN state = 'stale' THEN NULL ELSE stale_at END
				WHERE id = ?`, store.EncodeTime(at), skillID)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			use = Use{Recorded: n == 1, Revived: n == 1 && SkillState(state) == SkillStale}
			return nil
		})
		if err == nil {
			return use
		}
		lastErr = err
		if errors.Is(err, sql.ErrNoRows) {
			// No such skill. Definitive — retrying reads the same absence.
			log.WarnContext(ctx, "synthesized_skill_use_unknown_skill", "skill", skillID)
			return Use{}
		}
		if attempt+1 < useAttempts {
			log.WarnContext(ctx, "synthesized_skill_mark_used_retry",
				"skill", skillID, "error", err)
			// The Python engine waited 50-150ms here, sized for a round
			// trip to a remote Postgres. See retryBeat.
			sleep(ctx, retryBeat())
		}
	}
	log.ErrorContext(ctx, "synthesized_skill_mark_used_failed",
		"skill", skillID, "attempts", useAttempts, "error", lastErr)
	return Use{}
}

// retryBeat is the jittered pause between attempts at a conflicted write.
//
// Sized to what is being waited out: one local write transaction committing,
// which is microseconds here. Jittered because the conflict SQLite reports —
// "database snapshot is stale" — returns immediately with no wait of its own,
// so retries fired back to back re-collide inside the same contention window
// and a whole retry budget is spent in a few microseconds. Measured on four
// goroutines refining one skill twelve times: without the pause a run lost a
// refinement about one time in five, with it about one in fifteen.
func retryBeat() time.Duration {
	return time.Duration(rand.N(4_000)+1_000) * time.Microsecond
}

// sleep waits, or returns early if the context is done.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Guard is an optimistic precondition on a transition: apply it only while the
// row's last-used stamp still reads as it did when the caller looked.
//
// On is a separate field because the ZERO TIME IS A REAL GUARD VALUE — a skill
// that has never been used — so it cannot double as "no guard". Python needed
// a module-level sentinel object to break the same ambiguity, since None was
// likewise a real value.
type Guard struct {
	On         bool
	LastUsedAt time.Time
}

// LastUsed builds the guard that holds while a skill's last-used stamp still
// equals the snapshot. The zero time means "and it has still never been used".
func LastUsed(at time.Time) Guard { return Guard{On: true, LastUsedAt: at} }

// Transition is one lifecycle move.
type Transition struct {
	SkillID string
	To      SkillState
	// Reason lands in the log line; an operator asking why a skill
	// disappeared gets the window that expired, not "the curator did it".
	Reason string
	At     time.Time
	Guard  Guard
}

// Transition moves a skill to a new state and stamps the matching timestamp.
//
// Reports whether a row moved. FALSE IS NOT AN ERROR and an error is not a
// false: a guard that no longer holds — someone used the skill between the
// curator's read and this write — is the mechanism working, while a store that
// cannot be written is an outage. The Python store returned False for both,
// which made a database outage render on the dashboard as the race guard doing
// its job.
//
// No version row is archived: a transition is out-of-band metadata, not a body
// change, and the history table is reserved for bodies.
//
// A pinned row is NOT refused here. Pinning exempts a skill from AUTOMATIC
// transitions (see [CuratorPolicy.Next]); an operator archiving one by hand is
// exactly the case pinning is not meant to block.
func (s *Skills) Transition(ctx context.Context, t Transition) (bool, error) {
	if !validState(t.To) {
		return false, fmt.Errorf("%w: %q", ErrSkillState, t.To)
	}
	if t.At.IsZero() {
		return false, fmt.Errorf("learning: a transition needs a timestamp")
	}
	var (
		set  string
		args []any
	)
	switch t.To {
	case SkillStale:
		set = "state = ?, stale_at = ?, archived_at = NULL"
		args = []any{string(t.To), store.EncodeTime(t.At)}
	case SkillArchived:
		// stale_at is deliberately left standing. An archived row keeps the
		// date it started ageing, so an operator reading it can see the two
		// steps that led here rather than only the last one.
		set = "state = ?, archived_at = ?"
		args = []any{string(t.To), store.EncodeTime(t.At)}
	default:
		// Back to active: both stamps clear, because the row is starting its
		// disuse clock over and a leftover stale_at reads as "still ageing".
		set = "state = ?, stale_at = NULL, archived_at = NULL"
		args = []any{string(t.To)}
	}
	query := `UPDATE synthesized_skills SET ` + set + ` WHERE id = ?`
	args = append(args, t.SkillID)
	if t.Guard.On {
		// `IS` rather than `=` so a NULL stamp matches a never-used row.
		// With `=` the guard would never hold for a skill that has never
		// been used, which is precisely the population the curator ages out
		// first, and the whole pass would silently do nothing.
		query += ` AND last_used_at IS ?`
		args = append(args, store.NullTime(t.Guard.LastUsedAt))
	}

	res, err := s.db.SQL().ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("learning: move skill %s to %s: %w", t.SkillID, t.To, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("learning: move skill %s to %s: %w", t.SkillID, t.To, err)
	}
	if n == 0 {
		log.DebugContext(ctx, "synthesized_skill_transition_raced",
			"skill", t.SkillID, "to", string(t.To))
		return false, nil
	}
	log.InfoContext(ctx, "synthesized_skill_state_transition",
		"skill", t.SkillID, "to", string(t.To), "reason", t.Reason)
	return true, nil
}

// SetPinned toggles a skill's exemption from automatic transitions. Reports
// whether a row was there to toggle.
func (s *Skills) SetPinned(ctx context.Context, skillID string, pinned bool) (bool, error) {
	res, err := s.db.SQL().ExecContext(ctx,
		`UPDATE synthesized_skills SET pinned = ? WHERE id = ?`,
		pinned, skillID)
	if err != nil {
		return false, fmt.Errorf("learning: pin skill %s: %w", skillID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("learning: pin skill %s: %w", skillID, err)
	}
	return n == 1, nil
}

// CuratorCandidates returns the rows one curator pass should evaluate: not
// pinned, not already archived. An empty handle spans every seat.
//
// The order is stable so a pass that fails halfway resumes over the same list
// rather than a reshuffled one.
func (s *Skills) CuratorCandidates(ctx context.Context, handle string) ([]Skill, error) {
	// `pinned = 0` compares against whatever the driver stored for a Go
	// bool, and a driver that encoded one as anything but 0/1 would not
	// fail here — it would quietly hand every pinned skill to the curator.
	// Both certified drivers store 1 (measured, and asserted by the
	// encoding test), which is what makes this comparison safe to write.
	query := `SELECT ` + skillColumns + ` FROM synthesized_skills
		 WHERE pinned = 0 AND state <> 'archived'`
	var args []any
	if handle != "" {
		query += ` AND agent_handle = ?`
		args = append(args, handle)
	}
	rows, err := s.db.SQL().QueryContext(ctx, query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, fmt.Errorf("learning: curator candidates: %w", err)
	}
	return collectSkills(rows)
}

// CuratorPolicy is the disuse schedule the state machine runs on.
type CuratorPolicy struct {
	// StaleAfter is how long an active skill may go unused before it is
	// marked ageing; ArchiveAfter is how long a stale one may go unused
	// before it leaves the catalogue. Zero means the default.
	StaleAfter   time.Duration
	ArchiveAfter time.Duration
}

func (p CuratorPolicy) normalized() CuratorPolicy {
	if p.StaleAfter <= 0 {
		p.StaleAfter = defaultStaleAfter
	}
	if p.ArchiveAfter <= 0 {
		p.ArchiveAfter = defaultArchiveAfter
	}
	if p.ArchiveAfter < p.StaleAfter {
		// An archive window INSIDE the stale window is a misconfiguration,
		// and taken literally it archives rows the same policy calls fresh:
		// a skill left stale by a since-widened window, or staled by hand,
		// is used 10 days ago and archived under "archive after 5" while
		// "stale after 30" says it is current. Widening to the stale window
		// is the reading both halves agree on — such a row revives instead.
		p.ArchiveAfter = p.StaleAfter
	}
	return p
}

// StateChange is one transition the curator decided on, carrying the row AS
// SNAPSHOTTED — so the state on Skill is the state being left, which is what a
// SkillRevived event has to report.
type StateChange struct {
	Skill  Skill
	To     SkillState
	Reason string
}

// Next reports the transition a skill is due, and false when it should stay
// put.
//
// Pure: it decides, it does not write. The pinned exemption is enforced here
// as well as in the candidate query — the query is the fast path, and this is
// the invariant. A caller that assembles candidates any other way must not be
// able to lose it.
//
// Idleness is measured from the last use, or from creation for a skill that
// has never been used. A never-used skill therefore ages out too, which is the
// point: the drafts nobody loads are exactly what the machine is for.
//
// Archived is terminal for the machine. Coming back is an operator's explicit
// [Skills.Transition], because an automatic un-archive would fight the pass
// that archived it and the pair would flip on alternate ticks.
func (p CuratorPolicy) Next(sk Skill, now time.Time) (StateChange, bool) {
	p = p.normalized()
	if sk.Pinned {
		return StateChange{}, false
	}
	idle := sk.LastUsedAt
	if idle.IsZero() {
		idle = sk.CreatedAt
	}
	switch sk.State {
	case SkillActive:
		if idle.Before(now.Add(-p.StaleAfter)) {
			return StateChange{Skill: sk, To: SkillStale, Reason: reasonIdlePastStale}, true
		}
	case SkillStale:
		switch {
		case idle.Before(now.Add(-p.ArchiveAfter)):
			return StateChange{Skill: sk, To: SkillArchived, Reason: reasonIdlePastArchive}, true
		case idle.After(now.Add(-p.StaleAfter)):
			// Reachable when the WINDOW moves, not when the skill does: use
			// revives a stale skill inside MarkUsed, in the same statement
			// that bumps its counters. What lands here is a row left stale
			// by a narrower policy than the one now configured — an operator
			// widening StaleAfter gets those skills back on the next pass
			// instead of having to un-archive them one by one later.
			return StateChange{Skill: sk, To: SkillActive, Reason: reasonRecentUse}, true
		}
	}
	return StateChange{}, false
}

// CurateResult is what one pass did.
type CurateResult struct {
	Scanned int
	// Applied is the transitions that landed, in the order they landed. The
	// worker publishes one lifecycle event per entry.
	Applied []StateChange
	// Raced counts transitions whose guard no longer held: the skill was
	// used between the pass reading it and the pass writing it. Not failures
	// — the guard is what keeps a mid-turn skill from being archived out
	// from under the agent that is holding it.
	Raced int
}

// Curate runs one pass of the state machine over a seat's skills, or over
// every seat when handle is empty.
//
// A failing transition ABORTS the pass and returns what has already landed
// alongside the error. The transitions are independent guarded writes, so the
// ones that landed are real and their events still have to be published; a
// caller that dropped the result on error would archive rows nobody was ever
// told about.
func (s *Skills) Curate(ctx context.Context, p CuratorPolicy, handle string, now time.Time) (CurateResult, error) {
	candidates, err := s.CuratorCandidates(ctx, handle)
	if err != nil {
		return CurateResult{}, err
	}
	res := CurateResult{Scanned: len(candidates)}
	for _, sk := range candidates {
		change, due := p.Next(sk, now)
		if !due {
			continue
		}
		t := Transition{
			SkillID: sk.ID,
			To:      change.To,
			Reason:  change.Reason,
			At:      now,
		}
		if change.To != SkillActive {
			// Demotions are guarded against the snapshot this pass read. The
			// Plan prefetch caches a seat's skills at turn start, so without
			// the guard a skill loaded mid-turn can be archived underneath
			// the agent holding it and its next use_skill fails.
			//
			// Revival is unguarded on purpose: bringing a recently used
			// skill back is the safe direction, and a guard there would let
			// a use race block the very thing the use should cause.
			t.Guard = LastUsed(sk.LastUsedAt)
		}
		applied, err := s.Transition(ctx, t)
		if err != nil {
			return res, err
		}
		if !applied {
			res.Raced++
			continue
		}
		res.Applied = append(res.Applied, change)
	}
	log.InfoContext(ctx, "skill_curator_pass",
		"scanned", res.Scanned, "applied", len(res.Applied), "raced", res.Raced)
	return res, nil
}

// SkillHealth is the per-seat skill-use rollup, read from the learning_health
// view.
//
// AvgUsesPerSkill is the single load-bearing metric: below 0.1 the library is
// not paying for itself. Archived skills are excluded from every field — the
// curator deliberately aged them out and the prefetch hides them, so counting
// them only deflates the average an operator is reading.
type SkillHealth struct {
	AgentHandle           string
	TotalSkills           int
	SkillsUsedAtLeastOnce int
	TotalUses             int
	// MostRecentUse is zero when no skill of this seat has ever been used.
	MostRecentUse   time.Time
	AvgUsesPerSkill float64
	// AvgAgeDays is measured against the DATABASE's clock rather than a
	// caller's, because the view stamps its own now. It is an operator
	// rollup; nothing in the engine decides anything from it.
	AvgAgeDays float64
}

// Health returns the rollup for one seat, or for every seat when handle is
// empty. A seat with no skills has no row at all rather than a zero one.
func (s *Skills) Health(ctx context.Context, handle string) ([]SkillHealth, error) {
	query := `SELECT agent_handle, total_skills, skills_used_at_least_once,
			total_skill_uses, most_recent_skill_use, avg_uses_per_skill,
			avg_skill_age_days
		FROM learning_health`
	var args []any
	if handle != "" {
		query += ` WHERE agent_handle = ?`
		args = append(args, handle)
	}
	rows, err := s.db.SQL().QueryContext(ctx, query+` ORDER BY agent_handle`, args...)
	if err != nil {
		return nil, fmt.Errorf("learning: read skill health: %w", err)
	}
	defer rows.Close()
	var out []SkillHealth
	for rows.Next() {
		var (
			h        SkillHealth
			lastUsed sql.NullInt64
		)
		if err := rows.Scan(&h.AgentHandle, &h.TotalSkills, &h.SkillsUsedAtLeastOnce,
			&h.TotalUses, &lastUsed, &h.AvgUsesPerSkill, &h.AvgAgeDays); err != nil {
			return nil, fmt.Errorf("learning: scan skill health: %w", err)
		}
		h.MostRecentUse = store.TimeAt(lastUsed)
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: read skill health: %w", err)
	}
	return out, nil
}

// one reads a single skill through whichever handle the caller is on — the
// pool, or an open transaction.
func (s *Skills) one(ctx context.Context, q querier, where string, args ...any) (Skill, bool, error) {
	sk, err := scanSkill(q.QueryRowContext(ctx,
		`SELECT `+skillColumns+` FROM synthesized_skills `+where, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Skill{}, false, nil
	}
	if err != nil {
		return Skill{}, false, fmt.Errorf("learning: read skill: %w", err)
	}
	return sk, true, nil
}

func scanSkill(row interface{ Scan(...any) error }) (Skill, error) {
	var (
		sk                            Skill
		frontmatter, tools, sourceIDs string
		state                         string
		created, updated              int64
		pinned                        int
		lastUsed, staleAt, archivedAt sql.NullInt64
	)
	if err := row.Scan(&sk.ID, &sk.AgentHandle, &sk.Name, &sk.Description,
		&sk.Content, &frontmatter, &tools, &sourceIDs, &sk.Version,
		&created, &updated, &sk.UseCount, &lastUsed, &state, &pinned,
		&staleAt, &archivedAt); err != nil {
		return Skill{}, err
	}
	sk.Frontmatter = parseObject(frontmatter)
	sk.ToolSequence = parseList(tools)
	sk.SourceEpisodeIDs = parseList(sourceIDs)
	sk.CreatedAt = store.DecodeTime(created)
	sk.UpdatedAt = store.DecodeTime(updated)
	sk.LastUsedAt = store.TimeAt(lastUsed)
	sk.State = SkillState(state)
	sk.Pinned = pinned != 0
	sk.StaleAt = store.TimeAt(staleAt)
	sk.ArchivedAt = store.TimeAt(archivedAt)
	return sk, nil
}

func scanVersion(row interface{ Scan(...any) error }) (SkillVersion, error) {
	var (
		v                             SkillVersion
		frontmatter, tools, sourceIDs string
		kind                          string
		archivedAt                    int64
	)
	if err := row.Scan(&v.ID, &v.SkillID, &v.AgentHandle, &v.Name, &v.Description,
		&v.Content, &frontmatter, &tools, &sourceIDs, &v.Version, &kind,
		&v.Note, &archivedAt); err != nil {
		return SkillVersion{}, err
	}
	v.Frontmatter = parseObject(frontmatter)
	v.ToolSequence = parseList(tools)
	v.SourceEpisodeIDs = parseList(sourceIDs)
	v.Kind = RefinementKind(kind)
	v.ArchivedAt = store.DecodeTime(archivedAt)
	return v, nil
}

func collectSkills(rows *sql.Rows) ([]Skill, error) {
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		sk, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("learning: scan skill: %w", err)
		}
		out = append(out, sk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("learning: read skills: %w", err)
	}
	return out, nil
}

// parseObject reads a JSON object column, tolerating anything else.
//
// Same bargain as parseList: a row written by another version, or hand-edited,
// costs its frontmatter rather than the whole skill. The body is what the
// agent needs and it is still readable.
func parseObject(raw string) map[string]any {
	if raw == "" || raw == "{}" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}
