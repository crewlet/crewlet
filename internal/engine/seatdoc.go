package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ONE SEAT'S WHOLE DOCUMENT, read and written through the CHART.
//
// # What this replaced, and why the shape moved with it
//
// A per-seat write used to go through /config's entity route, addressing a
// seat by its handle inside the stored revision — because a merge patch
// replaces an array wholesale, so patching `roles` to change one seat would
// delete every other one. A revision carries no seats any more: the org
// chart is a domain of its own, with its own records and its own per-object
// arbitration, which is exactly the property that route was approximating.
//
// So the read and the write move to the chart, and the DOCUMENT moves with
// them: from the AUTHORED config.Role to the RUNTIME [org.Role] the chart's
// own blob holds. The difference a caller sees is one level of nesting — a
// seat's Slack identity is `slack` rather than `integrations.slack`, and its
// GitHub App is `github` rather than `integrations.github`. That is the
// running seat's own shape, which is what the opaque half IS.
//
// # It writes the WHOLE seat, and that is not a merge
//
// A chart content record is full post-state, so a write carries everything
// the read returned and a field left out is a field set to empty. Read, edit,
// write back is the gesture. The read is taken from this node's own rows
// rather than from whatever the caller happens to be holding, which is what
// stops a stale form clearing a field nobody touched.
//
// # What it deliberately does not carry back
//
// The view's DERIVATIONS. [org.SeatFrom] is the row rather than the view, so
// a `manages:` entry naming a unit is still a unit key and no auto-managed
// handle has been added. Writing an expanded list back would turn an answer
// the view recomputes on every build into a stored one that nothing
// recomputes, and it would grow every time a pass ran.

