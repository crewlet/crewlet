package types

import (
	"strconv"

	"github.com/crewlet/crewlet/internal/events"
)

// The runtime audit: what a PERSON did to the company through a running node,
// on the surfaces that keep no record of their own.
//
// # Why these exist beside the records every write already makes
//
// A tracker commit names its author and a page revision names its editor, but
// both are the record of a CHANGE — a call that was refused, or one whose
// answer never came back, leaves nothing there, and a verb that is not a
// tracker or wiki write (a backup, and the seat controls the dashboard gains)
// leaves nothing anywhere. The configuration and credential surfaces keep
// their own history (a revision names who created it, a credential row who
// stored it) and are not repeated here. These two events are the fifth
// source the Audit log reads: every CALL, whatever became of it.
//
// # Who has to agree on it
//
// Nobody but the node the call reached. The row is written by the event
// store's publish listener on the publishing node, like every other event, so
// a fleet's audit is the union of its nodes' logs — which is what the event
// query already answers. There is no fleet-wide ordering to keep: two people
// acting on two nodes did two independent things.

func init() {
	events.Register[OperatorActed]()
	events.Register[BackupRequested]()
}

// OperatorSource is the envelope source every runtime audit event carries —
// what `events?source=operator` selects. Not a node id and not a handle: the
// actor is the credential (see [OperatorActed.Actor]), and the source says
// which kind of writer this was, so the log can be narrowed to people without
// knowing who they are.
const OperatorSource = "operator"

// AuditOutcome is what became of an audited call.
//
// The first three are the state log's own write outcomes, spelled as it spells
// them so a reader holds ONE vocabulary: `applied` landed, `pending` was
// accepted and is not yet applied everywhere, `unknown` may or may not have
// landed — which is also what an interrupted call is, since the context ending
// says nothing about whether the write before it happened. The last two are
// the call's own: `refused` was turned down by the tool for a stated reason,
// and `failed` broke without one.
type AuditOutcome string

// The five outcomes an audited call can have.
const (
	AuditApplied AuditOutcome = "applied"
	AuditPending AuditOutcome = "pending"
	AuditUnknown AuditOutcome = "unknown"
	AuditRefused AuditOutcome = "refused"
	AuditFailed  AuditOutcome = "failed"
)

// Valid reports whether o is one of the five outcomes.
func (o AuditOutcome) Valid() bool {
	switch o {
	case AuditApplied, AuditPending, AuditUnknown, AuditRefused, AuditFailed:
		return true
	}
	return false
}

// failed reports whether the outcome says nothing was written — the two
// outcomes that are failures of the call rather than states of a write.
func (o AuditOutcome) failed() bool { return o == AuditRefused || o == AuditFailed }

// The two transports an operator tool call arrives on.
const (
	// TransportAct is the dashboard's write surface, `/operator/act`.
	TransportAct = "act"
	// TransportMCP is an operator's own assistant, `/operator/mcp`.
	TransportMCP = "mcp"
)

// OperatorActed is one operator tool call that is not a proven read, on
// either transport: the dashboard's act route and an operator's assistant
// over MCP call the same catalogue, and an audit that depended on which one a
// person used would be missing whichever half nobody tested.
//
// The ARGUMENTS ARE NEVER HERE, for the reason the act log line omits them: a
// page body or a comment is the company's content, and it already lives in
// the history of the object it changed. What is here is who, which verb,
// which request, and what became of it.
type OperatorActed struct {
	// OperatorID is the token's own name — the author every record the
	// call wrote carries — and ActorSeat the person it is bound to, empty
	// for a credential nobody bound (which only the MCP transport admits).
	OperatorID string `json:"operator_id"`
	ActorSeat  string `json:"actor_seat,omitempty"`

	Transport string `json:"transport"`
	Tool      string `json:"tool"`
	// RequestID is the caller's own request identity on the act
	// transport, canonical, so every retry of one gesture carries the same
	// value. MCP names none.
	RequestID string `json:"request_id,omitempty"`

	Outcome AuditOutcome `json:"outcome"`
	// Position is where the write landed, when the tool stated one.
	Position string `json:"position,omitempty"`
	// Refusal is the refusal class of a `refused` call.
	Refusal string `json:"refusal,omitempty"`
	// Failed is stamped for `refused` and `failed`, so the event store
	// tags the row and the log's failure filter finds it.
	Failed bool `json:"failed,omitempty"`
}

// EventType is the "operator_acted" wire type.
func (OperatorActed) EventType() string { return "operator_acted" }

// Actor is the credential, which is who the records name as the author.
func (e OperatorActed) Actor() string { return e.OperatorID }

// SummaryFor names the verb and what became of it, and the person when the
// credential is bound to one.
func (e OperatorActed) SummaryFor(actor string) string {
	return lead(asPerson(actor, e.ActorSeat), "ran "+e.Tool+": "+outcomeWords(e.Outcome, e.Refusal))
}

// NewOperatorActed builds the audit record for one call, with [Failed]
// derived from the outcome rather than set beside it.
func NewOperatorActed(e OperatorActed) OperatorActed {
	e.Failed = e.Outcome.failed()
	return e
}

// BackupRequested is one `POST /backup`: a copy of a node's whole durable
// state, credentials included, written to a directory on that node's host.
//
// Node is named in the payload because the envelope's source is
// [OperatorSource], and a backup is only findable on the host it was written
// to.
type BackupRequested struct {
	OperatorID string `json:"operator_id"`
	ActorSeat  string `json:"actor_seat,omitempty"`

	Node string `json:"node"`
	Dir  string `json:"dir"`
	// Outcome is `applied` for a backup whose manifest was written and
	// `failed` for one that was not — never anything else, since the
	// route is synchronous and finishes the copy even if the caller hangs
	// up.
	Outcome AuditOutcome `json:"outcome"`
	// Streams is how many streams the manifest covers.
	Streams int  `json:"streams,omitempty"`
	Failed  bool `json:"failed,omitempty"`
}

// EventType is the "backup_requested" wire type.
func (BackupRequested) EventType() string { return "backup_requested" }

// Actor is the credential.
func (e BackupRequested) Actor() string { return e.OperatorID }

// SummaryFor says where the copy went, which is the one thing somebody
// reading the log after the fact needs.
func (e BackupRequested) SummaryFor(actor string) string {
	if e.Outcome.failed() {
		return lead(asPerson(actor, e.ActorSeat), "backup of "+e.Node+" to "+e.Dir+" failed")
	}
	return lead(asPerson(actor, e.ActorSeat), "backed up "+e.Node+" to "+e.Dir+
		" ("+strconv.Itoa(e.Streams)+" streams)")
}

// NewBackupRequested builds the audit record for one backup, with [Failed]
// derived from the outcome.
func NewBackupRequested(e BackupRequested) BackupRequested {
	e.Failed = e.Outcome.failed()
	return e
}

// asPerson renders a credential with the person it is bound to.
func asPerson(actor, seat string) string {
	if seat == "" || seat == actor {
		return actor
	}
	return actor + " (" + seat + ")"
}

// outcomeWords renders an outcome, with the refusal class when there is one.
func outcomeWords(o AuditOutcome, refusal string) string {
	if o == AuditRefused && refusal != "" {
		return string(o) + " (" + refusal + ")"
	}
	return string(o)
}
