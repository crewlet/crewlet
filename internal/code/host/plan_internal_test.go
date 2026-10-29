package host

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// idOf is a stand-in object id for what a case's parent holds at p.
func idOf(p string) string { return fmt.Sprintf("%x", sha1.Sum([]byte(p))) }

// depthOf is a directory's depth counted from its path: 0 for the root.
func depthOf(d string) int {
	if d == "" {
		return 0
	}
	return strings.Count(d, "/") + 1
}

// A PLAN JUDGES EACH DIRECTORY ONCE, from its parent's judgement, however many
// rules ask of it: a plan of puts and edits at the foot of forty chains
// sixty-four deep, and of a removal halfway down each with a path made anew
// beneath it, judges every directory above a path it names exactly once and
// lays the directories out by depth in one pass — its work a few map
// operations a directory, whatever the depth.
//
// Mutation: judge a directory again at every ask (drop ancestry's memo) — the
// count grows with the depth squared, red.
func TestAPlanJudgesEachDirectoryOnce(t *testing.T) {
	t.Parallel()
	const depth, chains = 64, 40
	parent := idOf("parent")
	paths := map[string]object{}
	var files []code.FileOp
	text := "x\n"
	for i := range chains {
		foot := fmt.Sprintf("c%d", i) + strings.Repeat("/a", depth-2)
		for d := foot; d != "."; d = path.Dir(d) {
			paths[d] = object{id: idOf(d), kind: "tree", mode: modeTree}
		}
		paths[foot+"/f"] = object{id: idOf(foot + "/f"), kind: "blob", mode: code.ModeFile}
		files = append(files,
			code.FileOp{Op: code.FileOpPut, Path: foot + "/new", Content: &text},
			code.FileOp{Op: code.FileOpEdit, Path: foot + "/f", Old: "x", New: "y"})
		if i%2 == 0 {
			// The other half: a directory halfway down removed and a path made anew beneath it.
			half := fmt.Sprintf("d%d", i) + strings.Repeat("/a", depth/2-1)
			for d := half; d != "."; d = path.Dir(d) {
				paths[d] = object{id: idOf(d), kind: "tree", mode: modeTree}
			}
			files = append(files, code.FileOp{Op: code.FileOpDelete, Path: half},
				code.FileOp{Op: code.FileOpPut, Path: half + strings.Repeat("/b", depth/2-1) + "/new",
					Content: &text})
		}
	}
	p, refusal, detail := planFiles(files, parent, paths)
	if refusal != "" {
		t.Fatalf("the plan refused %s: %s", refusal, detail)
	}
	// Every chain rewrites the directories above its deepest path, depth-1 of
	// them, and the root.
	directories := (chains+chains/2)*(depth-1) + 1
	if len(p.dirs) != directories {
		t.Fatalf("the plan rewrites %d directories, want %d", len(p.dirs), directories)
	}
	if p.judged != len(p.dirs)-1 {
		t.Errorf("the plan judged %d directories for the %d above the paths it names, want each once", p.judged,
			len(p.dirs)-1)
	}
	levels := p.levels()
	if len(levels) != depth || p.depth != depth-1 {
		t.Fatalf("the plan lays out %d levels (deepest %d), want %d", len(levels), p.depth, depth)
	}
	for d, level := range levels {
		if !slices.IsSorted(level) {
			t.Errorf("level %d is not sorted: %q", d, level)
		}
		for _, dir := range level {
			if got := p.dirs[dir].depth; got != d || got != depthOf(dir) {
				t.Errorf("%q is laid out at level %d with depth %d, want %d", dir, d, got, depthOf(dir))
			}
		}
	}
	for i := 0; i < chains; i += 2 {
		half := fmt.Sprintf("d%d", i) + strings.Repeat("/a", depth/2-1)
		if dc := p.dirs[half]; dc == nil || dc.base != "" {
			t.Errorf("the removed directory %s, made anew, is laid out on %+v, want no base", half, dc)
		}
	}
}

