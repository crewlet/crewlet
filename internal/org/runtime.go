package org

import (
	"encoding/json"
	"fmt"
)

// THE HALF OF A SEAT THE CHART CANNOT SPEAK FOR.
//
// # The split, and where each half lives
//
// The chart domain owns who exists, where they sit and who reports to whom. It
// can say what every one of those means, validate them, arbitrate them and
// render them. It cannot say what an `mcp_env` key is for, what a model chain
// falls back to, or which sandbox cell a seat runs in — and a chart that grew
// a column per runtime setting would be the company document again with a log
// underneath it.
//
// So a seat is stored as ROWS for its structure and identity, and as ONE
// OPAQUE DOCUMENT for everything else. These functions are both ends of that
// document, and they are here because this package owns the shape: [Role] has
// the fields and the JSON tags, and the chart carries bytes.
//
// # What is deliberately NOT in it
//
// Every field the chart has a column, an edge table or a subject for. Writing
// those into the document as well would be two copies of one fact, and the
// copy inside an opaque blob is the one nothing validates, nothing indexes and
// nothing can arbitrate — so a rename that moved the row would leave the old
// name inside the document, and the view would read whichever half it
// unpacked last.
//
// [TestTheRuntimeDocumentAndTheChartRowsPartitionASeat] is what keeps that
// exact rather than remembered: it walks [Role]'s own fields and fails on one
// that is in neither half.

// seatRowFields are the fields the CHART's rows carry, which the runtime
// document therefore must not.
//
// Each is owned by something in the chart that is not a blob: a column
// (`name`, `kind`, `email`, `backstory`, `goal`, `project`, `space`), an
// authored edge table (`manages`), the structure's own subject (`unit`), or
// the row's primary key (`handle`). `AutoManaged` is in neither half because
// it is DERIVED — the view computes it from the lead cascade on every build —
// and storing a derivation is how one goes stale with nothing recomputing it.
var seatRowFields = map[string]string{
	"Name":                 "the row's `name` column",
	"Kind":                 "the row's `kind` column",
	"DeclaredHandle":       "the row's primary key",
	"Email":                "the row's `email` column, plus the derived `email_index`",
	"Backstory":            "the row's `backstory` column",
	"Goal":                 "the row's `goal` column",
	"Responsibilities":     "the row's own column",
	"BehavioralGuidelines": "the row's own column",
	"Project":              "the row's `project` column",
	"Space":                "the row's `space` column",
	"Manages":              "the `chart_manages` edge table, which is indexed in the unauthored direction",
	"UnitRef":              "the row's `unit_key`, written only by a record on the structure's own subject",
	"AutoManaged":          "derived by the view from the lead cascade on every build",
}

// unitRowFields is [seatRowFields] for a unit.
var unitRowFields = map[string]string{
	"Name":            "the row's `name` column",
	"ID":              "the row's primary key",
	"Type":            "the row's `type` column",
	"Purpose":         "the row's `purpose` column",
	"Goals":           "the row's own column",
	"Channel":         "the row's `channel` column",
	"Project":         "the row's `project` column",
	"Space":           "the row's `space` column",
	"KnowledgeRefs":   "the row's own column",
	"Lead":            "the `chart_leads` edge table",
	"DeclaredLead":    "the cascade's own bookkeeping, `json:\"-\"` and never stored: a row IS the declaration, so the view records it while it builds",
	"DeclaredChannel": "the cascade's own bookkeeping, like DeclaredLead",
	"Roles":           "the seats' own rows, found by `unit_key`",
	"Children":        "the child units' own rows, found by `parent_key`",
}

// SeatRuntime is one seat's engine-only content, encoded for the chart to
// carry.
//
// THE ROW-OWNED FIELDS ARE CLEARED rather than omitted by a tag, because they
// are legitimately part of [Role] on every other path: the same type is what
// a document parses into and what a turn reads.
func SeatRuntime(r *Role) (json.RawMessage, error) {
	if r == nil {
		return nil, nil
	}
	content := *r
	content.Name, content.Kind, content.DeclaredHandle = "", "", ""
	content.Email, content.Backstory, content.Goal = "", "", ""
	content.Responsibilities, content.BehavioralGuidelines = nil, nil
	content.Project, content.Space = "", ""
	content.Manages, content.AutoManaged, content.UnitRef = nil, nil, ""

	body, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("org: encode the seat's runtime document: %w", err)
	}
	return body, nil
}

// ApplySeatRuntime decodes a runtime document onto a seat.
//
// IT RUNS BEFORE THE ROW'S OWN FIELDS ARE SET, so a document that somehow
// carries one loses to the row rather than the other way round: the row is
// what a write arbitrated on, and the blob is what a write carried.
func ApplySeatRuntime(r *Role, body json.RawMessage) error {
	if r == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, r); err != nil {
		return fmt.Errorf("org: decode the seat's runtime document: %w", err)
	}
	return nil
}

// UnitRuntime is [SeatRuntime] for a unit.
func UnitRuntime(u *Unit) (json.RawMessage, error) {
	if u == nil {
		return nil, nil
	}
	content := *u
	content.Name, content.ID, content.Type, content.Purpose = "", "", "", ""
	content.Goals, content.Channel = nil, ""
	content.Project, content.Space, content.KnowledgeRefs = "", "", nil
	content.Lead = ""
	content.DeclaredLead, content.DeclaredChannel = "", ""
	content.Roles, content.Children = nil, nil

	body, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("org: encode the unit's runtime document: %w", err)
	}
	return body, nil
}

// ApplyUnitRuntime is [ApplySeatRuntime] for a unit.
func ApplyUnitRuntime(u *Unit, body json.RawMessage) error {
	if u == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, u); err != nil {
		return fmt.Errorf("org: decode the unit's runtime document: %w", err)
	}
	return nil
}
