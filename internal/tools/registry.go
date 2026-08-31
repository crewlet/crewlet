// Package tools is the engine's tool registry and the per-phase surfaces built
// from it.
//
// It holds four kinds of tool under one contract — the builtins the engine
// ships, tools an embedding application hands it, tools an extension
// registers, and tools an MCP server serves — and records WHICH at
// registration, because that is the only frame that knows and the last that
// can say.
package tools

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/mcp"
)

// Callable, Result and Annotations are the MCP package's, reused rather than
// re-declared.
//
// Not because MCP is special, but because a second structurally-identical
// interface is a conversion at every boundary and two places for a signature
// to drift. The names are general — a builtin satisfies Callable without
// knowing MCP exists.
type (
	// Callable is anything the engine can offer a model and then run.
	Callable = mcp.Callable
	// Result is what one call produced.
	Result = mcp.Result
	// Annotations are a tool's behavioural hints, tri-state.
	Annotations = mcp.Annotations
)

// The tool-origin grammar: who put a tool in front of the agents.
//
// Recorded AT REGISTRATION because it cannot be recovered afterwards. A tool
// an MCP server serves is structurally identical to one the engine ships —
// same name, same schema, same call signature. With nothing recorded, the
// operator surface called both "builtin", and a tool missing because its
// server failed to start read as a missing builtin, which sends someone to
// debug the wrong subsystem.
//
// TWO registrants, and mcp/origin.go says why there are not four.
//
// These strings are a CONTRACT with that surface, which groups on them.
// Re-exported rather than redefined: one definition, in the package this one
// already depends on for Callable, Result and Annotations.
const (
	// OriginBuiltin marks a tool the engine itself ships.
	OriginBuiltin = mcp.OriginBuiltin

	// OriginMCPPrefix prefixes the bare name of the MCP server serving
	// the tool.
	OriginMCPPrefix = mcp.OriginMCPPrefix
)

// Origin is the origin for a tool served by an MCP server.
func Origin(server string) string { return mcp.Origin(server) }

// Entry is a registered tool with everything the engine knows about it.
type Entry struct {
	Tool Callable

	// Origin is the grammar above.
	Origin string

	// Annotations are the behavioural hints. Tri-state, and the tri-state
	// is the point: an UNANNOTATED tool is not a known read, and treating
	// unknown as read exempts most of a fresh MCP server from the delivery
	// fence.
	Annotations Annotations
}

// Name is the tool's catalogue name.
func (e Entry) Name() string { return e.Tool.Name() }

// FromMCP is the server name for an MCP-served tool, and false otherwise.
func (e Entry) FromMCP() (string, bool) {
	server, ok := strings.CutPrefix(e.Origin, OriginMCPPrefix)
	return server, ok
}

// Registry holds every tool the engine can offer.
//
// Safe for concurrent use: MCP instances register and unregister as servers
// restart, while turns read the catalogue on every round.
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Entry
	// order preserves registration order so a catalogue rendered for a
	// prompt is stable. A map range would reorder it every turn and
	// invalidate the provider's prefix cache on a prompt that did not
	// change.
	order []string
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Entry)}
}

// Clone returns an independent registry holding the same entries.
//
// A SEAT'S SURFACE STARTS AS THE COMPANY'S, and then diverges: every seat sees
// the builtins and the shared MCP servers, and only its own per-role children
// on top. One flat registry cannot express that, and the failure is not
// cosmetic — a `shared: false` server is a template that gives each role its
// own child holding that role's credentials, and two children of one template
// publish the SAME tool names. In one registry the second shadows the first,
// so every seat would call whichever won, acting under another seat's identity
// in the tracker or the chat backend, invisibly. See internal/mcp/instance.go.
//
// The entries are copied, not the tools: a Callable is shared by reference,
// which is what makes a clone per seat cheap rather than a duplicate surface.
func (r *Registry) Clone() *Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := &Registry{
		byName: make(map[string]Entry, len(r.byName)),
		order:  slices.Clone(r.order),
	}
	for name, e := range r.byName {
		out.byName[name] = e
	}
	return out
}

// ErrDuplicate reports a name already registered.
type ErrDuplicate struct{ Name, Existing, Incoming string }

func (e *ErrDuplicate) Error() string {
	return fmt.Sprintf("tools: %q is already registered by %s, cannot register from %s",
		e.Name, e.Existing, e.Incoming)
}

