package cache

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// THE DERIVED FILES (ADR-0056). A node-local derived file is DECLARED here,
// lives inside its incarnation, and the cache's sweep, Verify, check, scrub,
// budget, plan and writers gate read the declaration — a derived file is added
// by adding a declaration, never beside them.
//
// A derived file is one the cache writes from what an incarnation already
// holds — its packs, its refs — and that no row names: the multi-pack-index (a
// full round's, and the maintainer's with its bitmap or without one) and the
// commit-graph. Each was spelled in a dozen places that agreed only because
// nothing had changed them apart: its file names in the sweep, Verify, the
// size measure, the check and the scrub; its argument lists, charges, lanes
// and deadlines beside each writer; what a removed pack and what a damage
// report did to it in the code that removed one or read the other. A file a
// writer leaves that the grammar does not admit is trashed under a running
// reader, and a file the grammar admits that no writer makes is a hole anyone
// may fill — and both fail silently. One declaration a kind is what makes
// adding a kind one change the readers cannot miss.
//
// What a declaration says, and who reads it:
//
//   - what it is a function of (Inputs), and every WRITE that makes it — its
//     trigger, lane, deadline, memory charge, the file and git subcommand that
//     run it, and its argument list (Writes): the maintainer's plan and the
//     round's index take their steps from here (maintain.go, memory.go), and
//     the writers gate holds each writer to its row in both directions
//     (TestNothingButSyncWritesACache);
//   - the files it OWNS — what the image keeps, what Verify admits, what the
//     check and the scrub hold to the declaration's Check, what a drop moves
//     and what the budget counts — and the files each write of it HOLDS only
//     while it runs (Write.Holds), which Verify admits only then and that
//     write leaves for removal when it does not succeed (sweep.go, verify.go,
//     check.go, scrub.go, maintain.go);
//   - what it promises a reader (Freshness), and what each change to its
//     inputs does to it (On): a pack added, a pack removed, the refs
//     rewritten, or its own files reported damaged — every one of them read by
//     dropFor, the one removal of a derived file, at the one place each change
//     is made;
//   - which part of a repository a damage report names it by (Part, in
//     gitproc's vocabulary: gitproc.Damage.Drops), so a report reaches the
//     declarations it names and no others.
//
// Its files live INSIDE THE INCARNATION, so they are counted by the budget,
// leave with the repository and are written again over a rebuild's packs; a
// pack's .pack, .idx and .rev are not derived files — the rows name them.

// DerivedName names one declared kind of node-local derived file: a closed
// set whose one list is DerivedFiles.
type DerivedName string

const (
	// DerivedMidx is the multi-pack-index over an incarnation's packs: a
	// full round's, written without a bitmap before the round answers once
	// the snapshot names MidxRoundPacks packs, and the maintainer's, with its
	// bitmap where the node's memory holds one and without one where it does
	// not (memory.go).
	DerivedMidx DerivedName = "midx"
	// DerivedGraph is the maintainer's commit-graph, written first on an
	// incarnation that has none (maintain.go, GRAPH FIRST).
	DerivedGraph DerivedName = "commit-graph"
)

// Valid reports whether n names a declaration of DerivedFiles.
func (n DerivedName) Valid() bool {
	return slices.ContainsFunc(DerivedFiles(), func(d Derived) bool { return d.Name == n })
}

// Input is what a derived file is a function of.
type Input string

const (
	// InputPacks is the incarnation's pack files — what an index merges, and
	// what a graph reads its commits from.
	InputPacks Input = "packs"
	// InputRefs is packed-refs — a bitmap's selected commits, and the graph's
	// --reachable tips.
	InputRefs Input = "refs"
)

// Inputs is every Input.
var Inputs = []Input{InputPacks, InputRefs}

// Valid reports whether i is one of Inputs.
func (i Input) Valid() bool { return slices.Contains(Inputs, i) }

// Trigger is what starts a write.
type Trigger string

const (
	// TriggerRound is a full round, before it answers.
	TriggerRound Trigger = "round"
	// TriggerMaintainer is the maintainer's pass — debounced, duty-cycled and
	// admitted on the node's memory.
	TriggerMaintainer Trigger = "maintainer"
)

