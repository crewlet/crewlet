package cache_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// TestNothingButSyncWritesACache holds the one rule a cache rests on: a cache
// is a FUNCTION OF ROWS — its refs are exactly the repository's code_refs at
// the position it records and its packs exactly the code_packs there — and
// the cache's own steps are what write it, by convergence. Its two halves
// hold the rule from the two sides it can break on.
//
// STATIC: no file under internal/code writes the filesystem, or runs a git
// process that may write the repository it runs in, unless this test's
// allowance names the file, what it does and why. A new writer is therefore
// an argued diff rather than an accident that serves an object no row names
// (writersStatic says exactly what is read and what is not).
//
// INTERLEAVINGS: after any interleaving of what a node's cache meets —
// pushes, tags, deletions, refused pushes, adoptions, syncs at every level,
// syncs killed mid-step, restarts, evictions, rebuilds, maintenance, swept
// tombstones, a log recreated backwards, compactions and the swaps that take
// their packs out, killed or crashed mid-way, publications by this node and
// by another, the installer's passes, layers the store lost or spoiled, and
// the node's tip — every cache equals a cache rebuilt from nothing at the
// same position, its installed chain a prefix of the rows', and no file under
// repos/ holds a pack a refused push uploaded (interleavings_test.go).
func TestNothingButSyncWritesACache(t *testing.T) {
	t.Parallel()
	t.Run("static", func(t *testing.T) {
		t.Parallel()
		writersStatic(t)
	})
	t.Run("interleavings", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		ran, reached := 0, map[string]bool{}
		// Once every seed has run: an outcome none of them reached is an
		// operation the half claims and never exercises.
		t.Cleanup(func() {
			if ran != interleavingSeeds || t.Failed() {
				return
			}
			for _, o := range interleavingOutcomes {
				if !reached[o] {
					t.Errorf("no seed reached %q: %d seeds of %d steps do not exercise it — "+
						"draw more of them rather than drop the operation", o, interleavingSeeds, interleavingSteps)
				}
			}
		})
		for seed := uint64(1); seed <= interleavingSeeds; seed++ {
			t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
				t.Parallel()
				got := interleave(t, seed)
				mu.Lock()
				defer mu.Unlock()
				ran++
				for o := range got {
					reached[o] = true
				}
			})
		}
	})
}

// THE STATIC HALF.
//
// # Where it looks
//
// Every .go file under internal/code that is not a _test.go file, but
// testdata, which is not compiled, and every directory whose name ends in
// "test" — the tree's test harnesses, gittest among them, which write
// fixtures by design and serve no cache. Every file is read WHOLE, in its
// package's scope and wherever it is built, through
// internal/sourcetree/decltype — the one walk that types a value from the
// declarations it reaches — because a value can be a Cmd in a file that
// imports nothing: an alias of Cmd declared in the file beside it, a Cmd a
// function of another file returns. A file the walk cannot parse fails the
// walk rather than passing unjudged.
//
// # What it flags
//
//   - A FILESYSTEM MUTATION: a selector, through the file's own import name
//     (a renamed import included, a local variable of the same name not), of
//     a function that creates, renames, links, removes, truncates or changes
//     the metadata of a path — the os, io/ioutil and syscall/unix names in
//     fsMutators — called or taken as a value. A write through an *os.File
//     or an *os.Root needs one of the opens that make it (os.Create,
//     os.OpenFile, os.CreateTemp, os.OpenRoot), which are matched. A dot
//     import of one of those packages is a mutation in itself: its calls
//     carry no qualifier to judge.
//   - A CMD LITERAL THAT MAY WRITE: every composite literal whose type is
//     gitproc's Cmd — named by gitproc, by an alias of Cmd or by a type Cmd
//     defines, or elided inside a literal of a collection of Cmds or of
//     pointers to them, at any depth — is judged by its Args. A []string
//     literal (or an append to one) whose first element is a string literal
//     naming one of gitReaders, spelling none of that reader's writing
//     option and holding nothing the walk cannot read beside one, is a read
//     and passes; any other subcommand is a writer, and Args the literal does
//     not spell — a variable, a call, a constant first word, a literal
//     without field names — are arguments built at run time, which the
//     allowance must argue for like a writer. A literal with no Args passes:
//     handed straight to the pool it runs nothing (Run refuses a Cmd with no
//     subcommand before any process starts), and given Args anywhere else it
//     reaches git only by being handed on, which the next rule judges. The
//     reader set is CLOSED: a subcommand this test has not heard of is a
//     writer until it is argued to be a read, because the dangerous ones do
//     not announce themselves — merge-tree --write-tree, pack-objects with a
//     base name, hash-object -w, archive --output and rev-list --output each
//     write objects or files into wherever they run. A gitproc imported with
//     a dot is a hit in itself: decltype does not follow a dot import, so
//     its Cmd is one the walk cannot type.
//   - A CMD HANDED ON THAT ITS CALL DOES NOT SPELL: a Cmd reaches git only
//     by being handed to a call — the pool's Run, or whatever stands between
//     the code that builds it and Run: a helper, an interface, a function
//     value, a method it is the receiver of — so every argument that is a
//     Cmd, by the parameter it is handed to or by its own type, and every
//     Cmd a method is selected on (called there, or kept as a method value),
//     must be a Cmd literal spelled there (behind & or a conversion between
//     Cmd types included), which the rule above has judged. Any other is
//     arguments built at run time, however it was built — Args assigned
//     after the literal, a Cmd from new, an element of Args overwritten, a
//     helper that changed them in place, a conversion from another struct of
//     the same fields — because what its Args hold by the time it is handed
//     on is decided outside every literal the walk reads, and this is the
//     one place all of those meet. A Cmd handed to a call is a hit whatever
//     the call is — a builtin, a log line — since the walk does not follow
//     where a call takes it.
//   - The one handoff passed is GITPROC'S OWN PLUMBING passing on the Cmd its
//     function was handed: from a file of gitproc, to one of gitproc's
//     unexported functions or methods (never a variable of a function type,
//     which may hold any function), a PARAMETER of a function the call is in
//     (its own, or one a closure it is in shares) — never a named result,
//     which the function builds, nor a receiver — handed singly to a
//     parameter its callee declares as a Cmd: never into a variadic one, one
//     of an interface type or one of a type parameter's. Both ends are
//     therefore Cmd parameters, which the next rule holds unchanged, so the
//     Cmd arrives as its caller handed it, and a Cmd gitproc builds for
//     itself is judged here like any other.
//   - A CMD GITPROC WAS HANDED, CHANGED: in gitproc's files, every use of a
//     Cmd parameter — closures included, matched by the object it declares
//     rather than by its name, which a local may shadow — that writes it,
//     its Args or an element of them (an assignment, an operator
//     assignment, a range clause, or the slice handed first to copy, clear
//     or append), takes the address of any of those, or copies the Cmd or
//     its Args anywhere but into a call (a copy shares the Args' elements),
//     followed through parentheses, a dereference, slicing and a conversion
//     that keeps it what it was. Its other fields are the pool's to set: a
//     Cmd is judged by its Args. Only gitproc is judged so, because only
//     gitproc runs git without handing a Cmd to a call — its one exec, held
//     by TestOnlyGitprocExecsGit, takes a parameter's Args — so a Cmd it was
//     handed and changed may run whether or not it is handed on; anywhere
//     else a Cmd reaches git only by being handed on, where it must be
//     spelled.
//   - A VALUE THE WALK CANNOT TYPE, handed to a call whose parameter it
//     cannot type either — a type switch's variable, decltype's own
//     boundary — because "cannot tell" is not "not a Cmd". What can be no
//     Cmd passes unread: nil, a constant (no constant is a struct), the type
//     make or new is handed, and a value of a type parameter, which is a Cmd
//     only once a conversion or an assertion makes it one — a typed
//     expression the rule above judges wherever it is handed on.
//   - And, in a file that runs git, every []string literal whose first
//     element names a git writer (gitWriterWords) wherever it is assembled,
//     so an argument list built away from its Cmd is named by its
//     subcommand rather than only as arguments built at run time.
//
// # Coverage boundary
//
// It sees the call that touches the disk and the argv git is handed, in the
// file that holds them, and nothing that reaches either another way:
//
//   - a write through one of a package's OWN helpers is that helper's file's
//     row — durable.go's stage and place write the state, packed-refs, HEAD,
//     config, a skeleton's files and the hook script that state.go, refs.go,
//     sweep.go, entry.go and open.go decide, and layout.go's toTrash is every
//     rename into trash/ — so which code calls a helper is the interleavings
//     half's to judge, by what the directory ends up holding;
//   - a path laundered through another package (flock's lock file,
//     hostbox) or reached by reflection, unsafe or cgo;
//   - a Cmd that comes back from a function of a package the walk does not
//     read — one of the standard library's generic functions, cmp.Or among
//     them — and is handed straight to a callee it cannot type: a value only
//     an unread package can type is taken for no Cmd, which nothing outside
//     this module can name;
//   - inside gitproc, what a function does with a Cmd parameter's Args once
//     they are handed to it — run hands them to exec.Command, prepare to its
//     checks — or once converted to a type of a package the walk does not
//     read, which only its declaration could tell from a function: the walk
//     does not follow a call, so the pool running exactly the Cmd its Run is
//     handed is gitproc's contract past that point, its own suite's to hold;
//   - a file written by a child process: git's own writes are judged by the
//     subcommand it was handed, not by what it then opens.
//
// The declared half ties each WRITER to a declaration of derived.go — its file,
// its subcommand and its argument list's literal — and never the FILES that
// writer leaves, which a static walk cannot see because a child process
// writes them: those are held to the declarations by
// TestEveryDerivedFileIsDeclared, which runs real git and reads what it left.
//
// If the rule a cache rests on ever goes away, DELETE this half rather than
// widening the allowance: an allowance kept past its subject is how a list
// of excuses quietly grows.
func writersStatic(t *testing.T) {
	found, err := codeWalk()
	if err != nil {
		t.Fatalf("walking internal/code: %v", err)
	}
	if found.cacheFiles == 0 {
		t.Fatalf("scanned %d files and none of internal/code/cache — the walk certified nothing; "+
			"check the module root and the internal/code start", found.files)
	}
	if found.cmds == 0 {
		t.Fatalf("scanned %d files and typed no gitproc.Cmd literal — the cache runs git, so the typing of "+
			"Cmd has gone inert and no git process was judged; check gitproc's Cmd and decltype", found.files)
	}

	// A guard asserting an ABSENCE passes identically when the thing is
	// absent and when the matcher has gone inert, so the matcher is held to
	// verdicts known in advance first.
	controlWriters(t)

	allowed := map[writerKey]bool{}
	for _, a := range cacheWriters {
		allowed[writerKey{a.file, a.kind}] = true
	}
	seen := map[writerKey]bool{}
	mutations, writers := 0, 0
	for _, h := range found.hits {
		k := writerKey{h.file, h.kind}
		seen[k] = true
		if h.kind == kindOS {
			mutations++
		} else {
			writers++
		}
		if allowed[k] {
			continue
		}
		t.Errorf("%s: %s %s outside the files allowed to.\n"+
			"\tA cache is written by its own steps alone, by convergence to its rows (internal/code/cache's "+
			"package doc); a new writer under internal/code is an entry in this test's allowance "+
			"(cacheWriters) with the reason it cannot go through the cache", h.pos, h.file, h.doing())
	}
	// Exact in BOTH directions: an entry whose site is gone must go too, or
	// the list becomes somewhere a future writer hides behind a stale excuse.
	for _, a := range cacheWriters {
		if !seen[writerKey{a.file, a.kind}] {
			t.Errorf("cacheWriters still has an entry for %s (%s), but the walk finds no such write there. "+
				"Delete the entry — it was: %s", a.file, a.kind, a.why)
		}
	}
	// THE DECLARED HALF: every git writer of the cache is a declared write of
	// a derived file, and every declared write's writer is a row — so a
	// derived file is written by nothing its declaration does not name.
	decls := cache.DerivedFiles()
	for _, problem := range derivedHalf(decls, cacheWriters, found.argv) {
		t.Error(problem)
	}
	if !t.Failed() {
		writes := 0
		for _, d := range decls {
			writes += len(d.Writes)
		}
		t.Logf("scanned %d files: %d mutations, %d git writers, %d Cmd literals judged, %d Cmds handed to a call, "+
			"every one allowed; %d declared writes of %d derived files tied to their rows", found.files, mutations,
			writers, found.cmds, found.handed, writes, len(decls))
	}
}

