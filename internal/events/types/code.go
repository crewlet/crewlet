package types

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/named"
)

// WHO READ A REPOSITORY A UNIT KEEPS TO ITSELF: Gitlet's fetch audit (ADR-0088).
// A read is one that SENT CONTENTS: a git fetch whose
// answer carried a pack (a stateless fetch negotiates in ROUNDS and only the
// last is answered with the pack, so a clone is one read), or a server-side
// read (a code tool's tree, file, diff...) once answered, from the copy or its
// memo alike. An `ls-refs`, an advertisement and a fetch ended early are none.
// The serving node ledgers reads (internal/gitlet/readaudit) and publishes
// every fifteen seconds ONE record per reader, repository and way per company
// day, counting them ([CodeFetched.Reads]), off the read's path. Never for a
// company repository: its readers are everyone. Counted on the serving node
// into the `usage` domain (kind `repo_read`); unfed (internal/events): a CI
// token fetching every build would bury the feed.

func init() {
	events.Register[CodeFetched]()
	events.Register[CodeCheckFinished]()
	events.Register[ChangeMerged]()
	events.Register[ChangeOpened]()
	events.Register[ReviewSubmitted]()
	events.Register[CodeQueueEjected]()
	events.Register[CodeTargetBroken]()
	events.Register[CodeMirrorPushed]()
	events.Register[CodeImported]()
	events.Register[CodeExpunged]()
}

// CodeFetchedVia is how a unit repository's contents were read.
//
// A named string with a closed set, so a value a newer build writes is a
// value this build carries rather than a panic — [CodeFetchedVia.Valid] is
// what a reader switching on it checks.
type CodeFetchedVia string

const (
	// FetchedViaGit is a read through Gitlet's git routes: upload-pack
	// serving a fetch or a clone, or a download beside them (Kind raw or
	// archive).
	FetchedViaGit CodeFetchedVia = "git"
	// FetchedViaTool is a server-side read through a code tool — a seat's,
	// or an operator assistant's.
	FetchedViaTool CodeFetchedVia = "tool"
)

// The kinds a read through the git routes other than a fetch is filed under
// (CodeFetched.Kind beside FetchedViaGit): a file's bytes, a commit's ZIP.
const (
	FetchedRaw     = "raw"
	FetchedArchive = "archive"
)

// CodeFetchedVias is the closed set.
func CodeFetchedVias() []CodeFetchedVia { return []CodeFetchedVia{FetchedViaGit, FetchedViaTool} }

// Valid reports whether this build knows the value.
func (v CodeFetchedVia) Valid() bool { return slices.Contains(CodeFetchedVias(), v) }

// CodeReaderKind is what kind of principal read a repository: Gitlet's
// own spelling of a principal kind (gitlet.PrincipalKind), held equal to it by
// internal/gitlet's TestEveryPrincipalKindIsAReaderTheAuditRecords. Declared
// here rather than read from internal/gitlet because the usage domain that keeps
// these records is beneath the code domain in the import graph, and the record
// is where the closed set is read.
type CodeReaderKind string

const (
	// CodeReaderOperator is a credential: a person through a credential of
	// theirs, a token bound to a person's seat, or a token bound to nobody.
	CodeReaderOperator CodeReaderKind = "operator"
	// CodeReaderAgent is a seat of the chart, by its handle.
	CodeReaderAgent CodeReaderKind = "agent"
)

// CodeReaderKinds is the closed set.
func CodeReaderKinds() []CodeReaderKind { return []CodeReaderKind{CodeReaderOperator, CodeReaderAgent} }

// Valid reports whether this build knows the value.
func (k CodeReaderKind) Valid() bool { return slices.Contains(CodeReaderKinds(), k) }

