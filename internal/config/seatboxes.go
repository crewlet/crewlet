package config

import (
	"fmt"
	"iter"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// SeatBox is one box a seat's executor can launch in, placed as the engine
// places it.
type SeatBox struct {
	// Seat is the seat whose executor runs in the box, and At where it is
	// written.
	Seat *Role
	At   Path

	// Cell is where the box runs: the cell its entry names, else the
	// catalogue's default, which is how the engine resolves an empty one at
	// launch. Empty when neither answers, which validation refuses on its
	// own ([Company.validateSandboxPlacement]).
	Cell Placement

	// Executor is the providers.llm entry the box's run launches on: the
	// first runner-capable member of the seat's executor chain placed in
	// this cell.
	Executor string

	// Steps are the setup steps the box is given, in the order the engine
	// applies them: providers.sandbox.setup, then the seat's own
	// sandbox.setup.
	Steps []BoxStep
}

// BoxStep is one setup step a box is given, and where it is written.
type BoxStep struct {
	At   Path
	Step *SandboxSetupStep
}

// SeatBoxes is every box a seat's executor can launch in, seat by seat in the
// order [Company.EachRole] walks them, placed as the engine places each one.
//
// ONE BOX PER SEAT PER CELL ITS EXECUTOR CAN LAND IN. A launch takes the first
// chain member that can run it and is not sitting out on its node, so any
// runner-capable member may be it, placed by its own `run_in`, else the
// catalogue's default ([SandboxProvider.RunIn]). Members in one cell open the
// same box, counted once. Each gets the engine-wide steps, then the seat's own.
//
// A SECOND RESOLUTION OF THE ENGINE'S only in its placing: the chain is
// [phase.Resolve]'s. Cells and step order are held to the engine's launch path
// by a case in internal/engine; a table here would agree whatever it did.
func (c *Company) SeatBoxes() iter.Seq[SeatBox] {
	return func(yield func(SeatBox) bool) {
		catalogue := c.Providers.Sandbox
		if !catalogue.Enabled() {
			return
		}
		for seat := range c.executorSeats() {
			runners := c.runners(seat.chain)
			if len(runners) == 0 {
				// Refused on its own: a chain no launch can run.
				continue
			}
			steps := make([]BoxStep, 0, len(catalogue.Setup))
			for i := range catalogue.Setup {
				steps = append(steps, BoxStep{
					At: idx(field("providers.sandbox.setup"), i), Step: &catalogue.Setup[i],
				})
			}
			if gate := seat.role.Sandbox; gate != nil {
				for i := range gate.Setup {
					steps = append(steps, BoxStep{
						At: idx(at(seat.path, "sandbox.setup"), i), Step: &gate.Setup[i],
					})
				}
			}
			placed := map[Placement]bool{}
			for _, key := range runners {
				cell := c.EntryCell(key)
				if placed[cell] {
					continue
				}
				placed[cell] = true
				if !yield(SeatBox{Seat: seat.role, At: seat.path, Cell: cell, Executor: key, Steps: steps}) {
					return
				}
			}
		}
	}
}

// EntryCell is the cell a run launched on providers.llm.<key> is placed in:
// the entry's own `run_in`, else the catalogue's default — which is how the
// engine's manager resolves an empty one at launch. Empty where neither
// answers: no catalogue, or no such entry and no default.
//
// ONE RESOLUTION for every reader that has to agree with the launch — the
// boxes validation places ([Company.SeatBoxes]) and what `crewlet llm doctor`
// tells an operator a run on the entry needs — so the cell a check reasons
// about is the cell the run is put in.
func (c *Company) EntryCell(key string) Placement {
	if cell := c.Providers.LLM[key].RunIn; cell != "" {
		return cell
	}
	return c.Providers.Sandbox.RunIn()
}

// setupFileRefused reports whether a box in cell refuses to write a setup
// file at file, as far as that can be judged before any box exists.
//
// A box writes one only inside its home, by [hostbox.InHome] — the rule both
// cells call themselves. A direct box's home is minted per box, so no document
// names it — the empty home. A container box's is its mount,
// [hostbox.ContainerHome].
func setupFileRefused(cell Placement, file string) bool {
	switch cell {
	case PlacementDirect:
		_, inside := hostbox.InHome("", file)
		return !inside
	case PlacementContainer:
		_, inside := hostbox.InHome(hostbox.ContainerHome, file)
		return !inside
	}
	return false
}

// validateSetupFileHomes refuses a setup step that writes a file a box it is
// applied to would refuse. A direct box writes only inside its home, a
// container box only under its home mount, each refusing at provisioning.
//
// THE BOXES' OWN RULE, NOT A COPY: paths are judged by [hostbox.InHome], which
// both cells call. Not judged: the host's links. Boxes are found as the engine
// places them ([Company.SeatBoxes]).
//
// AN ERROR, NOT A WARNING, yet AN ADMISSION RULE: a stored revision keeping
// such a step is applied and warned about, as [LLMProvider.validateCredentials] is.
// One problem per file and cell, naming the first seat and how many more.
func (c *Company) validateSetupFileHomes() error {
	if c.Providers.Sandbox.Enabled() && c.Providers.Sandbox.Fake {
		// The double writes nothing anywhere, so it refuses nothing.
		return nil
	}
	type refusal struct {
		at    Path
		step  string
		file  string
		cell  Placement
		boxes []SeatBox
		seats map[*Role]struct{}
	}
	found := map[string]*refusal{}
	var order []*refusal
	for box := range c.SeatBoxes() {
		for _, step := range box.Steps {
			for _, file := range slices.Sorted(maps.Keys(step.Step.Files)) {
				if strings.TrimSpace(file) == "" {
					// Refused on its own, as a path naming nothing.
					continue
				}
				if !setupFileRefused(box.Cell, file) {
					continue
				}
				key := step.At.String() + "\x00" + file + "\x00" + string(box.Cell)
				r := found[key]
				if r == nil {
					r = &refusal{at: step.At, step: step.Step.Name, file: file, cell: box.Cell,
						seats: map[*Role]struct{}{}}
					found[key] = r
					order = append(order, r)
				}
				r.boxes = append(r.boxes, box)
				r.seats[box.Seat] = struct{}{}
			}
		}
	}
	var p problems
	for _, r := range order {
		p.add(entry(at(r.at, "files"), r.file), ErrConflict, "%s",
			setupFileRefusal(r.step, r.file, r.cell, r.boxes[0], len(r.seats)-1))
	}
	return p.err()
}

// setupFileRefusal is the sentence for a setup file a box refuses: what the
// step writes, why the cell refuses it, whose box it fails, and the remedies,
// each of which works in every cell. The relative path comes first, because
// it is the one a file entry can always write: a direct box's home is minted
// per box, so no absolute path names it, and a relative one lands in it as it
// lands in a container's. A command is the other way, for a step that needs
// the file's absolute path itself.
func setupFileRefusal(step, file string, cell Placement, first SeatBox, others int) string {
	var why string
	// hostbox.InHome's tilde grammar: only a slash may follow a leading ~.
	tilde := len(file) > 1 && file[0] == '~' && file[1] != '/'
	switch {
	case tilde && file[1] == '\\':
		why = "a leading ~ is the box's home only before a /, and a backslash is a " +
			"character of a file name, not a separator"
	case tilde:
		why = "a path starting ~name names another account's home, and a box writes " +
			"a setup file only under its own"
	case !filepath.IsAbs(file):
		why = "a relative path, or one starting ~/, is under the box's home, and this " +
			"one climbs out of it or names the home itself, which is no file in it"
	case cell == PlacementDirect:
		why = "it writes a setup file only inside its own home, which it mints per " +
			"box, so no absolute path a document holds is inside it"
	default:
		why = fmt.Sprintf("it writes a setup file only under its home mount at %s",
			hostbox.ContainerHome)
	}
	whose := fmt.Sprintf("Seat %q runs its executor in one (providers.llm.%s)",
		first.Seat.Seat().Handle(), first.Executor)
	switch {
	case others == 1:
		whose += ", and 1 more seat does too"
	case others > 1:
		whose += fmt.Sprintf(", and %d more seats do too", others)
	}
	return fmt.Sprintf("setup step %q writes %s, which a %s box refuses: %s. %s, so "+
		"every such box fails at setup. Name the file relative to the box's home "+
		"(.npmrc, .local/bin/tool, or ~/.npmrc), which every cell writes under its "+
		"home, or have a command of the step write it under \"$HOME\"",
		step, file, cell, why, whose)
}
