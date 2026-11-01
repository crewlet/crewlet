package code

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN ENGINE WRITE is a change a seat or a person asks of a repository through
// a tool — files committed, a commit reverted or picked, a branch created or
// deleted, a review's suggestions applied — that the engine BUILDS on the
// repository's git node, in a scratch
// beside its cache, and LANDS as a push through the one landing every push
// takes (internal/code/host's Server.Write, ADR-0062). This package holds what
// both ends of it import, as it does for a read by object id: the WIRE — the
// write ([EngineWrite]), its answer ([EngineWriteResult]) and the change's
// own refusals ([WriteRefusal]) — the one rule a write is held to by the tool
// that asks it, the router that carries it and the git node that builds it
// ([EngineWrite.Validate]), and the bounds that size it.
//
// NOT A RECORD: nothing here rides the code log. What a write leaves is a
// push — its proposal and its row, whose shapes are the domain's own — so no
// field here takes a versionedFields row.

// Source is the code host as a delivery's SURFACE: the name a seat's code
// write delivers to (internal/agent/builtin registers every code write tool
// with tools.DeliversTo(Source)), and the notification source the code feed
// publishes a change's wakes under ([Translator], [Parser]) — one name for
// both, so the delivery gate compares a write's surface and a wake's source in
// one spelling, and an addressed code wake is answered by a code write, as
// tracker.Source and
// pages.Source are each one. FIXED: a wake published under another spelling
// would find no delivery to it, and the gate would read every obligation
// about code as reaching nobody.
const Source = "code"

// EngineWriteKind is what an engine write builds: a closed set
// ([EngineWriteKinds]).
type EngineWriteKind string

const (
	// WriteCommitFiles commits a set of file operations onto a branch: its
	// head ([EngineWrite.ExpectedHead]), or a new branch's start
	// ([EngineWrite.Base]), or nothing — a root commit.
	WriteCommitFiles EngineWriteKind = "commit_files"
	// WriteRevert commits the inverse of one commit onto a branch's head.
	WriteRevert EngineWriteKind = "revert"
	// WriteCherryPick commits one commit's change onto a branch's head.
	WriteCherryPick EngineWriteKind = "cherry_pick"
	// WriteCreateBranch creates a branch at a commit the repository holds.
	WriteCreateBranch EngineWriteKind = "create_branch"
	// WriteDeleteBranch deletes a branch at the head its caller read.
	WriteDeleteBranch EngineWriteKind = "delete_branch"
	// WriteApplySuggestions commits findings' suggestions onto a change's
	// source branch at the head its caller read ([EngineWrite.Suggestions]):
	// each suggestion's lines placed in the head's file by the one rule a
	// finding is shown by ([Reanchor]), and the file's lines put in their
	// place — all of them or none.
	WriteApplySuggestions EngineWriteKind = "apply_suggestions"
	// WriteMergeChange merges a change ([EngineWrite.Change]) into its
	// target ([EngineWrite.Ref]) by its method ([EngineWrite.Method]), at the
	// head its caller read ([EngineWrite.ExpectedHead]): built on the
	// repository's git node onto the target's tip and landed as ONE MERGE
	// RECORD its gate decides ([MergeProposal], ADR-0066) — or, on a target
	// whose merges land through its queue, enqueued there ([QueueProposal]).
	// Never a push: a merge moves its target, merges its change and may
	// delete its source in one record, and an engine-built push carries one
	// command (M5-D10).
	WriteMergeChange EngineWriteKind = "merge_change"
	// WriteRebaseChange replays a change's ([EngineWrite.Change]) commits onto
	// its target's tip, each keeping its author and its author date (U43 a),
	// and moves its source branch ([EngineWrite.Ref], at the head its caller
	// read) to the last of them — ONE PUSH, forced, since the branch no longer
	// holds its old head. At most [MaxRebaseCommits] commits, none a merge;
	// the first that conflicts stops it, nothing stored.
	WriteRebaseChange EngineWriteKind = "rebase_change"
)

// EngineWriteKinds is the closed set, in the order a reader is told them.
func EngineWriteKinds() []EngineWriteKind {
	return []EngineWriteKind{WriteCommitFiles, WriteRevert, WriteCherryPick, WriteCreateBranch, WriteDeleteBranch,
		WriteApplySuggestions, WriteMergeChange, WriteRebaseChange}
}

// Valid reports a kind this build knows.
func (k EngineWriteKind) Valid() bool { return slices.Contains(EngineWriteKinds(), k) }

// Commits reports whether the kind makes a commit — and so a pack, a
// signature and a build — rather than only move a ref to a commit the
// repository holds — and lands it as ONE PUSH. A merge_change is a pipeline
// of its own, whose commit is its method's and whose signature U43's
// (internal/code/host's merge.go).
func (k EngineWriteKind) Commits() bool {
	return k == WriteCommitFiles || k == WriteRevert || k == WriteCherryPick || k == WriteApplySuggestions ||
		k == WriteRebaseChange
}

// BuildDeadline is the git run time a write of the kind is given for its
// whole build, every process of it together: [MergeBuildDeadline] for a
// merge_change, whose one merge-tree is the one a change's mergeability is
// measured by; [RebaseBuildDeadline] for a rebase_change, which replays up to
// [MaxRebaseCommits] commits a merge each; and [WriteBuildDeadline] for every
// other kind. The estate's attempt on a git node follows it
// (internal/estate's Timing.GitWriteAttemptFor), so a kind given a longer
// build is given the time to run it and nothing more.
func (k EngineWriteKind) BuildDeadline() time.Duration {
	switch k {
	case WriteMergeChange:
		return MergeBuildDeadline
	case WriteRebaseChange:
		return RebaseBuildDeadline
	}
	return WriteBuildDeadline
}

// FileOpKind is what one operation of a commit_files does to one path: a
// closed set ([FileOpKinds]).
type FileOpKind string

const (
	// FileOpPut makes the path a file: of the text it carries, or of a blob
	// the repository holds.
	FileOpPut FileOpKind = "put"
	// FileOpEdit replaces text that occurs EXACTLY ONCE in an existing file.
	FileOpEdit FileOpKind = "edit"
	// FileOpDelete removes the entry at the path — a file, a link, a
	// submodule or a whole directory.
	FileOpDelete FileOpKind = "delete"
	// FileOpMove moves the entry at the path to another, sending no bytes.
	FileOpMove FileOpKind = "move"
	// FileOpMode flips an existing file between 100644 and 100755.
	FileOpMode FileOpKind = "mode"
)

// FileOpKinds is the closed set.
func FileOpKinds() []FileOpKind {
	return []FileOpKind{FileOpPut, FileOpEdit, FileOpDelete, FileOpMove, FileOpMode}
}

// Valid reports a kind this build knows.
func (k FileOpKind) Valid() bool { return slices.Contains(FileOpKinds(), k) }

// The modes a file operation names, as git spells them in a tree.
const (
	ModeFile       = "100644"
	ModeExecutable = "100755"
	ModeLink       = "120000"
)