// CodeFetched records the reads ONE READER made of a `visibility: unit`
// repository's contents, one way, since the serving node's last flush — git
// fetches whose answers carried a pack, server-side reads of one kind that
// answered, or downloads of a file or a commit's ZIP (Kind raw or archive) —
// within one company day, which the envelope's instant, the newest read's,
// falls in. Recorded on the node that SERVED them, and never for a
// `visibility: company` repository, whose readers nobody audits.
//
// A producer hands the node's ledger one read at a time in this same shape,
// Reads 1, and the ledger folds a reader's reads into the record it publishes.
type CodeFetched struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name as the newest of the reads named it.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// PrincipalKind is who read it: a credential or a seat.
	PrincipalKind CodeReaderKind `json:"principal_kind"`

	// Principal is the credential as every record names it (iam.ActorFor:
	// `pat:<id>`, `token:<id>`, `session:<lineage>`, an operator id) for an
	// operator, and the seat's handle for an agent.
	Principal string `json:"principal"`

	// Seat is the human seat the credential is bound to — a person's own, or
	// the one the identity directory binds a token to — or the agent's own
	// handle; empty for a token bound to nobody.
	Seat string `json:"seat,omitempty"`

	// Via is how it was read; Kind, for a server-side read, is which read (one
	// of gitlet.GitReadKinds, or merge_preview, a comparison's merge of two
	// commits, internal/gitlet/home's PreviewReadKind), for a download through
	// the git routes raw or archive, and empty for a fetch.
	Via  CodeFetchedVia `json:"via"`
	Kind string         `json:"kind,omitempty"`

	// Reads is how many reads the record stands for: one at least. A record
	// that counts none is no read, and the day's derivation skips it as it
	// skips one nobody can attribute (store.DB.UsageForDay).
	Reads int64 `json:"reads"`

	// Bytes is what the reader was sent, over every read the record stands
	// for: the whole answer that carried the pack, for a fetch, the answer's
	// encoding, for a server-side read, and the bytes sent, for a download.
	Bytes int64 `json:"bytes"`
}

// EventType is the "code_fetched" wire type.
func (CodeFetched) EventType() string { return "code_fetched" }

// Actor is who read it — the agent's handle, a credential's bound seat, or the
// credential itself where it is bound to nobody — overriding the envelope's
// source, which says nothing about a reader: a fetch's names nobody, and a
// tool read's is the seat already named here.
func (e CodeFetched) Actor() string {
	if e.Seat != "" {
		return e.Seat
	}
	return e.Principal
}

// PhraseFor names the repository and what was read of it: a fetch or a
// download by its size, a server-side read by its kind, each with how many
// times where the record stands for more than one.
func (e CodeFetched) PhraseFor(actor string) named.Line {
	repo := named.Repo(e.RepoName)
	what := named.Words("fetched ").Name(repo)
	switch {
	case e.Via == FetchedViaGit && e.Kind == FetchedRaw:
		what = named.Words("downloaded a file of ").Name(repo)
	case e.Via == FetchedViaGit && e.Kind == FetchedArchive:
		what = named.Words("downloaded ").Name(repo).Words(" as a ZIP")
	}
	if e.Via == FetchedViaTool {
		what = named.Words("read ").Name(repo)
		if e.Kind != "" {
			what = what.Words("'s " + e.Kind)
		}
	}
	if e.Reads > 1 {
		what = what.Words(" " + strconv.FormatInt(e.Reads, 10) + " times")
	}
	return leadLine(actor, what.Words(" ("+strconv.FormatInt(e.Bytes, 10)+" bytes)"))
}

