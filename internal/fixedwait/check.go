package fixedwait

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Rule is which of the gate's rules a [Finding] breaks.
type Rule string

const (
	// Uncovered refuses a counted reference no inventory entry covers: a
	// new fixed wait.
	Uncovered Rule = "uncovered"
	// OverFull refuses a function holding more counted references than its
	// entry records: a wait added beside the ones the inventory knows.
	OverFull Rule = "over-full entry"
	// StaleEntry refuses an entry recording more references than its
	// function holds, or naming a function or a file that is gone.
	StaleEntry Rule = "stale entry"
	// BadEntry refuses an entry the inventory cannot hold: no file or
	// function, no reference counted, a piece that is not one of the
	// series' (2 to [LastPiece]), or a second entry for the same function.
	BadEntry Rule = "malformed entry"
	// BadMarker refuses a marker of no kind the gate knows, a simulated one
	// that does not say what it stands in for, and a bench one outside a
	// function that takes a *testing.B.
	BadMarker Rule = "malformed marker"
	// StaleMarker refuses a marker with no counted reference on the one line
	// it covers, and a bubble-only declaration on a function that holds no
	// wait and calls no other bubble-only helper.
	StaleMarker Rule = "stale marker"
	// MisplacedBubble refuses a bubble-only declaration anywhere but a
	// test-code function's doc, and on a method with an exported name.
	MisplacedBubble Rule = "misplaced bubble declaration"
	// BubbleEscapes refuses a reference to a bubble-only helper outside a
	// bubble.
	BubbleEscapes Rule = "bubble-only helper outside a bubble"
)

// LastPiece is the number of the series' last pull request, in
// crewlet/crewlet#734's list: pieces 2 to 16 outside internal/engine, 17 to
// 22 inside it. An entry naming a piece past it names no pull request that
// will remove it. The series adds a piece only by planning it there, and
// raises this in the same diff.
const LastPiece = 22

// Finding is one refusal, at the position a person goes to fix it.
type Finding struct {
	Rule Rule
	// File is slash-separated from the walk's root; Line is 0 for an entry
	// whose file is gone.
	File string
	Line int
	// Func is the function the finding is about, where it is about one.
	Func   string
	Detail string
}

func (f Finding) String() string {
	var b strings.Builder
	b.WriteString(f.File)
	if f.Line > 0 {
		b.WriteString(":" + strconv.Itoa(f.Line))
	}
	b.WriteString(": ")
	if f.Func != "" {
		b.WriteString(f.Func + ": ")
	}
	b.WriteString(string(f.Rule) + ": " + f.Detail)
	return b.String()
}

func sortFindings(fs []Finding) {
	slices.SortStableFunc(fs, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(a.File, b.File),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Rule, b.Rule),
			cmp.Compare(a.Func, b.Func),
		)
	})
}

// Check holds tree's counted references to inventory and returns, sorted,
// the walk's own findings and every one of the inventory's: a reference no
// entry covers ([Uncovered], one finding per reference, so the list is the
// whole of what is new), a function holding more than its entry records
// ([OverFull]), an entry recording more than its function holds or naming
// one that is gone ([StaleEntry]), and an entry the inventory cannot hold
// ([BadEntry]).
func Check(tree *Tree, inventory []Entry) []Finding {
	out := slices.Clone(tree.Findings)

	type key struct{ file, fn string }
	entries := map[key]Entry{}
	for _, e := range inventory {
		k := key{e.File, e.Func}
		bad := func(detail string) {
			out = append(out, Finding{Rule: BadEntry, File: e.File, Func: e.Func, Detail: detail})
		}
		switch {
		case e.File == "" || e.Func == "":
			bad("an entry names the file and the top-level function its references are in")
			continue
		case e.Total() <= 0 || e.NegativeWindow < 0 || e.HeadStart < 0 ||
			e.PositiveDeadline < 0 || e.ClockStep < 0 || e.Exempt < 0:
			bad("an entry records how many references of each category its function " +
				"holds, at least one in all and none below zero")
			continue
		case e.Piece < 2 || e.Piece > LastPiece:
			bad(fmt.Sprintf("an entry names the piece of the series that removes it, "+
				"2 to %d in crewlet/crewlet#734's list (piece 1 is this gate)", LastPiece))
			continue
		}
		if _, dup := entries[k]; dup {
			bad("a second entry for the same function: one entry holds every " +
				"reference of a function, by category")
			continue
		}
		entries[k] = e
	}

	refs := map[key][]Ref{}
	var order []key
	for _, r := range tree.Refs {
		k := key{r.File, r.Func}
		if _, seen := refs[k]; !seen {
			order = append(order, k)
		}
		refs[k] = append(refs[k], r)
	}
	for _, k := range order {
		held := refs[k]
		e, ok := entries[k]
		switch {
		case !ok:
			for _, r := range held {
				out = append(out, Finding{
					Rule: Uncovered, File: r.File, Line: r.Line, Func: r.Func,
					Detail: "time." + r.Name,
				})
			}
		case len(held) > e.Total():
			lines := make([]string, len(held))
			for i, r := range held {
				lines[i] = strconv.Itoa(r.Line)
			}
			out = append(out, Finding{
				Rule: OverFull, File: k.file, Line: held[0].Line, Func: k.fn,
				Detail: fmt.Sprintf("holds %d counted references (lines %s) and its entry "+
					"records %d: a wait was added beside the ones the inventory knows, and "+
					"the inventory never grows — decide the new claim by a signal, or mark "+
					"the wait if it decides nothing", len(held), strings.Join(lines, ", "), e.Total()),
			})
		case len(held) < e.Total():
			out = append(out, staleEntry(e, len(held), held[0].Line))
		}
	}
	for k, e := range entries {
		if _, ok := refs[k]; ok {
			continue
		}
		decls, read := tree.Decls[k.file]
		switch {
		case !read:
			out = append(out, Finding{
				Rule: StaleEntry, File: e.File, Func: e.Func,
				Detail: "names a file that is gone (or is not test code): remove the entry",
			})
		case decls != nil && !decls[k.fn]:
			out = append(out, Finding{
				Rule: StaleEntry, File: e.File, Func: e.Func,
				Detail: "names a function its file no longer declares: remove the entry, " +
					"or rename it with the function",
			})
		default:
			out = append(out, staleEntry(e, 0, 0))
		}
	}
	sortFindings(out)
	return out
}

func staleEntry(e Entry, held, line int) Finding {
	return Finding{
		Rule: StaleEntry, File: e.File, Line: line, Func: e.Func,
		Detail: fmt.Sprintf("holds %d counted references and its entry records %d: "+
			"the piece that removed the others removes them from the entry in the same "+
			"diff, so the inventory only shrinks", held, e.Total()),
	}
}