// EngineWrite is one write the engine builds and lands as a push, asked of the
// repository's home git node (ADR-0059). Every field is held to
// [EngineWrite.Validate] by every party to it.
type EngineWrite struct {
	Kind   EngineWriteKind `json:"kind"`
	RepoID string          `json:"repo_id"`
	// Principal is who writes: a seat with the run that made the write
	// (RunProvenance, its turn at least, and never a coding run: a write is a
	// tool call's, and a coding run's box pushes through git), or a person
	// through a credential bound to their human seat. A token bound to nobody
	// writes nothing.
	Principal Principal `json:"principal"`
	// OpID is the caller's operation — a turn's derived step, a keyed
	// request's (the act route's Idempotency-Key, carried as
	// builtin.Actor.WorkKey) or an operator's op_id over MCP: a UUIDv7 this
	// engine minted, at most statelog.MaxCallerOpIDBytes. The push id is
	// derived from it ([WritePushID]), so a retry is the same push, and so is
	// its commit: both dates are read off the instant it carries.
	OpID string `json:"op_id"`
	// Fresh is the caller's floor on the code log — a seat's session, a
	// person's linearizable — which the git node's copy is brought to before
	// anything is judged. A floor on another log is [ErrInvalid].
	Fresh statelog.Freshness `json:"fresh"`
	// Ref is the branch the write moves: refs/heads/<branch>.
	Ref string `json:"ref"`
	// ExpectedHead is the commit the caller last read at Ref: required to
	// move or delete a branch, and empty to create one, which needs the ref
	// absent. The write's commit sits on it.
	ExpectedHead string `json:"expected_head,omitempty"`
	// Base is a commit_files CREATE's start: the commit the new branch's first
	// commit sits on, so a commit onto a new branch is one push; empty with an
	// empty ExpectedHead is a root commit.
	Base string `json:"base,omitempty"`
	// Message is the commit's message: required of a commit_files, and of a
	// revert or a cherry-pick it replaces the message git's own revert and
	// cherry-pick -x write.
	Message string `json:"message,omitempty"`
	// Files are a commit_files' operations, at most [MaxFileOps].
	Files []FileOp `json:"files,omitempty"`
	// Commit is a revert's or a cherry-pick's subject, and a create_branch's
	// target.
	Commit string `json:"commit,omitempty"`
	// Suggestions are an apply_suggestions' findings' suggestions, 1 to
	// [MaxSuggestionOps], each finding at most once.
	Suggestions []SuggestionOp `json:"suggestions,omitempty"`
	// Ceiling is whom a SEAT'S write is made for inside a turn somebody woke
	// ([WriteCeiling]), nil for a write made for its seat alone or for a
	// person: the git node that builds it judges the write's one command
	// against the seat's grant held to the ceiling's ([Grant.Under]), from
	// its own chart and the synced row, beside the seat's own. Refused beside
	// a person's write, which no ceiling bounds. The committer's decide never
	// reads it, as it never reads a coding box's (the run's grant and its
	// ceiling are an admission's).
	//
	// A write is no record (this file's doc), so the field takes no
	// versionedFields row; it crosses the estate's wire to the repository's
	// git node under the code domain's stated condition — no build carrying
	// the domain has run without it, none of the domain being on main — so
	// every git node asked it reads it.
	Ceiling *WriteCeiling `json:"ceiling,omitempty"`
	// Change is a merge_change's or a rebase_change's change, by its number
	// on the repository — web!42's 42, the name every change tool and the
	// `change` read give it — whose row the git node reads at the caller's
	// floor. A merge's Ref is the change's target and its ExpectedHead the
	// change's head; a rebase's Ref is the change's source, which it moves,
	// at that head — each as its caller read them.
	Change int64 `json:"change,omitempty"`
	// Method is a merge_change's [MergeMethod]: a merge commit, one squashed
	// commit, or a fast-forward.
	Method MergeMethod `json:"method,omitempty"`
	// DeleteSource is whether a merge_change deletes the change's source
	// branch as it merges, as its caller resolved it (Q4: a seat's branch, by
	// its rule's delete_source).
	DeleteSource bool `json:"delete_source,omitempty"`
	// Override is a PERSON'S reason for merging past the target's merge gate,
	// at most [MaxOverrideReasonBytes] — merged directly, on a queue target
	// too — and "" for a merge its gate holds.
	Override string `json:"override,omitempty"`
	// CloseTask and AutoRevert are what a merge_change onto a QUEUE TARGET
	// records on its entry ([Enqueue]): whether its landing closes its work
	// item, and the enqueuer's consent to a revert of a landing of it that
	// breaks the target.
	CloseTask  bool `json:"close_task,omitempty"`
	AutoRevert bool `json:"auto_revert,omitempty"`
	// Home is whether this attempt was asked of the walk's EFFECTIVE HOME —
	// stamped by the router's keyed git walk on every attempt
	// ([GitQuery.Home]'s rule), never by a caller — the one node that may
	// fill a copy of the repository it does not hold.
	Home bool `json:"home,omitempty"`
}

// FileOp is one operation of a commit_files, on one path: only the fields its
// kind takes are set ([EngineWrite.Validate]).
type FileOp struct {
	Op   FileOpKind `json:"op"`
	Path string     `json:"path"`
	// Content is a put's text — a JSON string, so UTF-8: a binary file goes
	// by BlobOID or through a coding box. A link's target, for a put of mode
	// 120000, which takes Content alone.
	Content *string `json:"content,omitempty"`
	// BlobOID is a put's existing blob of the repository, by its whole id.
	BlobOID string `json:"blob_oid,omitempty"`
	// Old is an edit's text, which must occur in its file exactly once, and
	// New its replacement ("" deletes it).
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
	// To is a move's new path.
	To string `json:"to,omitempty"`
	// Mode is a put's — 100644, 100755 or 120000, "" keeping an existing
	// file's or 100644 — and a mode operation's, 100644 or 100755: turning a
	// file's bytes into a link's target is no gesture git has (chmod flips
	// the executable bit alone), so a link is made by a put.
	Mode string `json:"mode,omitempty"`
}

