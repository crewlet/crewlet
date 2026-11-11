// Package mcpreview decides, from the fleet's listings and the definitions
// each MCP tool's level was decided on, which tools a server changed since,
// and which of them ask a person before each call until somebody accepts the
// change (ADR-0078).
//
// # One reading, every reader
//
// The node that files a tool asks it at each call ([Index.Unreviewed]), the
// node that lists a server first accepts its first listing by it
// ([Baseline]), the alarm counts by it, and a server's page shows it
// ([Changes]). Four readings of one rule would hold a tool on one screen and
// run it on another node, so the rule is here, over the records alone: no
// store, no clock, no configuration but the levels a caller hands in.
//
// # A level holds for one definition
//
// A tool's level is a person's decision about the tool as its server defined
// it then, named by the definition's digest. A server is free to change a
// definition, and a level that carried over would let it turn a tool somebody
// allowed into one nobody looked at, so a tool listed under any other
// definition than the accepted one, or added after the server's first
// listing, is UNREVIEWED: an allowed one asks a person first, whatever its
// level says, until somebody accepts the change. What it changed is said part
// by part (title, description, schema, annotations), and a change that makes a
// tool claim less risk than it did is LOOSENED: the direction a server would
// move a tool it wanted run unasked, which a reviewer is told before anything
// else.
//
// # A server's first listing is accepted as it is
//
// The operator who added a server chose it with its first listing in front of
// them (the add flow tests it and shows each tool's starting level), so that
// listing is accepted by the node that lists the server first. A server is
// BASELINED once any record names it; before that, nothing it lists waits.
package mcpreview

import (
	"cmp"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
)

// Index is the accepted definitions by server and then by the server's own
// name for the tool.
type Index map[string]map[string]coord.MCPAcceptedTool

// IndexOf indexes accepted definitions.
func IndexOf(accepted []coord.MCPAcceptedTool) Index {
	out := Index{}
	for _, a := range accepted {
		if out[a.Server] == nil {
			out[a.Server] = map[string]coord.MCPAcceptedTool{}
		}
		out[a.Server][a.Tool] = a
	}
	return out
}

// Baselined reports whether server's first listing has been accepted: whether
// any record names it.
func (x Index) Baselined(server string) bool { return len(x[server]) > 0 }

// Unreviewed reports whether the definition digest a server lists for its tool
// is not the one the tool's level was decided on: one the server changed, or a
// tool it added after its first listing, that nobody has accepted since. False
// for every tool of a server not yet baselined, whose first listing is about
// to be accepted as it is.
func (x Index) Unreviewed(server, tool, digest string) bool {
	tools := x[server]
	if len(tools) == 0 {
		return false
	}
	a, ok := tools[tool]
	return !ok || a.Removed || a.Digest != digest
}

// Kind is what a change did to a tool.
type Kind string

const (
	// KindAdded is a tool the server lists that its first listing did not,
	// and nobody has accepted since (or one whose removal was accepted,
	// listed again).
	KindAdded Kind = "added"
	// KindChanged is a tool the server lists under another definition than
	// the accepted one.
	KindChanged Kind = "changed"
	// KindRemoved is an accepted tool no live node's listing names any
	// more. It holds nothing (nothing serves it) and keeps its row until
	// somebody accepts that it went, so that its return reads as a tool
	// added rather than as the tool it was.
	KindRemoved Kind = "removed"
)

// Part is one part of a definition a change moved. The words are the change
// kinds a reader is shown beside [KindAdded] and [KindRemoved].
type Part string

// The parts of a definition, each named by the word a change kind reads.
const (
	PartTitle       Part = "title"
	PartDescription Part = "description"
	PartSchema      Part = "schema"
	PartAnnotations Part = "annotations"
)

// Parts is every part, in the order a reader is told them.
var Parts = []Part{PartTitle, PartDescription, PartSchema, PartAnnotations}

// Change is one tool's change since its level was decided.
type Change struct {
	// Tool is the server's own name for the tool, which a decision about it
	// is keyed by.
	Tool string
	Kind Kind
	// Parts are the parts a changed definition moved, in [Parts] order;
	// empty for an added or a removed tool.
	Parts []Part
	// Before is the accepted definition, nil for a tool never accepted.
	Before *coord.MCPAcceptedTool
	// After is the definition listed now, as the first live listing to name
	// it gives it, nil for a removed tool. Its instances are summed across
	// the live listings naming it.
	After *coord.MCPListedTool
	// Loosened is a changed definition whose declared hints claim less
	// risk than the accepted one's ([Loosened]).
	Loosened bool
}

