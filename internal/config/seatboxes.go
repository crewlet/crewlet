package config

import (
	"fmt"
	"iter"
	"maps"
	"path"
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
// ONE BOX PER SEAT PER CELL ITS EXECUTOR CAN LAND IN. Every agent seat's
// executor is its coding CLI's own run, and a launch takes the first member of
// the seat's executor chain that can run it and is not sitting out on its
// node — so any runner-capable member may be the one launched, and each is
// placed by its own `run_in`, else the catalogue's default
// ([SandboxProvider.RunIn]). Two members in one cell open the same kind of
// box, so the cell is counted once, under the first of them. Each box is
// given the engine-wide steps followed by the seat's own, which is what the
// engine's runEnvironment builds for it.
//
// A SECOND RESOLUTION OF THE ENGINE'S only in its placing: the chain is
// [phase.Resolve]'s, the call the engine launches through, so the two cannot
// disagree about which entries a seat reaches. The rest — the cell each box
// falls to, and the steps and their order — is held to the engine's own
// launch path by a case in internal/engine, since a table here would agree
// with this function whatever the engine did.
//
// Nothing when the company configures no sandbox: no box exists, and the
// seats that would need one are refused on their own.
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
// A LOCAL box writes one only inside its home, by [hostbox.InHome] — the rule
// both local boxes call themselves. A direct box's home is minted per box, so
// no document names it — the empty home. A container box's is its mount,
// [hostbox.ContainerHome].
//
// An E2B box writes one through its own agent, which takes almost any path
// ([e2bRefusesSetupFile]).
func setupFileRefused(cell Placement, file string) bool {
	switch cell {
	case PlacementDirect:
		_, inside := hostbox.InHome("", file)
		return !inside
	case PlacementContainer:
		_, inside := hostbox.InHome(hostbox.ContainerHome, file)
		return !inside
	case PlacementE2B:
		return e2bRefusesSetupFile(file)
	}
	return false
}

// e2bRefusesSetupFile reports whether an E2B box's own agent (envd) refuses
// to write a setup file at file, judged on the path alone.
//
// That agent expands a leading "~" to the home of the account every template
// runs as, joins a relative path onto that home, and creates the file
// wherever the result is — missing directories included, an absolute path
// outside the home too. It refuses two shapes. A path starting "~name" (the
// same tilde test as [hostbox.InHome]'s) is one it will not expand, and the
// write fails. And a path naming a DIRECTORY is answered "path is a
// directory". Only some directories are certain without seeing the template:
// the home itself and every directory above it, which exist in every box.
// Any other one a path could name — /tmp, /etc — is the template's, so a
// document cannot be judged on it, and the box says so at setup.
//
// Judged on the box's own paths, which are Linux paths whatever the engine
// host is, so with package path rather than filepath.
func e2bRefusesSetupFile(file string) bool {
	resolved := file
	switch {
	case strings.HasPrefix(file, "~"):
		if len(file) > 1 && file[1] != '/' && file[1] != '\\' {
			return true
		}
		resolved = path.Join(hostbox.ContainerHome, file[1:])
	case !path.IsAbs(file):
		resolved = path.Join(hostbox.ContainerHome, file)
	}
	resolved = path.Clean(resolved)
	// The home, then each directory above it up to the root.
	for dir := hostbox.ContainerHome; ; dir = path.Dir(dir) {
		if resolved == dir {
			return true
		}
		if dir == "/" {
			return false
		}
	}
}

// validateSetupFileHomes refuses a setup step that writes a file a box it is
// applied to would refuse.
//
// A direct box writes a setup file only inside its own home, and a container
// box only under its home mount, and each refuses anything else when the box
// is provisioned — a refusal that fails the acquisition and tears the box
// down. So a document whose step names /usr/local/bin for a seat whose
// executor runs `direct` validated cleanly and then failed every box that
// seat opened, at setup, long after the write. The reference example shipped exactly that.
// An E2B box takes almost any path, and refuses the few shapes
// [e2bRefusesSetupFile] names the same way.
//
// THE LOCAL BOXES' OWN RULE, NOT A COPY OF IT: a path is judged by
// [hostbox.InHome], which both local boxes call before they touch a file, so
// what this refuses and what they refuse cannot drift apart. What it cannot
// judge is the host's half — a link an existing directory on the engine host
// holds — which only the box can see; nor, in an E2B box, a directory the
// template made. And a seat's boxes are found the way the engine places them
// ([Company.SeatBoxes]), so a step is judged in every cell it actually
// reaches, and in no other: an engine-wide step that writes /usr/local/bin is
// fine in a company whose seats all run in E2B.
//
// AN ERROR, NOT A WARNING. A warning is for what is VALID and worth knowing;
// a step whose file no box it reaches will ever write is not valid, and a
// warning would leave that seat's code work broken for good behind a line in
// a log most deployments never read.
//
// AN ADMISSION RULE, not a runnable one: the company runs, and only the boxes
// that step reaches fail — so a stored revision that keeps such a step is
// applied and warned about, as [LLMProvider.validateCredentials] is, rather
// than taking a node off the fleet's epoch.
//
// One problem per file and cell, at the file's entry in the step that writes
// it, naming the first seat whose box it fails and how many more it fails
// too: an engine-wide step reaches every seat, and a problem per seat would
// bury the one line to change under the seats that inherit it.
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
// lands in a container's and an E2B box's. A command is the other way, for a
// step that needs the file's absolute path itself.
func setupFileRefusal(step, file string, cell Placement, first SeatBox, others int) string {
	var why string
	otherHome := strings.HasPrefix(file, "~") && len(file) > 1 && file[1] != '/' && file[1] != '\\'
	switch {
	case otherHome && cell == PlacementE2B:
		why = "a path starting ~name names another account's home, which its agent " +
			"will not expand"
	case otherHome:
		why = "a path starting ~name names another account's home, and a box writes " +
			"a setup file only under its own"
	case cell == PlacementE2B:
		why = fmt.Sprintf("the path names its home at %s or a directory above it, and "+
			"its agent refuses to write a file over a directory", hostbox.ContainerHome)
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
	article := "a"
	if cell == PlacementE2B {
		article = "an"
	}
	return fmt.Sprintf("setup step %q writes %s, which %s %s box refuses: %s. %s, so "+
		"every such box fails at setup. Name the file relative to the box's home "+
		"(.npmrc, .local/bin/tool, or ~/.npmrc), which every cell writes under its "+
		"home, or have a command of the step write it under \"$HOME\"",
		step, file, article, cell, why, whose)
}
