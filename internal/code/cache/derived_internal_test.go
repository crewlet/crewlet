package cache

import (
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

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
		{"a trigger of no kind", func(d *Derived) { d.Writes[0].Trigger = "push" }, "Writes[0].Trigger"},
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

// AN IN-PROCESS DECLARATION — the cache's own code writes its files — SAYS
// WHAT IT HOLDS AND CLAIMS NOTHING A GIT WRITE HAS: no lane, deadline, charge
// an object or argument list, which nothing would read, but a Charge
// sentence; where no git reads its files either (the index of pushes), nothing
// done on a report, since no report can name such a file; at most one main
// file, its presence otherwise its record's; and every file of one no git
// reads under crewlet/, never objects/, the namespace a push's scratch names
// as its alternate. And a git write beside its in-process ones is held to what
// git writing a declaration's files asks of it — a file whose presence is its
// own, and a part a damage report names it by — so no git process writes a
// file no git process reads. Each row breaks one of those on the provenance
// index's valid declaration.
//
// Mutation: let an in-process write name a lane (drop validateInProcess's lane
// case) — the "a lane" row passes.
func TestValidateRefusesAnInProcessDeclarationThatClaimsAGitWrite(t *testing.T) {
	t.Parallel()
	provenance := func() Derived {
		for _, d := range DerivedFiles() {
			if d.Name == DerivedProvenance {
				return d
			}
		}
		t.Fatal("the build declares no index of pushes")
		return Derived{}
	}
	for _, tc := range []struct {
		name   string
		mutate func(d *Derived)
		field  string
	}{
		{"a lane", func(d *Derived) { d.Writes[0].Lane = gitproc.ClassMaintenance }, "Writes[0].Lane"},
		{"a deadline", func(d *Derived) { d.Writes[0].Deadline = func(Covers) time.Duration { return 0 } },
			"Writes[0].Deadline"},
		{"a charge an object", func(d *Derived) { d.Writes[1].BytesPerObject = 8 }, "Writes[1].BytesPerObject"},
		{"an argument list", func(d *Derived) { d.Writes[0].Args = []string{"x"} }, "Writes[0].Args"},
		{"no charge said", func(d *Derived) { d.Writes[1].Charge = " " }, "Writes[1].Charge"},
		{"a git write of files no git reads", func(d *Derived) { d.Writes[1] = DerivedFiles()[0].Writes[0] },
			"Owns"},
		{"a drop on a report", func(d *Derived) { d.OnReported = Drop }, "OnReported"},
		{"two main files", func(d *Derived) {
			for _, name := range []string{"first", "second"} {
				d.Owns = append(d.Owns, Owned{FilePattern: FilePattern{engineRoot, regexp.MustCompile("^" + name + "$")},
					File: name, Main: true})
			}
		}, "Owns"},
		{"nothing said of a log gone back", func(d *Derived) { d.OnHistoryGone = "" }, "On(history-gone)"},
		{"nothing said of an expunge", func(d *Derived) { d.OnPackExpunged = "" }, "On(pack-expunged)"},
		{"no seed rule", func(d *Derived) { d.Seed = "" }, "Seed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := provenance()
			if err := d.Validate(); err != nil {
				t.Fatalf("the declaration before the change: %v", err)
			}
			tc.mutate(&d)
			err := d.Validate()
			if err == nil {
				t.Fatalf("Validate passed an in-process declaration with %s", tc.name)
			}
			if !strings.Contains(err.Error(), `"provenance"`) || !strings.Contains(err.Error(), tc.field) {
				t.Errorf("Validate answered %q: want it to name the declaration and the field %s", err, tc.field)
			}
		})
	}

	t.Run("roots", func(t *testing.T) {
		t.Parallel()
		set := func(change func(midx, provenance *Derived)) []Derived {
			decls := DerivedFiles()
			var midx, prov *Derived
			for i := range decls {
				switch decls[i].Name {
				case DerivedMidx:
					midx = &decls[i]
				case DerivedProvenance:
					prov = &decls[i]
				}
			}
			change(midx, prov)
			return decls
		}
		for _, tc := range []struct {
			name  string
			decls []Derived
		}{
			{"the index of pushes under objects/", set(func(_, p *Derived) { p.Owns[0].Dir = "objects/info" })},
			{"the index of pushes under refs/", set(func(_, p *Derived) { p.Owns[0].Dir = "refs/crewlet" })},
			{"git's index under crewlet/", set(func(m *Derived, _ *Derived) { m.Owns[1].Dir = engineRoot })},
			// The cache's own write of files git READS lives where git
			// looks: a part a report names, under objects/.
			{"the index of pushes read by git", set(func(_, p *Derived) { p.Part = gitproc.PartCommitGraph })},
		} {
			err := validateAll(tc.decls)
			if err == nil || !strings.Contains(err.Error(), "one no git process reads under crewlet/") {
				t.Errorf("%s: validateAll answered %v, want the root rule's refusal", tc.name, err)
			}
		}
		want := []string{"objects", gitproc.DroppingDir, engineRoot}
		if got := derivedRoots(DerivedFiles()); !slices.Equal(got, want) {
			t.Errorf("the roots a boot walks are %v, want %v: the walk reads the packs' two places and what the "+
				"declarations own", got, want)
		}
	})
}

