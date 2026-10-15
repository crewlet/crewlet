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
// actor is whoever the call acted AS (see [OperatorActed.Actor]), and the
// source says which kind of writer this was, so the log can be narrowed to
// people without knowing who they are.
const OperatorSource = "operator"

// RuntimeAuditTypes is every event type the runtime audit writes — each
// published through the operator surface's one Audit, with [OperatorSource] on
// its envelope. The Audit log reads exactly these as its "runtime" source, so a
// third record added here without the dashboard learning to draw it fails
// `TestTheAuditLogReadsEveryRuntimeAuditType` rather than arriving as rows the
// screen labels as nothing.
var RuntimeAuditTypes = []string{
	OperatorActed{}.EventType(),
	BackupRequested{}.EventType(),
}

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

// The three transports an operator tool call arrives on. Each reaches the
// catalogue through one dispatch (internal/api/operator), which is what records
// the call — so the transport is the ONLY thing about the record that depends
// on which surface a person used.
const (
	// TransportAct is the dashboard's write surface, `/operator/act`.
	TransportAct = "act"
	// TransportMCP is an operator's own assistant, `/operator/mcp`.
	TransportMCP = "mcp"
	// TransportWork is the human write surface's tool-backed routes,
	// `/work/…` and `/pages/…` (internal/api/workapi) — the scriptable
	// resource REST a CLI or a script writes through, which calls the
	// same dispatch rather than the tools directly.
	TransportWork = "work"
)

// OperatorActed is one operator tool call that is not a proven read, on any
// of the three transports: the dashboard's act route, an operator's assistant
// over MCP and the human write surface's tool-backed routes call the same
// catalogue, and an audit that depended on which one a person used would be
// missing whichever surface nobody tested.
//
// The ARGUMENTS ARE NEVER HERE, for the reason the act log line omits them: a
// page body or a comment is the company's content, and it already lives in
// the history of the object it changed. What is here is who, which verb,
// which request, and what became of it.
type OperatorActed struct {
	// ActorName, ActorKind and OperatorID are the caller exactly as every
	// record the call wrote names it — the three facts iam.ActorFor makes
	// of the principal, so the audit row and the history row it sits
	// beside cannot name two different people. ActorName is the author:
	// the seat for a person the identity directory binds to one, the
	// login for anybody else. ActorKind is iam.ActorKind's vocabulary
	// (agent, human, operator, system) — a string on the wire, so a kind
	// a later build adds is a value here rather than a refused row.
	// OperatorID is the CREDENTIAL the call came through — `pat:<id>`,
	// `session:<lineage>`, or the login where the principal is its own
	// credential — which is what an audit asks of a leaked token and what
	// tells a person's own write from one made through their token.
	ActorName  string `json:"actor"`
	ActorKind  string `json:"actor_kind"`
	OperatorID string `json:"operator_id"`

	Transport string `json:"transport"`
	Tool      string `json:"tool"`
	// RequestID is the OPERATION the call was made under: the op id
	// internal/api/opkey derived from the caller's key, so every retry of
	// one gesture carries the same value and the audit row joins the
	// ledger entry the write left. Empty for a call that named none.
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

// Actor is the author, which is who the records the call wrote name.
func (e OperatorActed) Actor() string { return e.ActorName }

// SummaryFor names the verb and what became of it.
func (e OperatorActed) SummaryFor(actor string) string {
	return lead(actor, "ran "+e.Tool+": "+outcomeWords(e.Outcome, e.Refusal))
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
// WHICH HOST is the envelope's `node`, and deliberately not a field here. A
// backup is only findable on the host it was written to, and the route that
// takes it publishes this record from that same node — so the node the queue
// stamps as the event's origin IS the node whose disk holds the copy. A
// payload `node` beside it would have been the one fact stated twice, and the
// envelope owns the key: a payload field under it is dropped on the way out.
type BackupRequested struct {
	// ActorName, ActorKind and OperatorID are who asked, in the three
	// halves of [OperatorActed.ActorName].
	ActorName  string `json:"actor"`
	ActorKind  string `json:"actor_kind"`
	OperatorID string `json:"operator_id"`

	Dir string `json:"dir"`
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

// Actor is who asked for the backup.
func (e BackupRequested) Actor() string { return e.ActorName }

// SummaryFor says where the copy went, which is the one thing somebody
// reading the log after the fact needs. The host is the row's own node, which
// every surface showing the row shows beside it.
func (e BackupRequested) SummaryFor(actor string) string {
	if e.Outcome.failed() {
		return lead(actor, "backup to "+e.Dir+" failed")
	}
	return lead(actor, "backed up to "+e.Dir+
		" ("+strconv.Itoa(e.Streams)+" streams)")
}

// NewBackupRequested builds the audit record for one backup, with [Failed]
// derived from the outcome.
func NewBackupRequested(e BackupRequested) BackupRequested {
	e.Failed = e.Outcome.failed()
	return e
}

// outcomeWords renders an outcome, with the refusal class when there is one.
func outcomeWords(o AuditOutcome, refusal string) string {
	if o == AuditRefused && refusal != "" {
		return string(o) + " (" + refusal + ")"
	}
	return string(o)
}
