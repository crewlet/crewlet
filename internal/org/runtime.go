package org

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/secrets"
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
	"OriginHandle":         "the row's `origin_handle`, frozen by the first rename and written by nothing else",
	"FormerHandles":        "the row's `former_keys_json`",
	"Email":                "the row's `email` column, a sealed address's `${VAR}` reference",
	"Backstory":            "the row's `backstory` column",
	"Goal":                 "the row's `goal` column",
	"Responsibilities":     "the row's own column",
	"BehavioralGuidelines": "the row's own column",
	"Project":              "the row's `project` column",
	"Space":                "the row's `space` column",
	"Manages":              "the `chart_manages` edge table, which is indexed in the unauthored direction",
	"UnitRef":              "the row's `unit_key`, written only by a record on the structure's own subject",
	"AutoManaged":          "derived by the view from the lead cascade on every build",
	"Incomplete":           "derived from the row's `version`: zero until a content record fills the seat",
}

// unitRowFields is [seatRowFields] for a unit.
var unitRowFields = map[string]string{
	"Name":            "the row's `name` column",
	"ID":              "the row's primary key",
	"OriginKey":       "the row's `origin_key`, frozen by the first rename and written by nothing else",
	"FormerKeys":      "the row's `former_keys_json`",
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
// carry — and NIL where the seat has none.
//
// THE ROW-OWNED FIELDS ARE CLEARED rather than omitted by a tag, because they
// are legitimately part of [Role] on every other path: the same type is what
// a document parses into and what a turn reads.
//
// # An empty runtime half is no runtime half
//
// Clearing is not the same as leaving nothing: `name` is authored on every
// seat and carries no `omitempty`, so a seat with no model chain, no
// credentials and no contact encoded to `{"name":""}` — eleven bytes that say
// nothing. And the chart classes a content write by whether its runtime half
// CHANGES, which is the company's configuration: every seeded and imported
// seat then carried a runtime that was not empty, and a lead correcting the
// goal of a plain human seat was refused as though they had touched its
// credentials. So a document that encodes exactly as a cleared seat with
// nothing in it — the zero [Role]'s encoding, computed once rather than
// spelled — is answered as nil, which the chart stores as no runtime at all.
//
// THE ZERO VALUE'S ENCODING AND NOT A LIST OF TAGS, so a field added to
// [Role] without `omitempty` moves the reference with it rather than making
// every seat's runtime non-empty again.
func SeatRuntime(r *Role) (json.RawMessage, error) {
	if r == nil {
		return nil, nil
	}
	return encodeRuntime(clearedSeat(*r), emptySeatRuntime, "seat")
}

// clearedSeat is a seat with every field the chart's rows own set to zero:
// what is left is the runtime half ([seatRowFields]).
func clearedSeat(content Role) Role {
	content.Name, content.Kind, content.DeclaredHandle = "", "", ""
	content.Email, content.Backstory, content.Goal = "", "", ""
	content.Responsibilities, content.BehavioralGuidelines = nil, nil
	content.Project, content.Space = "", ""
	content.Manages, content.AutoManaged, content.UnitRef = nil, nil, ""
	content.Incomplete = false
	content.OriginHandle, content.FormerHandles = "", nil
	return content
}

// emptySeatRuntime is the encoding of a seat whose runtime half holds nothing.
var emptySeatRuntime = sync.OnceValues(func() ([]byte, error) {
	return json.Marshal(clearedSeat(Role{}))
})

// encodeRuntime encodes one cleared object, and answers nil where it encodes
// exactly as the empty one does — see [SeatRuntime].
func encodeRuntime(content any, empty func() ([]byte, error), what string) (
	json.RawMessage, error) {

	body, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("org: encode the %s's runtime document: %w", what, err)
	}
	nothing, err := empty()
	if err != nil {
		return nil, fmt.Errorf("org: encode an empty %s's runtime document: %w",
			what, err)
	}
	if bytes.Equal(body, nothing) {
		return nil, nil
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

// UnitRuntime is [SeatRuntime] for a unit: its engine-only content, and nil
// where it has none.
func UnitRuntime(u *Unit) (json.RawMessage, error) {
	if u == nil {
		return nil, nil
	}
	return encodeRuntime(clearedUnit(*u), emptyUnitRuntime, "unit")
}

// clearedUnit is [clearedSeat] for a unit ([unitRowFields]).
func clearedUnit(content Unit) Unit {
	content.Name, content.ID, content.Type, content.Purpose = "", "", "", ""
	content.Goals, content.Channel = nil, ""
	content.Project, content.Space, content.KnowledgeRefs = "", "", nil
	content.Lead = ""
	content.DeclaredLead, content.DeclaredChannel = "", ""
	content.OriginKey, content.FormerKeys = "", nil
	content.Roles, content.Children = nil, nil
	return content
}

// emptyUnitRuntime is [emptySeatRuntime] for a unit.
var emptyUnitRuntime = sync.OnceValues(func() ([]byte, error) {
	return json.Marshal(clearedUnit(Unit{}))
})

// RuntimeShape is where a runtime half keeps its credentials: [chart.Runtime],
// answered from this package's own types.
//
// THE TAGS ARE THE ANSWER. Every field of [Role] and [Unit] that holds a
// credential carries `secret:"true"` — the same tag the authored config's
// fields carry, read by the same [secrets.Field] — and the half is walked
// against the type it decodes onto ([secrets.Walk]), so a credential field
// added to a seat is sealed by the chart the day it lands, with no list here
// or there for anybody to remember. A test in internal/config holds the two
// sets of tags against each other through the conversion that builds this
// half from an authored seat, so neither can drop one the other has.
type RuntimeShape struct{}

// The types the two halves decode onto, reflected once.
var (
	seatRuntimeType = reflect.TypeOf(Role{})
	unitRuntimeType = reflect.TypeOf(Unit{})
)

// Credentials implements [chart.Runtime].
func (RuntimeShape) Credentials(kind chart.ObjectKind, runtime json.RawMessage,
	visit secrets.Visit) (json.RawMessage, error) {

	switch kind {
	case chart.KindSeat:
		return secrets.Walk(seatRuntimeType, runtime, visit)
	case chart.KindUnit:
		return secrets.Walk(unitRuntimeType, runtime, visit)
	}
	return nil, fmt.Errorf("org: a %s carries no runtime half", kind)
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
