package config

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
)

// limitsDoc is a company whose `eng` unit turns `search` off and holds two of
// `tracker`'s tools back, inside which `backend` holds one tighter, and whose
// dev holds a tool of `docs` back on its own. The entry blocks wipe_items.
const limitsDoc = `
name: Acme
providers: {sandbox: {fake: true}}
mcp_servers:
  - {name: search, command: search-mcp, tools: {default: allow}}
  - {name: docs, command: docs-mcp, shared: false, tools: {default: allow}}
  - name: tracker
    command: tracker-mcp
    tools: {default: allow, levels: {wipe_items: blocked}}
roles:
  - {name: CEO, handle: ceo}
units:
  - name: Eng
    id: eng
    mcp_limits:
      search: {enabled: false}
      tracker: {tools: {close_items: approve, delete_items: approve}}
    roles:
      - {name: Architect, handle: arch}
    children:
      - name: Backend
        id: backend
        mcp_limits:
          tracker: {tools: {close_items: blocked}}
        roles:
          - name: Dev
            handle: dev
            mcp_limits:
              docs: {tools: {erase_doc: blocked}}
`

// A LIMIT TURNS A SERVER OFF FOR THE SEATS BENEATH IT AND KEEPS ITS GRANT. The
// division's `enabled: false` reaches a seat two units down: Grants says no, so nothing of
// the server starts for it or is offered to it, while Reach still says the
// grant stands, so a value stored for the seat and a box listing it are kept
// for the day the limit is lifted. A seat outside the unit keeps the server.
//
// Mutation: read the limit in Reach rather than Grants, or drop it from
// Grants, and this is red.
func TestALimitTurnsAServerOffForTheSeatsBeneathItAndKeepsItsGrant(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, limitsDoc)
	o, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	search := &cfg.MCPServers[0]
	for handle, granted := range map[string]bool{"ceo": true, "arch": false, "dev": false} {
		seat := o.Role(handle)
		if got := search.Grants(seat); got != granted {
			t.Errorf("search.Grants(%s) = %v, want %v", handle, got, granted)
		}
		if _, reached := search.Reach(seat); !reached {
			t.Errorf("search's grant no longer reaches %s: a limit is not a grant", handle)
		}
	}
	if got := o.Role("dev").LimitOn("tracker").Tools["close_items"]; got != (org.LimitedTool{Level: org.ToolBlocked, By: "backend"}) {
		t.Errorf("dev's close_items is held to %+v, want blocked by backend", got)
	}
}

// A LIMIT SAYS ONLY WHAT A LIMIT CAN, at its field: a block holding nothing
// back, a server turned on (only the entry turns one on), a tool at allow (the
// level only the entry sets), a level no limit has,
// a key no tool carries, a server nobody declares, and a person's seat holding
// one are each refused. A stored revision carrying any but the last runs and
// warns; its limits hold as written.
//
// Mutation: drop validateMCPLimits from the admission rules, or the limits
// from mcpServerReferences, and this is red.
func TestALimitSaysOnlyWhatALimitCan(t *testing.T) {
	t.Parallel()
	head := `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp}
`
	for _, c := range []struct {
		name, doc, path string
		kind            error
	}{
		{"a block holding nothing back", head + `
roles:
  - {name: Dev, handle: dev, mcp_limits: {tracker: {}}}
`, "roles[0].mcp_limits.tracker", ErrMissing},
		{"a server turned on", head + `
roles:
  - {name: Dev, handle: dev, mcp_limits: {tracker: {enabled: true}}}
`, "roles[0].mcp_limits.tracker.enabled", ErrConflict},
		{"a tool at allow", head + `
units:
  - name: Eng
    id: eng
    mcp_limits: {tracker: {tools: {close_items: allow}}}
    roles: [{name: Dev, handle: dev}]
`, "units[0].mcp_limits.tracker.tools.close_items", ErrUnknownValue},
		{"a level no limit has", head + `
roles:
  - {name: Dev, handle: dev, mcp_limits: {tracker: {tools: {close_items: ask}}}}
`, "roles[0].mcp_limits.tracker.tools.close_items", ErrUnknownValue},
		{"a key no tool carries", head + `
roles:
  - {name: Dev, handle: dev, mcp_limits: {tracker: {tools: {"close items": blocked}}}}
`, "roles[0].mcp_limits.tracker.tools.close items", ErrUnknownValue},
		{"a server nobody declares", head + `
units:
  - name: Eng
    id: eng
    mcp_limits: {trakcer: {enabled: false}}
    roles: [{name: Dev, handle: dev}]
`, "units[0].mcp_limits.trakcer", ErrUnknownValue},
		{"a seat's limit on a server nobody declares", head + `
roles:
  - {name: Dev, handle: dev, mcp_limits: {trakcer: {enabled: false}}}
`, "roles[0].mcp_limits.trakcer", ErrUnknownValue},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if err := rejects(t, c.doc, c.path); !errors.Is(err, c.kind) {
				t.Errorf("want %v, got %v", c.kind, err)
			}
			stored, err := ParseCompanyDocument([]byte(c.doc))
			if err != nil {
				t.Fatal(err)
			}
			if err := stored.ValidateRunnable(); err != nil {
				t.Fatalf("a stored revision carrying it must still run: %v", err)
			}
			if !slices.ContainsFunc(stored.AdmissionWarnings(), func(w Warning) bool { return w.Path == c.path }) {
				t.Errorf("a stored revision carrying it does not warn at %s: %+v", c.path, stored.AdmissionWarnings())
			}
		})
	}
	t.Run("a person's seat", func(t *testing.T) {
		t.Parallel()
		rejects(t, head+`
roles:
  - {name: Pat, handle: pat, kind: human, mcp_limits: {tracker: {enabled: false}}}
`, "roles[0].mcp_limits")
	})
	// A LIMIT ON A SERVER NOT YET GRANTED is a limit all the same, holding
	// from the day the grant does.
	mustCompany(t, `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, access: {seats: [ceo]}}
roles:
  - {name: CEO, handle: ceo}
  - {name: Dev, handle: dev, mcp_limits: {tracker: {enabled: false, tools: {close_items: approve}}}}
`)
}

