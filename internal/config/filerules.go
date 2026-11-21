package config

import (
	"errors"
	"iter"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// THE RULES ONLY A WHOLE DOCUMENT CAN CHECK.
//
// Every other rule here is a property of the thing it is written beside. The
// five here compare one part of the document against another, or one seat
// against every other, so they run together over the document whole.
//
// They are ADMISSION rules: none stops a company running, so a stored revision
// from a build that never checked may carry any of them and must keep running
// on upgrade. A SUBMITTED document is refused and a STORED one is reported by
// [Company.AdmissionWarnings], the class every rule added after companies
// existed belongs to. See [Company.ValidateRunnable].

// validateFileRules is the five whole-document rules, run together.
func (c *Company) validateFileRules() error {
	return errors.Join(
		c.validateMCPServerReferences(),
		c.validateOptionalKeys(),
		c.validateHeaderSpellings(),
		c.validateSeatEmails(),
		c.validateReferenceShapes(),
	)
}

// validateOptionalKeys refuses an `optional_keys` entry that leaves nothing
// out.
//
// A listed key may resolve to nothing without holding the server back
// ([MCPServer.OptionalKeys]). An entry naming no key the server is started with
// is never consulted, so the key it was MEANT for stays required while the
// document reads as if the server could run without it. Told apart by fix: a
// VARIABLE's name where a key's belongs, a variable only the url references, a
// name nothing carries. WHOLE-DOCUMENT because a seat's or unit's `mcp_env`
// block lands in the same environment or headers, so a key only seats write is
// still one the server reads. An entry whose values carry no reference is refused
// too: nothing in it can resolve to nothing.
func (c *Company) validateOptionalKeys() error {
	var p problems
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		if len(server.OptionalKeys) == 0 {
			continue
		}
		held := heldKeys{fold: server.ValueKey, url: envref.Names(server.URL)}
		held.settings(server.Env)
		held.settings(server.Headers)
		for role := range c.EachRole() {
			held.settings(role.MCPEnv[server.Name])
		}
		for unit := range c.EachUnit() {
			held.settings(unit.MCPEnv[server.Name])
		}
		own := "this server's env"
		if server.Kind() == TransportHTTP {
			own = "this server's headers"
		}
		held.check(&p, at(idx(field("mcp_servers"), i), "optional_keys"), server.OptionalKeys,
			own+", nor of any seat's or unit's mcp_env block for it", "the server is not started")
	}
	return p.err()
}

// validateRunOptionalKeys holds an executor box's `optional_keys` lists — a
// seat's sandbox block's, over its env, and every setup step's, over its env
// and its files — to the rule a tool server's list is held to
// ([Company.validateOptionalKeys]): an entry that leaves nothing out is
// refused, because the key it was meant for stays expected, and a run is
// logged as starting without it wherever its value resolves to nothing.
//
// AN ADMISSION RULE like that one: nothing about running depends on it, since
// an entry naming nothing is merely never consulted.
func (c *Company) validateRunOptionalKeys() error {
	var p problems
	steps := func(path Path, list []SandboxSetupStep) {
		for i := range list {
			step := &list[i]
			if len(step.OptionalKeys) == 0 {
				continue
			}
			held := heldKeys{}
			held.settings(step.Env)
			held.contents(step.Files)
			held.check(&p, at(idx(path, i), "optional_keys"), step.OptionalKeys,
				"this step's env, nor a path of its files", runStartsWithout)
		}
	}
	if c.Providers.Sandbox != nil {
		steps(field("providers.sandbox.setup"), c.Providers.Sandbox.Setup)
	}
	for role, path := range c.EachRole() {
		gate := role.Sandbox
		if gate == nil {
			continue
		}
		if len(gate.OptionalKeys) > 0 {
			held := heldKeys{}
			held.settings(gate.Env)
			held.check(&p, at(path, "sandbox.optional_keys"), gate.OptionalKeys,
				"this sandbox block's env", runStartsWithout)
		}
		steps(at(path, "sandbox.setup"), gate.Setup)
	}
	return p.err()
}

