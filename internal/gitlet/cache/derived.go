package cache

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/words"
)

// THE DERIVED FILES (ADR-0086; package doc, "Derived files"). Each node-local
// derived file is DECLARED here, and the sweep, Verify, check, scrub, budget,
// maintainer's plan and writers gate (TestNothingButSyncWritesACache) read the
// declaration: a kind is added by adding a declaration, never beside them, or
// a file a writer leaves that nothing admits is trashed under a running reader.
//
// A write is git's (a pool lane, a deadline, a per-object charge) or
// IN-PROCESS (Writer.Subcommand "": no git, no slot, its Charge says what it
// holds). Who READS a declaration's files decides its Part and root, never who
// writes them: git-read files live under objects/ and name a damage report's
// part; files no git reads live under crewlet/ (engineRoot) and name none, so
// the installed chain, the cache's own write of git's files, is under objects/.

// DerivedName names one declared kind of node-local derived file: a closed
// set whose one list is DerivedFiles.
type DerivedName string

const (
	// DerivedMidx is the multi-pack-index over an incarnation's packs: a full
	// round's, without a bitmap, once the snapshot names MidxRoundPacks packs,
	// and the maintainer's, with its bitmap where the node's memory holds one
	// (memory.go).
	DerivedMidx DerivedName = "midx"
	// DerivedGraph is the maintainer's commit-graph, written first on an
	// incarnation that has none (maintain.go, GRAPH FIRST).
	DerivedGraph DerivedName = "commit-graph"
	// DerivedProvenance is the index of which push introduced each commit and
	// every update each push landed on a ref (provenance.go, which says why it
	// is eager): written by a round's fold and by the backfill, and read by no
	// git process.
	DerivedProvenance DerivedName = "provenance"
	// DerivedMemo is the memo of answers by object id (memo.go): one file an
	// answer a server-side read cut, keyed by everything it is a function of,
	// under crewlet/memo, a directory it owns whole (Owned.Tree), and held to
	// MemoShare of the cache's budget.
	DerivedMemo DerivedName = "memo"
	// DerivedLayers is the INSTALLED PUBLISHED multi-pack-index chain
	// (layers.go): each layer's .midx and .bitmap installed byte for byte from
	// the object store once held to its row (gitlet.CheckLayer), and the chain
	// file written from the rows' order. No node writes a layer of its own
	// (layers.go says why).
	DerivedLayers DerivedName = "layers"
	// DerivedGraphLayers is the split commit-graph over the same chain: each
	// installed layer's commit-graph layer, the node's own TIP layer over the
	// tail (maintain.go), and the commit-graph-chain. Its own declaration,
	// since its files have their own directory and chain file; cut at the one
	// place the multi-pack-index chain is (prefixOf), so the chains never
	// disagree. Published layers and tip are ONE declaration, a write of each
	// kind, because both are graph-<checksum>.graph under one chain file,
	// which ownership by name cannot divide.
	DerivedGraphLayers DerivedName = "graph-layers"
	// DerivedCodeIndex is the trigram index of the default branch's tip
	// (codeindex.go, internal/codeindex): SEGMENTS of blobs' trigrams keyed by
	// blob id and a TIP MAP of the tip's files and blobs, under
	// crewlet/codeindex on the Gitlet nodes a `search: indexed` repository's
	// search is placed on (gitlet.IndexHolders). No git process reads it.
	DerivedCodeIndex DerivedName = "codeindex"
	// DerivedTags is the tag index (tags.go): every tag the copy's refs hold,
	// newest first by git's creatordate, levelled by a round that moves a tag
	// and placed with packed-refs.
	DerivedTags DerivedName = "tags"
)

// engineRoot is the one root directory besides objects/ a declaration may own
// files in: the engine's, for what the cache's own code writes. Not objects/
// (git's: a push's scratch names it as its one alternate, and git's tools walk
// it), nor refs/, hooks/, info/ or logs/ (a loose ref under refs/ outranks
// packed-refs for every reader), nor a root file's name.
const engineRoot = "crewlet"

// Valid reports whether n names a declaration of DerivedFiles.
func (n DerivedName) Valid() bool {
	return slices.ContainsFunc(DerivedFiles(), func(d Derived) bool { return d.Name == n })
}

// Input is what a derived file is a function of.
type Input string

const (
	// InputPacks is the incarnation's pack files: what an index merges and a
	// graph reads its commits from.
	InputPacks Input = "packs"
	// InputRefs is packed-refs: a bitmap's selected commits, a graph's
	// --reachable tips, and every tag the tag index lists.
	InputRefs Input = "refs"
	// InputPushRows is the code domain's push rows: those a delta's pushes
	// phase serves (gitlet.StateQuery.Pushes) and those the backfill pages
	// through (gitlet.ReadIntroduced), never the incarnation's files.
	InputPushRows Input = "push-rows"
	// InputOID is the objects an answer names, by id: immutable, so the
	// answer holds whatever packs hold them and whatever refs point at them.
	InputOID Input = "oid"
	// InputLayers is the published layer rows (code_layers, code_layer_parts)
	// a snapshot serves (gitlet.RepoSnapshot.Layers): each layer's packs, its
	// place in the chain, and the parts its bytes are read back from.
	InputLayers Input = "layers"
)

// Inputs is every Input.
var Inputs = []Input{InputPacks, InputRefs, InputPushRows, InputOID, InputLayers}

// Valid reports whether i is one of Inputs.
func (i Input) Valid() bool { return slices.Contains(Inputs, i) }

// Trigger is what starts a write.
type Trigger string

const (
	// TriggerRound is a round of a sync, before it answers; which rounds is
	// the write's own (a full one past MidxRoundPacks packs for the index,
	// every delta that served pushes for the provenance fold).
	TriggerRound Trigger = "round"
	// TriggerMaintainer is the maintainer's pass: debounced, duty-cycled and
	// admitted on the node's memory.
	TriggerMaintainer Trigger = "maintainer"
	// TriggerBackfill is the cache's background loop over what a round left
	// to read (provenance.go): a page of push rows, or a merge of runs, paced
	// by the fleet behind the serving.
	TriggerBackfill Trigger = "backfill"
	// TriggerRead is a server-side read by object id that ran its git
	// (internal/gitlet/query): the pages its one run cut, kept for the next
	// read that asks (memo.go).
	TriggerRead Trigger = "read"
	// TriggerInstall is a published layer placed from the object store
	// (layers.go): by the FULL round that first reads the chain, before it
	// answers, else by the installer behind the serving once a delta names a
	// layer the copy lacks.
	TriggerInstall Trigger = "install"
	// TriggerIndexer is the cache's indexer (codeindex.go), on a node a
	// `search: indexed` repository's search is placed on, behind the serving,
	// once the default branch has moved past its index's tip.
	TriggerIndexer Trigger = "indexer"
)