// WHO READS A DECLARATION'S FILES IS ITS PART'S TO SAY, never who writes them
// (§6.0's restatement of P2's rule): the installed chain is the cache's own
// write of files every git process reads, so a declaration whose every write
// is in-process may name a part a report drops — and then lives under
// objects/ — where one NO git reads names none and keeps its files on a
// report; one git writes names a part, whatever else it says. A declaration
// git reads may be written by both kinds of writer: the installed
// commit-graph layers by the cache's install and the node's tip over them by
// git, one chain file listing both. And what the installed chain's
// declarations hold beside it: a name carrying its own checksum (Named) has
// the one group Prior has and is never both; the chain file, the one Main,
// carries neither; a chain is cut to a prefix of itself and dropped on
// nothing; and no other declaration cuts. Each row breaks one of those on a
// valid declaration of the build.
//
// Mutation: restore P2's "an in-process declaration has Part 0" — the
// installed chain's own declaration is refused (the accepted rows fail, and
// Open of every cache with them); restore "a declaration's writes are of one
// kind" — the graph's, its install's and its tip's, is refused.
func TestValidateHoldsAPartToWhoReadsTheFiles(t *testing.T) {
	t.Parallel()
	of := func(name DerivedName) Derived {
		for _, d := range DerivedFiles() {
			if d.Name == name {
				return d
			}
		}
		t.Fatalf("the build declares no %s", name)
		return Derived{}
	}
	for _, tc := range []struct {
		name     DerivedName
		gitWrite bool
	}{{DerivedLayers, false}, {DerivedGraphLayers, true}} {
		d := of(tc.name)
		installs := slices.ContainsFunc(d.Writes, func(w Write) bool { return w.Writer.Subcommand == "" })
		if !installs || !d.gitReads() || d.gitWritten() != tc.gitWrite {
			t.Fatalf("%s is installed by the cache %t, read by git %t and written by git %t: the installed chain "+
				"is the first two, and its graph's the third besides, for the node's tip", tc.name, installs,
				d.gitReads(), d.gitWritten())
		}
		if err := d.Validate(); err != nil {
			t.Errorf("a declaration git reads that the cache installs, %s: %v", tc.name, err)
		}
	}
	// Validate alone accepts a part on an in-process write — the chain's
	// shape — and the root rule is what holds where its files live
	// (TestValidateRefusesAnInProcessDeclarationThatClaimsAGitWrite's roots).
	prov := of(DerivedProvenance)
	prov.Part = gitproc.PartCommitGraph
	if err := prov.Validate(); err != nil {
		t.Errorf("an in-process declaration with a part: %v", err)
	}
	for _, tc := range []struct {
		name   string
		decl   DerivedName
		mutate func(d *Derived)
		field  string
	}{
		{"a git write naming no part", DerivedMidx, func(d *Derived) { d.Part = 0 }, "Part"},
		{"a drop on a report of files no git reads", DerivedProvenance, func(d *Derived) { d.OnReported = Drop },
			"OnReported"},
		{"a checksum in a name of two groups", DerivedLayers, func(d *Derived) {
			d.Owns[1].Name = regexp.MustCompile(`^multi-pack-index-([0-9a-f]{40})\.(midx)$`)
		}, "Owns[1].Name"},
		{"a name both Prior and Named", DerivedLayers, func(d *Derived) { d.Owns[1].Prior = true }, "Owns[1]"},
		{"a chain file named by a checksum", DerivedGraphLayers, func(d *Derived) {
			d.Owns[0].Name = regexp.MustCompile(`^commit-graph-chain-([0-9a-f]{40})$`)
			d.Owns[0].Named = true
		}, "Owns[0]"},
		{"a chain dropped when a pack leaves", DerivedLayers, func(d *Derived) { d.OnPackRemoved = Drop },
			"On(pack-removed)"},
		{"a chain kept whole as it moves", DerivedGraphLayers, func(d *Derived) { d.OnLayersMoved = Keep },
			"OnLayersMoved"},
		{"a chain no git reads", DerivedLayers, func(d *Derived) { d.Part = 0 }, "Check"},
		{"a cut of what holds no chain", DerivedMidx, func(d *Derived) { d.OnPackRemoved = Prefix },
			"On(pack-removed)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := of(tc.decl)
			if err := d.Validate(); err != nil {
				t.Fatalf("the declaration before the change: %v", err)
			}
			tc.mutate(&d)
			err := d.Validate()
			if err == nil {
				t.Fatalf("Validate passed %s with %s", tc.decl, tc.name)
			}
			if !strings.Contains(err.Error(), `"`+string(tc.decl)+`"`) || !strings.Contains(err.Error(), tc.field) {
				t.Errorf("Validate answered %q: want it to name the declaration and the field %s", err, tc.field)
			}
		})
	}
}