// Triggers is every Trigger.
var Triggers = []Trigger{TriggerRound, TriggerMaintainer}

// Valid reports whether t is one of Triggers.
func (t Trigger) Valid() bool { return slices.Contains(Triggers, t) }

// Freshness is the promise a reader of a derived file may rely on.
type Freshness string

const (
	// FreshDebounced is current within MidxMaxWait of its inputs standing
	// still — sooner where a trigger writes it before a round answers — or
	// refused and said so (code_cache_maintenance_refused). Never a
	// correctness promise: git reads a pack's own index where a derived file
	// is absent or covers less.
	FreshDebounced Freshness = "debounced"
)

// Freshnesses is every Freshness.
var Freshnesses = []Freshness{FreshDebounced}

// Valid reports whether f is one of Freshnesses.
func (f Freshness) Valid() bool { return slices.Contains(Freshnesses, f) }

// Check is how a derived file this process did not write is held to its
// bytes before it serves, and how the scrub holds it behind the serving
// (check.go, scrub.go).
type Check string

const (
	// CheckTrailer is git's hashfile trailer — the last twenty bytes are the
	// SHA-1 of every byte before them — and, for an owned file whose name
	// carries a checksum (Owned.Prior), that checksum in the twenty before
	// it. git checks none of it when it reads.
	CheckTrailer Check = "trailer"
)

// Checks is every Check.
var Checks = []Check{CheckTrailer}

// Valid reports whether c is one of Checks.
func (c Check) Valid() bool { return slices.Contains(Checks, c) }

// Event is a change the cache makes in an incarnation that a derived file may
// be built over: what dropFor is told, at the one place each is made.
type Event string

const (
	// EventPackAdded is a pack installed into the incarnation — a round's
	// download, an adopted push.
	EventPackAdded Event = "pack-added"
	// EventPackRemoved is a pack's files taken out of the incarnation, or
	// renamed over with its bundle again where its index may differ — a full
	// round's stray (no row names it: a log gone back or recreated under the
	// cache), and a pack installed again over files at another size than its
	// row's, or whose index failed its checksum. A derived file built over the
	// old files may name what is no longer there.
	EventPackRemoved Event = "pack-removed"
	// EventRefsMoved is a new packed-refs renamed into place by a round.
	EventRefsMoved Event = "refs-moved"
	// EventReported is a report that the declaration's own files are damaged
	// — by a git process (gitproc.Damage, its Part among the parts it
	// names), a request's or a round's own git among them: git's word, which
	// names a part and never the bytes. A declaration that KEEPS its files on
	// a report has them held to its Check instead — the report makes the
	// copy's scrub due (report.go) — and a request that met it is repaired
	// as a pack's damage is (Cache.Rebuild).
	EventReported Event = "reported"

	// eventFailedCheck is a file of the declaration FAILING ITS CHECK — the
	// round's check, the scrub: the bytes' verdict, which no declaration
	// answers, because a file that fails its check is dropped whatever it
	// declares of a report (dropFor, image.dropping). A checkable file that
	// fails its check is never served, and one kept there would be found
	// failing again by every scrub its reports asked. Unexported and not
	// among Events, so no declaration states it.
	eventFailedCheck Event = "failed-check"
)

// Events is every Event a declaration answers, in the order it states them.
var Events = []Event{EventPackAdded, EventPackRemoved, EventRefsMoved, EventReported}

// Valid reports whether e is one of Events.
func (e Event) Valid() bool { return slices.Contains(Events, e) }

// Change is what an Event does to a declared derived file.
type Change string

const (
	// Keep means still valid — git reads what is new beside it.
	Keep Change = "keep"
	// Drop means invalid, or not to be trusted — moved into trash/ before the
	// change is visible to any git process (dropFor).
	Drop Change = "drop"
)

// Changes is every Change.
var Changes = []Change{Keep, Drop}

// Valid reports whether c is one of Changes.
func (c Change) Valid() bool { return slices.Contains(Changes, c) }

// FilePattern is a set of files in one directory of an incarnation.
type FilePattern struct {
	// Dir is relative to the incarnation, slash-separated and clean:
	// "objects/pack", "objects/info".
	Dir string
	// Name matches a whole directory entry's name: anchored ^…$ (Validate
	// refuses one that is not).
	Name *regexp.Regexp
}

