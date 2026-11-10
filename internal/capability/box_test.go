package capability_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/config"
)

// containerBox is a container box held to 4 GB whose image a run read.
func containerBox() capability.Box {
	return capability.Box{Cell: capability.CellContainer, MemoryBytes: 4 << 30, Observed: true,
		Browser: "chromium"}
}

// withBoxes is the SRE's input with boxes.
func withBoxes(boxes ...capability.Box) capability.Input {
	in := input(company())
	in.Boxes = boxes
	return in
}

// EACH TURN STATES THE BOX ITS RUNS USE BESIDE ITS ACCESS: where the box runs,
// the memory it is held to, and what a run found in its image, or that no run
// has read it yet. A seat with no integration and a box is still told its box.
//
// Mutation: drop the box line, tell an unread box's image as empty, or leave
// "Your access" out for a seat with a box and no integration, and this goes red.
func TestEachTurnStatesTheBoxItsRunsUse(t *testing.T) {
	t.Parallel()
	access := capability.YourAccess(capability.Take(withBoxes(containerBox())))
	want := "- Your box: a container on the engine host, held to 4 GB of memory. It has a browser " +
		"(chromium) and no container runtime, so containers do not run in it."
	if !strings.Contains(access, want) {
		t.Fatalf("Your access says\n%s\nwant the line\n%s", access, want)
	}

	unread := capability.Box{Cell: capability.CellE2B, MemoryBytes: 3 << 29, Browser: "chromium"}
	access = capability.YourAccess(capability.Take(withBoxes(unread)))
	want = "- Your box: a remote machine of its own, with 1.5 GB of memory. No run has read yet " +
		"whether it has a browser or a container runtime."
	if !strings.Contains(access, want) {
		t.Fatalf("an unread box is told as\n%s\nwant the line\n%s", access, want)
	}

	direct := capability.Box{Cell: capability.CellDirect, MemoryBytes: 512 << 20, Observed: true,
		ContainerRuntime: "podman"}
	alone := capability.Take(capability.Input{Company: &config.Company{Name: "Solo"}, Handle: "sre",
		Boxes: []capability.Box{direct}})
	want = capability.YourAccessHeading + "\n"
	if access = capability.YourAccess(alone); !strings.HasPrefix(access, want) || !strings.Contains(access,
		"- Your box: a directory on the engine host, run as the engine's own user, with 512 MB of memory. "+
			"It has no browser and a container runtime (podman).") {
		t.Fatalf("a seat with a box and no integration is told\n%s", access)
	}

	two := capability.YourAccess(capability.Take(withBoxes(containerBox(), unread)))
	if strings.Count(two, "- A box your runs can use, by the model they launch on: ") != 2 {
		t.Fatalf("a seat whose runs use two boxes is told\n%s", two)
	}
}

