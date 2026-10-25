package cache

import (
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// A DECLARATION THAT LEAVES ANYTHING UNSAID IS REFUSED, naming the
// declaration and the field: every reader of a derived file reads its
// declaration and nothing beside it, so a field left empty is a question
// some reader answers for itself — the sweep trashing a file nothing said it
// owned, a write with no deadline, a pattern matching part of a name. Each
// row breaks one field of a valid declaration. And what holds only across
// declarations: one name a declaration, one maintainer write a step (the
// refusal counter and gauge are keyed by step), every file under objects/,
// and every part a damage report drops some declaration's.
//
// Mutation: make Validate skip the anchoring check (anchored) — the
// unanchored rows pass.
func TestValidateRefusesAnUnsaidDeclaration(t *testing.T) {
	t.Parallel()
	midx := func() Derived { return DerivedFiles()[0] }
	anchoredLiteral := regexp.MustCompile(`^multi-pack-index-x$`)
	cases := []struct {
		name   string
		mutate func(d *Derived)
		field  string
	}{
		{"no such name", func(d *Derived) { d.Name = "nonesuch" }, "Name"},
		{"no inputs", func(d *Derived) { d.Inputs = nil }, "Inputs"},
		{"an input of no kind", func(d *Derived) { d.Inputs = []Input{"tree"} }, "Inputs"},
		{"no writes", func(d *Derived) { d.Writes = nil }, "Writes"},
		{"a step of no kind", func(d *Derived) { d.Writes[0].Step = "split" }, "Writes[0].Step"},
		{"a trigger of no kind", func(d *Derived) { d.Writes[0].Trigger = "read" }, "Writes[0].Trigger"},
		{"a lane of no kind", func(d *Derived) { d.Writes[0].Lane = "fast" }, "Writes[0].Lane"},
		{"no deadline", func(d *Derived) { d.Writes[0].Deadline = nil }, "Writes[0].Deadline"},
		{"no memory", func(d *Derived) { d.Writes[1].BytesPerObject = 0 }, "Writes[1].BytesPerObject"},
		{"no arguments", func(d *Derived) { d.Writes[0].Args = nil }, "Writes[0].Args"},
		{"another subcommand", func(d *Derived) { d.Writes[0].Args = []string{"repack", "-a"} }, "Writes[0].Args"},
		{"a writer outside internal/code", func(d *Derived) { d.Writes[0].Writer.File = "cmd/crewlet/x.go" },
			"Writes[0].Writer.File"},
		{"a writer that is no Go file", func(d *Derived) { d.Writes[0].Writer.File = "internal/code/cache/x.txt" },
			"Writes[0].Writer.File"},
		{"a writer's path not clean", func(d *Derived) {
			d.Writes[0].Writer.File = "internal/code/cache/../host/x.go"
		}, "Writes[0].Writer.File"},
		{"two writes of one trigger and step", func(d *Derived) { d.Writes[2] = d.Writes[1] }, "Writes[2]"},
		{"no owned files", func(d *Derived) { d.Owns = nil }, "Owns"},
		{"an absolute directory", func(d *Derived) { d.Owns[0].Dir = "/objects/pack" }, "Owns[0]"},
		{"a directory outside", func(d *Derived) { d.Owns[0].Dir = "../objects/pack" }, "Owns[0]"},
		{"a directory not clean", func(d *Derived) { d.Owns[0].Dir = "objects/./pack" }, "Owns[0]"},
		{"no name pattern", func(d *Derived) { d.Owns[1].Name = nil }, "Owns[1]"},
		{"an unanchored name", func(d *Derived) {
			d.Owns[1].Name = regexp.MustCompile(`multi-pack-index-[0-9a-f]{40}\.bitmap`)
		}, "Owns[1]"},
		{"anchors across an alternation", func(d *Derived) {
			d.Owns[1].Name = regexp.MustCompile(`^multi-pack-index|bitmap$`)
		}, "Owns[1]"},
		{"an unanchored file held while a write runs", func(d *Derived) {
			d.Writes[1].Holds[0].Name = regexp.MustCompile(`multi-pack-index\.lock`)
		}, "Writes[1].Holds[0]"},
		{"a file held outside the incarnation", func(d *Derived) {
			d.Writes[0].Holds[0].Dir = "../objects/pack"
		}, "Writes[0].Holds[0]"},
		{"an owned file counted as nothing", func(d *Derived) { d.Owns[1].File = "" }, "Owns[1].File"},
		{"a main file of many names", func(d *Derived) {
			d.Owns[0].Name = regexp.MustCompile(`^multi-pack-index.*$`)
		}, "Owns[0].Name"},
		{"no main file", func(d *Derived) { d.Owns[0].Main = false }, "Owns"},
		{"two main files", func(d *Derived) {
			d.Owns = append(d.Owns, Owned{FilePattern: FilePattern{"objects/pack", anchoredLiteral}, File: "x",
				Main: true})
		}, "Owns"},
		{"a checksum in a name with no group", func(d *Derived) {
			d.Owns[2].Name = regexp.MustCompile(`^multi-pack-index-[0-9a-f]{40}\.rev$`)
		}, "Owns[2].Name"},
		{"a check of no kind", func(d *Derived) { d.Check = "none" }, "Check"},
		{"no part", func(d *Derived) { d.Part = 0 }, "Part"},
		{"a part no damage drops", func(d *Derived) { d.Part = gitproc.PartPack }, "Part"},
		{"a part that drops two files", func(d *Derived) { d.Part = gitproc.PartChunk }, "Part"},
		{"no freshness", func(d *Derived) { d.Freshness = "" }, "Freshness"},
		{"nothing said of a pack added", func(d *Derived) { d.OnPackAdded = "" }, "On(pack-added)"},
		{"a report met with no change", func(d *Derived) { d.OnReported = "maybe" }, "On(reported)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := midx()
			if err := d.Validate(); err != nil {
				t.Fatalf("the declaration before the change: %v", err)
			}
			tc.mutate(&d)
			err := d.Validate()
			if err == nil {
				t.Fatalf("Validate passed a declaration with %s", tc.name)
			}
			if name := `"` + string(d.Name) + `"`; !strings.Contains(err.Error(), name) ||
				!strings.Contains(err.Error(), tc.field) {
				t.Errorf("Validate answered %q: want it to name the declaration %s and the field %s", err, name,
					tc.field)
			}
		})
	}

	t.Run("across declarations", func(t *testing.T) {
		t.Parallel()
		if err := validateAll(DerivedFiles()); err != nil {
			t.Fatalf("the build's declarations: %v", err)
		}
		set := func(change func(midx, graph *Derived)) []Derived {
			decls := DerivedFiles()
			change(&decls[0], &decls[1])
			return decls
		}
		for _, tc := range []struct {
			name  string
			decls []Derived
			says  string
		}{
			{"one name twice", []Derived{DerivedFiles()[0], DerivedFiles()[0], DerivedFiles()[1]}, "declared twice"},
			{"two maintainer writes of one step", set(func(_, graph *Derived) {
				graph.Writes[0].Step = StepIndex
			}), "each declare a maintainer write of step index"},
			{"an owned file under refs/", set(func(_, graph *Derived) {
				graph.Owns[0].Dir = "refs/heads"
			}), "lives under objects/"},
			{"a file held under the root's refs/", set(func(midx, _ *Derived) {
				midx.Writes[1].Holds[0].Dir = "refs"
			}), "lives under objects/"},
			{"a damage no declaration answers", DerivedFiles()[:1], "no derived file is declared by"},
		} {
			err := validateAll(tc.decls)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("%s: validateAll answered %v, want one saying %q", tc.name, err, tc.says)
			}
			if _, err := newImage(tc.decls); err == nil {
				t.Errorf("%s: newImage built an image over declarations validateAll refuses", tc.name)
			}
		}
	})

	t.Run("what the plan takes", func(t *testing.T) {
		t.Parallel()
		decls := DerivedFiles()
		decls[1].Writes[0].Trigger = TriggerRound
		im, err := newImage(decls)
		if err != nil {
			t.Fatalf("declarations with the graph written by a round: %v", err)
		}
		if _, err := newMaintWrites(im); err == nil {
			t.Error("newMaintWrites took declarations with no maintainer write of the commit-graph: the plan " +
				"writes one")
		}
	})
}