// runStartsWithout is what happens to an executor run whose environment holds
// a reference that resolves to nothing and is not optional, for the sentence
// [heldKeys.check] writes: the run is the seat's turn itself, so it starts,
// and the gap is logged for the operator to find (the engine's runEnvironment).
const runStartsWithout = "the seat's executor run starts without it and logs " +
	"the gap as sandbox_env_unresolved"

// heldKeys is what an `optional_keys` list is checked against: for each key
// its values are written under, whether one of them carries a reference, and
// for each variable those values reference, the keys it is written under.
type heldKeys struct {
	// fold is how two spellings are told to name one key — an http
	// server's headers by [MCPServer.ValueKey] — or nil where the key is
	// as written.
	fold func(string) string
	// url are the variables a server's url references, which no key can
	// leave out.
	url []string

	referenced map[string]bool
	keysOf     map[string][]string
}

// settings carries a map of settings, where a `${VAR}` anywhere in a value is
// a reference.
func (h *heldKeys) settings(block map[string]string) {
	for key, value := range block {
		h.carry(key, envref.Names(value))
	}
}

// contents carries a step's files, keyed by path, where a body is a reference
// only when it is exactly one whole `${VAR}` — any other `${…}` in it is the
// file's own syntax ([secrets.ReadContent]).
func (h *heldKeys) contents(files map[string]string) {
	for path, body := range files {
		var names []string
		if name, whole := envref.Whole(body); whole {
			names = []string{name}
		}
		h.carry(path, names)
	}
}

func (h *heldKeys) carry(key string, names []string) {
	if h.referenced == nil {
		h.referenced, h.keysOf = map[string]bool{}, map[string][]string{}
	}
	id := h.id(key)
	h.referenced[id] = h.referenced[id] || len(names) > 0
	for _, name := range names {
		if !slices.Contains(h.keysOf[name], key) {
			h.keysOf[name] = append(h.keysOf[name], key)
		}
	}
}

// id is the key key names.
func (h *heldKeys) id(key string) string {
	if h.fold == nil {
		return key
	}
	return h.fold(key)
}

// check refuses each entry of listed, an `optional_keys` list written at path,
// that leaves nothing out — told apart, because their fixes are: a VARIABLE's
// name where its key belongs, a variable the url needs, a name nothing
// carries, a key whose values reference nothing, and an entry listed twice or
// empty. among says where a key was looked for, and refused what happens
// without its value.
func (h *heldKeys) check(p *problems, path Path, listed []string, among, refused string) {
	seen := make(map[string]struct{}, len(listed))
	for j, key := range listed {
		where := idx(path, j)
		if strings.TrimSpace(key) == "" {
			p.add(where, ErrMissing, "an optional key names the key of a value to leave out")
			continue
		}
		id := h.id(key)
		if _, dup := seen[id]; dup {
			p.add(where, ErrConflict, "%q is listed twice", key)
			continue
		}
		seen[id] = struct{}{}
		has, carried := h.referenced[id]
		switch {
		case !carried && len(h.keysOf[key]) > 0:
			keys := slices.Clone(h.keysOf[key])
			sort.Strings(keys)
			p.add(where, ErrUnknownValue,
				"%q is a variable these values reference, not a key: list the key it "+
					"is written under (%s), which is what is left out when its value "+
					"resolves to nothing", key, strings.Join(keys, ", "))
		case !carried && slices.Contains(h.url, key):
			p.add(where, ErrUnknownValue,
				"%q is a variable the url references, and the url is never optional: "+
					"a server is dialled at it, so %s wherever %q resolves to nothing "+
					"whatever this list says", key, refused, key)
		case !carried:
			p.add(where, ErrUnknownValue,
				"%q is not a key of %s: nothing is left out for it, and the key it was "+
					"meant for stays required, so %s wherever that key's value resolves "+
					"to nothing", key, among, refused)
		case !has:
			p.add(where, ErrUnknownValue,
				"no value written under %q carries a ${VAR} reference, so nothing in it "+
					"can resolve to nothing: take it out of optional_keys", key)
		}
	}
}

