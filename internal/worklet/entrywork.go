package worklet

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE WORK THAT NAMES AN ATLAS ENTRY. A task names the entries it touches as
// `atlas_entry` relations on its own record (ADR-0140), so "what is being
// worked on" is the tracker's question to answer, from its own rows: how many
// open tasks name each entry, and one entry's open and recently closed tasks.
// Who may ask is the caller's to decide; this reads for a Worklet reader.

const (
	// MaxEntryWork is one page of the tasks naming an entry: fifty, a
	// screen of them ([PageDefault]), with the totals beside the page.
	MaxEntryWork = PageDefault

	// MaxEntryWorkEntries is the most entry ids one list names: an entry and
	// the ids an exact key merged into it, which stay a handful.
	MaxEntryWorkEntries = 64

	// MaxEntryMarks is the most entries one answer counts open work for: two
	// thousand, what the atlas's map draws before its groups close.
	MaxEntryMarks = 2000

	// EntryWorkRecent is how long a finished task stays on an entry's list:
	// thirty days, the window a person asks "what was done here lately" in.
	EntryWorkRecent = 30 * 24 * time.Hour
)

// EntryWorkQuery asks for the tasks naming any of Entries, an entry's own id
// and the ids merged into it.
type EntryWorkQuery struct {
	Entries []string
	Level   statelog.ReadLevel
}

// EntryWork is the tasks naming an entry: the open ones, most recently changed
// first, then the ones finished within [EntryWorkRecent], and how many of
// each there are.
type EntryWork struct {
	Open        []TaskRow  `json:"open"`
	Recent      []TaskRow  `json:"recent"`
	OpenTotal   ClaimTotal `json:"open_total"`
	RecentTotal ClaimTotal `json:"recent_total"`
}

// EntryMarks is how many open tasks name each atlas entry, by the id the
// task names. Capped says the company has more entries with open work than
// [MaxEntryMarks], and which were left out is not known.
type EntryMarks struct {
	Open   map[string]int `json:"open"`
	Capped bool           `json:"capped,omitempty"`
}

// entryWorkLive is a task a list of work shows: not in the trash, not archived.
const entryWorkLive = `t.removed_at IS NULL AND t.archived = 0`

// entryMarksStatement counts the open tasks naming each entry, read off the
// entry index alone and one task row per edge.
var entryMarksStatement = `
	SELECT r.other_id, COUNT(*)
	FROM tracker_relations r
	JOIN tracker_tasks t ON t.id = r.task_id
	WHERE r.kind = '` + string(RelationEntry) + `'
	  AND ` + entryWorkLive + ` AND t.status_group IN (` + openGroupsSQL + `)
	GROUP BY r.other_id
	ORDER BY r.other_id
	LIMIT ?`

// entryWorkWhere is the tasks naming any of n entries, open or finished since
// an instant by which. The kind is bound, so the read seeks the reverse edge
// by each id; spelled out, the planner walked every entry edge instead.
func entryWorkWhere(n int, open bool) string {
	named := `t.id IN (SELECT x.task_id FROM tracker_relations x WHERE x.other_id IN (` +
		placeholders(n) + `) AND x.kind = ?) AND ` + entryWorkLive
	if open {
		return named + ` AND t.status_group IN (` + openGroupsSQL + `)`
	}
	return named + ` AND t.status_group NOT IN (` + openGroupsSQL + `) AND t.finished_at >= ?`
}

// entryWorkOrder is most recently changed first, the id breaking a tie.
var entryWorkOrder = []sortTerm{
	{Column: "t.updated_at", Descending: true, NeverNull: true},
	{Column: "t.id", NeverNull: true},
}

// domainRead is a read over the whole tracker at one level: work that names an
// entry can be on any task in any container.
func (r *Reader) domainRead(ctx context.Context, level statelog.ReadLevel, read func(tx *sql.Tx) error) error {
	_, err := r.log.Read(ctx, statelog.Query{
		Level: level,
		Scope: statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Set:   true,
	}, read)
	return err
}

