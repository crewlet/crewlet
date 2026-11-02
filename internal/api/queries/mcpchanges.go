package queries

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/mcpreview"
)

// WHAT A SERVER CHANGED IN ITS TOOLS SINCE THEIR LEVELS WERE DECIDED
// (ADR-0078): the detail's `changes`, `tools[].held` and the rows of tools it
// stopped listing, and what `POST /mcp-servers/{name}/changes` decides by. One
// reading, [Sources.MCPReview], for both, over the same live listings and the
// same accepted definitions every node decides a call by
// (internal/mcpreview).

// MCPReview is what was accepted of one server's tools and what it changed
// since, by the live listings of it.
type MCPReview struct {
	Server string
	// Listings are the live nodes' listings of the server
	// ([coord.LiveMCPListings]).
	Listings []coord.MCPListing
	// Accepted is each tool's accepted definition, by the server's own name
	// for it; empty for a server whose first listing nobody has accepted.
	Accepted map[string]coord.MCPAcceptedTool
	// Changes are its tools' changes since ([mcpreview.Changes]).
	Changes []mcpreview.Change
	// Entry is the server's entry in the applied revision, nil for one it
	// does not declare.
	Entry *config.MCPServer
}

// MCPReview reads one server's review: [ErrMCPDetailUnavailable] on a node
// with no copy of the fleet's listings, any other error a copy or a presence
// this node could not read.
func (s Sources) MCPReview(ctx context.Context, name string) (MCPReview, error) {
	if s.MCPListings == nil || s.Coord == nil {
		return MCPReview{}, ErrMCPDetailUnavailable
	}
	leases, err := s.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return MCPReview{}, err
	}
	all, err := s.MCPListings.MCPListings(ctx)
	if err != nil {
		return MCPReview{}, err
	}
	accepted, err := s.MCPListings.MCPAccepted(ctx)
	if err != nil {
		return MCPReview{}, err
	}
	var listings []coord.MCPListing
	for _, l := range coord.LiveMCPListings(all, leases) {
		if l.Server == name {
			listings = append(listings, l)
		}
	}
	var entry *config.MCPServer
	if s.Company != nil {
		if company, _ := s.Company(); company != nil {
			if i := slices.IndexFunc(company.MCPServers, func(m config.MCPServer) bool { return m.Name == name }); i >= 0 {
				entry = &company.MCPServers[i]
			}
		}
	}
	return reviewOf(name, listings, accepted, entry), nil
}

// reviewOf is one server's review from the live listings of it, every accepted
// definition and its entry.
func reviewOf(name string, listings []coord.MCPListing, accepted []coord.MCPAcceptedTool,
	entry *config.MCPServer) MCPReview {

	out := MCPReview{Server: name, Listings: listings, Accepted: map[string]coord.MCPAcceptedTool{},
		Entry: entry}
	for _, a := range accepted {
		if a.Server == name {
			out.Accepted[a.Tool] = a
		}
	}
	out.Changes = mcpreview.Changes(out.Listings, out.Accepted)
	return out
}

// policy is the entry's tool levels, none for a server no revision declares.
func (r MCPReview) policy() config.MCPToolPolicy {
	if r.Entry == nil {
		return config.MCPToolPolicy{}
	}
	return r.Entry.Tools
}

// MCPChangeView is one change to a server's tools since their levels were
// decided: `changes[]` on the detail, the administrator's reading.
type MCPChangeView struct {
	// Tool is the server's own name for the tool, which a decision about
	// the change names it by.
	Tool string `json:"tool"`
	// Kinds is what changed: `added` or `removed` alone, or the parts of
	// the definition a change moved (`title`, `description`, `schema`,
	// `annotations`). ALWAYS A LIST.
	Kinds []string `json:"kinds"`
	// Before is the definition its level was decided on, null for a tool
	// the server added; After is the one listed now, null for a removed
	// one.
	Before *MCPAcceptedView `json:"before"`
	After  *MCPListedView   `json:"after"`
	// Loosened is a changed definition that claims less risk than the one
	// accepted ([mcpreview.Loosened]): read by the hints the server
	// declared, the direction a server would move a tool it wanted run
	// unasked.
	Loosened bool `json:"loosened"`
	// Held is whether the change holds the tool at Needs approval now: its
	// entry would let it run unasked, and it asks first until somebody
	// accepts the change ([mcpreview.Holds]).
	Held bool `json:"held"`
	// SeenAt is when the fleet first stored the definition listed now,
	// null for a removal and for a definition whose schemas are not stored.
	SeenAt *time.Time `json:"seen_at"`
}

// MCPDefinitionView is one definition of a tool, in the server's words.
type MCPDefinitionView struct {
	// Digest names it (internal/mcp's digest.go).
	Digest      string `json:"digest"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Annotations are the hints the server declared, no override applied:
	// the server's own claim, which is what a change is judged by; Risk,
	// RiskDeclared and External read from them ([mcp.RiskOf]).
	Annotations  ToolHints `json:"annotations"`
	Risk         mcp.Risk  `json:"risk"`
	RiskDeclared bool      `json:"risk_declared"`
	External     bool      `json:"external"`
	// InputSchema and OutputSchema are the definition's schemas, null
	// where none is stored.
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema"`
}