// validateHeaderSpellings refuses one http header spelled two ways where the
// engine cannot tell which value is meant.
//
// HTTP NAMES A HEADER IN ANY CASE, and a tool server is sent one value for it.
// The engine lays a seat's values over the server's own by case-folded key
// ([MCPServer.ValueKey]), but two spellings in ONE PLACE (two keys of one map,
// or a unit's block and a member's, which [org.MCPEnv.WithDefaults] lays one
// under the other by the key as written) leave nothing to say which wins.
// Only an http server's (an environment does tell `token` from `TOKEN`) and only
// within one place, because seats start apart and two spelling one header
// differently send one value each.
func (c *Company) validateHeaderSpellings() error {
	var p problems
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		spelledTwice(&p, server, at(idx(field("mcp_servers"), i), "headers"), server.Headers)
		for role, path := range c.EachRole() {
			spelledTwice(&p, server, entry(at(path, "mcp_env"), server.Name), role.MCPEnv[server.Name])
		}
		for unit, path := range c.EachUnit() {
			block := unit.MCPEnv[server.Name]
			spelledTwice(&p, server, entry(at(path, "mcp_env"), server.Name), block)
			for j := range unit.Roles {
				spelledUnlike(&p, server,
					entry(at(idx(at(path, "roles"), j), "mcp_env"), server.Name),
					unit.Roles[j].MCPEnv[server.Name], block)
			}
		}
	}
	return p.err()
}

// spelledTwice refuses two keys of block, written at path, naming one of
// server's headers — each after the first, in key order.
func spelledTwice(p *problems, server *MCPServer, path Path, block map[string]string) {
	first := make(map[string]string, len(block))
	for _, key := range slices.Sorted(maps.Keys(block)) {
		id := server.ValueKey(key)
		if was, twice := first[id]; twice {
			p.add(entry(path, key), ErrConflict,
				"%q and %q name one header, which HTTP does not tell apart by case, so "+
					"which of the two values is sent is not decided: write it once", key, was)
			continue
		}
		first[id] = key
	}
}

// spelledUnlike refuses a key of a seat's block, written at path, naming the
// header its unit's block writes under another spelling.
func spelledUnlike(p *problems, server *MCPServer, path Path, block, unit map[string]string) {
	if len(block) == 0 || len(unit) == 0 {
		return
	}
	spelled := make(map[string]string, len(unit))
	for key := range unit {
		spelled[server.ValueKey(key)] = key
	}
	for _, key := range slices.Sorted(maps.Keys(block)) {
		if theirs, ok := spelled[server.ValueKey(key)]; ok && theirs != key {
			p.add(entry(path, key), ErrConflict,
				"%q names the header its unit's block writes as %q: a seat's value "+
					"replaces its unit's only where the two are spelled alike, so as written "+
					"the seat's child would send either one: spell them the same", key, theirs)
		}
	}
}

// MCPReferenceField is the kind of field an [MCPServerReference] is written in.
type MCPReferenceField string

const (
	// MCPReferenceEnv is an `mcp_env` block keyed on the server's name: a
	// seat's own, or a unit's, which the unit's agent seats inherit.
	MCPReferenceEnv MCPReferenceField = "mcp_env"
	// MCPReferenceSandbox is an entry of a seat's `sandbox.mcp.servers`,
	// which gives the seat's executor box the server.
	MCPReferenceSandbox MCPReferenceField = "sandbox"
	// MCPReferenceLimits is an `mcp_limits` block keyed on the server's
	// name: a seat's own, or a unit's, which holds every agent seat beneath
	// it.
	MCPReferenceLimits MCPReferenceField = "mcp_limits"
)

// MCPServerReference is one field of the document that names an MCP server by
// its name: where it is written, whose it is, and what kind of field it is.
type MCPServerReference struct {
	// Server is the name the field names, as written.
	Server string
	// Field is the kind of field.
	Field MCPReferenceField
	// Seat is the handle of the seat whose field it is, and Unit the key of
	// the unit whose `mcp_env` or `mcp_limits` block it is: exactly one of
	// the two is set.
	Seat, Unit string
	// Name is that seat's or unit's display name, for a sentence a person
	// reads.
	Name string
	// Segments is the written field: ["roles", 0, "mcp_env", "linear"],
	// ["units", 1, "children", 0, "mcp_env", "linear"] or
	// ["units", 0, "roles", 2, "sandbox", "mcp", "servers", 1].
	Segments Path
	// EngineRead is an `mcp_env` block the engine reads by its name whether
	// or not a server of that name is declared ([org.EngineReadMCPEnv]): a
	// GitHub App's per-seat token, a GitLab or Atlassian account. Such a
	// block names something with no server at all, so it is never a
	// server's alone, and removing the server leaves it in place.
	EngineRead bool
}