// AN EXECUTOR BOX IS NOT GIVEN A SERVER A SEAT'S LIMITS HOLD A TOOL OF BACK. A
// box reaches a server whole, past the engine, so the listing is refused at the
// box's entry, whichever was written last, and named once where the entry
// already holds a tool back. A server a limit turns off is kept in the box for
// the day the limit is lifted, as one turned off on its entry is.
//
// Mutation: drop the box loop from validateMCPLimits, and this is red.
func TestABoxIsNotGivenAServerASeatsLimitsHoldBack(t *testing.T) {
	t.Parallel()
	doc := `
name: Acme
providers: {sandbox: {fake: true}}
mcp_servers:
  - {name: tracker, command: tracker-mcp, tools: {default: allow}}
units:
  - name: Eng
    id: eng
    mcp_limits: {tracker: {tools: {close_items: approve}}}
    roles:
      - {name: Dev, handle: dev, sandbox: {mcp: {servers: [tracker]}}}
`
	err := rejects(t, doc, "units[0].roles[0].sandbox.mcp.servers[0]")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), `"close_items" at approve by unit "eng"'s limits`) {
		t.Errorf("want a conflict naming the limit and who set it, got %v", err)
	}
	if n := strings.Count(err.Error(), "sandbox.mcp.servers[0]"); n != 1 {
		t.Errorf("the box entry is refused %d times, want once", n)
	}
	mustCompany(t, strings.Replace(doc, "{tools: {close_items: approve}}", "{enabled: false}", 1))
}

// A REMOVAL OF A SERVER TAKES EVERY LIMIT NAMING IT, a seat's and a unit's,
// and leaves every other server's alone; the limits are dependants the removal
// lists.
func TestARemovalTakesEveryLimitNamingTheServer(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, limitsDoc)
	var fields []string
	for _, ref := range cfg.MCPServerDependants("tracker") {
		fields = append(fields, ref.Segments.String())
	}
	if want := []string{"units[0].mcp_limits.tracker", "units[0].children[0].mcp_limits.tracker"}; !slices.Equal(fields, want) {
		t.Errorf("tracker's dependants are %v, want %v", fields, want)
	}
	if _, found := cfg.RemoveMCPServer("tracker"); !found {
		t.Fatal("tracker was not removed")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the document after the removal: %v", err)
	}
	eng := &cfg.Units[0]
	if _, held := eng.MCPLimits["tracker"]; held || !eng.MCPLimits["search"].TurnsOff() {
		t.Errorf("eng's limits after the removal are %+v, want search's alone", eng.MCPLimits)
	}
	if eng.Children[0].MCPLimits != nil {
		t.Errorf("backend's limits after the removal are %+v, want none", eng.Children[0].MCPLimits)
	}
	fields = nil
	for _, ref := range cfg.MCPServerDependants("docs") {
		fields = append(fields, ref.Segments.String())
	}
	if want := []string{"units[0].children[0].roles[0].mcp_limits.docs"}; !slices.Equal(fields, want) {
		t.Errorf("docs' dependants are %v, want the seat's own limits %v", fields, want)
	}
	if cfg.RemoveMCPServer("docs"); cfg.Units[0].Children[0].Roles[0].MCPLimits != nil {
		t.Errorf("dev's limits after docs is removed are %+v", cfg.Units[0].Children[0].Roles[0].MCPLimits)
	}
}

