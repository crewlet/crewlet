package memsync

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// THE WATERMARK IS NEVER REUSED. An append-only table is carried past its
// watermark value of the last row the changelog acknowledged, so a row written
// after that has to come out ABOVE it. The rowid did not: these
// tables are keyed on text, so a new row took max(rowid) + 1 — and once the
// newest row was deleted (the diary's trim and expiry, the lifecycle's sweeps,
// a skill's pruned history) the next insert took that rowid again, below the
// mark, and was never published while this node held the seat.

// appendOnlyRow writes one row of an append-only table for the seat, under
// the given id, the way that table's writers do — one per table, so a table
// added to the registry without one fails below rather than going unchecked.
var appendOnlyRow = map[string]func(t *testing.T, db *store.DB, id string){
	"agent_diary": func(t *testing.T, db *store.DB, id string) {
		t.Helper()
		if err := learning.NewDiary(db).Write(t.Context(), learning.DiaryEntry{
			ID: id, AgentID: seat.AgentID, Kind: learning.DiaryLong,
			Content: "note " + id, Source: "reflect", CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("write diary %s: %v", id, err)
		}
	},
	"episodes": func(t *testing.T, db *store.DB, id string) {
		t.Helper()
		if _, err := learning.NewEpisodes(db).Append(t.Context(), learning.Episode{
			ID: id, Handle: seat.Handle, Role: "Engineer", TurnID: "turn-" + id,
			StartedAt: time.Now().UTC(), EndedAt: time.Now().UTC(),
			PlanSummary: "did " + id, TaskSummary: "woken for " + id, ReviewOutcome: "done",
		}); err != nil {
			t.Fatalf("append episode %s: %v", id, err)
		}
	},
	"synthesized_skill_versions": func(t *testing.T, db *store.DB, id string) {
		t.Helper()
		exec(t, db, `INSERT INTO synthesized_skills (id, agent_handle, name,
			description, content, frontmatter, tool_sequence, source_episode_ids,
			version, created_at, updated_at, state)
			VALUES ('s-wm', ?, 'ship-it', 'how', 'body', '{}', '[]', '[]', 1, 0, 0, 'active')
			ON CONFLICT DO NOTHING`, seat.Handle)
		exec(t, db, `INSERT INTO synthesized_skill_versions (id, skill_id,
			agent_handle, name, description, content, version, refinement_kind,
			archived_at) VALUES (?, 's-wm', ?, 'ship-it', 'how', 'body', 1, 'seed', 0)`,
			id, seat.Handle)
	},
}

func exec(t *testing.T, db *store.DB, statement string, args ...any) {
	t.Helper()
	if _, err := db.SQL().ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatalf("%v\n%s", err, statement)
	}
}

// exportedIDs is what an incremental carry of spec past after would publish,
// by key, and the mark it would leave.
func exportedIDs(t *testing.T, db *store.DB, spec table, after int64) ([]string, int64) {
	t.Helper()
	rows, high := exportAll(t, db, spec, after)
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.Values[spec.key[0]].(string))
	}
	return ids, high
}

func TestNoAppendOnlyTableReusesItsWatermark(t *testing.T) {
	t.Parallel()
	for _, spec := range tables {
		if spec.wholeEachCycle {
			continue
		}
		t.Run(spec.name, func(t *testing.T) {
			t.Parallel()
			if spec.watermark == "" {
				t.Fatalf("%s is carried incrementally and names no watermark column", spec.name)
			}
			write, ok := appendOnlyRow[spec.name]
			if !ok {
				t.Fatalf("%s is an append-only memory table with no writer in "+
					"appendOnlyRow, so nothing here checks that a row written "+
					"after its newest was deleted is still carried — add one", spec.name)
			}
			if len(spec.key) != 1 {
				t.Fatalf("%s: this check names a row by a one-column key", spec.name)
			}
			db := openStore(t)
			write(t, db, "a")
			write(t, db, "b")
			first, mark := exportedIDs(t, db, spec, 0)
			if !slices.Equal(first, []string{"a", "b"}) {
				t.Fatalf("the first carry was %v, want [a b]", first)
			}

			// The newest row goes, which is what every sweep of these
			// tables does to SOME row, and the next write follows it.
			exec(t, db, "DELETE FROM "+spec.name+" WHERE "+spec.key[0]+" = 'b'")
			write(t, db, "c")

			again, _ := exportedIDs(t, db, spec, mark)
			if !slices.Equal(again, []string{"c"}) {
				t.Fatalf("after the newest row was deleted, the next one written "+
					"carried as %v past the mark %d, want [c] — a %s watermark that "+
					"is handed out again leaves it unpublished while this node "+
					"holds the seat", again, mark, spec.watermark)
			}
		})
	}
}

