package engine

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcpreview"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// publishedQueue keeps every event published through it, and serves nothing
// else.
type publishedQueue struct {
	queue.EventQueue

	mu   sync.Mutex
	sent []*events.Event
}

func (q *publishedQueue) Publish(_ context.Context, _ string, ev *events.Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sent = append(q.sent, ev)
	return nil
}

// acted is every `mcp_server_acted` published, in order.
func (q *publishedQueue) acted() []types.MCPServerActed {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []types.MCPServerActed
	for _, ev := range q.sent {
		if rec, ok := ev.Data.(*types.MCPServerActed); ok {
			out = append(out, *rec)
		}
	}
	return out
}

// autoAcceptRig is an engine over company with first accepted as each server's
// first listing and now live, its published events kept.
func autoAcceptRig(t *testing.T, company *Company, first, now []nodeListing) (*Engine, *coordmemory.Fleet,
	*publishedQueue) {

	t.Helper()
	e, fleet := reviewingEngine(t, company)
	published := &publishedQueue{}
	e.backends.Queue = published
	desired := map[string]nodeListing{}
	for _, l := range first {
		desired[l.listing.Server] = l
	}
	e.baselineMCPTools(t.Context(), desired, time.Now())
	waitFor(t, "the first listings to be accepted", func() bool {
		index, _ := e.reviewIndex()
		for server := range desired {
			if !index.Baselined(server) {
				return false
			}
		}
		return true
	})
	setLive(e, now...)
	return e, fleet, published
}

// setLive makes listings the fleet's live listings as this node reads them.
func setLive(e *Engine, listings ...nodeListing) {
	e.listings.mu.Lock()
	defer e.listings.mu.Unlock()
	e.listings.answered, e.listings.presenceRead = true, true
	e.listings.fleet = map[string]coord.MCPListing{}
	nodes := map[string]struct{}{}
	for _, l := range listings {
		e.listings.fleet[coord.MCPListingKey(l.listing.Server, l.listing.Node)] = l.listing
		nodes[l.listing.Node] = struct{}{}
	}
	e.listings.presence = nil
	for node := range nodes {
		e.listings.presence = append(e.listings.presence, coord.Lease{Resource: coord.NodeResource(node), Epoch: 1})
	}
}

// reworded is l with tool's description, digest and parts changed.
func reworded(l nodeListing, tool, description string) nodeListing {
	l.listing.Tools = slices.Clone(l.listing.Tools)
	for i := range l.listing.Tools {
		if l.listing.Tools[i].Raw != tool {
			continue
		}
		digest := testDigest(l.listing.Server + "/" + tool + "/" + description)
		l.listing.Tools[i].Description = description
		l.listing.Tools[i].Digest = digest
		l.listing.Tools[i].Parts.Description = digest[:coord.MCPToolPartLength]
	}
	return l
}

