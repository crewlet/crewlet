package gitlet

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// The sentinels: each a fact a caller acts on differently, errors.Is-comparable,
// and keeping that identity across the estate wire.
var (
	// ErrRepoNotFound is a repository nobody holds under that name or id.
	ErrRepoNotFound = errors.New("gitlet: no such repository")

	// ErrRefNotFound is a ref a repository does not hold live (never set, or
	// deleted) under any ref one name of a [RefQuery] was tried as, which it
	// names ([RefAnswer.Err]).
	ErrRefNotFound = errors.New("gitlet: no such ref")

	// ErrChangeNotFound is a change a repository does not hold under the
	// number a read named (web!42), which it names. Only of a repository the
	// asker may read: one it may not is [ErrNotPermitted] whatever its changes,
	// so a number never answers about a repository the grant guards.
	ErrChangeNotFound = errors.New("gitlet: no such change")

	// ErrCheckNotFound is a check attempt a repository does not hold (never
	// queued, or past the history its rows keep) under the commit, name and
	// attempt a `check` read named, which it names; only of a repository the
	// asker may read, as [ErrChangeNotFound].
	ErrCheckNotFound = errors.New("gitlet: no such check")

	// ErrPurging is a repository being removed part by part: its name
	// returns when the purge completes.
	ErrPurging = errors.New("gitlet: the repository is being purged; its name returns when the purge completes")

	// ErrNotYet is a deleted repository still inside its retention, so not
	// yet purged.
	ErrNotYet = errors.New("gitlet: the repository is within its deleted retention")

	// ErrNotDeleted is a purge asked of a repository that is not deleted.
	ErrNotDeleted = errors.New("gitlet: the repository is not deleted")

	// ErrNotConfigManaged is a configuration reconcile reaching a repository
	// its configuration does not manage.
	ErrNotConfigManaged = errors.New("gitlet: the repository is not managed by configuration")

	// ErrConfigManaged is the OTHER fact: a tool's lifecycle request reaching a
	// repository code.repos declares, which takes no tool's write
	// ([LifecycleConfigManaged] in error form). Never [ErrNotConfigManaged] read
	// backwards: confusing the two would tell an operator to rename the
	// declaration that is the reason for the refusal.
	ErrConfigManaged = errors.New("gitlet: a tool reaching a repository its configuration manages")

	// ErrNoActivation is a configuration write that does not name the
	// activation it came from.
	ErrNoActivation = errors.New("gitlet: a repository's settings must name the instant their configuration was activated")

	// ErrSuperseded is a configuration reconcile whose activation a LATER one
	// has fenced off: every node would refuse its decisions, so it makes none
	// and leaves the outcome to the later activation.
	ErrSuperseded = errors.New("gitlet: a later configuration activation owns the declared repositories")

	// ErrInvalid is a request or a declaration this domain refuses by field,
	// wrapped with the field and the reason.
	ErrInvalid = errors.New("gitlet: refused")

	// ErrInvalidProposal is a push or compaction proposal this domain cannot
	// decide, wrapped with the field and the reason ([Proposal.Validate],
	// [CompactionProposal.Validate]): also a push id naming a push to another
	// repository, or a compaction whose outputs hold more objects than the
	// packs it replaces. At a committer the node that formed the proposal is
	// at fault, never the push: the receiver already held the client's
	// commands to [Proposal.ValidateCommands], and only the engine builds a
	// compaction. From ValidateCommands it is the CLIENT'S push, past what one
	// push may carry, refused in the client's words.
	ErrInvalidProposal = errors.New("gitlet: not a proposal this domain can decide")

	// ErrInvalidLayer is a malformed published layer row ([LayerRef.Check]),
	// wrapped with the field and the reason, refused before any byte moves by
	// the committer deciding a publication (with [ErrInvalidProposal] beside
	// it) and by a Gitlet node about to install one.
	ErrInvalidLayer = errors.New("gitlet: a published layer is malformed")

	// ErrNotServing is a node's code half that RAN NOTHING: a committer that
	// decided and appended nothing (stopping, its node in a maintenance mode
	// ([Served]), or the code log refusing this NODE: an eviction, an
	// undecodable record, a floor it is below), or a half holding no reader.
	// The estate router asks another data node, which can commit the same push
	// under the same push id (collapsing a repeat) or answer the same read.
	ErrNotServing = errors.New("gitlet: this node is not serving Gitlet")

	// ErrRecordCap is a decision past a record's byte or row budget. The
	// committer's batching never forms one; the check keeps the invariant
	// local to the record.
	ErrRecordCap = errors.New("gitlet: the decision exceeds a record's budget")

	// ErrBadCursor is a cursor this reader did not issue.
	ErrBadCursor = errors.New("gitlet: not a cursor this read issued")

	// ErrBadLevel is a freshness this read does not serve.
	ErrBadLevel = errors.New("gitlet: not a freshness this read serves")

	// ErrSnapshotMoved is a later snapshot page that found the repository's
	// history moved under it (a tombstone a delta needed swept, or the log
	// reanchored or recreated). NOT A FAULT: the consumer rereads from the
	// first page, which decides full or delta afresh.
	ErrSnapshotMoved = errors.New("gitlet: the repository's history moved under this snapshot; read it again from the first page")

	// ErrNotPermitted is a question or a write the asking principal's grant on
	// the repository does not cover ([Grant.ReadRefusal] names why): a refusal
	// OF THE CALLER, never of the repository or the node, so every node
	// judging the same grant from the same chart refuses it too.
	ErrNotPermitted = errors.New("gitlet: the caller's grant does not cover this")

	// ErrAboveCeiling is a read the asking seat's own grant covers and the
	// grant of the seat it is made FOR does not ([ReadQuery.CeilingSeat]: the
	// colleague whose ask woke the turn), wrapped with that seat's
	// [Grant.ReadRefusal]: a refusal of the asker ACTING FOR THAT SEAT, alike
	// on every node judging the two grants from the same chart.
	//
	// NOT [ErrNotPermitted]: the same seat may read the repository in a turn
	// nobody woke, so that would be false about it, and its remedy (asking
	// for more access) would change nothing.
	ErrAboveCeiling = errors.New("gitlet: the seat this read is made for may not read the repository")

	// ErrBadBundle is a pack bundle whose bytes are not the pack, index and
	// reverse index its row describes (a section's header, a count, or the
	// shared pack checksum disagrees). Returned by [CheckBundle] on the
	// receiving node before it proposes a push and on an installing cache
	// before any rename; never seen by the applier, since such a push is
	// never published.
	ErrBadBundle = errors.New("gitlet: a pack bundle is not the one its row describes")

	// ErrBadLayer is a published layer whose files are not what its row
	// describes, or not git's formats as this package reads them
	// ([CheckLayer], [LayLayer]), wrapped naming the field (a trailer, a
	// covered pack, the bitmap's multi-pack-index, the commit-graph's bases,
	// a section's size). DEFINITE, never retried: [FetchLayerPart] checks the
	// store's hash, so the fault is the publisher's; the node serves from the
	// chain below and the next publication replaces the layer. A failed file
	// read is never this, and the applier never sees it: such a layer is
	// installed nowhere and named by no record its publisher proposes.
	ErrBadLayer = errors.New("gitlet: a published layer's files are not what its row says")

	// ErrObjectNotFound is an object id a read by oid ([GitQuery]) named,
	// whole or as an abbreviation, that the repository does not hold, naming
	// the id. Concluded only at the log's head: the node looks again after one
	// linearizable sync, since the asker may know of a push it had not synced.
	ErrObjectNotFound = errors.New("gitlet: no such object in the repository")

	// ErrNotATree is a directory listing ([GitTree]) whose path names a file,
	// or nothing, in the commit or tree asked of, wrapped naming both.
	ErrNotATree = errors.New("gitlet: the path names no directory")

	// ErrNotABlob is a file read ([GitBlob]) whose path names nothing in the
	// commit asked of (a submodule too, whose commit is another repository's),
	// wrapped naming both. A directory's path is not this: the read answers
	// its listing.
	ErrNotABlob = errors.New("gitlet: the path names no file")

	// ErrSliceTooLarge is a read by oid too large to answer: one item (a patch
	// line) longer than a page, or git work past the read's deadline
	// (gitproc.ReadDeadline, or the walk's bound by the repository's objects),
	// which any Gitlet node would take as long over. Wrapped naming the
	// narrowing that would answer it, and never cut: a page ending inside an
	// item would be read as the whole of it.
	ErrSliceTooLarge = errors.New("gitlet: the read is too large to answer whole; narrow it")
)

