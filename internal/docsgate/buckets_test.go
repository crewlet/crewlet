package docsgate

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestEveryCoordinationBucketIsInEveryInventory holds the pages that list the
// coordination store's buckets to the buckets the store opens. A reader sizing
// an external broker's account, backing the store up, or asking where a record
// lives reads these inventories rather than the source, and nothing else
// notices a bucket one of them leaves out: the atlas's three buckets and the
// uploads bucket each shipped missing from at least one of them.
//
// The buckets are read from internal/coord/kv's own source, every constant
// named `…Suffix`, so a bucket added there is a bucket every inventory has to
// name; the fleet store's are the ones OpenFleet's body names, and the lease
// store holds the rest. Each inventory is held both ways: a bucket it leaves
// out, and a name it lists that the store does not open.
func TestEveryCoordinationBucketIsInEveryInventory(t *testing.T) {
	t.Parallel()
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		t.Fatal(err)
	}
	all, fleet := bucketSuffixes(t, filepath.Join(root, "internal", "coord", "kv"))

	t.Run("architecture.md's coordination KV table", func(t *testing.T) {
		t.Parallel()
		page := readPage(t, root, "docs/concepts/architecture.md")
		table := tableAfter(t, page, func(l string) bool {
			return strings.HasPrefix(l, "**The whole company") && strings.HasSuffix(l, "coordination KV.**")
		})
		want := map[string]bool{}
		for _, s := range all {
			want["crewlet"+s] = true
		}
		sameNames(t, "docs/concepts/architecture.md's coordination KV table", firstCells(table, crewletName), want,
			"add a row naming the bucket, what it holds and its age")
	})

	slots := map[string]bool{}
	for _, s := range all {
		slots[slotName(s)] = true
	}
	for suffix := range slotAliases {
		if !slices.Contains(all, suffix) {
			t.Errorf("slotAliases names %q, and internal/coord/kv opens no such bucket: drop the alias", suffix)
		}
	}

	t.Run("coordination.md's diagram of what the fleet shares", func(t *testing.T) {
		t.Parallel()
		section := sectionOf(t, readPage(t, root, "docs/concepts/coordination.md"), "## What the fleet shares")
		sameNames(t, "docs/concepts/coordination.md's diagram under \"What the fleet shares\"",
			diagramSlots(t, section), slots,
			"add a node to the COORD subgraph whose label opens with the slot's name")
	})
	t.Run("coordination.md's table of what the fleet shares", func(t *testing.T) {
		t.Parallel()
		section := sectionOf(t, readPage(t, root, "docs/concepts/coordination.md"), "## What the fleet shares")
		sameNames(t, "docs/concepts/coordination.md's slot table under \"What the fleet shares\"",
			firstCells(firstTable(t, section), slotCell), slots,
			"add a row naming the slot, what it answers and the page that documents it")
	})
	t.Run("coordination.md's retention table", func(t *testing.T) {
		t.Parallel()
		section := sectionOf(t, readPage(t, root, "docs/concepts/coordination.md"), "## Retention is a bucket's age")
		sameNames(t, "docs/concepts/coordination.md's table under \"Retention is a bucket's age\"",
			firstCells(firstTable(t, section), slotCell), slots,
			"add a row naming the slot, its age and what the age is sized from")
	})

	t.Run("deployment.md's count of the buckets an account holds", func(t *testing.T) {
		t.Parallel()
		prose := strings.Join(strings.Fields(strings.Join(readPage(t, root, "docs/guides/deployment.md"), " ")), " ")
		for _, claim := range []string{
			fmt.Sprintf("the %s `crewlet_*` KV buckets", spelled(len(all))),
			fmt.Sprintf("%s in the lease store", spelled(len(all)-len(fleet))),
			fmt.Sprintf("and %s in the fleet store", spelled(len(fleet))),
		} {
			if !strings.Contains(prose, claim) {
				t.Errorf("docs/guides/deployment.md does not say %q: internal/coord/kv opens %d buckets, "+
					"%d of them in the fleet store. An operator sizes an external broker's account from "+
					"that sentence, so correct its counts", claim, len(all), len(fleet))
			}
		}
	})
}

// slotAliases is the slot name coordination.md gives a bucket whose name the
// rest of the docs and the code do not use for it: the token counters are
// "budgets" everywhere a reader meets them, and the state log's register is
// "the positions register". Every other slot is its bucket's suffix with its
// underscores read as spaces.
var slotAliases = map[string]string{
	"_token_windows":      "budgets",
	"_statelog_positions": "positions",
}

func slotName(suffix string) string {
	if alias, ok := slotAliases[suffix]; ok {
		return alias
	}
	return strings.ReplaceAll(strings.TrimPrefix(suffix, "_"), "_", " ")
}

