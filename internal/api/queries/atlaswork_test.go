package queries_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// atlasWorkSources is acme's atlas beside a tracker that answers w.
func atlasWorkSources(w *stubWork) queries.Sources {
	s := atlasSources(atlasCode())
	s.Work = w
	return s
}

// THE WORK ON AN ENTRY IS WORKLET'S TO SHOW (ADR-0127): a holder of the atlas
// who does not use Worklet is told no work was read and why, and the tracker
// is never asked; one who does reads each entry's open work.
//
// Mutation: drop the Worklet check in workUnread and the first reader is
// handed the counts and the tasks.
func TestTheWorkOnAnEntryIsReadOnlyForAWorkletReader(t *testing.T) {
	t.Parallel()
	web := entryID(atlas.KindHost, "ssh/web-1")
	w := &stubWork{marks: worklet.EntryMarks{Open: map[string]int{web: 2}},
		entryWork: worklet.EntryWork{Open: []worklet.TaskRow{{ID: "t-1", Key: "ENG-1", Title: "Patch web-1"}},
			Recent: []worklet.TaskRow{}, OpenTotal: worklet.ClaimTotal{Total: 1}}}
	s := atlasWorkSources(w)

	outsider := asAtlasReader(t)
	for what, params := range map[string]map[string]any{"atlas_work": nil, "atlas_entry_work": {"id": web}} {
		answer := askAtlas(outsider, t, s, what, params)
		encoded, _ := json.Marshal(answer)
		if reason, _ := answer["unread"].(string); !strings.Contains(reason, "Worklet") ||
			strings.Contains(string(encoded), "ENG-1") || strings.Contains(string(encoded), web) {
			t.Errorf("%s answered a reader without Worklet %s", what, encoded)
		}
	}
	if w.entryReads != 0 {
		t.Errorf("the tracker was read %d times for a reader without Worklet", w.entryReads)
	}

	holder := asAtlasReader(t, iam.GrantWorklet)
	marks := askAtlas(holder, t, s, "atlas_work", nil)
	if open, _ := marks["open"].(map[string]any); marks["unread"] != nil || open[web] != float64(2) {
		t.Errorf("a Worklet reader's marks read %v", marks)
	}
	work := askAtlas(holder, t, s, "atlas_entry_work", map[string]any{"id": web})
	if rows := listOf(t, work["open"]); len(rows) != 1 || rows[0]["key"] != "ENG-1" {
		t.Errorf("a Worklet reader's work on web-1 reads %v", work)
	}
	if len(w.entryQuery.Entries) != 1 || w.entryQuery.Entries[0] != web {
		t.Errorf("the tracker was asked for %v, want the entry's own id", w.entryQuery.Entries)
	}
}

// A MARK LANDS ON THE ENTRY THE MAP DRAWS: an id the asker's atlas does not
// hold marks nothing, so a task naming an entry that has left the atlas, or
// one the asker is not shown, says nothing about any other.
func TestAMarkOnAnEntryTheAtlasDoesNotHoldMarksNothing(t *testing.T) {
	t.Parallel()
	web := entryID(atlas.KindHost, "ssh/web-1")
	gone := entryID(atlas.KindHost, "ssh/web-9")
	s := atlasWorkSources(&stubWork{marks: worklet.EntryMarks{Open: map[string]int{web: 1, gone: 4, "not-an-id": 3}}})
	marks := askAtlas(asAtlasReader(t, iam.GrantWorklet), t, s, "atlas_work", nil)
	if open, _ := marks["open"].(map[string]any); len(open) != 1 || open[web] != float64(1) {
		t.Errorf("the marks read %v, want web-1 alone", marks["open"])
	}
}

// A COMPANY WHOSE WORK IS NOT IN WORKLET HAS NONE TO READ, said in the answer
// rather than failed; any other failure of the tracker is the question's.
func TestTheWorkOnAnEntrySaysWhereWorkletIsNotKept(t *testing.T) {
	t.Parallel()
	holder := asAtlasReader(t, iam.GrantWorklet)
	web := entryID(atlas.KindHost, "ssh/web-1")
	for name, s := range map[string]queries.Sources{
		"no tracker on this surface":            atlasSources(atlasCode()),
		"a tracker the company keeps elsewhere": atlasWorkSources(&stubWork{err: queries.ErrUnknown}),
	} {
		for what, params := range map[string]map[string]any{"atlas_work": nil, "atlas_entry_work": {"id": web}} {
			if reason, _ := askAtlas(holder, t, s, what, params)["unread"].(string); !strings.Contains(reason, "not kept") {
				t.Errorf("%s with %s says %q", what, name, reason)
			}
		}
	}
	r := queries.NewRegistry()
	queries.Register(r, atlasWorkSources(&stubWork{err: errors.New("the estate is behind")}))
	if _, err := r.Answer(holder, "atlas_work", nil); err == nil {
		t.Error("a tracker that failed answered as though it held no work")
	}
	if _, err := r.Answer(holder, "atlas_entry_work", map[string]any{"id": entryID(atlas.KindHost, "ssh/web-9")}); !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("the work on an entry the atlas does not hold: %v", err)
	}
}

