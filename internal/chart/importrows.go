package chart

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// TURNING AN AUTHORED COMPANY INTO ROWS, which is what an import does and
// what the equivalence between the two derivations is measured over.
//
// # Why it lives here rather than in the config layer
//
// The shape it produces is this domain's — units, seats, the two authored edge
// sets — and the config layer already depends on the organization model, which
// depends on this. So the direction is fixed: the chart cannot import config,
// and what it CAN take is the smallest description of an authored company that
// is not a config type at all.
//
// THAT DESCRIPTION IS DELIBERATELY NOT A CONFIG STRUCT. A caller fills it from
// whatever it holds — a parsed document, a form submission, a restored
// backup — and nothing here knows which. What it buys is that the import path
// and the equivalence test drive the same function, so the rows a test
// compares against are the rows a company actually runs on.

// Authored is one company's chart as somebody wrote it, before any derivation.
//
// FLAT, and with every reference as it was written. The nesting a document has
// is carried by `Parent` rather than by containment: rows are flat, and a
// converter that took a tree would have to be written again by every caller
// that does not hold one.
type Authored struct {
	Units []AuthoredUnit
	Seats []AuthoredSeat
}

// AuthoredUnit is one unit as written.
type AuthoredUnit struct {
	Key    string
	Parent string

	Name    string
	Type    string
	Purpose string
	Goals   []string

	// Lead is the AUTHORED lead's handle, empty where the unit inherits
	// one. Never the effective lead: inheritance is a derivation over the
	// tree, and a row carrying the inherited answer would be a derived
	// value written down that nothing recomputes.
	Lead string

	Channel       string
	Project       string
	Space         string
	KnowledgeRefs []string

	// Runtime is the unit's engine-only content, opaque here. See
	// [Unit.Runtime].
	Runtime json.RawMessage
}

// AuthoredSeat is one seat as written.
type AuthoredSeat struct {
	Handle string
	Unit   string

	Kind SeatKind
	Name string

	Email     string
	Backstory string
	Goal      string

	Responsibilities     []string
	BehavioralGuidelines []string

	// Runtime is the seat's engine-only content, opaque here. See
	// [Seat.Runtime].
	Runtime json.RawMessage

	// Manages is the authored list, entries exactly as written — including
	// ones that resolve to nothing. Expanding here would store a derived
	// value, and the expansion is a function of the tree at the moment it
	// is read.
	Manages []string

	Project string
	Space   string
}

// Rows is this authored company as the chart holds it.
//
// EVERY ADDRESS IS FOLDED ON THE WAY IN, once, here — so a document that
// spelled a unit `Platform` in one place and `platform` in another produces
// one unit and one set of references, which is the same answer every read
// gives afterwards.
//
// THE POSITION IS THE CALLER'S. These rows have not been through a log: they
// are what an import is about to publish, or what a test compares a derivation
// against. A zero position is honest for both.
func (a Authored) Rows() Chart {
	out := Chart{
		Manages: map[string][]string{},
		Leads:   map[string]string{},
	}
	for _, unit := range a.Units {
		key := NormalizeKey(unit.Key)
		if key == "" {
			continue
		}
		out.Units = append(out.Units, Unit{
			V: DocumentVersion, Key: key,
			Name: unit.Name, Type: unit.Type, Purpose: unit.Purpose,
			Goals: unit.Goals, Channel: unit.Channel,
			Project: unit.Project, Space: unit.Space,
			KnowledgeRefs: unit.KnowledgeRefs,
			ParentKey:     NormalizeKey(unit.Parent),
			Lead:          NormalizeKey(unit.Lead),
			Runtime:       unit.Runtime,
		})
		if lead := NormalizeKey(unit.Lead); lead != "" {
			out.Leads[key] = lead
		}
	}
	for _, seat := range a.Seats {
		handle := NormalizeKey(seat.Handle)
		if handle == "" {
			continue
		}
		out.Seats = append(out.Seats, Seat{
			V: DocumentVersion, Handle: handle,
			Kind: seat.Kind, Name: seat.Name, Email: seat.Email,
			Backstory: seat.Backstory, Goal: seat.Goal,
			Responsibilities:     seat.Responsibilities,
			BehavioralGuidelines: seat.BehavioralGuidelines,
			Project:              seat.Project, Space: seat.Space,
			UnitKey: NormalizeKey(seat.Unit),
			Runtime: seat.Runtime,
		})
		// THE EDGE SET IS FOLDED AND DE-DUPLICATED exactly as the applier
		// folds it, because this is what the applier would have written:
		// a converter that kept the authored spelling would make an
		// imported chart and a written one differ in a table that claims
		// identity.
		if edges := sortedKeys(seat.Manages); len(edges) > 0 {
			out.Manages[handle] = edges
		}
	}
	return out
}

