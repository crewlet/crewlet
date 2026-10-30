package host

import (
	"fmt"
	"io"
	"maps"
	"path"
	"slices"

	"github.com/crewlet/crewlet/internal/code"
)

// A COMMIT_FILES' CHANGE IS PLANNED BEFORE ITS DIRECTORIES ARE READ (planFiles):
// its operations judged against what the parent holds at every path they name
// and every directory above one — the build's first read (building.paths) —
// and laid out by directory, so the build reads exactly the directories the
// change touches and writes exactly those again.
//
// THE TREE AS THE CALL LEAVES IT is what is judged, so one call may remove a
// path and make another beneath it — delete `a` and put `a/b` is one change,
// `a` a directory holding `b` alone. Each operation reads the PARENT: an
// edit, a mode, a delete and a move name what the parent holds, and a path
// the call names beneath one it removes may only be made anew there. Every
// other nesting of two named paths is a path_conflict, since one of the two
// would undo the other: a path beneath one the call makes a file, keeps a file
// or moves an entry to, and an edit, a mode, a delete or a move out of a path
// beneath one the call removes.
//
// EVERY DIRECTORY IS JUDGED ONCE, from its parent's judgement (ancestry):
// whether the call removes it or a path above it, the nearest path the call
// names at or above it, and the file above it a path the call makes would
// need to be a directory. Every rule above asks those of a path's directory,
// and a rule that walked the directory's ancestors again at every ask made a
// plan's work grow with the CUBE of its paths' depth — every directory of a
// path asking again of every one above it, each ask hashing a prefix as long
// as its own: one put 2 047 components deep, which a 4 KiB path holds, took
// 5.3 s of a core before a byte of git ran. So the plan's work is a few map
// operations a path it names and a directory above one, each hashing the
// directory's own path once, whatever the depth — and the depth is bounded
// besides (code.MaxTreePathDepth, refused before anything is read), so what a
// plan holds is at most a directory a component of every path it names.

// plan is a commit_files' change laid out by directory.
type plan struct {
	// dirs is every directory the change rewrites, by path, "" the root:
	// each one holding a path an operation names, and every one above it.
	dirs map[string]*dirChange
	// depth is the deepest directory's (dirChange.depth).
	depth int
	// edits are the files an edit rewrites, and puts the files a put writes
	// from its text, each in its operation's order.
	edits []*planEdit
	puts  []*planPut
	// judged is how many directories the plan judged (ancestry.judged): one
	// for each directory above a path the call names, however many ask —
	// TestAPlanJudgesEachDirectoryOnce's window onto the plan's work.
	judged int
}

// dirChange is one directory the change rewrites.
type dirChange struct {
	path string
	// depth is the directory's: 0 for the root, 1 for a child of it — its
	// parent's and one, never counted again from its path.
	depth int
	// base names the directory's entries at the parent — the parent's root
	// tree, or the directory's tree id — and is "" where the directory is
	// new: the parent holds none there, or the change removes what it held.
	base string
	// staged is where the build staged the base's entries as git stores
	// them, "" for none.
	staged string
	// drop is the names the change removes, set those it makes a file, a
	// link or a moved entry, and sub the child directories it rewrites.
	drop map[string]bool
	set  map[string]setEntry
	sub  map[string]bool
}

// touches reports a name the change removes, replaces or rewrites: one whose
// entry in the base is not passed through.
func (d *dirChange) touches(name string) bool {
	_, set := d.set[name]
	return set || d.drop[name] || d.sub[name]
}

// setEntry is one entry the change makes: an object the repository holds by
// its id, or a file the build writes anew.
type setEntry struct {
	mode, id string
	blob     *newBlob
}

// newBlob is a file the build writes anew — a put's text or an edit's result
// — staged, then hashed.
type newBlob struct {
	path, staged, id string
}

// planEdit is one edit: the file's blob at the parent, read through the edit
// into its new blob — through its old and new text, or, for an
// apply_suggestions, through its splices.
type planEdit struct {
	blob     *newBlob
	base     string
	old, new string
	splices  []splice
	stream   rewriter
}

// rewriter is a file read through a change to it into its staged new blob —
// an edit's ([editStream]) or an apply_suggestions' ([spliceStream]) — and
// closed with the change's refusal of the file as it ended, or none.
type rewriter interface {
	io.Writer
	Close(path string) (code.WriteRefusal, string, error)
}

// planPut is one put of a file's text.
type planPut struct {
	blob    *newBlob
	content string
}

// order is every directory the change rewrites, sorted.
func (p *plan) order() []string { return slices.Sorted(maps.Keys(p.dirs)) }

// levels is every directory the change rewrites by its depth, the root's
// first, each level sorted: ONE pass over them, never one a level.
func (p *plan) levels() [][]string {
	out := make([][]string, p.depth+1)
	for _, d := range p.order() {
		depth := p.dirs[d].depth
		out[depth] = append(out[depth], d)
	}
	return out
}

// newBlobs is every file the build writes anew, puts then edits.
func (p *plan) newBlobs() []*newBlob {
	out := make([]*newBlob, 0, len(p.puts)+len(p.edits))
	for _, put := range p.puts {
		out = append(out, put.blob)
	}
	for _, e := range p.edits {
		out = append(out, e.blob)
	}
	return out
}

// The ways a commit_files names a path.
const (
	// pathRemoved is a delete's path and a move's source.
	pathRemoved = iota + 1
	// pathMade is a put's path and a move's destination.
	pathMade
	// pathKept is an edit's and a mode's path: a file that stays a file.
	pathKept
)