// Changes is every change to server's tools, by the live listings of it and
// the definitions accepted for it: one per (tool, listed definition) that is
// not the accepted one, and one per accepted tool no listing names. In tool
// order, then by definition. None for a server not yet baselined.
//
// EVERY LISTED TOOL IS REVIEWED, since a listing names every tool its node's
// instances list (internal/coord's mcplisting.go). A REMOVAL IS JUDGED ONLY
// WHERE THE LISTINGS CAN SAY: no live listing says nothing about what the
// server lists.
func Changes(listings []coord.MCPListing, accepted map[string]coord.MCPAcceptedTool) []Change {
	if len(accepted) == 0 {
		return nil
	}
	type at struct{ tool, digest string }
	found := map[at]*Change{}
	named := map[string]struct{}{}
	for _, l := range listings {
		for _, t := range l.Tools {
			named[t.Raw] = struct{}{}
			before, ok := accepted[t.Raw]
			if ok && !before.Removed && before.Digest == t.Digest {
				continue
			}
			key := at{t.Raw, t.Digest}
			if c := found[key]; c != nil {
				c.After.Instances += t.Instances
				continue
			}
			after := t
			c := &Change{Tool: t.Raw, Kind: KindAdded, After: &after}
			if ok && !before.Removed {
				c.Kind, c.Before = KindChanged, &before
				c.Parts = partsMoved(before.Parts, t.Parts)
				c.Loosened = Loosened(before.Hints, t.Declared)
			}
			found[key] = c
		}
	}
	out := make([]Change, 0, len(found))
	for _, c := range found {
		out = append(out, *c)
	}
	if len(listings) > 0 {
		for _, tool := range slices.Sorted(maps.Keys(accepted)) {
			before := accepted[tool]
			if _, listed := named[tool]; listed || before.Removed {
				continue
			}
			out = append(out, Change{Tool: tool, Kind: KindRemoved, Before: &before})
		}
	}
	slices.SortFunc(out, func(a, b Change) int {
		return cmp.Or(cmp.Compare(a.Tool, b.Tool), cmp.Compare(digestOf(a.After), digestOf(b.After)))
	})
	return out
}

// digestOf is a listed definition's digest, "" for none.
func digestOf(t *coord.MCPListedTool) string {
	if t == nil {
		return ""
	}
	return t.Digest
}

// partsMoved is every part whose name differs between two definitions of a
// tool, in [Parts] order.
func partsMoved(before, after coord.MCPToolParts) []Part {
	var out []Part
	if before.Title != after.Title {
		out = append(out, PartTitle)
	}
	if before.Description != after.Description {
		out = append(out, PartDescription)
	}
	if before.Schema != after.Schema {
		out = append(out, PartSchema)
	}
	if before.Hints != after.Hints {
		out = append(out, PartAnnotations)
	}
	return out
}

// Loosened reports whether a tool's declared hints claim less risk after than
// before: a risk read lower (destructive to additive, either to read-only), or
// a tool that said it reaches past the company now saying it does not. Read
// from the hints the SERVER declared, never an operator's overrides: the
// question is what the server now claims.
func Loosened(before, after coord.MCPToolHints) bool {
	was, now := mcp.RiskOf(Annotations(before)), mcp.RiskOf(Annotations(after))
	return slices.Index(mcp.Risks, now.Risk) < slices.Index(mcp.Risks, was.Risk) ||
		(was.External && !now.External)
}

// Annotations reads a listing's hint words back into the engine's tri-state.
// A word this build does not write is unknown: never read as an assertion.
func Annotations(h coord.MCPToolHints) mcp.Annotations {
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

// Decided is the level a listed tool's entry decides for it, before any change
// holds it: blocked where its listing says no instance filed it, else its own
// level, the entry's default, or the level its hints (overrides applied) start
// it at.
func Decided(policy config.MCPToolPolicy, t coord.MCPListedTool) config.MCPToolLevel {
	if t.Blocked {
		return config.MCPToolBlocked
	}
	starting := config.MCPToolLevel(mcp.StartingLevel(mcp.RiskOf(Annotations(t.Hints))))
	level, _ := policy.Level(t.Raw, starting)
	return level
}

// Holds reports whether a change holds its tool at Needs approval: a tool
// listed under a definition nobody accepted whose entry would otherwise let it
// run unasked. A change to a tool that already asks, or is blocked, or went,
// holds nothing more, and waits only to be reviewed.
func Holds(policy config.MCPToolPolicy, c Change) bool {
	return c.After != nil && Decided(policy, *c.After) == config.MCPToolAllow
}

// Accepting is the record that accepts a listed definition, by who accepted
// it ("" for a first listing's) at when.
func Accepting(server string, t coord.MCPListedTool, by string, at time.Time) coord.MCPAcceptedTool {
	return coord.MCPAcceptedTool{
		Server: server, Tool: t.Raw, Digest: t.Digest, First: by == "", By: by, At: at.UTC(),
		Title: t.Title, Description: t.Description, Hints: t.Declared, Parts: t.Parts,
	}
}

// Removing is the record that accepts a tool's removal, by who accepted it at
// when.
func Removing(server, tool, by string, at time.Time) coord.MCPAcceptedTool {
	return coord.MCPAcceptedTool{Server: server, Tool: tool, Removed: true, By: by, At: at.UTC()}
}

// Baseline is the records that accept a server's first listing as it is: one
// per tool it lists, blocked ones included, each its own definition.
func Baseline(l coord.MCPListing, at time.Time) []coord.MCPAcceptedTool {
	out := make([]coord.MCPAcceptedTool, 0, len(l.Tools))
	seen := map[string]struct{}{}
	for _, t := range l.Tools {
		if _, dup := seen[t.Raw]; dup {
			continue
		}
		seen[t.Raw] = struct{}{}
		out = append(out, Accepting(l.Server, t, "", at))
	}
	return out
}