// Edges is this authored company's structure, as one import's placements.
//
// THE ORDER IS PARENTS BEFORE CHILDREN, which the apply does not require — a
// placement creates a stub for an object that is not there yet — but which
// makes the record readable: an operator reading the log sees a chart being
// built rather than a set of references in whatever order a map produced.
// ImportKey is the ledger key for one authored chart: a hash of its own
// content.
//
// STABLE ACROSS PROCESSES AND NODES, which is the whole requirement — two
// nodes booting the same file, or a node and the command line importing it,
// must produce the SAME key or each would import the same structure under a
// name the other's ledger does not hold, and every one of them would rewrite
// every row and wake everybody again.
//
// So it is a hash of the canonical JSON of the authored value and of nothing
// derived from the process that computed it: no time, no node id, no map
// iteration. The converter builds slices in document order and the edge list
// is parents before children, which is what makes the JSON canonical without
// anybody sorting it.
//
// HERE RATHER THAN BESIDE EITHER CALLER, because there are two — the boot
// seed and `crewlet config import` — and a second implementation of a hash is
// a key that agrees with itself and with nothing else.
func ImportKey(a Authored) string {
	body, err := marshal(a)
	if err != nil {
		// UNREACHABLE: Authored is slices of strings and named string
		// types. Falling back to a time-based key would be worse than
		// the failure, because it would re-import on every boot.
		return "file:unencodable"
	}
	return fmt.Sprintf("file:%x", sha256.Sum256(body))
}

func (a Authored) Edges() []Edge {
	out := make([]Edge, 0, len(a.Units)+len(a.Seats))
	placed := map[string]bool{}
	remaining := make([]AuthoredUnit, len(a.Units))
	copy(remaining, a.Units)

	// A UNIT WHOSE PARENT IS NOT PLACED YET WAITS ONE ROUND. Bounded by
	// the number of units, because each round places at least one or the
	// remainder is unplaceable — a cycle, or a parent nothing declares —
	// and both of those are placed at the root rather than dropped: a
	// record that silently omitted an object would leave it out of the
	// chart with nothing reporting it.
	for round := 0; round <= len(a.Units) && len(remaining) > 0; round++ {
		var next []AuthoredUnit
		progressed := false
		for _, unit := range remaining {
			key := NormalizeKey(unit.Key)
			parent := NormalizeKey(unit.Parent)
			if parent != "" && !placed[parent] && declaresUnit(a.Units, parent) {
				next = append(next, unit)
				continue
			}
			out = append(out, Edge{
				Object: ObjectRef{Kind: KindUnit, ID: key},
				Parent: parent, Lead: NormalizeKey(unit.Lead),
			})
			placed[key] = true
			progressed = true
		}
		if !progressed {
			// THE REMAINDER IS UNPLACEABLE IN ORDER, so it is placed
			// as written and the chart's own rules decide: a cycle is
			// refused by the batch validator, and a parent that is not
			// there leaves the unit at the root.
			for _, unit := range next {
				out = append(out, Edge{
					Object: ObjectRef{Kind: KindUnit,
						ID: NormalizeKey(unit.Key)},
					Parent: NormalizeKey(unit.Parent),
					Lead:   NormalizeKey(unit.Lead),
				})
			}
			next = nil
		}
		remaining = next
	}

	for _, seat := range a.Seats {
		out = append(out, Edge{
			Object: ObjectRef{Kind: KindSeat, ID: NormalizeKey(seat.Handle)},
			Parent: NormalizeKey(seat.Unit),
		})
	}
	return out
}

// declaresUnit reports whether this company declares a unit with that key.
func declaresUnit(units []AuthoredUnit, key string) bool {
	for _, unit := range units {
		if NormalizeKey(unit.Key) == key {
			return true
		}
	}
	return false
}