// derivedHalf holds the allowance's rows to the cache's declarations of its
// derived files (cache.DerivedFiles) in both directions, over the argument
// lists each file that runs git spells (argv), and answers everything that
// breaks:
//
//  1. a declared write whose writer — its file and git subcommand — has no
//     row naming its declaration: a declaration naming a writer that does
//     not exist;
//  2. a row running one git subcommand in the cache that names no
//     declaration: every git writer in a cache writes a derived file, or it
//     writes what no row names;
//  3. a row naming a declaration with no write in the row's file — for a
//     row of one git subcommand, none with that subcommand;
//  4. a row naming a name DerivedFiles does not hold;
//  5. a row naming a declaration in a file that is no declaration's writer;
//  6. a declared write whose argument list is no list its writer's file
//     spells whole, element for element: Args are read where git runs them,
//     or what a write hands git is a value the gate never read.
//
// A pure function over values, so its control (controlWriters) judges
// declarations and rows it makes up.
func derivedHalf(decls []cache.Derived, rows []writerRow, argv map[string][][]string) []string {
	var out []string
	names := map[cache.DerivedName]*cache.Derived{}
	writers := map[string]bool{}
	for i := range decls {
		names[decls[i].Name] = &decls[i]
		for _, w := range decls[i].Writes {
			writers[w.Writer.File] = true
		}
	}
	// Rule 1, and rule 6. An IN-PROCESS write (no subcommand: the cache's own
	// Go code writes the file, Write.Charge) runs no git, so its row is its
	// file's filesystem row, and it has no argument list to spell.
	for _, d := range decls {
		for _, w := range d.Writes {
			if w.Writer.Subcommand == "" {
				if !slices.ContainsFunc(rows, func(r writerRow) bool {
					return r.file == w.Writer.File && r.kind == kindOS && slices.Contains(r.derived, d.Name)
				}) {
					out = append(out, fmt.Sprintf("derived file %q declares an in-process %s write of step %s by "+
						"%s, and no row of cacheWriters (%s, %s) names it: the declaration names a writer that does "+
						"not exist, or the row forgot what it writes", d.Name, w.Trigger, w.Step, w.Writer.File,
						w.Writer.File, kindOS))
				}
				continue
			}
			kind := kindGit + " " + w.Writer.Subcommand
			if !slices.ContainsFunc(rows, func(r writerRow) bool {
				return r.file == w.Writer.File && r.kind == kind && slices.Contains(r.derived, d.Name)
			}) {
				out = append(out, fmt.Sprintf("derived file %q declares a %s write of step %s by %s running git %s, "+
					"and no row of cacheWriters (%s, %s) names it: the declaration names a writer that does not "+
					"exist, or the row forgot what it writes", d.Name, w.Trigger, w.Step, w.Writer.File,
					w.Writer.Subcommand, w.Writer.File, kind))
			}
			if !slices.ContainsFunc(argv[w.Writer.File], func(a []string) bool { return slices.Equal(a, w.Args) }) {
				out = append(out, fmt.Sprintf("derived file %q declares a %s write of step %s whose argument list "+
					"%q %s does not spell whole: a write's Args are one of its writer file's own literals, so this "+
					"gate reads what git is handed where git runs", d.Name, w.Trigger, w.Step, w.Args,
					w.Writer.File))
			}
		}
	}
	for _, r := range rows {
		sub, oneSub := strings.CutPrefix(r.kind, kindGit+" ")
		oneSub = oneSub && r.kind != kindRuntime
		// Rule 2.
		if oneSub && strings.HasPrefix(r.file, "internal/code/cache/") && len(r.derived) == 0 {
			out = append(out, fmt.Sprintf("%s runs git %s in the cache and its row names no declaration of a "+
				"derived file: every git writer of a cache writes a derived file, declared in derived.go, or it "+
				"writes what no row names", r.file, sub))
		}
		for _, name := range r.derived {
			d, ok := names[name]
			switch {
			case !ok: // Rule 4.
				out = append(out, fmt.Sprintf("the row (%s, %s) names derived file %q, which DerivedFiles does not "+
					"declare", r.file, r.kind, name))
			case !writers[r.file]: // Rule 5.
				out = append(out, fmt.Sprintf("the row (%s, %s) names derived file %q, and %s is no declaration's "+
					"writer", r.file, r.kind, name, r.file))
			case !slices.ContainsFunc(d.Writes, func(w cache.Write) bool {
				return w.Writer.File == r.file && (!oneSub || w.Writer.Subcommand == sub)
			}): // Rule 3.
				out = append(out, fmt.Sprintf("the row (%s, %s) names derived file %q, which declares no write by "+
					"that file of that kind", r.file, r.kind, name))
			}
		}
	}
	return out
}

// The kinds of write a hit is, and an allowance entry names: kindOS for a
// filesystem mutation, kindGit+" "+subcommand for a git writer, and
// kindRuntime for git handed arguments the walk cannot read where they are
// spelled.
const (
	kindOS      = "os"
	kindGit     = "git"
	kindRuntime = "git (arguments built at run time)"
)

// writerRow is one entry of the allowance: a file under internal/code that
// may write the filesystem or run a git writer, the kind of write, why it
// cannot go through the cache's own steps, and the declarations of derived
// files (cache.DerivedFiles) its writes are writes of — none for a write that
// makes no derived file. derivedHalf holds the last field to the
// declarations in both directions.
type writerRow struct {
	file, kind, why string
	derived         []cache.DerivedName
}

