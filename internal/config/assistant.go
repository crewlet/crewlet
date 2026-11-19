package config

import (
	"errors"
	"regexp"
	"slices"
	"sync"

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

// On reports whether the company assigned Ask Crewlet a model.
func (a Assistant) On() bool { return a.Model != "" }

// IsZero lets an unset block drop out of a JSON round trip.
func (a Assistant) IsZero() bool {
	return a.Model == "" && a.TokenBudget == (TokenBudget{}) && a.Research == nil
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
	p.wrap(a.Research.problems(at(path, "research")))
	p.wrap(c.searchProblems(at(path, "research.search")))
	return p.err()
}