// MCPServerReferences is every field of the document that names the MCP server
// called name, in document order: each seat's `mcp_env` and `mcp_limits` blocks
// keyed on it and each entry of its `sandbox.mcp.servers` listing it, at any
// depth, then each unit's `mcp_env` and `mcp_limits` blocks keyed on it.
//
// THE WRITTEN FIELDS, whether or not a server of that name is declared or on: a
// block kept for a server turned off is still what a removal has to clear and
// what [Company.validateMCPServerReferences] reports once the server is gone.
// ONE IMPLEMENTATION with every reader: that rule and the removal of a server
// (`DELETE /config/mcp-servers/{name}`), which clears what this answers in the
// same revision or is refused naming all of it.
func (c *Company) MCPServerReferences(name string) []MCPServerReference {
	var out []MCPServerReference
	for _, ref := range c.mcpServerReferences() {
		if ref.Server == name {
			out = append(out, ref)
		}
	}
	return out
}

// MCPServerDependants is every field that names the server called name and
// nothing else, in document order: its [Company.MCPServerReferences] but a
// block the engine reads by its name besides ([MCPServerReference.EngineRead]).
// It is what [Company.RemoveMCPServer] clears with the server.
//
// A BLOCK THE ENGINE READS BY ITS NAME IS NOT ONE: a seat's `mcp_env.github`
// is its GitHub App token as much as the `github` server's credentials, and
// the engine goes on reading it once the server is gone.
func (c *Company) MCPServerDependants(name string) []MCPServerReference {
	var out []MCPServerReference
	for _, ref := range c.MCPServerReferences(name) {
		if !ref.EngineRead {
			out = append(out, ref)
		}
	}
	return out
}

// RemoveMCPServer takes the server called name out of the document with every
// field that names it alone ([Company.MCPServerDependants]): each seat's and
// unit's `mcp_env` and `mcp_limits` blocks for it and each
// `sandbox.mcp.servers` entry listing it. It answers what it cleared, and
// whether the server was declared; a server that is not leaves the document as
// it was. A mapping left empty is dropped rather than written as `{}`.
//
// A LIMIT GOES WITH ITS SERVER whatever its name: unlike an `mcp_env` block
// the engine reads by name, nothing reads a limit on a server nobody declares,
// and one left behind would hold back a later server that took the name.
func (c *Company) RemoveMCPServer(name string) (cleared []MCPServerReference, found bool) {
	i := slices.IndexFunc(c.MCPServers, func(s MCPServer) bool { return s.Name == name })
	if i < 0 {
		return nil, false
	}
	cleared = c.MCPServerDependants(name)
	c.MCPServers = slices.Delete(c.MCPServers, i, i+1)
	alone := !engineReadsMCPEnv(name)
	drop := func(env org.MCPEnv) org.MCPEnv {
		if !alone {
			return env
		}
		delete(env, name)
		if len(env) == 0 {
			return nil
		}
		return env
	}
	unlimit := func(limits org.MCPLimits) org.MCPLimits {
		delete(limits, name)
		if len(limits) == 0 {
			return nil
		}
		return limits
	}
	for role := range c.EachRole() {
		role.MCPEnv = drop(role.MCPEnv)
		role.MCPLimits = unlimit(role.MCPLimits)
		if role.Sandbox == nil {
			continue
		}
		role.Sandbox.MCP.Servers = slices.DeleteFunc(role.Sandbox.MCP.Servers,
			func(server string) bool { return server == name })
		if len(role.Sandbox.MCP.Servers) == 0 {
			role.Sandbox.MCP.Servers = nil
		}
		if reflect.ValueOf(*role.Sandbox).IsZero() {
			role.Sandbox = nil
		}
	}
	for unit := range c.EachUnit() {
		unit.MCPEnv = drop(unit.MCPEnv)
		unit.MCPLimits = unlimit(unit.MCPLimits)
	}
	return cleared, true
}

