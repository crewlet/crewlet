package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// accessDoc is a company whose servers reach their seats every way a grant
// can: every agent, a unit (and the unit beneath it), a seat, a unit with a
// seat left out, and an integration lane for a server reading each agent's own
// vendor credential.
const accessDoc = `
name: Acme
mcp_servers:
  - {name: search, command: search-mcp}
  - {name: docs, command: docs-mcp, access: {units: [eng], except: [intern]}}
  - {name: web-docs, command: web-mcp, access: {units: [eng, web]}}
  - {name: tracker, command: tracker-mcp, shared: false, access: {seats: [ceo]}}
  - {name: nobody, command: nobody-mcp, shared: false, access: {}}
  - {name: off, command: off-mcp, enabled: false, access: {seats: [ceo]}}
  - name: chat
    command: chat-mcp
    shared: false
    seat_credentials: [{kind: slack, key: SLACK_MCP_XOXB_TOKEN}]
roles:
  - {name: Founder, handle: founder, kind: human}
  - {name: CEO, handle: ceo, integrations: {slack: {enabled: true}}}
units:
  - name: Eng
    id: eng
    mcp_env: {docs: {DOCS_TEAM: "${ENG_DOCS}"}}
    roles:
      - {name: Dev, handle: dev}
      - {name: Intern, handle: intern}
    children:
      - name: Web
        id: web
        roles:
          - {name: Front, handle: front}
  - name: Ops
    id: ops
    roles:
      - {name: SRE, handle: sre}
`

// A SEAT HOLDS A SERVER ONLY THROUGH ITS GRANT: `access` on the entry (absent,
// every agent seat; written, the units and seats it names, minus `except`), or
// for a server reading each agent's own vendor credential, the integration lane
// the seat is on at. Each seat it reaches says why, and a seat named nowhere is
// not reached whatever else it holds: the intern inherits its unit's docs block
// and is left out. A person's seat runs no tools. A server turned off keeps its
// grant (Reach) and grants nobody (Grants).
//
// Mutation: make Reach ignore `except`, or read `mcp_env` presence again, and
// this is red.
func TestASeatHoldsAServerOnlyThroughItsGrant(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, accessDoc)
	o, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	server := func(name string) *MCPServer {
		i := slices.IndexFunc(cfg.MCPServers, func(s MCPServer) bool { return s.Name == name })
		return &cfg.MCPServers[i]
	}
	type reach struct {
		seat, reason, via string
	}
	for name, want := range map[string][]reach{
		"search":   {{"ceo", "every_agent", ""}, {"dev", "every_agent", ""}, {"intern", "every_agent", ""}, {"front", "every_agent", ""}, {"sre", "every_agent", ""}},
		"docs":     {{"dev", "unit", "eng"}, {"front", "unit", "eng"}},
		"web-docs": {{"dev", "unit", "eng"}, {"intern", "unit", "eng"}, {"front", "unit", "web"}},
		"tracker":  {{"ceo", "seat", ""}},
		"nobody":   nil,
		"off":      {{"ceo", "seat", ""}},
		"chat":     {{"ceo", "integration", "slack"}},
	} {
		var got []reach
		for seat := range o.AllRoles() {
			if r, ok := server(name).Reach(seat); ok {
				if !r.Reason.Valid() {
					t.Errorf("%s reaches %s for %q, which is no reason", name, seat.Handle(), r.Reason)
				}
				got = append(got, reach{seat.Handle(), string(r.Reason), r.Via})
			}
			if server(name).Grants(seat) != (name != "off" && slices.ContainsFunc(want,
				func(r reach) bool { return r.seat == seat.Handle() })) {
				t.Errorf("%s.Grants(%s) = %v", name, seat.Handle(), server(name).Grants(seat))
			}
		}
		slices.SortFunc(got, func(a, b reach) int { return strings.Compare(a.seat, b.seat) })
		slices.SortFunc(want, func(a, b reach) int { return strings.Compare(a.seat, b.seat) })
		if !slices.Equal(got, want) {
			t.Errorf("%s reaches %v, want %v", name, got, want)
		}
	}
	if _, ok := server("search").Reach(o.Role("founder")); ok {
		t.Error("a person's seat is reached by a server: a person runs no tools")
	}
	if _, ok := server("search").Reach(nil); ok {
		t.Error("a seat the chart does not have is reached")
	}
}