// CodeCheckFinished records that one attempt of a check of a commit ENDED, as
// the attempt's row on the code log keeps the ending: published ONCE, by the
// data node whose checks writer published the record (internal/gitlet's
// ChecksWriter), on its apply — never by an applier. An ending the row did not
// keep is announced by none. A record may REPLACE the kept ending (a
// measurement replaces an infrastructure ending; an earlier ending replaces a
// later one across a failover); it is announced too, naming the replaced one in
// Replaces, as a CORRECTION a day counts once by attempt key. A failure the day
// counted (Owner) STAYS COUNTED even after a correction to a pass: a day's
// figures only grow, joined by max (internal/usage's CodeDay). The event, not
// the replicated row, is what a NODE'S DAY counts (store.DB.UsageForDay), or
// each data node would count each attempt. Persisted, `system`, unfed (internal/events).
type CodeCheckFinished struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name when the ending applied.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// OID is the commit checked, Name the check and Attempt the attempt.
	OID     string `json:"oid"`
	Name    string `json:"name"`
	Attempt int    `json:"attempt"`

	// Purpose is what the attempt was for (internal/gitlet's CheckPurpose:
	// head, candidate, tip, bisect; empty for a check a person's CI
	// reported); ChangeID a head check's change, or the change a reported
	// failure fails the head of (Owner, below), and Target the branch the
	// commit was checked for.
	Purpose  string `json:"purpose"`
	ChangeID string `json:"change_id,omitempty"`
	Target   string `json:"target"`

	// State is the ending (internal/gitlet's CheckState) and Reason why
	// (CheckReason); Placement is where it ran — container or external — and
	// empty for an attempt no runner claimed.
	State     string `json:"state"`
	Reason    string `json:"reason"`
	Placement string `json:"placement,omitempty"`

	// Required reports a check its target's protection requires;
	// DurationMs is how long its run took, 0 for none.
	Required   bool  `json:"required,omitempty"`
	DurationMs int64 `json:"duration_ms,omitempty"`

	// Replaces is the ending this one REPLACED on the attempt's row — its
	// state, as its writer read it — and empty for the attempt's first
	// ending: an event naming one is a correction, never a second ending
	// (see the type's doc).
	Replaces string `json:"replaces,omitempty"`

	// Owner is the owner of the change whose head the attempt measured, on
	// an ending that FAILS that head — failed, or errored past the home's own
	// requeue (internal/gitlet's checks_failed wake) — and empty on every
	// other: a seat by its handle, or a person by the human seat they hold,
	// OwnerKind which (internal/gitlet's PartyKind: agent, person). A head check
	// the engine ran names the change it was queued for; one a person's CI
	// reported (Placement external) names no change of its own, so its
	// failure names — in ChangeID too — the live change into Target whose
	// head is OID when it was reported, the one its checks_failed wake tells.
	Owner     string `json:"owner,omitempty"`
	OwnerKind string `json:"owner_kind,omitempty"`

	// Agent is the owner's agent id on such an ending — "" for a person, a
	// seat the company no longer has, and every ending that fails no head —
	// and the day counts the failure once by its attempt (`checks_failed`,
	// internal/store's usageCode), whatever the change did since (the type's
	// doc).
	//
	// `agent_id` is THE COLUMN THE EVENT STORE PROMOTES because these records
	// are common (every push ends each check its file defines) and only an
	// ending that fails a head names a seat. A day reads the column, so an
	// ending naming none neither moves the day's fingerprint nor has its
	// payload read. The column says what the event is ABOUT, the seat whose
	// code failed, never who acted: the actor is the node that wrote it (Source).
	Agent string `json:"agent_id,omitempty"`
}

// EventType is the "code_check_finished" wire type.
func (CodeCheckFinished) EventType() string { return "code_check_finished" }

// AgentID is the seat whose head the ending failed, "" for every other
// ending: what the event is about. The actor chain reaches it only for an
// ending published with no source, which its writer never publishes — the
// actor is the node that wrote it.
func (e CodeCheckFinished) AgentID() string { return e.Agent }

// PhraseFor names the check, its attempt, the commit's first twelve digits,
// the repository and how it ended.
func (e CodeCheckFinished) PhraseFor(actor string) named.Line {
	what := named.Words("check " + e.Name + " (attempt " + strconv.Itoa(e.Attempt) + ") of ").
		Name(named.Repo(e.RepoName)).Words("@").Name(named.CommitOf(e.RepoName, e.OID)).
		Words(" " + e.State)
	if e.Reason != "" {
		what = what.Words(" (" + e.Reason + ")")
	}
	if e.Replaces != "" {
		what = what.Words(", replacing its " + e.Replaces + " ending")
	}
	return leadLine(actor, what)
}

// ChangeMerged records that a change MERGED by a merge record (internal/gitlet's
// Merge): its target moved onto the commit its merge built, by a record whose
// decide held the change to its target's merge gate from one snapshot — or
// recorded a person's override of it (ADR-0096). Published ONCE, by the data
// node whose committer landed the record, on that record's apply — never by an
// applier, which every data node runs, and never for a retry its operation
// ledger collapsed onto the record. A change a push merged — its target pushed
// to its head, or past it — is that push's, and no merge record announces it.
// Fed, in the `system` category, one a change. It carries the change, branch,
// commit, who merged, and a person's reason for merging past the gate, which
// an audit of the branch reads.
type ChangeMerged struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name when the merge applied.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// ChangeID is the change, Number its number on the repository — web!42's
	// 42 — and Target the branch it merged into.
	ChangeID string `json:"change_id"`
	Number   int64  `json:"number"`
	Target   string `json:"target"`

	// Method is how it merged (internal/gitlet's MergeMethod: merge, squash,
	// fast_forward) and Commit the commit its target moved to.
	Method string `json:"method"`
	Commit string `json:"commit"`

	// MergedBy is who merged it — a seat by its handle, or a person by the
	// human seat they hold — and MergedByKind which (internal/gitlet's
	// PartyKind: agent, person).
	MergedBy     string `json:"merged_by"`
	MergedByKind string `json:"merged_by_kind"`

	// Override is a person's reason for merging the change past its target's
	// merge gate, "" for a merge its gate held.
	Override string `json:"override,omitempty"`
}

