package cache_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE CONCEPT PAGE NAMES EVERY DERIVED FILE (ADR-0086): docs/concepts/gitlet.md's
// table "What the cache writes beside the packs" has exactly one row per
// cache.DerivedFiles declaration, each spelling its owned files, its writes'
// triggers and steps in declared order, and what a compaction's retirement
// (OnPackRetired), an expunge's commit (OnPackExpunged) and any other pack
// leaving (OnPackRemoved) does to it.
//
// Mutation: add a row for `tip`, and the page names an undeclared file; drop
// `.bitmap` from midx's Owns, and the row's files differ; declare the graph
// dropped on a retirement, keep the installed chain whole when a pack it
// names leaves, or keep the memo on an expunge, and its row disagrees.
func TestTheConceptPageNamesEveryDerivedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(sourcetree.Root(t), "docs", "concepts", "gitlet.md")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := derivedTable(t, string(raw))

	decls := cache.DerivedFiles()
	for name := range rows {
		if !slices.ContainsFunc(decls, func(d cache.Derived) bool { return string(d.Name) == name }) {
			t.Errorf("%s: the page names a derived file the cache does not declare: %q", path, name)
		}
	}
	for _, d := range decls {
		row, ok := rows[string(d.Name)]
		if !ok {
			t.Errorf("%s: the table names no row for the declared derived file %q", path, d.Name)
			continue
		}
		if want := derivedFilesCell(t, d); row[1] != want {
			t.Errorf("%s: %s's files are %q, want what its declaration owns, %q", path, d.Name, row[1], want)
		}
		if want := derivedWritesCell(t, d); row[2] != want {
			t.Errorf("%s: %s is written by %q, want its declared writes, %q", path, d.Name, row[2], want)
		}
		if want := packLeavingCell(t, d, cache.EventPackRetired, d.OnPackRetired); row[3] != want {
			t.Errorf("%s: when a compaction retires a pack, %s is %q, want %q", path, d.Name, row[3], want)
		}
		if want := packLeavingCell(t, d, cache.EventPackExpunged, d.OnPackExpunged); row[4] != want {
			t.Errorf("%s: when an expunge drops a pack, %s is %q, want %q", path, d.Name, row[4], want)
		}
		if want := packLeavingCell(t, d, cache.EventPackRemoved, d.OnPackRemoved); row[5] != want {
			t.Errorf("%s: when any other pack leaves, %s is %q, want %q", path, d.Name, row[5], want)
		}
	}
}

// derivedTable is the trimmed six cells of each row of page's table "What the
// cache writes beside the packs", keyed by the name its first cell opens with.
func derivedTable(t *testing.T, page string) map[string][6]string {
	t.Helper()
	const title = "**What the cache writes beside the packs.**"
	start := strings.Index(page, title)
	if start < 0 {
		t.Fatalf("the concept page has no paragraph %s", title)
	}
	lead := regexp.MustCompile("^`([^`]+)`")
	rows := map[string][6]string{}
	inTable := false
	for _, line := range strings.Split(page[start:], "\n") {
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		if !inTable {
			inTable = true
			continue // the header
		}
		if strings.HasPrefix(line, "|---") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 6 {
			t.Fatalf("the table's row %q has %d cells, want 6", line, len(cells))
		}
		var row [6]string
		for i, c := range cells {
			row[i] = strings.TrimSpace(c)
		}
		m := lead.FindStringSubmatch(row[0])
		if m == nil {
			t.Fatalf("the table's row %q does not open with its declaration's name in backquotes", line)
		}
		if _, dup := rows[m[1]]; dup {
			t.Errorf("the table names %q twice", m[1])
		}
		rows[m[1]] = row
	}
	if !inTable {
		t.Fatalf("the concept page's paragraph %s is followed by no table", title)
	}
	return rows
}

// derivedFilesCell is the files cell d's Owns spell, each pattern read as a
// path in the incarnation.
func derivedFilesCell(t *testing.T, d cache.Derived) string {
	t.Helper()
	var out []string
	for _, o := range d.Owns {
		out = append(out, "`"+o.Dir+"/"+readPattern(t, o.Name)+"`")
	}
	return strings.Join(out, ", ")
}

// readPattern reads an owned file's pattern as an operator would: literal but
// for a 40-hex checksum (<checksum>) and a number from one (<n>). Any other
// spelling fails the case rather than guess at the page's words.
func readPattern(t *testing.T, re *regexp.Regexp) string {
	t.Helper()
	s := strings.TrimSuffix(strings.TrimPrefix(re.String(), "^"), "$")
	s = strings.ReplaceAll(s, "([0-9a-f]{40})", "<checksum>")
	s = strings.ReplaceAll(s, "[0-9a-f]{40}", "<checksum>")
	s = strings.ReplaceAll(s, "[1-9][0-9]*", "<n>")
	s = strings.ReplaceAll(s, `\.`, ".")
	if strings.ContainsAny(s, `\[](){}*+?|^$`) {
		t.Fatalf("the owned pattern %s reads as %q: teach readPattern its spelling", re, s)
	}
	return s
}

// derivedTriggers is how the page names what starts a write.
var derivedTriggers = map[cache.Trigger]string{
	cache.TriggerRound:      "a sync, before it answers",
	cache.TriggerMaintainer: "the background indexing",
	cache.TriggerBackfill:   "the backfill",
	cache.TriggerIndexer:    "the indexer of code indexes",
	cache.TriggerRead:       "a seat's read by object id",
	cache.TriggerInstall:    "the install of the maintainer's published layers",
}

// derivedWritesCell is the writes cell d's Writes spell: each trigger in the
// order of its first declared write, with its steps in declared order.
func derivedWritesCell(t *testing.T, d cache.Derived) string {
	t.Helper()
	var order []cache.Trigger
	steps := map[cache.Trigger][]string{}
	for _, w := range d.Writes {
		if _, ok := derivedTriggers[w.Trigger]; !ok {
			t.Fatalf("%s writes on %q, which the page has no words for: add them to derivedTriggers", d.Name,
				w.Trigger)
		}
		if !slices.Contains(order, w.Trigger) {
			order = append(order, w.Trigger)
		}
		steps[w.Trigger] = append(steps[w.Trigger], "`"+string(w.Step)+"`")
	}
	var out []string
	for _, tr := range order {
		out = append(out, fmt.Sprintf("%s: %s", derivedTriggers[tr], strings.Join(steps[tr], ", ")))
	}
	return strings.Join(out, "; ")
}

// packLeavingCell is the page's words for does, d's answer to a pack leaving
// by ev. An expunge's commit takes out packs no layer names, so its Prefix
// keeps every layer and drops only the node's tip (tipGoes), from the
// declaration the tip is a write of.
func packLeavingCell(t *testing.T, d cache.Derived, ev cache.Event, does cache.Change) string {
	t.Helper()
	switch {
	case does == cache.Drop:
		return "dropped first"
	case does == cache.Keep:
		return "kept"
	case does == cache.Prefix && ev == cache.EventPackExpunged &&
		slices.ContainsFunc(d.Writes, func(w cache.Write) bool { return w.Step == cache.StepTipGraph }):
		return "kept, but for the node's tip, dropped first"
	case does == cache.Prefix && ev == cache.EventPackExpunged:
		return "kept, no layer naming a pack an expunge drops"
	case does == cache.Prefix:
		return "cut first, below the layer that names it"
	}
	t.Fatalf("%s's answer to %s is %q, which the page has no words for", d.Name, ev, does)
	return ""
}