// engineReadsMCPEnv reports an `mcp_env` block the engine reads by its name
// whatever servers are declared ([org.EngineReadMCPEnv]). The one decision of
// it, which [MCPServerReference.EngineRead] and [Company.RemoveMCPServer] both
// read.
func engineReadsMCPEnv(server string) bool {
	return slices.Contains(org.EngineReadMCPEnv(), server)
}

// mcpServerReferences is every written field that names an MCP server,
// whichever server it names: [Company.MCPServerReferences] before it is
// narrowed to one.
//
// A BLOCK'S KEYS SORTED, because a map's iteration order is random and an
// error listing three undeclared servers in a different order on every run is
// one nobody can diff a CI log against.
func (c *Company) mcpServerReferences() []MCPServerReference {
	var out []MCPServerReference
	blocks := func(seat, unit, name string, path Path, env org.MCPEnv) {
		for _, server := range slices.Sorted(maps.Keys(env)) {
			out = append(out, MCPServerReference{
				Server: server, Field: MCPReferenceEnv, Seat: seat, Unit: unit, Name: name,
				Segments:   entry(at(path, "mcp_env"), server),
				EngineRead: engineReadsMCPEnv(server),
			})
		}
	}
	limits := func(seat, unit, name string, path Path, limits org.MCPLimits) {
		for _, server := range slices.Sorted(maps.Keys(limits)) {
			out = append(out, MCPServerReference{
				Server: server, Field: MCPReferenceLimits, Seat: seat, Unit: unit, Name: name,
				Segments: entry(at(path, "mcp_limits"), server),
			})
		}
	}
	for role, path := range c.EachRole() {
		seat := role.IdentityKey()
		blocks(seat, "", role.Name, path, role.MCPEnv)
		limits(seat, "", role.Name, path, role.MCPLimits)
		if role.Sandbox == nil {
			continue
		}
		for i, server := range role.Sandbox.MCP.Servers {
			out = append(out, MCPServerReference{
				Server: server, Field: MCPReferenceSandbox, Seat: seat, Name: role.Name,
				Segments: idx(at(path, "sandbox.mcp.servers"), i),
			})
		}
	}
	for unit, path := range c.EachUnit() {
		blocks("", unit.IdentityKey(), unit.Name, path, unit.MCPEnv)
		limits("", unit.IdentityKey(), unit.Name, path, unit.MCPLimits)
	}
	return out
}