// Register adds a tool.
//
// A duplicate name is REFUSED rather than overwritten. Overwriting means the
// second registrant silently wins and the first's tool vanishes from every
// prompt — with the two being structurally identical, nothing downstream can
// even report which one the agent is now calling.
func (r *Registry) Register(tool Callable, origin string) error {
	return r.RegisterWith(tool, origin, Annotations{})
}

// RegisterWith adds a tool together with its annotations.
func (r *Registry) RegisterWith(tool Callable, origin string, ann Annotations) error {
	if tool == nil {
		return fmt.Errorf("tools: cannot register a nil tool")
	}
	name := tool.Name()
	if name == "" {
		return fmt.Errorf("tools: cannot register a tool with no name")
	}
	if origin == "" {
		return fmt.Errorf("tools: %q has no origin — the registration call is "+
			"the only frame that knows who is registering", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, dup := r.byName[name]; dup {
		return &ErrDuplicate{Name: name, Existing: existing.Origin, Incoming: origin}
	}
	r.byName[name] = Entry{Tool: tool, Origin: origin, Annotations: ann}
	r.order = append(r.order, name)
	return nil
}

// RegisterMCP adds a bridged MCP registration, carrying its annotations.
func (r *Registry) RegisterMCP(reg mcp.Registration) error {
	ann := Annotations{}
	if t, ok := reg.Tool.(*mcp.Tool); ok {
		ann = t.Annotations()
	}
	return r.RegisterWith(reg.Tool, reg.Origin, ann)
}

// Unregister removes a tool, reporting whether it was there.
func (r *Registry) Unregister(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byName[name]; !ok {
		return false
	}
	delete(r.byName, name)
	r.order = slices.DeleteFunc(r.order, func(n string) bool { return n == name })
	return true
}

// UnregisterOrigin removes every tool from one origin, returning their names.
//
// The names are captured BEFORE anything is removed. A server restart
// unregisters then re-registers, and computing the doomed set while mutating
// the map means the second half of it is decided by a map that is already
// changing underneath.
func (r *Registry) UnregisterOrigin(origin string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var doomed []string
	for _, name := range r.order {
		if r.byName[name].Origin == origin {
			doomed = append(doomed, name)
		}
	}
	for _, name := range doomed {
		delete(r.byName, name)
	}
	r.order = slices.DeleteFunc(r.order, func(n string) bool { return slices.Contains(doomed, n) })
	return doomed
}

// Lookup returns one entry.
func (r *Registry) Lookup(name string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.byName[name]
	return e, ok
}

// List returns every entry in registration order.
func (r *Registry) List() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.byName[name])
	}
	return out
}

// Names returns every registered name in registration order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.order)
}

// MCPNames returns the names of every MCP-served tool.
//
// The delivery gate reads this: a delivery to a shared surface only ever comes
// from an MCP server, so a first-party builtin called during recon can never
// stand in for one.
func (r *Registry) MCPNames() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, name := range r.order {
		if _, ok := r.byName[name].FromMCP(); ok {
			out = append(out, name)
		}
	}
	return out
}

// KnownReads returns the names POSITIVELY annotated read-only.
//
// Positively is the operative word and the reason this is not a filter over
// "not a write": an unannotated tool is not a known read, and treating unknown
// as read exempts most of a fresh MCP server from the delivery fence.
func (r *Registry) KnownReads() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, name := range r.order {
		if r.byName[name].Annotations.ReadOnly == mcp.Yes {
			out = append(out, name)
		}
	}
	return out
}

// ForOrigin returns a view whose Register calls all carry one origin.
//
// The registrant does not have to repeat itself, and — more to the point —
// cannot get it wrong: an extension handed the bare registry could register
// under "builtin" by omission, and the whole grammar exists to stop exactly
// that reading.
func (r *Registry) ForOrigin(origin string) *OriginView { return &OriginView{reg: r, origin: origin} }

// OriginView is a registry view bound to one origin.
type OriginView struct {
	reg    *Registry
	origin string
}

// Register adds a tool under this view's origin.
func (v *OriginView) Register(tool Callable) error { return v.reg.Register(tool, v.origin) }

// RegisterWith adds a tool with annotations under this view's origin.
func (v *OriginView) RegisterWith(tool Callable, ann Annotations) error {
	return v.reg.RegisterWith(tool, v.origin, ann)
}

// Unregister removes every tool this view registered.
func (v *OriginView) Unregister() []string { return v.reg.UnregisterOrigin(v.origin) }

