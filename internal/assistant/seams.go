package assistant

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE SEAMS, each declared here by its consumer and kept to what a turn calls.
// The engine satisfies them (internal/engine/assistant.go), and a test writes
// each down without standing up a store, a broker or a model.

// Models resolves the assistant's chain off the company the node runs now, for
// one turn. Every completion made through it is RECORDED on the
// auxiliary-spend ledger under use (the operator stage, a person's spend) and
// charged nowhere: the turn's own [Budget] meter charges, so a round is
// counted once. An error names why there is no chain (the assistant off, a
// chain the rules refuse, no model configured).
type Models interface {
	Chain(ctx context.Context, person iam.Principal, use auxspend.Use) (llm.Provider, string, error)
}

// Budget is the meter a turn charges its rounds through: the company's
// windows, and the person's own where a company caps one. Nil with a nil
// error is a node with no counter to charge, whose turns are recorded on the
// ledger and limited by nothing else.
type Budget interface {
	Meter(ctx context.Context, person iam.Principal) (toolloop.BudgetMeter, error)
}

// Catalogue is the curated tool table (internal/assistant/catalog), cut to
// what this person may be offered now: reads by their question's posture
// against their grants, writes by the act verbs the authority table admits
// them to (acts). Offering is a filter and never authority: every call is
// still decided by the route it sends, as the person.
type Catalogue interface {
	Offered(p iam.Principal, acts []string) []catalog.Tool
}

// Cards turns a tool's answer into the engine-built cards the person sees and
// the summary the model reads (internal/assistant/cards): a number on a card
// is the route's own, never the model's.
type Cards interface {
	// Read cards a read tool's answer. The summary is what the model is
	// handed: the turn redacts it, fences it where the tool says its
	// answer carries somebody else's text, and cuts it to
	// [ModelResultBytes].
	Read(t catalog.Tool, args map[string]any, r exec.Response, at time.Time) (Carded, error)
	// Navigate is a navigation tool's link, and the read whose answer says
	// whether the person may open it (nil for a link every person may).
	Navigate(t catalog.Tool, args map[string]any) (wire.Link, *exec.Request, error)
}

// Carded is a read's cards and its summary for the model.
type Carded struct {
	Cards   []wire.Card
	Summary string
}

// Transcripts is the read side of the assistant's own domain, every read keyed
// on the owner: *assistantdomain.Reader as written.
type Transcripts interface {
	Conversations(ctx context.Context, owner uuid.UUID, cursor string, limit int) (wire.ConversationList, error)
	Conversation(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Conversation, error)
	Transcript(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Transcript, error)
	Turn(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Turn, error)
	Parked(ctx context.Context, owner, turn uuid.UUID) ([]byte, error)
	Card(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Card, error)
}

// Records is the write side of the assistant's own domain, one method per
// record a turn writes. The engine adapts *assistantdomain.Writer to it: this
// package imports no state log, so the operation id a record is published
// under is derived there from the [Write]'s key and steps, and a write whose
// outcome is unknown answers [ErrOutcomeUnknown].
type Records interface {
	CreateConversation(ctx context.Context, w Write, id uuid.UUID, title string) error
	TitleConversation(ctx context.Context, w Write, id uuid.UUID, title string) error
	ClearConversation(ctx context.Context, w Write, id uuid.UUID) (Written, error)
	OpenTurn(ctx context.Context, w Write, t assistantdomain.NewTurn) (int, error)
	RecordMessage(ctx context.Context, w Write, turn uuid.UUID, m assistantdomain.NewMessage) error
	RecordStep(ctx context.Context, w Write, turn uuid.UUID, s wire.StepEvent) error
	RecordCard(ctx context.Context, w Write, turn uuid.UUID, c wire.CardEvent, apps []string) error
	ParkTurn(ctx context.Context, w Write, turn uuid.UUID, parked []byte) error
	ResumeTurn(ctx context.Context, w Write, turn uuid.UUID, node string) error
	CloseTurn(ctx context.Context, w Write, turn uuid.UUID, end assistantdomain.TurnEnd) error
}

