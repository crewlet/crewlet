package config

import (
	"errors"
	"iter"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// THE RULES ONLY A WHOLE DOCUMENT CAN CHECK.
//
// # Why they are collected rather than scattered
//
// Every other rule in this package is a property of the thing it is written
// beside: a provider key names a provider, a container key is well shaped, a
// human seat holds no app. The five here are properties of the WHOLE
// DOCUMENT — they compare one part of it against another, or one seat against
// every other seat — so they run together, over the document whole. Each was
// a real fault caught late, by a consumer, silently, or not at all.
//
// # Why they are ADMISSION rules
//
// None of them stops a company running — that is precisely what makes them
// hard to find. A stored revision may carry any of them, from a build that
// never checked, and it runs today exactly as it ran yesterday; refusing to
// apply it would take a working company down on upgrade over a rule its
// author never saw. So a SUBMITTED document is refused and a STORED one is
// reported by [Company.AdmissionWarnings], which is the class every rule
// added after companies existed belongs to. See [Company.ValidateRunnable]
// for the distinction in full.

// validateFileRules is the five whole-document rules, run together.
func (c *Company) validateFileRules() error {
	return errors.Join(
		c.validateMCPEnvServers(),
		c.validateOptionalKeys(),
		c.validateHeaderSpellings(),
		c.validateSeatEmails(),
		c.validateReferenceShapes(),
	)
}

// validateOptionalKeys refuses an `optional_keys` entry that leaves nothing
// out.
//
// A key listed there is one whose value may resolve to nothing without holding
// the server back ([MCPServer.OptionalKeys]). An entry naming no key the server
// is started with is not an error anywhere at run time — it is simply never
// consulted — and that silence is exactly what makes it worth refusing: the
// key it was MEANT for stays required, so the server is not started on every
// host that does not set the value, while the document reads as though the
// operator had said the server could run without it. The shapes of that
// mistake are told apart because their fixes are: a VARIABLE's name where a
// key's belongs, a variable only the url references, and a name nothing
// carries at all.
//
// A WHOLE-DOCUMENT RULE because the keys a server is started with are not all
// the server's: a seat's or unit's `mcp_env` block for it lands in the same
// environment or the same headers, so a key only seats write is still a key
// the server reads. And an entry whose values carry no reference anywhere is
// refused too: nothing in it can resolve to nothing, so the entry says
// something the engine will never act on.
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
// HTTP NAMES A HEADER IN ANY CASE, so `authorization` and `Authorization` are
// one header, and a tool server is sent one value for it. The engine reads
// them as one where it lays a seat's values over the server's own
// ([MCPServer.ValueKey]): a seat's key replaces the template's however either
// is spelled. Two spellings in ONE PLACE leave nothing to say which wins —
// two keys of one map, or a unit's block and a member's, which the org chart
// lays one under the other by the key as written ([org.MCPEnv.WithDefaults]),
// keeping both. Sent as written, each request carried whichever of the two
// came last out of a map, so a seat's identity went out on some requests and
// its unit's on the rest.
//
// Only an http server's — keys are compared as the server reads them, and an
// environment does tell `token` from `TOKEN` — and only within one place,
// because seats are started apart and two of them spelling one header
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

// validateMCPEnvServers refuses an `mcp_env` block keyed on a name nothing
// reads.
//
// A SEAT'S `mcp_env` IS KEYED BY SERVER NAME, and the engine reads it from
// the other end: it walks `mcp_servers`, and for each one looks up that
// seat's block (internal/engine/mcp.go). A key naming no server is therefore
// never read by anything. It is not an error at run time, it is not a
// warning, and it does not appear in any prompt — the credentials sit in the
// document looking configured, the tool server they were meant for starts
// with no credentials at all, and the seat's tools fail to authenticate
// against a vendor nobody typed wrong.
//
// It is the exact fault the provider-key rule already catches one field over
// ([Company.validateProviderRefs]), for the same reason: a key that misses
// has no run-time symptom, so the document is the only place it can be seen.
//
// THE VENDOR BLOCKS ARE NAMED EVEN WITH NO SERVER, and leaving them out is
// what makes this rule wrong rather than strict. Six blocks are read by the
// ENGINE directly, by name, whether or not a tool server of that name exists
// — a GitHub App's per-seat token, a GitLab account's, a Datadog application
// key, an Atlassian account's under the shared block or either product's own
// — so a company running the GitHub App with an empty `mcp_servers:` is an
// ordinary arrangement this must admit. The set comes from
// [org.EngineReadMCPEnv] rather than being listed here, because a private
// seventh copy of it is precisely the drift that table exists to end.
//
// THE BRIDGE NAME NEEDS NO CASE OF ITS OWN. A server may not be called
// [ToolServerName] — that is refused where servers are declared — so an
// `mcp_env` block keyed on it names no server and is reported by this rule
// like any other. Writing a second, better-worded case for it would be a
// second place to keep the reserved name.
func (c *Company) validateMCPEnvServers() error {
	vendor := org.EngineReadMCPEnv()
	declared := make(map[string]struct{}, len(c.MCPServers)+len(vendor))
	for i := range c.MCPServers {
		if name := strings.TrimSpace(c.MCPServers[i].Name); name != "" {
			declared[name] = struct{}{}
		}
	}
	// KEPT OUT OF THE MESSAGE'S LIST, which names the company's own servers:
	// telling an operator who misspelled one of their two servers that the
	// allowed set also holds `datadog` and `confluence` describes the engine
	// rather than their file.
	readable := make(map[string]struct{}, len(declared)+len(vendor))
	maps.Copy(readable, declared)
	for _, name := range vendor {
		readable[name] = struct{}{}
	}

	servers := listOrNone(slices.Sorted(maps.Keys(declared)), "no servers at all")

	var p problems
	check := func(path Path, env org.MCPEnv) {
		// SORTED, because a map's iteration order is random and an error
		// listing three undeclared servers in a different order on every
		// run is one nobody can diff a CI log against.
		keys := make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, ok := readable[key]; ok {
				continue
			}
			p.add(entry(at(path, "mcp_env"), key), ErrUnknownValue,
				"%q is not a declared tool server: mcp_servers has %s. "+
					"Nothing reads this block: the engine looks the "+
					"credentials up per SERVER, so a key naming none is never "+
					"consulted and the server it was meant for starts with "+
					"none", key, servers)
		}
	}

	for role, path := range c.EachRole() {
		check(path, role.MCPEnv)
	}
	for unit, path := range c.EachUnit() {
		check(path, unit.MCPEnv)
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
// (internal/notify/registry.go): a second seat claiming an address is not
// reported, not merged and not preferred — it is dropped from that index and
// every lookup by address answers the first seat for ever. So mail addressed
// to one person reaches another, and the person who is shadowed has no
// symptom at all: nothing they do fails, they simply stop being found.
//
// The identical rule already exists one field over for every EXTERNAL
// identity ([org.Organization.validateContactIdentities]) and was written for
// the same registry behaviour. Email was the one address it did not cover.
//
// IN THE FORM THE REGISTRY MATCHES ON — [iam.NormalizeEmail], lower-cased
// with the plus tag dropped — because agreeing with the consumer is the whole
// point: a rule comparing `Ada@example.com` and `ada+ops@example.com` as
// different values would pass a document the registry then collapses onto
// one mailbox. A whole `${VAR}` is compared as written, since two seats naming
// one variable are one address and validation resolves nothing to tell two
// variables apart.
//
// REPORTED AT EVERY SEAT THAT HOLDS THE ADDRESS, not just the second: an
// operator fixing this has to decide which seat keeps it, and a message
// naming one of the two tells them nothing about the other.
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
// # What this is and is not
//
// It checks the SHAPE, never the resolution. A reference that is well formed
// and resolves to nothing stays a WARNING ([Company.ReferenceWarnings]), and
// deliberately so: a chart is assembled in pieces and every intermediate
// state is one every node applies, so a `lead:` naming a seat that has not
// been hired yet must not refuse the document. What is refused here is a
// value that could never resolve under any chart at all.
//
// # Why it is worth refusing
//
// These three fields named a seat's DISPLAY NAME once, and resolution fell
// back to it. It does not any more: a seat is addressed by its handle and a
// unit by its key, so `lead: QA Lead` went from a working reference to one
// that resolves to nothing — with a space in it, which no handle may carry
// and no chart will ever mint. A document carrying one has a typo the shape
// alone proves, and reporting it as an ordinary dangling reference would put
// it in the same list as the seat somebody is about to hire.
//
// # The two grammars, and why manages takes either
//
// A `lead:` names a seat, so it takes [org.ValidHandle]. A root seat's
// `unit:` names a unit, so it takes the key grammar [entityID]. A `manages:`
// entry names EITHER — that is the feature, since `manages: [engineering]`
// expands to a division's every seat — so it is refused only when it is
// neither.
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
		if ref := strings.TrimSpace(role.Unit); ref != "" && !entityID.MatchString(ref) {
			p.add(at(path, "unit"), ErrShape,
				"%q is not a unit key: %s. A seat declared at the org root "+
					"joins its unit by KEY (the unit's `id:`), not by the "+
					"team's display name. %q matches no key, so this seat "+
					"stays at the root, outside the unit's credentials and "+
					"invisible to its lead", ref, unitKeyRule, ref)
		}
		for i, target := range role.Manages {
			target = strings.TrimSpace(target)
			if target == "" || org.ValidHandle(target) || entityID.MatchString(target) {
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