// validateMCPServerReferences refuses a field that names an MCP server nothing
// declares: an `mcp_env` or `mcp_limits` block keyed on one, or a
// `sandbox.mcp.servers` entry listing one.
//
// The engine reads a seat's `mcp_env` from the server end, walking `mcp_servers`
// (internal/engine/mcp.go), so a key naming no server is never read and has no
// run-time symptom: credentials look configured while the tool server starts
// without them. A box is given declared servers only (internal/engine/sandbox.go).
// THE VENDOR BLOCKS ARE NAMED EVEN WITH NO SERVER: the engine reads six `mcp_env`
// blocks directly by name, so an empty `mcp_servers:` is ordinary. The set comes
// from [org.EngineReadMCPEnv] through [MCPServerReference.EngineRead], never a
// private copy. [ToolServerName] needs no case: no server may bear it.
func (c *Company) validateMCPServerReferences() error {
	// KEPT TO THE COMPANY'S OWN SERVERS, which is what the message lists:
	// telling an operator who misspelled one of their two servers that the
	// allowed set also holds `datadog` and `confluence` describes the engine
	// rather than their file.
	declared := make(map[string]struct{}, len(c.MCPServers))
	signedIn := map[string]bool{}
	for i := range c.MCPServers {
		if name := strings.TrimSpace(c.MCPServers[i].Name); name != "" {
			declared[name] = struct{}{}
			signedIn[name] = c.MCPServers[i].OAuth != nil
		}
	}
	// RENDERED ONCE, for every refusal below: a document misspelling ten
	// references sorts the same set once rather than ten times.
	servers := listOrNone(slices.Sorted(maps.Keys(declared)), "no servers at all")
	var p problems
	for _, ref := range c.mcpServerReferences() {
		// A BOX CANNOT RENEW A SIGN-IN. It is handed a server's values
		// once, at its launch, and an OAuth server's access token runs out
		// within the hour while only the engine holds what renews it; the
		// box reaches the server's tools through the bridge instead.
		if ref.Field == MCPReferenceSandbox && signedIn[ref.Server] {
			p.add(ref.Segments, ErrConflict,
				"%q is signed in to with OAuth, and an executor box is handed a server's "+
					"credentials once, at its launch, which an access token outlives by "+
					"an hour at most: remove the entry, and the box reaches the server's "+
					"tools through the bridge", ref.Server)
			continue
		}
		if _, ok := declared[ref.Server]; ok || ref.EngineRead {
			continue
		}
		switch ref.Field {
		case MCPReferenceEnv:
			p.add(ref.Segments, ErrUnknownValue,
				"%q is not a declared tool server: mcp_servers has %s. "+
					"Nothing reads this block: the engine looks the "+
					"credentials up per SERVER, so a key naming none is never "+
					"consulted and the server it was meant for starts with "+
					"none", ref.Server, servers)
		case MCPReferenceSandbox:
			p.add(ref.Segments, ErrUnknownValue,
				"%q is not a declared tool server: mcp_servers has %s. "+
					"A box is given only a server the company declares, so this "+
					"entry gives the seat's executor nothing: name a declared "+
					"server, or remove the entry", ref.Server, servers)
		case MCPReferenceLimits:
			p.add(ref.Segments, ErrUnknownValue,
				"%q is not a declared tool server: mcp_servers has %s. A limit "+
					"holds back a server the company declares, and one naming none "+
					"would hold back whichever server later takes the name: name a "+
					"declared server, or remove the entry", ref.Server, servers)
		}
	}
	return p.err()
}

// listOrNone renders names for a message, or none where there are none.
//
// The caller states its own empty case, because a list followed by nothing —
// "mcp_servers has ", "the units are: " — is the shape of message that makes
// an operator think the renderer broke rather than that their document
// declares none, and what "none" reads as differs by list. The names go in
// the order given: a caller holding a SET sorts it first, since a map's order
// is random and a message that changes on every run is one nobody can diff a
// CI log against.
func listOrNone(names []string, none string) string {
	if len(names) == 0 {
		return none
	}
	return strings.Join(names, ", ")
}

// validateSeatEmails refuses two seats declaring one email address.
//
// THE PARTY REGISTRY INDEXES SEATS BY EMAIL AND THE FIRST ONE WINS
// (internal/notify/registry.go): a second seat claiming an address is dropped
// from that index, so mail for one person reaches another and the shadowed one
// has no symptom. [org.Organization.validateContactIdentities] is the same rule
// for every EXTERNAL identity. Compared IN THE FORM THE REGISTRY MATCHES ON,
// [iam.NormalizeEmail] (lower-cased, plus tag dropped), so `Ada@example.com` and
// `ada+ops@example.com` do not pass a document the registry collapses onto one
// mailbox; a whole `${VAR}` is compared as written, as validation resolves
// nothing. REPORTED AT EVERY SEAT THAT HOLDS THE ADDRESS: an operator must
// decide which keeps it, and a message naming one tells them nothing of the other.
func (c *Company) validateSeatEmails() error {
	type holder struct {
		name string
		path Path
	}
	// The order seats were met in, kept separately, so the report follows
	// the document rather than a map.
	var order []string
	byEmail := map[string][]holder{}
	for role, path := range c.EachRole() {
		email := strings.TrimSpace(role.Email)
		if _, isRef := envref.Whole(email); !isRef {
			email = iam.NormalizeEmail(email)
		}
		if email == "" {
			continue
		}
		if _, seen := byEmail[email]; !seen {
			order = append(order, email)
		}
		byEmail[email] = append(byEmail[email], holder{name: role.Name, path: path})
	}

	var p problems
	for _, email := range order {
		holders := byEmail[email]
		if len(holders) < 2 {
			continue
		}
		names := make([]string, 0, len(holders))
		for _, h := range holders {
			names = append(names, h.name)
		}
		for _, h := range holders {
			p.add(at(h.path, "email"), ErrConflict,
				"%d seats declare the email %q (%s; compared lower-cased with "+
					"any plus tag dropped). The party registry keys on that "+
					"form and the first seat wins, so all but one of these "+
					"silently stops being findable by address, and mail meant for "+
					"one person resolves to another. Give each seat its own "+
					"address",
				len(holders), email, strings.Join(names, ", "))
		}
	}
	return p.err()
}

