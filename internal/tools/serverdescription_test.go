package tools_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tools"
)

// A SERVER'S DESCRIPTION IS LISTED BESIDE ITS NAME, which is all a phase is
// shown of a server before it spends a call listing the server's tools. A
// server nobody described keeps the bare line.
func TestAServerIsListedWithWhatItIsFor(t *testing.T) {
	t.Parallel()
	r := tools.NewRegistry()
	r.DescribeServers(map[string]string{"handbook": "Searches the engineering handbook"})
	mustRegister(t, r, tool("hb_search"), tools.Origin("handbook"))
	mustRegister(t, r, tool("gh_a"), tools.Origin("github"))
	mustRegister(t, r, tool("lookup"), tools.OriginBuiltin)

	lines := strings.Split(r.Snapshot().Discoverable(nil), "\n")
	for _, want := range []string{
		"- MCP server `handbook`: Searches the engineering handbook",
		"- MCP server `github`",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("the list has no line %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
	if entry, _ := r.Lookup("lookup"); entry.ServerDescription != "" {
		t.Errorf("a builtin carries the server description %q", entry.ServerDescription)
	}
}

// THE DESCRIPTION GOES WHEREVER THE TOOL GOES: stamped on a tool filed before
// the registry was described and on one filed after, carried into a clone (a
// seat's registry is its epoch's, cloned) and through every cut of a snapshot
// (a worker's grant is rebuilt entry by entry). A table kept beside the
// entries is one each of those would have to remember to carry.
func TestAServerDescriptionGoesWhereverItsToolGoes(t *testing.T) {
	t.Parallel()
	r := tools.NewRegistry()
	mustRegister(t, r, tool("hb_before"), tools.Origin("handbook"))
	r.DescribeServers(map[string]string{"handbook": "Searches the handbook"})
	mustRegister(t, r, tool("hb_after"), tools.Origin("handbook"))

	seat := r.Clone()
	mustRegister(t, seat, tool("hb_seat"), tools.Origin("handbook"))
	for _, name := range []string{"hb_before", "hb_after", "hb_seat"} {
		entry, ok := seat.Lookup(name)
		if !ok || entry.ServerDescription != "Searches the handbook" {
			t.Errorf("%s carries %q, want its server's description", name, entry.ServerDescription)
		}
	}

	cut := seat.Snapshot().Without("hb_before")
	rebuilt := tools.Snapshot{}
	for _, entry := range cut.Entries() {
		var err error
		if rebuilt, err = rebuilt.With(entry); err != nil {
			t.Fatal(err)
		}
	}
	if got := rebuilt.Discoverable(nil); got != "- MCP server `handbook`: Searches the handbook" {
		t.Errorf("a rebuilt snapshot lists %q, want the server with its description", got)
	}

	// DESCRIBED AGAIN, as each apply's new registry is: what a revision
	// no longer says is said by nobody.
	r.DescribeServers(nil)
	if entry, _ := r.Lookup("hb_before"); entry.ServerDescription != "" {
		t.Errorf("a description the revision dropped is still carried: %q", entry.ServerDescription)
	}
}
