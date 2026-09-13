package config

import (
	"fmt"
	"regexp"
	"strings"
)

// A CONTAINER KEY is the short, stable name a unit files work under and
// writes pages into — `ENG`, `PROD`, `HOME`. One grammar covers both kinds
// and every backend, and that is the point rather than a simplification.
//
// # Why the grammar is the tightest vendor's
//
// The keys became vendor-neutral so that switching backends does not rewrite
// the org chart. A grammar wider than the narrowest backend would forfeit
// exactly that: a company running natively could write `engineering-platform`
// and discover on the day it moves to Jira that its whole hierarchy has to be
// renamed, with every item key it ever minted pointing at the old one.
//
// So the accepted shape is Jira's own project-key rule — an upper-case letter
// followed by one to nine upper-case letters or digits — which Confluence
// space keys and the native containers both accept as a subset. An org chart
// written against it is portable by construction.
//
// # Refused rather than upper-cased
//
// A lower-case key is an error naming the fix, not a value silently mangled.
// Comparison is case-insensitive on the vendors and exact natively, so
// accepting `eng` would make one company's `eng` and `ENG` the same container
// and another's two — and the config would not say which.
var containerKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

// ValidContainerKey reports whether s is a well-formed container key.
//
// Exported because the native backends check it again at their own edge: a
// key reaching them over the API never passed through this loader.
func ValidContainerKey(s string) bool { return containerKeyPattern.MatchString(s) }

// ContainerKeyRule is the grammar, phrased for a person, so every message
// that refuses a key says the same thing.
const ContainerKeyRule = "an upper-case letter followed by 1-9 upper-case " +
	"letters or digits (ENG, PROD, TS2) — the shape every backend accepts, " +
	"so the org chart survives a backend switch"

// validateContainerKeys checks every project and space key in the document:
// the shape, and the two knowledge containers the engine reserves.
//
// UNIQUENESS IS DELIBERATELY NOT CHECKED. Two units sharing one project is an
// ordinary arrangement — the shipped example has Product Management and
// Content both filing into PROD, because they collaborate too tightly to
// justify cross-project linking — and what actually goes wrong is narrower
// than duplication: a key whose units resolve to DIFFERENT leads has no
// single recipient for activity that names nobody. That is a question about
// the normalised org chart rather than about the document, and
// [org.Organization.LeadsBy] already answers it, reporting each ambiguity
// with the candidates it chose between. A second rule here would refuse
// configurations that work.
func (c *Company) validateContainerKeys() error {
	var p problems

	skills := c.SkillsContainerKey()
	root := c.RootSpaceKey()

	// The reserved containers, and what each is reserved FOR — the message
	// has to say, because "HOME is reserved" tells an operator nothing about
	// which of their two settings to change.
	reserved := map[string]string{}
	if skills != "" {
		reserved[skills] = "holds this company's tool-skill pages, which are " +
			"excluded from knowledge search and from routing — a unit writing " +
			"there would have its pages silently unreadable. Pick another key, " +
			"or move the skills container with knowledge.skills_container"
	}
	if root != "" && root != skills {
		reserved[root] = "holds the organisation's own pages, starting with the " +
			"root Onboarding page every seat reads first. Pick another key, or " +
			"move it with knowledge.root_space"
	}

	shape := func(path, kind, key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if !ValidContainerKey(key) {
			p.add(path, ErrUnknownValue, "%q is not a %s key: want %s",
				key, kind, ContainerKeyRule)
		}
	}
	space := func(path, key string) {
		key = strings.TrimSpace(key)
		shape(path, "container", key)
		if why, taken := reserved[key]; taken {
			p.add(path, ErrConflict, "%q is reserved: it %s", key, why)
		}
	}

	if c.Knowledge.SkillsContainer != nil && skills != "" {
		shape("knowledge.skills_container", "container", skills)
	}
	if c.Knowledge.RootSpace != nil && root != "" {
		shape("knowledge.root_space", "container", root)
	}
	for i, s := range c.Knowledge.KnowledgeScope {
		// A scope entry is NOT checked against the reserved set: naming the
		// skills container in a read scope is refused by the exclusion
		// itself, and naming the root space there is an ordinary narrowing.
		shape(idx("knowledge.scope", i), "container", s)
	}

	for i := range c.Roles {
		rp := idx("roles", i)
		shape(at(rp, "project"), "project", c.Roles[i].Project)
		space(at(rp, "space"), c.Roles[i].Space)
	}
	var walk func(path string, u *Unit)
	walk = func(path string, u *Unit) {
		shape(at(path, "project"), "project", u.Project)
		space(at(path, "space"), u.Space)
		for i := range u.Roles {
			rp := idx(at(path, "roles"), i)
			shape(at(rp, "project"), "project", u.Roles[i].Project)
			space(at(rp, "space"), u.Roles[i].Space)
		}
		for i := range u.Children {
			walk(idx(at(path, "children"), i), &u.Children[i])
		}
	}
	for i := range c.Units {
		walk(idx("units", i), &c.Units[i])
	}
	return p.err()
}

// ErrContainerKey names a key the grammar refuses, for a caller outside the
// loader — the REST surface and the operator MCP server both mint containers
// from input this package never saw.
func ErrContainerKey(kind, key string) error {
	return fmt.Errorf("%w: %q is not a %s key: want %s",
		ErrUnknownValue, key, kind, ContainerKeyRule)
}