// MCPAcceptedView is the definition a tool's level was decided on.
type MCPAcceptedView struct {
	MCPDefinitionView
	// AcceptedBy is who accepted it, null for the server's first listing,
	// which was accepted as it was listed (FirstListing); AcceptedAt when.
	AcceptedBy   *string   `json:"accepted_by"`
	AcceptedAt   time.Time `json:"accepted_at"`
	FirstListing bool      `json:"first_listing"`
}

// MCPListedView is the definition a server lists for a tool now.
type MCPListedView struct {
	MCPDefinitionView
	// Instances is how many instances across the fleet list it.
	Instances int `json:"instances"`
}

// definitionView is one definition's words and reading.
func definitionView(digest, title, description string, declared coord.MCPToolHints,
	schema *coord.MCPToolSchema) MCPDefinitionView {

	hints := mcpreview.Annotations(declared)
	risk := mcp.RiskOf(hints)
	out := MCPDefinitionView{Digest: digest, Title: title, Description: description,
		Annotations: toolHintsOf(hints), Risk: risk.Risk, RiskDeclared: risk.Declared, External: risk.External}
	if schema != nil {
		out.InputSchema, out.OutputSchema = schemaObject(schema.Input), schemaObject(schema.Output)
	}
	return out
}

// toolHintsOf is a tool's four hints as words.
func toolHintsOf(h mcp.Annotations) ToolHints {
	return ToolHints{ReadOnly: h.ReadOnly.String(), Destructive: h.Destructive.String(),
		Idempotent: h.Idempotent.String(), OpenWorld: h.OpenWorld.String()}
}

// changeDigests is every digest a review's changes name, before and after, for
// the schemas shown beside them.
func changeDigests(changes []mcpreview.Change) []string {
	var out []string
	for _, c := range changes {
		if c.Before != nil && !c.Before.Removed {
			out = append(out, c.Before.Digest)
		}
		if c.After != nil && c.After.SchemaKept {
			out = append(out, c.After.Digest)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// changeViews is a review's changes as the detail answers them, with the
// schemas stored under their digests.
func changeViews(r MCPReview, schemas map[string]coord.MCPToolSchema) []MCPChangeView {
	out := make([]MCPChangeView, 0, len(r.Changes))
	policy := r.policy()
	for _, c := range r.Changes {
		view := MCPChangeView{Tool: c.Tool, Kinds: []string{}, Loosened: c.Loosened,
			Held: mcpreview.Holds(policy, c)}
		switch c.Kind {
		case mcpreview.KindAdded, mcpreview.KindRemoved:
			view.Kinds = append(view.Kinds, string(c.Kind))
		default:
			for _, p := range c.Parts {
				view.Kinds = append(view.Kinds, string(p))
			}
		}
		if b := c.Before; b != nil {
			var schema *coord.MCPToolSchema
			if s, ok := schemas[b.Digest]; ok {
				schema = &s
			}
			before := &MCPAcceptedView{
				MCPDefinitionView: definitionView(b.Digest, b.Title, b.Description, b.Hints, schema),
				AcceptedAt:        b.At, FirstListing: b.First,
			}
			if !b.First {
				by := b.By
				before.AcceptedBy = &by
			}
			view.Before = before
		}
		if a := c.After; a != nil {
			var schema *coord.MCPToolSchema
			if s, ok := schemas[a.Digest]; ok && a.SchemaKept {
				schema = &s
				seen := s.At
				view.SeenAt = &seen
			}
			view.After = &MCPListedView{
				MCPDefinitionView: definitionView(a.Digest, a.Title, a.Description, a.Declared, schema),
				Instances:         a.Instances,
			}
		}
		out = append(out, view)
	}
	return out
}

// reviewTools lays a review over the detail's tools: each tool a change holds
// answers its level as approve and says it is held, and each tool the server
// stopped listing keeps its row, named by what was accepted of it and marked
// removed.
func reviewTools(tools []MCPToolView, r MCPReview, schemas map[string]coord.MCPToolSchema) []MCPToolView {
	held := map[string]struct{}{}
	policy := r.policy()
	for _, c := range r.Changes {
		if mcpreview.Holds(policy, c) {
			held[c.Tool] = struct{}{}
		}
	}
	for i := range tools {
		if _, ok := held[tools[i].Raw]; ok && tools[i].Level == config.MCPToolAllow {
			tools[i].Level, tools[i].Held = config.MCPToolApprove, true
		}
	}
	prefix := ""
	if r.Entry != nil {
		prefix = r.Entry.ToolPrefix
	}
	for _, c := range r.Changes {
		if c.Kind != mcpreview.KindRemoved {
			continue
		}
		b := c.Before
		var schema *coord.MCPToolSchema
		if s, ok := schemas[b.Digest]; ok {
			schema = &s
		}
		def := definitionView(b.Digest, b.Title, b.Description, b.Hints, schema)
		starting := config.MCPToolLevel(mcp.StartingLevel(mcp.RiskOf(mcpreview.Annotations(b.Hints))))
		level, custom := policy.Level(b.Tool, starting)
		tools = append(tools, MCPToolView{
			Name: prefix + b.Tool, Raw: b.Tool, Title: def.Title, Description: def.Description,
			Annotations: def.Annotations, Risk: def.Risk, RiskDeclared: def.RiskDeclared,
			External: def.External, Digest: b.Digest, Nodes: []string{},
			InputSchema: def.InputSchema, OutputSchema: def.OutputSchema,
			Level: level, Custom: custom, Removed: true,
		})
	}
	slices.SortStableFunc(tools, func(a, b MCPToolView) int { return strings.Compare(a.Name, b.Name) })
	return tools
}
