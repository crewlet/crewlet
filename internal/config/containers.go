package config

import (
	"fmt"
	"regexp"
	"strings"
)

// A CONTAINER KEY is the short, stable name a unit files work under and writes
// pages into (`ENG`, `PROD`, `HOME`). One grammar covers both kinds and every
// backend, on purpose: keys are vendor-neutral so that switching backends does
// not rewrite the org chart, and a wider grammar would let a company write
// `engineering-platform` and find on moving to Jira that its hierarchy must be
// renamed. So the shape is the tightest vendor's, Jira's project-key rule (an
// upper-case letter then one to nine upper-case letters or digits).
//
// Refused rather than upper-cased: comparison is case-insensitive on the
// vendors and exact natively, so `eng` and `ENG` would be one container for one
// company and two for another.
var containerKeyPattern = regexp.MustCompile(ContainerKeyPattern)

// ContainerKeyPattern is [ValidContainerKey]'s grammar as text, at most
// [MaxContainerKey] characters.
//
// EXPORTED FOR THE DASHBOARD's copy (`CONTAINER_KEY_PATTERN`, held equal by
// TestTheDashboardKeysAContainerByTheEnginesGrammar): the new-project dialog
// holds the key a person types to it before the write that names it, rather
// than send one every node refuses, and Ask Crewlet is told a Doclet space by
// this grammar where a company has more spaces than an enum may list. Written
// with no flag and no escape, so the dashboard compiles the same text.
const ContainerKeyPattern = `^[A-Z][A-Z0-9]{1,9}$`

// MaxContainerKey is the longest a container key may be: [ContainerKeyPattern]'s
// bound, where the dashboard's key field stops (`CONTAINER_KEY_MAX`).
const MaxContainerKey = 10

// ValidContainerKey reports whether s is a well-formed container key.
//
// Exported because the native backends check it again at their own edge: a
// key reaching them over the API never passed through this loader.
func ValidContainerKey(s string) bool { return containerKeyPattern.MatchString(s) }

// ContainerKeyRule is the grammar, phrased for a person, so every message
// that refuses a key says the same thing.
const ContainerKeyRule = "an upper-case letter followed by 1-9 upper-case " +
	"letters or digits (ENG, PROD, TS2), the shape every backend accepts, " +
	"so the org chart survives a backend switch"

// validateContainerKeys checks every project and space key in the document: the
// shape, and the two knowledge containers the engine reserves.
//
// UNIQUENESS IS DELIBERATELY NOT CHECKED. Two units sharing one project is
// ordinary (the shipped example files Product Management and Content both into
// PROD). What goes wrong is narrower: a key whose units resolve to DIFFERENT
// leads has no single recipient for activity naming nobody, which
// [org.Organization.LeadsBy] reports with its candidates. A second rule here
// would refuse configurations that work.
func (c *Company) validateContainerKeys() error {
	var p problems

	skills := c.SkillsContainerKey()
	root := c.RootSpaceKey()

	// The reserved containers, and what each is reserved FOR — the message
	// has to say, because "HOME is reserved" tells an operator nothing about
	// which of their two settings to change.
	// What each reserved key holds, in a person's words: the same sentence
	// whichever side of the clash was just changed, a unit's space or the
	// knowledge setting.
	type reservation struct{ why, holds string }
	reserved := map[string]reservation{}
	if skills != "" {
		reserved[skills] = reservation{
			why: "holds this company's tool-skill pages, which are " +
				"excluded from knowledge search and from routing, so a unit writing " +
				"there would have its pages silently unreadable. Pick another key, " +
				"or move the skills container with knowledge.skills_container",
			holds: "the tool skills",
		}
	}
	if root != "" && root != skills {
		reserved[root] = reservation{
			why: "holds the organisation's own pages, starting with the " +
				"root Onboarding page every seat reads first. Pick another key, or " +
				"move it with knowledge.root_space",
			holds: "the company pages",
		}
	}

	shape := func(path Path, kind, key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if !ValidContainerKey(key) {
			p.addSaying(path, ErrUnknownValue, ContainerKeySaid(kind, key),
				"%q is not a %s key: want %s", key, kind, ContainerKeyRule)
		}
	}
	space := func(path Path, key string) {
		key = strings.TrimSpace(key)
		shape(path, "container", key)
		if r, taken := reserved[key]; taken {
			p.addSaying(path, ErrConflict,
				fmt.Sprintf("%s is both a unit's space and the space of %s, and each "+
					"needs a space of its own. Pick another key.", key, r.holds),
				"%q is reserved: it %s", key, r.why)
		}
	}

	if c.Knowledge.SkillsContainer != nil && skills != "" {
		shape(field("knowledge.skills_container"), "container", skills)
	}
	if c.Knowledge.RootSpace != nil && root != "" {
		shape(field("knowledge.root_space"), "container", root)
	}
	for i, s := range c.Knowledge.KnowledgeScope {
		// A scope entry is NOT checked against the reserved set: naming the
		// skills container in a read scope is refused by the exclusion
		// itself, and naming the root space there is an ordinary narrowing.
		shape(idx(field("knowledge.scope"), i), "container", s)
	}

	for i := range c.Roles {
		rp := idx(field("roles"), i)
		shape(at(rp, "project"), "project", c.Roles[i].Project)
		space(at(rp, "space"), c.Roles[i].Space)
	}
	var walk func(path Path, u *Unit)
	walk = func(path Path, u *Unit) {
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
		walk(idx(field("units"), i), &c.Units[i])
	}
	return p.err()
}

// ContainerKeySaid is a key the grammar refuses, in a person's words: kind is
// "project" or "container", which a person knows as a space.
func ContainerKeySaid(kind, key string) string {
	noun := kind
	if kind == "container" {
		noun = "space"
	}
	return fmt.Sprintf("%q is not a %s key. Use an upper-case letter followed by 1 to 9 "+
		"upper-case letters or digits, like ENG, PROD or TS2.", key, noun)
}

// ErrContainerKey names a key the grammar refuses, for a caller outside the
// loader — the REST surface and the operator MCP server both mint containers
// from input this package never saw.
func ErrContainerKey(kind, key string) error {
	return fmt.Errorf("%w: %q is not a %s key: want %s",
		ErrUnknownValue, key, kind, ContainerKeyRule)
}