// ErrGitUnready is a read by oid ([GitQuery]) or an engine write
// ([EngineWrite]) THIS Gitlet node could not answer inside its budget, for a
// reason about the node, not the repository's content: a walk moves on to the
// next node without holding it against this one, which may be right a moment
// later. Every other refusal (ErrNotPermitted, ErrObjectNotFound, …) is about
// the request or the repository and is the answer wherever asked.
//
// ITS REASON CROSSES THE WIRE: whether the node a walk lets fill a copy
// ([GitQuery.Home], [EngineWrite.Home]) stays the one depends on it
// ([UnreadyReason.PassesFill]).
type ErrGitUnready struct {
	// Reason is which of [UnreadyReasons] holds.
	Reason UnreadyReason
	// Detail is the sentence that names this node and what it is doing.
	Detail string
}

func (e *ErrGitUnready) Error() string {
	return fmt.Sprintf("gitlet: this Gitlet node cannot answer now (%s): %s", e.Reason, e.Detail)
}

// UnreadyReason is why a Gitlet node answered a read [ErrGitUnready]: a closed
// set ([UnreadyReasons]), each a fact about the node that answered.
type UnreadyReason string

const (
	// UnreadySyncing means the node is BRINGING ITS COPY UP and another node
	// answers meanwhile: the copy missed the read's budget or due instant (a
	// cold download continues under the cache's lifetime), git reported
	// damage the cache is repairing, or a sync failed on what EVERY node's
	// fill reads (the code log, the object store, the rows, or a repair the
	// cache refuses to repeat), which another fill would only meet again but
	// a node already holding a copy may answer around.
	UnreadySyncing UnreadyReason = "syncing"
	// UnreadyShare means the node's git capacity for seats' reads is spent:
	// its seat share ([GitShare]) held past [GitShareWait], its read lane busy
	// past gitproc.SlotWait, or waits or git running past [AnswerDue]; or, for
	// a peer's engine write, its writes' lane full (internal/estate's
	// gitWritesHeld). The node holds its copy, synced, and is only busy.
	UnreadyShare UnreadyReason = "share"
	// UnreadyNoChart means the read is a seat's and the node holds no chart to
	// judge the grant by (no epoch installed yet). Not "this seat may not
	// read it", which a node holding a chart answers.
	UnreadyNoChart UnreadyReason = "no_chart"
	// UnreadyBehindConfig means the read is a seat's and the node is confirmed
	// behind the company's configuration (shed or stuck): it judges no grant
	// from a replaced chart, whose successor may have revoked it.
	UnreadyBehindConfig UnreadyReason = "behind_config"
	// UnreadyNotHeld means the node holds no copy and is not the walk's
	// effective home ([GitQuery.Home]), the one node a walk lets fill a copy,
	// so it starts nothing: otherwise every node the walk reached would
	// download the repository for a read one node answers.
	UnreadyNotHeld UnreadyReason = "not_held"
	// UnreadyNodeFailed means the node failed the read FOR A REASON OF ITS OWN
	// and nothing is bringing a copy up: its sync failed on nothing the log,
	// the object store or the rows said (its disk, its cache stopping), or a
	// short check of what the read names ran its whole bound, which no
	// healthy node takes over a few ids. Another node's fill may serve the read.
	UnreadyNodeFailed UnreadyReason = "node_failed"
	// UnreadyIndexBuilding means a search of a `search: indexed` repository's
	// default branch ([GitSearch]) reached the walk's home, the first of its
	// [IndexHolders], whose index does not yet answer for the tip. The walk
	// asks the second holder, which uses its index where it can and git grep
	// where not. Never answered by the second holder or any other node, which
	// all grep at once, so a walk always ends at an answer.
	UnreadyIndexBuilding UnreadyReason = "index_building"
)

