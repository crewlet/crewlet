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
// # One record per read that SENT CONTENTS
//
// A git fetch is recorded once its request ended well and its answer carried a
// pack. A fetch over a stateless transport negotiates in ROUNDS, each a request
// of its own whose answer is acknowledgments and no object, and only the round
// that ends the negotiation is answered with the pack — so a clone or a fetch
// is one record however many rounds it took, and a round that sent nothing is
// none. An `ls-refs`, an advertisement and a fetch the node ended before its
// answer was whole are no record: they sent no contents, or not all of them.
// A server-side read — a code tool's tree, file, diff, comparison or landed-in
// answer — is recorded once it answered, from the repository's copy or from its
// memo alike, since the memo is the copy's and answers every asker.
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

// CodeFetched records one read of a `visibility: unit` repository's contents:
// a git fetch whose answer carried a pack, or a server-side read that
// answered. Recorded on the node that SERVED it, and never for a
// `visibility: company` repository, whose readers nobody audits.
type CodeFetched struct {
	// RepoID is the repository's id, which outlives its name; RepoName its
	// name as the read named it.
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
	// (code.GitReadKind: tree, blob, diff, compare, landed_in), and empty for
	// a fetch.
	Via  CodeFetchedVia `json:"via"`
	Kind string         `json:"kind,omitempty"`

	// Bytes is what the reader was sent: the whole answer that carried the
	// pack, for a fetch, and the answer's encoding, for a server-side read.
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
// size, a server-side read by its kind.
func (e CodeFetched) SummaryFor(actor string) string {
	if e.Via == FetchedViaTool {
		what := "read " + e.RepoName
		if e.Kind != "" {
			what += "'s " + e.Kind
		}
		return lead(actor, what+" ("+strconv.FormatInt(e.Bytes, 10)+" bytes)")
	}
	return lead(actor, "fetched "+e.RepoName+" ("+strconv.FormatInt(e.Bytes, 10)+" bytes)")
}
