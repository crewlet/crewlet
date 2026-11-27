package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestEveryRunnerDeclaresAPeakUpToTheKeptSession holds each runner's
// declaration to what the node budget reads off it: a measured release with
// points from a run that resumes nothing to the largest session a take keeps,
// on the grid a stand-in's curve is taken across, or a stand-in saying why.
func TestEveryRunnerDeclaresAPeakUpToTheKeptSession(t *testing.T) {
	t.Parallel()
	grid := measuredPeak("", 0, 0, 0, 0, 0).Points
	for _, h := range Registered() {
		p := h.ResidentPeak()
		if p.StandIn != "" {
			if p.Release != "" || p.Points != nil {
				t.Errorf("%s: a stand-in declares a release %q or points %v of its own", h.Name(), p.Release, p.Points)
			}
			continue
		}
		if p.Release == "" {
			t.Errorf("%s: measured points name no release", h.Name())
		}
		if len(p.Points) != len(grid) {
			t.Fatalf("%s: %d points, want the %d of the measuring grid", h.Name(), len(p.Points), len(grid))
		}
		for i, pt := range p.Points {
			if pt.Session != grid[i].Session || pt.Resident <= 0 {
				t.Errorf("%s: point %d is %+v, want a positive peak at %d bytes of session", h.Name(), i, pt, grid[i].Session)
			}
		}
		if last := p.Points[len(p.Points)-1].Session; last != executorkeep.SessionPlainMaxBytes {
			t.Errorf("%s: its points end at %d bytes, short of the kept bound %d", h.Name(), last,
				executorkeep.SessionPlainMaxBytes)
		}
	}
}

// TestAStandInIsNeverLighterThanAMeasuredHarness: a harness nobody measured
// is reckoned at least as heavy as each measured one, at every session size.
func TestAStandInIsNeverLighterThanAMeasuredHarness(t *testing.T) {
	t.Parallel()
	var standIns, measured []*Runner
	for _, h := range Registered() {
		if h.ResidentPeak().StandIn != "" {
			standIns = append(standIns, New(h))
		} else {
			measured = append(measured, New(h))
		}
	}
	if len(standIns) == 0 || len(measured) == 0 {
		t.Fatalf("%d stand-ins and %d measured runners: the case holds nothing", len(standIns), len(measured))
	}
	for _, s := range standIns {
		got := s.ResidentPeak()
		for _, m := range measured {
			for _, at := range []int64{0, 1 << 20, 40 << 20, 100 << 20, 200 << 20, executorkeep.SessionPlainMaxBytes} {
				if g, w := got.At(at), m.ResidentPeak().At(at); g < w {
					t.Errorf("stand-in %s holds %d MiB at %d MiB of session, lighter than %s's %d MiB",
						s.harness.Name(), g>>20, at>>20, m.harness.Name(), w>>20)
				}
			}
		}
	}
}

// TestAPeakNeverFallsAsTheSessionGrows: the curve read between and at the
// points is their upper envelope, so a measurement that came out lower at a
// larger session than at a smaller one never lowers the figure.
func TestAPeakNeverFallsAsTheSessionGrows(t *testing.T) {
	t.Parallel()
	p := ResidentPeak{Points: []PeakPoint{{0, 100}, {10, 300}, {20, 200}, {40, 500}}}
	for _, c := range []struct{ session, want int64 }{
		{0, 100}, {5, 200}, {10, 300}, {15, 300}, {20, 300}, {30, 400}, {40, 500}, {60, 700},
	} {
		if got := p.At(c.session); got != c.want {
			t.Errorf("At(%d) = %d, want %d", c.session, got, c.want)
		}
	}
	for _, h := range Registered() {
		r := New(h).ResidentPeak()
		last := r.Base()
		for at := int64(0); at <= executorkeep.SessionPlainMaxBytes; at += 4 << 20 {
			got := r.At(at)
			if got < last {
				t.Errorf("%s: %d MiB at %d MiB of session, below %d MiB at a smaller one", h.Name(), got>>20, at>>20, last>>20)
			}
			last = got
		}
		if r.Most() != r.At(executorkeep.SessionPlainMaxBytes) || r.Most() < r.Base() {
			t.Errorf("%s: most %d below base %d", h.Name(), r.Most(), r.Base())
		}
	}
}

// TestTheConceptPageListsEachRunnersDeclaredMemory: code-sandbox.md's table is
// what a person sizes a node by, so each row is the runner's declaration, its
// release (or "stand-in") and its MiB at each measured session.
//
// Mutation: change one declared point, or mark a measured runner a stand-in,
// and it goes red.
func TestTheConceptPageListsEachRunnersDeclaredMemory(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), "docs", "concepts", "code-sandbox.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := memoryRows(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Names() {
		p := New(harnessNamed(name)).ResidentPeak()
		want := []string{fmt.Sprintf("[%s](#%s)", runnerSections[name], anchorOf(runnerSections[name])), p.Release}
		if p.StandIn != "" {
			want[1] = "stand-in"
		}
		for _, pt := range p.Points {
			want = append(want, strconv.FormatInt(pt.Resident>>20, 10))
		}
		got, ok := rows[runnerSections[name]]
		if !ok {
			t.Errorf("%s has no row in the page's memory table", name)
			continue
		}
		if strings.Join(got, " | ") != strings.Join(want, " | ") {
			t.Errorf("%s's row is\n\t%q\nwant\n\t%q", name, got, want)
		}
	}
	if len(rows) != len(Names()) {
		t.Errorf("the memory table has %d rows for %d runners", len(rows), len(Names()))
	}
}

// memoryRows is the table under "### Each runner's memory", each row's
// cells by the runner's section title, thousands separators dropped.
func memoryRows(page string) (map[string][]string, error) {
	_, rest, ok := strings.Cut(page, "\n### Each runner's memory\n")
	if !ok {
		return nil, fmt.Errorf("code-sandbox.md has no \"Each runner's memory\" section")
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(rest, "\n") {
		if strings.HasPrefix(line, "#") {
			break
		}
		if !strings.HasPrefix(line, "| [") {
			continue
		}
		var cells []string
		for _, c := range strings.Split(strings.Trim(line, "|"), "|") {
			cells = append(cells, strings.ReplaceAll(strings.TrimSpace(c), ",", ""))
		}
		title, _, _ := strings.Cut(strings.TrimPrefix(cells[0], "["), "]")
		rows[title] = cells
	}
	return rows, nil
}

// anchorOf is the anchor a heading of title renders, as the page's links
// write it.
func anchorOf(title string) string {
	return strings.ReplaceAll(strings.ToLower(title), " ", "-")
}