// bucketSuffixes is every bucket suffix declared in dir's source, sorted, and
// the ones OpenFleet's body names: the fleet store's.
func bucketSuffixes(t *testing.T, dir string) (all, fleet []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	declared := map[string]string{}
	var openFleet *ast.FuncDecl
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.Name == "OpenFleet" {
					openFleet = d
				}
			case *ast.GenDecl:
				if d.Tok != token.CONST {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, ident := range vs.Names {
						if !strings.HasSuffix(ident.Name, "Suffix") || i >= len(vs.Values) {
							continue
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							continue
						}
						value, err := strconv.Unquote(lit.Value)
						if err != nil || !strings.HasPrefix(value, "_") {
							continue
						}
						declared[ident.Name] = value
					}
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatalf("read no `…Suffix` bucket constant in %s: the gate is blind, so teach it where the "+
			"bucket names went", dir)
	}
	if openFleet == nil {
		t.Fatalf("found no OpenFleet in %s: the gate cannot tell the fleet store's buckets from the "+
			"lease store's, so teach it where the fleet store is opened", dir)
	}
	inFleet := map[string]bool{}
	ast.Inspect(openFleet.Body, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok {
			if value, ok := declared[ident.Name]; ok {
				inFleet[value] = true
			}
		}
		return true
	})
	for _, value := range declared {
		all = append(all, value)
		if inFleet[value] {
			fleet = append(fleet, value)
		}
	}
	slices.Sort(all)
	slices.Sort(fleet)
	if len(fleet) == 0 || len(fleet) == len(all) {
		t.Fatalf("OpenFleet names %d of the %d buckets: the gate expects the fleet store to hold some and "+
			"the lease store the rest, so teach it how the two stores are opened now", len(fleet), len(all))
	}
	return all, fleet
}

func readPage(t *testing.T, root, rel string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(raw), "\n")
}

// sectionOf is the lines under heading, up to the next heading of its level
// or above.
func sectionOf(t *testing.T, lines []string, heading string) []string {
	t.Helper()
	level := strings.Index(heading, " ")
	start := slices.Index(lines, heading)
	if start < 0 {
		t.Fatalf("no heading %q: the gate reads its inventory under it, so point the gate at where "+
			"the inventory moved", heading)
	}
	inFence := false
	for i := start + 1; i < len(lines); i++ {
		if fence(lines[i]) {
			inFence = !inFence
		}
		if inFence {
			continue
		}
		if hashes := len(lines[i]) - len(strings.TrimLeft(lines[i], "#")); hashes > 0 && hashes <= level &&
			strings.HasPrefix(lines[i][hashes:], " ") {
			return lines[start+1 : i]
		}
	}
	return lines[start+1:]
}

// tableAfter is the first table after the first line marker matches.
func tableAfter(t *testing.T, lines []string, marker func(string) bool) []string {
	t.Helper()
	start := slices.IndexFunc(lines, marker)
	if start < 0 {
		t.Fatal("no line opens the inventory: the gate reads the table after it, so point the gate " +
			"at where the inventory moved")
	}
	return firstTable(t, lines[start+1:])
}

// firstTable is the body rows of the first table in lines: its header and
// delimiter rows dropped.
func firstTable(t *testing.T, lines []string) []string {
	t.Helper()
	var rows []string
	for _, line := range lines {
		if strings.HasPrefix(line, "|") {
			rows = append(rows, line)
			continue
		}
		if len(rows) > 0 {
			break
		}
	}
	if len(rows) < 3 {
		t.Fatalf("found no table with a body: %d rows", len(rows))
	}
	return rows[2:]
}

var (
	crewletName = regexp.MustCompile("`(crewlet_[a-z_]+)`")
	slotCell    = regexp.MustCompile("`([a-z][a-z ]*)`")
	diagramNode = regexp.MustCompile(`\[\("([^<"]+)<br/>`)
)

// firstCells is every name pattern finds in each row's first cell.
func firstCells(rows []string, pattern *regexp.Regexp) map[string]bool {
	names := map[string]bool{}
	for _, row := range rows {
		cells := strings.SplitN(row, "|", 3)
		if len(cells) < 3 {
			continue
		}
		for _, m := range pattern.FindAllStringSubmatch(cells[1], -1) {
			names[m[1]] = true
		}
	}
	return names
}

// diagramSlots is the name each node of the coordination store's subgraph
// opens its label with.
func diagramSlots(t *testing.T, section []string) map[string]bool {
	t.Helper()
	start := slices.IndexFunc(section, func(l string) bool { return strings.Contains(l, "subgraph COORD") })
	if start < 0 {
		t.Fatal("no `subgraph COORD` in the section's diagram")
	}
	names := map[string]bool{}
	for _, line := range section[start+1:] {
		if strings.TrimSpace(line) == "end" {
			return names
		}
		for _, m := range diagramNode.FindAllStringSubmatch(line, -1) {
			names[m[1]] = true
		}
	}
	t.Fatal("the COORD subgraph has no `end`")
	return nil
}

func sameNames(t *testing.T, where string, got, want map[string]bool, fix string) {
	t.Helper()
	for _, name := range keys(want) {
		if !got[name] {
			t.Errorf("%s leaves out %q, which internal/coord/kv opens: %s", where, name, fix)
		}
	}
	for _, name := range keys(got) {
		if !want[name] {
			t.Errorf("%s names %q, and internal/coord/kv opens no such bucket: correct or drop it", where, name)
		}
	}
}

// spelled is n in words, as the docs write a count: "twenty-seven".
func spelled(n int) string {
	ones := []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
		"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}
	tens := []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}
	switch {
	case n < 20:
		return ones[n]
	case n < 100 && n%10 == 0:
		return tens[n/10]
	case n < 100:
		return tens[n/10] + "-" + ones[n%10]
	}
	return strconv.Itoa(n)
}
