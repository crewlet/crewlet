package config

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/mcp/catalogue"
)

// Assistant is the Tier B `assistant:` block: the one model Ask Crewlet runs
// on, and each person's spend ceiling.
//
//	assistant:
//	  model: assistant    # a providers.llm key; absent means off
//
// Every bound of a turn is fixed in code where it is consumed. An
// absent model is off, so an upgrade never switches on a model reading the
// company's data.
type Assistant struct {
	// Model is the providers.llm entry Ask Crewlet runs on, held to
	// [Company.HelperModelProblem]: a harness whose run leaves the model no
	// tool of its own and no web tool, so its only tool source on the engine
	// host is the engine's `crewlet` server, serving the person's own
	// requests (ADR-0050). [Company.AssistantModel] hands it out.
	Model string `yaml:"model,omitempty" json:"model,omitempty" desc:"The providers.llm key Ask Crewlet runs on: an entry whose harness's local tools are denied and, through overrides.web_tools, its web tools. Absent: Ask Crewlet is off."`

	// TokenBudget is each PERSON's ceiling per calendar window, in the shape
	// a seat's budget takes, charged on top of the company's own.
	TokenBudget TokenBudget `yaml:"token_budget,omitempty" json:"token_budget,omitzero" desc:"Each person's Ask Crewlet token ceilings per calendar window on the company clock: day, week and month, each optional. Absent = no per-person ceiling; the company's own budget still applies. A ceiling at or above the company's on a window that holds it is warned of, since the company's is reached first."`

	// DeepWork is the assistant's escalation into a harness's own
	// harness for a task that needs research, scripts, files or many steps
	// (ADR-0054). Absent: the assistant answers in quick mode only.
	DeepWork *AssistantDeepWork `yaml:"deep_work,omitempty" json:"deep_work,omitempty" desc:"Deep work: Ask Crewlet hands a large task to a harness running its own tools in a sandbox, reaching Crewlet only through the person's own Ask Crewlet tools. Absent: quick mode only."`

	// Research is the web Ask Crewlet may read, through the engine (ADR-0151).
	Research *AssistantResearch `yaml:"research,omitempty" json:"research,omitempty" desc:"Web research: Ask Crewlet reads pages, through the engine, from the hosts listed here. Absent: it reads no web page."`
}

// AssistantResearch is the `assistant.research` block (ADR-0151).
type AssistantResearch struct {
	Hosts []string `yaml:"hosts" json:"hosts" desc:"The hosts Ask Crewlet may read pages from over https, each a host name alone (docs.example.com), at most 50. Required."`

	// Search is the company's search server Ask Crewlet searches the hosts with.
	Search *ResearchSearch `yaml:"search,omitempty" json:"search,omitempty" desc:"Web search over the listed hosts, through the company's own search tool server. Absent: Ask Crewlet reads pages it is given but searches none."`
}

// ResearchSearch is the `assistant.research.search` block (ADR-0151).
type ResearchSearch struct {
	MCPServer string `yaml:"mcp_server" json:"mcp_server" desc:"The mcp_servers entry Ask Crewlet searches through: a shared, enabled entry the curated catalogue recognises as Tavily, whose tavily_search is not blocked. Each node serving the API runs its own instance of it for research alone. Required."`
}

// SearchServerCatalogueID is the curated entry a research search server must be.
const SearchServerCatalogueID = "tavily"

// searchProblems reports a search block naming no entry, one the document does not
// hold, or one that is not a shared, enabled Tavily server allowed to search.
func (c *Company) searchProblems(path Path) error {
	r := c.Assistant.Research
	if r == nil || r.Search == nil {
		return nil
	}
	var p problems
	serverPath := at(path, "mcp_server")
	name := r.Search.MCPServer
	i := slices.IndexFunc(c.MCPServers, func(m MCPServer) bool { return m.Name == name })
	switch {
	case name == "":
		p.add(serverPath, ErrMissing, "research search names no tool server: name the mcp_servers "+
			"entry that runs Tavily")
	case i < 0:
		p.add(serverPath, ErrUnknownValue, "%q is not an mcp_servers entry", name)
	default:
		m := &c.MCPServers[i]
		entry, known := catalogue.Recognise(m.Launch())
		level := m.Tools.Levels[researchSearchTool]
		if level == "" {
			level = m.Tools.Default
		}
		switch {
		case !known || entry.ID != SearchServerCatalogueID:
			p.add(serverPath, ErrConflict, "%s is not the curated Tavily server (tavily-mcp), the "+
				"one search Ask Crewlet knows how to ask", name)
		case !m.IsShared() || !m.IsEnabled():
			p.add(serverPath, ErrConflict, "%s must be shared and enabled: research searches as the "+
				"company, with the entry's own key", name)
		case level == MCPToolBlocked:
			p.add(serverPath, ErrConflict, "%s blocks %s, the tool a research search calls", name,
				researchSearchTool)
		}
	}
	return p.err()
}

