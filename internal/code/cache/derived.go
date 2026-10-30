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

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// THE DERIVED FILES (ADR-0056). A node-local derived file is DECLARED here,
// lives inside its incarnation, and the cache's sweep, Verify, check, scrub,
// budget, plan and writers gate read the declaration — a derived file is added
// by adding a declaration, never beside them.
//
// A derived file is one the cache writes from what an incarnation already
// holds — its packs, its refs, the push rows a round read beside them, or the
// answer a read computed from its objects — and that no row names: the
// multi-pack-index (a full round's, and the maintainer's with its bitmap or
// without one), the commit-graph, the index of which push introduced each
// commit (provenance.go), the memo of answers by object id (memo.go), and the
// installed published chain — a repository's maintainer's multi-pack-index and
// commit-graph layers, installed from the object store by the rows that name
// them (layers.go). Each
// was spelled in a dozen places that agreed only because
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
//     rewritten, its own files reported damaged, the log under it gone back,
//     a pack a compaction retired, a dropping pack an expunge's commit took
//     out, or the installed chain moving — every one of them read by dropFor,
//     the one removal of a derived file, at the one place each change is
//     made, and a pack's leaving and the chain's by the swap too, which links
//     into the next incarnation only what they keep (swap.go) — and for the
//     installed chain a CUT to a prefix of itself (Prefix), never a drop;
//   - which part of a repository a damage report names it by (Part, in
//     gitproc's vocabulary: gitproc.Damage.Drops), so a report reaches the
//     declarations it names and no others.
//
// Its files live INSIDE THE INCARNATION, so they are counted by the budget,
// leave with the repository and are written again over a rebuild's packs — or
// linked into the rebuild's incarnation, for a declaration whose files a
// rebuild keeps (Seed); a pack's .pack, .idx and .rev are not derived files —
// the rows name them.
//
// TWO KINDS OF WRITER. git writes the multi-pack-index and the commit-graph,
// in a lane of the pool, under a deadline over what it covers and a memory
// charge an object; the cache's OWN GO CODE writes the provenance index, the
// memo and the installed published chain (an IN-PROCESS write:
// Writer.Subcommand ""), which runs no git, holds no slot and is bounded by its
// caller's context, so its row says what it holds instead (Write.Charge). Each
// WRITE is of one kind, and a declaration git reads may have writes of both:
// the installed commit-graph layers are placed by the cache's own code and the
// node's TIP commit-graph over them is git's (maintain.go), both under one
// chain file in one directory, which ownership by name cannot divide. One
// git does NOT read is the cache's own code's alone — no git process writes a
// file no git process reads (Validate).
//
// WHO READS A DECLARATION'S FILES IS ITS PART'S TO SAY, never who writes them:
// a declaration git READS names the part a damage report names it by (Part),
// whoever writes its files — git's own multi-pack-index and commit-graph, and
// the published layers the cache installs byte for byte as their maintainer's
// git wrote them (layers.go) — and lives under objects/, git's own namespace;
// one NO git process reads names none, which no report can name, and lives
// under crewlet/, the engine's (engineRoot): the provenance index and the memo.
// Read by who writes them, the rule refused the installed chain — files every
// git process reads, written by the cache's own code — or had it live where
// git never looks for an index.

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
	// DerivedProvenance is the index of which push introduced each commit and
	// every update each push landed on a ref (provenance.go): written by every
	// round that reads pushes before it answers, and by the backfill behind
	// the serving — the cache's own write, which no git process reads.
	DerivedProvenance DerivedName = "provenance"
	// DerivedMemo is the memo of answers by object id (memo.go): one file an
	// answer a server-side read cut — a page of a listing, a file, a diff or a
	// comparison — keyed by everything it is a function of, under the
	// incarnation's crewlet/memo, a directory it owns whole (Owned.Tree), and
	// held to a share of the cache's budget (MemoShare).
	DerivedMemo DerivedName = "memo"
	// DerivedLayers is the incarnation's INSTALLED PUBLISHED multi-pack-index
	// chain (layers.go): every layer's .midx and .bitmap as the repository's
	// maintainer wrote them, installed byte for byte by the cache from the
	// object store once held to its row (code.CheckLayer), and the chain file
	// the cache writes from the rows' order — no node writes a multi-pack-index
	// layer of its own (layers.go says why).
	DerivedLayers DerivedName = "layers"
	// DerivedGraphLayers is the incarnation's split commit-graph over the same
	// chain: each installed layer's commit-graph layer, installed with it from
	// the same parts, the node's own TIP layer over the commits no installed
	// layer holds — the tail's (maintain.go) — and the commit-graph-chain
	// listing them. Its own declaration, because the graph's files live in a
	// directory of their own under a chain file of their own; cut at the one
	// place the multi-pack-index chain is (prefixOf), so the two chains never
	// disagree. The published layers and the tip are ONE declaration, with a
	// write of each kind, because both are graph-<checksum>.graph under one
	// chain file, which ownership by name cannot divide.
	DerivedGraphLayers DerivedName = "graph-layers"
)