// A PLAN JUDGES AS WALKING EVERY ANCESTOR DID: over twenty thousand changes
// drawn at random against random parents — files, links, submodules and
// directories three names wide and four deep, puts, edits, modes, deletes
// and moves among them — the plan that judges each directory once answers
// every change exactly as the plan that walked every ancestor of every path
// at every ask: the same refusal and sentence, or the same layout.
//
// Mutations: judge a path's nesting by its nearest named directory's role
// alone — a removal above a kept file passes, red; let a blocker stand beneath
// a removal — a directory made anew under a removed file is refused, red.
func TestAPlanJudgesAsWalkingEveryAncestorDid(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(20261029, 3))
	names := []string{"a", "b", "c"}
	randPath := func() string {
		parts := make([]string, 1+rng.IntN(4))
		for i := range parts {
			parts[i] = names[rng.IntN(len(names))]
		}
		return strings.Join(parts, "/")
	}
	leaves := []object{
		{kind: "blob", mode: code.ModeFile}, {kind: "blob", mode: code.ModeExecutable},
		{kind: "blob", mode: code.ModeLink}, {kind: "submodule", mode: modeSubmodule}, {kind: "tree", mode: modeTree},
	}
	text := "t\n"
	refused, laid := 0, 0
	for n := range 20_000 {
		parent := ""
		held := map[string]object{}
		if rng.IntN(8) > 0 {
			parent = idOf(fmt.Sprint("parent", n))
		leaf:
			for range rng.IntN(7) {
				p := randPath()
				if _, ok := held[p]; ok {
					continue
				}
				for d := path.Dir(p); d != "."; d = path.Dir(d) {
					if o, ok := held[d]; ok && o.kind != "tree" {
						continue leaf
					}
				}
				for d := path.Dir(p); d != "."; d = path.Dir(d) {
					held[d] = object{id: idOf(d), kind: "tree", mode: modeTree}
				}
				o := leaves[rng.IntN(len(leaves))]
				o.id = idOf(p)
				held[p] = o
			}
		}
		var files []code.FileOp
		named := map[string]bool{}
		claim := func(p string) bool {
			if named[p] {
				return false
			}
			named[p] = true
			return true
		}
		for range 1 + rng.IntN(4) {
			f := code.FileOp{Path: randPath()}
			switch rng.IntN(5) {
			case 0:
				f.Op = code.FileOpPut
				if rng.IntN(2) == 0 {
					f.Content = &text
				} else {
					f.BlobOID = idOf("blob")
				}
				f.Mode = []string{"", code.ModeFile, code.ModeExecutable}[rng.IntN(3)]
			case 1:
				f.Op, f.Old, f.New = code.FileOpEdit, "t", "u"
			case 2:
				f.Op, f.Mode = code.FileOpMode, code.ModeExecutable
			case 3:
				f.Op = code.FileOpDelete
			default:
				f.Op, f.To = code.FileOpMove, randPath()
				if f.To == f.Path || !claim(f.To) {
					continue
				}
			}
			if !claim(f.Path) {
				continue
			}
			files = append(files, f)
		}
		if len(files) == 0 {
			continue
		}
		got, gotRefusal, gotDetail := planFiles(files, parent, held)
		want, wantRefusal, wantDetail := walkedPlan(files, parent, held)
		if gotRefusal != wantRefusal || gotDetail != wantDetail {
			t.Fatalf("case %d (%+v over %v): the plan answers %q %q, walking every ancestor %q %q", n, files,
				slices.Sorted(maps.Keys(held)), gotRefusal, gotDetail, wantRefusal, wantDetail)
		}
		if gotRefusal != "" {
			refused++
			continue
		}
		laid++
		if why := sameLayout(got, want); why != "" {
			t.Fatalf("case %d (%+v over %v): %s", n, files, slices.Sorted(maps.Keys(held)), why)
		}
	}
	if refused < 1_000 || laid < 1_000 {
		t.Errorf("the cases drew %d refusals and %d layouts: too few of one to judge the two plans alike", refused,
			laid)
	}
}