// researchSearchTool is the tool a research search calls (internal/research).
const researchSearchTool = "tavily_search"

// MaxResearchHosts bounds the list a person confirms host by host.
const MaxResearchHosts = 50

// researchHost is a host name as the list takes it: lower case, dotted, no
// scheme, port, path or wildcard.
var researchHost = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
})

// ResearchHosts are the hosts research may read from; none where it is off.
func (a Assistant) ResearchHosts() []string {
	if a.Research == nil {
		return nil
	}
	return a.Research.Hosts
}

// problems reports a research block with no host, too many, or one that
// is not a bare host name.
func (r *AssistantResearch) problems(path Path) error {
	var p problems
	if r == nil {
		return nil
	}
	hosts := at(path, "hosts")
	switch {
	case len(r.Hosts) == 0:
		p.add(hosts, ErrMissing, "research lists no host: list the documentation hosts "+
			"Ask Crewlet may read, such as developers.cloudflare.com")
	case len(r.Hosts) > MaxResearchHosts:
		p.add(hosts, ErrOutOfRange, "research lists %d hosts, past the %d a person can "+
			"confirm", len(r.Hosts), MaxResearchHosts)
	}
	seen := map[string]bool{}
	for i, h := range r.Hosts {
		entry := idx(hosts, i)
		switch {
		case !researchHost().MatchString(h):
			p.add(entry, ErrUnknownValue, "%q is not a host name: write the host alone, in "+
				"lower case, with no scheme, port, path or wildcard (docs.example.com)", h)
		case seen[h]:
			p.add(entry, ErrConflict, "%s is listed twice", h)
		}
		seen[h] = true
	}
	return p.err()
}

// AssistantDeepWork is the `assistant.deep_work` block.
//
// The entry it names runs as its harness's own run in a box, as a seat's
// executor does: the harness's shell, files, web search and image reading are
// the point. Its one way into Crewlet is the person's own assistant tools,
// bridged, so every read is still the person's request and every write a
// proposal they approve. It runs where the entry's `run_in` says, else the
// first of e2b and container configured, else the host under OS confinement,
// which a host that cannot enforce one refuses (internal/sandbox).
//
// Numeric zeros read as their defaults, as in [Assistant].
type AssistantDeepWork struct {
	LLM            string `yaml:"llm,omitempty" json:"llm,omitempty" desc:"providers.llm key deep work runs on: an entry driving claude-code. Required."`
	TimeoutSeconds int    `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty" js:"min=0;max=1200" desc:"Longest one deep-work run may take, 60..1200 seconds. Absent: 1200."`
	MaxSteps       int    `yaml:"max_steps,omitempty" json:"max_steps,omitempty" js:"min=0;max=400" desc:"Most agentic rounds and Crewlet tool calls one deep-work run may make, 10..400. Absent: 150."`
	MaxTokens      int    `yaml:"max_tokens,omitempty" json:"max_tokens,omitempty" js:"min=0;max=20000000" desc:"Most tokens one deep-work run may spend before it is stopped, 10000..20000000. Absent: 3000000. The person's and the company's budgets still apply."`
}

// assistantBound is one numeric field's range and the default its zero reads as.
type assistantBound struct {
	key      string
	min, max int
	def      int
	unit     string
}

// Deep work's bounds, each anchored where it is consumed:
//
//   - timeout_seconds: a run lives inside one turn segment, whose read grant
//     lasts at most [deepTimeoutCeiling], so twenty minutes is both
//     default and ceiling; a longer run parks on a proposal and continues on
//     the fresh grant its approval derives. One minute reads one page.
//   - max_steps: a redesign that reads a brand, writes assets, renders and
//     checks them takes a hundred-odd rounds; four hundred is the most a
//     twenty-minute run makes at the harness's three-second round.
//   - max_tokens: 150 rounds re-reading a growing context are about three
//     million tokens, mostly cache reads; twenty million bounds a run nobody meant.
var (
	deepTimeout = assistantBound{key: "timeout_seconds", min: 60, max: deepTimeoutCeiling, def: deepTimeoutCeiling, unit: "seconds"}
	deepSteps   = assistantBound{key: "max_steps", min: 10, max: 400, def: 150, unit: "steps"}
	deepTokens  = assistantBound{key: "max_tokens", min: 10_000, max: 20_000_000, def: 3_000_000, unit: "tokens"}
)