// THE DIARY, end to end through a real broker: the holder publishes the
// seat's notes; the expiry and the trim delete the newest of them — the trim
// the oldest never-retrieved note, which is where the fill used to re-file the
// last note it moved; then a new note is written and an old one filled. Both
// reach the changelog, and a node hydrating the seat from it holds both.
func TestANoteWrittenOrFilledAfterTheNewestWasSweptIsStillCarried(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	ctx := context.Background()
	holder := openStore(t)
	diary := learning.NewDiary(holder)
	now := time.Now().UTC()
	write := func(e learning.DiaryEntry) {
		t.Helper()
		e.AgentID, e.Source = seat.AgentID, "reflect"
		if err := diary.Write(ctx, e); err != nil {
			t.Fatalf("write %s: %v", e.ID, err)
		}
	}
	// keep is retrieved, so the trim keeps it; it has no vector yet.
	write(learning.DiaryEntry{ID: "keep", Kind: learning.DiaryLong,
		Content: "the release train is thursdays", CreatedAt: now.Add(-time.Hour)})
	diary.MarkRetrieved(ctx, []string{"keep"}, now)
	// expiring is short and past its deadline by the time the expiry runs.
	write(learning.DiaryEntry{ID: "expiring", Kind: learning.DiaryShort,
		Content: "deploy freeze until noon", CreatedAt: now.Add(-2 * time.Hour),
		TTLUntil: now.Add(time.Minute)})
	// trimmed is the oldest never-retrieved durable note, written last.
	write(learning.DiaryEntry{ID: "trimmed", Kind: learning.DiaryLong,
		Content: "an old fact nobody recalled", CreatedAt: now.Add(-3 * time.Hour)})

	syncer := syncerOn(t, holder, conn)
	if _, err := syncer.PublishOwn(ctx, seat.Handle); err != nil {
		t.Fatalf("first publish: %v", err)
	}

	if n, err := diary.Expire(ctx, now.Add(2*time.Minute)); err != nil || n != 1 {
		t.Fatalf("Expire = %d, %v; want the one short note", n, err)
	}
	if n, err := diary.TrimLong(ctx, 1); err != nil || n != 1 {
		t.Fatalf("TrimLong = %d, %v; want the never-retrieved note", n, err)
	}

	write(learning.DiaryEntry{ID: "fresh", Kind: learning.DiaryLong,
		Content: "reviews want tests first", CreatedAt: now})
	if filled, err := diary.FillEmbeddings(ctx, []learning.VectorFill{{
		ID: "keep", Vector: learning.Vector{Values: []float32{0.6, 0.8}, Model: "model-a"},
	}}); err != nil || filled != 1 {
		t.Fatalf("FillEmbeddings = %d, %v; want the one note", filled, err)
	}

	// The seat has no rows in the whole-each-cycle tables, so a second
	// publish is exactly what changed since the first.
	again, err := syncer.PublishOwn(ctx, seat.Handle)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if again.Rows != 2 {
		t.Fatalf("the second publish carried %d rows, want the new note and the "+
			"filled one", again.Rows)
	}

	next := openStore(t)
	if _, err := syncerOn(t, next, conn).Hydrate(ctx, seat.Handle); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if got := countRows(t, next, "agent_diary"); got < 2 {
		t.Fatalf("the next holder hydrated %d notes", got)
	}
	var content string
	if err := next.SQL().QueryRowContext(ctx,
		"SELECT content FROM agent_diary WHERE id = 'fresh'").Scan(&content); err != nil {
		t.Fatalf("the note written after the sweep never reached the changelog: %v", err)
	}
	if blob, model := diaryVector(t, next, "keep"); len(blob) == 0 || model != "model-a" {
		t.Fatalf("the vector filled after the sweep never reached the changelog: "+
			"%d bytes in %q", len(blob), model)
	}
}

