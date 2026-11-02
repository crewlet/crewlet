// One MCP server, whole: what it lists across the fleet.

package queries

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
)

// MCPListingReader is the fleet's listings of its MCP servers' tools
// (internal/coord's mcplisting.go) as this node holds them, and the schemas
// they name.
type MCPListingReader interface {
	// MCPListings is every node's listing of every server, whatever the
	// node's presence: the answer filters them by the presence it reads
	// ([coord.LiveMCPListings]). An error is a copy that has not been read
	// yet, never a fleet that lists nothing.
	MCPListings(ctx context.Context) ([]coord.MCPListing, error)

	// MCPToolSchemas reads the schemas stored under digests, leaving out a
	// digest the store holds nothing under.
	MCPToolSchemas(ctx context.Context, digests []string) (map[string]coord.MCPToolSchema, error)
}

// MCPServerAnswer is `GET /mcp-servers/{name}`: one server, whole.
type MCPServerAnswer struct {
	// Server is the server's row exactly as `mcp_servers_status` answers
	// it, and Nodes the live nodes its cells are drawn against.
	Server MCPServerRow    `json:"server"`
	Nodes  []MCPStatusNode `json:"nodes"`

	// Listed is each live node's listing of the server, by node: a node
	// running no instance of it that has listed has none. ALWAYS A LIST.
	Listed []MCPServerListing `json:"listed"`

	// Tools is every tool a live node's instances list, by name: each
	// once, its instances and nodes summed across the fleet. ALWAYS A
	// LIST. ToolCount is how many tools the server lists, as the node
	// counting the most counts them, never fewer than are here: a node
	// names at most [coord.MaxMCPListedTools].
	Tools     []MCPToolView `json:"tools"`
	ToolCount int           `json:"tool_count"`

	// ServerInfo is what the server said of itself in its handshake, and
	// Instructions what it offers a client for using it, both as the
	// listing changed last gave them: null and "" where no live node's
	// listing carries them. THE SERVER'S OWN WORDS, bounded
	// ([coord.MaxMCPServerInfoBytes], [coord.MaxMCPInstructionsBytes]).
	ServerInfo   *coord.MCPServerInfo `json:"server_info"`
	Instructions string               `json:"instructions"`

	// Dependants is what removing the server takes with it, exactly as a
	// dry run of `DELETE /config/mcp-servers/{name}` answers it
	// ([configapi.Service.ServerDependants]): empty lists for a server the
	// configuration does not declare.
	Dependants configapi.MCPDependants `json:"dependants"`

	// Instances is every instance of the server on the nodes said to hold
	// one, read from each node at query time (ADR-0021), by node then seat;
	// InstanceNodes names each node asked, whether it answered and why not,
	// and how many instances it holds, so a short list is never read as a
	// server with fewer instances. ALWAYS LISTS.
	Instances     []MCPInstanceView     `json:"instances"`
	InstanceNodes []MCPInstanceNodeView `json:"instance_nodes"`

	// LastSuccess is when an instance last answered, and LastFailure the
	// latest failure of any instance read, kept after it came back: null
	// where none has, or no node holding one answered.
	LastSuccess *time.Time      `json:"last_success"`
	LastFailure *MCPFailureView `json:"last_failure"`

	// Resolved is what the engine decides about the server from its
	// configuration: how it signs in, where it runs, the timeouts in force,
	// the integration it reads, the boxes it is given and the placements
	// expected to fail. Null for a server the configuration does not
	// declare, which has nothing to resolve.
	Resolved *MCPResolved `json:"resolved"`
}

// MCPServerListing is one live node's listing of the server.
type MCPServerListing struct {
	Node string `json:"node"`
	// At is when the node last changed its listing.
	At time.Time `json:"at"`
	// Instances is how many instances on the node the listing is the
	// union of, and Tools how many tools it counts.
	Instances int `json:"instances"`
	Tools     int `json:"tools"`
}

// MCPToolView is one tool the server lists.
type MCPToolView struct {
	// Name is the name it is offered under, the server's tool_prefix
	// applied; Raw is the server's own name for it.
	Name string `json:"name"`
	Raw  string `json:"raw"`
	// Title and Description are the server's words, Description cut to
	// [coord.MaxMCPToolDescriptionBytes].
	Title       string `json:"title"`
	Description string `json:"description"`
	// Annotations are its hints with the company's overrides applied, as
	// words, as [ToolRow.Annotations] carries them.
	Annotations ToolHints `json:"annotations"`
	// Risk is how much a call can change, RiskDeclared whether a hint the
	// tool carries decided it, and External whether a call may reach past
	// the company: the engine's one reading of the hints ([mcp.RiskOf]),
	// which a screen shows rather than reads again.
	Risk         mcp.Risk `json:"risk"`
	RiskDeclared bool     `json:"risk_declared"`
	External     bool     `json:"external"`
	// Digest names the definition shown (internal/mcp's digest.go), the
	// one most instances list; Definitions counts how many different
	// definitions the fleet lists under the name, 1 unless instances
	// disagree (a server mid-upgrade, or one that serves each seat its
	// own).
	Digest      string `json:"digest"`
	Definitions int    `json:"definitions"`
	// Instances is how many instances across the fleet list it, and Nodes
	// the nodes whose listing names it, sorted. ALWAYS A LIST.
	Instances int      `json:"instances"`
	Nodes     []string `json:"nodes"`
	// InputSchema and OutputSchema are the definition's schemas, null
	// where none is stored: an output schema the server publishes none
	// of, schemas past [coord.MaxMCPToolSchemaBytes], or ones a sweep
	// removed a moment ago.
	InputSchema  map[string]any `json:"input_schema"`
	OutputSchema map[string]any `json:"output_schema"`
}