// EventType is the "change_merged" wire type.
func (ChangeMerged) EventType() string { return "change_merged" }

// Actor is who merged it, overriding the envelope's source — the node that
// landed the record, which merged nothing.
func (e ChangeMerged) Actor() string { return e.MergedBy }

// PhraseFor names the change by its reference (web!42), the branch it merged
// into and how, and a person's override where there was one.
func (e ChangeMerged) PhraseFor(actor string) named.Line {
	what := named.Words("merged ").Name(changeOf(e.RepoName, e.Number)).Words(" into " +
		strings.TrimPrefix(e.Target, "refs/heads/") + " (" + e.Method + ")")
	if e.Override != "" {
		what = what.Words(", overriding its merge gate: " + e.Override)
	}
	return leadLine(actor, what)
}

// ChangeOpened records that a change was OPENED (internal/gitlet's change_open,
// or a push whose options asked for one): its row written, numbered and owned
// by whoever opened it. Published ONCE, by the data node whose committer
// landed the record that opened it, on that record's apply — never by an
// applier, which every data node runs, and never for a retry its operation
// ledger collapsed onto the record, which the landing's own call announced.
// The row is replicated; the event is what the OWNER'S DAY counts
// (store.DB.UsageForDay, a seat's `changes_opened`) from the publishing node's
// own log, since a count off the rows would count each change on every data
// node. So it names the owner's agent id where the owner is a seat
// ([ChangeOpened.Agent], the `agent_id` column), resolved when published; a
// person's change is on no seat's day. Fed, one a change, as a merge is.
type ChangeOpened struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name when the change opened.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// ChangeID is the change, Number its number on the repository — web!42's
	// 42 — Target the branch it proposes into and Title its title.
	//
	// THE BRANCH IS `target_ref`, never `target`: the event store promotes
	// `target` as an A2A participant's handle (internal/store's tag keys)
	// and files the event under whichever seat answers to it.
	ChangeID string `json:"change_id"`
	Number   int64  `json:"number"`
	Target   string `json:"target_ref"`
	Title    string `json:"title"`

	// Owner is who opened it — a seat by its handle, or a person by the
	// human seat they hold — and OwnerKind which (internal/gitlet's PartyKind:
	// agent, person).
	Owner     string `json:"owner"`
	OwnerKind string `json:"owner_kind"`

	// Agent is the owner's agent id, "" for a person or a seat the
	// publishing node's chart did not hold: the seat whose day counts it.
	Agent string `json:"agent_id"`

	// ByPush is a change a push opened from its options, rather than a
	// change tool or the API.
	ByPush bool `json:"by_push"`
}

// EventType is the "change_opened" wire type.
func (ChangeOpened) EventType() string { return "change_opened" }

// Actor is who opened it, overriding the envelope's source — the node that
// landed the record, which opened nothing.
func (e ChangeOpened) Actor() string { return e.Owner }

// AgentID is the owner's agent id, "" for a person.
func (e ChangeOpened) AgentID() string { return e.Agent }

// PhraseFor names the change by its reference (web!42), the branch it
// proposes into and its title.
func (e ChangeOpened) PhraseFor(actor string) named.Line {
	what := named.Words("opened ").Name(changeOf(e.RepoName, e.Number)).Words(" into " +
		strings.TrimPrefix(e.Target, "refs/heads/"))
	if e.Title != "" {
		what = what.Words(": " + e.Title)
	}
	return leadLine(actor, what)
}

