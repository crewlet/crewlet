package code

import (
	"errors"
	"fmt"
	"slices"
)

// The sentinels. Each is a fact a caller acts on differently, which is why
// they are separate; each is errors.Is-comparable, and each keeps that
// identity across the estate wire once a caller on another node reads it.
var (
	// ErrRepoNotFound is a repository nobody holds under that name or id.
	ErrRepoNotFound = errors.New("code: no such repository")

	// ErrRefNotFound is a ref a repository does not hold live — never set,
	// or deleted — under any ref one name of a [RefQuery] was tried as,
	// which it names ([RefAnswer.Err]).
	ErrRefNotFound = errors.New("code: no such ref")

	// ErrPurging is a repository being removed part by part: its name
	// returns when the purge completes.
	ErrPurging = errors.New("code: the repository is being purged; its name returns when the purge completes")

	// ErrNotYet is a deleted repository still inside its retention, so not
	// yet purged.
	ErrNotYet = errors.New("code: the repository is within its deleted retention")

	// ErrNotDeleted is a purge asked of a repository that is not deleted.
	ErrNotDeleted = errors.New("code: the repository is not deleted")

	// ErrNotConfigManaged is a configuration reconcile reaching a repository
	// its configuration does not manage.
	ErrNotConfigManaged = errors.New("code: the repository is not managed by configuration")

	// ErrConfigManaged is the OTHER fact: a tool's lifecycle request reaching
	// a repository its configuration manages (code.repos declares it), which
	// takes no tool's write — its lifecycle refusal config_managed
	// ([LifecycleConfigManaged]) in error form. Its own sentinel, never
	// [ErrNotConfigManaged] read backwards: the reconcile's sentinel names the
	// opposite case, and a caller reading one for the other would tell an
	// operator to rename a declaration that is the reason for the refusal.
	ErrConfigManaged = errors.New("code: a tool reaching a repository its configuration manages")

	// ErrNoActivation is a configuration write that does not name the
	// activation it came from.
	ErrNoActivation = errors.New("code: a repository's settings must name the instant their configuration was activated")

	// ErrSuperseded is a configuration reconcile whose activation a LATER
	// one has already fenced off: its decisions would be refused on every
	// node, so it makes none, and what it reports is the later activation's
	// to decide.
	ErrSuperseded = errors.New("code: a later configuration activation owns the declared repositories")

	// ErrInvalid is a request or a declaration this domain refuses by field,
	// wrapped with the field and the reason.
	ErrInvalid = errors.New("code: refused")

	// ErrInvalidProposal is a proposal this domain cannot decide — a push's
	// or a compaction's — wrapped with the field and the reason
	// ([Proposal.Validate], [CompactionProposal.Validate]), or a push whose
	// push id names a push to another repository, or a compaction whose
	// outputs hold more objects than the packs it replaces. At a committer it
	// is never a refusal of the push or the compaction — the node that formed
	// the proposal is the one at fault: the receiver held the client's
	// commands (every one receive-pack acts on, read as it reads them) to
	// [Proposal.ValidateCommands] before any work, and a compaction is built
	// by the engine alone. From ValidateCommands itself it is the CLIENT'S
	// push, past what one push may carry, which the receiver refuses in the
	// client's words.
	ErrInvalidProposal = errors.New("code: not a proposal this domain can decide")

	// ErrInvalidLayer is a published layer's row that is not one
	// ([LayerRef.Check]), wrapped with the field and the reason: what every
	// holder of a layer row refuses before any byte moves — the committer
	// deciding a publication, where it is the proposal's fault too
	// ([ErrInvalidProposal] beside it), and a git node about to install one.
	ErrInvalidLayer = errors.New("code: a published layer is malformed")

	// ErrNotServing is a node's code half that RAN NOTHING: a committer that
	// decided nothing for this proposal and appended nothing — it is
	// stopping, its node publishes nothing (a maintenance mode, [Served]),
	// or the code log refused this NODE before anything was appended (an
	// eviction, a record it cannot decode, a floor it is below) — or a half
	// holding no reader to answer from. Another data node can commit the
	// same push under the same push id, which collapses a repeat, and can
	// answer the same read; the estate router asks it.
	ErrNotServing = errors.New("code: this node is not serving the code host")

	// ErrRecordCap is a decision past a record's byte or row budget — which
	// the committer's batching keeps from ever forming; the check makes the
	// invariant local to the record.
	ErrRecordCap = errors.New("code: the decision exceeds a record's budget")

	// ErrBadCursor is a cursor this reader did not issue.
	ErrBadCursor = errors.New("code: not a cursor this read issued")

	// ErrBadLevel is a freshness this read does not serve.
	ErrBadLevel = errors.New("code: not a freshness this read serves")

	// ErrSnapshotMoved is a later page of a snapshot that found the
	// repository's history moved under it — a tombstone a delta needed was
	// swept, or the log was reanchored or recreated. NOT A FAULT: the
	// consumer reads the snapshot again from its first page, which decides
	// full or delta afresh.
	ErrSnapshotMoved = errors.New("code: the repository's history moved under this snapshot; read it again from the first page")

	// ErrNotPermitted is a question or a write the asking principal's grant
	// on the repository does not cover ([Grant.ReadRefusal] names why) — a
	// refusal OF THE CALLER, never of the repository or the node: another
	// node judging the same grant from the same chart refuses it too.
	ErrNotPermitted = errors.New("code: the caller's grant does not cover this")

	// ErrAboveCeiling is a read the asking seat's own grant covers and the
	// grant of the seat it is made FOR does not ([ReadQuery.CeilingSeat]:
	// the colleague whose ask woke the turn it is made in), wrapped with that
	// seat's [Grant.ReadRefusal] — a refusal of the asker ACTING FOR THAT
	// SEAT, which every node judging the two grants from the same chart makes
	// alike, and never of the repository or the node.
	//
	// NOT [ErrNotPermitted], which is a fact about the asker's own grant: the
	// same seat reads the repository in a turn nobody woke, so telling it
	// that it may not read would be telling it something false about itself
	// — and its remedy, asking for more access, would change nothing.
	ErrAboveCeiling = errors.New("code: the seat this read is made for may not read the repository")

	// ErrBadBundle is a pack bundle whose bytes are not the pack, index and
	// reverse index its row describes — a section's header, a count, or the
	// pack checksum the three must share disagrees. Returned by
	// [CheckBundle] on the receiving node before it proposes a push, and on
	// an installing cache before it renames anything into place; never a
	// value the applier sees, because a push whose bundle failed it is never
	// published.
	ErrBadBundle = errors.New("code: a pack bundle is not the one its row describes")

	// ErrBadLayer is a published layer whose files are not the layer its
	// row describes, or not git's formats as this package reads them
	// ([CheckLayer], [LayLayer]): a trailer, a pack it covers, the
	// multi-pack-index its bitmap belongs to, the bases its commit-graph
	// layer names, a section's size — wrapped naming the field. A DEFINITE
	// answer about the bytes, never retried: a git node reads a part
	// through the store's own hash check ([FetchLayerPart]), so the bytes
	// are the ones the publisher stored, and what differs is the
	// publisher's — the node serves from the chain below the layer, and the
	// maintainer's next publication replaces it. A read of a file that
	// failed is never this, nor a value the applier sees: a layer that
	// fails it is installed nowhere and named by no record its publisher
	// proposes.
	ErrBadLayer = errors.New("code: a published layer's files are not what its row says")

	// ErrObjectNotFound is an object id a read by oid ([GitQuery]) named —
	// whole, or an abbreviation no object begins with — that the repository
	// does not hold. Concluded only at the log's head: the git node that
	// answers looks again after one linearizable sync, since the asker may
	// have learned of a push this node had not synced yet, and only then
	// answers this, naming the id.
	ErrObjectNotFound = errors.New("code: no such object in the repository")

	// ErrNotATree is a directory listing ([GitTree]) whose path names a file,
	// or nothing at all, in the commit or tree it was asked of — wrapped
	// naming the path and that object.
	ErrNotATree = errors.New("code: the path names no directory")

	// ErrNotABlob is a file read ([GitBlob]) whose path names nothing in the
	// commit it was asked of — a submodule included, whose commit is another
	// repository's object — wrapped naming the path and that object. A path
	// naming a directory is not this: the read answers the directory's
	// listing.
	ErrNotABlob = errors.New("code: the path names no file")

	// ErrSliceTooLarge is a read by oid too large to answer: one item of its
	// answer — a line of a patch — longer than a page, or git work past the
	// read's deadline (gitproc.ReadDeadline, or the walk's bound by the
	// repository's objects), which another git node would take as long
	// over. Wrapped naming the narrowing that would answer it — a line
	// range, the paths a diff names, a narrower range — and never cut: a
	// page that ended inside an item would be read as the whole of it.
	ErrSliceTooLarge = errors.New("code: the read is too large to answer whole; narrow it")
)