// cacheWriters is the closed set of files under internal/code that may write
// the filesystem or run a git writer, each with what it does and why it
// cannot go through the cache's own steps. Exact in both directions.
var cacheWriters = []writerRow{
	{"internal/code/gitproc/gitproc.go", kindOS,
		"HOME for every git process, created and kept empty and private by the pool (gitproc.New)", nil},
	{"internal/code/cache/layout.go", kindOS,
		"Prepare: the cache directory, its fixed subdirectories and CACHEDIR.TAG, written in place before " +
			"tmp/ exists; and toTrash, the one rename of anything into trash/ — a leftover home/, a boot's " +
			"scratch/ and tmp/, a swept entry, a retired or evicted incarnation, a removed scratch", nil},
	{"internal/code/cache/open.go", kindOS,
		"Open: repos/, trash/ and hooks/ made sure of, and scratch/ and tmp/ made anew once the boot has " +
			"moved the old ones into trash/", nil},
	{"internal/code/cache/durable.go", kindOS,
		"stage and place: every file a cache writes but the tag — the state, packed-refs, HEAD, config, " +
			"the skeleton's files and the hook script — staged in tmp/ and renamed over its destination, " +
			"and a staging given up on removed", nil},
	{"internal/code/cache/entry.go", kindOS,
		"a new incarnation's directory and its skeleton's subdirectories", nil},
	{"internal/code/cache/install.go", kindOS,
		"a download's three files in tmp/, renamed in the order git reads a pack by into objects/pack — or, " +
			"for a pack only rows an expunge's begin left dropping name, into the dropping view, made with the " +
			"first pack placed there (ensureView) — and what a failed download left removed", nil},
	{"internal/code/cache/sweep.go", kindOS,
		"the image's directories made again where a full round's sweep found them missing", nil},
	{"internal/code/cache/evict.go", kindOS,
		"the janitor: trash/ emptied, and a repository's last use stamped on its state file's " +
			"modification time — by its tick, and on every state written (stampUse, state.go's writeState)",
		nil},
	{"internal/code/cache/maintain.go", kindOS,
		"the files a declared write holds while it runs (Write.Holds): what a maintenance run that did " +
			"not succeed — the node's tip's lock and temporary layer among them — or a round's index that " +
			"failed, leaves behind",
		[]cache.DerivedName{cache.DerivedMidx, cache.DerivedGraph, cache.DerivedGraphLayers}},
	{"internal/code/cache/maintain.go", kindGit + " multi-pack-index",
		"the declared writes of the multi-pack-index — a full round's without a bitmap (indexRound) and the " +
			"maintainer's with one and without — in one file so the writer of an index is one file",
		[]cache.DerivedName{cache.DerivedMidx}},
	{"internal/code/cache/maintain.go", kindGit + " commit-graph",
		"the declared writes of the maintainer's commit-graph — own mode's whole graph, and the node's tip " +
			"layer over an installed chain (writeTip) — in one file so the writer of a graph is one file",
		[]cache.DerivedName{cache.DerivedGraph, cache.DerivedGraphLayers}},
	{"internal/code/cache/maintain.go", kindRuntime,
		"indexRound, runMaintenance and writeTip run declared writes (derived.go), whose argument lists are " +
			"this file's five literals above: the round's index, the maintainer's with its bitmap and without " +
			"one, own mode's graph and the node's tip",
		[]cache.DerivedName{cache.DerivedMidx, cache.DerivedGraph, cache.DerivedGraphLayers}},
	{"internal/code/cache/scratch.go", kindOS,
		"a push's scratch repository, a merge's and an advertisement view under scratch/: their directories, " +
			"HEAD, config and alternates, a view's packed-refs linked from its incarnation, and a merge's store " +
			"of exactly the packs it merges, their files linked from its incarnation; a publication's scratch " +
			"(NewLayerScratch): its directories, HEAD and config, its incarnation's packed-refs, every pack file " +
			"and the base layers' files HARD-LINKED — the same inodes, written by nothing here — the two chain " +
			"files naming exactly that base, and its repack's outputs linked in from the merge's scratch they " +
			"were built in (Gather); and a request's spool (Spool), created there and unlinked at once, so it " +
			"is no file of the cache's", nil},
	{"internal/code/cache/adopt.go", kindOS,
		"the receiving node's own pack, once its row has applied: linked into tmp/ and from there into " +
			"objects/pack, and its staging names removed; and a maintainer's own published layer, once its " +
			"record has applied, linked from its scratch into tmp/ for the installer, which places it " +
			"(layers.go)", nil},
	{"internal/code/cache/layers.go", kindOS,
		"the installed published chain: a layer's three files staged in tmp/ from its parts or from its " +
			"maintainer's own, made durable and renamed — or, kept across a swap or a rebuild, linked — into " +
			"an incarnation, the two chain files written from the rows (staged, synced, renamed) — the " +
			"commit-graph's ending in the node's tip where one is held, and written over the installed layers " +
			"alone before the maintainer writes the tip over them (writeChain) — and a placement given up on, " +
			"a layer cut or the tip dropped moved into trash/: a function of the code_layers rows the " +
			"incarnation installed from", []cache.DerivedName{cache.DerivedLayers, cache.DerivedGraphLayers}},
	{"internal/code/cache/provenance.go", kindOS,
		"the index of pushes, the cache's own derived file under the incarnation's crewlet/: the directory " +
			"made for its first run, and a rebuild's links of the runs of the copy it replaces into its own — " +
			"every run itself staged in tmp/ and renamed in through durable.go, and retired through layout.go's " +
			"toTrash", []cache.DerivedName{cache.DerivedProvenance}},
	{"internal/code/cache/memo.go", kindOS,
		"the memo of answers by object id, the cache's own derived file under the incarnation's crewlet/memo: " +
			"each answer staged in tmp/ — with no sync, since every read holds an entry to its digest — " +
			"completed with its run's tail and renamed into the tree it makes, the tree of the copy a rebuild " +
			"replaces or a swap leaves moved whole into the copy it promotes, an entry that fails its digest or " +
			"is trimmed removed, and the tree moved whole through layout.go's toTrash",
		[]cache.DerivedName{cache.DerivedMemo}},
	{"internal/code/cache/swap.go", kindOS,
		"a swap's next incarnation, staged under scratch/: its directory, every incarnation's directories " +
			"and those of what it links — its dropping view among them, made for the first dropping pack it " +
			"links — every pack's three files where the live set places them — and every pack its installed " +
			"chain names — HEAD, config, the packed-refs and each derived file a retirement keeps but the memo and " +
			"the chain HARD-LINKED from the incarnation it replaces — the same inodes, written by nothing " +
			"here — and the whole renamed into repos/ under its number; its state goes through durable.go, " +
			"the memo's tree moves whole through memo.go, and the chain is placed through layers.go, the " +
			"node's tip a retirement keeps linked beside its layers", nil},
	{"internal/code/cache/background.go", kindRuntime,
		"Admission.Run hands a compaction's build and a publication's — internal/code/compact's pack-objects, " +
			"multi-pack-index and commit-graph literals, which their own rows judge — unchanged to the maintenance " +
			"slot its admission reserved (gitproc.Reserved.Run): each build runs in its own scratch, never in an " +
			"incarnation", nil},
	{"internal/code/compact/build.go", kindGit + " pack-objects",
		"a compaction's build: ONE `pack-objects --stdin-packs` in the compaction's own SCRATCH " +
			"(cache.NewMergeScratch), its inputs read through the scratch's one alternate — a store of exactly " +
			"them, linked from the incarnation — and a publication's repack, the same, in a merge's scratch of its " +
			"own, its outputs then gathered into the publication's scratch of links (cache.Scratch.Gather); the " +
			"outputs written into the scratch alone, never an incarnation, entering the cache only by Adopt, " +
			"once their rows have applied", nil},
	{"internal/code/compact/compactor.go", kindRuntime,
		"runInSlot hands the build's one pack-objects Cmd — build.go's literal — to the admission's reserved " +
			"slot (cache.Admission.Run), unchanged: a function value, so a suite can stand in a builder", nil},
	{"internal/code/compact/publish.go", kindGit + " multi-pack-index",
		"a publication's layer: `multi-pack-index write --incremental --bitmap --no-write-chain-file` over the " +
			"layer's packs, in the publication's own SCRATCH of links (cache.NewLayerScratch), whose chain file " +
			"names exactly the layer's base — never an incarnation; the layer enters the cache only by " +
			"AdoptLayer, once its record has applied, or by the installer's fetch", nil},
	{"internal/code/compact/publish.go", kindGit + " commit-graph",
		"a publication's commit-graph layer: `commit-graph write --stdin-packs --split=no-merge` over the " +
			"layer's packs in the same scratch, its chain naming exactly the base's graph layers — never an " +
			"incarnation; it enters the cache with the layer", nil},
	{"internal/code/host/receive.go", kindGit + " receive-pack",
		"a push, received in its SCRATCH repository (cache.NewScratch) — never in a cache, since git " +
			"migrates a push's objects into the repository it runs in before any hook can refuse them; " +
			"the pack enters the cache only by Adopt, once its row has applied", nil},
	{"internal/code/host/advertise.go", kindGit + " receive-pack",
		"a push's advertisement (`--advertise-refs`, which writes nothing), run in an advertisement view " +
			"under scratch/ (cache.AdvertiseView), never in a cache", nil},
	{"internal/code/host/build.go", kindGit + " hash-object",
		"an engine write's new files, hashed into its own SCRATCH (cache.NewScratch, named its push id), never a " +
			"cache: an object the copy already holds is FRESHENED in the alternate — its pack's modification " +
			"time, not a byte — which is no part of a copy's image nor of a pack's identity (check.go's fileID)",
		nil},
	{"internal/code/host/build.go", kindGit + " mktree",
		"an engine write's touched directories, written into its scratch a level at a time; freshening as above", nil},
	{"internal/code/host/build.go", kindGit + " commit-tree",
		"an engine write's commit, written into its scratch", nil},
	{"internal/code/host/build.go", kindGit + " merge-tree",
		"a revert's or a cherry-pick's merged tree (`--write-tree`), and its conflicted files, written into its " +
			"scratch; freshening as above", nil},
	{"internal/code/host/build.go", kindGit + " pack-objects",
		"an engine write's one pack (`--stdout`), of its scratch's loose objects into a file staged in its " +
			"scratch — pack-objects writes a repository only with a base name, so the row is argued for what " +
			"the build does with its output", nil},
	{"internal/code/host/build.go", kindGit + " index-pack",
		"an engine write's pack (`--strict --stdin`) indexed into its scratch's objects/pack under the relay's " +
			"own fsck; it enters a cache only by Adopt, once its row has applied", nil},
	{"internal/code/host/build.go", kindOS,
		"an engine write's staging under its scratch's write/: its new files, its edited files, the directories " +
			"it touches as git stores them, each level's input to mktree and its pack — removed with the scratch",
		nil},
	{"internal/code/host/scan.go", kindRuntime,
		"rev-list over a push's new values in its scratch — the path of a blob over the limit and the " +
			"commits the push introduces — whose argument lists are this file's literals and object ids", nil},
	{"internal/code/host/relay.go", kindOS,
		"the hook relay's private socket directory under TMPDIR, made at Start and removed at Stop: no " +
			"path of the cache's", nil},
	{"internal/code/query/diff.go", kindGit + " diff-tree",
		"a seat's read of a diff or an interdiff — its stat's raw records, with their counts where the stat " +
			"admits every file, and the raw record of the one file a patch names — whose only arguments not " +
			"spelled as literals are the two whole object ids a read resolved and `-M`; a path never reaches " +
			"the command line (the stat is filtered after git answers, an interdiff's by its touched paths, and " +
			"the records it admits reach diff-pairs on its standard input), and " +
			"query.TestAPathReachesGitOnlyOnStandardInput holds every argument list to its template, so no " +
			"--output can be added unseen", nil},
	{"internal/code/query/compare.go", kindGit + " rev-list",
		"a seat's comparison of two commits: `--left-right --count <base>...<head>` over two whole commit " +
			"ids a read resolved; query.TestAPathReachesGitOnlyOnStandardInput holds the argument list to its " +
			"template", nil},
	{"internal/code/query/facts.go", kindGit + " diff-tree",
		"a revision's facts: the paths a change touches (`-r --name-only -z --no-renames`) and its patch for " +
			"patch-id (`-r -p --full-index …`, written to the reader through its Stdout, into a spool the cache " +
			"made with no name), whose only arguments not spelled as literals are the base and head the read " +
			"resolved to whole ids — the empty tree's id, a constant, for a history sharing none with its " +
			"target's; query.TestAPathReachesGitOnlyOnStandardInput holds both argument lists to their " +
			"templates, so no --output can be added unseen", nil},
}

type writerKey struct{ file, kind string }

// fsMutators is, for each package that can write the filesystem, the names
// in it that create, rename, link, remove, truncate or change the metadata
// of a path.
var fsMutators = map[string]map[string]bool{
	"os": setOf("Create", "CreateTemp", "OpenFile", "WriteFile", "Rename", "Remove", "RemoveAll", "Link",
		"Symlink", "Mkdir", "MkdirAll", "MkdirTemp", "Chmod", "Chown", "Lchown", "Chtimes", "Truncate",
		"CopyFS", "OpenRoot"),
	"io/ioutil":             setOf("WriteFile", "TempFile", "TempDir"),
	"syscall":               syscallMutators,
	"golang.org/x/sys/unix": syscallMutators,
}

var syscallMutators = setOf("Rename", "Renameat", "Renameat2", "Unlink", "Unlinkat", "Link", "Linkat",
	"Symlink", "Symlinkat", "Mkdir", "Mkdirat", "Mknod", "Mknodat", "Rmdir", "Open", "Openat", "Openat2",
	"Creat", "Truncate", "Chmod", "Fchmodat", "Chown", "Lchown", "Fchownat", "Utimes", "UtimesNano",
	"UtimesNanoAt", "Lutimes", "Setxattr", "Lsetxattr", "Removexattr", "Lremovexattr")

// gitReader is a git subcommand that never writes the repository it runs in,
// given the pool's fixed configuration — unless handed writes, the one
// option that makes it a writer, always a long option. Every spelling git
// may accept of it is refused (spellsOption): its name — what comes before
// any "=" — whole or an unambiguous prefix of it, with the value attached or
// as the next argument. fsck's option parser takes a prefix (`fsck --lost`
// is --lost-found); the revision walker under rev-list does not (`rev-list
// --outp` exits 129) — refused for both, since refusing a spelling git
// itself refuses costs nothing and one rule cannot then be the wrong one for
// a reader. Beside a writing option, an argument the walk cannot read is
// refused too: it may spell the option.
type gitReader struct{ writes, why string }

// gitReaders is the CLOSED set of git subcommands judged to read; every
// other is a writer until argued here.
var gitReaders = map[string]gitReader{
	"cat-file":    {why: "reads objects by id"},
	"fsck":        {writes: "--lost-found", why: "checks objects and connectivity; --lost-found writes lost-found/"},
	"version":     {why: "prints the binary's version and opens no repository"},
	"upload-pack": {why: uploadPackReads},
	"rev-list": {writes: "--output", why: "walks history; --output, a diff option its revision walker " +
		"takes, creates the file it names in the directory git runs in (git 2.43: `rev-list " +
		"--output=f HEAD` exits 0 and leaves f behind)"},
	"merge-base": {why: "walks history"},
	"for-each-ref": {why: "lists the refs a pattern names, with their objects' names and types; it opens no " +
		"file to write, and its options choose what it prints"},
	"rev-parse": {why: "expands an abbreviated object id from the objects alone (--disambiguate)"},
	"ls-tree":   {why: "lists a tree by id"},
	"diff-tree": {writes: "--output", why: "compares two trees by id; --output, a diff option, creates the " +
		"file it names in the directory git runs in (2.56.0: `diff-tree --output=f <a> <b>` exits 0 and " +
		"leaves f behind)"},
	"diff-pairs": {writes: "--output", why: "diffs the pairs of blobs diff-tree's raw records on its standard " +
		"input name, by id; --output, a diff option, creates the file it names in the directory git runs in " +
		"(2.56.0: `diff-pairs -z --numstat --output=f` exits 0 and leaves f behind)"},
	"patch-id": {why: "hashes the patch on its standard input and prints its id: it opens no object and no ref, " +
		"and its options (--stable, --unstable, --verbatim) choose how it hashes, none what it writes"},
}