// Owned is a set of files a declaration owns: what the image keeps, and what
// its Check holds.
type Owned struct {
	FilePattern
	// File is what a check of it is counted as: the `file` attribute of
	// crewlet.code.cache.checks.
	File string
	// Main: this is the file whose presence is the declaration's — the index
	// itself, the graph itself — where every other owned file is a part of it
	// (a bitmap, a reverse index). Exactly one a declaration, its Name a
	// literal.
	Main bool
	// Prior: the forty hex digits of its name, the pattern's one capture
	// group, are the checksum it carries in the twenty bytes before its
	// trailer — a multi-pack-index's reverse index carries its index's, as a
	// pack's .rev carries its pack's (git's write_rev_trailer).
	Prior bool
}

// Step names one write: the `step` attribute of a refusal's line and counter
// and of the refused gauge (crewlet.code.cache.maintenance.refusals{reason,
// step}, crewlet.code.cache.maintenance.refused{step}).
type Step string

const (
	// StepIndex is a multi-pack-index without a bitmap.
	StepIndex Step = "index"
	// StepBitmap is a multi-pack-index with its bitmap and the bitmap's
	// lookup table.
	StepBitmap Step = "bitmap"
	// StepGraph is the commit-graph.
	StepGraph Step = "graph"
)

// Steps is every Step, in the order a line or a count names them.
var Steps = []Step{StepBitmap, StepIndex, StepGraph}

// Valid reports whether s is one of Steps.
func (s Step) Valid() bool { return slices.Contains(Steps, s) }

// Covers is what one write is made over, read when it is planned: what its
// deadline and its charge are functions of.
type Covers struct {
	// Packs is the packs it covers.
	Packs int
	// Objects is their objects, as their rows count them.
	Objects int64
	// DiskBytes is the incarnation's pack files and packed-refs.
	DiskBytes int64
	// IndexBytes is the covered packs' .idx bytes, as their rows say.
	IndexBytes int64
}

// Writer is the code that runs a write: the non-test file under internal/code,
// slash-separated from the module root, and the git subcommand it runs there.
// TestNothingButSyncWritesACache holds it to that file's allowance row.
type Writer struct {
	File       string
	Subcommand string
}

// Write is one way a declared derived file is written.
type Write struct {
	Step    Step
	Trigger Trigger
	Lane    gitproc.Class
	// Deadline is the write's process bound over what it covers.
	Deadline func(Covers) time.Duration
	// BytesPerObject is the write's own memory an object of what it covers:
	// it is admitted at charge(objects, BytesPerObject), which adds room for
	// one transfer of the same repository (memory.go).
	BytesPerObject int64
	Writer         Writer
	// Args is the argument list its writer hands git: one of the writer
	// file's own literals, so the writers gate reads it where git runs.
	Args []string
	// Holds is what THIS write holds only while it runs: admitted by Verify
	// only then, and removed after a run of it that did not succeed — a lock
	// git removes when asked to stop and leaves when killed, a temporary
	// file it leaves either way (clearWriting). A write's, not its
	// declaration's, because two writes of one file hold different files:
	// the index with its bitmap holds the bitmap's temporary file beside
	// the lock, the round's index the lock alone — so a failed write is
	// cleared of exactly what it may have left, a literal name by name and
	// with no listing. Empty for a write that holds nothing in the
	// incarnation (one staged elsewhere and renamed in).
	Holds []FilePattern
}

// Derived is one declared kind of node-local derived file.
type Derived struct {
	Name   DerivedName
	Inputs []Input
	Writes []Write
	// Owns is what the image keeps (sweep.go's imaged).
	Owns []Owned
	// Check is how its files are held to their bytes (check.go, scrub.go).
	Check Check
	// Part is the part of a repository a damage report names it by: one of
	// the parts gitproc.Damage.Drops answers. A report naming it is
	// EventReported for it.
	Part      gitproc.Part
	Freshness Freshness
	// What each Event does to it.
	OnPackAdded, OnPackRemoved, OnRefsMoved, OnReported Change
}

