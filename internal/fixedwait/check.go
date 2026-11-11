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
	// new fixed wait, or a new reading of elapsed time.
	Uncovered Rule = "uncovered"
	// OverFull refuses a function holding more fixed waits, or more
	// readings of elapsed time, than its entry records for that shape: one
	// added beside the ones the inventory knows.
	OverFull Rule = "over-full entry"
	// StaleEntry refuses an entry recording more fixed waits, or more
	// readings of elapsed time, than its function holds, or naming a
	// function or a file that is gone.
	StaleEntry Rule = "stale entry"
	// BadEntry refuses an entry the inventory cannot hold: no file or
	// function, no reference counted, a piece that is not one of the
	// series' (2 to [LastPiece]), or a second entry for the same function.
	BadEntry Rule = "malformed entry"
	// BadMarker refuses a marker of no kind the gate knows, a simulated one
	// that does not say what it stands in for, a measure one that does not
	// say why its reading decides nothing, a stated one that does not say
	// which instants or clock its comparison reads, a bench one outside a
	// function that takes a *testing.B, and a marker over only references it
	// does not cover: a measure or a stated over a fixed wait, a fixed wait's
	// kind over a reading of elapsed time, a stated over time.Since or
	// time.Until.
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
	// Untyped refuses a Sub, Before, After or Compare in a test-code file the
	// platform the gate runs on does not build, whose receiver's type it
	// therefore cannot read.
	Untyped Rule = "untyped receiver"
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
	// Refs are an over-full function's counted references, every one of
	// them: which is new the gate cannot say, since the inventory counts a
	// function's references rather than naming them, so the listing names
	// them all.
	Refs []Ref
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
// whole of what is new), a function holding more of a shape than its entry
// records for it ([OverFull]), an entry recording more of a shape than its
// function holds or naming one that is gone ([StaleEntry]), and an entry the
// inventory cannot hold ([BadEntry]). Each shape is held to its own count —
// the fixed waits to [Entry.Waits], the readings of elapsed time to
// [Entry.Readings] — never to the entry's total.
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
		case e.Total() <= 0 || e.NegativeWindow < 0 || e.HeadStart < 0 || e.PositiveDeadline < 0 ||
			e.ClockStep < 0 || e.ExemptWait < 0 || e.ElapsedBound < 0 || e.ExemptElapsed < 0:
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
		if !ok {
			for _, r := range held {
				out = append(out, Finding{
					Rule: Uncovered, File: r.File, Line: r.Line, Func: r.Func,
					Detail: "time." + r.Name,
				})
			}
			continue
		}
		// EACH SHAPE TO ITS OWN COUNT: a reading added where a removed wait
		// was is a new reading, and the other way round, though the
		// function's total is unchanged.
		var waits, readings []Ref
		for _, r := range held {
			if r.Elapsed() {
				readings = append(readings, r)
			} else {
				waits = append(waits, r)
			}
		}
		out = append(out, matchShape(k.file, k.fn, "fixed waits", waits, e.Waits(), held[0].Line)...)
		out = append(out, matchShape(k.file, k.fn, "readings of elapsed time", readings, e.Readings(), held[0].Line)...)
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

// matchShape holds a function's references of one shape, what names them,
// to the count its entry records for that shape: more is [OverFull], with
// every one of them listed, and fewer is [StaleEntry], at line, the
// function's first counted reference of either shape.
func matchShape(file, fn, what string, held []Ref, recorded, line int) []Finding {
	switch {
	case len(held) > recorded:
		lines := make([]string, len(held))
		for i, r := range held {
			lines[i] = strconv.Itoa(r.Line)
		}
		return []Finding{{
			Rule: OverFull, File: file, Line: held[0].Line, Func: fn,
			Detail: fmt.Sprintf("holds %d %s (lines %s) and its entry records %d: one was "+
				"added beside the ones the inventory knows, and the inventory never grows — "+
				"decide the new claim by a signal, or mark the reference if it decides nothing",
				len(held), what, strings.Join(lines, ", "), recorded),
			Refs: held,
		}}
	case len(held) < recorded:
		return []Finding{{
			Rule: StaleEntry, File: file, Line: line, Func: fn,
			Detail: fmt.Sprintf("holds %d %s and its entry records %d: the piece that "+
				"removed the others removes them from the entry in the same diff, so the "+
				"inventory only shrinks", len(held), what, recorded),
		}}
	}
	return nil
}

func staleEntry(e Entry, held, line int) Finding {
	return Finding{
		Rule: StaleEntry, File: e.File, Line: line, Func: e.Func,
		Detail: fmt.Sprintf("holds %d counted references and its entry records %d: "+
			"the piece that removed the others removes them from the entry in the same "+
			"diff, so the inventory only shrinks", held, e.Total()),
	}
}