// EntryMarks counts the open tasks naming each atlas entry.
func (r *Reader) EntryMarks(ctx context.Context, level statelog.ReadLevel) (EntryMarks, error) {
	if level == "" {
		return EntryMarks{}, invalid("a read of the work on atlas entries names a level")
	}
	out := EntryMarks{Open: map[string]int{}}
	err := r.domainRead(ctx, level, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, entryMarksStatement, MaxEntryMarks+1)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var entry string
			var open int
			if err = rows.Scan(&entry, &open); err != nil {
				return fmt.Errorf("scan a count: %w", err)
			}
			if len(out.Open) == MaxEntryMarks {
				out.Capped = true
				break
			}
			out.Open[entry] = open
		}
		return rows.Err()
	})
	if err != nil {
		return EntryMarks{}, fmt.Errorf("worklet: count the open work on atlas entries: %w", err)
	}
	return out, nil
}

// EntryWork reads the tasks naming any of q's entries at now.
func (r *Reader) EntryWork(ctx context.Context, q EntryWorkQuery, now time.Time) (EntryWork, error) {
	switch {
	case q.Level == "":
		return EntryWork{}, invalid("a read of the work on an atlas entry names a level")
	case len(q.Entries) == 0 || len(q.Entries) > MaxEntryWorkEntries:
		return EntryWork{}, invalid("a read of the work on an atlas entry names 1 to %d entry ids, not %d",
			MaxEntryWorkEntries, len(q.Entries))
	}
	for _, id := range q.Entries {
		if _, err := uuid.Parse(id); err != nil {
			return EntryWork{}, invalid("a read of the work on an atlas entry names it by its id")
		}
	}
	out := EntryWork{}
	err := r.domainRead(ctx, q.Level, func(tx *sql.Tx) error {
		ids := append(anyOf(q.Entries), string(RelationEntry))
		since := store.EncodeTime(now.UTC().Add(-EntryWorkRecent))
		for _, half := range []struct {
			set   taskSet
			rows  *[]TaskRow
			total *ClaimTotal
		}{
			{tasksWhere(entryWorkWhere(len(q.Entries), true), ids), &out.Open, &out.OpenTotal},
			{tasksWhere(entryWorkWhere(len(q.Entries), false), append(slices.Clone(ids), since)), &out.Recent,
				&out.RecentTotal},
		} {
			total, capped, err := countHint(ctx, tx, half.set)
			if err != nil {
				return err
			}
			*half.total = ClaimTotal{Total: total, Capped: capped}
			// NEVER NIL: an entry no task names answers two empty lists.
			*half.rows = []TaskRow{}
			if total == 0 {
				continue
			}
			page, _, err := readTasks(ctx, tx, half.set, entryWorkOrder, MaxEntryWork, now.UTC())
			if err != nil {
				return err
			}
			*half.rows = page
		}
		return nil
	})
	if err != nil {
		return EntryWork{}, fmt.Errorf("worklet: read the work on an atlas entry: %w", err)
	}
	return out, nil
}

// Entries is the ids of the atlas entries t names, in the order it names them.
func (t Task) Entries() []string {
	return relationMembers(t.Relations, RelationEntry)
}

// WithoutEntries is d for a reader who may not read the atlas: every atlas
// entry the task names left out of its record and its links, and counted. An
// entry's id is derived from its kind and key, so the id is withheld with the
// name it would confirm.
func (d TaskDetail) WithoutEntries() TaskDetail {
	entry := func(k RelationKind) bool { return k == RelationEntry }
	d.EntriesWithheld = len(d.Task.Entries())
	d.Task.Relations = slices.DeleteFunc(slices.Clone(d.Task.Relations),
		func(r Relation) bool { return entry(r.Kind) })
	d.Links = slices.DeleteFunc(slices.Clone(d.Links), func(l DetailLink) bool { return entry(l.Kind) })
	return d
}
