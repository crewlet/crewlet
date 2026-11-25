package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// A HELPER (Ask Crewlet, the Advisor) RUNS ON AN ENTRY THE COMPANY ALREADY HAS,
// never on one made for it. Its run is not the entry's: the engine builds it
// from [LLMProvider.HelperOverrides], which turns the harness's own web tools
// off whatever the entry says, so the helper's model has no tool but the
// engine's own while a seat on the same entry keeps its web (ADR-0050). What a
// helper refuses is a harness that cannot be run that way, never an entry's
// settings: [Company.HelperModelProblem].

// HelperOverrides is the entry's overrides as a helper's run takes them: its
// own, with `web_tools` forced to denied.
func (l LLMProvider) HelperOverrides() map[string]any {
	out := maps.Clone(l.Overrides)
	if out == nil {
		out = map[string]any{}
	}
	out["web_tools"] = string(harnessprofile.WebToolsDenied)
	return out
}

// SearchOverrides is the entry's overrides as Ask Crewlet's search run takes
// them: its own, with `web_tools` set to search, the harness's web search and
// no other tool.
func (l LLMProvider) SearchOverrides() map[string]any {
	out := maps.Clone(l.Overrides)
	if out == nil {
		out = map[string]any{}
	}
	out["web_tools"] = string(harnessprofile.WebToolsSearch)
	return out
}

// HelperModelProblem says why the providers.llm entry key cannot run a helper,
// or nil: an entry the document does not hold, or one whose harness cannot be
// run with no tool of its own. The one rule both helpers' models are held to,
// at admission, where each is handed out, and on the Models screen.
func (c *Company) HelperModelProblem(key string) error {
	return c.helperModelProblem(entryPath(key), key)
}

// helperModelProblem is [Company.HelperModelProblem], located at path, the
// field that names key. A fleet reference holds every member to the rule.
func (c *Company) helperModelProblem(path Path, key string) error {
	var p problems
	if c.fleetReferenceProblem(&p, path, []string{key}) {
		for _, member := range c.FleetMembers(key) {
			p.wrap(c.helperEntryProblem(path, member))
		}
		return p.err()
	}
	return c.helperEntryProblem(path, key)
}

// helperEntryProblem is [Company.helperModelProblem] for one entry key.
func (c *Company) helperEntryProblem(path Path, key string) error {
	var p problems
	entry, ok := c.Providers.LLM[key]
	if !ok {
		configured := "none"
		if known := slices.Sorted(maps.Keys(c.Providers.LLM)); len(known) > 0 {
			configured = strings.Join(known, ", ")
		}
		p.addSaying(path, ErrUnknownValue, "This model is not one of the company's models.",
			"%q is not a configured provider: providers.llm has %s", key, configured)
		return p.err()
	}
	if entry.Harness != "" && !entry.Harness.Valid() {
		return nil // the entry's own rule names the harness; there is no profile
	}
	if problem := helperRunProblem(&entry); problem != "" {
		p.addSaying(path, ErrConflict, helperRefusal,
			"providers.llm.%s runs the %s harness, which %s, so a helper's run of it could "+
				"read what no request of the person's returned: a helper runs only on a "+
				"harness the engine can run with no tool of its own, which of the built-in "+
				"profiles is %s", key, entry.HarnessName(), problem,
			strings.Join(harnessprofile.HelperEligible(), ", "))
	}
	return p.err()
}

// helperRefusal is a refused harness in a person's words.
const helperRefusal = "This model runs with tools of its own that Crewlet cannot turn off, " +
	"so it cannot run Ask Crewlet or the Advisor."

// helperRunProblem says why the run a helper makes of entry would leave its
// model a tool of its own, or "": judged on the argv that run carries
// ([harnessprofile.Profile.ToolsOffProblem]), never on the profile's claim.
func helperRunProblem(entry *LLMProvider) string {
	own, err := harnessprofile.Load(entry.HarnessName(), entry.Overrides)
	if err != nil {
		return "" // the entry's own rule reports why its profile does not load
	}
	if !own.CanDenyWeb() {
		return "has no measured way to turn its own web tools off (" +
			strings.TrimSpace(own.WebToolsNote) + ")"
	}
	profile, err := harnessprofile.Load(entry.HarnessName(), entry.HelperOverrides())
	switch {
	case err != nil:
		return "does not load with its web tools off (" + err.Error() + ")"
	case profile.LocalTools != harnessprofile.LocalToolsDenied:
		return fmt.Sprintf("keeps its own local tools %s (%s)", localToolsWord(profile.LocalTools),
			strings.TrimSpace(profile.LocalToolsNote))
	}
	if problem := profile.ToolsOffProblem(); problem != "" {
		return "is not shown to run with no tool of its own: " + problem
	}
	// THE HELPER'S RUN SHARES THE ENTRY'S HOME, sign-in and settings files, so
	// a denial that only a settings file of its own makes cannot hold there.
	if !slices.Equal(profile.SeedFiles, own.SeedFiles) || !slices.Equal(mcpSeeds(profile), mcpSeeds(own)) {
		return "turns its web tools off only through settings files of its own, which a run " +
			"sharing the entry's sign-in cannot keep apart"
	}
	return ""
}

// mcpSeeds is the settings files a profile's run with tools writes.
func mcpSeeds(p harnessprofile.Profile) []harnessprofile.SeedFile {
	if p.MCP == nil {
		return nil
	}
	return p.MCP.Seed
}

// HelperKeys are the providers.llm entries the company's helpers run on,
// each once, a fleet's members included: what the engine builds a helper's
// run of.
func (c *Company) HelperKeys() []string {
	var out []string
	for _, model := range []string{c.Assistant.Model, c.Advisor.Model} {
		if model == "" {
			continue
		}
		for _, key := range c.FleetMembers(model) {
			if !slices.Contains(out, key) {
				out = append(out, key)
			}
		}
	}
	return out
}

// localToolsWord renders a local-tools stance inside a sentence.
func localToolsWord(t harnessprofile.LocalTools) string {
	if t == "" {
		return "undeclared"
	}
	return "at the " + string(t) + " stance"
}