// ReviewSubmitted records that a REVIEW of a change was given (internal/gitlet's
// review): its verdict on one revision and the findings it raised. Published
// ONCE, by the data node whose change writer landed the record, on that
// record's apply — never by an applier, and never for a retry its operation
// ledger collapsed onto the record. Two seats' days count it: the REVIEWER'S
// (`reviews_given`) and the change's OWNER'S (`findings_received`), so the
// event names both agent ids, resolved when published — Agent the reviewer's
// (the `agent_id` column a day groups by), OwnerAgent the owner's. Either is ""
// for a person, who has no seat. Fed and stored: a review is at most one
// turn's result, and a person watching wants to see one land.
type ReviewSubmitted struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name when the review applied.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// ChangeID is the change, Number its number on the repository, ReviewID
	// the review and Revision the revision of the change it judged.
	ChangeID string `json:"change_id"`
	Number   int64  `json:"number"`
	ReviewID string `json:"review_id"`
	Revision int64  `json:"revision"`

	// Reviewer is who gave it — a seat by its handle, or a person by the
	// human seat they hold — ReviewerKind which, and Agent the reviewer's
	// agent id, "" for a person.
	Reviewer     string `json:"reviewer"`
	ReviewerKind string `json:"reviewer_kind"`
	Agent        string `json:"agent_id"`

	// Verdict is the review's (internal/gitlet's Verdict: approve,
	// request_changes, comment) and Findings how many findings it raised.
	Verdict  string `json:"verdict"`
	Findings int64  `json:"findings"`

	// Owner is the change's owner, OwnerKind which kind of party, and
	// OwnerAgent the owner's agent id, "" for a person: the seat whose day
	// counts the findings received.
	Owner      string `json:"owner"`
	OwnerKind  string `json:"owner_kind"`
	OwnerAgent string `json:"owner_agent_id"`
}

// EventType is the "review_submitted" wire type.
func (ReviewSubmitted) EventType() string { return "review_submitted" }

// Actor is who reviewed, overriding the envelope's source — the node that
// landed the record, which reviewed nothing.
func (e ReviewSubmitted) Actor() string { return e.Reviewer }

// AgentID is the reviewer's agent id, "" for a person.
func (e ReviewSubmitted) AgentID() string { return e.Agent }

// PhraseFor names the change by its reference, the verdict, the revision it
// judged and how many findings it raised.
func (e ReviewSubmitted) PhraseFor(actor string) named.Line {
	what := named.Words("reviewed ").Name(changeOf(e.RepoName, e.Number)).Words(" (revision " +
		strconv.FormatInt(e.Revision, 10) + "): " + strings.ReplaceAll(e.Verdict, "_", " "))
	switch e.Findings {
	case 0:
	case 1:
		what = what.Words(", 1 finding")
	default:
		what = what.Words(", " + strconv.FormatInt(e.Findings, 10) + " findings")
	}
	return leadLine(actor, what)
}

// CodeQueueEjected records that a merge queue put an entry OUT (internal/gitlet's
// Eject): the change it held will not land through this enqueue — a required
// check failed on what it was tested in, its merge conflicted, its merge gate
// no longer holds with its enqueuer as the merger, its head moved, or its
// fast-forward is none. Published ONCE an entry, by the data node whose
// committer landed the eject record, on that record's apply — a push that
// moved a queued head ends its entry head_moved in its own apply and names no
// eject — never by an applier, and never for a retry its operation ledger
// collapsed. Fed, in the `system` category: authors and enqueuer act on it and
// few entries eject. It carries the change, queue, why, failed checks or paths.
type CodeQueueEjected struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name when the eject applied.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// EntryID is the queue entry that was put out; ChangeID the change it
	// held, Number its number on the repository — web!42's 42 — and Target
	// the branch whose queue it was in.
	EntryID  string `json:"entry_id"`
	ChangeID string `json:"change_id"`
	Number   int64  `json:"number"`
	Target   string `json:"target"`

	// Reason is why (internal/gitlet's EjectReasons); Checks the required checks
	// that failed (check_failed) or that no checks file defines in a form that
	// runs (checks_undefined); Paths the conflicted paths (conflict).
	Reason string   `json:"reason"`
	Checks []string `json:"checks,omitempty"`
	Paths  []string `json:"paths,omitempty"`

	// EnqueuedBy is who enqueued it — a seat by its handle, or a person by
	// the human seat they hold — and EnqueuedByKind which (internal/gitlet's
	// PartyKind: agent, person).
	EnqueuedBy     string `json:"enqueued_by"`
	EnqueuedByKind string `json:"enqueued_by_kind"`

	// Owner is the change's owner — a seat by its handle, or a person by the
	// human seat they hold — and OwnerKind which; OwnerAgent is the owner's
	// agent id, "" for a person or a seat the running company no longer has:
	// the seat whose day counts the ejection once by its entry (`ejections`,
	// internal/store's usageCode) — the change's, never the enqueuer's, who
	// may be anybody that may merge it. Never `agent_id`, which the event
	// store promotes.
	Owner      string `json:"owner"`
	OwnerKind  string `json:"owner_kind"`
	OwnerAgent string `json:"owner_agent_id,omitempty"`
}

