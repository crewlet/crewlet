package configapi_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
)

// toolsDoc is a company with a server whose levels are set and one a seat's
// executor box lists.
const toolsDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    default:
      model: claude-opus-5-5
mcp_servers:
  - name: tracker
    command: tracker-mcp
    description: Projects and items.
    tools: {levels: {delete_project: blocked, merge_projects: blocked}}
  - {name: notes, command: notes-mcp, tools: {default: allow}}
roles:
  - {name: Dev, handle: dev, sandbox: {mcp: {servers: [notes]}}}
`

func level(l config.MCPToolLevel) *config.MCPToolLevel { return &l }

// A WRITE OF A SERVER'S TOOL LEVELS CHANGES WHAT IT NAMES AND NOTHING ELSE, as
// one revision of the document as it stands: a level the request leaves out
// stays, null removes one, the default is set and removed the same way, every
// other field of the entry is kept, and a dry run stores nothing. A write sent
// with no summary is refused once it is checked, and stores nothing.
//
// Mutation: make applied replace the levels rather than change them, or let
// the dry run commit, and this is red.
func TestAWriteOfToolLevelsChangesWhatItNamesAsOneRevision(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	first := s.seed(t, toolsDoc)
	write := configapi.MCPToolsWrite{Server: "tracker", By: valueWriter, Change: configapi.MCPToolsChange{
		SetDefault: true, Default: level(config.MCPToolBlocked),
		Levels: map[string]*config.MCPToolLevel{"list_projects": level(config.MCPToolAllow), "merge_projects": nil},
	}}

	checked := write
	checked.DryRun = true
	got, err := s.service().SetMCPTools(t.Context(), checked)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if got.Applied || got.Answer["valid"] != true || got.Tools.Default != config.MCPToolBlocked {
		t.Fatalf("dry run answered %+v", got)
	}
	if revision, _, _ := s.active(t.Context()); revision.ID != first {
		t.Fatalf("the dry run stored revision %s", revision.ID)
	}

	if _, err = s.service().SetMCPTools(t.Context(), write); !errors.Is(err, configapi.ErrMCPToolsSummary) {
		t.Fatalf("a write with no summary: %v, want ErrMCPToolsSummary", err)
	}
	if revision, _, _ := s.active(t.Context()); revision.ID != first {
		t.Fatalf("a write with no summary stored revision %s", revision.ID)
	}

	write.Summary = "tracker: block every tool but list_projects"
	got, err = s.service().SetMCPTools(t.Context(), write)
	if err != nil {
		t.Fatalf("SetMCPTools: %v", err)
	}
	if !got.Applied || got.Answer["revision_id"] == "" {
		t.Fatalf("the write answered %+v", got)
	}
	company, _, err := s.service().ActiveCompany(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	tracker := company.MCPServers[0]
	want := map[string]config.MCPToolLevel{"delete_project": config.MCPToolBlocked,
		"list_projects": config.MCPToolAllow}
	if tracker.Tools.Default != config.MCPToolBlocked || len(tracker.Tools.Levels) != len(want) {
		t.Fatalf("tracker's levels are %+v, want default blocked and %v", tracker.Tools, want)
	}
	for tool, l := range want {
		if tracker.Tools.Levels[tool] != l {
			t.Errorf("tracker's %s is %q, want %q", tool, tracker.Tools.Levels[tool], l)
		}
	}
	if tracker.Description != "Projects and items." || tracker.Command != "tracker-mcp" {
		t.Errorf("the write touched the rest of the entry: %+v", tracker)
	}

	// THE DEFAULT REMOVED, AND EVERY LEVEL: the entry writes no `tools`.
	if _, err = s.service().SetMCPTools(t.Context(), configapi.MCPToolsWrite{Server: "tracker", By: valueWriter,
		Summary: "tracker: allow every tool", Change: configapi.MCPToolsChange{SetDefault: true,
			Levels: map[string]*config.MCPToolLevel{"delete_project": nil, "list_projects": nil}}}); err != nil {
		t.Fatalf("clear the levels: %v", err)
	}
	if document := s.activeDocument(t); strings.Contains(document, `"tools"`) {
		t.Errorf("an entry with no level left still writes tools: %s", document)
	}
}

// A WRITE OF TOOL LEVELS IS HELD TO THE WHOLE DOCUMENT'S RULES: a level that
// holds a tool back on a server an executor box lists is refused at the box's
// entry, a level outside allow, approve and blocked at its own field, and a
// server the document does not declare is no entity to write.
//
// Mutation: drop validateBoxToolLevels, or build on a server that is absent,
// and this is red.
func TestAWriteOfToolLevelsIsHeldToTheWholeDocument(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, toolsDoc)
	for _, c := range []struct {
		name  string
		write configapi.MCPToolsWrite
		path  string
	}{
		{"a box lists the server", configapi.MCPToolsWrite{Server: "notes", Change: configapi.MCPToolsChange{
			Levels: map[string]*config.MCPToolLevel{"erase_note": level(config.MCPToolBlocked)}}},
			"roles[0].sandbox.mcp.servers[0]"},
		{"a level outside the set", configapi.MCPToolsWrite{Server: "tracker", Change: configapi.MCPToolsChange{
			Levels: map[string]*config.MCPToolLevel{"delete_project": level("ask")}}},
			"mcp_servers[0].tools.levels.delete_project"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.write.By, c.write.Summary = valueWriter, "set levels"
			_, err := s.service().SetMCPTools(t.Context(), c.write)
			var invalid *configapi.ValidationError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), c.path) {
				t.Fatalf("SetMCPTools: %v, want a refusal at %s", err, c.path)
			}
		})
	}
	_, err := s.service().SetMCPTools(t.Context(), configapi.MCPToolsWrite{Server: "billing", By: valueWriter,
		Summary: "set levels", Change: configapi.MCPToolsChange{SetDefault: true}})
	if !errors.Is(err, configapi.ErrNoSuchEntity) {
		t.Fatalf("a server the document does not declare: %v, want ErrNoSuchEntity", err)
	}
}