// ErrGitUnready is a read by oid ([GitQuery]) that THIS git node could not
// answer inside its budget, for a reason about the node rather than about the
// repository's content — so another git node may answer it, and a walk asking
// git nodes in turn moves on from this one without holding it against the
// node: a node that is syncing, at its share, holding no chart or holding no
// copy now is the right node a moment later. Every other refusal of a read is about the
// request or the repository (ErrNotPermitted, ErrObjectNotFound, …) and is the
// answer wherever it is asked.
//
// ITS REASON CROSSES THE WIRE, because the walk acts on it: whether the one
// node a walk lets fill a copy ([GitQuery.Home]) is still the one depends on
// why that node did not answer ([UnreadyReason.PassesFill]).
type ErrGitUnready struct {
	// Reason is which of [UnreadyReasons] holds.
	Reason UnreadyReason
	// Detail is the sentence that names this node and what it is doing.
	Detail string
}

func (e *ErrGitUnready) Error() string {
	return fmt.Sprintf("code: this git node cannot answer the read now (%s): %s", e.Reason, e.Detail)
}

// UnreadyReason is why a git node answered a read [ErrGitUnready]: a closed
// set ([UnreadyReasons]), each a fact about the node that answered.
type UnreadyReason string

const (
	// UnreadySyncing: the node is BRINGING ITS COPY UP and another node
	// answers meanwhile — the copy could not be brought to what the read
	// needs inside its budget, or before the read's answer fell due (a cold
	// copy downloading the repository, which goes on under the cache's
	// lifetime after the read stops waiting), a git process over the copy
	// reported damage the cache is repairing, or a sync failed on what EVERY
	// node's fill of the repository reads — the code log, the object store,
	// the repository's rows, or a repair the cache refuses to repeat because
	// a whole read of them did not end the damage — which another node's
	// fill would only meet again, and which a node already holding a copy
	// may still answer around.
	UnreadySyncing UnreadyReason = "syncing"
	// UnreadyShare: the node's git capacity for seats' reads is spent — its
	// seat git-work share ([GitShare]) held past [GitShareWait], its read
	// lane busy past gitproc.SlotWait, or the read's waits for either, or its
	// git, running past the instant its answer fell due ([AnswerDue]). The
	// node holds its copy, synced, and is only busy.
	UnreadyShare UnreadyReason = "share"
	// UnreadyNoChart: the read is a seat's, and the node holds no chart to
	// judge a seat's grant by (no epoch installed yet: at boot, or before a
	// first apply). Not "this seat may not read it", which a node holding a
	// chart answers.
	UnreadyNoChart UnreadyReason = "no_chart"
	// UnreadyBehindConfig: the read is a seat's, and the node is confirmed
	// behind the company's configuration (shed or stuck): it judges no seat's
	// grant from the chart the fleet replaced, which may be the revision that
	// revoked it.
	UnreadyBehindConfig UnreadyReason = "behind_config"
	// UnreadyNotHeld: the node holds no copy of the repository and the read
	// is not its walk's effective home's ([GitQuery.Home]), the one node a
	// walk lets fill a copy — so it starts nothing and the walk asks the
	// next node, rather than every node the walk reaches downloading the
	// repository for a read one node answers.
	UnreadyNotHeld UnreadyReason = "not_held"
	// UnreadyNodeFailed: the node failed the read FOR A REASON OF ITS OWN,
	// and nothing here is bringing a copy up for it — its sync of the
	// repository failed on nothing the code log, the object store or the
	// rows said (its disk refusing a write, its cache stopping), or one of
	// the short checks of what the read names ran its whole bound, which no
	// healthy node takes over a few ids. Another git node's fill may serve
	// the read.
	UnreadyNodeFailed UnreadyReason = "node_failed"
)