// AN MCP_ENV BLOCK GRANTS NOTHING, and one for a server that does not reach its
// seat is refused, so a grant that changes fails the write instead of silently
// dropping a seat's tools: a seat's block, a unit's block reaching none of the
// unit's own agents, a box listing, and a block for a server reading a vendor
// credential the seat is not on at. A server turned off is held to the same
// rule. A block the engine reads by its own name is not a server's alone, so it
// is left to its own rules. A stored revision carrying one runs and warns.
//
// Mutation: drop the seat loop of validateMCPAccess, and this is red.
func TestAnMCPEnvBlockForAServerThatDoesNotReachItsSeatIsRefused(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, doc, path string
	}{
		{"a seat's block", `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false, access: {seats: [ceo]}}
roles:
  - {name: CEO, handle: ceo}
  - {name: Dev, handle: dev, mcp_env: {tracker: {TOKEN: "${DEV_TRACKER}"}}}
`, "roles[1].mcp_env.tracker"},
		{"a shared server's block", `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, access: {seats: [ceo]}}
roles:
  - {name: CEO, handle: ceo}
  - {name: Dev, handle: dev, mcp_env: {tracker: {TOKEN: "${DEV_TRACKER}"}}}
`, "roles[1].mcp_env.tracker"},
		{"a turned-off server's block", `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false, enabled: false, access: {seats: [ceo]}}
roles:
  - {name: CEO, handle: ceo}
  - {name: Dev, handle: dev, mcp_env: {tracker: {TOKEN: "${DEV_TRACKER}"}}}
`, "roles[1].mcp_env.tracker"},
		{"a unit's block reaching none of its agents", `
name: Acme
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false, access: {units: [ops]}}
units:
  - name: Eng
    id: eng
    mcp_env: {tracker: {TOKEN: "${ENG_TRACKER}"}}
    roles: [{name: Dev, handle: dev}]
  - name: Ops
    id: ops
    roles: [{name: SRE, handle: sre}]
`, "units[0].mcp_env.tracker"},
		{"a box listing", `
name: Acme
providers: {sandbox: {fake: true}}
mcp_servers:
  - {name: tracker, command: tracker-mcp, access: {seats: [ceo]}, tools: {default: allow}}
roles:
  - {name: CEO, handle: ceo}
  - {name: Dev, handle: dev, sandbox: {mcp: {servers: [tracker]}}}
`, "roles[1].sandbox.mcp.servers[0]"},
		{"a vendor credential the seat is not on at", `
name: Acme
mcp_servers:
  - name: chat
    command: chat-mcp
    shared: false
    seat_credentials: [{kind: slack, key: SLACK_MCP_XOXB_TOKEN}]
roles:
  - {name: Dev, handle: dev, mcp_env: {chat: {SLACK_MCP_XOXB_TOKEN: "${DEV_SLACK}"}}}
`, "roles[0].mcp_env.chat"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, c.doc, c.path)
			if !errors.Is(err, ErrConflict) {
				t.Errorf("want ErrConflict, got %v", err)
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
	// GRANTED, the same blocks are values, and admitted.
	mustCompany(t, `
name: Acme
providers: {sandbox: {fake: true}}
mcp_servers:
  - {name: tracker, command: tracker-mcp, shared: false, access: {units: [eng]}}
  - {name: notes, command: notes-mcp, access: {seats: [dev]}, tools: {default: allow}}
units:
  - name: Eng
    id: eng
    mcp_env: {tracker: {TOKEN: "${ENG_TRACKER}"}}
    roles:
      - {name: Dev, handle: dev, mcp_env: {tracker: {TOKEN: "${DEV_TRACKER}"}}, sandbox: {mcp: {servers: [notes]}}}
`)
	// THE ENGINE'S OWN BLOCK is read by its name whatever the server's grant.
	mustCompany(t, `
name: Acme
mcp_servers:
  - {name: gitlab, command: glab, args: [mcp, serve], shared: false}
roles:
  - {name: Dev, handle: dev, mcp_env: {gitlab: {GITLAB_HOST: gitlab.example.com}}}
`)
}

// A GRANT NAMES ONLY WHAT THE CHART HOLDS: a unit's key, or an agent seat's
// handle, each once, and no seat both granted and left out. Removing a seat or
// a unit a grant still names is refused the same way, at the grant's entry, so
// a later seat or unit taking the same handle or key is never granted by a
// grant written for another.
//
// Mutation: skip the unit lookup in validateAccess, and this is red.
func TestAGrantNamesOnlyWhatTheChartHolds(t *testing.T) {
	t.Parallel()
	const head = `
name: Acme
roles:
  - {name: Founder, handle: founder, kind: human}
  - {name: CEO, handle: ceo}
units:
  - name: Eng
    id: eng
    roles: [{name: Dev, handle: dev}]
mcp_servers:
`
	for _, c := range []struct {
		name, access, path string
		want               error
	}{
		{"a seat no chart has", "{seats: [cto]}", "mcp_servers[0].access.seats[0]", ErrUnknownValue},
		{"a person's seat", "{seats: [founder]}", "mcp_servers[0].access.seats[0]", ErrConflict},
		{"a unit no chart has", "{units: [design]}", "mcp_servers[0].access.units[0]", ErrUnknownValue},
		{"an empty unit", `{units: [""]}`, "mcp_servers[0].access.units[0]", ErrMissing},
		{"a left-out seat no chart has", "{units: [eng], except: [cto]}", "mcp_servers[0].access.except[0]", ErrUnknownValue},
		{"a seat granted and left out", "{seats: [dev], except: [dev]}", "mcp_servers[0].access.except[0]", ErrConflict},
		{"a seat listed twice", "{seats: [ceo, ceo]}", "mcp_servers[0].access.seats[1]", ErrConflict},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := rejects(t, head+"  - {name: docs, command: docs-mcp, access: "+c.access+"}\n", c.path)
			if !errors.Is(err, c.want) {
				t.Errorf("want %v, got %v", c.want, err)
			}
		})
	}
	// REMOVING what a grant names: the same document with the seat and the
	// unit gone is refused at the grant, until the grant lets them go.
	granted := head + "  - {name: docs, command: docs-mcp, access: {seats: [ceo], units: [eng]}}\n"
	mustCompany(t, granted)
	rejects(t, strings.Replace(granted, "  - {name: CEO, handle: ceo}\n", "", 1),
		"mcp_servers[0].access.seats[0]")
	rejects(t, strings.Replace(granted, "  - name: Eng\n    id: eng\n    roles: [{name: Dev, handle: dev}]\n",
		"", 1), "mcp_servers[0].access.units[0]")
}