const uploadPackReads = "serves a fetch: its pack goes to the client, and the one file it writes in the " +
	"repository — a shallow request's temporary list of shallow commits, removed as git exits — is no object " +
	"and no ref"

// gitWriterWords are the subcommands a []string literal is named by when it
// begins with one, in a file that runs git: an argument list assembled away
// from its Cmd. A naming aid only — what makes the gate sound is that a Cmd
// whose Args it cannot read where they are spelled is a hit of its own.
var gitWriterWords = setOf("index-pack", "multi-pack-index", "commit-graph", "receive-pack", "update-ref",
	"symbolic-ref", "pack-refs", "gc", "repack", "prune", "prune-packed", "fetch", "unpack-objects",
	"fast-import", "config", "maintenance", "replace", "notes", "tag", "branch", "commit", "merge", "init",
	"clone", "worktree", "merge-tree", "pack-objects", "hash-object", "mktag", "mktree", "commit-tree",
	"write-tree", "update-index", "read-tree", "checkout", "reset", "am", "apply", "cherry-pick", "revert",
	"rebase", "stash", "pull", "push", "reflog", "bundle", "http-backend", "archive")

func setOf(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// gitprocPath is gitproc's import path, read off its own type, so a move of
// the module cannot leave the gate matching an import nobody writes; and
// modulePath the module's, read off it, in which the walk's world reads
// packages from the tree it judges.
var (
	gitprocPath = reflect.TypeOf(gitproc.Cmd{}).PkgPath()
	modulePath  = strings.TrimSuffix(gitprocPath, "/internal/code/gitproc")
)

// writerHit is one write the walk found.
type writerHit struct {
	pos  string // file:line:column, relative to the walk's root
	file string // the file, slash-separated and relative to the root
	kind string // kindOS, kindGit+" "+subcommand, or kindRuntime
	what string // the selector (os.Rename), the subcommand, or what was built at run time
}

func (h writerHit) doing() string {
	switch h.kind {
	case kindOS:
		return "writes the filesystem (" + h.what + ")"
	case kindRuntime:
		return "runs git with " + h.what
	}
	return "runs git " + h.what
}

type writerWalk struct {
	files      int // non-test files read
	cacheFiles int // of them, internal/code/cache's
	cmds       int // Cmd literals judged
	handed     int // Cmds handed to a call, a literal spelled there or not
	hits       []writerHit
	// argv is every argument list a file that runs git spells whole, by the
	// file, slash-separated and relative to the root.
	argv map[string][][]string
	// literals is every Cmd literal the walk judged, as the dropping view's
	// gate reads one (TestOnlyServingReadsNameTheDroppingView).
	literals []cmdLiteral
}

// cmdLiteral is one Cmd literal as the dropping view's gate reads it: where it
// is, the subcommand its Args begin with ("" where they begin with no string
// literal), and whether it names a dropping view (Cmd.Dropping) and an
// alternate (Cmd.Alternate).
type cmdLiteral struct {
	pos, file, word     string
	dropping, alternate bool
}

// codeWalk is the walk of internal/code every static gate of this package
// reads, made once a test binary: a walk types every file of the tree, and
// two gates reading it apart walked it twice.
var codeWalk = sync.OnceValues(func() (writerWalk, error) {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return writerWalk{}, err
	}
	return walkForWriters(root)
})

// harnessDir is a directory the walk does not enter: a test harness, named
// as the tree names them.
var harnessDir = regexp.MustCompile(`^[a-z0-9]+test$`)

// walkForWriters reads every non-test .go file under root's internal/code
// and collects its writes.
func walkForWriters(root string) (writerWalk, error) {
	start := filepath.Join(root, "internal", "code")
	gitprocDir := filepath.Join(start, "gitproc")
	cacheDir := filepath.Join(start, "cache")
	var paths []string
	err := sourcetree.Walk(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || harnessDir.MatchString(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil {
		return writerWalk{}, err
	}
	w := decltype.New(root, modulePath, nil)
	cmd, err := cmdStruct(w)
	if err != nil {
		return writerWalk{}, err
	}
	out := writerWalk{argv: map[string][][]string{}}
	for _, path := range paths {
		// Every file, wherever it is built: one this platform does not
		// build is read against the scope of the files it does.
		f := w.FileAt(path)
		if f == nil {
			continue // why is among w.Errors(), returned below
		}
		out.files++
		if filepath.Dir(path) == cacheDir {
			out.cacheFiles++
		}
		m, err := newWriterMatch(w, f, cmd, filepath.Dir(path) == gitprocDir)
		if err != nil {
			return out, err
		}
		m.walk()
		out.hits = append(out.hits, m.hits...)
		if len(m.argvs) > 0 {
			out.argv[f.Rel] = append(out.argv[f.Rel], m.argvs...)
		}
		out.cmds += m.cmds
		out.handed += m.handed
		out.literals = append(out.literals, m.literals...)
	}
	if errs := w.Errors(); len(errs) > 0 {
		return out, fmt.Errorf("a file the walk could not read is a file it judged nothing in: %w",
			errors.Join(errs...))
	}
	return out, nil
}

// cmdStruct is the struct literal gitproc's Cmd is declared as, in w: what
// every type a value is a Cmd under — Cmd, an alias of it, a type it
// defines, a pointer to any of them — resolves to.
func cmdStruct(w *decltype.World) (ast.Expr, error) {
	p := w.Load(gitprocPath)
	if p == nil {
		return nil, fmt.Errorf("cannot read %s, where Cmd is declared: no git process could be judged", gitprocPath)
	}
	spec := p.TypeSpec("Cmd")
	if spec == nil {
		return nil, fmt.Errorf("%s declares no Cmd: this gate judges git processes by the Cmd the pool runs — "+
			"follow the type to what runs git now", gitprocPath)
	}
	if _, ok := spec.Type.(*ast.StructType); !ok {
		return nil, fmt.Errorf("%s's Cmd is no struct type: this gate reads a Cmd's Args field", gitprocPath)
	}
	return spec.Type, nil
}

// writerMatch is one file's judgement.
type writerMatch struct {
	w *decltype.World
	f *decltype.File
	// cmd is gitproc's Cmd struct literal (cmdStruct).
	cmd ast.Expr
	// names maps each import name in the file to the path it imports, for
	// the packages the gate judges.
	names map[string]string
	// inGitproc: a file of gitproc itself; dotGitproc: a file that
	// dot-imports it — each a file that runs git.
	inGitproc, dotGitproc bool
	// judged is every []string literal already judged as a Cmd's Args.
	judged map[token.Pos]bool
	// paramFields is the fields of every parameter list in the file — a
	// function declaration's or a function literal's, never a result list
	// or a receiver.
	paramFields map[*ast.Field]bool
	// cmds counts the Cmd literals judged, handed the Cmds handed to a call.
	cmds, handed int
	hits         []writerHit
	// argvs is every argument list the file spells whole — a []string
	// literal of string literals — in a file that runs git: what a declared
	// write's Args are held to (derivedHalf).
	argvs [][]string
	// literals is every Cmd literal judged, as the dropping view's gate reads
	// one.
	literals []cmdLiteral
}

func newWriterMatch(w *decltype.World, f *decltype.File, cmd ast.Expr, inGitproc bool) (*writerMatch, error) {
	m := &writerMatch{w: w, f: f, cmd: cmd, names: map[string]string{}, inGitproc: inGitproc,
		judged: map[token.Pos]bool{}, paramFields: map[*ast.Field]bool{}}
	for _, imp := range f.AST.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: import %s: %w", f.Rel, imp.Path.Value, err)
		}
		if fsMutators[path] == nil && path != gitprocPath {
			continue
		}
		name := decltype.ImportName(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch name {
		case "_":
			continue
		case ".":
			if path == gitprocPath {
				// A Cmd reached through a dot import is one the walk cannot
				// type (decltype does not follow one), so the import is a
				// hit in itself rather than a file judged clean.
				m.dotGitproc = true
				m.hit(imp.Pos(), kindRuntime, path+", imported with a dot: its names carry no qualifier to type")
			} else {
				m.hit(imp.Pos(), kindOS, path+", imported with a dot: its calls carry no qualifier to judge")
			}
			continue
		}
		m.names[name] = path
	}
	return m, nil
}

func (m *writerMatch) hit(pos token.Pos, kind, what string) {
	p := m.w.FileSet().Position(pos)
	m.hits = append(m.hits, writerHit{
		pos: fmt.Sprintf("%s:%d:%d", m.f.Rel, p.Line, p.Column), file: m.f.Rel, kind: kind, what: what,
	})
}

// runsGit reports whether the file can name gitproc.
func (m *writerMatch) runsGit() bool {
	if m.inGitproc || m.dotGitproc {
		return true
	}
	for _, path := range m.names {
		if path == gitprocPath {
			return true
		}
	}
	return false
}

func (m *writerMatch) walk() {
	file := m.f.AST
	// The Cmd literals first, so a []string literal judged as one's Args is
	// not named a second time by the free-standing scan; and every function's
	// parameters, which the handoffs below ask after.
	var funcs []*ast.FuncType
	var bodies []*ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			// An elided literal is judged through the literal holding it,
			// which is what says its type.
			if n.Type != nil {
				m.literal(n, decltype.Spelled(n.Type, m.f))
			}
		case *ast.FuncDecl:
			funcs, bodies = append(funcs, n.Type), append(bodies, n.Body)
		case *ast.FuncLit:
			funcs, bodies = append(funcs, n.Type), append(bodies, n.Body)
		}
		return true
	})
	for i, ft := range funcs {
		if ft.Params == nil {
			continue
		}
		for _, field := range ft.Params.List {
			m.paramFields[field] = true
			if m.inGitproc && bodies[i] != nil && m.isCmd(decltype.Spelled(field.Type, m.f)) {
				for _, name := range field.Names {
					m.unchanged(bodies[i], name)
				}
			}
		}
	}
	git := m.runsGit()
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			m.mutation(n)
			m.receiver(n)
		case *ast.CompositeLit:
			if !git || !isStrings(n.Type) || len(n.Elts) == 0 {
				return true
			}
			if argv, ok := spelledArgv(n); ok {
				m.argvs = append(m.argvs, argv)
			}
			if m.judged[n.Lbrace] {
				return true
			}
			if word, ok := stringLit(n.Elts[0]); ok && gitWriterWords[word] {
				m.hit(n.Pos(), kindGit+" "+word, word)
			}
		case *ast.CallExpr:
			m.handoff(n)
		}
		return true
	})
}

// mutation judges a selector as a filesystem mutation: one of fsMutators,
// through the file's own import name.
func (m *writerMatch) mutation(sel *ast.SelectorExpr) {
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Obj != nil {
		// id.Obj set: the name resolves to something this file declares — a
		// variable or a parameter — so it is not the import.
		return
	}
	if path, ok := m.names[id.Name]; ok && fsMutators[path][sel.Sel.Name] {
		m.hit(sel.Pos(), kindOS, id.Name+"."+sel.Sel.Name)
	}
}