// Triggers is every Trigger.
var Triggers = []Trigger{TriggerRound, TriggerMaintainer, TriggerBackfill, TriggerRead, TriggerInstall,
	TriggerIndexer}

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
	// FreshAtRound is current at the incarnation's position whenever a round
	// has answered, over the range its coverage names (gitlet.Coverage): an
	// answer about a push outside it says so rather than answering none. The
	// tag index is current whole, placed with packed-refs.
	FreshAtRound Freshness = "at-round"
	// FreshImmutable is keyed by everything its answer is a function of
	// (object ids and the read's arguments), so it is never stale, only
	// absent: a reader that finds no entry runs its git.
	FreshImmutable Freshness = "immutable"
	// FreshInstalled is installed by the full round that first reads it, else
	// by the installer within one pass of the round that names it; until then
	// the copy serves from the longest prefix it holds, or in own mode. Never
	// a correctness promise: git reads every pack outside the chain beside it.
	FreshInstalled Freshness = "installed"
	// FreshAtTip is current at the tip it names only (Cache.IndexTip): a
	// search of any other commit reads with git grep, so an index behind its
	// branch answers nothing wrong, only less of it from the index.
	FreshAtTip Freshness = "at-tip"
)

// Freshnesses is every Freshness.
var Freshnesses = []Freshness{FreshDebounced, FreshAtRound, FreshImmutable, FreshInstalled, FreshAtTip}

// Valid reports whether f is one of Freshnesses.
func (f Freshness) Valid() bool { return slices.Contains(Freshnesses, f) }

// Check is how a derived file this process did not write is held to its
// bytes before it serves, and how the scrub holds it behind the serving
// (check.go, scrub.go).
type Check string

const (
	// CheckTrailer is git's hashfile trailer (the last twenty bytes the SHA-1
	// of every byte before them) and, for an Owned.Prior file, its name's
	// checksum in the twenty before it. git checks none of it when it reads.
	CheckTrailer Check = "trailer"
	// CheckRunTrailer is a run's own trailer: the last 32 bytes the SHA-256
	// of every byte before them (provenance.go). Nothing the cache reads of a
	// run is read from one this process did not write or check.
	CheckRunTrailer Check = "run-trailer"
	// CheckEntryDigest is a memo entry's header: its key and the SHA-256 of
	// its body (memo.go), held on EVERY read whoever wrote the entry, so the
	// check and the scrub pass over such files (Check.hashed).
	CheckEntryDigest Check = "entry-digest"
	// CheckLayers is the installed chain's (layers.go): each layer file a git
	// hashfile held to its trailer, an Owned.Named one also to its name; the
	// CHAIN FILE, which carries no trailer, held line for line to the ids the
	// incarnation installed. A failing layer file CUTS the chain there, to be
	// fetched again with every layer above; a failing chain file is written
	// again. Never a drop to own mode.
	CheckLayers Check = "layers"
)

// Checks is every Check.
var Checks = []Check{CheckTrailer, CheckRunTrailer, CheckEntryDigest, CheckLayers}

// Valid reports whether c is one of Checks.
func (c Check) Valid() bool { return slices.Contains(Checks, c) }

// hashed reports whether the check and the scrub hold files checked by c to
// their bytes (ownedFile.held): all but CheckEntryDigest's, which every read
// holds; hashed behind the serving too, a memo of gigabytes would be read
// whole by every scrub for nothing a read does not already refuse.
func (c Check) hashed() bool { return c != CheckEntryDigest }

// Event is a change the cache makes in an incarnation that a derived file may
// be built over: what dropFor is told, at the one place each is made.
type Event string

const (
	// EventPackAdded is a pack installed into the incarnation: a round's
	// download, an adopted push.
	EventPackAdded Event = "pack-added"
	// EventPackRemoved is a pack's files taken out, or renamed over where its
	// index may differ: a full round's stray no retirement accounts for (a log
	// gone back or recreated), or a pack installed again over files of another
	// size than its row's or whose index failed its checksum. A file built
	// over the old files may name what is gone, and commits may be gone too.
	EventPackRemoved Event = "pack-removed"
	// EventRefsMoved is a new packed-refs renamed into place by a round.
	EventRefsMoved Event = "refs-moved"
	// EventReported is a git process's report that the declaration's own
	// files are damaged (gitproc.Damage naming its Part): git's word names a
	// part, never the bytes. A declaration that KEEPS its files on a report
	// has them held to its Check instead (the report makes the copy's scrub
	// due, report.go), and the request that met it is repaired as a pack's
	// damage is (Cache.Rebuild).
	EventReported Event = "reported"
	// EventHistoryGone is the code log under the incarnation no longer the
	// history it was read from (recreated, re-anchored onto another
	// generation, or gone back), so any position it recorded may name another
	// record. Made by the full round that reads the incarnation whole for it,
	// BEFORE its sweep and before any git process sees the round's image
	// (sync.go apply); the packs the restored history no longer names leave
	// in that sweep as EventPackRemoved.
	EventHistoryGone Event = "history-gone"
	// EventPackRetired is a pack file taken out because a COMPACTION RETIRED
	// it (code_packs_retired): a compaction keeps every object of its inputs
	// in its outputs, which the same round installs first; an expunge's BEGIN
	// retires its inputs as this event too. A file built over packs' NAMES or
	// OFFSETS meets a pack gone; one over COMMITS or OBJECT IDS meets nothing
	// that left. Made by the round whose answer names the retirement (swap.go).
	EventPackRetired Event = "pack-retired"
	// EventPackExpunged is a pack taken out of the DROPPING VIEW by an
	// EXPUNGE'S COMMIT (gitlet.ExpungeCommit), the one change that takes
	// objects out of the repository (package doc, "The dropping view"). A
	// file built over the copy's COMMITS or ANSWERS meets objects gone; the
	// multi-pack-index and the published layers, which never cover the view,
	// meet nothing. Made by a delta's retired phase naming a row held
	// dropping, or a full round's stray in the view no row names.
	EventPackExpunged Event = "pack-expunged"
	// EventLayersMoved is the installed published chain changing (layers.go):
	// a layer placed, or the chain cut to a prefix. git prefers a
	// non-incremental multi-pack-index or a non-split commit-graph to a chain,
	// and a whole write over a chain deletes it, so the modes never coexist
	// and an own-mode file answers the chain moving with a drop.
	EventLayersMoved Event = "layers-moved"

	// eventFailedCheck is a file of the declaration FAILING ITS CHECK (the
	// round's check, the scrub): dropped whatever the declaration says of a
	// report (dropFor, image.dropping), since it is never served and kept it
	// would fail every scrub again. Unexported and not among Events, so no
	// declaration states it.
	eventFailedCheck Event = "failed-check"
)

// Events is every Event a declaration answers, in the order it states them.
var Events = []Event{EventPackAdded, EventPackRemoved, EventRefsMoved, EventReported, EventHistoryGone,
	EventPackRetired, EventPackExpunged, EventLayersMoved}

