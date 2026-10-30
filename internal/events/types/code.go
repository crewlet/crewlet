package types

import (
	"slices"
	"strconv"

	"github.com/crewlet/crewlet/internal/events"
)

// WHO READ A REPOSITORY A UNIT KEEPS TO ITSELF: the code host's fetch audit.
//
// # Why a read of code is a record
//
// A `visibility: unit` repository is code the company has said not every seat
// may read. Every person and every Tier A token reads it anyway (ADR-0058), and
// so does every seat the chart relates to its owner, so "who has read it, and
// how much" is the question somebody answerable for that code asks — and before
// this record nothing in the engine kept the answer: a fetch was a log line
// on the node that served it, and a server-side read nothing at all.
//
// # A read is one that SENT CONTENTS
//
// A git fetch is a read once its request ended well and its answer carried a
// pack. A fetch over a stateless transport negotiates in ROUNDS, each a request
// of its own whose answer is acknowledgments and no object, and only the round
// that ends the negotiation is answered with the pack — so a clone or a fetch
// is one read however many rounds it took, and a round that sent nothing is
// none. An `ls-refs`, an advertisement and a fetch the node ended before its
// answer was whole are no read: they sent no contents, or not all of them. A
// server-side read — a code tool's tree, file, diff, comparison or landed-in
// answer — is a read once it answered, from the repository's copy or from its
// memo alike, since the memo is the copy's and answers every asker.
//
// # One record per reader per flush
//
// The node that served the reads keeps them in a ledger (internal/code/readaudit)
// and publishes, every fifteen seconds, ONE record for each reader's reads of
// one repository by one way — of one kind, for a server-side read — in one
// company day, counting them ([CodeFetched.Reads]) and summing what they were
// sent. So a CI token fetching a repository every few seconds is a record a
// flush rather than a record a fetch, and no read's answer waits for the
// broker to take its record: the record is published off the read's path.
//
// # Never for a company repository
//
// Its readers are every seat of the company by declaration, so a list of them
// would answer nothing anybody asked, and a busy company's fetches would fill
// the event log with it.
//
// # Where it is counted
//
// On the node that served the read, as every event is (the event store's own
// rule), and from there into the replicated `usage` domain as one record per
// (node, day, repository) naming each reader (internal/usage, kind
// `repo_read`), which the code host's readers route and `code_repo_readers`
// read summed across the fleet. It is kept out of the activity feed
// (internal/events' unfed list): a CI token fetching on every build would bury
// the feed, and the readers' own answer is where the record is read.

func init() {
	events.Register[CodeFetched]()
	events.Register[CodeCheckFinished]()
}

// CodeFetchedVia is how a unit repository's contents were read.
//
// A named string with a closed set, so a value a newer build writes is a
// value this build carries rather than a panic — [CodeFetchedVia.Valid] is
// what a reader switching on it checks.
type CodeFetchedVia string

const (
	// FetchedViaGit is upload-pack serving a fetch or a clone through the
	// code host's git routes.
	FetchedViaGit CodeFetchedVia = "git"
	// FetchedViaTool is a server-side read through a code tool — a seat's,
	// or an operator assistant's.
	FetchedViaTool CodeFetchedVia = "tool"
)

// CodeFetchedVias is the closed set.
func CodeFetchedVias() []CodeFetchedVia { return []CodeFetchedVia{FetchedViaGit, FetchedViaTool} }

// Valid reports whether this build knows the value.
func (v CodeFetchedVia) Valid() bool { return slices.Contains(CodeFetchedVias(), v) }

// CodeReaderKind is what kind of principal read a repository: the code host's
// own spelling of a principal kind (code.PrincipalKind), held equal to it by
// internal/code's TestEveryPrincipalKindIsAReaderTheAuditRecords. Declared
// here rather than read from internal/code because the usage domain that keeps
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
// fetches whose answers carried a pack, or server-side reads of one kind that
// answered — within one company day, which the envelope's instant, the newest
// read's, falls in. Recorded on the node that SERVED them, and never for a
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

	// Via is how it was read; Kind, for a server-side read, is which read
	// (code.GitReadKind: tree, blob, diff, compare, landed_in, log and blame,
	// and a change's revision_facts, reanchor and interdiff; or merge_preview,
	// a comparison's merge of two commits, internal/code/home's
	// PreviewReadKind), and empty for a fetch.
	Via  CodeFetchedVia `json:"via"`
	Kind string         `json:"kind,omitempty"`

	// Reads is how many reads the record stands for: one at least. A record
	// that counts none is no read, and the day's derivation skips it as it
	// skips one nobody can attribute (store.DB.UsageForDay).
	Reads int64 `json:"reads"`

	// Bytes is what the reader was sent, over every read the record stands
	// for: the whole answer that carried the pack, for a fetch, and the
	// answer's encoding, for a server-side read.
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

