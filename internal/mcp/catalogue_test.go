package mcp_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
)

// ONE RENDERER, asserted once. Four call sites render this line — the tool
// registry's catalogue, both list_mcp_server_tools listings, and a sub-agent's
// surface — and a model sees more than one of them in a single turn, so two
// implementations is how one starts cutting again. tools.CatalogueLine
// delegates here; the last case is what holds that true.
func TestACatalogueLineKeepsTheWholeDescription(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, desc, want string
	}{
		{"no description", "", "- get_file"},
		{"one line", "Read a file", "- get_file: Read a file"},
		{
			// The half a first-line-only renderer threw away: the argument
			// rules and preconditions a vendor writes below its opening
			// sentence, which is what a model needs to call the tool right.
			"multi-line indents under its bullet",
			"Read a file.\nAccepts a ref; returns base64 for binaries.",
			"- get_file: Read a file.\n  Accepts a ref; returns base64 for binaries.",
		},
		{
			// A blank line stays blank rather than becoming two spaces.
			"blank continuation line",
			"Read a file.\n\nPaging: pass a cursor.",
			"- get_file: Read a file.\n\n  Paging: pass a cursor.",
		},
		{"surrounding whitespace is trimmed", "  Read a file  ", "- get_file: Read a file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mcp.CatalogueLine("get_file", tc.desc); got != tc.want {
				t.Errorf("CatalogueLine = %q, want %q", got, tc.want)
			}
			if got := tools.CatalogueLine("get_file", tc.desc); got != tc.want {
				t.Errorf("tools.CatalogueLine diverged: %q, want %q", got, tc.want)
			}
		})
	}
}

// Nothing in the rendering may drop content: a description is shown here or
// nowhere.
func TestACatalogueLineDropsNothing(t *testing.T) {
	t.Parallel()
	desc := strings.Repeat("a long paragraph of vendor guidance. ", 200)
	got := mcp.CatalogueLine("get_file", desc)
	if !strings.Contains(got, strings.TrimSpace(desc)) {
		t.Error("a long description was shortened")
	}
}