// receiver judges a method selected on a Cmd: the selector hands the Cmd to
// the method as its receiver — called there, or later as a method value — so
// it is judged as an argument handed to a call is, and must be a Cmd literal
// spelled there. No receiver is gitproc's plumbing: the pool hands a Cmd on as
// an argument, and only a parameter is a Cmd its function was handed.
func (m *writerMatch) receiver(sel *ast.SelectorExpr) {
	if m.isCmd(decltype.Spelled(ast.Unparen(sel.X), m.f)) {
		// A method expression, Cmd.m: the selector names a type and hands no
		// value — its receiver is the call's first argument, which handoff
		// judges.
		return
	}
	t := m.w.TypeOf(sel.X, m.f)
	if !m.isCmd(t) || m.w.Select(t, sel.Sel.Name).Kind == decltype.Field {
		return
	}
	m.handed++
	if !m.spelledCmd(sel.X) {
		m.hit(sel.X.Pos(), kindRuntime, "a Cmd handed to "+types.ExprString(sel)+" that the call does not spell")
	}
}

// conversion reports whether call converts a value of what reach says to a
// type that keeps it what it was: a Cmd to a Cmd type — through a pointer, an
// alias or a type Cmd defines, as spelledCmd reads one — or its Args to a
// slice or array type, or a pointer to one, which may share their elements.
// Its callee is read as a type spelled there, so a function's name is none,
// and nor is a type of a package the walk does not read, which only its
// declaration could tell from a function (the coverage boundary).
func (m *writerMatch) conversion(call *ast.CallExpr, reach int) bool {
	to := decltype.Spelled(ast.Unparen(call.Fun), m.f)
	switch reach {
	case reachWhole:
		return m.isCmd(to)
	case reachArgs:
		u, how := m.w.Underlying(to)
		_, list := u.Expr().(*ast.ArrayType)
		return how == decltype.Resolved && list
	}
	return false
}

// What a path rooted at a Cmd parameter denotes (change).
const (
	reachWhole   = iota // the Cmd, or the pointer to it
	reachArgs           // its Args, or a slice of them, which shares their elements
	reachElement        // an element of its Args: a string, a value
	reachOther          // another field, or what is read from one
)

// unchanged judges the Cmd parameter name declares, in the body of the
// function — closures included — that it is a parameter of: a hit wherever
// the body changes what the pool was handed. Only gitproc's files are judged
// so, and only a parameter: gitproc runs git without handing a Cmd to a call
// (its one exec, which takes a parameter's Args), so a Cmd it was handed and
// then changed may run whether or not it is handed on; anywhere else a Cmd
// reaches git only by being handed on, where it must be spelled.
func (m *writerMatch) unchanged(body *ast.BlockStmt, name *ast.Ident) {
	// Matched by the object the parser declared it as — "_" included, which
	// it declares and never puts in scope, so nothing in the body names it —
	// rather than by name, which a closure's own parameter or a local can
	// shadow.
	var stack []ast.Node
	ast.Inspect(body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if id, ok := n.(*ast.Ident); ok && id.Obj == name.Obj {
			if how := m.change(id, stack); how != "" {
				m.hit(id.Pos(), kindRuntime, "a Cmd it was handed, changed in place (parameter "+id.Name+", "+how+")")
			}
		}
		stack = append(stack, n)
		return true
	})
}

// change is how one use of a Cmd parameter changes what its Args hold, given
// the nodes enclosing it, innermost last — followed through parentheses, a
// dereference, its fields, an element or a slice of its Args and a
// conversion that keeps it what it was (conversion): "written" when an
// assignment or a range clause writes the Cmd, its Args or an element of
// them; "its address or a part's taken" for an & of any of those; "copied
// whole" or "its Args copied" when the Cmd or its Args are held anywhere but
// in a call — a copy shares the Args' elements, so a write through it is a
// write to them. "" for every other use: a read of an element, a comparison,
// a range over the Args, a field other than Args — the Cmd is judged by its
// Args — and a call handed the Cmd, which handoff judges, or its Args, which
// the coverage boundary leaves to the callee.
func (m *writerMatch) change(id *ast.Ident, stack []ast.Node) string {
	var cur ast.Node = id
	reach := reachWhole
	i := len(stack) - 1
path:
	for ; i >= 0; i-- {
		switch p := stack[i].(type) {
		case *ast.ParenExpr, *ast.StarExpr:
		case *ast.CallExpr:
			if len(p.Args) != 1 || ast.Node(p.Args[0]) != cur || !m.conversion(p, reach) {
				break path
			}
		case *ast.SelectorExpr:
			// Its Args, or anything else: another field, or a method,
			// whose selector hands it to the method as its receiver,
			// which receiver judges.
			if reach == reachWhole && p.Sel.Name == "Args" {
				reach = reachArgs
			} else {
				reach = reachOther
			}
		case *ast.IndexExpr:
			if p.X != cur {
				break path // the index of something else, judged as it stands
			}
			if reach == reachArgs {
				reach = reachElement
			}
		case *ast.SliceExpr:
			if p.X != cur {
				break path
			}
		default:
			break path
		}
		cur = stack[i]
	}
	if reach == reachOther {
		return ""
	}
	var parent ast.Node
	if i >= 0 {
		parent = stack[i]
	}
	switch p := parent.(type) {
	case *ast.AssignStmt:
		for _, l := range p.Lhs {
			if ast.Node(l) == cur {
				return "written"
			}
		}
	case *ast.RangeStmt:
		if cur == ast.Node(p.Key) || cur == ast.Node(p.Value) {
			return "written"
		}
		return "" // ranged over
	case *ast.UnaryExpr:
		if p.Op == token.AND {
			return "its address or a part's taken"
		}
		return ""
	case *ast.CallExpr:
		if reach == reachArgs && len(p.Args) > 0 && ast.Node(p.Args[0]) == cur && m.writesFirst(p) {
			return "written"
		}
		return "" // handed to a call
	case *ast.BinaryExpr:
		return "" // compared or combined
	case *ast.KeyValueExpr:
		if cur == ast.Node(p.Key) && i > 0 && m.structLiteral(stack[i-1]) {
			return "" // a field's name in a struct literal, not the parameter
		}
	}
	switch reach {
	case reachWhole:
		return "copied whole"
	case reachArgs:
		return "its Args copied"
	}
	return ""
}

// writesFirst reports whether call is to a builtin that writes the elements
// of the slice it is handed first, or may hand back a slice sharing them:
// copy's destination, clear's operand, append's slice. They are the
// language's own, so the walk knows what they do; what a function does with
// a slice it is handed, the walk does not follow (the coverage boundary).
func (m *writerMatch) writesFirst(call *ast.CallExpr) bool {
	// A name the file declares shadows the builtin; one only another file of
	// the package declares is taken for it, the stricter reading.
	fn, ok := ast.Unparen(call.Fun).(*ast.Ident)
	if !ok || fn.Obj != nil {
		return false
	}
	switch fn.Name {
	case "copy", "clear", "append":
		return true
	}
	return false
}

// structLiteral reports whether n is a composite literal of a struct type,
// whose keys are field names rather than values.
func (m *writerMatch) structLiteral(n ast.Node) bool {
	lit, ok := n.(*ast.CompositeLit)
	if !ok {
		return false
	}
	u, how := m.w.Underlying(m.w.TypeOf(lit, m.f))
	if how != decltype.Resolved {
		return false
	}
	_, ok = u.Expr().(*ast.StructType)
	return ok
}

// isCmd reports whether t is gitproc's Cmd — through pointers, aliases and
// the types Cmd defines, each of which resolves to its struct literal.
func (m *writerMatch) isCmd(t decltype.Type) bool {
	lit, how := m.w.Underlying(t)
	return how == decltype.Resolved && lit.Expr() == m.cmd
}

// literal judges a composite literal of type t: a Cmd's by its Args, and a
// collection's elided elements by the type the collection gives them.
func (m *writerMatch) literal(lit *ast.CompositeLit, t decltype.Type) {
	u, how := m.w.Underlying(t)
	if how != decltype.Resolved {
		// A type of a package the walk does not read cannot be gitproc's
		// Cmd, which only this module can name.
		return
	}
	switch x := u.Expr().(type) {
	case *ast.StructType:
		if u.Expr() == m.cmd {
			m.cmds++
			m.judgeCmd(lit)
		}
	case *ast.ArrayType:
		m.elided(lit, decltype.Type{}, u.In(x.Elt))
	case *ast.MapType:
		m.elided(lit, u.In(x.Key), u.In(x.Value))
	}
}

// elided judges the elements of a collection's literal whose type the
// collection gives them: key and elt, the zero Type for an array's index.
func (m *writerMatch) elided(lit *ast.CompositeLit, key, elt decltype.Type) {
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if inner, ok := kv.Key.(*ast.CompositeLit); ok && inner.Type == nil && key.Known() {
				m.literal(inner, key)
			}
			el = kv.Value
		}
		if inner, ok := el.(*ast.CompositeLit); ok && inner.Type == nil {
			m.literal(inner, elt)
		}
	}
}

// judgeCmd judges one Cmd literal by its Args, and records it as the dropping
// view's gate reads it (cmdLiteral).
func (m *writerMatch) judgeCmd(lit *ast.CompositeLit) {
	var args ast.Expr
	p := m.w.FileSet().Position(lit.Pos())
	named := cmdLiteral{pos: fmt.Sprintf("%s:%d:%d", m.f.Rel, p.Line, p.Column), file: m.f.Rel}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			// A literal without field names: which value is Args is the
			// struct's field order, which this walk does not read.
			m.hit(lit.Pos(), kindRuntime, "the arguments of a Cmd literal without field names")
			return
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Args":
			args = kv.Value
		case "Dropping":
			named.dropping = true
		case "Alternate":
			named.alternate = true
		}
	}
	if list, _ := argvLiteral(args); list != nil {
		named.word, _ = stringLit(firstOf(list.Elts))
	}
	m.literals = append(m.literals, named)
	if args == nil {
		// No Args: handed straight to the pool, it runs nothing — Run
		// refuses a Cmd with no subcommand before any process starts — and
		// given Args anywhere else it reaches git only by being handed on,
		// which handoff judges.
		return
	}
	list, rest := argvLiteral(args)
	if list == nil {
		m.hit(args.Pos(), kindRuntime, "arguments built at run time")
		return
	}
	m.judged[list.Lbrace] = true
	word, ok := stringLit(firstOf(list.Elts))
	if !ok {
		m.hit(args.Pos(), kindRuntime, "arguments built at run time")
		return
	}
	reader, known := gitReaders[word]
	if !known {
		m.hit(args.Pos(), kindGit+" "+word, word)
		return
	}
	if reader.writes == "" {
		return
	}
	argv := append(slices.Clone(list.Elts[1:]), rest...)
	for _, a := range argv {
		s, ok := stringLit(a)
		if !ok || spellsOption(reader.writes, s) {
			// An argument the walk cannot read beside an option that
			// writes, or that option in a spelling git may accept.
			m.hit(args.Pos(), kindGit+" "+word, word+" "+reader.writes)
			return
		}
	}
}

// spellsOption reports whether arg may spell the long option option: its
// name, before any "=", is the option whole or an unambiguous prefix of it
// (more than the dashes). A value given as the next argument follows a name
// this matches.
func spellsOption(option, arg string) bool {
	name, _, _ := strings.Cut(arg, "=")
	return len(name) > len("--") && strings.HasPrefix(option, name)
}