// SummaryFor names the repository and what was read of it: a fetch by its
// size, a server-side read by its kind, each with how many times where the
// record stands for more than one.
func (e CodeFetched) SummaryFor(actor string) string {
	what := "fetched " + e.RepoName
	if e.Via == FetchedViaTool {
		what = "read " + e.RepoName
		if e.Kind != "" {
			what += "'s " + e.Kind
		}
	}
	if e.Reads > 1 {
		what += " " + strconv.FormatInt(e.Reads, 10) + " times"
	}
	return lead(actor, what+" ("+strconv.FormatInt(e.Bytes, 10)+" bytes)")
}

// CodeCheckFinished records that one attempt of a check of a commit ENDED —
// passed, failed, errored, cancelled, or skipped at its dispatch — as the
// attempt's row on the code log keeps the ending: published ONCE, by the data
// node whose checks writer published the record that set it (internal/code's
// ChecksWriter), on that record's apply — never by an applier, which every
// data node runs. An ending the row did not keep — a later infrastructure
// ending beside a measurement, an ending asked again — is announced by none.
//
// # A replaced ending is a correction, never a second ending
//
// The row keeps ONE ending, and a later record may replace the one it kept: a
// measurement replaces an infrastructure ending — a runner that was only slow
// ends its attempt passed after the home's sweep of dead runners errored it
// runner_lost — and an earlier ending replaces a later one of its kind, which
// two writers of one repository across a failover can each record. The
// replacing ending is announced too, naming in Replaces the ending it
// replaced, so a day FILES IT AS A CORRECTION — the attempt moves from
// Replaces to State — and counts each attempt once, under the ending its row
// keeps. Replaces is the ending the replacing record's writer read on the row
// when it decided; a failover's two writers recording the attempt's first
// ending at once each read none, which is the one replacement announced
// without it.
//
// # Why an event, beside the row
//
// The row is the attempt's state, replicated, and kept CheckHistory; the event
// is what a NODE'S DAY counts, from its own event log, as a day counts every
// other activity it derives (store.DB.UsageForDay) — the row is no day's, and
// a count read off the rows at a day's close would count each attempt on
// every data node. So the event carries what a day files the ending under and
// nothing a reader of the attempt needs: the attempt is read from its row.
//
// # Where it is filed
//
// Persisted, in the `system` category, and kept out of the activity feed
// (internal/events' unfed list): every push of every change ends each check
// its file defines, a busy company's faster than a person reads a feed, and an
// attempt's ending is read where it is asked — the change's checks, the
// repository's.
type CodeCheckFinished struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name when the ending applied.
	RepoID   string `json:"repo_id"`
	RepoName string `json:"repo_name"`

	// OID is the commit checked, Name the check and Attempt the attempt.
	OID     string `json:"oid"`
	Name    string `json:"name"`
	Attempt int    `json:"attempt"`

	// Purpose is what the attempt was for (internal/code's CheckPurpose:
	// head, candidate, tip, bisect); ChangeID a head check's change, and
	// Target the branch the commit was checked for.
	Purpose  string `json:"purpose"`
	ChangeID string `json:"change_id,omitempty"`
	Target   string `json:"target"`

	// State is the ending (internal/code's CheckState) and Reason why
	// (CheckReason); Placement is where it ran — container, e2b or external
	// — and empty for an attempt no runner claimed.
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
}

// EventType is the "code_check_finished" wire type.
func (CodeCheckFinished) EventType() string { return "code_check_finished" }

// SummaryFor names the check, its attempt, the commit's first twelve digits,
// the repository and how it ended.
func (e CodeCheckFinished) SummaryFor(actor string) string {
	commit := e.OID
	if len(commit) > 12 {
		commit = commit[:12]
	}
	what := "check " + e.Name + " (attempt " + strconv.Itoa(e.Attempt) + ") of " + e.RepoName + "@" + commit +
		" " + e.State
	if e.Reason != "" {
		what += " (" + e.Reason + ")"
	}
	if e.Replaces != "" {
		what += ", replacing its " + e.Replaces + " ending"
	}
	return lead(actor, what)
}