// EventType is the "code_queue_ejected" wire type.
func (CodeQueueEjected) EventType() string { return "code_queue_ejected" }

// PhraseFor names the change by its reference (web!42), the branch whose
// queue put it out, and why.
func (e CodeQueueEjected) PhraseFor(actor string) named.Line {
	what := named.Words("ejected ").Name(changeOf(e.RepoName, e.Number)).Words(" from " +
		strings.TrimPrefix(e.Target, "refs/heads/") + "'s merge queue (" + e.Reason + ")")
	switch {
	case len(e.Checks) > 0:
		what = what.Words(": " + strings.Join(e.Checks, ", ") + " failed")
	case len(e.Paths) > 0:
		what = what.Words(": " + strconv.Itoa(len(e.Paths)) + " paths conflicted")
	}
	return leadLine(actor, what)
}

// CodeTargetBroken records that a protected branch's tip checks found the
// FIRST landing they failed on (internal/gitlet's Broken, ADR-0098): a red tip
// bisected over the branch's landing rows down to the landing whose commit the
// named required checks failed on, the landing before it having passed them.
// Published ONCE a landing, by the data node whose committer landed the broken
// record, on that record's apply — never by an applier, which every data node
// runs. Fed, in the `system` category: a broken branch is what its team stops
// for and a bisect names few. It carries the branch, landing, change, who
// landed it and the checks broken.
type CodeTargetBroken struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name when the broken record applied.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// Target is the protected branch, Commit the landing's commit, and Kind
	// how it landed (internal/gitlet's LandingKinds: queue, direct, push).
	Target string `json:"target"`
	Commit string `json:"commit"`
	Kind   string `json:"kind"`

	// ChangeID is the change the landing landed and Number its number on the
	// repository — web!42's 42 — each empty for a person's push that landed
	// no change; LandedBy who landed it (its merger, or its pusher), by the
	// seat they hold.
	ChangeID string `json:"change_id,omitempty"`
	Number   int64  `json:"number,omitempty"`
	LandedBy string `json:"landed_by,omitempty"`

	// Checks are the required checks that failed on Commit.
	Checks []string `json:"checks"`
}

// EventType is the "code_target_broken" wire type.
func (CodeTargetBroken) EventType() string { return "code_target_broken" }

// CodeMirrorPushed records that a repository's home pushed it to its PUSH
// MIRROR (internal/gitlet/mirror, M6-D16): which refs it set at the other host
// and onto what, which it deleted there, and which protected branches it held
// back, each still waiting for a commit its required checks passed
// (code_green). Published once a push that moved something, by the home Gitlet
// node that pushed: the code log records no push to another host, so this is
// the one durable account of what the company published. Stored, not fed: a
// mirror pushes as often as once a minute and nobody acts on a push that went
// where told. A push that FAILED is no event: it is the home's state, which
// code_mirror_failing raises.
type CodeMirrorPushed struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name when it was pushed.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// URL is the repository at the other host — never a credential: the
	// mirror's is a ${VAR} reference, resolved at the dial and sent in a
	// header — and As the seat whose own identity there the push carried.
	URL string `json:"url"`
	As  string `json:"as"`

	// Updated are the refs the push set at the other host, each
	// "<ref> <oid>"; Deleted the refs it deleted there, which the repository
	// no longer has; Waiting the protected branches it left where they were,
	// none of whose commits newer than what the other host holds is green.
	// Each is the first [CodeMirrorPushedRefs] of its kind in ref order, and
	// its count beside it all of them ([CodeMirrorPushed.WithRefs]).
	Updated      []string `json:"updated,omitempty"`
	UpdatedCount int      `json:"updated_count,omitempty"`
	Deleted      []string `json:"deleted,omitempty"`
	DeletedCount int      `json:"deleted_count,omitempty"`
	Waiting      []string `json:"waiting,omitempty"`
	WaitingCount int      `json:"waiting_count,omitempty"`
}