// handoff judges one call: every Cmd it is handed is one spelled in it.
func (m *writerMatch) handoff(call *ast.CallExpr) {
	if m.isCmd(decltype.Spelled(ast.Unparen(call.Fun), m.f)) {
		// A conversion between Cmd types hands the value to nothing: the
		// value it makes is judged where it is handed on.
		return
	}
	params := m.params(call)
	for i, a := range call.Args {
		if m.spelledCmd(a) {
			m.handed++
			continue
		}
		var param decltype.Type
		if i < len(params) {
			param = params[i]
		}
		arg := m.w.TypeOf(a, m.f)
		switch {
		case m.isCmd(param) || m.isCmd(arg):
			m.handed++
			if m.passedOn(call, i, a) {
				continue
			}
			m.hit(a.Pos(), kindRuntime, "a Cmd handed to "+types.ExprString(call.Fun)+" that the call does not spell")
		case !m.typed(param) && !m.typed(arg) && !noValue(call, i):
			// Cannot tell is not "not a Cmd": an absence gate that took one
			// for the other would certify nothing about exactly the values
			// it skipped.
			m.hit(a.Pos(), kindRuntime, "a value the walk cannot type, handed to "+types.ExprString(call.Fun)+
				", which it cannot type either — type the value (a declared variable, a typed constant) or "+
				"teach internal/sourcetree/decltype the shape")
		}
	}
}

// typed reports whether the walk knows enough of t to say whether it is a
// Cmd: a type it resolved, one of a package it does not read (which cannot
// be gitproc's Cmd, a type only this module can name), a value only such a
// package can type, or a value of a type parameter nothing binds — which is
// a Cmd only once a conversion or an assertion makes it one, a typed
// expression this rule judges wherever it is handed on.
func (m *writerMatch) typed(t decltype.Type) bool {
	if _, unread := t.Unread(); unread {
		return true
	}
	if id, ok := t.Expr().(*ast.Ident); ok && id.Obj != nil && id.Obj.Kind == ast.Typ {
		if _, param := id.Obj.Decl.(*ast.Field); param {
			return true
		}
	}
	_, how := m.w.Underlying(t)
	return how == decltype.Resolved || how == decltype.Outside
}

// noValue reports whether call's i-th argument is no value a Cmd could be:
// the type make or new is handed, an untyped nil, or a constant — no
// constant is a struct. Each is what the walk cannot type on either side of
// a call in the tree as it stands: make's operand, nil, and the constants
// an iota list continues, which decltype leaves unknown.
func noValue(call *ast.CallExpr, i int) bool {
	if fn, ok := ast.Unparen(call.Fun).(*ast.Ident); ok && i == 0 && fn.Obj == nil &&
		(fn.Name == "make" || fn.Name == "new") {
		return true
	}
	id, ok := ast.Unparen(call.Args[i]).(*ast.Ident)
	switch {
	case !ok:
		return false
	case id.Obj == nil:
		return id.Name == "nil"
	}
	return id.Obj.Kind == ast.Con
}

// params is the type of the parameter each of call's arguments is handed
// to, as far as the walk can type the callee: none where it cannot, and none
// for a slice handed whole to a variadic parameter.
func (m *writerMatch) params(call *ast.CallExpr) []decltype.Type {
	sig, how := m.w.Underlying(m.w.TypeOf(call.Fun, m.f))
	if how != decltype.Resolved {
		return nil
	}
	ft, ok := sig.Expr().(*ast.FuncType)
	if !ok || ft.Params == nil {
		return nil
	}
	var ps []decltype.Type
	for _, field := range ft.Params.List {
		for range max(1, len(field.Names)) {
			ps = append(ps, sig.In(field.Type))
		}
	}
	if n := len(ps); n > 0 {
		if ell, ok := ps[n-1].Expr().(*ast.Ellipsis); ok {
			ps = ps[:n-1]
			if !call.Ellipsis.IsValid() {
				// Each argument from the variadic one on is an element.
				for len(ps) < len(call.Args) {
					ps = append(ps, sig.In(ell.Elt))
				}
			}
		}
	}
	return ps
}

// spelledCmd reports whether e is a Cmd literal spelled where it stands —
// behind & or a conversion between Cmd types included — which literal has
// judged by its Args.
func (m *writerMatch) spelledCmd(e ast.Expr) bool {
	switch x := ast.Unparen(e).(type) {
	case *ast.CompositeLit:
		return x.Type != nil && m.isCmd(decltype.Spelled(x.Type, m.f))
	case *ast.UnaryExpr:
		return x.Op == token.AND && m.spelledCmd(x.X)
	case *ast.CallExpr:
		return len(x.Args) == 1 && m.isCmd(decltype.Spelled(ast.Unparen(x.Fun), m.f)) && m.spelledCmd(x.Args[0])
	}
	return false
}

// passedOn reports whether call's i-th argument a is the one handoff passed:
// gitproc's plumbing passing on the Cmd its function was handed — a call, from
// a file of gitproc, to one of gitproc's unexported functions or methods, of a
// Cmd parameter of a function the call is in (its own, or the one a closure
// it is in shares it with), handed singly to a Cmd parameter. Both parameters
// are therefore ones unchanged judges, so the Cmd reaches the callee as the
// caller handed it, and the callee is judged the same way in turn.
func (m *writerMatch) passedOn(call *ast.CallExpr, i int, a ast.Expr) bool {
	return m.plumbing(call) && m.handedIn(a) && m.cmdParam(call, i)
}

// handedIn reports whether e names a Cmd parameter of a function of this
// file: a name its parameter list declares — never a named result, which the
// function builds, nor a receiver, which a selector hands in and receiver
// judges there.
func (m *writerMatch) handedIn(e ast.Expr) bool {
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok || id.Obj == nil || id.Obj.Kind != ast.Var {
		return false
	}
	field, ok := id.Obj.Decl.(*ast.Field)
	return ok && m.paramFields[field] && m.isCmd(decltype.Spelled(field.Type, m.f))
}

// cmdParam reports whether call's i-th argument is handed to a parameter its
// callee declares as a Cmd — as declared, so a type parameter is none,
// whatever the call binds it to — and never to a variadic one, which copies
// it into a slice.
func (m *writerMatch) cmdParam(call *ast.CallExpr, i int) bool {
	sig, how := m.w.Underlying(m.w.TypeOf(call.Fun, m.f))
	if how != decltype.Resolved {
		return false
	}
	ft, ok := sig.Expr().(*ast.FuncType)
	if !ok || ft.Params == nil {
		return false
	}
	at := 0
	for _, field := range ft.Params.List {
		at += max(1, len(field.Names))
		if i < at {
			// A variadic parameter's type is a slice, never a Cmd.
			return m.isCmd(decltype.Spelled(field.Type, sig.File()))
		}
	}
	return false
}

// plumbing reports whether call is to one of gitproc's unexported functions
// or methods, from a file of gitproc — the only files that can name one.
func (m *writerMatch) plumbing(call *ast.CallExpr) bool {
	if !m.inGitproc {
		return false
	}
	switch x := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		if token.IsExported(x.Name) {
			return false
		}
		if x.Obj == nil {
			// A package-level name another file of gitproc declares — a
			// function, never a variable of a function type, which may hold
			// any function — or a builtin, which gitproc does not declare.
			return m.f.Package != nil && declaresFunc(m.f.Package, x.Name)
		}
		_, isFunc := x.Obj.Decl.(*ast.FuncDecl)
		return isFunc
	case *ast.SelectorExpr:
		if token.IsExported(x.Sel.Name) {
			return false
		}
		sel := m.w.Select(m.w.TypeOf(x.X, m.f), x.Sel.Name)
		return sel.Kind == decltype.Method && sel.Owner.Package == gitprocPath
	}
	return false
}