// WHAT A LIST OF IDS NAMES IS ANSWERED ID BY ID, in the order asked and each
// once, with the ids that name no entry beside them: what a work item's chips
// are drawn from, and what lets a link be taken off by the id it was kept by.
func TestTheEntriesAListOfIDsNames(t *testing.T) {
	t.Parallel()
	web, db := entryID(atlas.KindHost, "ssh/web-1"), entryID(atlas.KindHost, "ssh/db-1")
	gone := entryID(atlas.KindHost, "ssh/web-9")
	s := atlasSources(atlasCode())
	answer := askAtlas(asAtlasReader(t), t, s, "atlas_named", map[string]any{"ids": db + ", " + gone + "," + web + "," + db})
	named := listOf(t, answer["named"])
	missing, _ := answer["missing"].([]any)
	if len(named) != 2 || named[0]["id"] != db || named[1]["id"] != web || len(missing) != 1 || missing[0] != gone {
		t.Fatalf("the ids name %v with %v missing, want db-1 then web-1 and web-9 missing", named, missing)
	}
	if entry := asMap(t, named[0]["entry"]); entry["id"] != db || entry["name"] != "db-1" {
		t.Errorf("db-1's id names %v", entry)
	}
	r := queries.NewRegistry()
	queries.Register(r, s)
	for name, ids := range map[string]string{"none": "", "a name": "web-1",
		"too many": strings.Repeat(web+",", worklet.MaxOtherRelations+1)} {
		if _, err := r.Answer(asAtlasReader(t), "atlas_named", map[string]any{"ids": ids}); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("ids naming %s: %v, want bad params", name, err)
		}
	}
}

// A READER WITHOUT THE ATLAS READS HOW MANY ENTRIES A TASK NAMES AND NONE OF
// THEM (ADR-0140): the id of an entry is derived from its kind and key, so it
// is withheld with the name it would confirm. A holder reads the ids.
//
// Mutation: drop the atlas check in workItem and the first reader is handed
// both ids.
func TestAWorkItemNamesItsAtlasEntriesOnlyToAnAtlasReader(t *testing.T) {
	t.Parallel()
	web, db := entryID(atlas.KindHost, "ssh/web-1"), entryID(atlas.KindHost, "ssh/db-1")
	relations := []worklet.Relation{{Kind: worklet.RelationEntry, Other: web},
		{Kind: worklet.RelationPage, Other: "s-1"}, {Kind: worklet.RelationEntry, Other: db}}
	w := &stubWork{detail: worklet.TaskDetail{Task: worklet.Task{ID: "t-1", Key: "ENG-1", Project: "ENG",
		Relations: relations}, Links: []worklet.DetailLink{{Kind: worklet.RelationEntry, Other: web},
		{Kind: worklet.RelationPage, Other: "s-1"}, {Kind: worklet.RelationEntry, Other: db}}}}
	ask := func(grants ...iam.Grant) (map[string]any, string) {
		t.Helper()
		r := queries.NewRegistry()
		queries.Register(r, queries.Sources{Work: w})
		answer, err := r.Answer(asGrants(t, "", grants...), "work_item", map[string]any{"id": "ENG-1"})
		if err != nil {
			t.Fatalf("work_item: %v", err)
		}
		encoded, err := json.Marshal(answer)
		if err != nil {
			t.Fatal(err)
		}
		return asMap(t, answer), string(encoded)
	}
	answer, encoded := ask(iam.GrantWorklet)
	if answer["entries_withheld"] != float64(2) || strings.Contains(encoded, web) || strings.Contains(encoded, db) ||
		len(listOf(t, answer["links"])) != 1 {
		t.Errorf("a reader without the atlas was answered %s", encoded)
	}
	answer, encoded = ask(iam.GrantWorklet, iam.GrantAtlas)
	if answer["entries_withheld"] != nil || !strings.Contains(encoded, web) || len(listOf(t, answer["links"])) != 3 {
		t.Errorf("an atlas reader was answered %s", encoded)
	}
}