// CodeMirrorPushedRefs is the most refs each list of a [CodeMirrorPushed]
// names: SIXTEEN, the first in ref order, beside the count of the whole list.
// A first push of a repository sets every ref it covers, up to
// gitlet.MaxRefsPerRepo (100 000), and an account naming them all — some 60
// bytes a ref — is megabytes on every data node's event log, and past some 45
// bytes a ref name over queue.MaxPayloadBytes (8 MiB): refused at its publish
// and lost, the very push the audit most needs. Sixteen keeps the worst case —
// three lists of refs at gitref.MaxRefNameBytes (255) whose every byte JSON
// escapes to six, each beside a SHA-256 id — some 77 KB, two orders of
// magnitude under that ceiling, as events.MaxDiagnosticBytes reasons for a
// free-text field.
const CodeMirrorPushedRefs = 16

// WithRefs is e with what one push moved: each of updated, deleted and
// waiting — in ref order — cut to its first [CodeMirrorPushedRefs], and
// counted whole.
func (e CodeMirrorPushed) WithRefs(updated, deleted, waiting []string) CodeMirrorPushed {
	first := func(refs []string) []string { return slices.Clone(refs[:min(len(refs), CodeMirrorPushedRefs)]) }
	e.Updated, e.UpdatedCount = first(updated), len(updated)
	e.Deleted, e.DeletedCount = first(deleted), len(deleted)
	e.Waiting, e.WaitingCount = first(waiting), len(waiting)
	return e
}

// EventType is the "code_mirror_pushed" wire type.
func (CodeMirrorPushed) EventType() string { return "code_mirror_pushed" }

// PhraseFor names the repository, the other host and what moved there, by
// the counts of the whole push.
func (e CodeMirrorPushed) PhraseFor(actor string) named.Line {
	what := named.Words("pushed ").Name(named.Repo(e.RepoName)).Words(" to its mirror " + e.URL +
		": " + strconv.Itoa(e.UpdatedCount) + " ref(s) set")
	if e.DeletedCount > 0 {
		what = what.Words(", " + strconv.Itoa(e.DeletedCount) + " deleted")
	}
	if e.WaitingCount > 0 {
		what = what.Words(", " + strconv.Itoa(e.WaitingCount) + " waiting for green")
	}
	return leadLine(actor, what)
}

// CodeImported records that a repository's IMPORT ended (internal/gitlet/mirror,
// M6-D19): its home filled it from another host — a person's import
// (code_import_repo, `crewlet gitlet import`), a declared `code.repos[].import`,
// or a pull mirror's first fill — and its end (gitlet.ImportDone) applied,
// whole or short of its source. Published ONCE an import, by the home Gitlet
// node that proposed the end and heard it applied. The row keeps only the
// LATEST import state and the push rows (`via import`) name neither source nor
// end, so this is the one durable account of where contents came from.
// An end the home did not hear applied is no event (the repository is active
// by then, so its row names how it ended), nor is a pull mirror's sync (its
// push rows `via mirror` record each). Stored, not fed: a person's import is
// already in the feed as `operator_acted`, a declared one is the document's.
type CodeImported struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name when its import ended.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// URL is the repository at the other host it was filled from — never a
	// credential: an import's is a ${VAR} reference, resolved at the dial and
	// sent in a header. Mirrored says the import was a pull mirror's first
	// fill, which its syncs then keep on that source.
	URL      string `json:"url"`
	Mirrored bool   `json:"mirrored,omitempty"`

	// Chunks is how many chunks landed — the repository's import_seq, its
	// end's Seq — 0 for an import that landed none.
	Chunks int64 `json:"chunks"`

	// Refs and Bytes are what the repository holds once its import ended, as
	// its home reads them from its copy synced at or past the end's record:
	// its refs, and the bytes of its live packs. Nothing but its import moves
	// a repository's refs or adds its packs while it imports, so they are what
	// the import landed — beside, for a repository made a pull mirror later,
	// the packs it already held, and whatever a push landed in the moment
	// between the end and that read. DefaultBranch is its default branch
	// then: the one it declares, or the source's HEAD branch where the one it
	// declares did not arrive.
	Refs          int64  `json:"refs"`
	Bytes         int64  `json:"bytes"`
	DefaultBranch string `json:"default_branch"`

	// Error is why the import ended short of its source — its end's
	// import_error, one sentence naming why (a blob past
	// code.limits.max_blob_bytes, the repository past
	// code.limits.repo_max_bytes, a source that answered over git's dumb HTTP
	// protocol) — and "" for one that landed whole. The repository keeps what
	// landed either way.
	Error string `json:"error,omitempty"`

	// Position is the end's record on the repository's code log.
	Position string `json:"position"`
}