// deepTimeoutCeiling is the longest a turn's read grant lives, in seconds
// (internal/assistant/exec's ReadLifetime), which a deep-work run never outlives.
const deepTimeoutCeiling = 1200

// value is v with its default applied and clamped into range.
func (b assistantBound) value(v int) int {
	switch {
	case v == 0:
		return b.def
	case v < b.min:
		return b.min
	case v > b.max:
		return b.max
	}
	return v
}

// rangeOf is b as the settings registry declares it ([settingRanges]); 0 is
// absent, the default.
func (b assistantBound) rangeOf() Range {
	return Range{Min: float64(b.min), Max: atMost(float64(b.max)), OrZero: true}
}

// check reports v when it is set and out of range; absent (0) is the default.
func (b assistantBound) check(path Path, v int, p *problems) {
	if v == 0 {
		return
	}
	p.within(at(path, b.key), float64(v), fmt.Sprintf("in %s, or absent for the default of %d",
		b.unit, b.def))
}

// On reports whether the company assigned Ask Crewlet a model.
func (a Assistant) On() bool { return a.Model != "" }

// IsZero lets an unset block drop out of a JSON round trip.
func (a Assistant) IsZero() bool {
	return a.Model == "" && a.TokenBudget == (TokenBudget{}) && a.DeepWork == nil && a.Research == nil
}

// Timeout is the longest one deep-work run may take.
func (d AssistantDeepWork) Timeout() time.Duration {
	return time.Duration(deepTimeout.value(d.TimeoutSeconds)) * time.Second
}

// Steps is the most agentic rounds and bridged calls one run may make.
func (d AssistantDeepWork) Steps() int { return deepSteps.value(d.MaxSteps) }

// Tokens is the most tokens one run may spend.
func (d AssistantDeepWork) Tokens() int { return deepTokens.value(d.MaxTokens) }

// ErrDeepWorkOff is what [Company.AssistantDeepWork] answers for a company
// whose assistant has no deep work.
var ErrDeepWorkOff = errors.New("deep work is off for Ask Crewlet: set assistant.deep_work.llm " +
	"to a Claude Code providers.llm entry")

// AssistantDeepWork is the providers.llm entry deep work runs on and its key,
// or why there is none: [ErrAssistantOff], [ErrDeepWorkOff], or the problems
// admission reports for the block. THE ONE PLACE IT IS HANDED OUT, for the
// reason [Company.AssistantChain] is.
func (c *Company) AssistantDeepWork() (string, LLMProvider, error) {
	if !c.Assistant.On() {
		return "", LLMProvider{}, ErrAssistantOff
	}
	d := c.Assistant.DeepWork
	if d == nil || d.LLM == "" {
		return "", LLMProvider{}, ErrDeepWorkOff
	}
	if err := c.deepWorkProblems(at(field("assistant"), "deep_work")); err != nil {
		return "", LLMProvider{}, err
	}
	return d.LLM, c.Providers.LLM[d.LLM], nil
}

// DeepWorkPlacement is the cell deep work runs in, and whether the company
// runs deep work at all: its entry's own `run_in`, else the first of e2b and
// container the company configures, else [PlacementDirect], the engine host,
// where the engine confines every run (ADR-0054). A REMOTE OR CONTAINED CELL
// IS PREFERRED whenever one is configured, because the host is the cell whose
// boundary is the engine's to hold rather than a machine's.
func (c *Company) DeepWorkPlacement() (Placement, bool) {
	_, entry, err := c.AssistantDeepWork()
	if err != nil {
		return "", false
	}
	if entry.RunIn != "" {
		return entry.RunIn, true
	}
	sb := c.Providers.Sandbox
	switch {
	case sb.Configured(PlacementE2B):
		return PlacementE2B, true
	case sb.Configured(PlacementContainer) && sb.Local != nil && sb.Local.Image != "":
		return PlacementContainer, true
	}
	return PlacementDirect, true
}