// engineRoot is the one directory of an incarnation's root besides objects/
// that a declaration may own files in: the ENGINE'S, for what the cache's own
// code writes. Not objects/, which is git's namespace — a push's scratch names
// it as its one alternate, and git's tools walk it — and not refs/, hooks/,
// info/ or logs/, git's too (a loose ref under refs/ outranks packed-refs for
// every reader), nor a root file's name.
const engineRoot = "crewlet"

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
	// InputRefs is packed-refs — a bitmap's selected commits, and a graph's
	// --reachable tips (own mode's whole graph, and the node's tip over the
	// tail).
	InputRefs Input = "refs"
	// InputPushRows is the code domain's push rows: those a delta's pushes
	// phase serves (code.StateQuery.Pushes), and those the backfill reads a
	// page at a time (code.ReadIntroduced) — never the incarnation's files.
	InputPushRows Input = "push-rows"
	// InputOID is the objects an answer names, by their ids: immutable, so an
	// answer computed from them is the answer for as long as the ids name
	// them, whatever packs hold them and whatever refs point at them.
	InputOID Input = "oid"
	// InputLayers is the code domain's published layer rows (code_layers and
	// code_layer_parts) a snapshot serves (code.RepoSnapshot.Layers): what a
	// layer is, the packs it covers, where in the chain it stands, and the
	// parts its bytes are read back from.
	InputLayers Input = "layers"
)

// Inputs is every Input.
var Inputs = []Input{InputPacks, InputRefs, InputPushRows, InputOID, InputLayers}

// Valid reports whether i is one of Inputs.
func (i Input) Valid() bool { return slices.Contains(Inputs, i) }

// Trigger is what starts a write.
type Trigger string

const (
	// TriggerRound is a round of a sync, before it answers: which rounds is
	// the write's own — the round's multi-pack-index a full one's past
	// MidxRoundPacks packs, the provenance index's fold every delta's that
	// served pushes.
	TriggerRound Trigger = "round"
	// TriggerMaintainer is the maintainer's pass — debounced, duty-cycled and
	// admitted on the node's memory.
	TriggerMaintainer Trigger = "maintainer"
	// TriggerBackfill is the cache's own background loop over what a round
	// left to read (provenance.go): a page of push rows a full round passed
	// over or a seat's question wants, or a merge of runs — paced by the
	// fleet, behind the serving.
	TriggerBackfill Trigger = "backfill"
	// TriggerRead is a server-side read by object id that ran its git
	// (internal/code/query): the pages its one run cut, each kept for the
	// next read that asks for it (memo.go).
	TriggerRead Trigger = "read"
	// TriggerInstall is a published layer placed into an incarnation from the
	// object store (layers.go): by the FULL round that first reads the chain,
	// before it answers, and otherwise by the cache's installer, behind the
	// serving, once a delta has named a layer the copy does not hold.
	TriggerInstall Trigger = "install"
)

// Triggers is every Trigger.
var Triggers = []Trigger{TriggerRound, TriggerMaintainer, TriggerBackfill, TriggerRead, TriggerInstall}

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
	// has answered, for the range its coverage names (code.Coverage): an
	// answer about a push outside that range says so, rather than answering
	// that there is none.
	FreshAtRound Freshness = "at-round"
	// FreshImmutable is keyed by everything it answers is a function of —
	// object ids, which name one object for ever, and the read's arguments —
	// so it is never stale, only absent: a reader that finds no entry runs
	// its git, and one that finds one is answered by it.
	FreshImmutable Freshness = "immutable"
	// FreshInstalled is installed by the full round that first reads it, and
	// otherwise by the installer within one of its passes of the round that
	// names it; until it is, the copy serves from the longest prefix of the
	// chain it holds, or in own mode with none. Never a correctness promise:
	// git reads every pack outside the chain beside it, and a pack's own
	// index where there is no chain at all.
	FreshInstalled Freshness = "installed"
)

// Freshnesses is every Freshness.
var Freshnesses = []Freshness{FreshDebounced, FreshAtRound, FreshImmutable, FreshInstalled}

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
	// CheckRunTrailer is a provenance run's own trailer: the last 32 bytes are
	// the SHA-256 of every byte before them, written with the run
	// (provenance.go). Nothing the cache reads of a run is read from one this
	// process did not write or check.
	CheckRunTrailer Check = "run-trailer"
	// CheckEntryDigest is a memo entry's own header: the key it answers and
	// the SHA-256 of its body (memo.go), held on EVERY read of the entry, so
	// no entry is served unchecked whoever wrote it — and the check and the
	// scrub, which hash a file behind the serving, have nothing to add: they
	// pass over such a declaration's files (Check.hashed).
	CheckEntryDigest Check = "entry-digest"
	// CheckLayers is the installed published chain's (layers.go): every file
	// a layer installed is a git hashfile held to its trailer, and one named
	// by its own checksum (Owned.Named: a layer's .midx and .graph) to the
	// forty hex digits of its name as that trailer; a CHAIN FILE — which the
	// cache writes from the rows and which carries no trailer — is held, line
	// for line, to the ids the incarnation installed. A file of a layer that
	// fails CUTS the chain at that layer, to be fetched again with every layer
	// above it, and a chain file that fails is written again from what the
	// incarnation installed (layers.go): never a drop to own mode.
	CheckLayers Check = "layers"
)

// Checks is every Check.
var Checks = []Check{CheckTrailer, CheckRunTrailer, CheckEntryDigest, CheckLayers}

// Valid reports whether c is one of Checks.
func (c Check) Valid() bool { return slices.Contains(Checks, c) }

// hashed reports whether the check and the scrub hold the files of a
// declaration checked by c to their bytes (ownedFile.held): every Check but
// CheckEntryDigest, whose every read holds an entry instead — hashed behind
// the serving too, a memo of gigabytes would be read whole by every scrub for
// nothing a read does not already refuse.
func (c Check) hashed() bool { return c != CheckEntryDigest }