// EventType is the "code_imported" wire type.
func (CodeImported) EventType() string { return "code_imported" }

// PhraseFor names the repository, its source and what landed, and says
// where the import ended short.
func (e CodeImported) PhraseFor(actor string) named.Line {
	what := named.Words("imported ").Name(named.Repo(e.RepoName)).Words(" from " + e.URL + ": " +
		strconv.FormatInt(e.Refs, 10) + " ref(s), " + strconv.FormatInt(e.Bytes, 10) + " bytes in " +
		strconv.FormatInt(e.Chunks, 10) + " chunk(s)")
	if e.Error != "" {
		what = what.Words(", ended short of its source")
	}
	return leadLine(actor, what)
}

// PhraseFor names the branch, the landing (its change by reference, web!42,
// or its commit) and the checks it broke.
func (e CodeTargetBroken) PhraseFor(actor string) named.Line {
	landing := named.CommitOf(e.RepoName, e.Commit)
	if e.ChangeID != "" {
		landing = changeOf(e.RepoName, e.Number)
	}
	return leadLine(actor, named.Words("found "+strings.TrimPrefix(e.Target, "refs/heads/")+" broken by ").
		Name(landing).Words(": "+strings.Join(e.Checks, ", ")+" failed"))
}

// CodeExpunged records that a repository's unreachable objects were dropped:
// an expunge's COMMIT applied (internal/gitlet/compact, ADR-0099), retiring the
// packs its begin made dropping and keeping live, as its rescue, whatever of
// them a push made reachable again. Published ONCE a commit, by the
// maintainer whose compactor proposed it and heard it applied (Request names
// a person's purge, empty is a weekly cycle). The code log keeps only the
// LATEST expunge, so this is the one durable account of every one. A commit
// whose answer was not heard applied is no event: the next pass finds it closed.
// Stored, not fed: a person's purge is already in the feed as `operator_acted`
// and the weekly cycle is housekeeping.
type CodeExpunged struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name when the commit applied.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// Request is the person's purge this expunge served — the request's
	// operation id — and empty for one the weekly schedule began. Expunge is
	// the expunge's own id, which its begin and its commit both carry.
	Request string `json:"request,omitempty"`
	Expunge string `json:"expunge"`

	// DroppedObjects and DroppedBytes are what the commit took out of the
	// repository: the dropping packs' objects and bytes, less the rescue's.
	DroppedObjects int64 `json:"dropped_objects"`
	DroppedBytes   int64 `json:"dropped_bytes"`

	// Position is the commit's record on the repository's code log.
	Position string `json:"position"`

	// StoreUntil is the latest the object store keeps the dropped bytes on a
	// healthy fleet: the retired packs' grace after the commit, so a clone
	// already reading one finishes, then the code-retention duty's next
	// pass, which removes their rows, and the collector's next, which deletes
	// the bytes. A versioned S3 bucket keeps a deleted object's old version
	// past it for as long as its own rules say.
	StoreUntil time.Time `json:"store_until"`
}

// EventType is the "code_expunged" wire type.
func (CodeExpunged) EventType() string { return "code_expunged" }

// PhraseFor names the repository, what the commit dropped, and whether a
// person's purge or the weekly schedule asked for it.
func (e CodeExpunged) PhraseFor(actor string) named.Line {
	why := "on its weekly schedule"
	if e.Request != "" {
		why = "serving a purge"
	}
	return leadLine(actor, named.Words("expunged "+strconv.FormatInt(e.DroppedObjects, 10)+
		" unreachable object(s), "+strconv.FormatInt(e.DroppedBytes, 10)+" bytes, from ").
		Name(named.Repo(e.RepoName)).Words(" "+why))
}

// changeOf names a change as its summary spells it, `<repository>!<number>`,
// which is its words alone where the payload is missing either half.
func changeOf(repository string, number int64) named.Ref {
	if ref := named.ChangeRef(repository, number); ref.Valid() {
		return ref
	}
	return named.Ref{Text: repository + "!" + strconv.FormatInt(number, 10)}
}
