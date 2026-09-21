package config

import (
	"errors"
	"iter"
	"maps"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/org"
)

// THE RULES ONLY A WHOLE FILE CAN CHECK.
//
// # Why they are collected rather than scattered
//
// Every other rule in this package is a property of the thing it is written
// beside: a provider key names a provider, a container key is well shaped, a
// human seat holds no app. The three here are properties of the WHOLE
// DOCUMENT — they compare one half of it against the other, or one seat
// against every other seat — and that is now a distinguishing fact rather
// than an implementation detail. The org chart is a log, so a per-object
// chart write sees ONE seat and its own snapshot of the structure; it cannot
// see the settings half at all, and it cannot see what a second writer is
// doing to a second seat on a second subject at the same moment. A rule of
// this class is therefore sound in exactly one place: over an authored file,
// whole, before it is split.
//
// That is what "promoted" means for them. Each was already a real fault; each
// was caught late, by a consumer, silently, or not at all. What changes is
// that the file becomes the only honest place to catch them, so they are
// caught there.
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

// validateFileRules is the three whole-document rules, run together.
func (c *Company) validateFileRules() error {
	return errors.Join(
		c.validateMCPEnvServers(),
		c.validateSeatEmails(),
		c.validateReferenceShapes(),
	)
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
// [BridgeServerName] — that is refused where servers are declared — so an
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
					"Nothing reads this block — the engine looks the "+
					"credentials up per SERVER, so a key naming none is never "+
					"consulted and the server it was meant for starts with "+
					"none", key, listOrNone(declared))
		}
	}

	for role, path := range c.eachRole() {
		check(path, role.MCPEnv)
	}
	for unit, path := range c.EachUnit() {
		check(path, unit.MCPEnv)
	}
	return p.err()
}

// listOrNone renders a set of names for a message, or says there are none.
//
// "mcp_servers has " followed by nothing is the shape of message that makes
// an operator think the renderer broke rather than that they declared no
// servers.
func listOrNone(set map[string]struct{}) string {
	if len(set) == 0 {
		return "no servers at all"
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
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
// FOLDED AND TRIMMED, exactly as the registry folds it, because agreeing with
// the consumer is the whole point: a rule comparing `Ada@example.com` and
// `ada@example.com` as different values would pass a document the registry
// then collapses.
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
	for role, path := range c.eachRole() {
		email := strings.ToLower(strings.TrimSpace(role.Email))
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
				"%d seats declare the email %q (%s). The party registry keys "+
					"on it and the first seat wins, so all but one of these "+
					"silently stops being findable by address — mail meant for "+
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
					"HANDLE, not its display name — %q resolves to nobody, so "+
					"this unit has no lead, every escalation from it reaches "+
					"the parent's, and its lead-targeted schedules never fire",
				lead, handleRule, lead)
		}
	}

	for role, path := range c.eachRole() {
		if ref := strings.TrimSpace(role.Unit); ref != "" && !entityID.MatchString(ref) {
			p.add(at(path, "unit"), ErrShape,
				"%q is not a unit key: %s. A seat declared at the org root "+
					"joins its unit by KEY (the unit's `id:`), not by the "+
					"team's display name — %q matches no key, so this seat "+
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
					"entry takes a seat's handle (%s) or a unit's key (%s) — a "+
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
// THE TWIN OF [Company.eachRole], and it exists for the reason that one
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