// ErrMCPDetailUnavailable is a detail asked of a node that holds no copy of
// the fleet's listings or no configuration surface to read the dependants
// from: one that serves no engine.
var ErrMCPDetailUnavailable = errors.New("queries: this node keeps no copy of the fleet's MCP listings")

// MCPServer answers one server, whole: `GET /mcp-servers/{name}`.
//
// A ROUTE AND NOT A QUESTION, until a screen watches it: the question registry
// is for answers a room keeps current, and refuses one no room asks for.
//
// THE FLEET'S LISTINGS, not this node's bridges: a per-agent server's children
// run only on the nodes holding its seats, so this node may run none of them.
// Each live node's current listing counts, and only that
// ([coord.LiveMCPListings]): a node that died, drained or restarted stops
// counting with its presence, before any sweep removes its record.
//
// ITS INSTANCES ARE READ WHERE THEY LIVE: every node whose last beat reports
// an instance of it, or that carried no report, is asked at query time
// ([Sources.MCPInstances]), and each is named with whether it answered.
//
// THE CONFIGURATION READER'S, as `mcp_servers_status` is admin's and for its
// reason: the row names the launch command and the first line of each
// failure. [ErrBadParams] for an empty name, [ErrNotFound] for a server no
// node configures, reports or lists, [ErrMCPDetailUnavailable] on a node with
// no copy, and any other error is a store that could not be read or a read of
// the instances this node could not make.
func (s Sources) MCPServer(ctx context.Context, name string) (MCPServerAnswer, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return MCPServerAnswer{}, fmt.Errorf("%w: name must be an MCP server's configured name", ErrBadParams)
	}
	if s.MCPListings == nil || s.Config == nil || s.Coord == nil {
		return MCPServerAnswer{}, ErrMCPDetailUnavailable
	}
	leases, err := s.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return MCPServerAnswer{}, err
	}
	all, err := s.MCPListings.MCPListings(ctx)
	if err != nil {
		return MCPServerAnswer{}, err
	}
	var listings []coord.MCPListing
	for _, l := range coord.LiveMCPListings(all, leases) {
		if l.Server == name {
			listings = append(listings, l)
		}
	}
	status := s.mcpStatusOf(leases)
	at := slices.IndexFunc(status.Servers, func(r MCPServerRow) bool { return r.Name == name })
	if at < 0 && len(listings) == 0 {
		return MCPServerAnswer{}, fmt.Errorf("%w: no MCP server %q is configured, reported or listed", ErrNotFound, name)
	}
	out := MCPServerAnswer{
		Nodes: status.Nodes, Listed: []MCPServerListing{}, Tools: []MCPToolView{},
		Instances: []MCPInstanceView{}, InstanceNodes: []MCPInstanceNodeView{},
		Dependants: configapi.MCPDependants{MCPCleared: configapi.MCPCleared{
			MCPEnv: []configapi.MCPEnvDependant{}, Sandbox: []configapi.MCPSandboxDependant{},
		}, Secrets: []string{}},
	}
	if at >= 0 {
		out.Server = status.Servers[at]
	} else {
		// LISTED BY A NODE AND NEITHER CONFIGURED NOR REPORTED: a node
		// on another revision between two beats. The row says so.
		out.Server = MCPServerRow{Name: name, Title: name, Enabled: true, Args: []string{},
			Shared: listings[0].Shared, State: MCPNotStarted, UnlistedAnnotations: []string{},
			RefusedTools: []MCPRefusedTool{}, ShadowedTools: []MCPShadowedTool{}, Nodes: []MCPServerNode{}}
	}
	var organization *org.Organization
	if s.Company != nil {
		company, roster := s.Company()
		organization = roster
		if company != nil {
			if i := slices.IndexFunc(company.MCPServers, func(m config.MCPServer) bool { return m.Name == name }); i >= 0 {
				resolved := resolvedOf(company, &company.MCPServers[i])
				out.Resolved = &resolved
			}
		}
	}
	if out.Server.Configured {
		dependants, found, readErr := s.Config.ServerDependants(ctx, name)
		if readErr != nil {
			return MCPServerAnswer{}, readErr
		}
		if found {
			out.Dependants = dependants
		}
	}
	if s.MCPInstances != nil {
		read, readErr := s.MCPInstances.MCPInstances(ctx, name, instanceNodes(out.Server))
		if readErr != nil {
			return MCPServerAnswer{}, readErr
		}
		out.Instances, out.InstanceNodes, out.LastSuccess, out.LastFailure = instancesOf(read, organization)
	}
	for _, l := range listings {
		out.Listed = append(out.Listed, MCPServerListing{
			Node: l.Node, At: l.At, Instances: l.Instances, Tools: l.ToolCount,
		})
		out.ToolCount = max(out.ToolCount, l.ToolCount)
	}
	if latest := latestIdentified(listings); latest != nil {
		out.ServerInfo, out.Instructions = latest.Info, latest.Instructions
	}
	tools, kept := mergeListedTools(listings)
	schemas, err := s.MCPListings.MCPToolSchemas(ctx, kept)
	if err != nil {
		return MCPServerAnswer{}, err
	}
	for i := range tools {
		if schema, ok := schemas[tools[i].Digest]; ok {
			tools[i].InputSchema, tools[i].OutputSchema = schemaObject(schema.Input), schemaObject(schema.Output)
		}
	}
	out.Tools = tools
	out.ToolCount = max(out.ToolCount, len(out.Tools))
	return out, nil
}