// A BOX CHANGE IS TOLD AS WHAT MOVED, and only between two facts both sides
// know: a box not read yet has not lost its browser, and a memory nobody said
// has not changed.
//
// Mutation: tell a fact one side does not know, or drop any of the lines, and
// this goes red.
func TestABoxChangeIsToldAsWhatMoved(t *testing.T) {
	t.Parallel()
	before := capability.Take(withBoxes(containerBox()))
	grown := containerBox()
	grown.MemoryBytes, grown.Browser, grown.ContainerRuntime = 8<<30, "", "docker"
	remote := capability.Box{Cell: capability.CellE2B}
	changes := capability.Diff(before, capability.Take(withBoxes(grown, remote)))
	var lines []string
	for _, ch := range changes {
		if !ch.Kind.Valid() || !ch.Access() {
			t.Errorf("box change %+v is not a told access change of the closed list", ch)
		}
		lines = append(lines, ch.Line)
	}
	want := []string{
		"Your box is now held to 8 GB of memory (was 4 GB).",
		"Your box no longer has a browser.",
		"Your box now has a container runtime (docker).",
		"Your runs can now use a remote machine of its own.",
	}
	if !slices.Equal(lines, want) {
		t.Fatalf("lines =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	gone := capability.Diff(capability.Take(withBoxes(grown, remote)), capability.Take(withBoxes(grown)))
	if len(gone) != 1 || gone[0].Kind != capability.ChangeBoxRemoved ||
		gone[0].Line != "Your runs no longer use a remote machine of its own." {
		t.Fatalf("a box the seat's runs no longer use is told as %+v", gone)
	}

	unread := containerBox()
	unread.Observed, unread.MemoryBytes = false, 0
	if got := capability.Diff(before, capability.Take(withBoxes(unread))); len(got) != 0 {
		t.Fatalf("a box not read yet is told as changes %+v", got)
	}
	person := capability.DiffFor(before, capability.Take(withBoxes(grown)), capability.ForPerson)
	if len(person) == 0 || person[0].Line != "Its box is now held to 8 GB of memory (was 4 GB)." {
		t.Fatalf("a person reads the seat's box change as %+v", person)
	}
}

// A BASELINE FROM BEFORE BOXES WERE TOLD IS NO BOX CHANGE: a seat told its
// access by an older build is told its box by "Your access", not as a change,
// so an upgrade asks no seat to review its access.
//
// Mutation: diff a version 1 baseline's absent boxes as removed or added, and
// this goes red.
func TestABaselineFromBeforeBoxesTellsNoBoxChange(t *testing.T) {
	t.Parallel()
	now := capability.Take(withBoxes(containerBox()))
	if now.V != capability.Version || now.V < 2 {
		t.Fatalf("a snapshot carries version %d", now.V)
	}
	var old map[string]any
	if err := json.Unmarshal(capability.Take(input(company())).JSON(), &old); err != nil {
		t.Fatal(err)
	}
	old["v"] = 1
	delete(old, "boxes")
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := capability.Parse(raw)
	if err != nil {
		t.Fatalf("a version 1 snapshot does not parse: %v", err)
	}
	if got := capability.Diff(baseline, now); len(got) != 0 {
		t.Fatalf("a version 1 baseline tells box changes %+v", got)
	}
	empty := capability.Take(input(company()))
	if got := capability.Diff(empty, now); len(got) != 1 || got[0].Kind != capability.ChangeBoxAdded {
		t.Fatalf("a box added after a version 2 baseline is told as %+v", got)
	}
}

// A NOTE ABOUT THE BOX IS MARKED BY THE BOX CHANGE THAT CONTRADICTS IT: the
// change's terms are the sandbox and what moved, so "no container runtime in
// the sandbox" is marked when a runtime arrives.
//
// Mutation: answer a box change no terms, or leave it out of Access, and this
// goes red.
func TestABoxChangeNamesTheTermsANoteUses(t *testing.T) {
	t.Parallel()
	was := containerBox()
	is := containerBox()
	is.ContainerRuntime = "docker"
	changes := capability.Diff(capability.Take(withBoxes(was)), capability.Take(withBoxes(is)))
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want the runtime's", changes)
	}
	terms := changes[0].Terms()
	for _, want := range []string{"sandbox", "container runtime", "docker"} {
		if !slices.Contains(terms, want) {
			t.Errorf("terms %q do not name %q", terms, want)
		}
	}
}

// A SNAPSHOT HOLDS ONE BOX PER CELL, of the cells boxes run in: a cell this
// build does not place a box in is not told, and an unread box keeps no image
// facts.
//
// Mutation: keep an unknown cell, two boxes of one cell, or an unread box's
// browser, and this goes red.
func TestASnapshotHoldsOneBoxPerKnownCell(t *testing.T) {
	t.Parallel()
	unread := capability.Box{Cell: capability.CellContainer, Browser: "firefox"}
	s := capability.Take(withBoxes(unread, containerBox(), capability.Box{Cell: "moon"}))
	if len(s.Boxes) != 1 || s.Boxes[0].Browser != "" || s.Boxes[0].Observed {
		t.Fatalf("boxes = %+v, want the first container box alone, its image unread", s.Boxes)
	}
}