// On is what event e does to d.
func (d Derived) On(e Event) Change {
	switch e {
	case EventPackAdded:
		return d.OnPackAdded
	case EventPackRemoved:
		return d.OnPackRemoved
	case EventRefsMoved:
		return d.OnRefsMoved
	case EventReported:
		return d.OnReported
	}
	return ""
}

// The patterns of DerivedFiles, compiled once: a regular expression is
// immutable and safe to share, so every copy DerivedFiles answers holds the
// same ones.
var (
	midxMain   = regexp.MustCompile(`^multi-pack-index$`)
	midxBitmap = regexp.MustCompile(`^multi-pack-index-[0-9a-f]{40}\.bitmap$`)
	midxRev    = regexp.MustCompile(`^multi-pack-index-([0-9a-f]{40})\.rev$`)
	midxLock   = regexp.MustCompile(`^multi-pack-index\.lock$`)
	tmpBitmap  = regexp.MustCompile(`^tmp_bitmap_[A-Za-z0-9]+$`)
	graphMain  = regexp.MustCompile(`^commit-graph$`)
	graphLock  = regexp.MustCompile(`^commit-graph\.lock$`)
)

// DerivedFiles is the closed list, in the order a pass plans its steps and a
// line names them: a fresh copy each call, so no caller can change what
// another reads.
//
// The multi-pack-index's REVERSE INDEX is owned although git ≥ 2.41 keeps it
// as a chunk inside the index and writes no multi-pack-index-<cs>.rev (only
// under GIT_TEST_MIDX_WRITE_REV): a file git may write that the image did not
// admit would be trashed under a running reader. A bitmap's TEMPORARY FILE is
// objects/pack/tmp_bitmap_XXXXXX on every git from the floor (git's mkstemp in
// pack-bitmap-write.c), registered for no cleanup, so a writer killed while it
// writes the bitmap — the end of a run, where most of its time is — leaves it.
// A non-incremental `multi-pack-index write` writes no multi-pack-index.d/.
func DerivedFiles() []Derived {
	maintained := func(c Covers) time.Duration { return gitproc.PackWorkDeadline(c.DiskBytes) }
	return []Derived{{
		Name:   DerivedMidx,
		Inputs: []Input{InputPacks, InputRefs},
		Writes: []Write{
			{Step: StepIndex, Trigger: TriggerRound, Lane: gitproc.ClassRead,
				Deadline:       func(c Covers) time.Duration { return gitproc.PackWorkDeadline(c.IndexBytes) },
				BytesPerObject: IndexBytesPerObject, Writer: Writer{maintainFile, procIndex}, Args: roundIndexArgs,
				Holds: []FilePattern{{"objects/pack", midxLock}}},
			{Step: StepBitmap, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance, Deadline: maintained,
				BytesPerObject: BitmapBytesPerObject, Writer: Writer{maintainFile, procIndex}, Args: bitmapArgs,
				Holds: []FilePattern{{"objects/pack", midxLock}, {"objects/pack", tmpBitmap}}},
			{Step: StepIndex, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance, Deadline: maintained,
				BytesPerObject: IndexBytesPerObject, Writer: Writer{maintainFile, procIndex}, Args: plainArgs,
				Holds: []FilePattern{{"objects/pack", midxLock}}},
		},
		Owns: []Owned{
			{FilePattern: FilePattern{"objects/pack", midxMain}, File: "multi-pack-index", Main: true},
			{FilePattern: FilePattern{"objects/pack", midxBitmap}, File: "bitmap"},
			{FilePattern: FilePattern{"objects/pack", midxRev}, File: "midx-rev", Prior: true},
		},
		Check:       CheckTrailer,
		Part:        gitproc.PartMultiPackIndex,
		Freshness:   FreshDebounced,
		OnPackAdded: Keep, OnPackRemoved: Drop, OnRefsMoved: Keep, OnReported: Drop,
	}, {
		Name:   DerivedGraph,
		Inputs: []Input{InputPacks, InputRefs},
		Writes: []Write{{Step: StepGraph, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance,
			Deadline: maintained, BytesPerObject: GraphBytesPerObject, Writer: Writer{maintainFile, procGraph},
			Args: graphArgs, Holds: []FilePattern{{"objects/info", graphLock}}}},
		Owns:        []Owned{{FilePattern: FilePattern{"objects/info", graphMain}, File: "commit-graph", Main: true}},
		Check:       CheckTrailer,
		Part:        gitproc.PartCommitGraph,
		Freshness:   FreshDebounced,
		OnPackAdded: Keep, OnPackRemoved: Drop, OnRefsMoved: Keep, OnReported: Drop,
	}}
}