// Valid reports whether e is one of Events.
func (e Event) Valid() bool { return slices.Contains(Events, e) }

// Change is what an Event does to a declared derived file.
type Change string

const (
	// Keep means still valid: git reads what is new beside it.
	Keep Change = "keep"
	// Drop means invalid or untrusted: moved into trash/ before the change is
	// visible to any git process (dropFor).
	Drop Change = "drop"
	// Prefix is the installed chain's answer (layers.go): keep the longest
	// PREFIX whose every layer names none of the event's packs and, for
	// EventLayersMoved, matches the chain wanted layer for layer; trash the
	// rest before the change is visible and write the chain files again (or
	// remove them). A layer is valid only over its exact base, which only the
	// chain's order names, so every layer above a cut goes with it.
	Prefix Change = "prefix"
)

// Changes is every Change.
var Changes = []Change{Keep, Drop, Prefix}

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
	// Main: the file whose presence is the declaration's (the index, the
	// graph), every other owned file a part of it. Exactly one, a literal
	// Name, for a git-written declaration; at most one for an in-process one,
	// which may be present by its record instead (Cache.recordDerived).
	Main bool
	// Prior: the forty hex digits of its name (the one capture group) are the
	// checksum in the twenty bytes before its trailer, as a pack's .rev
	// carries its pack's (git's write_rev_trailer).
	Prior bool
	// Named: the forty hex digits of its name (the one capture group) are its
	// OWN trailer (multi-pack-index-<id>.midx, graph-<id>.graph). git opens it
	// by the name its chain file lists and reads no trailer, so the name is
	// all that ties the file to its row (check.go's expect.trailer).
	Named bool
	// Tree: Name is one literal DIRECTORY an in-process declaration owns
	// WHOLE (crewlet/memo). The image keeps it and Verify admits it without
	// reading inside; no measure stats it, since its bytes are its writer's
	// running total (memo.go) and a walk on a round's path would cost a stat
	// an entry. No declaration's files live under it.
	Tree bool
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
	// StepTipGraph is the node's TIP commit-graph layer over an installed
	// chain (maintain.go): the tail's commits, over exactly the installed
	// commit-graph layers.
	StepTipGraph Step = "tip-graph"
	// StepProvenance is a run of the provenance index: a round's fold, a
	// backfilled page or a merge. Never refused for memory, so no value of
	// the maintenance refusals' counter or gauge, which name git's steps.
	StepProvenance Step = "provenance"
	// StepMemo is an entry of the memo: an answer a read cut. Never refused
	// for memory either.
	StepMemo Step = "memo"
	// StepLayer is a published layer fetched, held to its row and placed, and
	// the chain files written from the rows (layers.go). Never refused for
	// memory either: it runs no git, and holds a part's copy buffer and a
	// fetch slot, as a pack's download does.
	StepLayer Step = "layer"
	// StepCodeIndex is the trigram index of the default branch's tip: a
	// segment, a merge of segments, and the tip map (codeindex.go). Never
	// refused for memory either: its git only reads, and it holds one
	// segment's postings.
	StepCodeIndex Step = "codeindex"
	// StepTags is the tag index, merged or written whole by a round that
	// moves a tag. Never refused for memory either: the git it runs only reads.
	StepTags Step = "tags"
)

// Steps is every Step, in the order a line or a count names them.
var Steps = []Step{StepBitmap, StepIndex, StepGraph, StepTipGraph, StepProvenance, StepMemo, StepLayer,
	StepCodeIndex, StepTags}

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

// Writer is the code that runs a write: the non-test file under internal/gitlet,
// slash-separated from the module root, and the git subcommand it runs there
// — "" for an IN-PROCESS write, the cache's own Go code writing the file.
// TestNothingButSyncWritesACache holds it to that file's allowance row: a git
// write's subcommand's, an in-process write's filesystem row.
type Writer struct {
	File       string
	Subcommand string
}

// Write is one way a declared derived file is written.
type Write struct {
	Step    Step
	Trigger Trigger
	// Lane is the pool's lane git runs the write in; "" for an in-process
	// write, which runs no git and holds no slot of any lane.
	Lane gitproc.Class
	// Deadline is the write's process bound over what it covers; nil for an
	// in-process write, which its caller's context bounds.
	Deadline func(Covers) time.Duration
	// BytesPerObject is the write's own memory an object of Covers.Objects
	// (the repository's in own mode, the tail's for the node's tip), admitted
	// at Write.charge, which adds room for one transfer of the whole
	// repository (memory.go). Zero for an in-process write.
	BytesPerObject int64
	// BaseBytes is the write's own memory WHATEVER it covers, beside
	// BytesPerObject's: every commit-graph write's filter diffs filling git's
	// delta base cache to its cap (GraphBaseBytes), the bitmapless index's
	// merge (IndexBaseBytes). Zero where the per-object figure absorbs any
	// fixed part, and for an in-process write.
	BaseBytes int64
	Writer    Writer
	// Args is the argument list its writer hands git: one of the writer
	// file's own literals, so the writers gate reads it where git runs. Nil
	// for an in-process write.
	Args []string
	// Charge is an IN-PROCESS write's one-sentence account of what it holds,
	// set exactly when Writer.Subcommand is "": with no slot and no process,
	// a per-object charge or a deadline would be a figure nothing reads.
	Charge string
	// Holds is what THIS write holds only while it runs: Verify admits it only
	// then, and clearWriting removes it after a failed run (git leaves a lock
	// when killed, a temporary file either way). Per write, not per
	// declaration, since two writes of one file hold different files (the
	// bitmap write's temporary file), so a failure is cleared of exactly what
	// it may have left, a literal by name with no listing. Empty for a write
	// staged elsewhere and renamed in.
	Holds []FilePattern
}

// SeedRule is what a rebuild does with a declaration's files in the
// incarnation it replaces (rebuild.go's seedRebuild and promote).
type SeedRule string

const (
	// SeedNone means the new incarnation writes them again: functions of the
	// packs, which may be what failed in the copy being replaced.
	SeedNone SeedRule = "none"
	// SeedLink hard-links them in with their record: functions of the ROWS,
	// which a rebuild reads again unchanged. A link is the same inode, so a
	// file this process vouched for stays vouched for, and one it has not is
	// held to its Check before anything reads it.
	SeedLink SeedRule = "link"
	// SeedMove MOVES the directory the declaration owns whole (Owned.Tree) in
	// ONE rename, its record and in-memory half with it (memo.go's
	// carryMemo): linked an entry at a time, a hot memo would cost a stat and
	// a link an answer inside the round a request's repair waits on, and on
	// every node a compaction's swap reaches. Moved at the PROMOTION, so the
	// old incarnation serves it until the new one does and a failed rebuild
	// leaves it; a reader holding the old one misses from then on. Only a
	// tree moves, and a tree is never linked (Validate).
	SeedMove SeedRule = "move"
)