// latestIdentified is the listing changed last that carries the server's
// identity, nil where none does.
func latestIdentified(listings []coord.MCPListing) *coord.MCPListing {
	var latest *coord.MCPListing
	for i := range listings {
		l := &listings[i]
		if l.Info == nil {
			continue
		}
		if latest == nil || l.At.After(latest.At) {
			latest = l
		}
	}
	return latest
}

// mergeListedTools is every tool the listings name, each once by name, sorted:
// the definition most instances list shown, the instances and nodes summed.
// Its schemas are left for the caller to read, under kept: the digests shown
// whose schemas a listing says are stored, sorted, so a definition listed
// without them costs no read of the store.
func mergeListedTools(listings []coord.MCPListing) (views []MCPToolView, kept []string) {
	type definition struct {
		tool      coord.MCPListedTool
		instances int
		at        time.Time
	}
	byName := map[string]map[string]*definition{}
	nodes := map[string]map[string]struct{}{}
	for _, l := range listings {
		for _, t := range l.Tools {
			if byName[t.Name] == nil {
				byName[t.Name], nodes[t.Name] = map[string]*definition{}, map[string]struct{}{}
			}
			d := byName[t.Name][t.Digest]
			if d == nil {
				d = &definition{tool: t}
				byName[t.Name][t.Digest] = d
			}
			d.instances += t.Instances
			if l.At.After(d.at) {
				d.at = l.At
			}
			nodes[t.Name][l.Node] = struct{}{}
		}
	}
	out := make([]MCPToolView, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		definitions := slices.Collect(maps.Values(byName[name]))
		// THE ONE MOST INSTANCES LIST, then the one listed most recently,
		// then by digest, so every node answers the same definition.
		slices.SortFunc(definitions, func(a, b *definition) int {
			return cmp.Or(cmp.Compare(b.instances, a.instances), b.at.Compare(a.at),
				cmp.Compare(a.tool.Digest, b.tool.Digest))
		})
		instances := 0
		for _, d := range definitions {
			instances += d.instances
		}
		shown := definitions[0].tool
		if shown.SchemaKept {
			kept = append(kept, shown.Digest)
		}
		hints := annotationsOf(shown.Hints)
		risk := mcp.RiskOf(hints)
		out = append(out, MCPToolView{
			Name: shown.Name, Raw: shown.Raw, Title: shown.Title, Description: shown.Description,
			Annotations: ToolHints{
				ReadOnly: hints.ReadOnly.String(), Destructive: hints.Destructive.String(),
				Idempotent: hints.Idempotent.String(), OpenWorld: hints.OpenWorld.String(),
			},
			Risk: risk.Risk, RiskDeclared: risk.Declared, External: risk.External,
			Digest: shown.Digest, Definitions: len(definitions), Instances: instances,
			Nodes: slices.Sorted(maps.Keys(nodes[name])),
		})
	}
	slices.Sort(kept)
	return out, slices.Compact(kept)
}

// annotationsOf reads a listing's hint words back into the engine's tri-state.
// A word this build does not write is unknown: never read as an assertion.
func annotationsOf(h coord.MCPToolHints) mcp.Annotations {
	word := func(w string) mcp.Hint {
		switch w {
		case coord.HintYes:
			return mcp.Yes
		case coord.HintNo:
			return mcp.No
		}
		return mcp.Unknown
	}
	return mcp.Annotations{
		ReadOnly: word(h.ReadOnly), Destructive: word(h.Destructive),
		Idempotent: word(h.Idempotent), OpenWorld: word(h.OpenWorld),
	}
}

// schemaObject decodes a stored schema, nil for none or one that is not a
// JSON object.
func schemaObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}