// declaresFunc reports whether p declares a function — not a method — named
// name at package level.
func declaresFunc(p *decltype.Package, name string) bool {
	for _, f := range p.Files() {
		for _, d := range f.AST.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// argvLiteral is the []string literal an Args value spells — the value
// itself, or the first argument of an append to it — and the further
// arguments an append adds; nil when the value spells none.
func argvLiteral(expr ast.Expr) (*ast.CompositeLit, []ast.Expr) {
	if call, ok := expr.(*ast.CallExpr); ok {
		fn, ok := call.Fun.(*ast.Ident)
		if !ok || fn.Name != "append" || fn.Obj != nil || len(call.Args) == 0 {
			return nil, nil
		}
		list, ok := call.Args[0].(*ast.CompositeLit)
		if !ok || !isStrings(list.Type) {
			return nil, nil
		}
		return list, call.Args[1:]
	}
	list, ok := expr.(*ast.CompositeLit)
	if !ok || !isStrings(list.Type) {
		return nil, nil
	}
	return list, nil
}

// isStrings reports whether expr is the type []string.
func isStrings(expr ast.Expr) bool {
	arr, ok := expr.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return false
	}
	id, ok := arr.Elt.(*ast.Ident)
	return ok && id.Name == "string" && id.Obj == nil
}

func firstOf(exprs []ast.Expr) ast.Expr {
	if len(exprs) == 0 {
		return nil
	}
	return exprs[0]
}

// spelledArgv is the value of a []string literal every element of which is a
// string literal: an argument list spelled whole where it stands.
func spelledArgv(lit *ast.CompositeLit) ([]string, bool) {
	argv := make([]string, 0, len(lit.Elts))
	for _, e := range lit.Elts {
		s, ok := stringLit(e)
		if !ok {
			return nil, false
		}
		argv = append(argv, s)
	}
	return argv, true
}

// stringLit is the value of a string literal.
func stringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// controlWriters holds the matcher and the walk to verdicts known in advance,
// over a synthetic tree: a matcher that stopped recognising a mutation or a
// writer fails HERE rather than certifying a clean tree.
func controlWriters(t *testing.T) {
	t.Helper()
	for word, r := range gitReaders {
		if r.writes != "" && (!strings.HasPrefix(r.writes, "--") || len(r.writes) <= len("--")) {
			t.Errorf("gitReaders[%q] writes with %q, which is no long option: spellsOption reads a long "+
				"option's spellings and nothing else", word, r.writes)
		}
	}
	gp := strconv.Quote(gitprocPath)
	files := map[string]string{
		// A renamed import, a call and a value.
		"internal/code/a/rename.go": "package a\n\nimport o \"os\"\n\n" +
			"func f(a, b string) error { return o.Rename(a, b) }\n\nvar rm = o.RemoveAll\n",
		// A read, a doc comment naming a mutation, and a local named os.
		"internal/code/a/read.go": "package a\n\nimport \"os\"\n\n" +
			"// clean calls os.Remove on nothing; this comment is not a call.\n" +
			"func clean(p string) { f, _ := os.Open(p); _ = f; var os struct{ Remove func() }; os.Remove() }\n",
		// syscall, and a dot import of os.
		"internal/code/a/sys.go": "package a\n\nimport \"syscall\"\n\nfunc g(p string) error { return syscall.Unlink(p) }\n",
		"internal/code/a/dot.go": "package a\n\nimport . \"os\"\n\nvar _ = Getpid\n",
		// git: a writer; a subcommand the closed reader set does not name,
		// behind & — outside gitWriterWords too, so only the Cmd's own
		// judgement can find it, as for the elided literal, gitproc's own
		// Cmd and package c below; a reader; a reader handed its writing
		// option abbreviated, and one without it; rev-list handed its
		// writing option with its value attached and as the next argument,
		// and beside arguments the walk cannot read, and an option sharing
		// its first letters; Args built at run time; an append; an elided
		// literal in a slice of Cmds; and a free-standing writer's argv
		// beside a list that is no argv.
		"internal/code/a/git.go": "package a\n\nimport g " + gp + "\n\n" +
			"func h(r, o string, args []string) {\n" +
			"\t_ = g.Cmd{Args: []string{\"update-ref\", r, o}}\n" +
			"\t_ = &g.Cmd{Args: []string{\"send-pack\", \"origin\"}}\n" +
			"\t_ = &g.Cmd{Args: []string{\"cat-file\", \"--batch-check\"}}\n" +
			"\t_ = g.Cmd{Args: []string{\"fsck\", \"--lost\"}}\n" +
			"\t_ = g.Cmd{Args: []string{\"fsck\", \"--connectivity-only\"}}\n" +
			"\t_ = g.Cmd{Args: []string{\"rev-list\", \"--output=x\", \"HEAD\"}}\n" +
			"\t_ = g.Cmd{Args: []string{\"rev-list\", \"--output\", \"x\", \"HEAD\"}}\n" +
			"\t_ = g.Cmd{Args: append([]string{\"rev-list\", \"HEAD\"}, args...)}\n" +
			"\t_ = g.Cmd{Args: []string{\"rev-list\", \"--objects\", \"--output-indicator-new=+\", \"HEAD\"}}\n" +
			"\t_ = g.Cmd{Args: args}\n" +
			"\t_ = g.Cmd{Args: append([]string{\"merge-tree\", \"--write-tree\"}, args...)}\n" +
			"\t_ = []g.Cmd{{Args: []string{\"verify-pack\", \"x.idx\"}}}\n" +
			"\t_ = []string{\"commit-graph\", \"write\"}\n" +
			"\t_ = []string{\"HEAD\", \"config\"}\n" +
			"}\n",
		// Cmd under other names: an alias, a type Cmd defines, a named
		// slice of Cmds, a consumer's interface over the pool.
		"internal/code/c/types.go": "package c\n\nimport (\n\t\"context\"\n\n\tg " + gp + "\n)\n\n" +
			"type alias = g.Cmd\n\ntype defined g.Cmd\n\ntype cmds []g.Cmd\n\n" +
			"type runner interface{ Run(context.Context, g.Cmd) error }\n",
		// Literals of those, in a file that imports nothing: through the
		// alias, the defined type, elided in the named slice, and elided
		// twice behind a pointer in a map; and one with no Args.
		"internal/code/c/use.go": "package c\n\n" +
			"var _ = alias{Args: []string{\"rerere\"}}\n\n" +
			"var _ = defined{Args: []string{\"sparse-checkout\", \"set\"}}\n\n" +
			"var _ = cmds{{Args: []string{\"bisect\", \"start\"}}}\n\n" +
			"var _ = map[string][]*alias{\"x\": {{Args: []string{\"filter-branch\"}}}}\n\n" +
			"var _ = defined{Dir: \"x\"}\n",
		// Cmds handed on: Args assigned after the literal; a Cmd from new
		// with Args assigned, handed as a value and as a pointer; one handed
		// through the interface, to a function value, to a function the
		// walk does not read and as an element of a variadic parameter; one
		// handed to a method of the defined type as its receiver, converted
		// from the Cmd above; a receiver of the defined type converted and
		// handed to the pool; a parameter, its Args replaced, passed on to
		// an unexported function outside gitproc, which is no plumbing of
		// the pool's — one hit, where it is handed on, since only gitproc's
		// files are judged for changing a parameter itself — and, spelled
		// in the call, a literal, a converted literal of the defined type,
		// one through the alias, one behind & and one spelled as a method's
		// receiver, which pass, beside a conversion that hands nothing on
		// and a slice handed whole to the variadic parameter.
		"internal/code/c/handoff.go": "package c\n\nimport (\n\t\"context\"\n\t\"fmt\"\n\n\tg " + gp + "\n)\n\n" +
			"func k(ctx context.Context, p *g.Pool, r runner, run func(g.Cmd), o string, extra []string, cs cmds) {\n" +
			"\tc := g.Cmd{Dir: o}\n\tc.Args = []string{\"replay\", \"--onto\", o}\n\t_ = p.Run(ctx, c)\n" +
			"\td := new(g.Cmd)\n\td.Args = extra\n\t_ = p.Run(ctx, *d)\n\tptr(d)\n" +
			"\t_ = r.Run(ctx, c)\n\trun(c)\n\tfmt.Println(c)\n" +
			"\tmany(g.Cmd{Args: []string{\"version\"}}, c)\n\tmany(cs...)\n" +
			"\t_ = defined(c).run(ctx, p)\n" +
			"\t_ = p.Run(ctx, g.Cmd{Args: []string{\"cat-file\", \"-e\", o}})\n" +
			"\t_ = p.Run(ctx, g.Cmd(defined{Args: []string{\"version\"}}))\n" +
			"\t_ = p.Run(ctx, alias{Args: []string{\"cat-file\", \"-t\", o}})\n" +
			"\tptr(&g.Cmd{Args: []string{\"cat-file\", \"-p\", o}})\n" +
			"\t_ = defined{Args: []string{\"version\"}}.run(ctx, p)\n" +
			"\t_ = defined(c)\n" +
			"}\n\n" +
			"func (x defined) run(ctx context.Context, p *g.Pool) error { return p.Run(ctx, g.Cmd(x)) }\n\n" +
			"func ptr(*g.Cmd) {}\n\nfunc many(...g.Cmd) {}\n\nfunc one(g.Cmd) {}\n\n" +
			"func pass(c g.Cmd) {\n\tc.Args = []string{\"rerere\"}\n\tone(c)\n}\n",
		// A value the walk cannot type — a type switch's variable — handed
		// to a callee it cannot type either, and one handed to an element
		// of a variadic parameter of Cmds, which the parameter alone says
		// is a Cmd; and what passes on neither side because it can be no
		// Cmd: nil, a constant an iota list continues, the type make is
		// handed, and a value of a type parameter.
		"internal/code/c/untyped.go": "package c\n\nimport (\n\t\"fmt\"\n\n\tg " + gp + "\n)\n\n" +
			"const (\n\tfirst int = iota\n\tsecond\n)\n\n" +
			"func u(x any) {\n\tswitch v := x.(type) {\n\tcase int:\n\t\tfmt.Println(v)\n" +
			"\tcase g.Cmd:\n\t\tmany(v)\n\t}\n" +
			"\tfmt.Println(nil, second, make([]string, 1), smaller(1, 2))\n}\n\n" +
			"func smaller[T int | uint](a, b T) T { return min(a, b) }\n",
		// gitproc imported with a dot: its Cmd is one the walk cannot type.
		"internal/code/d/dot.go": "package d\n\nimport . " + gp + "\n\nvar _ = Cmd{Args: []string{\"rerere\"}}\n",
		// gitproc's own Cmd, named unqualified, and its plumbing. What
		// passes: its Run passing the Cmd it was handed on; run reading
		// that Cmd's Args (len, an element, a comparison, a range, a
		// function's one argument, a map's key, copy's source, append's
		// later arguments, a local function named clear), writing a field
		// other than Args, naming a struct field like it, writing a local
		// that shadows it, passing it on from a closure and handing its
		// Args to a call; a function literal's parameter passed on; and a
		// blank parameter, which nothing names, beside a blank assignment.
		// What does not — every subcommand outside gitWriterWords, so only
		// these rules can find it: a Cmd it builds itself, a named result
		// and a receiver, each handed to the plumbing; a receiver built at
		// run time, beside one spelled in the selector and a method
		// expression, which pass; a parameter handed into a variadic
		// parameter, to one that is no Cmd, to a type parameter and to a
		// variable of a function type hook.go declares; and a parameter
		// replaced, its Args replaced (directly and in a closure), an
		// element written by =, by an operator, through parentheses,
		// through a slice and by a range clause, its Args handed first to
		// copy, clear and append, its Args' address taken, a pointer to it
		// written through, the whole copied — as it is, converted to
		// Cmd, to a type of hook.go and through a pointer — and its Args
		// copied — sliced, converted to a slice and converted to a pointer
		// to an array — each changed where it was handed, so a hit where
		// the change is, handed on or not.
		"internal/code/gitproc/hook.go": "package gitproc\n\ntype local Cmd\n\nvar hook func(Cmd) error\n",
		"internal/code/gitproc/own.go": `package gitproc

import "context"

type Cmd struct {
	Dir  string
	Args []string
}

type Pool struct{}

type prepared struct{ c int }

func (p *Pool) Run(ctx context.Context, c Cmd) error { return p.run(ctx, c) }

func (p *Pool) run(ctx context.Context, c Cmd) error {
	if len(c.Args) == 0 || c.Args[0] == "" || c.Args == nil {
		return nil
	}
	for _, a := range c.Args {
		_ = a
	}
	c.Dir = "/elsewhere"
	_ = prepared{c: len(c.Args)}
	word, head := first(c.Args), c.Args[0]
	seen := map[string]bool{}
	seen[c.Args[0]] = true
	n := copy(make([]string, 1), c.Args)
	all := append([]string{}, c.Args...)
	clear := func([]string) {}
	clear(c.Args)
	_, _, _, _ = word, head, n, all
	for _, c := range []Cmd{{Args: []string{"version"}}} {
		c.Args = nil
	}
	go func() { _ = start(c) }()
	return argv(c.Args...)
}

func start(Cmd) error { return nil }

func argv(...string) error { return nil }

func first(args []string) string { return args[0] }

func (p *Pool) literal(ctx context.Context) (func(Cmd) error, func(Cmd) error) {
	return func(c Cmd) error { return p.run(ctx, c) }, func(c Cmd) error {
		c.Args = nil
		return p.run(ctx, c)
	}
}

func ignore(_ Cmd) local {
	_ = local{}
	return local{}
}

func (p *Pool) probe(ctx context.Context) error {
	c := Cmd{}
	c.Args = []string{"bisect", "reset"}
	return p.run(ctx, c)
}

func (p *Pool) named(ctx context.Context) (c Cmd) {
	c.Args = []string{"replay"}
	_ = p.run(ctx, c)
	return
}

func (c Cmd) launch(ctx context.Context, p *Pool) error { return p.run(ctx, c) }

func (p *Pool) launched(ctx context.Context) error {
	x := Cmd{}
	x.Args = []string{"bisect"}
	_ = Cmd{Args: []string{"version"}}.launch
	_ = Cmd.launch
	return x.launch(ctx, p)
}

func (p *Pool) spread(c Cmd) {
	p.all(c)
	p.keep(c)
	through(c)
	_ = hook(c)
}

func (p *Pool) all(...Cmd) {}

func (p *Pool) keep(any) {}

func through[T any](T) {}

func (p *Pool) replaced(ctx context.Context, c Cmd) error {
	b := Cmd{}
	b.Args = []string{"bisect"}
	c = b
	return p.run(ctx, c)
}

func (p *Pool) rewritten(ctx context.Context, c Cmd) error {
	c.Args = []string{"rerere"}
	c.Args[0] = "rerere"
	c.Args[0] += "-x"
	(c.Args)[0] = "rerere"
	c.Args[1:][0] = "rerere"
	for _, c.Args[0] = range []string{"rerere"} {
	}
	func() { c.Args = []string{"rerere"} }()
	copy(c.Args, []string{"rerere"})
	clear(c.Args[1:])
	more := append(c.Args[:0], "rerere")
	_ = more
	return p.run(ctx, c)
}

func (p *Pool) aliased(ctx context.Context, c Cmd) error {
	args := &c.Args
	d := c
	e := c.Args[1:]
	f, g := Cmd(c), local(c)
	h, k := []string(c.Args), (*[1]string)(c.Args)
	_, _, _, _, _, _, _ = args, d, e, f, g, h, k
	return p.run(ctx, c)
}

func (p *Pool) pointed(c *Cmd) *Cmd {
	*c = Cmd{}
	return (*Cmd)(c)
}

var _ = Cmd{Args: []string{"replay", "--onto", "x"}}

var _ = Cmd{Args: []string{"version"}}
`,
		// Never read: a harness, a test file and testdata.
		"internal/code/ztest/z.go":           "package ztest\n\nimport \"os\"\n\nvar _ = os.Remove\n",
		"internal/code/a/a_test.go":          "package a\n\nimport \"os\"\n\nvar _ = os.Remove\n",
		"internal/code/a/testdata/t.go":      "package t\n\nimport \"os\"\n\nvar _ = os.Remove\n",
		"internal/code/cache/placeholder.go": "package cache\n",
	}
	found, err := walkForWriters(syntheticTree(t, files))
	if err != nil {
		t.Fatalf("control: walking the synthetic tree: %v", err)
	}
	var got []string
	for _, h := range found.hits {
		got = append(got, h.file+" "+h.kind+" "+h.what)
	}
	slices.Sort(got)
	const handed = "a Cmd handed to %s that the call does not spell"
	const changed = "a Cmd it was handed, changed in place (parameter %s, %s)"
	want := []string{
		"internal/code/a/dot.go os os, imported with a dot: its calls carry no qualifier to judge",
		"internal/code/a/git.go git (arguments built at run time) arguments built at run time",
		"internal/code/a/git.go git commit-graph commit-graph",
		"internal/code/a/git.go git fsck fsck --lost-found",
		"internal/code/a/git.go git merge-tree merge-tree",
		"internal/code/a/git.go git rev-list rev-list --output",
		"internal/code/a/git.go git rev-list rev-list --output",
		"internal/code/a/git.go git rev-list rev-list --output",
		"internal/code/a/git.go git send-pack send-pack",
		"internal/code/a/git.go git update-ref update-ref",
		"internal/code/a/git.go git verify-pack verify-pack",
		"internal/code/a/rename.go os o.RemoveAll",
		"internal/code/a/rename.go os o.Rename",
		"internal/code/a/sys.go os syscall.Unlink",
		"internal/code/c/handoff.go git (arguments built at run time) " + fmt.Sprintf(handed, "defined(c).run"),
		"internal/code/c/handoff.go git (arguments built at run time) " + fmt.Sprintf(handed, "fmt.Println"),
		"internal/code/c/handoff.go git (arguments built at run time) " + fmt.Sprintf(handed, "many"),
		"internal/code/c/handoff.go git (arguments built at run time) " + fmt.Sprintf(handed, "one"),
		"internal/code/c/handoff.go git (arguments built at run time) " + fmt.Sprintf(handed, "p.Run"),
		"internal/code/c/handoff.go git (arguments built at run time) " + fmt.Sprintf(handed, "p.Run"),
		"internal/code/c/handoff.go git (arguments built at run time) " + fmt.Sprintf(handed, "p.Run"),
		"internal/code/c/handoff.go git (arguments built at run time) " + fmt.Sprintf(handed, "ptr"),
		"internal/code/c/handoff.go git (arguments built at run time) " + fmt.Sprintf(handed, "r.Run"),
		"internal/code/c/handoff.go git (arguments built at run time) " + fmt.Sprintf(handed, "run"),
		"internal/code/c/untyped.go git (arguments built at run time) " + fmt.Sprintf(handed, "many"),
		"internal/code/c/untyped.go git (arguments built at run time) a value the walk cannot type, handed to " +
			"fmt.Println, which it cannot type either — type the value (a declared variable, a typed constant) or " +
			"teach internal/sourcetree/decltype the shape",
		"internal/code/c/use.go git bisect bisect",
		"internal/code/c/use.go git filter-branch filter-branch",
		"internal/code/c/use.go git rerere rerere",
		"internal/code/c/use.go git sparse-checkout sparse-checkout",
		"internal/code/d/dot.go git (arguments built at run time) " + gitprocPath +
			", imported with a dot: its names carry no qualifier to type",
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(handed, "hook"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(handed, "p.all"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(handed, "p.keep"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(handed, "through"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(handed, "p.run"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(handed, "p.run"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(handed, "p.run"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(handed, "x.launch"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(changed, "c", "copied whole"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(changed, "c", "copied whole"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(changed, "c", "copied whole"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(changed, "c", "copied whole"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(changed, "c", "its Args copied"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(changed, "c", "its Args copied"),
		"internal/code/gitproc/own.go git (arguments built at run time) " + fmt.Sprintf(changed, "c", "its Args copied"),
		"internal/code/gitproc/own.go git (arguments built at run time) " +
			fmt.Sprintf(changed, "c", "its address or a part's taken"),
		"internal/code/gitproc/own.go git replay replay",
	}
	// Thirteen writes: replaced's one, rewritten's ten, a function literal's
	// parameter's and pointed's dereference.
	for range 13 {
		want = append(want, "internal/code/gitproc/own.go git (arguments built at run time) "+
			fmt.Sprintf(changed, "c", "written"))
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("control: the synthetic tree's hits are\n\t%s\nwant\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	// Thirteen files read: a's five, c's four, d's, cache's and gitproc's
	// two — never the harness's, the test file or testdata.
	if found.files != 13 || found.cacheFiles != 1 {
		t.Errorf("control: the walk read %d files (%d of cache), want 13 (1)", found.files, found.cacheFiles)
	}

	controlDerivedHalf(t)

	// A file the walk cannot parse is an error, never a file judged clean.
	broken := syntheticTree(t, map[string]string{
		"internal/code/gitproc/own.go":       files["internal/code/gitproc/own.go"],
		"internal/code/cache/placeholder.go": "package cache\n",
		"internal/code/b/broken.go":          "package b\n\nthis is not Go\n",
	})
	if _, err := walkForWriters(broken); err == nil || !strings.Contains(err.Error(), "broken.go") {
		t.Errorf("control: walking a tree holding a file that is not Go answered %v, want an error naming it", err)
	}
}

// controlDerivedHalf holds derivedHalf to verdicts known in advance, over
// declarations and rows made up: one break of each of its six rules, each
// reported once and nothing else — and the matching declaration, row and
// argument list alone, which report nothing. A derived half that went inert
// would certify the real tree's declarations whatever they said.
func controlDerivedHalf(t *testing.T) {
	t.Helper()
	const x, y = "internal/code/cache/x.go", "internal/code/cache/y.go"
	midx := cache.Derived{Name: cache.DerivedMidx, Writes: []cache.Write{{
		Step: cache.StepIndex, Trigger: cache.TriggerMaintainer,
		Writer: cache.Writer{File: x, Subcommand: "multi-pack-index"}, Args: []string{"multi-pack-index", "write"},
	}}}
	// A declaration naming a writer with no row, whose argument list its
	// file does not spell.
	graph := cache.Derived{Name: cache.DerivedGraph, Writes: []cache.Write{{
		Step: cache.StepGraph, Trigger: cache.TriggerMaintainer,
		Writer: cache.Writer{File: x, Subcommand: "commit-graph"}, Args: []string{"commit-graph", "write"},
	}}}
	argv := map[string][][]string{x: {{"multi-pack-index", "write"}}}
	pair := writerRow{file: x, kind: kindGit + " multi-pack-index", why: "the pair",
		derived: []cache.DerivedName{cache.DerivedMidx}}
	rows := []writerRow{
		pair,
		{file: x, kind: kindGit + " repack", why: "a git writer in the cache naming no declaration"},
		{file: x, kind: kindOS, why: "a name nobody declared", derived: []cache.DerivedName{"nonesuch"}},
		{file: y, kind: kindOS, why: "a file that is no writer's", derived: []cache.DerivedName{cache.DerivedMidx}},
		{file: x, kind: kindGit + " fsck", why: "a write its file does not hold",
			derived: []cache.DerivedName{cache.DerivedMidx}},
	}
	want := []string{
		`derived file "commit-graph" declares a maintainer write of step graph by internal/code/cache/x.go ` +
			`running git commit-graph, and no row`,
		`derived file "commit-graph" declares a maintainer write of step graph whose argument list ` +
			`["commit-graph" "write"] internal/code/cache/x.go does not spell whole`,
		`internal/code/cache/x.go runs git repack in the cache and its row names no declaration`,
		`names derived file "nonesuch", which DerivedFiles does not declare`,
		`names derived file "midx", and internal/code/cache/y.go is no declaration's writer`,
		`the row (internal/code/cache/x.go, git fsck) names derived file "midx", which declares no write`,
	}
	got := derivedHalf([]cache.Derived{midx, graph}, rows, argv)
	if len(got) != len(want) {
		t.Errorf("control: the derived half reported %d problems on a tree that breaks each of its rules once, "+
			"want %d:\n\t%s", len(got), len(want), strings.Join(got, "\n\t"))
	}
	for _, w := range want {
		if n := len(slices.DeleteFunc(slices.Clone(got), func(g string) bool { return !strings.Contains(g, w) })); n != 1 {
			t.Errorf("control: the derived half reported %d problems saying %q, want 1; it reported:\n\t%s", n, w,
				strings.Join(got, "\n\t"))
		}
	}
	if got := derivedHalf([]cache.Derived{midx}, []writerRow{pair}, argv); len(got) != 0 {
		t.Errorf("control: the derived half reported problems on a declaration, its row and its argument list "+
			"that match:\n\t%s", strings.Join(got, "\n\t"))
	}

	// AN IN-PROCESS DECLARATION — the cache's own code writes it, no git —
	// is tied to its file's filesystem row: with none naming it, reported
	// once; with one, nothing, though no argument list is spelled anywhere;
	// and an os row in that file naming nothing is no row of it.
	own := cache.Derived{Name: cache.DerivedProvenance, Writes: []cache.Write{{
		Step: cache.StepProvenance, Trigger: cache.TriggerRound, Writer: cache.Writer{File: y},
		Charge: "what it holds",
	}}}
	bare := writerRow{file: y, kind: kindOS, why: "an os row naming nothing"}
	got = derivedHalf([]cache.Derived{own}, []writerRow{bare}, nil)
	if want := `derived file "provenance" declares an in-process round write of step provenance by ` +
		`internal/code/cache/y.go, and no row`; len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("control: an in-process declaration whose file's os row names nothing reported:\n\t%s\nwant one "+
			"problem saying %q", strings.Join(got, "\n\t"), want)
	}
	tied := writerRow{file: y, kind: kindOS, why: "its row", derived: []cache.DerivedName{cache.DerivedProvenance}}
	if got := derivedHalf([]cache.Derived{own}, []writerRow{tied}, nil); len(got) != 0 {
		t.Errorf("control: the derived half reported problems on an in-process declaration and the os row naming "+
			"it:\n\t%s", strings.Join(got, "\n\t"))
	}
}

// syntheticTree writes files, by slash-separated path, under a new directory
// and returns it.
func syntheticTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