// sameLayout is why two plans lay a change out differently, "" where they do
// not.
func sameLayout(got, want *plan) string {
	if !slices.Equal(got.order(), want.order()) {
		return fmt.Sprintf("the plan rewrites %q, walking every ancestor %q", got.order(), want.order())
	}
	if got.depth != want.depth {
		return fmt.Sprintf("the plan's deepest directory is %d, walking every ancestor %d", got.depth, want.depth)
	}
	for _, d := range got.order() {
		g, w := got.dirs[d], want.dirs[d]
		if g.base != w.base || !maps.Equal(g.drop, w.drop) || !maps.Equal(g.sub, w.sub) || g.depth != depthOf(d) {
			return fmt.Sprintf("%q is %+v, walking every ancestor %+v", d, g, w)
		}
		if len(g.set) != len(w.set) {
			return fmt.Sprintf("%q sets %v, walking every ancestor %v", d, g.set, w.set)
		}
		for name, ge := range g.set {
			we, ok := w.set[name]
			if !ok || ge.mode != we.mode || ge.id != we.id || (ge.blob == nil) != (we.blob == nil) ||
				ge.blob != nil && ge.blob.path != we.blob.path {
				return fmt.Sprintf("%q sets %s to %+v, walking every ancestor %+v", d, name, ge, we)
			}
		}
	}
	if len(got.puts) != len(want.puts) || len(got.edits) != len(want.edits) {
		return fmt.Sprintf("the plan writes %d puts and %d edits, walking every ancestor %d and %d", len(got.puts),
			len(got.edits), len(want.puts), len(want.edits))
	}
	for i, e := range got.edits {
		if w := want.edits[i]; e.blob.path != w.blob.path || e.base != w.base || e.old != w.old || e.new != w.new {
			return fmt.Sprintf("edit %d is %+v, walking every ancestor %+v", i, e, w)
		}
	}
	return ""
}

// walkedPlan is the plan as it judged before each directory was judged once:
// every rule walking every ancestor of the path it asks of. The oracle of the
// plan's rules, never its cost.
func walkedPlan(files []code.FileOp, parent string, paths map[string]object) (*plan, code.WriteRefusal, string) {
	at := func(p string) object { return paths[p] }
	where := "in a commit with no parent"
	if parent != "" {
		where = "at " + shortID(parent)
	}
	role := map[string]int{}
	by := map[string]int{}
	for i, f := range files {
		switch f.Op {
		case code.FileOpPut:
			role[f.Path] = pathMade
		case code.FileOpEdit, code.FileOpMode:
			role[f.Path] = pathKept
		case code.FileOpDelete:
			role[f.Path] = pathRemoved
		case code.FileOpMove:
			role[f.Path], role[f.To], by[f.To] = pathRemoved, pathMade, i
		}
		by[f.Path] = i
	}
	removedAbove := func(p string) bool {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if role[d] == pathRemoved {
				return true
			}
		}
		return false
	}
	named := slices.Sorted(maps.Keys(role))
	for _, p := range named {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			r, isNamed := role[d]
			if !isNamed || (r == pathRemoved && role[p] == pathMade) {
				continue
			}
			return nil, code.WritePathConflict, nested(files, by[d], d, by[p], p, r)
		}
	}
	for i, f := range files {
		o := at(f.Path)
		switch f.Op {
		case code.FileOpEdit, code.FileOpMode:
			switch {
			case !o.present():
				return nil, code.WriteNotFound, fmt.Sprintf("files[%d]: %s does not exist %s: read the tree again",
					i, f.Path, where)
			case !o.file():
				return nil, code.WriteNotAFile, fmt.Sprintf("files[%d]: %s is %s %s, not a file: an %s changes a "+
					"regular or an executable file", i, f.Path, o.what(), where, f.Op)
			}
		case code.FileOpDelete, code.FileOpMove:
			if !o.present() {
				return nil, code.WriteNotFound, fmt.Sprintf("files[%d]: %s does not exist %s: read the tree again",
					i, f.Path, where)
			}
		case code.FileOpPut:
			if o.kind == "tree" && !removedAbove(f.Path) {
				return nil, code.WritePathConflict, fmt.Sprintf("files[%d]: %s is a directory %s, and a put makes "+
					"a file: remove the directory in a write of its own first", i, f.Path, where)
			}
		}
		if f.Op == code.FileOpMove {
			if t := at(f.To); t.present() && !removedAbove(f.To) {
				return nil, code.WritePathConflict, fmt.Sprintf("files[%d]: %s already exists %s, and a move "+
					"overwrites nothing (git mv's rule): remove it in a write of its own first", i, f.To, where)
			}
		}
	}
	for _, p := range named {
		if role[p] != pathMade {
			continue
		}
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			if _, isNamed := role[d]; isNamed {
				break
			}
			if o := at(d); o.present() && o.kind != "tree" && !removedAbove(d) {
				return nil, code.WritePathConflict, fmt.Sprintf("files[%d]: %s is beneath %s, which is %s %s, not "+
					"a directory: delete or move %s in this write to make a directory there", by[p], p, d, o.what(),
					where, d)
			}
		}
	}
	p := &plan{dirs: map[string]*dirChange{}}
	var dir func(d string) *dirChange
	dir = func(d string) *dirChange {
		if dc, ok := p.dirs[d]; ok {
			return dc
		}
		dc := &dirChange{path: d, drop: map[string]bool{}, set: map[string]setEntry{}, sub: map[string]bool{}}
		switch {
		case d == "":
			if parent != "" {
				dc.base = parent + "^{tree}"
			}
		case role[d] != pathRemoved && !removedAbove(d):
			if o := at(d); o.kind == "tree" {
				dc.base = o.id
			}
		}
		p.dirs[d] = dc
		p.depth = max(p.depth, depthOf(d))
		if d != "" {
			dir(parentDir(d)).sub[path.Base(d)] = true
		}
		return dc
	}
	entry := func(fp string) (*dirChange, string) { return dir(parentDir(fp)), path.Base(fp) }
	for _, f := range files {
		switch f.Op {
		case code.FileOpPut:
			dc, name := entry(f.Path)
			mode := f.Mode
			if mode == "" {
				mode = code.ModeFile
				if o := at(f.Path); o.file() && !removedAbove(f.Path) {
					mode = o.mode
				}
			}
			if f.Content == nil {
				dc.set[name] = setEntry{mode: mode, id: f.BlobOID}
				continue
			}
			nb := &newBlob{path: f.Path}
			p.puts = append(p.puts, &planPut{blob: nb, content: *f.Content})
			dc.set[name] = setEntry{mode: mode, blob: nb}
		case code.FileOpEdit:
			dc, name := entry(f.Path)
			o := at(f.Path)
			nb := &newBlob{path: f.Path}
			p.edits = append(p.edits, &planEdit{blob: nb, base: o.id, old: f.Old, new: f.New})
			dc.set[name] = setEntry{mode: o.mode, blob: nb}
		case code.FileOpMode:
			dc, name := entry(f.Path)
			dc.set[name] = setEntry{mode: f.Mode, id: at(f.Path).id}
		case code.FileOpDelete:
			dc, name := entry(f.Path)
			dc.drop[name] = true
		case code.FileOpMove:
			src := at(f.Path)
			dc, name := entry(f.Path)
			dc.drop[name] = true
			dt, to := entry(f.To)
			dt.set[to] = setEntry{mode: src.mode, id: src.id}
		}
	}
	return p, "", ""
}

