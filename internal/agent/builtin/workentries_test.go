package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The two servers acme's settings declare, by their atlas ids.
var (
	entryWeb = atlas.MustRef(atlas.KindHost, "ssh/web-1").ID().String()
	entryDB  = atlas.MustRef(atlas.KindHost, "ssh/db-1").ID().String()
)

// runsAcmeServers is acme's settings with two servers, so its atlas holds two
// entries a task can name.
func runsAcmeServers(context.Context) (builtin.AtlasSettings, bool) {
	s := atlasCompany()
	s.Company.Integrations.SSH = &config.SSH{Enabled: true, Hosts: []config.SSHHost{
		{ID: "web-1", Address: "9.9.9.9"}, {ID: "db-1", Address: "10.0.0.5"}}}
	return s, true
}

// entrySurface is the operator's catalogue over trk and acme's atlas, deciding
// as every surface decides.
func entrySurface(t *testing.T, trk *fakeTracker) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work:      builtin.WorkDeps{Reader: trk, Writer: trk.as, Actor: builtin.PrincipalActor},
		Atlas:     builtin.AtlasDeps{Settings: runsAcmeServers},
		Authorize: builtin.Decide(chartLeads),
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// callEntries makes one call of tool as the person ctx carries, in no turn.
func callEntries(ctx context.Context, t *testing.T, reg *tools.Registry, tool string, args map[string]any) tools.Result {
	t.Helper()
	entry, ok := reg.Lookup(tool)
	if !ok {
		t.Fatalf("%s is not registered", tool)
	}
	got, err := entry.Tool.Call(ctx, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return got
}

// entriesOf is the atlas entries a relation gesture adds and removes.
func entriesOf(t *testing.T, patch worklet.TaskPatch) (added, removed []string) {
	t.Helper()
	if patch.Relate == nil {
		t.Fatal("the call wrote no relation gesture")
	}
	for _, r := range patch.Relate.Add {
		if r.Kind == worklet.RelationEntry {
			added = append(added, r.Other)
		}
	}
	for _, r := range patch.Relate.Remove {
		if r.Kind == worklet.RelationEntry {
			removed = append(removed, r.Other)
		}
	}
	return added, removed
}

// A TASK NAMES AN ENTRY BY ITS ID, WHATEVER THE CALLER CALLED IT: a name, a
// key or an id is resolved through the caller's own read of the atlas, and
// what reaches the writer is the entry's id (ADR-0140). A whole set is a
// delta over the entries alone, so the item's other edges stay.
func TestATaskIsLinkedToTheAtlasEntriesItTouches(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	detail := trk.tasks["ENG-1"]
	detail.Task.Relations = []worklet.Relation{{Kind: worklet.RelationEntry, Other: entryDB},
		{Kind: worklet.RelationPage, Other: "s-1"}}
	trk.tasks["ENG-1"] = detail
	reg := entrySurface(t, trk)
	holder := personBoundTo("jane", iam.GrantWorklet, iam.GrantAtlas)

	got := callEntries(holder, t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "linked_entries": map[string]any{"add": []any{"web-1"}}})
	if got.Failed || len(trk.patched) != 1 {
		t.Fatalf("linking web-1 by its name answered %q with %d writes", got.Output, len(trk.patched))
	}
	if added, removed := entriesOf(t, trk.patched[0]); !slices.Equal(added, []string{entryWeb}) || len(removed) != 0 {
		t.Errorf("the write adds %v and removes %v, want web-1's id added", added, removed)
	}
	if trk.kinds[0] != worklet.ChangeRelations {
		t.Errorf("the write is a %s change, want a relations one", trk.kinds[0])
	}

	got = callEntries(holder, t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "linked_entries": map[string]any{"set": []any{entryWeb}}})
	if got.Failed || len(trk.patched) != 2 {
		t.Fatalf("setting the entries answered %q", got.Output)
	}
	added, removed := entriesOf(t, trk.patched[1])
	if !slices.Equal(added, []string{entryWeb}) || !slices.Equal(removed, []string{entryDB}) {
		t.Errorf("the set adds %v and removes %v, want web-1 in and db-1 out", added, removed)
	}
	if slices.ContainsFunc(trk.patched[1].Relate.Remove, func(r worklet.Relation) bool {
		return r.Kind != worklet.RelationEntry
	}) {
		t.Errorf("a set of entries removed another kind of edge: %+v", trk.patched[1].Relate.Remove)
	}

	for name, ref := range map[string]string{"no entry's name": "orders-db",
		"no entry's id": atlas.MustRef(atlas.KindHost, "ssh/web-9").ID().String()} {
		got = callEntries(holder, t, reg, builtin.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", "linked_entries": map[string]any{"add": []any{ref}}})
		if !got.Failed || !strings.Contains(got.Output, "no entry of the atlas") {
			t.Errorf("linking %s answered %q, want a refusal", name, got.Output)
		}
	}
	if len(trk.patched) != 2 {
		t.Errorf("a refused link still wrote: %d writes", len(trk.patched))
	}
}