// A TREE IS ONE DIRECTORY THE CACHE'S OWN CODE OWNS WHOLE, AND NOTHING ELSE
// LIVES IN IT. A declaration that owns a directory whole (Owned.Tree) — the
// memo's crewlet/memo, one file an answer — is the cache's own write: git
// writes no directory the cache keeps without reading inside. It is one
// literal name, carrying no checksum, and the declaration's main entry, whose
// presence and bytes are the declaration's record (Derived.tree). Its files'
// check is read by the cache's own reads alone (CheckEntryDigest), never by a
// git-written declaration's. And no other declaration's files live under it:
// the image keeps a tree without reading inside, so one there would be kept by
// no sweep and counted by no measure. Each row breaks one of those.
//
// Mutation: admit a tree that is not its declaration's main entry (drop the
// tree's Main case) — the "not main" row passes.
func TestValidateRefusesATreeThatIsNoDirectoryOfItsOwn(t *testing.T) {
	t.Parallel()
	of := func(name DerivedName) Derived {
		for _, d := range DerivedFiles() {
			if d.Name == name {
				return d
			}
		}
		t.Fatalf("the build declares no %s", name)
		return Derived{}
	}
	for _, tc := range []struct {
		name   string
		decl   DerivedName
		mutate func(d *Derived)
		field  string
	}{
		{"a tree git writes", DerivedMidx, func(d *Derived) { d.Owns[0].Tree = true }, "Owns[0].Tree"},
		{"a tree of many names", DerivedMemo, func(d *Derived) {
			d.Owns[0].Name = regexp.MustCompile(`^memo[0-9]$`)
		}, "Owns[0].Name"},
		{"a tree carrying a checksum", DerivedMemo, func(d *Derived) { d.Owns[0].Prior = true }, "Owns[0].Name"},
		{"a tree that is not its declaration's main entry", DerivedMemo, func(d *Derived) { d.Owns[0].Main = false },
			"Owns[0].Main"},
		{"an entry's digest on a file git writes", DerivedMidx, func(d *Derived) { d.Check = CheckEntryDigest },
			"Check"},
		{"a move of no tree", DerivedProvenance, func(d *Derived) { d.Seed = SeedMove }, "Seed"},
		{"a tree linked an entry at a time", DerivedMemo, func(d *Derived) { d.Seed = SeedLink }, "Seed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := of(tc.decl)
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

	t.Run("nothing inside a tree", func(t *testing.T) {
		t.Parallel()
		decls := DerivedFiles()
		for i := range decls {
			if decls[i].Name == DerivedProvenance {
				decls[i].Owns[0].Dir = filepath.ToSlash(memoRel)
			}
		}
		err := validateAll(decls)
		if err == nil || !strings.Contains(err.Error(), "owns whole") {
			t.Errorf("validateAll answered %v, want it to refuse the index of pushes' runs inside the memo's tree",
				err)
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
			case d.Name == DerivedMidx && w.Trigger == TriggerRound:
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
			map[string][]string{"objects/pack": {tmpBitmap().String()}}},
		{"a write held twice", []Write{bitmap, bitmap}, []string{lock},
			map[string][]string{"objects/pack": {tmpBitmap().String()}}},
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
