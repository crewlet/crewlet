package assistant

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/live"
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
//
// Unready is the failure a turn's first round would meet as far as the node
// knows WITHOUT CALLING A MODEL, or nil when any member may answer: every
// member's plan spent, its harness signed out or unable to start
// (chain.Unready over the members). It is what `/viewer` tells a person
// before they ask and what refuses a question before any round.
type Models interface {
	Chain(ctx context.Context, person iam.Principal, use auxspend.Use) (llm.Provider, string, error)
	Unready(ctx context.Context) error
}

// Budget is the meter a turn charges its rounds through: the company's
// windows, and the person's own where a company caps one. Nil with a nil
// error is a node with no counter to charge, whose turns are recorded on the
// ledger and limited by nothing else.
type Budget interface {
	Meter(ctx context.Context, person iam.Principal) (toolloop.BudgetMeter, error)
}

// Catalogue is the curated tool table cut to what this person may be offered
// now: catalog.Offered as written, reads by their apps and writes by the act
// verbs the authority table admits them to (acts). Offering is a filter and
// never authority: every call is still decided by the route it sends, as the
// person.
type Catalogue interface {
	Offered(p iam.Principal, acts []string) []catalog.Tool
}

// Transcripts is the read side of the assistant's own domain, every read keyed
// on the owner but the expiry backstop's nomination: *assistantdomain.Reader as
// written.
type Transcripts interface {
	Conversations(ctx context.Context, owner uuid.UUID, cursor string, limit int) (wire.ConversationList, error)
	Conversation(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Conversation, error)
	Transcript(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Transcript, error)
	Turn(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Turn, error)
	Parked(ctx context.Context, owner, turn uuid.UUID) ([]byte, error)
	Card(ctx context.Context, owner, id uuid.UUID) (assistantdomain.Card, error)
	// ParkedPast nominates parked turns, of any owner, none of whose
	// proposals expires after now: the expiry backstop's one question
	// across owners, which only nominates ([Service.ReapLapsed] decides).
	ParkedPast(ctx context.Context, now time.Time, limit int) ([]assistantdomain.TurnRef, error)
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
	// OpenTurn answers [assistantdomain.ErrTurnOpen] for a turn already
	// open (a retry the ledger collapsed, or one finding its turn open).
	OpenTurn(ctx context.Context, w Write, t assistantdomain.NewTurn) (int, error)
	RecordMessage(ctx context.Context, w Write, turn uuid.UUID, m assistantdomain.NewMessage) error
	RecordStep(ctx context.Context, w Write, turn uuid.UUID, s wire.StepEvent) error
	RecordCard(ctx context.Context, w Write, turn uuid.UUID, c wire.CardEvent, apps []string) error
	ParkTurn(ctx context.Context, w Write, turn uuid.UUID, parked []byte, steps int) error
	ResumeTurn(ctx context.Context, w Write, turn uuid.UUID, node string) error
	CloseTurn(ctx context.Context, w Write, turn uuid.UUID, end assistantdomain.TurnEnd) error
	// CreateProposal records a proposal a write's preview became
	// ([proposal.New]), as it is.
	CreateProposal(ctx context.Context, w Write, p assistantdomain.NewProposal) error
	// ApproveProposal records the turn's own approval of a proposal it just
	// made: a page action its screen declares `confirm: none` (D11), which
	// is applied at once with an Undo receipt rather than asked of the
	// person, and is recorded as a proposal all the same.
	ApproveProposal(ctx context.Context, w Write, proposal uuid.UUID) error
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

// Remote is this node's half of the fleet (internal/assistant/live's Fleet,
// through [FleetRemote]): it tells the fleet every frame this node appends,
// follows a turn running on another node, and stops one there. NIL IS A NODE
// THAT SERVES ITS OWN TURNS: a stream of a turn running elsewhere is answered
// with its persisted transcript, and a Stop of one is unavailable.
type Remote interface {
	// Publish tells the fleet one frame this node appended. It never
	// blocks (live.Tap).
	Publish(f wire.Frame)
	// Follow streams owner's turn running on node after seq after, and
	// reports what asking found: served (the runner answered), gone (its
	// node is not running it or has left the fleet: the turn died with
	// it) or unreachable. Nothing is written unless it was served.
	// Another person's turn is [ErrNotFound].
	Follow(ctx context.Context, owner, turn uuid.UUID, node string, after uint64,
		w FrameWriter) (live.Reach, error)
	// Stop asks node to stop owner's turn, answering the state it ended
	// in where it was served. Another person's turn is [ErrNotFound].
	Stop(ctx context.Context, owner, turn uuid.UUID, node string) (wire.TurnState, live.Reach, error)
	// LastSeq is the newest seq any node numbered for owner's turn, and the
	// node holding it: what a segment opened here continues after, so a
	// client that drew frames from another node never meets a number it has
	// seen, and where a stream of the turn's newest segment is answered.
	LastSeq(ctx context.Context, owner, turn uuid.UUID) (string, uint64, error)
}

// TurnRate is the fleet's fixed-window counter (internal/coord's Counter as
// written): a person's turn starts are counted across every node, so a
// script spreading its questions over the fleet meets the same
// [TurnsPerMinute] one node would hold it to.
type TurnRate interface {
	Allow(ctx context.Context, bucket string, limit int, window time.Duration, now time.Time) (bool, error)
}