// changeIndexes names each watermark table's index on (seat, change_seq),
// node migration 0041's, which a page of the table is read through. One per
// table, so a table added to the registry without one fails below rather
// than going unchecked.
var changeIndexes = map[string]string{
	"agent_diary":                "agent_diary_agent_change_idx",
	"episodes":                   "episodes_agent_change_idx",
	"synthesized_skill_versions": "synthesized_skill_versions_agent_change_idx",
}

// THE INCREMENTAL EXPORT IS A RANGE SEEK, NOT A SORT.
//
// A page is read in watermark order until it holds its budget, and the read
// stops there, which saves the rest only when the plan returns rows in that
// order as it finds them: a seek on node migration 0041's (seat, change_seq)
// index, with no sort and no pass over the table. With a sort, the first row
// of every page waits on reading, and sorting, every row above the mark, and
// the page bounds nothing. Planned as a deployment plans it, over a store
// nothing has ANALYZEd, where episodes has four other indexes that lead on
// the seat, for both of a seat's ranges: a page of its own rows, above a mark,
// and a page of its backlog, between a mark and the seat's boundary.
//
// Mutation: read either range's page in key order, and this goes red.
func TestTheIncrementalExportIsARangeSeekNotASort(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	for _, spec := range tables {
		if spec.wholeEachCycle {
			continue
		}
		index, ok := changeIndexes[spec.name]
		if !ok {
			t.Errorf("%s is carried a page at a time and changeIndexes names no index for "+
				"it, so nothing here holds its page to a seek: add the one its rows are "+
				"read through", spec.name)
			continue
		}
		for _, page := range []struct {
			of        string
			statement string
			args      []any
		}{
			{"its own rows", pageStatement(spec), []any{seat.value(spec), int64(0)}},
			{"its backlog", backlogStatement(spec), []any{seat.value(spec), int64(0), int64(1) << 40}},
		} {
			plan := storetest.Plan(t, db, page.statement, page.args)
			if storetest.Sorts(plan) || storetest.ScansHeap(plan, spec.name) || !storetest.Seeks(plan, index) {
				t.Errorf("a page of %s of %s is planned as %s, want a seek on %s with no sort: a "+
					"page that sorts reads, and sorts, every row of its range above the mark "+
					"before it returns its first", page.of, spec.name, strings.Join(plan, "; "), index)
			}
		}
	}
}