// Event is a change the cache makes in an incarnation that a derived file may
// be built over: what dropFor is told, at the one place each is made.
type Event string

const (
	// EventPackAdded is a pack installed into the incarnation — a round's
	// download, an adopted push.
	EventPackAdded Event = "pack-added"
	// EventPackRemoved is a pack's files taken out of the incarnation, or
	// renamed over with its bundle again where its index may differ — a full
	// round's stray that no retirement accounts for (no row names it: a log
	// gone back or recreated under the cache), and a pack installed again
	// over files at another size than its row's, or whose index failed its
	// checksum. A derived file built over the old files may name what is no
	// longer there, and a commit read from them may be gone with them.
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
	// EventHistoryGone is the code log under the incarnation no longer being
	// the history it was read from — recreated (another stream identity than
	// the one recorded), re-anchored onto another generation, or gone back (a
	// linearizable answer below a position read before) — so every position it
	// recorded may name another record. Made by the full round that reads the
	// incarnation whole for that reason, BEFORE its sweep and before any git
	// process sees the round's image (sync.go apply). A declaration built over
	// the packs alone meets the same change as the packs the restored history
	// no longer names, which that round's sweep removes as EventPackRemoved.
	EventHistoryGone Event = "history-gone"
	// EventPackRetired is a pack file taken out of the incarnation because a
	// COMPACTION RETIRED it (the code domain's code_packs_retired): its
	// objects are all in packs the incarnation keeps or installs in the same
	// round, since a compaction merges packs without judging reachability —
	// every object of its inputs is in its outputs — and the round that
	// takes the file out has installed those outputs first. A declaration
	// built over the packs' NAMES or OFFSETS meets a pack gone; one built
	// over their COMMITS or OBJECT IDS meets nothing that left. Made by the
	// round whose answer names the retirement (swap.go): a delta's retired
	// phase, or a full answer's retirements above its caller's position. An
	// expunge's BEGIN retires its inputs as this event too: every object of
	// them is in the packs it adds, kept or dropping, which the same round
	// installs.
	EventPackRetired Event = "pack-retired"
	// EventPackExpunged is a pack file taken out of the incarnation's
	// DROPPING VIEW because no row names it any more: an EXPUNGE'S COMMIT
	// retired it (code.ExpungeCommit) — a pack its begin left dropping
	// (code.PackDropping), holding what no ref reached — and what it held
	// left the repository, the one change that takes objects out of it (the
	// package doc's "The dropping view"). A declaration built over the
	// copy's COMMITS or its ANSWERS meets objects gone — a commit-graph
	// naming a commit no pack holds, over which git answers a walk as though
	// the commit were there; a memo's answer about content the expunge
	// purged — where one built over objects/pack alone, the multi-pack-index
	// and the published layers, which neither cover nor name the view, meets
	// nothing that left. Made by the round that takes the pack out: a delta's
	// retired phase naming a row its live set held dropping, or a full
	// round's stray in the view no row names.
	EventPackExpunged Event = "pack-expunged"
	// EventLayersMoved is the incarnation's installed published chain changing
	// (layers.go): a layer placed, or the chain cut back to a prefix of
	// itself. git reads a non-incremental multi-pack-index in preference to a
	// chain, a non-incremental write deletes every layer and the chain file,
	// and a commit-graph that is no split one is read before the chain and a
	// whole write over a chain unlinks it — so the two modes never coexist,
	// and what an own-mode file answers to the chain moving is a drop.
	EventLayersMoved Event = "layers-moved"

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
var Events = []Event{EventPackAdded, EventPackRemoved, EventRefsMoved, EventReported, EventHistoryGone,
	EventPackRetired, EventPackExpunged, EventLayersMoved}

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
	// Prefix is the installed chain's answer (layers.go): keep the longest
	// PREFIX of the installed chain whose every layer names none of the
	// event's packs and — for EventLayersMoved — is, layer for layer, the
	// chain wanted, and move the rest into trash/ before the change is
	// visible, the chain files written again over what is kept (or removed,
	// when nothing is). A layer is valid only over its exact base — its file
	// does not name the base, the chain's order does — so a chain is never
	// cut in its middle: every layer above a cut goes with it.
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
	// Main: this is the file whose presence is the declaration's — the index
	// itself, the graph itself — where every other owned file is a part of it
	// (a bitmap, a reverse index). Exactly one a git-written declaration, its
	// Name a literal; at most one an in-process one, whose presence may be its
	// record instead: a set of the provenance index's runs has no file whose
	// presence is the index's — its state names the runs (Cache.recordDerived).
	Main bool
	// Prior: the forty hex digits of its name, the pattern's one capture
	// group, are the checksum it carries in the twenty bytes before its
	// trailer — a multi-pack-index's reverse index carries its index's, as a
	// pack's .rev carries its pack's (git's write_rev_trailer).
	Prior bool
	// Named: the forty hex digits of its name, the pattern's one capture
	// group, are its OWN trailer — a published layer's .midx and .graph are
	// named by their checksum (multi-pack-index-<id>.midx, graph-<id>.graph),
	// as a pack's .pack is (check.go's expect.trailer). git opens such a file
	// by the name its chain file lists and reads none of its trailer, so the
	// name is the one thing that ties the file to the row that names it.
	Named bool
	// Tree: Name matches a DIRECTORY the declaration owns WHOLE — every entry
	// below it is the declaration's (crewlet/memo, one file an answer). The
	// image keeps it as a directory, and only as one, without reading inside
	// it; Verify admits it whole; and no measure stats it: what it holds is
	// its writer's running total (memo.go), since a stat of the directory is
	// its inode's few KiB whatever it holds, and a walk of it on a round's or
	// a maintenance run's path would cost a stat an entry. A Tree is one
	// literal name of a declaration the cache's own code writes — git writes
	// no directory the cache owns whole — and no declaration's files live
	// under it.
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
	// chain (maintain.go): the commits of the tail, the packs no installed
	// layer names, written over exactly the installed commit-graph layers.
	StepTipGraph Step = "tip-graph"
	// StepProvenance is a run of the provenance index — a round's fold, a
	// backfilled page or a merge. Never refused for memory: it is no value of
	// the maintenance refusals' counter or gauge, which name git's steps.
	StepProvenance Step = "provenance"
	// StepMemo is an entry of the memo: an answer a read cut. Never refused
	// for memory either.
	StepMemo Step = "memo"
	// StepLayer is a published layer installed — fetched, held to its row,
	// placed — and the chain files written from the rows (layers.go). Never
	// refused for memory either: it runs no git, and what it holds is a
	// part's copy buffer and a fetch slot, as a pack's download does.
	StepLayer Step = "layer"
)