// storedRecord is the accepted record of server's tool as the store holds it.
func storedRecord(t *testing.T, fleet *coordmemory.Fleet, server, tool string) coord.MCPAcceptedTool {
	t.Helper()
	held, err := fleet.ListMCPAccepted(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range held {
		if a.Server == server && a.Tool == tool {
			return a
		}
	}
	t.Fatalf("no accepted record of %s on %s", tool, server)
	return coord.MCPAcceptedTool{}
}

// A CHANGE TO A SERVER SET TO AUTO-ACCEPT IS ACCEPTED AS LISTED AND AUDITED
// WITH ITS DIFF, once, by the node (ADR-0078); a server beside it is not.
// Mutation: accept every server's changes or leave Auto off, and this is red.
func TestAChangeToAServerSetToAutoAcceptIsAcceptedAndAuditedWithItsDiff(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company := specCompany(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, tools: {auto_accept_changes: true}}
  - {name: docs, command: docs-mcp}
`)
	tracker := fakeListing("tracker", "node-a", 1, "close_items")
	docs := fakeListing("docs", "node-a", 1, "search")
	trackerNow := reworded(fakeListing("tracker", "node-a", 1, "close_items", "purge_items"),
		"close_items", "Close the items, and delete their history")
	docsNow := reworded(docs, "search", "Search every page")
	e, fleet, published := autoAcceptRig(t, company, []nodeListing{tracker, docs},
		[]nodeListing{trackerNow, docsNow})
	before := storedRecord(t, fleet, "tracker", "close_items")

	e.autoAcceptMCPChanges(ctx, time.Now())

	for _, tool := range []string{"close_items", "purge_items"} {
		got := storedRecord(t, fleet, "tracker", tool)
		want := listedDigest(trackerNow, tool)
		if got.Digest != want || !got.Auto || got.First || got.By != "node-a" {
			t.Errorf("%s is accepted as %+v, want %s accepted automatically by node-a", tool, got, want)
		}
	}
	if got := storedRecord(t, fleet, "docs", "search"); got.Digest != listedDigest(docs, "search") || got.Auto {
		t.Errorf("a server whose entry does not set auto_accept_changes had its change accepted: %+v", got)
	}
	acted := published.acted()
	if len(acted) != 1 {
		t.Fatalf("published %d mcp_server_acted, want one for tracker: %+v", len(acted), acted)
	}
	rec := acted[0]
	if rec.Action != types.MCPServerAutoAcceptChanges || rec.Server != "tracker" || rec.ActorName != "node-a" ||
		rec.ActorKind != "engine" || rec.Outcome != types.AuditApplied ||
		!slices.Equal(rec.Accepted, []string{"close_items", "purge_items"}) {
		t.Fatalf("the audit record is %+v, want the node's auto_accept_changes of both tools", rec)
	}
	changed, added := rec.Changes[0], rec.Changes[1]
	if changed.Kind != "changed" || !slices.Equal(changed.Parts, []string{"description"}) ||
		changed.Before == nil || changed.Before.Digest != before.Digest ||
		changed.Before.Description != "does close_items" || changed.After == nil ||
		changed.After.Description != "Close the items, and delete their history" ||
		changed.After.ReadOnly != coord.HintYes {
		t.Errorf("the changed tool's record is %+v (before %+v, after %+v), want its description's diff",
			changed, changed.Before, changed.After)
	}
	if added.Kind != "added" || added.Before != nil || added.After == nil ||
		added.After.Digest != listedDigest(trackerNow, "purge_items") {
		t.Errorf("the added tool's record is %+v, want it added with its definition", added)
	}
	if want := "accepted changed tools of MCP server tracker automatically (close_items: description; " +
		"purge_items: added)"; !strings.Contains(rec.SummaryFor("node-a"), want) {
		t.Errorf("the audit line is %q, want it to say %q", rec.SummaryFor("node-a"), want)
	}

	e.autoAcceptMCPChanges(ctx, time.Now())
	if n := len(published.acted()); n != 1 {
		t.Errorf("a second beat audited again (%d records): only a write that lands is audited", n)
	}
}

// AUTO-ACCEPT IS OFF UNLESS THE ENTRY SETS IT: its changed tool still asks
// first and stands on the condition after a beat, and nothing is audited.
func TestAutoAcceptIsOffUnlessTheEntrySetsIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company := specCompany(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, tools: {levels: {close_items: allow}}}
`)
	if company.Config.MCPServers[0].Tools.AutoAcceptChanges {
		t.Fatal("an entry that does not set tools.auto_accept_changes reads as on")
	}
	tracker := fakeListing("tracker", "node-a", 1, "close_items")
	now := reworded(tracker, "close_items", "Close and delete the items")
	e, fleet, published := autoAcceptRig(t, company, []nodeListing{tracker}, []nodeListing{now})

	e.autoAcceptMCPChanges(ctx, time.Now())

	if got := storedRecord(t, fleet, "tracker", "close_items"); got.Digest != listedDigest(tracker, "close_items") {
		t.Errorf("the change was accepted with auto-accept off: %+v", got)
	}
	if !e.toolUnreviewed("tracker", "close_items", listedDigest(now, "close_items")) {
		t.Error("the changed tool runs unasked with auto-accept off")
	}
	if acted := published.acted(); len(acted) != 0 {
		t.Errorf("published %+v with auto-accept off", acted)
	}
	conds := []statelog.MCPServerCondition{{Server: "tracker"}}
	e.changedConditions(ctx, company, conds)
	if conds[0].Changes != 1 {
		t.Errorf("the condition counts %d changes, want the one waiting on a person", conds[0].Changes)
	}
}

// A TOOL LISTED UNDER TWO DEFINITIONS IS LEFT FOR A PERSON and raised, while
// a change auto-accept will take raises nothing even before its beat.
func TestAToolListedUnderTwoDefinitionsIsLeftForAPerson(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	company := specCompany(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, tools: {default: allow, auto_accept_changes: true}}
`)
	tracker := fakeListing("tracker", "node-a", 1, "close_items", "list_items")
	onA := reworded(reworded(tracker, "close_items", "Close the items now"), "list_items", "List every item")
	onB := reworded(fakeListing("tracker", "node-b", 1, "close_items", "list_items"),
		"close_items", "Close the items later")
	onB = reworded(onB, "list_items", "List every item")
	e, fleet, published := autoAcceptRig(t, company, []nodeListing{tracker}, []nodeListing{onA, onB})

	conds := []statelog.MCPServerCondition{{Server: "tracker"}}
	e.changedConditions(ctx, company, conds)
	if conds[0].Changes != 2 || conds[0].Change.Tool != "close_items" {
		t.Errorf("before the beat the condition is %+v, want close_items' two definitions alone", conds[0])
	}

	e.autoAcceptMCPChanges(ctx, time.Now())

	if got := storedRecord(t, fleet, "tracker", "list_items"); got.Digest != listedDigest(onA, "list_items") ||
		!got.Auto {
		t.Errorf("a tool listed under one definition is %+v, want it accepted automatically", got)
	}
	if got := storedRecord(t, fleet, "tracker", "close_items"); got.Digest != listedDigest(tracker, "close_items") {
		t.Errorf("a tool listed under two definitions was accepted as %+v, want it left for a person", got)
	}
	if acted := published.acted(); len(acted) != 1 || !slices.Equal(acted[0].Accepted, []string{"list_items"}) {
		t.Errorf("the audit is %+v, want list_items alone", acted)
	}
	held, err := fleet.ListMCPAccepted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stored := mcpreview.IndexOf(held)["tracker"]
	if got := mcpreview.AutoAccepts(mcpreview.Changes([]coord.MCPListing{onA.listing, onB.listing},
		stored)); len(got) != 0 {
		t.Errorf("AutoAccepts takes %+v of one tool's two definitions, want none", got)
	}
}

// listedDigest is the digest l lists tool under.
func listedDigest(l nodeListing, tool string) string {
	for _, t := range l.listing.Tools {
		if t.Raw == tool {
			return t.Digest
		}
	}
	return ""
}