// Validate refuses a declaration that leaves anything unsaid, naming the
// declaration and the field.
func (d Derived) Validate() error {
	bad := func(field, format string, args ...any) error {
		return fmt.Errorf("derived file %q: %s %s", d.Name, field, fmt.Sprintf(format, args...))
	}
	if !d.Name.Valid() {
		return bad("Name", "is none of DerivedFiles' names")
	}
	if len(d.Inputs) == 0 {
		return bad("Inputs", "is empty: say what it is a function of")
	}
	for _, in := range d.Inputs {
		if !in.Valid() {
			return bad("Inputs", "holds %q, which is none of %v", in, Inputs)
		}
	}
	if len(d.Writes) == 0 {
		return bad("Writes", "is empty: say what writes it")
	}
	type key struct {
		trigger Trigger
		step    Step
	}
	seen := map[key]bool{}
	for i, w := range d.Writes {
		field := fmt.Sprintf("Writes[%d]", i)
		switch {
		case !w.Step.Valid():
			return bad(field+".Step", "is %q, none of %v", w.Step, Steps)
		case !w.Trigger.Valid():
			return bad(field+".Trigger", "is %q, none of %v", w.Trigger, Triggers)
		case !w.Lane.Valid():
			return bad(field+".Lane", "is %q, none of %v", w.Lane, gitproc.Classes)
		case w.Deadline == nil:
			return bad(field+".Deadline", "is nil: say how long its process may run")
		case w.BytesPerObject <= 0:
			return bad(field+".BytesPerObject", "is %d: say what an object costs it in memory", w.BytesPerObject)
		case len(w.Args) == 0:
			return bad(field+".Args", "is empty")
		case w.Args[0] != w.Writer.Subcommand:
			return bad(field+".Args", "begin %q, not its writer's subcommand %q", w.Args[0], w.Writer.Subcommand)
		case !writerFile(w.Writer.File):
			return bad(field+".Writer.File", "is %q: a clean slash path of a .go file under internal/code/",
				w.Writer.File)
		case seen[key{w.Trigger, w.Step}]:
			return bad(field, "is a second %s write of step %s", w.Trigger, w.Step)
		}
		for j, p := range w.Holds {
			if err := p.validate(); err != nil {
				return bad(fmt.Sprintf("%s.Holds[%d]", field, j), "%v", err)
			}
		}
		seen[key{w.Trigger, w.Step}] = true
	}
	if len(d.Owns) == 0 {
		return bad("Owns", "is empty: say which files it is")
	}
	mains := 0
	for i, o := range d.Owns {
		field := fmt.Sprintf("Owns[%d]", i)
		if err := o.FilePattern.validate(); err != nil {
			return bad(field, "%v", err)
		}
		switch {
		case o.File == "":
			return bad(field+".File", "is empty: say what a check of it is counted as")
		case o.Main:
			mains++
			if _, whole := o.Name.LiteralPrefix(); !whole {
				return bad(field+".Name", "%q is no literal, and the main file is one name", o.Name)
			}
		}
		if o.Prior && o.Name.NumSubexp() != 1 {
			return bad(field+".Name", "%q has %d capture groups, and a name carrying its checksum has one: "+
				"the forty hex digits", o.Name, o.Name.NumSubexp())
		}
	}
	if mains != 1 {
		return bad("Owns", "has %d main files, and a declaration has one: the file whose presence is its", mains)
	}
	if !d.Check.Valid() {
		return bad("Check", "is %q, none of %v", d.Check, Checks)
	}
	if d.Part == 0 || (&gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: d.Part}).Drops() != d.Part {
		return bad("Part", "is %v, which is no part a damage report drops (gitproc.Damage.Drops)", d.Part)
	}
	if !d.Freshness.Valid() {
		return bad("Freshness", "is %q, none of %v", d.Freshness, Freshnesses)
	}
	for _, e := range Events {
		if !d.On(e).Valid() {
			return bad("On("+string(e)+")", "is %q, none of %v", d.On(e), Changes)
		}
	}
	return nil
}