// Steps is every Step, in the order a line or a count names them.
var Steps = []Step{StepBitmap, StepIndex, StepGraph, StepTipGraph, StepProvenance, StepMemo, StepLayer}

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
	// BytesPerObject is the write's own memory an object of what it covers
	// (Covers.Objects: the repository's for own mode's writes, the tail's for
	// the node's tip): it is admitted at its charge (Write.charge), which adds
	// room for one transfer of the whole repository (memory.go). Zero for an
	// in-process write, which no admission charges: its Charge says what it
	// holds.
	BytesPerObject int64
	// BaseBytes is the write's own memory WHATEVER it covers, charged beside
	// BytesPerObject's: zero for a write over the whole repository, whose
	// per-object figure folds any fixed part into the repository's many
	// objects, and set where a write covers few objects and holds a part that
	// does not follow them — the node's tip, whose changed-path filters are
	// bounded by MaxNewFilters rather than by its tail (TipGraphBaseBytes).
	// Zero for an in-process write.
	BaseBytes int64
	Writer    Writer
	// Args is the argument list its writer hands git: one of the writer
	// file's own literals, so the writers gate reads it where git runs. Nil
	// for an in-process write.
	Args []string
	// Charge is an IN-PROCESS write's account of what it holds, one
	// sentence: set exactly when Writer.Subcommand is "". An in-process write
	// holds no slot and is bounded by its caller's context, so a per-object
	// charge or a process deadline would be a figure nothing reads; what it
	// holds is the bound a reader of the declaration needs instead.
	Charge string
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

// SeedRule is what a rebuild does with a declaration's files in the
// incarnation it replaces (rebuild.go's seedRebuild and promote).
type SeedRule string

const (
	// SeedNone: the new incarnation writes them again — the multi-pack-index
	// and the commit-graph, functions of the packs, which may be what failed
	// in the copy being replaced.
	SeedNone SeedRule = "none"
	// SeedLink: hard-linked into the new incarnation, its record carried
	// with them — files that are a function of the ROWS rather than the
	// packs, which a rebuild reads again unchanged, so writing them again
	// would read the rows again for nothing. A link is the same inode, so a
	// file this process vouched for in the copy being replaced is vouched
	// for in the new one, and one it has not is held to its Check before
	// anything reads it there, as it would have been in the old.
	SeedLink SeedRule = "link"
	// SeedMove: MOVED whole into the new incarnation as it is promoted — ONE
	// rename of the directory the declaration owns whole (Owned.Tree), its
	// record and its in-memory half with it (memo.go's carryMemo) — for a
	// tree of files that are a function of what the new incarnation holds as
	// the old did, and of no other number: linked file by file, the memo of
	// a hot repository would cost a stat and a link an answer inside the
	// round that holds the repository, which a request's repair waits on, and
	// carried by a compaction's swap the same way it would cost that on
	// every git node that holds the repository. Moved at the PROMOTION
	// rather than at the seeding, so the replaced incarnation serves it
	// until the new one does and a rebuild that fails leaves it where it
	// was; a reader still holding the replaced incarnation misses from then
	// on. Only a tree is moved, and a tree is never linked (Validate).
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
	// Part is the part of a repository a damage report names it by: one of
	// the parts gitproc.Damage.Drops answers, for a declaration whose files
	// git READS, whoever writes them. A report naming it is EventReported for
	// it. Zero for one NO git process reads — the provenance index, the memo:
	// written by the cache's own code and read by nothing else — so no report
	// can name it.
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

// gitWritten reports whether a git process writes any of d's files: a write
// of it git runs (Writer.Subcommand), beside any the cache's own code makes.
// git writes what it reads, so Validate holds such a declaration to a part a
// damage report names it by, to one file whose presence is its own, and to
// no directory owned whole.
func (d *Derived) gitWritten() bool {
	return slices.ContainsFunc(d.Writes, func(w Write) bool { return w.Writer.Subcommand != "" })
}

// gitReads reports whether a git process reads d's files: whether it names a
// part a damage report names it by (Part), whoever writes them. Where its
// files live (validateAll), whether the round's check holds them before a
// copy serves (check.go) and whether dropping one can end a failure a git
// process met (droppedDerived) are all this, never who writes them.
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

// The patterns of DerivedFiles, compiled once, at their FIRST USE: a regular
// expression is immutable and safe to share, so every copy DerivedFiles
// answers holds the same ones — and none is compiled at init, because the
// code host's proc-receive hook is this binary, exec'd once a push, and every
// package it links runs its initialisation before the hook's first statement
// (cmd/crewlet's TestTheHookPaysLittleForTheEnginesInit). A pattern holding
// a forty-digit checksum compiles to an instruction for every digit, and
// these and the package's three other file-name patterns were nearly all
// this package allocated at init — about 215 KB of its 217 under the race
// detector, on every push, for a hook that opens no cache.
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
	// get_midx_chain_filename and get_split_midx_filename_ext,
	// commit-graph.c's get_commit_graph_chain_filename and
	// get_split_graph_filename): a layer's .midx and .bitmap and the chain file
	// under objects/pack/multi-pack-index.d, a commit-graph layer and its chain
	// file under objects/info/commit-graphs. No .rev: git writes a layer's
	// reverse index as its RIDX chunk, as it writes a whole index's.
	layerChainName  = lazyPattern(`^multi-pack-index-chain$`)
	layerMidxName   = lazyPattern(`^multi-pack-index-([0-9a-f]{40})\.midx$`)
	layerBitmapName = lazyPattern(`^multi-pack-index-[0-9a-f]{40}\.bitmap$`)
	graphChainName  = lazyPattern(`^commit-graph-chain$`)
	graphLayerName  = lazyPattern(`^graph-([0-9a-f]{40})\.graph$`)
	// What the node's TIP write holds while it runs, as git 2.56.0's
	// commit-graph.c writes a split layer (write_commit_graph_file): the
	// chain file's lock, held from the layer's start to the chain's commit,
	// and the layer itself as a temporary file (mks_tempfile_m of
	// "tmp_graph_XXXXXX", six of git's mkstemp letters and digits) until it
	// is renamed graph-<checksum>.graph.
	graphChainLock = lazyPattern(`^commit-graph-chain\.lock$`)
	tmpGraph       = lazyPattern(`^tmp_graph_[A-Za-z0-9]{6}$`)
)

