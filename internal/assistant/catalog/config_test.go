package catalog_test

import (
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/provenance"
)

// A section reads GET /config?section=, a member its collection's entity route,
// and the card is titled for the part read.
func TestOneSectionOfTheCompanyDocumentIsRead(t *testing.T) {
	t.Parallel()
	tool := mustTool(t, "get_config_section")
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"section": "assistant"}, "/config?section=assistant"},
		{map[string]any{"section": "seat", "member": "agent-cto"}, "/config/roles/agent-cto?"},
		{map[string]any{"section": "tool_server", "member": "github"}, "/config/mcp-servers/github?"},
	} {
		req, err := tool.Request(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if got := req.Path + "?" + req.Query.Encode(); got != tc.want {
			t.Errorf("get_config_section %v asks %s, want %s", tc.args, got, tc.want)
		}
	}
	res, err := catalog.Shape(tool, map[string]any{"section": "assistant"}, exec.Response{
		Status: http.StatusOK, Body: []byte(`{"enabled":true,"max_steps":30}`)},
		provenance.NewBoundary(), evals.Epoch, cards.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cards) != 1 || res.Cards[0].Entity == nil ||
		res.Cards[0].Entity.Title != "Configuration › assistant" {
		t.Errorf("the section is carded as %+v, want one record titled for it", res.Cards)
	}
}
