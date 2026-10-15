package artifactdata_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// THREE PEOPLE AND AN AGENT EDIT ONE SHEET AT ONCE, THROUGH TWO NODES, AND
// EVERY DATA NODE CONVERGES, byte for byte, with a third node replaying the log
// from nothing rebuilding the same estate: the Phase 2 exit criterion, held
// across ticks, chips, counters, guarded edits, deletes and a refused write.
//
// Mutation: write the applying node's own clock into any row (time.Now for an
// updated_at), or encode a body from an unsorted map, and the nodes' tables
// differ.
func TestEditorsOnTwoNodesConvergeAndAReplayRebuildsTheirEstate(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	a := newNode(t, log, openStore(t, "a.db"), "node-a")
	b := newNode(t, log, openStore(t, "b.db"), "node-b")
	owner := person("jane", artifacts.RoleOwner)
	a.configure(owner, reviewSheet(), artifacts.Collection{Name: "votes", Preset: artifacts.PresetContribute})
	for i := range 6 {
		reviewer := []string{"bob", "sam", "dana"}[i%3]
		a.applied(owner, set("review", fmt.Sprintf("row-%d", i), map[string]string{
			"title": fmt.Sprintf(`"Item %d"`, i), "reviewer": `"` + reviewer + `"`}))
	}
	b.drain()

	writers := []struct {
		n *node
		c func() (string, artifacts.Role, bool)
	}{
		{a, func() (string, artifacts.Role, bool) { return "bob", artifacts.RoleContributor, false }},
		{b, func() (string, artifacts.Role, bool) { return "sam", artifacts.RoleContributor, false }},
		{a, func() (string, artifacts.Role, bool) { return "jane", artifacts.RoleEditor, false }},
		{b, func() (string, artifacts.Role, bool) { return "agent-iris", artifacts.RoleContributor, true }},
	}
	var wg sync.WaitGroup
	failures := make(chan error, 64)
	for _, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle, role, isAgent := w.c()
			c := person(handle, role)
			if isAgent {
				c = agent(handle, role)
			}
			ctx := context.Background()
			for i := range 6 {
				row := fmt.Sprintf("row-%d", i)
				ops := []artifacts.DataOp{
					field(artifacts.DataIncrement, "votes", "tally", "count", `1`),
				}
				switch handle {
				case "jane":
					ops = append(ops, field(artifacts.DataAdd, "review", row, "labels", `"copy"`),
						merge("review", row, map[string]string{"status": `"done"`}))
				case "agent-iris":
					// AN AGENT is refused the approval and still counts.
					ops = []artifacts.DataOp{merge("review", row, map[string]string{"approved": `true`})}
				default:
					ops = append(ops[:0], merge("review", row, map[string]string{"done": `true`}))
				}
				if _, err := w.n.writer.Write(ctx, c, sheet, artifacts.DataWrite{Ops: ops}); err != nil {
					failures <- fmt.Errorf("%s write %d: %w", handle, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	a.drain()
	b.drain()
	replay := newNode(t, log, openStore(t, "replay.db"), "node-c")
	replay.drain()
	for _, table := range replicatedTables() {
		want := a.table(table)
		if got := b.table(table); got != want {
			t.Errorf("%s differs between the two writing nodes:\n a: %s\n b: %s", table, want, got)
		}
		if got := replay.table(table); got != want {
			t.Errorf("%s differs on the node that replayed the log:\n a: %s\n c: %s", table, want, got)
		}
	}
	if a.table("artifact_data_docs") == "" || a.count(`SELECT COUNT(*) FROM artifact_data_writes`) < 20 {
		t.Fatal("the compared estate is nearly empty, so the comparison proves little")
	}
	// EVERY TICK LANDED WHERE ITS RULE ADMITS IT, and the agent's approvals
	// none.
	done := a.count(`SELECT COUNT(*) FROM artifact_data_writes WHERE verdict = 'applied' AND actor IN ('bob', 'sam')`)
	if done != 4 {
		t.Errorf("%d contributor ticks applied, want the 4 on rows assigned to bob and sam", done)
	}
	if n := a.count(`SELECT COUNT(*) FROM artifact_data_writes WHERE actor = 'agent-iris' AND verdict = 'applied'`); n != 0 {
		t.Errorf("%d of the agent's approvals applied", n)
	}
	computed, stored := a.digestFromRows(sheet)
	if computed != stored {
		t.Fatal("the converged store's digest is not the sum of its rows")
	}
}