// SuggestionOp is one finding's suggestion as an apply_suggestions carries it:
// the lines the finding is anchored to — a file of a revision's head, by the
// blob they were read in, and its lines, 1-based and inclusive — and the text
// to put in their place, "" deleting them. Its lines are placed in the file at
// the head the write lands on by [Reanchor], from the diff between that blob
// and the head's, before anything is put.
type SuggestionOp struct {
	FindingID string `json:"finding_id"`
	Path      string `json:"path"`
	BlobOID   string `json:"blob_oid"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Text      string `json:"text"`
}

// MaxSuggestionOps is the most suggestions one apply_suggestions carries: a
// review's findings ([MaxFindingsPerReview]), so the suggestions one review
// made are applied by one write, and what its build places at once is bounded
// as a re-anchoring's is ([MaxReanchors]).
const MaxSuggestionOps = MaxFindingsPerReview

// EngineWriteResult is what an engine write came to.
type EngineWriteResult struct {
	// Outcome is the push's — applied, pending or unknown, from its row only
	// — and empty where the push was refused or no push was made.
	Outcome statelog.Outcome `json:"outcome,omitempty"`
	// Command is the ref update as the landing answered it: its result, its
	// reason and its sentence. Zero beside a Refusal.
	Command Command `json:"command"`
	// Commit is the commit the write made — or, for a create_branch, the
	// commit the branch was created at — where the outcome is applied or
	// pending.
	Commit string `json:"commit,omitempty"`
	// PushID is the push the write lands under ([WritePushID]).
	PushID string `json:"push_id"`
	// Position is the push row's, where it applied or is pending.
	Position statelog.Position `json:"position"`
	// Refusal is a refusal of the CHANGE rather than of the push — nothing
	// was uploaded and nothing proposed — with Detail its sentence.
	Refusal WriteRefusal `json:"refusal,omitempty"`
	Detail  string       `json:"detail,omitempty"`
	// Conflicts are a revert's or a cherry-pick's, as many as fit
	// MaxGitAnswerBytes encoded, the rest counted in ConflictsOmitted.
	// Nothing is stored for a write that conflicts.
	Conflicts        []Conflict `json:"conflicts,omitempty"`
	ConflictsOmitted int        `json:"conflicts_omitted,omitempty"`
	// Merge is a merge_change's own answer beside the write's fields — nil
	// for every other kind. Outcome, Commit (the commit the target moved to),
	// PushID (the merge's) and Position are the merge record's, or the
	// enqueue's where its target's queue took the change; a refusal of the
	// change's build (a conflict, not_fast_forward, too_large) is Refusal's,
	// as any write's is.
	Merge *MergeAnswer `json:"merge,omitempty"`
}

// MergeAnswer is what a merge_change came to beside an engine write's own
// answer.
type MergeAnswer struct {
	// Queued is a change its target's queue took rather than merged — a
	// target whose merge gate requires a check (M5-D9) — with EntryID its
	// entry and Place its place in the queue, from 1, once the enqueue
	// applied.
	Queued  bool   `json:"queued,omitempty"`
	EntryID string `json:"entry_id,omitempty"`
	Place   int    `json:"place,omitempty"`
	// Refusal is why nothing merged or was enqueued — the committer's
	// decide's, or the git node's own before it built anything, judged at
	// the log's head — with Detail its sentence and Unmet every precondition
	// of the change's merge gate a gate_unmet did not meet, each naming whom
	// it waits on.
	Refusal MergeRefusal   `json:"refusal,omitempty"`
	Detail  string         `json:"detail,omitempty"`
	Unmet   []Precondition `json:"unmet,omitempty"`
	// Recomputes is how many times the merge was built again onto a target
	// that moved under it before its record was decided, at most
	// [MergeRecomputes].
	Recomputes int `json:"recomputes,omitempty"`
}

// Conflict is one path a three-way merge could not resolve.
type Conflict struct {
	Path string `json:"path"`
	// Kind is merge-tree's own word for it: "CONFLICT (contents)",
	// "CONFLICT (modify/delete)", ….
	Kind string `json:"kind"`
	// Base, Ours and Theirs are the stage blobs — merge-tree's stages 1, 2
	// and 3 — "" where a stage is absent: each is in the repository, so a
	// read by object id pages it whole, and a conflict cut short is never a
	// dead end (M3-decisions M3-O42: tagged wire fields).
	Base   string `json:"base,omitempty"`
	Ours   string `json:"ours,omitempty"`
	Theirs string `json:"theirs,omitempty"`
	// Marked is the start of the file as merge-tree wrote it, its conflict
	// markers in, at most ConflictMarkedBytes; Truncated is a file longer
	// than that.
	Marked    string `json:"marked,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// WriteRefusal is a refusal of the change an engine write asked for:
// UNRECORDED, decided on the git node before anything is stored. A closed set
// ([WriteRefusals]).
type WriteRefusal string

const (
	// WriteConflict is a revert or a cherry-pick that does not apply cleanly.
	WriteConflict WriteRefusal = "conflict"
	// WriteNothingChanged is a change that leaves the tree as it is: no
	// commit is made.
	WriteNothingChanged WriteRefusal = "nothing_to_change"
	// WriteEditNotFound is an edit whose old text occurs nowhere in its file.
	WriteEditNotFound WriteRefusal = "edit_not_found"
	// WriteEditAmbiguous is an edit whose old text occurs more than once.
	WriteEditAmbiguous WriteRefusal = "edit_ambiguous"
	// WriteEditBinary is an edit of a file git reads as binary: a NUL in its
	// first 8 000 bytes, git's own heuristic.
	WriteEditBinary WriteRefusal = "edit_binary"
	// WriteEditLineEndings is an edit spanning a line in a file holding a
	// carriage return.
	WriteEditLineEndings WriteRefusal = "edit_line_endings"
	// WritePathConflict is a path that is a file where a directory is needed,
	// or the reverse, or two operations on one path or one beneath another's.
	WritePathConflict WriteRefusal = "path_conflict"
	// WriteNotFound is an edit, a delete, a move or a mode of a path the
	// tree does not hold.
	WriteNotFound WriteRefusal = "path_not_found"
	// WriteNotAFile is an edit or a mode of a directory, a link or a
	// submodule.
	WriteNotAFile WriteRefusal = "not_a_file"
	// WriteMergeCommit is a revert or a cherry-pick of a merge commit, or a
	// rebase of a change one of whose commits is one: a merge needs a
	// mainline to replay.
	WriteMergeCommit WriteRefusal = "merge_commit"
	// WriteTooLarge is a pack past MaxWritePackBytes, or a build past
	// WriteBuildDeadline.
	WriteTooLarge WriteRefusal = "too_large"
	// WriteRejectedObject is an object the build made that the relay's own
	// check — index-pack --strict, receive.fsckObjects' — refuses, or a
	// commit a rebase would replay that git would not write back as it was:
	// an author it names in a form git trims or refuses, or a message in an
	// encoding other than UTF-8.
	WriteRejectedObject WriteRefusal = "rejected_object"
	// WriteSuggestionOutdated is an apply_suggestions one of whose findings'
	// lines changed since they were read — a hunk between the blob they were
	// read in and the head's touched them, the file is gone or no regular
	// file at the head, or it holds fewer lines than the finding names — each
	// such finding named: nothing is applied.
	WriteSuggestionOutdated WriteRefusal = "suggestion_outdated"
	// WriteSuggestionOverlap is an apply_suggestions two of whose findings'
	// lines, placed in the head's file, overlap — every finding in an
	// overlap named, beside one it overlaps: nothing is applied.
	WriteSuggestionOverlap WriteRefusal = "suggestion_overlap"
	// WriteNotFastForward is a fast_forward merge_change of a change whose
	// head does not hold its target's tip: the target cannot move to it
	// without dropping what it holds — merged or squashed, it can.
	WriteNotFastForward WriteRefusal = "not_fast_forward"
	// WriteTooManyCommits is a rebase_change of a change carrying more than
	// [MaxRebaseCommits] commits its target does not hold: a history that
	// long is rebased in a coding box.
	WriteTooManyCommits WriteRefusal = "too_many_commits"
)

// WriteRefusals is the closed set.
func WriteRefusals() []WriteRefusal {
	return []WriteRefusal{WriteConflict, WriteNothingChanged, WriteEditNotFound, WriteEditAmbiguous,
		WriteEditBinary, WriteEditLineEndings, WritePathConflict, WriteNotFound, WriteNotAFile, WriteMergeCommit,
		WriteTooLarge, WriteRejectedObject, WriteSuggestionOutdated, WriteSuggestionOverlap, WriteNotFastForward,
		WriteTooManyCommits}
}

// Valid reports a refusal this build knows.
func (r WriteRefusal) Valid() bool { return slices.Contains(WriteRefusals(), r) }

// The bounds of an engine write.
const (
	// MaxCommitPayloadBytes bounds what one write carries: its Files or its
	// Suggestions and its Message together, EACH AS encoding/json SPELLS IT —
	// the encoding every hop of the write's way sends it in, in which a '<', a
	// '>', a '&' and a control byte other than a tab, a newline, a carriage
	// return, a backspace or a form feed take six bytes — for every kind that
	// carries any: a commit_files' operations and message, an
	// apply_suggestions' suggestions and message, and the message a revert or
	// a cherry-pick gives in place of git's own ([EngineWrite.Validate]). A
	// MEBIBYTE: an engine write is a doc or a configuration fix, never a
	// codebase — a change larger than this belongs in a coding box, which
	// pushes through the relay with no such bound. It is what sizes a write in
	// flight: a git node holds each one it builds for a peer through every
	// wait it makes, in a slot of its writes' lane (internal/estate's
	// laneGitWrites, which states what a slot holds, measured).
	MaxCommitPayloadBytes = 1 << 20

	// MaxFileOps is the most operations one commit_files carries: past every
	// doc or configuration change, and — with MaxTreePathDepth — what bounds
	// the directories one build rewrites: every directory above a path the
	// write names, at most 2 × MaxFileOps × (MaxTreePathDepth − 1) and the
	// root, since a move names two paths. MEASURED (BenchmarkEngineCommit,
	// internal/code/host's doc.go): a thousand files put at depth six into a
	// 100 000-file tree build in 0.28–0.52 s at the p50 and 0.37–0.64 s at
	// the p99 over two runs — a fifteenth of WriteBuildDeadline at the most —
	// into a pack of 58 KB, so at the depths a write's paths keep the bound
	// is the payload's and the purpose's, never the build's: a thousand
	// operations of a kilobyte each are MaxCommitPayloadBytes. A thousand
	// paths at MaxTreePathDepth itself are the build deadline's to refuse
	// (MaxTreePathDepth).
	MaxFileOps = 1_000

	// MaxWritePackBytes bounds the one pack an engine write stores: FOUR
	// MEBIBYTES, four payloads' worth — a commit_files' pack is at most its
	// payload compressed plus the trees it touches, and a revert's or a
	// cherry-pick's past it is a change for a coding box. The largest new
	// object a write makes is a touched directory's tree, written whole into
	// its self-contained pack, its 20-byte ids incompressible. MEASURED
	// (BenchmarkEngineCommit's flat arms): a put into a directory of 100 000
	// entries packs 2 393 208 bytes, 24 an entry where the estimate was 40 —
	// zlib takes the names, never the ids — and one of 10 000, 240 KB; a
	// thousand files at depth six, 58 KB. So a directory of some 175 000
	// entries is the largest one write can touch, and four mebibytes stands.
	// A proposal of an engine-built push carrying a pack
	// past it is refused by every committer as one no git node built
	// (Proposal.Validate).
	MaxWritePackBytes = 4 << 20

	// WriteBuildDeadline is a write's whole build — every git process it
	// runs, together, sequentially: ONE READ'S DEADLINE, gitproc.ReadDeadline's
	// ten seconds, since every build step is a read-lane process of a
	// bounded local read's size, and BenchmarkEngineCommit's p99 of a hundred
	// files at depth six in a 100 000-file tree is held to a tenth of it:
	// MEASURED 0.07–0.24 s over two runs, and the dearest build it measures,
	// one file into a directory of 100 000 entries, 0.68–1.2 s. This package
	// imports no git process wrapper, so internal/code/host's
	// TestAWriteBuildIsOneReadsDeadline holds the equality.
	WriteBuildDeadline = 10 * time.Second

	// MergeBuildDeadline is a merge_change's whole build — its ancestry, its
	// one `merge-tree --write-tree`, its commit and its pack: THIRTY
	// SECONDS, three of a write's own (code/home's MergeabilityDeadline is
	// this constant), since the one merge-tree a merge runs is the one a
	// change's mergeability is recorded by, merging the same two commits —
	// the merge base found, both sides diffed against it, their changes
	// merged — and BenchmarkMergeability measures it (code/home's doc: 125 ms
	// for 1 000 files changed both ways, some 240 times inside). PROVISIONAL
	// as that deadline is: what moves it is a production merge whose p99
	// nears it. A merge past it is too_large, a change to merge in a coding
	// box.
	MergeBuildDeadline = 3 * WriteBuildDeadline

	// MergeRecomputes is how many times a merge_change is built again onto a
	// target that moved between its build and its decide (target_moved)
	// before it is answered target_moving: FOUR, the design's (M5-D10). A
	// branch moving under five consecutive builds is a branch moving faster
	// than its merges are built — a queue's work (M5-D9), or a person's —
	// and a fifth rebuild would only spend the attempt the write is given.
	MergeRecomputes = 4

	// MaxRebaseCommits is the most commits one rebase_change replays: A
	// THOUSAND (the design's, PLAN.md). Each is one merge-tree and one
	// commit-tree in its build, so the bound is what holds a rebase inside
	// RebaseBuildDeadline; a longer history is a coding box's to rebase.
	MaxRebaseCommits = 1_000

	// RebaseBuildDeadline is a rebase_change's whole build — its listing,
	// its commits read, and one merge-tree and one commit-tree a replayed
	// commit: FORTY SECONDS, PROVISIONAL. MEASURED (BenchmarkEngineRebase,
	// internal/code/host's doc.go "What a rebase costs", one sample at this
	// machine's bound): MaxRebaseCommits commits over a tree of 1 000 files
	// build in 3.1 s, some three milliseconds a commit, so the deadline holds
	// the longest rebase some thirteen times over for commits whose changes
	// are wider; what moves it is a production rebase whose p99 nears it.
	RebaseBuildDeadline = 40 * time.Second

	// MaxTreePathBytes is the longest path a write names: Linux's PATH_MAX,
	// since a path no checkout can create is a tree no client can use.
	MaxTreePathBytes = 4_096

	// MaxTreePathDepth is the most components a path an engine write NAMES
	// may have — an operation's path, a move's destination: SIXTY-FOUR, a
	// JUDGMENT. An engine write is a document or a configuration fix, whose
	// paths run from a few components to a couple of dozen even in a deep
	// tree — a source file of a Java module in a monorepo sits some fifteen
	// down — so sixty-four refuses no such change, and a deeper one belongs in
	// a coding box, whose push has no such bound.
	//
	// It is what bounds every cost of a write that GROWS WITH DEPTH, which
	// MaxTreePathBytes alone leaves at 2 048 components: the directories one
	// build rewrites, every one above a path the write names (MaxFileOps); its
	// mktree, one process a level, each a read-lane slot; what its plan and
	// its asks hold, a map entry and an ask of git a directory; and its read
	// of what every named path and every directory above one is, which git
	// walks from the root, at a cost growing with the SQUARE of the depth
	// (internal/code/host's building.paths: a thousand paths at this bound,
	// each with every directory above it, took 19 s of git on two E-cores,
	// which WriteBuildDeadline answers too_large). Unbounded, one mebibyte of
	// paths 2 047 components deep was some 1.1 GB of asks for git, the
	// directories of 253 of them for the plan to hold, and 2 047 mktree
	// processes for one. A repository's own deeper paths are never judged:
	// only the paths a write names, which EngineWrite.Validate holds to it
	// beside ValidTreePath — a rule of what a write may carry, as MaxFileOps
	// is, never of what a name in a tree may be, so ValidTreePath's other
	// callers (a check's report path) are not held to it.
	MaxTreePathDepth = 64

	// ConflictMarkedBytes is how much of a conflicted file a write answers:
	// TWO KIBIBYTES, enough to see what collided — each stage is readable
	// whole by its id (Conflict).
	ConflictMarkedBytes = 2 << 10

	// WriteUploadAllowance is what a write's attempt holds for its bundle's
	// PUT: objstore.MiBPace, one mebibyte at the pace floor every object
	// transfer is held to. A write's bundle is a few kilobytes in the common
	// case, and its one PUT a first byte's wait (BenchmarkEngineWrite: a p99
	// of 265 ms at the most against the S3 fake answering 200 ms late,
	// sixty-four writes at once, under a hundredth of this); a larger one on a
	// bucket at the floor is
	// answered busy BEFORE its upload when the caller's deadline cannot hold
	// it and the proposal after it (internal/code/host's deadline rule), so a
	// slow bucket costs a refusal, never a PUT nobody proposes.
	WriteUploadAllowance = objstore.MiBPace

	// MinProposeAttempt is the least a push's proposal needs of its caller's
	// deadline as it leaves the node that built the push: the serving data
	// node's wait for the asker's floor at its whole budget
	// (statelog.ReadBudget), then a whole CommitBudget and ReplyMargin, which a
	// repository's committer must still find before the push's deadline to
	// start its batch at all — under it the push is answered busy, undecided.
	// ONE SUM: the estate router refuses a write attempt no longer than it
	// (internal/estate's minWriteAttempt is this constant), and the code host
	// refuses to upload an engine write whose caller left less than it after
	// the upload.
	MinProposeAttempt = statelog.ReadBudget + CommitBudget + ReplyMargin
)

// WritePushID is the push id an engine write lands under: an UNNAMED
// derivation of the caller's operation at the operation's own instant — a
// canonical UUIDv7, which is what a scratch's name and a proposal's push id
// must be (a named derivation carries a `.<name>` suffix both refuse) — and
// never statelog.StepOpID, which would give two steps of one gesture one
// push. So a retry of the write — on the same node, on the runner-up after
// silence, after a crash — is the same push, and collapses at its committer.
// A caller holds the id to statelog.CheckCallerOpID first
// ([EngineWrite.Validate]).
func WritePushID(opID string) string {
	t, _ := statelog.OpMintedAt(opID)
	return statelog.DeriveOpID(t, "", "code_write", opID)
}

// Instant is the instant the write's operation was minted at, to the second:
// the author date of the commit it makes, so a retry builds the same commit.
// Zero for an operation id that carries none, which [EngineWrite.Validate]
// refuses.
func (w EngineWrite) Instant() time.Time {
	t, _ := statelog.OpMintedAt(w.OpID)
	return t.Truncate(time.Second)
}

// Validate refuses a write no party to it may act on ([ErrInvalid], naming the
// field): the one rule the tool that asks a write, the router that carries it
// and the git node that builds it all hold it to.
func (w EngineWrite) Validate() error {
	if !w.Kind.Valid() {
		return fmt.Errorf("%w: kind: %q is not one of %v", ErrInvalid, w.Kind, EngineWriteKinds())
	}
	if id, err := uuid.Parse(w.RepoID); err != nil || id.String() != w.RepoID {
		return fmt.Errorf("%w: repo_id: %q is not a repository's id", ErrInvalid, w.RepoID)
	}
	// WHO WRITES IS NAMED, and is somebody who may: a seat with the run that
	// made the write, whose push names it; a person through a credential
	// bound to their human seat, whose seat is the commit's author. A token
	// bound to nobody fetches and writes nothing (ADR-0055).
	if why := w.Principal.invalid(); why != "" {
		return fmt.Errorf("%w: %s", ErrInvalid, why)
	}
	switch {
	case w.Principal.Kind == PrincipalAgent && w.Principal.Run.IsZero():
		return fmt.Errorf("%w: principal.run: a seat's write names the run that made it — its turn at least, "+
			"stamped by the engine from the call", ErrInvalid)
	case w.Principal.Run.RunID != "":
		// A WRITE IS A TOOL CALL'S, whose run is the turn that called it —
		// a coding run's box pushes through git — and the push it lands as
		// is engine-built, which Proposal.Validate refuses naming a coding
		// run. Refused here, by the rule every party holds a write to,
		// rather than by the landing after a git node has built it.
		return fmt.Errorf("%w: principal.run.run_id: a write the engine builds is a tool call's, whose run "+
			"is its turn: it names no coding run", ErrInvalid)
	case w.Principal.Kind == PrincipalOperator && w.Principal.Seat == "":
		return fmt.Errorf("%w: principal.seat: a write is a person's, through a credential bound to their human "+
			"seat — a token bound to nobody writes nothing", ErrInvalid)
	}
	if w.Ceiling != nil {
		if why := w.Ceiling.invalid(w.Principal); why != "" {
			return fmt.Errorf("%w: %s", ErrInvalid, why)
		}
	}
	if err := statelog.CheckCallerOpID(w.OpID); err != nil {
		var bad *statelog.CallerOpIDError
		if errors.As(err, &bad) {
			return fmt.Errorf("%w: op_id %s", ErrInvalid, bad.Fault)
		}
		return fmt.Errorf("%w: op_id: %w", ErrInvalid, err)
	}
	if err := w.Fresh.OnlyOn(topics.CodeLogStream); err != nil {
		return fmt.Errorf("%w: fresh: %w", ErrInvalid, err)
	}
	if branch, ok := strings.CutPrefix(w.Ref, "refs/heads/"); !ok || !gitref.ValidBranch(branch) {
		return fmt.Errorf("%w: ref: %q is not a branch: an engine write moves refs/heads/<branch>, a branch git "+
			"takes", ErrInvalid, w.Ref)
	}
	for _, id := range []struct{ field, oid string }{
		{"expected_head", w.ExpectedHead}, {"base", w.Base}, {"commit", w.Commit},
	} {
		if id.oid != "" && (!gitref.ValidOID(id.oid) || id.oid == ZeroOID) {
			return fmt.Errorf("%w: %s: %q is not a whole object id: a write names commits by the 40-digit ids a "+
				"read resolved", ErrInvalid, id.field, id.oid)
		}
	}
	if err := w.validKind(); err != nil {
		return err
	}
	if w.Message != "" {
		if strings.IndexByte(w.Message, 0) >= 0 {
			return fmt.Errorf("%w: message: it holds a NUL byte, which git's own check of a commit refuses",
				ErrInvalid)
		}
		if why := gitref.CommitText(w.Message); why != "" {
			return fmt.Errorf("%w: message: it %s", ErrInvalid, why)
		}
	}
	if err := w.validPayload(); err != nil {
		return err
	}
	if err := w.validSuggestions(); err != nil {
		return err
	}
	return w.validFiles()
}

// validPayload holds what the write carries to [MaxCommitPayloadBytes]: its
// operations or its suggestions and its message together, each as
// encoding/json encodes it, for every kind that carries any — a commit_files'
// operations and message, an apply_suggestions' suggestions and message, and
// a revert's or a cherry-pick's message.
//
// MEASURED AS THE WIRE SPELLS IT, NEVER BY THE TEXT'S LENGTH. What a write
// costs every party holding it is its encoding — the router's request, the git
// node's slot of its writes' lane — and encoding/json spells a '<', a '>', a
// '&' and most control bytes in six bytes each (queue.ErrTooLarge states the
// same of every payload). Counted by its length, a commit_files' message of a
// mebibyte of '<' crossed at six, and a revert's or a cherry-pick's message,
// bounded by nothing here, at up to the transport's whole eight
// (queue.MaxPayloadBytes): past it the transport refused the request to every
// peer, while the home's own seat had the same write built in process — an
// answer that depended on which node asked. A suggestion's text is code a
// reviewer wrote, as full of '<' and '&' as any file's.
func (w EngineWrite) validPayload() error {
	n := 0
	if len(w.Files) > 0 {
		encoded, err := json.Marshal(w.Files)
		if err != nil {
			return fmt.Errorf("%w: files: %w", ErrInvalid, err)
		}
		n += len(encoded)
	}
	if len(w.Suggestions) > 0 {
		encoded, err := json.Marshal(w.Suggestions)
		if err != nil {
			return fmt.Errorf("%w: suggestions: %w", ErrInvalid, err)
		}
		n += len(encoded)
	}
	if w.Message != "" {
		encoded, err := json.Marshal(w.Message)
		if err != nil {
			return fmt.Errorf("%w: message: %w", ErrInvalid, err)
		}
		n += len(encoded)
	}
	switch {
	case n <= MaxCommitPayloadBytes:
		return nil
	case len(w.Files) > 0:
		return fmt.Errorf("%w: files: the change — its operations and its message, as JSON spells them — is %d "+
			"bytes, past the %d one engine write may carry (code.MaxCommitPayloadBytes); make a change this large "+
			"in a coding box (run_sandbox)", ErrInvalid, n, MaxCommitPayloadBytes)
	case len(w.Suggestions) > 0:
		return fmt.Errorf("%w: suggestions: the change — its suggestions and its message, as JSON spells them — "+
			"is %d bytes, past the %d one engine write may carry (code.MaxCommitPayloadBytes): apply them in two "+
			"writes", ErrInvalid, n, MaxCommitPayloadBytes)
	}
	return fmt.Errorf("%w: message: it is %d bytes as JSON spells it, past the %d one engine write may carry "+
		"(code.MaxCommitPayloadBytes); shorten it, or leave it out to take git's own", ErrInvalid, n,
		MaxCommitPayloadBytes)
}

// validSuggestions holds an apply_suggestions' suggestions to their shape: 1
// to [MaxSuggestionOps], each finding at most once, each a finding's id, a
// path one write may name, the whole id of the blob its lines were read in,
// its lines from 1 with the last at or past the first, and a text of at most
// [MaxSuggestionBytes] of valid UTF-8 holding no NUL — their bytes, with the
// message's, being [EngineWrite.validPayload]'s.
func (w EngineWrite) validSuggestions() error {
	if len(w.Suggestions) == 0 {
		return nil
	}
	if len(w.Suggestions) > MaxSuggestionOps {
		return fmt.Errorf("%w: suggestions: %d, past the %d one write applies (code.MaxSuggestionOps)",
			ErrInvalid, len(w.Suggestions), MaxSuggestionOps)
	}
	named := map[string]int{}
	for i, s := range w.Suggestions {
		at := fmt.Sprintf("suggestions[%d]", i)
		if id, err := uuid.Parse(s.FindingID); err != nil || id.Version() != 7 || id.String() != s.FindingID {
			return fmt.Errorf("%w: %s.finding_id: %q is not a finding's id, a UUIDv7 in its canonical lowercase "+
				"spelling", ErrInvalid, at, s.FindingID)
		}
		if j, twice := named[s.FindingID]; twice {
			return fmt.Errorf("%w: %s.finding_id: finding %s is suggestions[%d]'s too: one suggestion a finding",
				ErrInvalid, at, s.FindingID, j)
		}
		named[s.FindingID] = i
		if err := validWritePath(s.Path); err != nil {
			return fmt.Errorf("%w: %s.path: %w", ErrInvalid, at, err)
		}
		switch {
		case !gitref.ValidOID(s.BlobOID) || s.BlobOID == ZeroOID:
			return fmt.Errorf("%w: %s.blob_oid: %q is not a whole object id", ErrInvalid, at, s.BlobOID)
		case s.LineStart < 1 || s.LineEnd < s.LineStart:
			return fmt.Errorf("%w: %s: lines %d to %d: a finding's lines count from 1, the last at or past the "+
				"first", ErrInvalid, at, s.LineStart, s.LineEnd)
		}
		if why := textProblem(s.Text, MaxSuggestionBytes, "suggestion"); why != "" {
			return fmt.Errorf("%w: %s.text: %s", ErrInvalid, at, why)
		}
	}
	return nil
}

// validKind holds each field to the kind that takes it.
func (w EngineWrite) validKind() error {
	type field struct {
		name string
		set  bool
	}
	want := func(fields ...field) error {
		for _, f := range fields {
			if !f.set {
				return fmt.Errorf("%w: %s: a %s names it", ErrInvalid, f.name, w.Kind)
			}
		}
		return nil
	}
	none := func(fields ...field) error {
		for _, f := range fields {
			if f.set {
				return fmt.Errorf("%w: %s: a %s takes none", ErrInvalid, f.name, w.Kind)
			}
		}
		return nil
	}
	head := field{"expected_head", w.ExpectedHead != ""}
	base := field{"base", w.Base != ""}
	commit := field{"commit", w.Commit != ""}
	message := field{"message", w.Message != ""}
	files := field{"files", len(w.Files) > 0}
	suggestions := field{"suggestions", len(w.Suggestions) > 0}
	merging := []field{{"change", w.Change != 0}, {"method", w.Method != ""},
		{"delete_source", w.DeleteSource}, {"override", w.Override != ""}, {"close_task", w.CloseTask},
		{"auto_revert", w.AutoRevert}}
	switch w.Kind {
	case WriteMergeChange:
		return w.validMerge(want(head), none(commit, files, base, message, suggestions))
	case WriteRebaseChange:
		// ONTO ITS TARGET'S TIP, FROM THE HEAD ITS CALLER READ: the commits
		// replayed keep their own messages, so a rebase carries none.
		if err := want(head); err != nil {
			return err
		}
		if err := none(append([]field{commit, files, base, message, suggestions}, merging[1:]...)...); err != nil {
			return err
		}
		if w.Change < 1 {
			return fmt.Errorf("%w: change: %d is not a change's number, which counts from 1", ErrInvalid, w.Change)
		}
		return nil
	}
	if err := none(merging...); err != nil {
		return err
	}
	switch w.Kind {
	case WriteCommitFiles:
		switch {
		case len(w.Files) > MaxFileOps:
			return fmt.Errorf("%w: files: %d operations, past the %d one write may carry (code.MaxFileOps); "+
				"make a change this wide in a coding box (run_sandbox)", ErrInvalid, len(w.Files), MaxFileOps)
		case w.Base != "" && w.ExpectedHead != "":
			return fmt.Errorf("%w: base: a commit onto a branch sits on its head (expected_head); base starts a "+
				"branch the write creates", ErrInvalid)
		}
		if err := want(files, message); err != nil {
			return err
		}
		return none(commit, suggestions)
	case WriteRevert, WriteCherryPick:
		if err := want(commit, head); err != nil {
			return err
		}
		return none(files, base, suggestions)
	case WriteCreateBranch:
		if err := want(commit); err != nil {
			return err
		}
		return none(head, files, base, message, suggestions)
	case WriteApplySuggestions:
		// ONTO THE HEAD ITS CALLER READ, always: a suggestion is placed in the
		// file at that head, so an apply that created a branch would place it
		// in nothing.
		if err := want(head, message, suggestions); err != nil {
			return err
		}
		return none(commit, files, base)
	}
	if err := want(head); err != nil {
		return err
	}
	return none(commit, files, base, message, suggestions)
}

// validMerge holds a merge_change to its own fields, once the rules every kind
// shares have passed (fields, the kind's wants and nones): its change by a
// number from 1, a method this build knows, and a person's override reason
// within [MaxOverrideReasonBytes] of valid text. A merge's message is the
// engine's own — its change's reference and title — so it carries none, nor
// files, a base or a commit; its Ref is its change's target and its
// ExpectedHead its change's head. AutoRevert and CloseTask ride an enqueue,
// and an override merges directly, so a consent beside an override is a
// write asking two things.
func (w EngineWrite) validMerge(fields ...error) error {
	for _, err := range fields {
		if err != nil {
			return err
		}
	}
	switch {
	case w.Change < 1:
		return fmt.Errorf("%w: change: %d is not a change's number, which counts from 1", ErrInvalid, w.Change)
	case !w.Method.Valid():
		return fmt.Errorf("%w: method: %q is not one of %v", ErrInvalid, w.Method, MergeMethods())
	case w.Override != "" && w.AutoRevert:
		return fmt.Errorf("%w: auto_revert: an override merges the change directly, and a consent to a revert "+
			"rides its entry in its target's queue", ErrInvalid)
	}
	if why := textProblem(w.Override, MaxOverrideReasonBytes, "merge's override reason"); why != "" {
		return fmt.Errorf("%w: override: %s", ErrInvalid, why)
	}
	return nil
}

// validFiles holds a commit_files' operations to their kinds and paths, and
// every path to one operation — their bytes, with the message's, being
// [EngineWrite.validPayload]'s.
func (w EngineWrite) validFiles() error {
	if len(w.Files) == 0 {
		return nil
	}
	named := make(map[string]int, len(w.Files))
	claim := func(i int, p string) error {
		if j, taken := named[p]; taken {
			return fmt.Errorf("%w: files[%d]: %q is named by files[%d] too: one operation a path", ErrInvalid, i, p,
				j)
		}
		named[p] = i
		return nil
	}
	for i, f := range w.Files {
		if err := f.valid(i); err != nil {
			return err
		}
		if err := claim(i, f.Path); err != nil {
			return err
		}
		if f.Op == FileOpMove {
			if err := claim(i, f.To); err != nil {
				return err
			}
		}
	}
	return nil
}

// valid holds one operation to its kind: the fields it takes and no other, and
// its paths to [ValidTreePath] and the depth one write may name
// ([validWritePath]).
func (f FileOp) valid(i int) error {
	at := fmt.Sprintf("files[%d]", i)
	if !f.Op.Valid() {
		return fmt.Errorf("%w: %s.op: %q is not one of %v", ErrInvalid, at, f.Op, FileOpKinds())
	}
	if err := validWritePath(f.Path); err != nil {
		return fmt.Errorf("%w: %s.path: %w", ErrInvalid, at, err)
	}
	type field struct {
		name string
		set  bool
	}
	fields := []field{
		{"content", f.Content != nil}, {"blob_oid", f.BlobOID != ""}, {"old", f.Old != ""},
		{"new", f.New != ""}, {"to", f.To != ""}, {"mode", f.Mode != ""},
	}
	takes := map[FileOpKind][]string{
		FileOpPut:    {"content", "blob_oid", "mode"},
		FileOpEdit:   {"old", "new"},
		FileOpDelete: nil,
		FileOpMove:   {"to"},
		FileOpMode:   {"mode"},
	}[f.Op]
	for _, fd := range fields {
		if fd.set && !slices.Contains(takes, fd.name) {
			return fmt.Errorf("%w: %s.%s: a %s takes none", ErrInvalid, at, fd.name, f.Op)
		}
	}
	switch f.Op {
	case FileOpPut:
		return f.validPut(at)
	case FileOpEdit:
		switch {
		case f.Old == "":
			return fmt.Errorf("%w: %s.old: an edit names the text it replaces", ErrInvalid, at)
		case f.Old == f.New:
			return fmt.Errorf("%w: %s.new: it is the old text again, which changes nothing", ErrInvalid, at)
		}
	case FileOpMove:
		if err := validWritePath(f.To); err != nil {
			return fmt.Errorf("%w: %s.to: %w", ErrInvalid, at, err)
		}
	case FileOpMode:
		if f.Mode != ModeFile && f.Mode != ModeExecutable {
			return fmt.Errorf("%w: %s.mode: %q is not %s or %s: a mode flips a file's executable bit, and a link "+
				"is made by a put", ErrInvalid, at, f.Mode, ModeFile, ModeExecutable)
		}
	}
	return nil
}

// validPut holds a put to exactly one source and to a mode it may make —
// and a link to a target that is its text alone.
func (f FileOp) validPut(at string) error {
	switch {
	case (f.Content == nil) == (f.BlobOID == ""):
		return fmt.Errorf("%w: %s: a put carries exactly one of content and blob_oid", ErrInvalid, at)
	case f.BlobOID != "" && (!gitref.ValidOID(f.BlobOID) || f.BlobOID == ZeroOID):
		return fmt.Errorf("%w: %s.blob_oid: %q is not a whole object id", ErrInvalid, at, f.BlobOID)
	}
	switch f.Mode {
	case "", ModeFile, ModeExecutable:
		return nil
	case ModeLink:
	default:
		return fmt.Errorf("%w: %s.mode: %q is not %s, %s or %s", ErrInvalid, at, f.Mode, ModeFile, ModeExecutable,
			ModeLink)
	}
	// A LINK TAKES ITS TARGET AS TEXT, and only as text: its target is a
	// short path, and a blob_oid would carry any blob of the repository —
	// up to code.limits.max_blob_bytes — past that bound unread, a link no
	// checkout can create (git's fsck has no check of a link's target).
	switch {
	case f.Content == nil:
		return fmt.Errorf("%w: %s.blob_oid: a link (mode %s) takes its target as content, never an existing "+
			"blob", ErrInvalid, at, ModeLink)
	case *f.Content == "":
		return fmt.Errorf("%w: %s.content: a link points somewhere: its target is empty", ErrInvalid, at)
	case len(*f.Content) > MaxTreePathBytes:
		return fmt.Errorf("%w: %s.content: a link's target is %d bytes, past the %d a path may be", ErrInvalid, at,
			len(*f.Content), MaxTreePathBytes)
	case strings.IndexByte(*f.Content, 0) >= 0:
		return fmt.Errorf("%w: %s.content: a link's target holds a NUL byte", ErrInvalid, at)
	}
	if name := path.Base(f.Path); gitControlName(name) {
		return fmt.Errorf("%w: %s.path: %q made a link is a file git reads only when it is a regular file "+
			"(git refuses a linked .gitmodules outright, and ignores a linked .gitattributes, .gitignore or "+
			".mailmap in every checkout): put it as a file", ErrInvalid, at, name)
	}
	return nil
}

// validWritePath is what a path an engine write names may be: a name
// [ValidTreePath] admits, at most [MaxTreePathDepth] components deep — refused
// by name before anything is read, as a write's other bounds are, since every
// cost of its build that grows with a path's depth is bounded by it.
func validWritePath(p string) error {
	if err := ValidTreePath(p); err != nil {
		return err
	}
	if n := strings.Count(p, "/") + 1; n > MaxTreePathDepth {
		return fmt.Errorf("%d components deep, past the %d a path one engine write names may be "+
			"(code.MaxTreePathDepth); make a change this deep in a coding box (run_sandbox)", n, MaxTreePathDepth)
	}
	return nil
}

// ValidTreePath is the EARLY, NAMED refusal of a path an engine write may make
// in a tree: what git's own check of a tree (fsck, which the build's
// `index-pack --strict` runs, the relay's receive.fsckObjects) refuses of a
// NAME, and more, so a write is refused at least what a push of the same
// objects is — before anything is read. What an object HOLDS — a .gitmodules'
// url, a .gitattributes' size — is that check's alone.
//
// At most [MaxTreePathBytes], each component at most 255 bytes of valid UTF-8;
// no empty, `.` or `..` component and no leading or trailing `/`; no control
// character (U+0000–U+001F, U+007F) and no backslash; no component that is
// `.git` or `git~1` in any spelling git's is_ntfs_dotgit reads as one — any
// case, trailing dots and spaces, and a `:` stream suffix (`.GIT::$INDEX_ALLOCATION`
// is `.git` on NTFS); and no component holding a code point HFS+ ignores
// (U+200C–U+200F, U+202A–U+202E, U+206A–U+206F, U+FEFF), by which `.g\u200cit`
// is `.git` on a Mac. STRICTER THAN GIT on purpose — a control character,
// a backslash or an HFS-ignorable code point in a NEW name is refused
// outright, where git refuses one only where it spells `.git` — because no
// tool needs one in a name it writes and each is a way to make a name read as
// another. Existing names in a tree are never judged: a write passes every
// entry it does not touch through unchanged. A path a write names is held to
// [MaxTreePathDepth] besides — a bound of what one write carries, never of a
// name ([validWritePath]).
func ValidTreePath(p string) error {
	switch {
	case p == "":
		return errors.New("a path is required")
	case len(p) > MaxTreePathBytes:
		return fmt.Errorf("%d bytes, past the %d a path may be (code.MaxTreePathBytes)", len(p), MaxTreePathBytes)
	case !utf8.ValidString(p):
		return errors.New("not valid UTF-8")
	case strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/"):
		return fmt.Errorf("%q has a leading or a trailing slash: a path is relative to the repository's root", p)
	}
	for c := range strings.SplitSeq(p, "/") {
		switch {
		case c == "" || c == "." || c == "..":
			return fmt.Errorf("%q holds an empty, \".\" or \"..\" component", p)
		case len(c) > 255:
			return fmt.Errorf("a component of %d bytes, past the 255 a file name may be", len(c))
		}
		for _, r := range c {
			switch {
			case r < 0x20 || r == 0x7f:
				return fmt.Errorf("%q holds the control character %U", p, r)
			case r == '\\':
				return fmt.Errorf("%q holds a backslash, which Windows reads as a separator", p)
			case hfsIgnorable(r):
				return fmt.Errorf("%q holds %U, which HFS+ ignores in a name", p, r)
			}
		}
		if dotGit(c) {
			return fmt.Errorf("%q has the component %q, which git reads as its own .git directory", p, c)
		}
	}
	return nil
}

// dotGit reports a component git's is_ntfs_dotgit and is_hfs_dotgit read as
// .git: `.git` or `git~1` in any ASCII case, followed by nothing but dots and
// spaces up to its end or a `:` — HFS's ignorable code points are refused
// apart ([ValidTreePath]).
func dotGit(c string) bool {
	lower := asciiLowerString(c)
	switch {
	case strings.HasPrefix(lower, ".git"):
		return dotsAndSpaces(lower[len(".git"):])
	case strings.HasPrefix(lower, "git~1"):
		return dotsAndSpaces(lower[len("git~1"):])
	}
	return false
}

// dotsAndSpaces is git's only_spaces_and_periods: s is nothing but dots and
// spaces up to its end or a `:`, which NTFS reads as nothing at all — a name
// ending so is the name before it, and a `:` starts a stream of it.
func dotsAndSpaces(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ':':
			return true
		case '.', ' ':
		default:
			return false
		}
	}
	return true
}