// THE ASKS ARE SPELLED AS GIT READS THEM, one at a time: read through a
// buffer of any size — a byte, a few, more than all of them — the input is
// every ask NUL-ended, ids first, then each path at its commit, and git's
// answer that an ask names nothing is matched to that ask alone, never to
// one it begins or ends like.
func TestTheLookupsAreSpelledAsGitReadsThem(t *testing.T) {
	t.Parallel()
	at := idOf("parent")
	q := lookups{ids: []string{idOf("one"), idOf("two")}, at: at, paths: []string{"a", "a/b c", "a/b c/d\nlf"}}
	want := idOf("one") + "\x00" + idOf("two") + "\x00" + at + ":a\x00" + at + ":a/b c\x00" + at + ":a/b c/d\nlf\x00"
	for _, size := range []int{1, 2, 3, 7, 41, 43, 4096} {
		r := q.reader()
		var got bytes.Buffer
		buf := make([]byte, size)
		for {
			n, err := r.Read(buf)
			got.Write(buf[:n])
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("a read of %d bytes: %v", size, err)
			}
		}
		if got.String() != want {
			t.Errorf("read %d bytes at a time, the asks are %q, want %q", size, got.String(), want)
		}
	}
	for _, tc := range []struct {
		i   int
		rec string
		ok  bool
	}{
		{0, idOf("one") + " missing", true},
		{0, idOf("two") + " missing", false},
		{0, idOf("one") + " blob 3 ", false},
		{2, at + ":a missing", true},
		{3, at + ":a/b c missing", true},
		{3, at + ":a missing", false},
		{3, at + ":a/b c/d missing", false},
		{4, at + ":a/b c/d\nlf missing", true},
		{2, idOf("other") + ":a missing", false},
		{2, at + "/a missing", false},
		{2, at + ":a", false},
	} {
		if got := q.missing(tc.i, []byte(tc.rec)); got != tc.ok {
			t.Errorf("%q answering ask %d (%q) reads as missing: %t, want %t", tc.rec, tc.i, q.name(tc.i), got,
				tc.ok)
		}
	}
}