// A FAILED WRITE CLEARS WHAT IT HOLDS, A LOCK BY NAME AND A DIRECTORY LISTED
// AT MOST ONCE. Each write declares what it holds while it runs (Write.Holds),
// so a failure clears exactly that: the round's index holds its lock alone,
// which is removed by name with no listing — objects/pack holds three files a
// pack, and a failed round's index was a listing of it on the request the
// round answers — and a maintainer's run over the index with its bitmap and
// the graph removes both locks by name and lists objects/pack ONCE for the
// bitmap's temporary file, the one pattern that is no literal.
//
// Mutations: clear every held pattern by listing (the round's write lists
// objects/pack); list a directory a pattern (two patterns in one directory
// list it twice).
func TestAFailedWriteClearsWhatItHoldsListingEachDirectoryOnce(t *testing.T) {
	t.Parallel()
	var round, bitmap, plain, graph Write
	for _, d := range DerivedFiles() {
		for _, w := range d.Writes {
			switch {
			case w.Trigger == TriggerRound:
				round = w
			case w.Step == StepBitmap:
				bitmap = w
			case w.Step == StepIndex:
				plain = w
			case w.Step == StepGraph:
				graph = w
			}
		}
	}
	lock := "objects/pack/multi-pack-index.lock"
	graphLock := "objects/info/commit-graph.lock"
	for _, tc := range []struct {
		name   string
		writes []Write
		named  []string
		listed map[string][]string
	}{
		{"the round's index", []Write{round}, []string{lock}, map[string][]string{}},
		{"the maintainer's index without a bitmap", []Write{plain}, []string{lock}, map[string][]string{}},
		{"the graph and the index with its bitmap", []Write{graph, bitmap}, []string{graphLock, lock},
			map[string][]string{"objects/pack": {tmpBitmap.String()}}},
		{"a write held twice", []Write{bitmap, bitmap}, []string{lock},
			map[string][]string{"objects/pack": {tmpBitmap.String()}}},
	} {
		named, listed := clearPlan(tc.writes)
		var got []string
		for _, rel := range named {
			got = append(got, filepath.ToSlash(rel))
		}
		if !slices.Equal(got, tc.named) {
			t.Errorf("%s: removes by name %v, want %v", tc.name, got, tc.named)
		}
		gotListed := map[string][]string{}
		for rel, patterns := range listed {
			for _, re := range patterns {
				gotListed[filepath.ToSlash(rel)] = append(gotListed[filepath.ToSlash(rel)], re.String())
			}
		}
		if !maps.EqualFunc(gotListed, tc.listed, slices.Equal) {
			t.Errorf("%s: lists %v, want %v", tc.name, gotListed, tc.listed)
		}
	}
}
