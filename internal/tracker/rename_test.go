package tracker_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/seatnames"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// renamed is a chart in which each seat answers to one handle and was created
// under another — [tracker.Identities] over a rename, with nothing else of a
// chart in it.
type renamed map[string]string // current handle → the handle it was created under

func (r renamed) Identity(handle string) string {
	if origin, ok := r[handle]; ok {
		return origin
	}
	return handle
}

func (r renamed) Current(identity string) string {
	for current, origin := range r {
		if origin == identity {
			return current
		}
	}
	return identity
}

// Pin implements [tracker.Identities]: the map IS the chart, so a test that
// renames a seat by writing to it is read by every call after.
func (r renamed) Pin() seatnames.Chart { return r }

// renaming is a chart a rename lands on every time somebody reads it: the
// seat created as `cto` answers to `chief` on the first reading, `boss` on the
// second, and so on — so two readings inside one call name it two ways.
type renaming struct{ readings atomic.Int32 }

func (r *renaming) Pin() seatnames.Chart {
	names := []string{"chief", "boss", "head", "lead"}
	n := int(r.readings.Add(1)) - 1
	return renamed{names[n%len(names)]: "cto"}
}

// AN ANSWER NAMES EVERY SEAT FROM ONE READING OF THE CHART.
//
// A grouped board names the same seat twice — as the column key and as the
// assignee on every card beneath it — in two passes over the answer. Each pass
// asked the live chart afresh, so a rename landing between them drew `chief`'s
// cards under a column keyed `boss`, and a board matching a card to its column
// by the key it drew found none. A call now takes ONE reading and names every
// person in its question and its answer by it.
//
// Mutation: take a reading per pass (the column keys by one, the rows by
// another) and the column no longer matches its cards; take one per helper and
// the call reads the chart more than once.
func TestAnAnswerNamesEverySeatFromOneReadingOfTheChart(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	routeTo(t, r, "one", "ENG-1", "cto")
	routeTo(t, r, "two", "ENG-2", "cto")
	r.drain()

	chart := &renaming{}
	r.reader.Identities = chart
	answer, err := r.reader.Tasks(t.Context(), tracker.Query{
		GroupBy: "assignee", Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if got := chart.readings.Load(); got != 1 {
		t.Errorf("one call read the chart %d times, want once", got)
	}
	matched := false
	for _, group := range answer.Groups {
		for _, row := range group.Rows {
			if row.Assignee != group.Key {
				t.Errorf("%s is drawn as %q's card under the column keyed %q",
					row.ID, row.Assignee, group.Key)
			}
			matched = matched || row.Assignee == "chief"
		}
	}
	if !matched {
		t.Errorf("no card names the seat as the call's one reading calls it: %+v",
			answer.Groups)
	}
}

// A WRITE NAMES EVERY SEAT FROM ONE READING OF THE CHART, as an answer does.
//
// A write resolves people in more than one place — its payload before the
// decide, a notice inside it, a lead's wake worded with the lead's name — and
// each asked the chart afresh, so one write could resolve its patch by one
// chart and word its wake by the next. It now holds one reading, and a write
// that funnels into another (a comment edit into a task update) keeps it.
//
// Mutation: drop the pin from UpdateTask's funnel, from WritePriorities or
// from EditComment, or make pinned() take a fresh reading when one is held.
func TestAWriteNamesEverySeatFromOneReadingOfTheChart(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.applyWhileWriting()
	ctx := t.Context()
	routeTo(t, r, "held", "ENG-1", "bob")
	ada := r.writer.As("ada", tracker.AuthorHuman, tracker.Provenance{})
	if _, err := ada.UpdateTask(ctx, "op-remark", "held", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-1", Task: "held", Author: "ada", AuthorKind: tracker.AuthorHuman,
			Body: "first", CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("ada's comment: %v", err)
	}
	r.drain()

	chart := &renaming{}
	r.writer.Identities = chart
	ada = r.writer.As("ada", tracker.AuthorHuman, tracker.Provenance{})
	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
	for _, write := range []struct {
		name string
		do   func() error
	}{
		{"a patch", func() error {
			_, err := ada.UpdateTask(ctx, "op-collab", "held", "ENG",
				tracker.NoIfMatch, tracker.TaskPatch{Collaborators: &[]string{"cto"}},
				tracker.ChangeCollaborators, nil)
			return err
		}},
		{"a lead's order of somebody's queue", func() error {
			_, err := ada.WritePriorities(ctx, "op-prio", "bob", []string{"held"},
				tracker.PersonAuthority{Authorized: true})
			return err
		}},
		{"an edit of one's own remark", func() error {
			_, err := ada.EditComment(ctx, "op-edit", "held", "ENG", "cm-1",
				"second", nil)
			return err
		}},
		{"a refused edit of somebody else's remark", func() error {
			_, err := bob.EditComment(ctx, "op-edit-bob", "held", "ENG", "cm-1",
				"third", nil)
			if err == nil {
				return errors.New("bob rewrote ada's remark")
			}
			return nil
		}},
	} {
		before := chart.readings.Load()
		if err := write.do(); err != nil {
			t.Fatalf("%s: %v", write.name, err)
		}
		if got := chart.readings.Load() - before; got != 1 {
			t.Errorf("%s read the chart %d times, want once", write.name, got)
		}
	}
}

// A RENAMED SEAT KEEPS ITS WORK, ITS INBOX, ITS QUEUE AND ITS PINS.
//
// Every tracker column that names somebody stored the handle they answered to
// at the write, and `my_work`, `work_inbox`, a person's record and every
// filter asked for the handle they answered to at the read — so a seat renamed
// from `cto` to `chief` opened an empty day: its assignment, the notice that
// told it so, its own priority list and its pinned views all sat under an
// address nobody asked for, and its own comment was no longer its own to edit.
// A seat's identity is the handle it was CREATED under (ADR-0019), and that is
// what every one of those is written and read by now, and shown as the handle
// the seat answers to.
//
// The first half runs before the rename, exactly as a company that never
// renamed anybody does — the identity IS the handle, which is why nothing
// already written needs migrating.
//
// Mutation: drop the question's rewrite in any reader wrapper and the renamed
// seat's day is empty; drop the answer's and it is shown as `cto`; drop the
// writer's rewrite of a task or a patch and work filed at `chief` after the
// rename is stored under a handle no read asks for; drop it from Writer.As
// and the seat may not edit its own comment.
func TestARenamedSeatKeepsItsWorkInboxQueueAndPins(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// A GESTURE HERE WRITES ONE SUBJECT TWICE — a person's record, a task
	// and its comment — so the node applies while it writes, as a running
	// one does.
	r.applyWhileWriting()
	ctx := t.Context()
	chart := renamed{}
	r.writer.Identities, r.reader.Identities = chart, chart
	cto := r.writer.As("cto", tracker.AuthorAgent, tracker.Provenance{})

	// BEFORE: work at cto, a notice for it, cto's own queue, pins and a
	// comment of its own, and a task it watches without holding.
	routeTo(t, r, "held", "ENG-1", "cto")
	if _, err := cto.WritePriorities(ctx, "op-prio", "cto", []string{"held"},
		tracker.PersonAuthority{}); err != nil {
		t.Fatalf("cto's own priorities: %v", err)
	}
	if _, err := cto.WritePins(ctx, "op-pins", "cto", []string{"v-board"}, nil,
		tracker.PersonAuthority{}); err != nil {
		t.Fatalf("cto's own pins: %v", err)
	}
	if _, err := cto.UpdateTask(ctx, "op-remark", "held", "ENG", tracker.NoIfMatch,
		tracker.TaskPatch{Comment: &tracker.Comment{
			ID: "cm-1", Task: "held", Author: "cto", AuthorKind: tracker.AuthorAgent,
			Body: "taking this", CreatedAt: wednesday,
		}}, tracker.ChangeComment, nil); err != nil {
		t.Fatalf("cto's comment: %v", err)
	}
	watched := newTask("watched")
	watched.Key, watched.Assignee, watched.Watchers = "ENG-2", "bob", []string{"cto"}
	if _, err := r.writer.CreateTask(ctx, "op-watched", watched, nil); err != nil {
		t.Fatalf("a task cto watches: %v", err)
	}
	r.drain()

	// THE RENAME. Nothing is rewritten: the rows still say `cto`.
	chart["chief"] = "cto"
	chief := r.writer.As("chief", tracker.AuthorAgent, tracker.Provenance{})

	// AND AFTER IT, work filed at the handle the seat answers to now, and a
	// patch bringing the seat onto a colleague's task by that handle.
	routeTo(t, r, "after", "ENG-3", "chief")
	if _, err := r.writer.UpdateTask(ctx, "op-collab", "watched", "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Collaborators: &[]string{"chief"}},
		tracker.ChangeCollaborators, nil); err != nil {
		t.Fatalf("bring chief onto bob's task: %v", err)
	}
	r.drain()

	day := r.myWork("chief")
	if day.Handle != "chief" {
		t.Errorf("the day is %q's, want the handle the seat answers to", day.Handle)
	}
	if got := rowIDs(day.Assigned); !slices.Equal(got, []string{"after", "held"}) &&
		!slices.Equal(got, []string{"held", "after"}) {
		t.Fatalf("chief's assignments are %v, want the work held before the "+
			"rename and the work filed after it", got)
	}
	for _, row := range day.Assigned {
		if row.Assignee != "chief" {
			t.Errorf("%s is shown as held by %q, want chief", row.ID, row.Assignee)
		}
	}
	if got := rowIDs(day.Priorities); !slices.Equal(got, []string{"held"}) {
		t.Errorf("chief's priorities are %v, want the list it set as cto", got)
	}
	if got := rowIDs(day.WatchingRecent); !slices.Equal(got, []string{"watched"}) {
		t.Errorf("chief is watching %v, want the task it watched as cto", got)
	}
	if got := rowIDs(day.Collaborating); !slices.Equal(got, []string{"watched"}) {
		t.Errorf("chief collaborates on %v, want the task it was brought onto "+
			"as chief", got)
	}

	inbox := r.inbox(tracker.InboxQuery{Handle: "chief"})
	if len(inbox.Notices) != 2 {
		t.Fatalf("chief's inbox holds %d notices, want the one written before "+
			"the rename and the one after", len(inbox.Notices))
	}
	if inbox.Handle != "chief" {
		t.Errorf("the inbox is %q's, want chief", inbox.Handle)
	}

	// ONE PERSON RECORD, whichever handle a write named it by: a pin set
	// as chief replaces the pins set as cto rather than starting a second
	// record nobody reads.
	if _, err := chief.WritePins(ctx, "op-pins-after", "chief",
		[]string{"v-board", "v-mine"}, nil, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("chief's own pins: %v", err)
	}
	r.drain()
	person := r.person("chief")
	if !slices.Equal(person.PinnedViews, []string{"v-board", "v-mine"}) ||
		!slices.Equal(person.Priorities, []string{"held"}) {
		t.Errorf("chief's record pins %v and prioritises %v — want both of its "+
			"own writes on the one record", person.PinnedViews, person.Priorities)
	}

	// A FILTER BY THE CURRENT HANDLE finds the seat's work, and shows it so.
	answer, err := r.reader.Tasks(ctx, tracker.Query{
		Assignee: []string{"chief"}, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	if got := rowIDs(answer.Rows); len(got) != 2 {
		t.Errorf("assignee=chief matched %v, want both tasks the seat holds", got)
	}

	// ITS OWN REMARK IS STILL ITS OWN.
	if _, err := chief.EditComment(ctx, "op-edit", "held", "ENG", "cm-1",
		"taking this — ETA friday", nil); err != nil {
		t.Errorf("chief may not edit the comment it wrote as cto: %v", err)
	}

	// AND THE ROWS ARE KEYED BY THE IDENTITY, which is the claim the rest
	// rests on: the work filed at `chief` is stored under `cto`, where no
	// later rename can move it.
	stored := map[string]string{}
	if err := r.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, assignee FROM tracker_tasks WHERE id IN ('held', 'after')`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, assignee string
			if err := rows.Scan(&id, &assignee); err != nil {
				return err
			}
			stored[id] = assignee
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the rows: %v", err)
	}
	for _, id := range []string{"held", "after"} {
		if stored[id] != "cto" {
			t.Errorf("%s is stored as held by %q, want cto — the handle the "+
				"seat was created under", id, stored[id])
		}
	}
}

// A PURGE BY A RENAMED PERSON NAMES THEM AS THEY ARE CALLED NOW.
//
// The purge's excerpt is prose the project lead reads and nothing rewrites
// afterwards, and the writer's actor is the seat's IDENTITY — so built from it
// the lead was told the task was "purged by cto" by a colleague who answers to
// `chief`, a name the lead cannot find on the chart. The priorities wake's own
// excerpt already named the current handle; this is the same rule.
//
// Mutation: hand purgeWake the writer's actor as it is and the excerpt names
// cto.
func TestAPurgeByARenamedPersonNamesThemAsTheyAreCalledNow(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	filedTask(t, r, "t-1")
	chart := renamed{"chief": "cto"}
	r.writer.Identities, r.reader.Identities = chart, chart
	r.writer.Leads = fixedLeads{project: "eng-lead"}

	chief := r.writer.As("chief", tracker.AuthorHuman, tracker.Provenance{})
	if _, err := chief.PurgeTask(t.Context(), "op-purge", "t-1", "ENG",
		"asked for by legal"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	excerpt := historyExcerpt(t, r, "task", "t-1")
	if !strings.Contains(excerpt, "purged by chief") || strings.Contains(excerpt, "cto") {
		t.Errorf("the purge excerpt reads %q, want the purger named chief — the "+
			"handle the lead knows them by", excerpt)
	}
}

// rowIDs is the ids of a block, in its order.
func rowIDs(rows []tracker.TaskRow) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}