// UnreadyReasons is the closed set.
func UnreadyReasons() []UnreadyReason {
	return []UnreadyReason{UnreadySyncing, UnreadyShare, UnreadyNoChart, UnreadyBehindConfig, UnreadyNotHeld,
		UnreadyNodeFailed}
}

// Valid reports a reason this build knows.
func (r UnreadyReason) Valid() bool { return slices.Contains(UnreadyReasons(), r) }

// PassesFill reports whether a walk that met r at the node it let fill a copy
// ([GitQuery.Home], ADR-0059) lets the NEXT node of its order fill one
// instead. True where no copy that serves the read is coming on that node and
// another node's fill may serve it: behind_config, which judges no seat until
// the node's apply succeeds — however long a stuck node takes — and
// node_failed, a failure of the node's own; and not_held, a node that fills
// nothing (never the walk's filler, which is stamped home, so it moves
// nothing). False where the node is bringing the copy up, or will in a moment:
// syncing — filling, repairing, or failing on what any node's fill would meet,
// so a second fill would only double the download — and no_chart, a node
// between its boot and its first epoch, seconds from judging the read itself.
//
// AND FALSE FOR share, a node that holds its copy and is only busy: the share
// is the NODE'S, spent by every repository it serves, so a home at its share
// says nothing about the repository the walk asks for — passing the fill for
// it would answer a fleet busy everywhere with a download of every repository
// read on each busy node, each evicting a warm one to make the room, and add
// no capacity anywhere. So one repository's seat reads are served by the git
// nodes holding a copy of it: its home, and any node a person's fetch, a push
// or an earlier fill has warmed — a ceiling ADR-0059 states.
//
// AN UNKNOWN REASON PASSES NOTHING — a newer build's, read by this one — so
// the walk keeps its filler where it was, as every walk did before the
// reason crossed the wire: an older asker never starts a second download for
// a reason it cannot read.
func (r UnreadyReason) PassesFill() bool {
	switch r {
	case UnreadyBehindConfig, UnreadyNodeFailed, UnreadyNotHeld:
		return true
	}
	return false
}

// TooWideError is a push whose entry can reach Bytes in its record, past the
// Room one push's entry may take ([EntryRoom]) — [ErrInvalidProposal] by
// identity. Typed, so the receiver tells the client what the push takes and
// what one push may, and the client pushes its refs in parts: from
// [Proposal.ValidateCommands] it is the client's push, and from
// [Proposal.Validate] at a committer the receiver's bug.
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