// SeedRules is every SeedRule.
var SeedRules = []SeedRule{SeedNone, SeedLink, SeedMove}

// Valid reports whether r is one of SeedRules.
func (r SeedRule) Valid() bool { return slices.Contains(SeedRules, r) }

// Derived is one declared kind of node-local derived file.
type Derived struct {
	Name   DerivedName
	Inputs []Input
	Writes []Write
	// Owns is what the image keeps (sweep.go's imaged).
	Owns []Owned
	// Check is how its files are held to their bytes (check.go, scrub.go).
	Check Check
	// Part is the part a damage report names it by (one gitproc.Damage.Drops
	// answers), for a declaration whose files git READS, whoever writes them;
	// a report naming it is its EventReported. Zero where no git process
	// reads it, so no report can name it.
	Part      gitproc.Part
	Freshness Freshness
	// What each Event does to it.
	OnPackAdded, OnPackRemoved, OnRefsMoved, OnReported, OnHistoryGone, OnPackRetired, OnPackExpunged,
	OnLayersMoved Change
	// Seed is what a rebuild does with its files.
	Seed SeedRule
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
	case EventHistoryGone:
		return d.OnHistoryGone
	case EventPackRetired:
		return d.OnPackRetired
	case EventPackExpunged:
		return d.OnPackExpunged
	case EventLayersMoved:
		return d.OnLayersMoved
	}
	return ""
}

// inProcess reports whether d's files are the cache's own Go code's alone to
// write — every write of it an in-process one (Write.Charge) — and no git
// process's.
func (d *Derived) inProcess() bool {
	return len(d.Writes) > 0 && !d.gitWritten()
}

// gitWritten reports whether a git process writes any of d's files
// (Writer.Subcommand). git reads what it writes, so Validate holds such a
// declaration to a Part, to one Main file, and to no Tree.
func (d *Derived) gitWritten() bool {
	return slices.ContainsFunc(d.Writes, func(w Write) bool { return w.Writer.Subcommand != "" })
}

// gitReads reports whether a git process reads d's files: whether it names a
// Part. Where its files live (validateAll), whether the round's check holds
// them before a copy serves (check.go) and whether dropping one can end a git
// failure (droppedDerived) all follow this, never who writes them.
func (d *Derived) gitReads() bool { return d.Part != 0 }

// chained reports whether d is one of the installed published chain's
// declarations (CheckLayers): its files are placed and cut by the chain's own
// code (layers.go), never written over or dropped whole.
func (d *Derived) chained() bool { return d.Check == CheckLayers }

// hasMain reports whether d owns a file whose presence is its own
// (Owned.Main); one that owns none is present where its record says so.
func (d *Derived) hasMain() bool {
	return slices.ContainsFunc(d.Owns, func(o Owned) bool { return o.Main })
}

// tree reports whether d's main file is a directory it owns whole
// (Owned.Tree): its bytes are its writer's running total, never a measure's.
func (d *Derived) tree() bool {
	return slices.ContainsFunc(d.Owns, func(o Owned) bool { return o.Main && o.Tree })
}

// The patterns of DerivedFiles, compiled once at FIRST USE and shared (a
// regexp is safe to share) by every copy DerivedFiles answers. Never at init: Gitlet's proc-receive hook
// is this binary, exec'd once a push, and runs every linked package's init
// before its first statement (cmd/crewlet's
// TestTheHookPaysLittleForTheEnginesInit), and a forty-digit checksum pattern
// compiles to an instruction a digit, nearly all this package would allocate
// at init.
var (
	midxMain   = lazyPattern(`^multi-pack-index$`)
	midxBitmap = lazyPattern(`^multi-pack-index-[0-9a-f]{40}\.bitmap$`)
	midxRev    = lazyPattern(`^multi-pack-index-([0-9a-f]{40})\.rev$`)
	midxLock   = lazyPattern(`^multi-pack-index\.lock$`)
	tmpBitmap  = lazyPattern(`^tmp_bitmap_[A-Za-z0-9]+$`)
	graphMain  = lazyPattern(`^commit-graph$`)
	graphLock  = lazyPattern(`^commit-graph\.lock$`)
	// provenanceRunName is a run of the provenance index:
	// crewlet/provenance.<n>, n the run's number in decimal from 1, as the
	// index's record names it (provenanceRun).
	provenanceRunName = lazyPattern(`^provenance\.[1-9][0-9]*$`)
	// memoTreeName is the memo's directory, crewlet/memo, which it owns whole
	// (Owned.Tree): every entry in it an answer, named by its key's hex.
	memoTreeName = lazyPattern(`^memo$`)
	// The installed chain's files, as git 2.56.0 names them (midx.c's
	// get_midx_chain_filename and get_split_midx_filename_ext, commit-graph.c's
	// get_commit_graph_chain_filename and get_split_graph_filename). No .rev:
	// git writes a layer's reverse index as its RIDX chunk.
	layerChainName  = lazyPattern(`^multi-pack-index-chain$`)
	layerMidxName   = lazyPattern(`^multi-pack-index-([0-9a-f]{40})\.midx$`)
	layerBitmapName = lazyPattern(`^multi-pack-index-[0-9a-f]{40}\.bitmap$`)
	graphChainName  = lazyPattern(`^commit-graph-chain$`)
	graphLayerName  = lazyPattern(`^graph-([0-9a-f]{40})\.graph$`)
	// What the node's TIP write holds while it runs (git 2.56.0's
	// write_commit_graph_file): the chain file's lock, from the layer's start
	// to the chain's commit, and the layer as a temporary file (mks_tempfile_m
	// of "tmp_graph_XXXXXX") until it is renamed graph-<checksum>.graph.
	graphChainLock = lazyPattern(`^commit-graph-chain\.lock$`)
	tmpGraph       = lazyPattern(`^tmp_graph_[A-Za-z0-9]{6}$`)
	// The code index's files under crewlet/codeindex (codeIndexDir): seg.<n>
	// and tip.<n>, n in decimal from 1, as its record names them
	// (codeIndexState).
	codeIndexSegName = lazyPattern(`^seg\.[1-9][0-9]*$`)
	codeIndexTipName = lazyPattern(`^tip\.[1-9][0-9]*$`)
	// tagsName is the tag index, crewlet/tags (tags.go).
	tagsName = lazyPattern(`^tags$`)
)

// lazyPattern is expr compiled at first use and shared (sync.OnceValue);
// every file-name pattern of this package is one. A bad pattern panics at
// first use rather than at init, and the tests compile every one through
// DerivedFiles' validation.
func lazyPattern(expr string) func() *regexp.Regexp {
	return sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(expr) })
}

// The installed chain's two directories, relative to an incarnation.
const (
	layersDir      = "objects/pack/multi-pack-index.d"
	graphLayersDir = "objects/info/commit-graphs"
)