// UnreadyReasons is the closed set.
//
// A REASON IS AN ESTATE REQUEST'S, NEVER A RECORD'S: an older asker reads an
// unknown one as unready and passes no fill for it ([UnreadyReason.PassesFill]);
// no build carrying the domain lacks index_building ([RecordVersion]).
func UnreadyReasons() []UnreadyReason {
	return []UnreadyReason{UnreadySyncing, UnreadyShare, UnreadyNoChart, UnreadyBehindConfig, UnreadyNotHeld,
		UnreadyNodeFailed, UnreadyIndexBuilding}
}

// Valid reports a reason this build knows.
func (r UnreadyReason) Valid() bool { return slices.Contains(UnreadyReasons(), r) }

// PassesFill reports whether a walk that met r at its filler ([GitQuery.Home],
// ADR-0089) lets the NEXT node fill a copy instead. True where no serving copy
// is coming there: behind_config (judges no seat until its apply succeeds),
// node_failed, and not_held (never the filler, so it moves nothing). False
// where the copy is coming: syncing (a second fill would double the download)
// and no_chart (seconds from its first epoch). False for share too: the share
// is the NODE'S, spent by every repository it serves, so passing the fill
// would answer a busy fleet with downloads that evict warm copies and add no
// capacity; a repository's seat reads are served by the nodes holding a copy,
// a ceiling ADR-0089 states. AN UNKNOWN REASON (a newer build's) PASSES
// NOTHING: an older asker never starts a second download for it.
func (r UnreadyReason) PassesFill() bool {
	switch r {
	case UnreadyBehindConfig, UnreadyNodeFailed, UnreadyNotHeld:
		return true
	}
	return false
}

