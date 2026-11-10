package capability

import (
	"fmt"
	"slices"
	"strings"
)

// A seat is told the box its runs use beside its access: where the box runs,
// the memory it is held to, and whether its image has a browser and a
// container runtime. A seat that is not told concludes from one failed command
// that it has no box at all, keeps that as a note, and repeats it to people.

// boxVendor is who a box change is about, as a told note's suffix names it.
const boxVendor = "sandbox"

// gib is a gibibyte, the unit a container runtime's memory flag counts in.
const gib = 1 << 30

// boxLine is one box a seat's runs use, in audience's voice; several says the
// seat's runs use more than one, by the model each launches on.
func boxLine(b Box, several bool, v voice) string {
	head := capitalized(v.your) + " box: "
	if several {
		head = "A box " + v.your + " runs can use, by the model they launch on: "
	}
	line := head + whereBox(b) + "."
	if !b.Observed {
		return line + " No run has read yet whether it has a browser or a container runtime."
	}
	return line + " " + imageWords(b)
}

// whereBox is where b runs and the memory it is held to.
func whereBox(b Box) string {
	var where, memory string
	switch b.Cell {
	case CellDirect:
		where, memory = "a directory on the engine host, run as the engine's own user", ", with"
	case CellContainer:
		where, memory = "a container on the engine host", ", held to"
	default:
		where, memory = "a remote machine of its own", ", with"
	}
	if b.MemoryBytes > 0 {
		where += memory + " " + memoryWords(b.MemoryBytes) + " of memory"
	}
	return where
}

// imageWords is what a run found in b's image.
func imageWords(b Box) string {
	browser := "no browser"
	if b.Browser != "" {
		browser = "a browser (" + b.Browser + ")"
	}
	runtime := "no container runtime, so containers do not run in it"
	if b.ContainerRuntime != "" {
		runtime = "a container runtime (" + b.ContainerRuntime + ")"
	}
	return "It has " + browser + " and " + runtime + "."
}

// memoryWords is n bytes as a person reads a box's memory.
func memoryWords(n int64) string {
	if n < gib {
		return fmt.Sprintf("%d MB", n>>20)
	}
	if n%gib == 0 {
		return fmt.Sprintf("%d GB", n/gib)
	}
	return fmt.Sprintf("%.1f GB", float64(n)/gib)
}

// boxChanges is what moved in the boxes a seat's runs use. A baseline older
// than [boxesFrom] told no box, so nothing is a change from it: "Your access"
// states the boxes.
func boxChanges(old, now Snapshot, v voice) []Change {
	if old.V < boxesFrom {
		return nil
	}
	before := map[string]Box{}
	for _, b := range old.Boxes {
		before[b.Cell] = b
	}
	var out []Change
	for _, is := range now.Boxes {
		was, had := before[is.Cell]
		delete(before, is.Cell)
		if !had {
			out = append(out, Change{Kind: ChangeBoxAdded, Vendor: boxVendor, Items: []string{is.Cell},
				Line: capitalized(v.your) + " runs can now use " + whereBox(is) + "."})
			continue
		}
		out = append(out, sameBoxChanges(was, is, v)...)
	}
	for _, cell := range Cells {
		if was, ok := before[cell]; ok {
			out = append(out, Change{Kind: ChangeBoxRemoved, Vendor: boxVendor, Items: []string{cell},
				Line: capitalized(v.your) + " runs no longer use " + whereBox(was) + "."})
		}
	}
	return out
}

// sameBoxChanges is what moved in one cell's box: each fact only where both
// sides know it, since a box not read yet is not one that lost its browser.
func sameBoxChanges(was, is Box, v voice) []Change {
	var out []Change
	box := capitalized(v.your) + " box"
	if was.MemoryBytes > 0 && is.MemoryBytes > 0 && was.MemoryBytes != is.MemoryBytes {
		from, to := memoryWords(was.MemoryBytes), memoryWords(is.MemoryBytes)
		out = append(out, Change{Kind: ChangeBoxChanged, Vendor: boxVendor,
			Items: []string{"out of memory", "OOM"}, From: from, To: to,
			Line: fmt.Sprintf("%s is now held to %s of memory (was %s).", box, to, from)})
	}
	if !was.Observed || !is.Observed {
		return out
	}
	if c, ok := toolChange(box, "a browser", "browser", was.Browser, is.Browser); ok {
		out = append(out, c)
	}
	if c, ok := toolChange(box, "a container runtime", "container runtime",
		was.ContainerRuntime, is.ContainerRuntime); ok {
		out = append(out, c)
	}
	return out
}

// toolChange is one image tool that came, went or was replaced in a box.
func toolChange(box, article, noun, from, to string) (Change, bool) {
	if from == to {
		return Change{}, false
	}
	items := slices.DeleteFunc([]string{noun, from, to}, func(s string) bool { return s == "" })
	c := Change{Kind: ChangeBoxChanged, Vendor: boxVendor, Items: items, From: from, To: to}
	switch {
	case from == "":
		c.Line = fmt.Sprintf("%s now has %s (%s).", box, article, to)
	case to == "":
		c.Line = fmt.Sprintf("%s no longer has %s.", box, article)
	default:
		c.Line = fmt.Sprintf("%s's %s is now %s (was %s).", box, noun, to, from)
	}
	return c, true
}

// OwesReview reports whether c may make work the seat raised under its earlier
// access wrong (ADR-0111): every change but a box's, whose size and tools
// change no question the seat asked.
func (c Change) OwesReview() bool { return !c.boxChange() }

// boxChange reports a change to the boxes the seat's runs use.
func (c Change) boxChange() bool {
	return c.Kind == ChangeBoxAdded || c.Kind == ChangeBoxRemoved || c.Kind == ChangeBoxChanged
}

// boxTerms is what a note naming a box change contains.
func (c Change) boxTerms() []string {
	terms := slices.Clone(c.Items)
	return slices.DeleteFunc(terms, func(s string) bool { return strings.TrimSpace(s) == "" })
}