// Catalogue renders the slim tool catalogue a planner is shown: first-party
// tools by name and one-line description, MCP servers by name only.
//
// MCP servers are named rather than expanded because a real server publishes
// dozens of tools and a planner shown all of them plans against a wall of
// text. Discovery is a tool call — which is also what keeps the prompt prefix
// stable while a server's catalogue changes underneath.
func (r *Registry) Catalogue() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var lines []string
	servers := map[string]bool{}
	var serverOrder []string
	for _, name := range r.order {
		e := r.byName[name]
		if server, ok := e.FromMCP(); ok {
			if !servers[server] {
				servers[server] = true
				serverOrder = append(serverOrder, server)
			}
			continue
		}
		lines = append(lines, CatalogueLine(name, e.Tool.Description()))
	}
	for _, server := range serverOrder {
		lines = append(lines, "- MCP server `"+server+"` (use the discovery tools to list its tools)")
	}
	if len(lines) == 0 {
		return "(no tools available)"
	}
	return strings.Join(lines, "\n")
}

// CatalogueLine renders one "- name: description" catalogue entry with the
// description whole; see [mcp.CatalogueLine] for why.
//
// Re-exported rather than reimplemented. The registry's catalogue and the
// discovery listings must render identically — a model sees both in one turn —
// and this package already imports internal/mcp, which the dependency edge
// does not allow in the other direction.
func CatalogueLine(name, description string) string {
	return mcp.CatalogueLine(name, description)
}

// Snapshot is an immutable view of the registry, taken once.
//
// A turn reads the catalogue on every round and a server restarting mid-turn
// would otherwise change what a phase is judged against between the call and
// the gate. Taking one snapshot per phase is what makes "the surface Execute
// reported" a fact rather than a moving target.
type Snapshot struct {
	entries []Entry
	byName  map[string]Entry
}

// Snapshot takes one.
func (r *Registry) Snapshot() Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s := Snapshot{
		entries: make([]Entry, 0, len(r.order)),
		byName:  make(map[string]Entry, len(r.order)),
	}
	for _, name := range r.order {
		e := r.byName[name]
		s.entries = append(s.entries, e)
		s.byName[name] = e
	}
	return s
}

// Lookup returns one entry from the snapshot.
func (s Snapshot) Lookup(name string) (Entry, bool) { e, ok := s.byName[name]; return e, ok }

// Names returns every name in the snapshot, in registration order.
func (s Snapshot) Names() []string {
	out := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.Name())
	}
	return out
}

// MCPNames returns the MCP-served names in the snapshot.
func (s Snapshot) MCPNames() []string {
	var out []string
	for _, e := range s.entries {
		if _, ok := e.FromMCP(); ok {
			out = append(out, e.Name())
		}
	}
	return out
}

// MCPServers returns the distinct MCP server names in the snapshot, sorted.
//
// The SERVERS rather than their tools, which is the vocabulary a tool-skill
// trigger is written in: an operator documents "how we use GitLab", not each
// of the forty tools that server publishes.
func (s Snapshot) MCPServers() []string {
	var out []string
	seen := map[string]bool{}
	for _, e := range s.entries {
		if server, ok := e.FromMCP(); ok && !seen[server] {
			seen[server] = true
			out = append(out, server)
		}
	}
	slices.Sort(out)
	return out
}

// KnownReads returns the positively read-only names in the snapshot.
func (s Snapshot) KnownReads() []string {
	var out []string
	for _, e := range s.entries {
		if e.Annotations.ReadOnly == mcp.Yes {
			out = append(out, e.Name())
		}
	}
	return out
}

// Entries returns every entry in the snapshot.
func (s Snapshot) Entries() []Entry { return slices.Clone(s.entries) }

// With returns a snapshot carrying one additional entry.
//
// A COPY, because the extra tool is per-phase state: Plan's submission tool
// and Review's are different objects belonging to different phases of one
// turn, and registering either into the shared registry would leave it visible
// to the other — or, worse, to the next turn, still holding the last one's
// answer.
//
// A name already in the snapshot is refused for the same reason Register
// refuses it: silently shadowing a real tool with a phase-local one means the
// model's call reaches something other than what the catalogue described.
func (s Snapshot) With(e Entry) (Snapshot, error) {
	if e.Tool == nil || e.Name() == "" {
		return Snapshot{}, fmt.Errorf("tools: cannot add an unnamed tool to a snapshot")
	}
	if _, dup := s.byName[e.Name()]; dup {
		return Snapshot{}, fmt.Errorf("tools: %q is already in the snapshot", e.Name())
	}
	out := Snapshot{
		entries: append(slices.Clone(s.entries), e),
		byName:  maps.Clone(s.byName),
	}
	if out.byName == nil {
		out.byName = map[string]Entry{}
	}
	out.byName[e.Name()] = e
	return out, nil
}