// Write is one record's caller: whose conversation, who acts, and the
// operation it is published under.
type Write struct {
	Owner uuid.UUID
	By    iam.Actor
	// Key is the operation the gesture was made under (the request's
	// Idempotency-Key, scoped by internal/api/opkey), and Steps the record's
	// place inside it, so a retry of the gesture publishes the same
	// operations and the ledger collapses them.
	Key   string
	Steps []string
	// Derived names an operation no request named, in place of Key: one
	// every node that makes it derives alike from the turn it is about
	// (an interrupted turn's close), so the ledger keeps one.
	Derived *Derivation
}

// Derivation is an operation derived from what it is about: the instant it
// is anchored at (the turn's start), a name, and the ids that make it this
// operation and no other.
type Derivation struct {
	At       time.Time
	Name     string
	Identity []string
}

// Step is w for one more step inside its operation.
func (w Write) Step(steps ...string) Write {
	w.Steps = append(append([]string(nil), w.Steps...), steps...)
	return w
}

// Written is what a purge reported: the state log's outcome and the position
// its record landed at.
type Written struct {
	Outcome  string
	Position string
}

// ErrOutcomeUnknown reports a record whose outcome nobody can confirm: it may
// or may not have landed, and a retry under the same operation is the only
// safe move.
var ErrOutcomeUnknown = errors.New("assistant: the record's outcome is unknown")

// Revoker ends grants: *auth.Delegations as written. A turn ends its read
// grant when it ends or parks, and a cleared conversation every grant of its
// turns.
type Revoker interface {
	Revoke(grant uuid.UUID)
	RevokeTurn(turn uuid.UUID)
}

// AuditPublisher is where a turn's runtime audit record goes: the node's own
// queue, whose publish listener writes the event store row.
type AuditPublisher interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
}

// Metrics is the node's recorder, as a turn records into it.
type Metrics interface {
	Add(name string, n uint64, attrs metrics.Attrs)
	Observe(name string, d time.Duration, attrs metrics.Attrs)
	Set(name string, v float64, attrs metrics.Attrs)
}

// Company is the company this node runs now: its document and its built org,
// read per turn so an apply is the next turn's.
type Company func() (*config.Company, *org.Organization)

// FrameWriter is one live stream as the service writes to it: a frame, or a
// keepalive comment when the stream is quiet.
type FrameWriter interface {
	Frame(f wire.Frame) error
	Keepalive() error
}

// Proposer turns a write tool's call into a proposal the person approves
// (internal/assistant/proposal, ADR-0047): the server-side preview, the digest,
// the `proposal_created` record and the card rendered from the preview, never
// from what the model wrote. NIL OFFERS NO WRITE: a node whose approval path is
// not wired offers the model no write or page-action tool at all, so nothing
// write-shaped can be asked for.
type Proposer interface {
	Propose(ctx context.Context, c ProposalCall) (wire.Card, error)
}

// ProposalCall is one write tool call to turn into a proposal.
type ProposalCall struct {
	Person       iam.Principal
	Conversation uuid.UUID
	Turn         uuid.UUID
	// Write is the turn's record caller, for the proposal's own records.
	Write Write
	// Index is the step's place in the turn.
	Index int
	Tool  catalog.Tool
	Args  map[string]any
	// Read is the turn's read grant: the preview reads the object's current
	// values through it, as the person.
	Read exec.Grant
	// Page is the page context the turn was sent with, for a page action.
	Page *wire.PageContext
}

// Remote follows and stops a turn running on another node (the fleet's
// sealed broadcast and signed instruction channels). NIL IS A NODE THAT
// SERVES ITS OWN TURNS: a stream of a turn running elsewhere is answered with
// its persisted transcript, and a Stop of one is unavailable.
type Remote interface {
	// Follow streams a turn running on another node after seq after,
	// reporting false when no node answers for it (its runner died).
	Follow(ctx context.Context, owner, turn uuid.UUID, after uint64, w FrameWriter) (bool, error)
	// Stop asks the node running a turn to stop it, reporting false when
	// no node answers for it.
	Stop(ctx context.Context, owner, turn uuid.UUID) (wire.StopAnswer, bool, error)
}
