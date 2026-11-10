package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A SEAT IS TOLD THE BOXES ITS RUNS USE (ADR-0104): each cell its executor
// chain places a run in, the memory the document holds a container box to, and
// what a run last found in the box, kept per seat and cell in the coordination
// store under the basis the box was made from. A reading under another basis
// is a box the document no longer makes, and is not told.

// seatBoxes is what seat handle is told of its boxes. A reading the store does
// not answer is left out, never guessed: the cell and the document's memory
// are still told.
func (e *Engine) seatBoxes(ctx context.Context, cfg *config.Company, o *org.Organization,
	handle string) []capability.Box {

	boxes := cfg.SeatBoxesOf(handle)
	if len(boxes) == 0 {
		return nil
	}
	readings := e.boxReadings()
	seat, seated := o.AgentIDFor(o.AgentSeatByHandle(handle))
	out := make([]capability.Box, 0, len(boxes))
	for _, b := range boxes {
		box := capability.Box{Cell: string(b.Cell)}
		if b.Cell == config.PlacementContainer {
			box.MemoryBytes, _ = cfg.Providers.Sandbox.Local.MemoryLimit()
		}
		if readings != nil && seated {
			r, found, err := readings.BoxReading(ctx, seat, string(b.Cell))
			switch {
			case err != nil:
				log.WarnContext(ctx, "box_reading_unread", "agent", handle, "cell", string(b.Cell),
					"error", err.Error(), "detail", "this turn is told its box from the document alone")
			case found && r.Basis == cfg.BoxBasis(b.Seat, b.Cell):
				box.Observed, box.Browser, box.ContainerRuntime = true, r.Browser, r.ContainerRuntime
				if r.MemoryBytes > 0 {
					box.MemoryBytes = r.MemoryBytes
				}
			}
		}
		out = append(out, box)
	}
	return out
}

// keepBoxReading keeps what a launch of seat handle in cell found in its fresh
// box, under the basis the box was made from; an unchanged reading is not
// written again. A store that does not answer costs the seat this reading,
// which the next fresh box makes again.
func (e *Engine) keepBoxReading(ctx context.Context, company *Company, handle string,
	cell sandbox.Placement, read sandbox.BoxFacts) {

	readings := e.boxReadings()
	if readings == nil || company == nil || company.Config == nil || company.Org == nil {
		return
	}
	seat, ok := company.Org.AgentIDFor(company.Org.AgentSeatByHandle(handle))
	if !ok {
		return
	}
	var basis string
	for _, b := range company.Config.SeatBoxesOf(handle) {
		if string(b.Cell) == string(cell) {
			basis = company.Config.BoxBasis(b.Seat, b.Cell)
		}
	}
	if basis == "" {
		return
	}
	r := coord.BoxReading{Seat: seat, Cell: string(cell), Basis: basis, MemoryBytes: read.MemoryBytes,
		Browser: read.Browser, ContainerRuntime: read.ContainerRuntime, At: time.Now().UTC()}
	if was, found, err := readings.BoxReading(ctx, seat, r.Cell); err == nil && found && sameReading(was, r) {
		return
	}
	if err := readings.PutBoxReading(ctx, r); err != nil {
		log.WarnContext(ctx, "box_reading_unkept", "agent", handle, "cell", r.Cell, "error", err.Error(),
			"detail", "the seat is told its box from the document until a fresh box is read again")
	}
}

// sameReading reports whether two readings say the same of one box.
func sameReading(a, b coord.BoxReading) bool {
	return a.Basis == b.Basis && a.MemoryBytes == b.MemoryBytes && a.Browser == b.Browser &&
		a.ContainerRuntime == b.ContainerRuntime
}

// boxReadings is the fleet's box readings, nil on a node with no fleet store.
func (e *Engine) boxReadings() coord.BoxReadings {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return e.backends.Fleet
}

// clearRemovedBoxReadings drops the readings of every seat next no longer has;
// only the fleet's company clears.
func (e *Engine) clearRemovedBoxReadings(ctx context.Context, next *Company) {
	readings := e.boxReadings()
	if next == nil || next.Org == nil || readings == nil {
		return
	}
	if err := e.fleetCompany(ctx, next); err != nil {
		return
	}
	all, err := readings.ListBoxReadings(ctx)
	if err != nil {
		log.WarnContext(ctx, "box_readings_not_swept", "error", err.Error(),
			"detail", "a removed seat's box readings are dropped by the next apply")
		return
	}
	for _, r := range all {
		if next.Org.AgentSeatByID(r.Seat) != nil {
			continue
		}
		if err := readings.DeleteBoxReading(ctx, r.Seat, r.Cell); err != nil {
			log.WarnContext(ctx, "box_reading_not_dropped", "seat", r.Seat.String(), "error", err.Error())
		}
	}
}