// A SERVER READING EACH AGENT'S OWN VENDOR CREDENTIAL TAKES NO ACCESS: the
// integration's lane grants it, and two switches for one grant would disagree.
// A declared binding and a recognised one alike.
func TestAServerBoundToAVendorCredentialTakesNoAccess(t *testing.T) {
	t.Parallel()
	for _, entry := range []string{
		"{name: chat, command: chat-mcp, shared: false, seat_credentials: [{kind: slack, key: SLACK_MCP_XOXB_TOKEN}], access: {seats: [dev]}}",
		"{name: code, command: glab, args: [mcp, serve], shared: false, access: {seats: [dev]}}",
	} {
		err := rejects(t, "name: Acme\nroles:\n  - {name: Dev, handle: dev}\nmcp_servers:\n  - "+entry+"\n",
			"mcp_servers[0].access")
		if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "integrations") {
			t.Errorf("want a conflict naming the integration lane, got %v", err)
		}
	}
	// SHARED, the same server reads no seat's credential, and takes access.
	mustCompany(t, "name: Acme\nroles:\n  - {name: Dev, handle: dev}\nmcp_servers:\n"+
		"  - {name: code, command: glab, args: [mcp, serve], access: {seats: [dev]}}\n")
}

// A GRANT TO NOBODY IS ADVISED, never refused: the page writes one while a
// person is still choosing, and turning a server off is the way to start nothing.
func TestAGrantReachingNoAgentIsAdvised(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, accessDoc)
	var paths []string
	for _, w := range cfg.AdvisoryWarnings() {
		if strings.HasSuffix(w.Path, ".access") {
			paths = append(paths, w.Path)
		}
	}
	if !slices.Equal(paths, []string{"mcp_servers[4].access"}) {
		t.Errorf("advised grants = %v, want only the one reaching nobody", paths)
	}
}