// TooWideError is a push whose entry can reach Bytes in its record, past the
// Room one entry may take ([EntryRoom]); [ErrInvalidProposal] by identity.
// Typed so the receiver tells the client both sizes and the client pushes in
// parts. From [Proposal.ValidateCommands] it is the client's push; from
// [Proposal.Validate] at a committer, the receiver's bug.
type TooWideError struct {
	// Bytes is what the push's entry can reach; Room what one entry may.
	Bytes, Room int
}

func (e *TooWideError) Error() string {
	return fmt.Sprintf("%v: commands: the push's entry can reach %d bytes in its record, past the %d one "+
		"entry may take (MaxRecordBytes less a record's own overhead) — a push this wide is pushed in parts",
		ErrInvalidProposal, e.Bytes, e.Room)
}

// Unwrap is ErrInvalidProposal.
func (e *TooWideError) Unwrap() error { return ErrInvalidProposal }

// FieldRefused is a request this domain refuses by one of its fields ([ErrInvalid],
// [ErrInvalidProposal]): Field names the field as the request spells it (`findings[0].lines`,
// `owner`, `limit`), so a surface answering a person can say which value was refused without
// reading the sentence, which is written for whoever built the request (ADR-0060). Err is the
// refusal itself, the sentinel and its sentence, which errors.Is and the message read as before.
type FieldRefused struct {
	Field string
	Err   error
}

func (e *FieldRefused) Error() string { return e.Err.Error() }

func (e *FieldRefused) Unwrap() error { return e.Err }

// RefusedField is the field refused, as a surface outside the domain asks it of an error's chain.
func (e *FieldRefused) RefusedField() string { return e.Field }

// refusedf is fmt.Errorf for a refusal by one field, whose format leads "%w: <field>: " with the
// field written in the format or as its first argument after the sentinel ("%w: %s: "): the error
// says exactly what the format says, and carries the field as a value ([FieldRefused]). The field
// is read from the CALL, never from text the error carries, so no sentence becomes a value.
func refusedf(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	head, _, ok := strings.Cut(strings.TrimPrefix(format, "%w: "), ": ")
	switch {
	case !ok || !strings.HasPrefix(format, "%w: "):
		return err
	case head == "%s" && len(args) > 1:
		head = fmt.Sprint(args[1])
	case strings.Contains(head, "%"):
		return err
	}
	return &FieldRefused{Field: head, Err: err}
}