// hfsIgnorable reports a code point HFS+ ignores in a file name: what git's
// is_hfs_dotgit skips as it compares a name with .git.
func hfsIgnorable(r rune) bool {
	return (0x200C <= r && r <= 0x200F) || (0x202A <= r && r <= 0x202E) || (0x206A <= r && r <= 0x206F) ||
		r == 0xFEFF
}

// gitControlName reports a name git reads as its own control file —
// .gitmodules, .gitattributes, .gitignore or .mailmap — in any spelling git's
// is_ntfs_dot_generic reads as one: the name in any ASCII case, its NTFS 8.3
// short name (its first six letters and ~1 to ~4, or git's hashed fallback
// short name, and ~1 to ~9 then digits), each followed by nothing but dots and
// spaces up to its end or a `:`.
func gitControlName(name string) bool {
	for _, n := range []struct{ dotted, short string }{
		{"gitmodules", "gi7eba"}, {"gitattributes", "gi7d29"}, {"gitignore", "gi250a"}, {"mailmap", "maba30"},
	} {
		if ntfsDotGeneric(name, n.dotted, n.short) {
			return true
		}
	}
	return false
}

// ntfsDotGeneric is git's is_ntfs_dot_generic (path.c) for one control file:
// "."+dotted in any ASCII case, its first six letters and ~1..~4, or the short
// name's hashed fallback prefix and ~1..~9 and digits — each then nothing but
// dots and spaces up to the name's end or a ':'.
func ntfsDotGeneric(name, dotted, short string) bool {
	lower := asciiLowerString(name)
	if strings.HasPrefix(lower, "."+dotted) {
		return dotsAndSpaces(lower[1+len(dotted):])
	}
	if len(lower) >= 8 && lower[:6] == dotted[:6] && lower[6] == '~' && lower[7] >= '1' && lower[7] <= '4' {
		return dotsAndSpaces(lower[8:])
	}
	// The fall-back short name: up to six letters of the hashed prefix, then
	// ~ and a digit 1..9, then digits, eight characters in all — git's loop,
	// which steps past the digit after the ~ itself.
	i := 0
	for sawTilde := false; i < 8; i++ {
		if i >= len(lower) {
			return false
		}
		c := lower[i]
		switch {
		case sawTilde:
			if c < '0' || c > '9' {
				return false
			}
		case c == '~':
			i++
			if i >= len(lower) || lower[i] < '1' || lower[i] > '9' {
				return false
			}
			sawTilde = true
		case i >= 6 || c&0x80 != 0:
			return false
		case c != short[i]:
			return false
		}
	}
	return dotsAndSpaces(lower[i:])
}

// asciiLowerString is s with every ASCII upper-case letter in lower case and
// every other byte as it is: git's strncasecmp and tolower fold ASCII alone,
// so a name folded by Unicode's rules could be read as one git never would.
func asciiLowerString(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