// A WRITE THAT LIFTS OR LOOSENS A LIMIT AN AGENT SEAT IS HELD TO IS LISTED
// APART, however it does it: editing the block, moving the seat out from under
// it, or removing the unit around it. Narrowing, a limit on a server the seat is
// not granted, one the entry already holds as far, and a seat new to the
// company are not.
//
// Mutation: compare the blocks as written rather than as each seat is held, or
// drop the entry's own level from the comparison, and this is red.
func TestAWriteThatLiftsALimitIsListedApart(t *testing.T) {
	t.Parallel()
	eng := func(c *Company) *Unit { return &c.Units[0] }
	backend := func(c *Company) *Unit { return &c.Units[0].Children[0] }
	for _, tc := range []struct {
		name string
		// from readies the document the write replaces, and change makes
		// the write; a nil from leaves limitsDoc as it is.
		from, change func(c *Company)
		want         []string
	}{
		{name: "turning a server back on", change: func(c *Company) {
			delete(eng(c).MCPLimits, "search")
		}, want: []string{"arch mcp_limits.search.enabled", "dev mcp_limits.search.enabled"}},
		{name: "loosening blocked to approve", change: func(c *Company) {
			backend(c).MCPLimits["tracker"] = org.MCPLimit{Tools: map[string]org.ToolLimit{"close_items": org.ToolApprove}}
		}, want: []string{"dev mcp_limits.tracker.tools.close_items"}},
		{name: "moving a seat out from under the limits", change: func(c *Company) {
			dev := backend(c).Roles[0]
			backend(c).Roles = nil
			c.Roles = append(c.Roles, dev)
		}, want: []string{"dev mcp_limits.search.enabled", "dev mcp_limits.tracker.tools.close_items",
			"dev mcp_limits.tracker.tools.delete_items"}},
		{name: "removing the unit around a seat", change: func(c *Company) {
			eng(c).Roles = append(eng(c).Roles, backend(c).Roles...)
			eng(c).Children = nil
		}, want: []string{"dev mcp_limits.tracker.tools.close_items"}},
		{name: "narrowing further", change: func(c *Company) {
			eng(c).MCPLimits["docs"] = org.MCPLimit{Enabled: org.Off()}
			backend(c).MCPLimits["tracker"].Tools["delete_items"] = org.ToolBlocked
		}},
		{name: "lifting a limit the entry already holds as far", from: func(c *Company) {
			eng(c).MCPLimits["tracker"].Tools["wipe_items"] = org.ToolApprove
		}, change: func(c *Company) {
			delete(eng(c).MCPLimits["tracker"].Tools, "wipe_items")
		}},
		{name: "adding a seat", change: func(c *Company) {
			backend(c).Roles = append(backend(c).Roles, Role{Name: "Intern", Handle: "intern"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			before, after := mustCompany(t, limitsDoc), mustCompany(t, limitsDoc)
			if tc.from != nil {
				tc.from(before)
				tc.from(after)
			}
			tc.change(after)
			for _, c := range []*Company{before, after} {
				if err := c.Validate(); err != nil {
					t.Fatalf("a document of the write: %v", err)
				}
			}
			var got []string
			for _, w := range DiffOrg(before, after).Widened {
				got = append(got, w.Seat+" "+w.Field())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("widened = %v, want %v", got, tc.want)
			}
		})
	}
}

// A LIMIT LIFTED FROM A SERVER THE SEAT IS NOT GRANTED HANDS IT NOTHING, and is
// no widening; lifted from one granted but turned off, it hands the seat the
// server the day it is turned back on, and is one.
func TestALimitLiftedFromAServerNotGrantedIsNoWidening(t *testing.T) {
	t.Parallel()
	doc := `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, access: {seats: [ceo]}}
  - {name: docs, command: docs-mcp, enabled: false}
roles:
  - {name: CEO, handle: ceo}
  - {name: Dev, handle: dev, mcp_limits: {tracker: {enabled: false}, docs: {enabled: false}}}
`
	before, after := mustCompany(t, doc), mustCompany(t, doc)
	after.Roles[1].MCPLimits = nil
	var got []string
	for _, w := range DiffOrg(before, after).Widened {
		got = append(got, w.Seat+" "+w.Field())
	}
	if want := []string{"dev mcp_limits.docs.enabled"}; !slices.Equal(got, want) {
		t.Errorf("widened = %v, want %v", got, want)
	}
}