// writerFile reports whether f names a Go file under internal/code the way
// the writers gate names one: slash-separated, clean, relative to the module
// root.
func writerFile(f string) bool {
	return path.Clean(f) == f && !strings.Contains(f, `\`) && strings.HasPrefix(f, "internal/code/") &&
		strings.HasSuffix(f, ".go")
}

// validate refuses a pattern that does not name files of the incarnation by
// their whole names.
func (p FilePattern) validate() error {
	switch {
	case p.Dir == "" || path.Clean(p.Dir) != p.Dir || strings.Contains(p.Dir, `\`) || path.IsAbs(p.Dir) ||
		p.Dir == "." || p.Dir == ".." || strings.HasPrefix(p.Dir, "../"):
		return fmt.Errorf("has Dir %q, which is no clean slash path of a directory inside the incarnation", p.Dir)
	case p.Name == nil:
		return errors.New("has no Name")
	case !anchored(p.Name):
		return fmt.Errorf("has Name %q, which is not anchored ^…$: it would match part of a name", p.Name)
	}
	return nil
}

// anchored reports whether re matches only whole strings: its parsed form a
// concatenation that begins at the start of the text and ends at its end — so
// `^a|b$`, which spells both anchors, is not.
func anchored(re *regexp.Regexp) bool {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return false
	}
	parsed = parsed.Simplify()
	if parsed.Op != syntax.OpConcat || len(parsed.Sub) < 2 {
		return false
	}
	return parsed.Sub[0].Op == syntax.OpBeginText && parsed.Sub[len(parsed.Sub)-1].Op == syntax.OpEndText
}

// validateAll is every declaration's Validate, and what holds only across
// them: names unique; no two maintainer writes of one Step, since the refusal
// counter and gauge are keyed by step; every file a declaration owns or holds
// under objects/ — the root is the repository's own fixed files, and refs/
// stays empty (a loose ref would outrank packed-refs for every reader); and
// every part a damage report may drop (gitproc.Damage.Drops) some
// declaration's, or that damage would drop nothing. Open refuses a cache
// whose declarations fail it, so a build whose declarations are wrong never
// opens one — and TestEveryDerivedFileIsDeclared catches it first.
//
// THE ROOT RULE IS WHAT LETS A BOOT WALK objects/ ALONE: a copy with no state
// is counted by its files under objects/ and its packed-refs
// (measureIncarnation), which counts every derived file only because this
// rule keeps them there. A root admitted here is a root that walk must read
// too, and the two change together.
func validateAll(decls []Derived) error {
	names := map[DerivedName]bool{}
	steps := map[Step]DerivedName{}
	var parts gitproc.Part
	for _, d := range decls {
		if err := d.Validate(); err != nil {
			return err
		}
		if names[d.Name] {
			return fmt.Errorf("derived file %q is declared twice", d.Name)
		}
		names[d.Name] = true
		for _, w := range d.Writes {
			if w.Trigger != TriggerMaintainer {
				continue
			}
			if other, ok := steps[w.Step]; ok {
				return fmt.Errorf("derived files %q and %q each declare a maintainer write of step %s: the "+
					"refusal counter and gauge are keyed by step", other, d.Name, w.Step)
			}
			steps[w.Step] = d.Name
		}
		for _, p := range d.patterns() {
			if p.Dir != "objects" && !strings.HasPrefix(p.Dir, "objects/") {
				return fmt.Errorf("derived file %q has files in %s, and a derived file lives under objects/: the "+
					"root holds the repository's own files, refs/ stays empty since a loose ref outranks "+
					"packed-refs for every reader, and a boot counts a copy with no state by objects/ and "+
					"packed-refs", d.Name, p.Dir)
			}
		}
		parts |= d.Part
	}
	for p := gitproc.Part(1); p != 0; p++ {
		damage := &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: p}
		if damage.Derived() && damage.Drops()&^parts != 0 {
			return fmt.Errorf("a damage report naming %v drops %v, and no derived file is declared by %v",
				p, damage.Drops(), damage.Drops()&^parts)
		}
	}
	return nil
}