// SeatDocument is one seat's whole document, as JSON.
func (e *Engine) SeatDocument(ctx context.Context, handle string) ([]byte, error) {
	reader := e.Chart()
	if reader == nil {
		return nil, fmt.Errorf("engine: this node runs no chart domain, so it " +
			"cannot read a seat")
	}
	// LINEARIZABLE, because the caller is about to edit what it reads and
	// write it back whole: a read from a node that is behind would take a
	// seat's state as it was before somebody else's edit and then publish
	// that state back over theirs.
	detail, err := reader.Seat(ctx, handle, statelog.Freshness{
		Level: statelog.ReadLinearizable,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: read the seat %s: %w", handle, err)
	}
	if detail.Seat.Handle == "" {
		return nil, fmt.Errorf("engine: this company has no seat %q", handle)
	}
	body, err := json.Marshal(org.SeatFrom(detail.Seat, detail.Manages))
	if err != nil {
		return nil, fmt.Errorf("engine: encode the seat %s: %w", handle, err)
	}
	return body, nil
}

// SetSeatDocument writes one seat's whole document back, reporting where it
// landed on the chart's log.
//
// THE POSITION RATHER THAN A REVISION, because there is no revision: a seat
// is not part of the stored configuration any more. A caller that reported a
// revision id here would be reporting the one this write did not make.
//
// BY IS THE PARTY THE RECORD RECORDS, whole: the author, its kind and the
// credential the write came through — the same three `/chart` records for a
// write it takes itself.
func (e *Engine) SetSeatDocument(ctx context.Context, handle string, body []byte,
	summary string, by iam.Actor) (statelog.Position, error) {

	reader, writer := e.Chart(), e.ChartWriter()
	if reader == nil || writer == nil {
		return statelog.Position{}, fmt.Errorf("engine: this node runs no chart " +
			"domain, so it cannot write a seat")
	}
	var role org.Role
	if err := json.Unmarshal(body, &role); err != nil {
		return statelog.Position{}, fmt.Errorf("engine: decode the seat %s: %w",
			handle, err)
	}
	runtime, err := org.SeatRuntime(&role)
	if err != nil {
		return statelog.Position{}, err
	}
	// THE UNIT COMES FROM THE ROW, never from the document. A seat's
	// placement is STRUCTURE, written only by a structural record, and the
	// content write has to state the unit it believes the seat is in so
	// that its own blast radius is filed under that team — see
	// [chart.SeatContent.Unit]. Taking it from an edited document would let
	// a provisioning pass move somebody.
	detail, err := reader.Seat(ctx, handle, statelog.Freshness{
		Level: statelog.ReadLinearizable,
	})
	if err != nil {
		return statelog.Position{}, fmt.Errorf("engine: read the seat %s: %w",
			handle, err)
	}
	if detail.Seat.Handle == "" {
		return statelog.Position{}, fmt.Errorf("engine: this company has no seat %q",
			handle)
	}
	// THE CALLER IS THE PARTY, and the grant is the company's: this write
	// carries the opaque half — a vendor credential pointer, an app id —
	// which internal/chart refuses below the grant that writes the company
	// document. Every caller of this method has already been authorized at
	// its own door; what this says is which party the record records.
	//
	// THE AUTHOR AND THE CREDENTIAL APART, as `/chart` records them. This
	// was handed one name and recorded it as both, and the name was the
	// credential's: a person connecting their seat's integration through
	// their own machine token was the chart AUTHOR `pat:<id>`, of kind
	// operator — so the seat's history named a token rather than the person
	// whose authority it was, and nothing in it said who that was once the
	// token's row was swept.
	party := writer.As(by.Name, chart.AuthorKindOf(by.Kind),
		[]iam.Grant{iam.GrantConfigWrite},
		chart.Provenance{OperatorID: by.OperatorID})
	result, err := party.WriteSeat(ctx, uuid.NewString(), chart.SeatContent{
		Handle: detail.Seat.Handle, Kind: detail.Seat.Kind,
		Unit: detail.Seat.UnitKey,
		Name: role.Name, Email: role.Email,
		Backstory: role.Backstory, Goal: role.Goal,
		Responsibilities:     role.Responsibilities,
		BehavioralGuidelines: role.BehavioralGuidelines,
		Manages:              role.Manages,
		Project:              role.Project, Space: role.Space,
		Runtime: runtime,
	})
	if err != nil {
		return statelog.Position{}, fmt.Errorf("engine: write the seat %s (%s): %w",
			handle, summary, err)
	}
	return result.Result.Position, nil
}

// ImportReady reports why a whole-company import must wait, and empty when it
// need not.
//
// # What it is protecting
//
// An import rewrites the placement of EVERY object in the chart, in one
// record on the structure's own subject, and every node applies it. A node
// running an older build applies that record under its own reading of what a
// placement means — and the two are each individually correct and jointly
// wrong, which is the whole reason the protocol is versioned at all.
//
// A seat host already refuses to CLAIM under the same condition. This is the
// same question one level up, asked through the same function, because two
// spellings of it would let the fleet start claiming under a rule the import
// did not know about.
//
// # An unreadable coordination store is a REFUSAL here
//
// The seat sweep reads a failure as "not blocked", because it runs every five
// seconds and a transient blip that stopped every claim would turn a store
// hiccup into a fleet-wide stall. This runs once, at an operator's hand, so
// the opposite reading is right: refusing costs them a retry, and proceeding
// costs them a chart every older node rewrites.
func (e *Engine) ImportReady(ctx context.Context) (string, error) {
	// THE LEASE BACKEND, which is where a protocol lives: the estate
	// beside it holds counters and ledgers rather than ownership.
	fleet := e.backends.Coord
	floor, lagging, err := coord.Lagging(ctx, fleet, coord.ProtocolVersion)
	if err != nil {
		return "", fmt.Errorf("engine: this node cannot read the fleet's "+
			"protocol floor, so it cannot say whether an older build is still "+
			"running — and an import lands on every node at once: %w", err)
	}
	if !lagging {
		return "", nil
	}
	// THE NAME IS A COURTESY and never the decision. A second read that
	// found nothing still leaves the refusal standing, because the floor
	// is what said so.
	who := coord.LaggingOwner(ctx, fleet, floor)
	if who == "" {
		who = "a node this read could not name"
	}
	return fmt.Sprintf("%s is still running protocol %d and this build speaks "+
		"%d, so a rolling upgrade is in progress. An import rewrites every "+
		"placement in the chart and every node applies it, including that one "+
		"— under its own reading of what a placement means. Finish the upgrade "+
		"and import again.", who, floor, coord.ProtocolVersion), nil
}