// deepWorkProblems reports every way `assistant.deep_work` cannot run: no
// entry, an unknown one, a harness other than Claude Code, or a run_in this
// company configures no backend for.
func (c *Company) deepWorkProblems(path Path) error {
	var p problems
	d := c.Assistant.DeepWork
	if d == nil {
		return nil
	}
	for _, b := range []struct {
		bound assistantBound
		value int
	}{
		{deepTimeout, d.TimeoutSeconds},
		{deepSteps, d.MaxSteps},
		{deepTokens, d.MaxTokens},
	} {
		b.bound.check(path, b.value, &p)
	}
	llmPath := at(path, "llm")
	if d.LLM == "" {
		p.add(llmPath, ErrMissing, "deep work names no model: set it to a providers.llm "+
			"key whose entry has harness: claude-code")
		return p.err()
	}
	entry, ok := c.Providers.LLM[d.LLM]
	if !ok {
		p.add(llmPath, ErrUnknownValue, "%q is not a configured provider: providers.llm has %s",
			d.LLM, strings.Join(slices.Sorted(maps.Keys(c.Providers.LLM)), ", "))
		return p.err()
	}
	entryField := func(f string) Path { return at(entryPath(d.LLM), f) }
	// CLAUDE CODE ALONE. Both in-box runners continue a parked run in its
	// harness's own session after the approval, but deep work's run on the
	// engine host is confined around one harness's install and temp roots
	// (internal/sandbox's confinement, measured against Claude Code's), and
	// the engine drives it with that harness's runner: another harness there would
	// meet a confinement nobody measured it against.
	if Harness(entry.HarnessName()) != HarnessClaudeCode {
		p.add(llmPath, ErrConflict, "providers.llm.%s drives %s, and deep work runs on %s "+
			"alone: its run is confined around that harness's own install and temp "+
			"directories, which no other harness has been measured against. Set %s: %s",
			d.LLM, entry.HarnessName(), HarnessClaudeCode, entryField("harness"), HarnessClaudeCode)
	}
	if entry.RunIn != "" && entry.RunIn != PlacementDirect && !c.Providers.Sandbox.Configured(entry.RunIn) {
		p.add(entryField("run_in"), ErrConflict, "deep work on providers.llm.%s runs in %s, "+
			"which providers.sandbox configures no backend for: configure it, or remove "+
			"run_in to take the first of e2b and container the company configures, else "+
			"the engine host under confinement", d.LLM, entry.RunIn)
	}
	return p.err()
}

// ErrAssistantOff is what [Company.AssistantModel] answers for a company that
// assigns Ask Crewlet no model.
var ErrAssistantOff = errors.New("assistant.model is not set, so Ask Crewlet is off for this " +
	"company: set it to a providers.llm key")

// AssistantModel is the providers.llm key Ask Crewlet runs on, or why there is
// none: [ErrAssistantOff] for a company that assigns it none, and otherwise the
// problem admission reports for `assistant.model`.
//
// THE ONE PLACE THE MODEL IS HANDED OUT. Admission refuses a document whose
// model no helper can run on, but a revision is APPLIED under the runnable
// rules alone (see [Company.ValidateRunnable]), so a stored revision can still
// carry one. Whatever builds the assistant's model asks here on every turn
// rather than reading `Assistant.Model`, so such a revision leaves Ask Crewlet
// unavailable, with this error as the reason, and every seat running.
func (c *Company) AssistantModel() (string, error) {
	if !c.Assistant.On() {
		return "", ErrAssistantOff
	}
	if err := c.helperModelProblem(field("assistant.model"), c.Assistant.Model); err != nil {
		return "", err
	}
	return c.Assistant.Model, nil
}

// validateAssistant holds the `assistant:` block to its rules.
//
// AN ADMISSION RULE, every part of it: nothing about running a SEAT depends on
// the block, and the model's rule is enforced again where it is handed out
// ([Company.AssistantModel]), so applying a revision that breaks one of these
// is safe and refusing it would stop every seat over Ask Crewlet.
func (c *Company) validateAssistant() error {
	path := field("assistant")
	a := c.Assistant
	var p problems
	p.wrap(a.TokenBudget.validate(at(path, "token_budget")))
	if a.On() {
		p.wrap(c.helperModelProblem(at(path, "model"), a.Model))
	}
	p.wrap(c.deepWorkProblems(at(path, "deep_work")))
	p.wrap(a.Research.problems(at(path, "research")))
	p.wrap(c.searchProblems(at(path, "research.search")))
	return p.err()
}