// ancestry is what the plan knows of every directory above a path the call
// names, each JUDGED ONCE, from its parent's judgement, and asked of as often
// as a rule needs it.
type ancestry struct {
	role  map[string]int
	at    func(string) object
	facts map[string]dirFacts
	// judged counts the directories judged, each once.
	judged int
}

// dirFacts is what one directory is to the call.
type dirFacts struct {
	// gone: the call removes the directory or a path above it, so whatever
	// the parent holds there is not the change's to keep.
	gone bool
	// named is the nearest path the call names at or above the directory,
	// "" for none.
	named string
	// blocker is the deepest path at or above the directory, beneath named,
	// that the parent holds as something other than a directory and that no
	// removal above clears — where a path the call makes beneath it would
	// need a directory — "" for none.
	blocker string
}

// of is what directory d is to the call, "" the root, which is nothing.
func (a *ancestry) of(d string) dirFacts {
	if d == "" {
		return dirFacts{}
	}
	if f, ok := a.facts[d]; ok {
		return f
	}
	up := a.of(parentDir(d))
	a.judged++
	f := up
	if r, isNamed := a.role[d]; isNamed {
		// A NAMED DIRECTORY ENDS THE SEARCH for a blocker: what it is is
		// its operation's to judge, and so is everything above it.
		f.named, f.blocker = d, ""
		f.gone = f.gone || r == pathRemoved
	} else if o := a.at(d); o.present() && o.kind != "tree" && !up.gone {
		f.blocker = d
	}
	a.facts[d] = f
	return f
}

// above is what the directory holding path p is to the call: what p's place is.
func (a *ancestry) above(p string) dirFacts { return a.of(parentDir(p)) }

// planFiles judges files against what the parent commit holds at each path
// they name and each directory above one (paths, by path: a path the parent
// does not hold is absent, and a root commit, parent "", holds nothing) and
// lays them out, or answers the change's refusal and its sentence.
func planFiles(files []code.FileOp, parent string, paths map[string]object) (*plan, code.WriteRefusal, string) {
	at := func(p string) object { return paths[p] }
	where := "in a commit with no parent"
	if parent != "" {
		where = "at " + shortID(parent)
	}
	role := map[string]int{}
	by := map[string]int{} // the operation naming each path
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
	a := &ancestry{role: role, at: at, facts: map[string]dirFacts{}}
	named := slices.Sorted(maps.Keys(role))

	// 1. NESTING: beneath a path the call removes, only a path it makes. The
	// nearest named path above each is the one judged: where a further one
	// conflicts and the nearest does not, the nearest — removed, beneath it —
	// conflicts with that further one itself, and sorts before.
	for _, p := range named {
		if d := a.above(p).named; d != "" && (role[d] != pathRemoved || role[p] != pathMade) {
			return nil, code.WritePathConflict, nested(files, by[d], d, by[p], p, role[d])
		}
	}

	// 2. EACH OPERATION against what the parent holds where it reads.
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
			if o.kind == "tree" && !a.above(f.Path).gone {
				return nil, code.WritePathConflict, fmt.Sprintf("files[%d]: %s is a directory %s, and a put makes "+
					"a file: remove the directory in a write of its own first", i, f.Path, where)
			}
		}
		if f.Op == code.FileOpMove {
			if t := at(f.To); t.present() && !a.above(f.To).gone {
				return nil, code.WritePathConflict, fmt.Sprintf("files[%d]: %s already exists %s, and a move "+
					"overwrites nothing (git mv's rule): remove it in a write of its own first", i, f.To, where)
			}
		}
	}

	// 3. ABOVE EVERY PATH THE CALL MAKES, a directory: the parent's, a new
	// one, or one made anew where the call removes what was there.
	for _, p := range named {
		if role[p] != pathMade {
			continue
		}
		if d := a.above(p).blocker; d != "" {
			return nil, code.WritePathConflict, fmt.Sprintf("files[%d]: %s is beneath %s, which is %s %s, not "+
				"a directory: delete or move %s in this write to make a directory there", by[p], p, d, at(d).what(),
				where, d)
		}
	}

	// 4. THE LAYOUT, by directory.
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
		case !a.of(d).gone:
			if o := at(d); o.kind == "tree" {
				dc.base = o.id
			}
		}
		if d != "" {
			up := dir(parentDir(d))
			up.sub[path.Base(d)] = true
			dc.depth = up.depth + 1
		}
		p.dirs[d] = dc
		p.depth = max(p.depth, dc.depth)
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
				if o := at(f.Path); o.file() && !a.above(f.Path).gone {
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
	p.judged = a.judged
	return p, "", ""
}

// nested is the sentence of a path the call names beneath another it names:
// inner (files[ii]) beneath outer (files[oi]), which the call treats as r.
func nested(files []code.FileOp, oi int, outer string, ii int, inner string, r int) string {
	switch r {
	case pathRemoved:
		return fmt.Sprintf("files[%d] removes %s, and files[%d] (%s) names %s beneath it, which goes with it: "+
			"beneath a path the change removes, name only what it makes anew", oi, outer, ii, files[ii].Op, inner)
	case pathKept:
		return fmt.Sprintf("files[%d] keeps %s a file, and files[%d] names %s beneath it: a file holds no paths",
			oi, outer, ii, inner)
	}
	if files[oi].Op == code.FileOpMove {
		return fmt.Sprintf("files[%d] moves an entry to %s, and files[%d] names %s beneath it: put it beside the "+
			"move in a write of its own", oi, outer, ii, inner)
	}
	return fmt.Sprintf("files[%d] makes %s a file, and files[%d] names %s beneath it: a file holds no paths", oi,
		outer, ii, inner)
}

// parentDir is the directory holding p, "" for the root.
func parentDir(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// joinPath is the path of name in dir, dir "" the root.
func joinPath(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