// lazyPattern is expr compiled at its first use and shared from then on
// (sync.OnceValue): every file-name pattern of this package is one, so none
// is compiled at init. A pattern that does not compile panics at its first
// use rather than at init — and every one is a literal this package's tests
// compile through DerivedFiles' validation and the readers that call it.
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
// The multi-pack-index's REVERSE INDEX is owned although git ≥ 2.41 keeps it
// as a chunk inside the index and writes no multi-pack-index-<cs>.rev (only
// under GIT_TEST_MIDX_WRITE_REV): a file git may write that the image did not
// admit would be trashed under a running reader. A bitmap's TEMPORARY FILE is
// objects/pack/tmp_bitmap_XXXXXX on every git from the floor (git's mkstemp in
// pack-bitmap-write.c), registered for no cleanup, so a writer killed while it
// writes the bitmap — the end of a run, where most of its time is — leaves it.
// A non-incremental `multi-pack-index write` writes no multi-pack-index.d/.
//
// The PROVENANCE INDEX is the cache's own: a run written by the round that
// folds a delta's pushes and by the backfill, each staged in tmp/ and renamed
// in, so no write of it holds a file in the incarnation; built from the push
// rows rather than the packs, so no pack added, removed or retired and no ref
// moved changes it, and a log gone back drops it; kept by a rebuild, which
// reads the same rows again (provenance.go says why it is eager).
//
// The MEMO is the cache's own too: an answer a read cut, staged in tmp/ and
// renamed into a cohort of crewlet/memo once the read's git has answered
// whole, so no write of it holds a file in the incarnation; a function of
// object ids alone, so no pack added or removed and no ref moved changes an
// entry, while a log gone back drops it whole — the memo cannot tell which
// entries read what, and no answer outlives the history it was computed from
// on this host — and so does an expunge's commit, whose answers may hold the
// content it purged; and kept by a rebuild, which repairs packs and not
// answers (memo.go). Of its memory a declaration states, NONE IS AN ENTRY'S:
// what the cache holds of a copy's memo is its cohorts and their bytes, 16
// bytes a cohort and some seventeen cohorts at the most, however many answers
// they hold — the order of use is the cohorts', on the disk.
//
// The INSTALLED CHAIN (layers.go) is git's files placed by the cache's own
// code: each layer's .midx, .bitmap and commit-graph layer as its maintainer's
// git wrote them, fetched from the object store and held to its row before
// any git process is handed them, and both chain files written from the rows'
// order — a function of the rows and of the packs they name, so a pack added
// or the refs moved change nothing of it, and a pack that leaves, a retirement
// or the rows' chain moving CUT it to the longest prefix still valid (Prefix),
// never more: a layer is valid only over its exact base. A report about it
// keeps its files and makes the copy's scrub due (a drop could only cut the
// chain, and no git process says which layer it met), and a log gone back
// reaches it as the rows' chain moving, which the full round that reads the
// copy whole cuts it by. Its install holds no file in the incarnation: every
// file is staged in tmp/ and renamed in. Kept by a rebuild where every pack a
// layer names is seeded and the layer passes code.CheckLayer there and then —
// the rows name it, and a cold copy's first clone would otherwise walk
// without a bitmap until the chain was fetched again.
//
// THE NODE'S TIP is git's own split commit-graph layer over the installed
// commit-graph layers (maintain.go): the commits of the TAIL, the packs pushed
// since the last publication, which no installed layer holds — written by the
// maintainer, in its lane, at a charge an object of the tail with room for a
// clone of the whole repository beside it, holding the chain file's lock and
// its temporary layer while it runs. A function of the packs and the refs
// over exactly the layers beneath it, so it goes with any layer that goes,
// with any chain placed and with any pack removed (commits may go with it);
// a retirement that keeps the whole chain keeps it (X04: a merge drops no
// commit, and the tip names commits by id) — but an expunge's commit, which
// keeps every layer (none names a dropping pack) and takes the commits it
// expunged, which the tip may name. A rebuild writes it again.
func DerivedFiles() []Derived {
	maintained := func(c Covers) time.Duration { return gitproc.PackWorkDeadline(c.DiskBytes) }
	return []Derived{{
		Name:   DerivedMidx,
		Inputs: []Input{InputPacks, InputRefs},
		Writes: []Write{
			{Step: StepIndex, Trigger: TriggerRound, Lane: gitproc.ClassRead,
				Deadline:       func(c Covers) time.Duration { return gitproc.PackWorkDeadline(c.IndexBytes) },
				BytesPerObject: IndexBytesPerObject, Writer: Writer{maintainFile, procIndex}, Args: roundIndexArgs,
				Holds: []FilePattern{{"objects/pack", midxLock()}}},
			{Step: StepBitmap, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance, Deadline: maintained,
				BytesPerObject: BitmapBytesPerObject, Writer: Writer{maintainFile, procIndex}, Args: bitmapArgs,
				Holds: []FilePattern{{"objects/pack", midxLock()}, {"objects/pack", tmpBitmap()}}},
			{Step: StepIndex, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance, Deadline: maintained,
				BytesPerObject: IndexBytesPerObject, Writer: Writer{maintainFile, procIndex}, Args: plainArgs,
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
		// A pack RETIRED drops it as a pack removed does: the index names
		// every pack it covers by checksum and places each object at an
		// offset in one of them, so an index naming a pack that left fails
		// every lookup that reaches it. The chain MOVING drops it: it is OWN
		// mode's index, which git reads in preference to a chain and whose
		// non-incremental write deletes every layer and the chain file, so it
		// goes before a chain is placed (layers.go). An expunge's commit KEEPS
		// it: the packs it takes out lie in the dropping view, which no index
		// covers — the round's is written over objects/pack's packs, the
		// maintainer's over the pack directory git writes it in.
		OnPackAdded: Keep, OnPackRemoved: Drop, OnRefsMoved: Keep, OnReported: Drop, OnHistoryGone: Keep,
		OnPackRetired: Drop, OnPackExpunged: Keep, OnLayersMoved: Drop,
		Seed: SeedNone,
	}, {
		Name:   DerivedGraph,
		Inputs: []Input{InputPacks, InputRefs},
		Writes: []Write{{Step: StepGraph, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance,
			Deadline: maintained, BytesPerObject: GraphBytesPerObject, Writer: Writer{maintainFile, procGraph},
			Args: graphArgs, Holds: []FilePattern{{"objects/info", graphLock()}}}},
		Owns:      []Owned{{FilePattern: FilePattern{"objects/info", graphMain()}, File: "commit-graph", Main: true}},
		Check:     CheckTrailer,
		Part:      gitproc.PartCommitGraph,
		Freshness: FreshDebounced,
		// A pack RETIRED KEEPS IT (X04): a compaction merges packs and drops
		// no object, so every commit the graph lists is still in a pack the
		// incarnation keeps, and git reads the graph by commit id, never by
		// pack. A pack REMOVED — a stray no retirement accounts for, a log
		// gone back — may take commits with it, and drops it. The chain
		// MOVING drops it as it does the index: git loads a commit-graph that
		// is no split one before the chain, and a whole write over a chain
		// unlinks commit-graph-chain and expires its every layer. An EXPUNGE'S
		// COMMIT DROPS IT, the one retirement that does (X04's one drop): the
		// packs it takes out hold commits no ref reached, which leave the
		// repository, and over a graph still naming one git answers a walk —
		// `rev-list -1 <it>`, `log --format=%H` — as though the commit were
		// there, and the connectivity check fails (measured on 2.56.0). Its
		// begin keeps it, every commit then still in the copy.
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
				Charge: "a page of push rows (code.MaxPageBytes) as one run, or a merge of adjacent runs streamed " +
					"from their mapped files through one buffer — nothing held a push or a ref, but one key's " +
					"ordinals"},
		},
		Owns:      []Owned{{FilePattern: FilePattern{engineRoot, provenanceRunName()}, File: "provenance"}},
		Check:     CheckRunTrailer,
		Freshness: FreshAtRound,
		// Its input is the push rows, which no pack and no ref of the
		// incarnation moves — a pack a compaction retired included; a report
		// names no file of it, since no git process reads one; and a log gone
		// back makes every position it records another record's. No layer
		// moves a push row either, and nor does an expunge.
		OnPackAdded: Keep, OnPackRemoved: Keep, OnRefsMoved: Keep, OnReported: Keep, OnHistoryGone: Drop,
		OnPackRetired: Keep, OnPackExpunged: Keep, OnLayersMoved: Keep,
		Seed: SeedLink,
	}, {
		Name:   DerivedMemo,
		Inputs: []Input{InputOID},
		Writes: []Write{{Step: StepMemo, Trigger: TriggerRead, Writer: Writer{File: memoFile},
			Charge: "the pages one read's run cuts, each staged in tmp/ as it is cut and renamed in once the run " +
				"has answered whole: one page of at most code.MaxGitAnswerBytes held at a time, and of a diff's " +
				"stat the counts of one page's files, about 24 bytes a file, written into that page's staged " +
				"entry once the stat has counted past it (MemoBatch.Extend)"}},
		Owns:      []Owned{{FilePattern: FilePattern{engineRoot, memoTreeName()}, File: "memo", Main: true, Tree: true}},
		Check:     CheckEntryDigest,
		Freshness: FreshImmutable,
		// An object id names one object for ever, so no pack added, removed or
		// retired and no ref moved alters an answer keyed by the ids it read —
		// and a compaction keeps every object of the packs it retires; a report
		// names no file of it, since no git process reads one; and a restored
		// or recreated log is another history, which the memo cannot tell entry
		// by entry — so it goes whole, and is computed again on demand. No
		// layer moves an object either. An expunge's commit drops it whole: an
		// answer may hold the content it purged — a file's lines, a diff — which
		// no read serves once the commit has applied, and the memo cannot tell
		// which entries read what.
		OnPackAdded: Keep, OnPackRemoved: Keep, OnRefsMoved: Keep, OnReported: Keep, OnHistoryGone: Drop,
		OnPackRetired: Keep, OnPackExpunged: Drop, OnLayersMoved: Keep,
		// A rebuild repairs packs, not answers, and a compaction's swap keeps
		// every object: the whole tree moves into the new incarnation as it is
		// promoted, one rename whatever it holds, where every read holds an
		// entry to its key and digest as it did here.
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
		// A layer names its packs by checksum and places each object at an
		// offset in one of them, so a pack it names leaving — removed, or
		// retired — cuts the chain at that layer; the chain moving cuts it to
		// the prefix the rows still name. A pack added changes no layer (it
		// stays outside the chain until the next publication), nor do the
		// refs. A report keeps it (the scrub decides), and a log gone back
		// reaches it as the chain moving. An expunge's commit cuts it where a
		// layer names a pack it takes out, which none does — nothing is
		// published while an expunge is open (code's publication rule) — so it
		// keeps every layer.
		OnPackAdded: Keep, OnPackRemoved: Prefix, OnRefsMoved: Keep, OnReported: Keep, OnHistoryGone: Keep,
		OnPackRetired: Prefix, OnPackExpunged: Prefix, OnLayersMoved: Prefix,
		Seed: SeedLink,
	}, {
		Name:   DerivedGraphLayers,
		Inputs: []Input{InputPacks, InputRefs, InputLayers},
		Writes: []Write{
			{Step: StepLayer, Trigger: TriggerInstall, Writer: Writer{File: layersFile},
				Charge: "installed with its layer, from the same parts — the commit-graph layer is the last " +
					"section of a layer's bytes — and the commit-graph-chain from the rows, a line a layer that " +
					"carries one"},
			// THE NODE'S TIP over the tail (maintain.go), git's: its Covers are
			// the tail's — its packs, their objects and their bytes — since a
			// split write walks only the commits no layer beneath it holds, so
			// its time and its own memory follow the tail, never the history,
			// beside a fixed part its filters' bound sets (TipGraphBaseBytes).
			{Step: StepTipGraph, Trigger: TriggerMaintainer, Lane: gitproc.ClassMaintenance,
				Deadline: func(c Covers) time.Duration {
					return gitproc.PackWorkDeadline(c.DiskBytes) + gitproc.HistoryWalk(c.Objects)
				},
				BytesPerObject: TipGraphBytesPerObject, BaseBytes: TipGraphBaseBytes,
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
		// Cut wherever the multi-pack-index chain is (prefixOf): a graph layer
		// is its layer's, over the same packs, and names its base's. The
		// node's tip goes with every cut, every placement and every pack
		// removed, and stays across a retirement that keeps the whole chain
		// (layers.go's tipGoes); the refs moving keep it, since it covers the
		// commits it covers whatever the refs name next. An expunge's commit
		// keeps every published layer, as the multi-pack-index chain's
		// declaration does, and takes the tip (tipGoes), which may name a
		// commit it expunged: in chain mode the tip is the node's own graph,
		// and goes where own mode's commit-graph does.
		OnPackAdded: Keep, OnPackRemoved: Prefix, OnRefsMoved: Keep, OnReported: Keep, OnHistoryGone: Keep,
		OnPackRetired: Prefix, OnPackExpunged: Prefix, OnLayersMoved: Prefix,
		// The published graph layers only, with their layers (rebuild.go's
		// seedLayers): no row names the node's tip, so a rebuild writes it
		// again over its new copy's tail, at once.
		Seed: SeedLink,
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
	// EACH WRITE IS OF ONE KIND, said whole for its kind; a declaration may
	// have writes of both (the installed commit-graph layers and the node's
	// tip over them), and what git writing any of its files asks of the
	// declaration is held below (gitWritten).
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
			return bad(field+".Step", "is %q, none of %v", w.Step, Steps)
		case !w.Trigger.Valid():
			return bad(field+".Trigger", "is %q, none of %v", w.Trigger, Triggers)
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
		switch {
		case o.Prior && o.Named:
			return bad(field, "is both Prior and Named: the forty hex digits of a name are the checksum before "+
				"its trailer or the trailer itself, never both")
		case (o.Prior || o.Named) && o.Name.NumSubexp() != 1:
			return bad(field+".Name", "%q has %d capture groups, and a name carrying its checksum has one: "+
				"the forty hex digits", o.Name, o.Name.NumSubexp())
		}
		// A TREE IS ONE DIRECTORY THE CACHE'S OWN CODE OWNS WHOLE: git writes
		// no directory the cache keeps without reading inside, and a pattern of
		// many names would be many trees no writer keeps a total of — so its
		// one literal name carries no checksum either, which the rule above
		// already holds of a name with no capture group.
		if o.Tree {
			_, whole := o.Name.LiteralPrefix()
			switch {
			case git:
				return bad(field+".Tree", "is set on a derived file git writes: git writes no directory the "+
					"cache owns whole")
			case !whole:
				return bad(field+".Name", "%q is no literal, and a tree is one directory", o.Name)
			case !o.Main:
				// What a tree holds is its writer's record (Derived.tree), so the
				// tree is the declaration: its presence, and its bytes.
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
	// A DAMAGE REPORT NAMES ONLY WHAT GIT READS, and who reads a declaration's
	// files is its Part's to say (gitReads), never who writes them. Every
	// declaration git WRITES — any write of it — is one git reads, so it
	// names a part a report drops; one the cache's own code writes names one
	// too where git reads its files (the installed chain) and none where
	// nothing but the cache does (the provenance index, the memo), which then
	// keeps its files on a report — no report can be about them, and the
	// check and the scrub hold them to their Check instead.
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
		return bad("Check", "is %q, none of %v", d.Check, Checks)
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
		return bad("Freshness", "is %q, none of %v", d.Freshness, Freshnesses)
	}
	for _, e := range Events {
		switch ch := d.On(e); {
		case !ch.Valid():
			return bad("On("+string(e)+")", "is %q, none of %v", ch, Changes)
		case ch == Prefix && !d.chained():
			// A PREFIX IS A CUT OF THE INSTALLED CHAIN, which only the chain's
			// own declarations hold: anything else has no prefix to keep.
			return bad("On("+string(e)+")", "is %q, which cuts an installed chain, and this declaration holds "+
				"none (Check %q is the chain's)", ch, CheckLayers)
		}
	}
	switch {
	case !d.Seed.Valid():
		return bad("Seed", "is %q, none of %v", d.Seed, SeedRules)
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

// validateGit refuses a write git runs that leaves anything of it unsaid: its
// lane, its process's deadline, its memory charge an object and its argument
// list, which begins with its subcommand — and that names a Charge, which is
// an in-process write's.
func (w Write) validateGit(field string, bad func(field, format string, args ...any) error) error {
	switch {
	case !w.Lane.Valid():
		return bad(field+".Lane", "is %q, none of %v", w.Lane, gitproc.Classes)
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
		return bad(field+".Args", "is %q, and an in-process write hands git nothing", w.Args)
	case strings.TrimSpace(w.Charge) == "":
		return bad(field+".Charge", "is empty: say, in a sentence, what the write holds while it runs")
	}
	return nil
}

// validateChained refuses a declaration of the installed chain (CheckLayers)
// that the chain's code could not keep in step with the chain: one git does
// not read — the chain is git's index, read by every git process — one whose
// files are not one chain file and the layers' hashfiles (the chain file the
// declaration's one Main, a literal, and no other file named so), or one that
// answers an event Drop or a change of the chain anything but Prefix. Dropped
// whole, a chain would leave its other declaration's chain naming layers the
// first no longer holds, and the two chains are cut at the one place
// (prefixOf) or not at all.
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

// derivedRoots is every directory of an incarnation's root that holds what
// the rows and the declarations put there: objects/ — the packs', and git's
// derived files' — the dropping view (gitproc.DroppingDir, the packs an
// expunge's begin left dropping), and every other root a declaration owns or
// holds a file under, each once, in the declarations' order. What a boot's
// walk of a copy with no state reads (measureIncarnation), so a root
// validateAll admits is one that walk reads.
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
// counter and gauge are keyed by step (only git's writes are maintainer
// writes); every file a declaration git READS owns or holds under objects/ —
// whoever writes it: the installed chain is the cache's own write of git's
// files — and every file one no git process reads under crewlet/ (engineRoot)
// — the root is the repository's own fixed files, refs/ stays empty (a loose
// ref would outrank packed-refs for every reader), and objects/ is git's, the
// one directory a push's scratch names as its alternate; every part a
// damage report may drop (gitproc.Damage.Drops) some declaration's, or that
// damage would drop nothing; and no declaration's files inside a directory one
// owns whole (Owned.Tree). Open refuses a cache whose declarations fail it,
// so a build whose declarations are wrong never opens one — and
// TestEveryDerivedFileIsDeclared catches it first.
//
// THE ROOT RULE IS WHAT A BOOT'S WALK READS (M3-decisions M3-O77): a copy with
// no state is counted by its files under objects/ and every other root a
// declaration owns files in (derivedRoots), and its packed-refs
// (measureIncarnation) — which counts every derived file only because this
// rule keeps them under those roots. A root admitted here is a root that walk
// reads too, read from the same declarations, so the two cannot drift.
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