// NOBODY LINKS WHAT THEY MAY NOT READ, AND NOBODY IS HANDED WHAT THEY MAY NOT
// READ: a Worklet holder without the atlas is refused a link, naming the
// atlas's app, and reads the task with its entries counted and none named.
// They may still take a link off by its id, which reads nothing.
//
// Mutation: drop the atlas decision in entryRelations and the link is
// written; drop readsAtlas in get_work_item and the answer carries the id.
func TestLinkingAndReadingATasksEntriesTakesTheAtlas(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	detail := trk.tasks["ENG-1"]
	detail.Task.Relations = []worklet.Relation{{Kind: worklet.RelationEntry, Other: entryDB}}
	detail.Links = []worklet.DetailLink{{Kind: worklet.RelationEntry, Other: entryDB}}
	trk.tasks["ENG-1"] = detail
	reg := entrySurface(t, trk)
	outsider := personBoundTo("jane", iam.GrantWorklet)

	for _, ref := range []string{"web-1", entryWeb} {
		got := callEntries(outsider, t, reg, builtin.UpdateWorkItemTool, map[string]any{
			"item": "ENG-1", "linked_entries": map[string]any{"add": []any{ref}}})
		refused, ok := errors.AsType[*builtin.DecisionRefused](got.Cause)
		if !got.Failed || !ok || !slices.Equal(refused.Decision.Grants, []iam.Grant{iam.GrantAtlas}) {
			t.Errorf("linking %q without the atlas answered %q (%v), want a refusal naming the atlas app",
				ref, got.Output, got.Cause)
		}
	}
	if len(trk.patched) != 0 {
		t.Fatalf("a refused link wrote %d times", len(trk.patched))
	}

	read := callEntries(outsider, t, reg, builtin.GetWorkItemTool, map[string]any{"item": "ENG-1",
		"include": []any{"links"}})
	if read.Failed || strings.Contains(read.Output, entryDB) || !strings.Contains(read.Output, `"entries_withheld": 1`) {
		t.Errorf("a reader without the atlas was answered %q", read.Output)
	}
	held := callEntries(personBoundTo("jane", iam.GrantWorklet, iam.GrantAtlas), t, reg, builtin.GetWorkItemTool,
		map[string]any{"item": "ENG-1", "include": []any{"links"}})
	if held.Failed || !strings.Contains(held.Output, entryDB) || strings.Contains(held.Output, "entries_withheld") {
		t.Errorf("an atlas reader was answered %q", held.Output)
	}

	got := callEntries(outsider, t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "linked_entries": map[string]any{"remove": []any{strings.ToUpper(entryDB)}}})
	if got.Failed || len(trk.patched) != 1 {
		t.Fatalf("taking a link off by its id answered %q", got.Output)
	}
	if added, removed := entriesOf(t, trk.patched[0]); len(added) != 0 || !slices.Equal(removed, []string{entryDB}) {
		t.Errorf("the write adds %v and removes %v, want db-1's id removed in its canonical spelling", added, removed)
	}
	if encoded, _ := json.Marshal(trk.patched[0]); strings.Contains(string(encoded), "db-1") {
		t.Errorf("the write carries an entry's name: %s", encoded)
	}
}
