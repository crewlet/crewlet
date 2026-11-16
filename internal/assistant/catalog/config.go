package catalog

import (
	"net/url"

	"github.com/crewlet/crewlet/internal/assistant/exec"
)

// configCollections are the company document's collections read one member at
// a time (internal/api/configapi's entity routes), by the name a person says.
var configCollections = map[string]string{
	"seat": "roles", "unit": "units", "model": "llm-providers", "tool_server": "mcp-servers",
}

// configSectionRead reads one section of the company document, or one member of
// its seats, units, models or tool servers, instead of the whole document.
func configSectionRead() Tool {
	return Tool{
		Name: "get_config_section", Phrase: "read the company's configuration",
		Description: "Administrators: read one part of the company document, redacted, as " +
			"it is now: a top-level section by its key (assistant, integrations, providers, " +
			"token_budget, ...), or one seat, unit, model or tool server by its key with " +
			"member. Read the part a question is about rather than the whole document.",
		schema: object([]string{"section"}, map[string]*schema{
			"section": text("A top-level key of the document, or seat, unit, model or "+
				"tool_server for one member.", nameBytes),
			"member": text("With seat, unit, model or tool_server: the member's key as the "+
				"document writes it.", nameBytes),
		}),
		Routes: []string{"GET /config", "GET /config/roles/{id}", "GET /config/units/{id}",
			"GET /config/llm-providers/{id}", "GET /config/mcp-servers/{id}"},
		Apps: adminOnly, Fence: true, Infer: true,
		Build: func(a map[string]any) (exec.Request, error) {
			args := Args(a)
			kind, member := configCollections[args.String("section")], args.String("member")
			if kind != "" && member != "" {
				return get("GET /config/"+kind+"/{id}", map[string]string{"id": member}, nil)
			}
			return get("GET /config", nil, url.Values{"section": {args.String("section")}})
		},
		Title: func(a Args) string {
			if m := a.String("member"); m != "" {
				return "Configuration › " + a.String("section") + " " + m
			}
			return "Configuration › " + a.String("section")
		},
		Label: func(a map[string]any) string {
			return "Reading the company's " + Args(a).String("section") + " configuration"
		},
	}
}