// writtenTogether writes two rows of a watermark table for the seat, under the
// ids a and b, in ONE multi-row INSERT: one statement, so the two stamps are
// taken inside it, one trigger run per row. One per table, so a table added to
// the registry without one fails below rather than going unchecked.
var writtenTogether = map[string]func(t *testing.T, db *store.DB){
	"agent_diary": func(t *testing.T, db *store.DB) {
		t.Helper()
		// AT ONE INSTANT, as a turn's notes are: a watermark over the
		// creation time would hand both the same value.
		exec(t, db, `INSERT INTO agent_diary (id, agent_id, kind, content, source, created_at)
			VALUES ('a', ?, 'diary_long', 'a', 'reflect', 7),
			       ('b', ?, 'diary_long', 'b', 'reflect', 7)`, seat.AgentID, seat.AgentID)
	},
	"episodes": func(t *testing.T, db *store.DB) {
		t.Helper()
		exec(t, db, `INSERT INTO episodes (id, agent_handle, agent_role, turn_id,
			started_at, ended_at, plan_summary, task_summary, tool_sequence,
			review_outcome, duration_ms, kind)
			VALUES ('a', ?, 'Engineer', 't1', 7, 7, 'p', 't', '[]', 'done', 1, 'raw'),
			       ('b', ?, 'Engineer', 't2', 7, 7, 'p', 't', '[]', 'done', 1, 'raw')`,
			seat.Handle, seat.Handle)
	},
	"synthesized_skill_versions": func(t *testing.T, db *store.DB) {
		t.Helper()
		exec(t, db, `INSERT INTO synthesized_skills (id, agent_handle, name,
			description, content, frontmatter, tool_sequence, source_episode_ids,
			version, created_at, updated_at, state)
			VALUES ('s-wm', ?, 'ship-it', 'how', 'body', '{}', '[]', '[]', 1, 0, 0, 'active')
			ON CONFLICT DO NOTHING`, seat.Handle)
		exec(t, db, `INSERT INTO synthesized_skill_versions (id, skill_id,
			agent_handle, name, description, content, version, refinement_kind,
			archived_at)
			VALUES ('a', 's-wm', ?, 'ship-it', 'how', 'body', 1, 'seed', 0),
			       ('b', 's-wm', ?, 'ship-it', 'how', 'body', 2, 'seed', 0)`,
			seat.Handle, seat.Handle)
	},
}

// ROWS WRITTEN TOGETHER CARRY DISTINCT WATERMARKS.
//
// A mark per acknowledged row is sound only while no two rows of a table share
// a watermark value above 0: a page boundary or a cut between two that did
// would leave the mark at their value, and the next read, strictly above it,
// would never publish the second while this node holds the seat. A mark per
// table could not split such a pair inside one export; pages and a mark per
// row can. So every way a row is stamped is held here: two rows of one
// statement, a vector filled in place, and a row a hydration imports.
//
// Mutation: point the diary's watermark at created_at, and this goes red.
func TestRowsWrittenTogetherCarryDistinctWatermarks(t *testing.T) {
	t.Parallel()
	for _, spec := range tables {
		if spec.wholeEachCycle {
			continue
		}
		t.Run(spec.name, func(t *testing.T) {
			t.Parallel()
			write, ok := writtenTogether[spec.name]
			if !ok {
				t.Fatalf("%s is carried a page at a time and writtenTogether has no writer "+
					"for it, so nothing here checks that its rows never share a watermark "+
					"value: add one", spec.name)
			}
			db := openStore(t)
			write(t, db)
			if spec.fillsVector {
				exec(t, db, "UPDATE "+spec.name+" SET embedding = ?, embedding_model = 'model-a' "+
					"WHERE id = 'a'", []byte{1, 2, 3, 4})
			}
			peer := openStore(t)
			appendOnlyRow[spec.name](t, peer, "c")
			carry(t, peer, db)

			rows, err := db.SQL().QueryContext(t.Context(),
				"SELECT id, "+spec.watermark+" FROM "+spec.name+" WHERE "+spec.seatCol+" = ?",
				seat.value(spec))
			if err != nil {
				t.Fatalf("read %s's watermarks: %v", spec.name, err)
			}
			defer func() { _ = rows.Close() }()
			holder := map[int64]string{}
			read := 0
			for rows.Next() {
				var id string
				var value int64
				if err := rows.Scan(&id, &value); err != nil {
					t.Fatalf("scan %s's watermarks: %v", spec.name, err)
				}
				read++
				if other, shared := holder[value]; shared && value > 0 {
					t.Errorf("%s's rows %s and %s share the %s value %d", spec.name, other, id,
						spec.watermark, value)
				}
				holder[value] = id
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("read %s's watermarks: %v", spec.name, err)
			}
			if read != 3 {
				t.Fatalf("%s holds %d of the seat's rows, want the 3 written here", spec.name, read)
			}
		})
	}
}