// validateReferenceShapes refuses a `lead:`, `unit:` or `manages:` value that
// is not shaped like the identity it has to name.
//
// It checks the SHAPE, never the resolution. A well-formed reference that
// resolves to nothing stays a WARNING ([Company.ReferenceWarnings]): a chart is
// assembled in pieces, so a `lead:` naming a seat not yet hired must not refuse
// the document. What is refused could never resolve under any chart, such as a
// stale display name (`lead: QA Lead`; seats are addressed by handle, units by
// key), a typo the shape proves and a dangling-reference list would bury.
// A `lead:` takes [org.ValidHandle]. A root seat's `unit:` takes the key grammar
// [entityID]. A `manages:` entry names EITHER (`manages: [engineering]` expands
// to a division's every seat), so it is refused only when it is neither.
func (c *Company) validateReferenceShapes() error {
	var p problems

	for unit, path := range c.EachUnit() {
		lead := strings.TrimSpace(unit.Lead)
		if lead != "" && !org.ValidHandle(lead) {
			p.add(at(path, "lead"), ErrShape,
				"%q is not a seat handle: %s. A unit's lead names the seat's "+
					"HANDLE, not its display name. %q resolves to nobody, so "+
					"this unit has no lead, every escalation from it reaches "+
					"the parent's, and its lead-targeted schedules never fire",
				lead, handleRule, lead)
		}
	}

	for role, path := range c.EachRole() {
		if ref := strings.TrimSpace(role.Unit); ref != "" && !entityID().MatchString(ref) {
			p.add(at(path, "unit"), ErrShape,
				"%q is not a unit key: %s. A seat declared at the org root "+
					"joins its unit by KEY (the unit's `id:`), not by the "+
					"team's display name. %q matches no key, so this seat "+
					"stays at the root, outside the unit's credentials and "+
					"invisible to its lead", ref, unitKeyRule, ref)
		}
		for i, target := range role.Manages {
			target = strings.TrimSpace(target)
			if target == "" || org.ValidHandle(target) || entityID().MatchString(target) {
				continue
			}
			p.add(idx(at(path, "manages"), i), ErrShape,
				"%q is neither a seat handle nor a unit key. A `manages:` "+
					"entry takes a seat's handle (%s) or a unit's key (%s). A "+
					"display name matches neither, so this entry expands to "+
					"nobody and the seat's roster is short without saying so",
				target, handleRule, unitKeyRule)
		}
	}
	return p.err()
}

// The two grammars, phrased for a person, so every message that refuses a
// reference says the same thing the field's own shape check says.
const (
	handleRule = "lowercase letters, digits and `-`, starting with a letter " +
		"or a digit"
	unitKeyRule = "lowercase letters, digits, `-` and `_`, starting with a " +
		"letter, up to 64 characters"
)

// EachUnit walks every unit this document declares, at any depth, parent
// before children, each with the path it was written at.
//
// THE TWIN OF [Company.EachRole], and it exists for the reason that one
// records: the walk was written inline in every rule that needed it, and each
// copy is a chance for a rule to cover the top-level units and quietly exempt
// every team one level down — which, in a company with an org chart, is most
// of them. A rule that holds for `units[0]` and not for
// `units[0].children[0]` is not a rule.
//
// AN ITERATOR rather than a callback, so a caller looking for ONE unit can
// stop at it.
func (c *Company) EachUnit() iter.Seq2[*Unit, Path] {
	return func(yield func(*Unit, Path) bool) {
		var walk func(units []Unit, path Path) bool
		walk = func(units []Unit, path Path) bool {
			for i := range units {
				unit := &units[i]
				here := idx(path, i)
				if !yield(unit, here) {
					return false
				}
				if !walk(unit.Children, at(here, "children")) {
					return false
				}
			}
			return true
		}
		walk(c.Units, field("units"))
	}
}