// DerivedFiles is the closed list, in the order a pass plans its steps and a
// line names them: a fresh copy each call, so no caller can change what
// another reads.
//
// The multi-pack-index's REVERSE INDEX is owned although git ≥ 2.41 writes it
// as a chunk (a .rev file only under GIT_TEST_MIDX_WRITE_REV): a file git may
// write that the image does not admit is trashed under a running reader. A
// bitmap's tmp_bitmap_XXXXXX (pack-bitmap-write.c) is registered for no
// cleanup, so a writer killed late in its run leaves it. Every in-process
// write stages its files in tmp/ and renames them in, so it holds none.
func DerivedFiles() []Derived {
	maintained := func(c Covers) time.Duration { return gitproc.PackWorkDeadline(c.DiskBytes) }
	return []Derived{{
		Name:   DerivedMidx,
		Inputs: []Input{InputPacks, InputRefs},
		Writes: []Write{
			{Step: StepIndex, Trigger: TriggerRound, Lane: gitproc.ClassRead,
				Deadline:       func(c Covers) time.Duration { return gitproc.PackWorkDeadline(c.IndexBytes) },
				BytesPerObject: IndexBytesPerObject, BaseBytes: IndexBaseBytes, Writer: Writer{maintainFile, procIndex},
				Args:  roundIndexArgs,
				Holds: []FilePattern{{"objects/pack", midxLock()}}},
			{Step: StepBitmap, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance, Deadline: maintained,
				BytesPerObject: BitmapBytesPerObject, Writer: Writer{maintainFile, procIndex}, Args: bitmapArgs,
				Holds: []FilePattern{{"objects/pack", midxLock()}, {"objects/pack", tmpBitmap()}}},
			{Step: StepIndex, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance, Deadline: maintained,
				BytesPerObject: IndexBytesPerObject, BaseBytes: IndexBaseBytes, Writer: Writer{maintainFile, procIndex},
				Args:  plainArgs,
				Holds: []FilePattern{{"objects/pack", midxLock()}}},
		},
		Owns: []Owned{
			{FilePattern: FilePattern{"objects/pack", midxMain()}, File: "multi-pack-index", Main: true},
			{FilePattern: FilePattern{"objects/pack", midxBitmap()}, File: "bitmap"},
			{FilePattern: FilePattern{"objects/pack", midxRev()}, File: "midx-rev", Prior: true},
		},
		Check:     CheckTrailer,
		Part:      gitproc.PartMultiPackIndex,
		Freshness: FreshDebounced,
		// A pack RETIRED drops it as one removed does: it names each pack by
		// checksum and each object by offset, so a pack that left fails every
		// lookup reaching it. The chain MOVING drops it: git prefers OWN
		// mode's index to a chain (layers.go). An expunge's commit KEEPS it:
		// no index covers the dropping view.
		OnPackAdded: Keep, OnPackRemoved: Drop, OnRefsMoved: Keep, OnReported: Drop, OnHistoryGone: Keep,
		OnPackRetired: Drop, OnPackExpunged: Keep, OnLayersMoved: Drop,
		Seed: SeedNone,
	}, {
		Name:   DerivedGraph,
		Inputs: []Input{InputPacks, InputRefs},
		Writes: []Write{{Step: StepGraph, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance,
			Deadline: maintained, BytesPerObject: GraphBytesPerObject, BaseBytes: GraphBaseBytes,
			Writer: Writer{maintainFile, procGraph}, Args: graphArgs,
			Holds: []FilePattern{{"objects/info", graphLock()}}}},
		Owns:      []Owned{{FilePattern: FilePattern{"objects/info", graphMain()}, File: "commit-graph", Main: true}},
		Check:     CheckTrailer,
		Part:      gitproc.PartCommitGraph,
		Freshness: FreshDebounced,
		// A pack RETIRED KEEPS IT (X04): a compaction drops no object, and git
		// reads the graph by commit id. A pack REMOVED with no retirement may
		// take commits with it. The chain MOVING drops it as it does the
		// index: git loads a non-split graph before the chain. An EXPUNGE'S
		// COMMIT drops it (X04's one drop): over a graph naming an expunged
		// commit git answers a walk (`rev-list -1 <it>`) as though it were
		// there, and the connectivity check fails (measured on 2.56.0). Its begin keeps it.
		OnPackAdded: Keep, OnPackRemoved: Drop, OnRefsMoved: Keep, OnReported: Drop, OnHistoryGone: Keep,
		OnPackRetired: Keep, OnPackExpunged: Drop, OnLayersMoved: Drop,
		Seed: SeedNone,
	}, {
		Name:   DerivedProvenance,
		Inputs: []Input{InputPushRows},
		Writes: []Write{
			{Step: StepProvenance, Trigger: TriggerRound, Writer: Writer{File: provenanceFile},
				Charge: "a delta's pushes, which its snapshot already holds, folded into one run streamed to tmp/: " +
					"the run's commits and moves sorted in memory, at most the delta's own rows"},
			{Step: StepProvenance, Trigger: TriggerBackfill, Writer: Writer{File: provenanceFile},
				Charge: "a page of push rows (gitlet.MaxPageBytes) as one run, or a merge of adjacent runs streamed " +
					"from their mapped files through one buffer — nothing held a push or a ref, but one key's " +
					"ordinals"},
		},
		Owns:      []Owned{{FilePattern: FilePattern{engineRoot, provenanceRunName()}, File: "provenance"}},
		Check:     CheckRunTrailer,
		Freshness: FreshAtRound,
		// Its input is the push rows, which no pack, ref, layer or expunge
		// moves, and no git process reads it, so no report names it; a log
		// gone back makes every position it records another record's.
		OnPackAdded: Keep, OnPackRemoved: Keep, OnRefsMoved: Keep, OnReported: Keep, OnHistoryGone: Drop,
		OnPackRetired: Keep, OnPackExpunged: Keep, OnLayersMoved: Keep,
		Seed: SeedLink,
	}, {
		Name:   DerivedMemo,
		Inputs: []Input{InputOID},
		Writes: []Write{{Step: StepMemo, Trigger: TriggerRead, Writer: Writer{File: memoFile},
			Charge: "the pages one read's run cuts, each staged in tmp/ as it is cut and renamed in once the run " +
				"has answered whole: one page of at most gitlet.MaxGitAnswerBytes held at a time, and of a diff's " +
				"stat the counts of one page's files, about 24 bytes a file, written into that page's staged " +
				"entry once the stat has counted past it (MemoBatch.Extend)"}},
		Owns:      []Owned{{FilePattern: FilePattern{engineRoot, memoTreeName()}, File: "memo", Main: true, Tree: true}},
		Check:     CheckEntryDigest,
		Freshness: FreshImmutable,
		// An object id names one object for ever, so no pack, ref or layer
		// change alters an answer keyed by the ids it read, and no git process
		// reads it, so no report names it. A log gone back is another history,
		// and an expunge's commit may have purged what an answer holds; the
		// memo cannot tell which entries read what, so either drops it whole.
		// Its memory is its cohorts', never an entry's (package doc, "The memo").
		OnPackAdded: Keep, OnPackRemoved: Keep, OnRefsMoved: Keep, OnReported: Keep, OnHistoryGone: Drop,
		OnPackRetired: Keep, OnPackExpunged: Drop, OnLayersMoved: Keep,
		// A rebuild repairs packs, not answers, and a swap keeps every object:
		// the tree moves whole, one rename, and every read still holds an
		// entry to its key and digest.
		Seed: SeedMove,
	}, {
		Name:   DerivedLayers,
		Inputs: []Input{InputPacks, InputLayers},
		Writes: []Write{{Step: StepLayer, Trigger: TriggerInstall, Writer: Writer{File: layersFile},
			Charge: "a layer's files streamed from the object store a part at a time, each part one fetch slot " +
				"and its copy buffer as a pack's download is, into tmp/ and renamed in once the layer is held to " +
				"its row; the chain file from the rows, a line a layer"}},
		Owns: []Owned{
			{FilePattern: FilePattern{layersDir, layerChainName()}, File: "layers-chain", Main: true},
			{FilePattern: FilePattern{layersDir, layerMidxName()}, File: "layer-midx", Named: true},
			{FilePattern: FilePattern{layersDir, layerBitmapName()}, File: "layer-bitmap"},
		},
		Check:     CheckLayers,
		Part:      gitproc.PartMultiPackIndex,
		Freshness: FreshInstalled,
		// A layer names its packs by checksum and its objects by offset, so a
		// pack it names leaving cuts the chain there, and the chain moving cuts
		// it to the prefix the rows still name; a pack added stays outside the
		// chain until the next publication. A report keeps it and the scrub
		// decides (a drop could only cut, and git does not say which layer it
		// met); a log gone back reaches it as the chain moving. No layer names
		// a pack an expunge takes out: nothing publishes while one is open.
		OnPackAdded: Keep, OnPackRemoved: Prefix, OnRefsMoved: Keep, OnReported: Keep, OnHistoryGone: Keep,
		OnPackRetired: Prefix, OnPackExpunged: Prefix, OnLayersMoved: Prefix,
		// Linked where every pack a layer names is seeded and the layer passes
		// gitlet.CheckLayer there (rebuild.go's seedLayers): a cold copy would
		// otherwise clone without a bitmap until the chain was fetched again.
		Seed: SeedLink,
	}, {
		Name:   DerivedGraphLayers,
		Inputs: []Input{InputPacks, InputRefs, InputLayers},
		Writes: []Write{
			{Step: StepLayer, Trigger: TriggerInstall, Writer: Writer{File: layersFile},
				Charge: "installed with its layer, from the same parts — the commit-graph layer is the last " +
					"section of a layer's bytes — and the commit-graph-chain from the rows, a line a layer that " +
					"carries one"},
			// THE NODE'S TIP over the tail (maintain.go): its Covers are the
			// tail's, since a split write walks only the commits no layer beneath
			// holds, beside its filters' fixed part (GraphBaseBytes).
			{Step: StepTipGraph, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance,
				Deadline: func(c Covers) time.Duration {
					return gitproc.PackWorkDeadline(c.DiskBytes) + gitproc.HistoryWalk(c.Objects)
				},
				BytesPerObject: TipGraphBytesPerObject, BaseBytes: GraphBaseBytes,
				Writer: Writer{maintainFile, procGraph}, Args: tipGraphArgs,
				Holds: []FilePattern{{graphLayersDir, graphChainLock()}, {graphLayersDir, tmpGraph()}}},
		},
		Owns: []Owned{
			{FilePattern: FilePattern{graphLayersDir, graphChainName()}, File: "graph-chain", Main: true},
			{FilePattern: FilePattern{graphLayersDir, graphLayerName()}, File: "graph-layer", Named: true},
		},
		Check:     CheckLayers,
		Part:      gitproc.PartCommitGraph,
		Freshness: FreshInstalled,
		// Cut wherever the multi-pack-index chain is (prefixOf). The node's
		// tip goes with every cut, placement and pack removed, and with an
		// expunge's commit, which may take commits it names, as own mode's
		// commit-graph does; a retirement that keeps the whole chain keeps it
		// (layers.go's tipGoes), as do the refs moving.
		OnPackAdded: Keep, OnPackRemoved: Prefix, OnRefsMoved: Keep, OnReported: Keep, OnHistoryGone: Keep,
		OnPackRetired: Prefix, OnPackExpunged: Prefix, OnLayersMoved: Prefix,
		// The published graph layers only, with their layers (rebuild.go's
		// seedLayers): no row names the node's tip, so a rebuild writes it
		// again over its new copy's tail.
		Seed: SeedLink,
	}, {
		Name:   DerivedCodeIndex,
		Inputs: []Input{InputRefs, InputOID},
		Writes: []Write{{Step: StepCodeIndex, Trigger: TriggerIndexer, Writer: Writer{File: codeIndexFile},
			Charge: "one segment's postings: at most codeindex.SegmentSourceBytes of source read through one " +
				"cat-file, each blob's bytes held only while its trigrams are taken, and the tip's listing — " +
				"every path, blob and size — for the whole build, about 260 bytes a file; every file streamed " +
				"into its staged file and every segment it placed read back through a mapping of its file, never " +
				"held, so a merge holds its inputs' tables of blobs, the merged trigram table it writes last and " +
				"two buffers of one trigram's postings"}},
		// By name, with NO MAIN: its record (codeIndexState) names the
		// segments and tip map. NOT a tree: its files are few
		// (codeindex.MaxSegments and a tip map), carried across incarnations
		// by HARD LINK (S18), and Validate refuses a linked tree.
		Owns: []Owned{
			{FilePattern: FilePattern{codeIndexDir, codeIndexSegName()}, File: "codeindex-segment"},
			{FilePattern: FilePattern{codeIndexDir, codeIndexTipName()}, File: "codeindex-tip"},
		},
		// Each file carries a SHA-256 trailer over every byte before it
		// (internal/codeindex's formats): the provenance run's rule.
		Check:     CheckRunTrailer,
		Freshness: FreshAtTip,
		// Postings are keyed by blob id, so no pack or layer change alters
		// one, and the refs moving leave them standing while the tip map says
		// which tip they answer for. No git process reads it, so no report
		// names it. A log gone back is another history; an EXPUNGE'S COMMIT
		// drops it whole, or a search would find a purged blob's text (M6-D5).
		OnPackAdded: Keep, OnPackRemoved: Keep, OnRefsMoved: Keep, OnReported: Keep, OnHistoryGone: Drop,
		OnPackRetired: Keep, OnPackExpunged: Drop, OnLayersMoved: Keep,
		// Blob ids alone: a rebuild and a swap link every file its record
		// names, so a compaction needs no re-index (S18).
		Seed: SeedLink,
	}, {
		Name:   DerivedTags,
		Inputs: []Input{InputRefs, InputOID},
		Writes: []Write{{Step: StepTags, Trigger: TriggerRound, Writer: Writer{File: tagsFile},
			Charge: "a delta's merge: the tags it moved with their facts and 4 bytes a tag for the new " +
				"file's offsets, the old file streamed through one buffer into the new; a whole build (a full " +
				"round, an index set aside): every tag's name and facts and the old file's facts by object id, " +
				"some 1.2 KB allocated a tag over the build (121 MB at 100 000), at most gitlet.MaxRefsPerRepo; " +
				"the facts git reads streamed through one cat-file --batch-check, one cat-file --batch and one log"}},
		Owns:      []Owned{{FilePattern: FilePattern{engineRoot, tagsName()}, File: "tags", Main: true}},
		Check:     CheckRunTrailer,
		Freshness: FreshAtRound,
		// A function of the refs and of object ids: no pack leaving changes an
		// entry, and a log gone back is read again whole.
		OnPackAdded: Keep, OnPackRemoved: Keep, OnRefsMoved: Keep, OnReported: Keep, OnHistoryGone: Drop,
		OnPackRetired: Keep, OnPackExpunged: Keep, OnLayersMoved: Keep,
		// A rebuild's full round reads the tags again over its own packs.
		Seed: SeedNone,
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
			return bad("Inputs", "holds %q, which is none of %s", in, words.Or(Inputs))
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
	// EACH WRITE IS OF ONE KIND; a declaration may have both (the graph
	// layers and the tip over them), and what git writing any of its files
	// asks of the declaration is held below (gitWritten).
	git := d.gitWritten()
	for i, w := range d.Writes {
		field := fmt.Sprintf("Writes[%d]", i)
		var err error
		if w.Writer.Subcommand == "" {
			err = w.validateInProcess(field, bad)
		} else {
			err = w.validateGit(field, bad)
		}
		switch {
		case err != nil:
			return err
		case !w.Step.Valid():
			return bad(field+".Step", "is %q, none of %s", w.Step, words.Or(Steps))
		case !w.Trigger.Valid():
			return bad(field+".Trigger", "is %q, none of %s", w.Trigger, words.Or(Triggers))
		case !writerFile(w.Writer.File):
			return bad(field+".Writer.File", "is %q: a clean slash path of a .go file under internal/gitlet/",
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
		switch {
		case o.Prior && o.Named:
			return bad(field, "is both Prior and Named: the forty hex digits of a name are the checksum before "+
				"its trailer or the trailer itself, never both")
		case (o.Prior || o.Named) && o.Name.NumSubexp() != 1:
			return bad(field+".Name", "%q has %d capture groups, and a name carrying its checksum has one: "+
				"the forty hex digits", o.Name, o.Name.NumSubexp())
		}
		// A TREE IS ONE DIRECTORY THE CACHE'S OWN CODE OWNS WHOLE: git writes
		// none the cache keeps unread, and a pattern of many names would be
		// many trees no writer keeps a total of.
		if o.Tree {
			_, whole := o.Name.LiteralPrefix()
			switch {
			case git:
				return bad(field+".Tree", "is set on a derived file git writes: git writes no directory the "+
					"cache owns whole")
			case !whole:
				return bad(field+".Name", "%q is no literal, and a tree is one directory", o.Name)
			case !o.Main:
				// A tree's bytes are its writer's record (Derived.tree): the tree
				// is the declaration's presence.
				return bad(field+".Main", "is unset on a tree, and a directory owned whole is its declaration's "+
					"presence, its bytes the declaration's record")
			}
		}
	}
	switch {
	case git && mains != 1:
		return bad("Owns", "has %d main files, and a declaration git writes has one: the file whose presence "+
			"is its", mains)
	case mains > 1:
		return bad("Owns", "has %d main files, and a declaration has at most one", mains)
	}
	// A DAMAGE REPORT NAMES ONLY WHAT GIT READS (gitReads). git reads what it
	// writes, so a git-written declaration names a Part; an in-process one
	// names one where git reads its files (the installed chain) and none
	// otherwise, and then keeps its files on a report, which cannot be about
	// them: the check and the scrub hold them to their Check instead.
	switch {
	case git && !d.gitReads():
		return bad("Part", "is none on a derived file git writes, and git reads what it writes: name the part a "+
			"damage report names it by (gitproc.Damage.Drops)")
	case d.gitReads() && (&gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: d.Part}).Drops() != d.Part:
		return bad("Part", "is %v, which is no part a damage report drops (gitproc.Damage.Drops)", d.Part)
	case !d.gitReads() && d.OnReported != Keep:
		return bad("OnReported", "is %q, and no damage report names a file no git process reads: %q",
			d.OnReported, Keep)
	}
	switch {
	case !d.Check.Valid():
		return bad("Check", "is %q, none of %s", d.Check, words.Or(Checks))
	case d.gitReads() && d.Check == CheckEntryDigest:
		// git's files carry git's own trailer, and the scrub is what holds
		// them behind the serving: an entry's digest is read by the cache's
		// own reads alone.
		return bad("Check", "is %q on a derived file git reads, whose files carry git's trailer", d.Check)
	case d.chained():
		if err := d.validateChained(bad); err != nil {
			return err
		}
	}
	if !d.Freshness.Valid() {
		return bad("Freshness", "is %q, none of %s", d.Freshness, words.Or(Freshnesses))
	}
	for _, e := range Events {
		switch ch := d.On(e); {
		case !ch.Valid():
			return bad("On("+string(e)+")", "is %q, none of %s", ch, words.Or(Changes))
		case ch == Prefix && !d.chained():
			// A PREFIX IS A CUT OF THE INSTALLED CHAIN, which only the chain's
			// own declarations hold: anything else has no prefix to keep.
			return bad("On("+string(e)+")", "is %q, which cuts an installed chain, and this declaration holds "+
				"none (Check %q is the chain's)", ch, CheckLayers)
		}
	}
	switch {
	case !d.Seed.Valid():
		return bad("Seed", "is %q, none of %s", d.Seed, words.Or(SeedRules))
	case d.Seed == SeedMove && !d.tree():
		return bad("Seed", "is %q, which moves the directory a declaration owns whole, and this one owns none",
			d.Seed)
	case d.Seed == SeedLink && d.tree():
		// A TREE MOVES WHOLE: linked, it would be a stat and a link an entry
		// inside the round that holds the repository.
		return bad("Seed", "is %q on a directory owned whole, which a rebuild moves in one rename: %q", d.Seed,
			SeedMove)
	}
	return nil
}

// validateGit refuses a write git runs that leaves unsaid its lane, deadline,
// charge an object or argument list (which begins with its subcommand), or
// that names a Charge, which is an in-process write's.
func (w Write) validateGit(field string, bad func(field, format string, args ...any) error) error {
	switch {
	case !w.Lane.Valid():
		return bad(field+".Lane", "is %q, none of %s", w.Lane, words.Or(gitproc.Classes))
	case w.Deadline == nil:
		return bad(field+".Deadline", "is nil: say how long its process may run")
	case w.BytesPerObject <= 0:
		return bad(field+".BytesPerObject", "is %d: say what an object costs it in memory", w.BytesPerObject)
	case w.BaseBytes < 0:
		return bad(field+".BaseBytes", "is %d: a fixed part of a process's memory is never less than none",
			w.BaseBytes)
	case len(w.Args) == 0:
		return bad(field+".Args", "is empty")
	case w.Args[0] != w.Writer.Subcommand:
		return bad(field+".Args", "begin %q, not its writer's subcommand %q", w.Args[0], w.Writer.Subcommand)
	case w.Charge != "":
		return bad(field+".Charge", "is set on a write git runs, whose lane, deadline and charge an object say "+
			"what it holds")
	}
	return nil
}

// validateInProcess refuses an in-process write that claims what only a git
// write has — a lane, a deadline, a charge an object or an argument list,
// figures nothing would read — or that does not say what it holds (Charge).
func (w Write) validateInProcess(field string, bad func(field, format string, args ...any) error) error {
	switch {
	case w.Lane != "":
		return bad(field+".Lane", "is %q, and the cache's own code runs no git and holds no slot", w.Lane)
	case w.Deadline != nil:
		return bad(field+".Deadline", "is set, and an in-process write is bounded by its caller's context")
	case w.BytesPerObject != 0:
		return bad(field+".BytesPerObject", "is %d, and no admission charges an in-process write: its Charge "+
			"says what it holds", w.BytesPerObject)
	case w.BaseBytes != 0:
		return bad(field+".BaseBytes", "is %d, and no admission charges an in-process write: its Charge "+
			"says what it holds", w.BaseBytes)
	case len(w.Args) != 0:
		return bad(field+".Args", "is %s, and an in-process write hands git nothing", words.And(w.Args))
	case strings.TrimSpace(w.Charge) == "":
		return bad(field+".Charge", "is empty: say, in a sentence, what the write holds while it runs")
	}
	return nil
}

// validateChained refuses an installed-chain declaration (CheckLayers) the
// chain's code could not keep in step: one git does not read, one whose files
// are not one literal Main chain file and the layers' hashfiles, or one that
// answers any event Drop or the chain moving anything but Prefix. Dropped
// whole, one chain would leave the other naming layers the first no longer
// holds: both are cut at one place (prefixOf) or not at all.
func (d *Derived) validateChained(bad func(field, format string, args ...any) error) error {
	if !d.gitReads() {
		return bad("Check", "is %q, the installed chain's, on a derived file no git process reads", d.Check)
	}
	for i, o := range d.Owns {
		if o.Main && (o.Named || o.Prior || o.Tree) {
			return bad(fmt.Sprintf("Owns[%d]", i), "is the chain file, which the cache writes from the rows and "+
				"which carries no trailer and names no checksum")
		}
	}
	if !d.hasMain() {
		return bad("Owns", "has no main file, and an installed chain's is its chain file")
	}
	for _, e := range Events {
		if d.On(e) == Drop {
			return bad("On("+string(e)+")", "is %q, and an installed chain is only ever cut to a prefix of itself "+
				"(%q), at the one place both of its chains are cut", Drop, Prefix)
		}
	}
	if d.OnLayersMoved != Prefix {
		return bad("OnLayersMoved", "is %q, and the chain moving is what the installed chain follows: %q",
			d.OnLayersMoved, Prefix)
	}
	return nil
}

// derivedRoots is every root directory of an incarnation that holds what the
// rows and declarations put there: objects/, the dropping view
// (gitproc.DroppingDir), and every root a declaration owns or holds a file
// under, once each, in order. A boot's walk of a copy with no state reads
// them (measureIncarnation), so every root validateAll admits is walked.
func derivedRoots(decls []Derived) []string {
	roots := []string{"objects", gitproc.DroppingDir}
	for i := range decls {
		for _, p := range decls[i].patterns() {
			root, _, _ := strings.Cut(p.Dir, "/")
			if !slices.Contains(roots, root) {
				roots = append(roots, root)
			}
		}
	}
	return roots
}

// writerFile reports whether f names a Go file under internal/gitlet the way
// the writers gate names one: slash-separated, clean, relative to the module
// root.
func writerFile(f string) bool {
	return path.Clean(f) == f && !strings.Contains(f, `\`) && strings.HasPrefix(f, "internal/gitlet/") &&
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
// them: unique names; no two maintainer writes of one Step (the refusal
// counter and gauge are keyed by step); a declaration git reads under
// objects/, one no git process reads under crewlet/ (engineRoot says why);
// every part a damage report may drop (gitproc.Damage.Drops) some
// declaration's, or that damage would drop nothing; nothing inside a
// directory one owns whole (Owned.Tree). Open refuses a cache whose
// declarations fail it, and TestEveryDerivedFileIsDeclared catches it first.
//
// THE ROOT RULE IS WHAT A BOOT'S WALK READS (M3-O77): a copy with no state is
// counted by its packed-refs and its files under derivedRoots
// (measureIncarnation), read from the same declarations, so they cannot drift.
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
		root := "objects"
		if !d.gitReads() {
			root = engineRoot
		}
		for _, p := range d.patterns() {
			if p.Dir != root && !strings.HasPrefix(p.Dir, root+"/") {
				return fmt.Errorf("derived file %q has files in %s, and a derived file git reads lives under "+
					"objects/, one no git process reads under %s/: the root holds the repository's own files, "+
					"refs/ stays empty since a loose ref outranks packed-refs for every reader, objects/ is git's "+
					"own namespace — a push's scratch names it as its one alternate — and a boot counts a copy "+
					"with no state by the roots its declarations own and packed-refs", d.Name, p.Dir, engineRoot)
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
	// NOTHING LIVES INSIDE A TREE BUT ITS OWN ENTRIES: the image keeps a tree
	// without reading inside it and its writer counts what it holds, so a
	// file another declaration owned there would be kept by no sweep, counted
	// by no measure and dropped with the tree by a change that is not its own.
	for _, t := range decls {
		for _, o := range t.Owns {
			if !o.Tree {
				continue
			}
			literal, _ := o.Name.LiteralPrefix()
			tree := path.Join(o.Dir, literal)
			for _, d := range decls {
				for _, p := range d.patterns() {
					if p.Dir == tree || strings.HasPrefix(p.Dir, tree+"/") {
						return fmt.Errorf("derived file %q has files in %s, inside the directory %s derived file "+
							"%q owns whole (Owned.Tree): nothing but that tree's own entries lives in it", d.Name,
							p.Dir, tree, t.Name)
					}
				}
			}
		}
	}
	return nil
}
