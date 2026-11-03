package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT A RELAUNCH OF A SEAT'S TURN WOULD BE LAUNCHED ON, answered by the node
// holding the seat ([sandbox.RelaunchTarget]).
//
// # Asked of the holder, never decided where the question lands
//
// Whether a relaunch would continue a turn's session turns on the member of the
// seat's executor chain it is launched on, and which member that is turns on the
// plans the node holding the seat has met spent — its own bench, which no node
// shares (internal/providers/llm/harness) — and on the company revision that
// node runs, which says whether sessions are kept. A relaunch is decided on that
// node. So a reader of a seat's sessions, on whichever node serves the API, asks
// it: this node where it holds the seat, and otherwise the incarnation the
// seat's lease names, over an ephemeral scatter only that incarnation answers
// ([topics.HeldRelaunch]) — as a seat's memory is read
// (internal/learning/memread).
//
// # What crosses
//
// The holder's pick: its member's harness, the cell that member's run lands in,
// the holder itself — a box on an engine host is continued in only by the node
// it was launched on — why there is no member or why none can be said now, and
// whether the company keeps sessions; and, for each archive the asker names,
// whether that member's runner restores it. The runner is the holder's, of the
// holder's build, which a rolling upgrade can make another than the asker's.
//
// AN UNKNOWN IS NEVER A NO: a lease that cannot be read, a seat no node holds,
// and a holder that does not answer within the fleet read budget — one that
// stopped, or a build that does not serve the question — are each a target that
// cannot be said now ([sandbox.RelaunchTarget.Undecided]), which a reader shows
// as undecided rather than as a session no relaunch continues.

// relaunchTargetBudget bounds how long a reader waits for the holder's answer:
// the fleet read budget every scatter in this engine answers inside
// ([eventfan.FleetReadBudget]) — a round trip on the broker and a pick from the
// holder's own memory, which a healthy node answers in milliseconds.
const relaunchTargetBudget = eventfan.FleetReadBudget

// relaunchWireVersion is the request and reply shape this build speaks.
// Evolution is ADDITIVE within a version: a key this build does not know is
// dropped on decode, which a rolling upgrade needs and nothing more.
const relaunchWireVersion = 1

// relaunchArchive is a session's archive as a request names it and a reply's
// answers are keyed by: what a runner's restore reads of it.
type relaunchArchive struct {
	Harness   string `json:"harness"`
	SessionID string `json:"session_id"`
	Format    int    `json:"format"`
	Version   string `json:"version"`
}

func relaunchArchiveOf(a sandbox.SessionArchive) relaunchArchive {
	return relaunchArchive{Harness: a.Harness, SessionID: a.SessionID, Format: a.Format, Version: a.Version}
}

func (a relaunchArchive) archive() sandbox.SessionArchive {
	return sandbox.SessionArchive{Harness: a.Harness, SessionID: a.SessionID, Format: a.Format,
		Version: a.Version}
}

// relaunchAsk is one reader's question, addressed to the incarnation the
// seat's lease named: only that process answers, so a holder that restarted
// since is silent rather than answered for by its successor.
type relaunchAsk struct {
	Version  int               `json:"v"`
	Owner    string            `json:"owner"`
	Handle   string            `json:"handle"`
	Archives []relaunchArchive `json:"archives,omitempty"`
}

// relaunchRestore is the holder's runner's answer for one archive.
type relaunchRestore struct {
	Restores bool   `json:"restores"`
	Why      string `json:"why,omitempty"`
}

// relaunchReply is the holder's answer. It names its owner because a
// scatter's replies carry no sender; Restores answers the asked archives, in
// the order they were asked.
type relaunchReply struct {
	Version   int               `json:"v"`
	Owner     string            `json:"owner"`
	Harness   string            `json:"harness,omitempty"`
	Placement string            `json:"placement,omitempty"`
	Node      string            `json:"node,omitempty"`
	Why       string            `json:"why,omitempty"`
	Undecided bool              `json:"undecided,omitempty"`
	NoKept    string            `json:"no_kept,omitempty"`
	Restores  []relaunchRestore `json:"restores,omitempty"`
}

// errRelaunchNotAddressed is a request for another incarnation: answered with
// nothing, since its asker waits on the one it named.
var errRelaunchNotAddressed = errors.New("engine: the relaunch target request is addressed to another incarnation")

// RelaunchTarget is what a relaunch of the seat's turn would be launched on now,
// as the node holding the seat picks it — this node's own pick where it holds
// the seat ([Engine.localRelaunchTarget]), and otherwise the holder's answer,
// asked over the broker — with whether that member's runner restores each of
// archives: what the read surface decides each row of a seat's sessions by
// (internal/api/queries' `continuable`, [sandbox.ReadContinuityOf]). Never an
// error: what cannot be said now is a target that says so.
func (e *Engine) RelaunchTarget(ctx context.Context, seat uuid.UUID, handle string,
	archives []sandbox.SessionArchive) sandbox.RelaunchTarget {

	return e.relaunchTargets().target(ctx, seat, handle, archives)
}

// relaunchTargets is this node as it asks, and answers, what a relaunch of a
// seat's turn would be launched on.
func (e *Engine) relaunchTargets() relaunchTargets {
	r := relaunchTargets{local: e.localRelaunchTarget, budget: relaunchTargetBudget, now: time.Now}
	if e.node == nil {
		return r
	}
	r.owner = e.node.Owner()
	// A NODE WITH NO BROKER IS THE FLEET, and holds every seat there is: it
	// answers every question itself.
	if e.backends != nil && e.backends.Queue != nil && e.backends.Coord != nil {
		r.queue, r.leases = e.backends.Queue, e.backends.Coord
	}
	return r
}

// relaunchAsker scatters one question — the half of the broker a reader needs.
type relaunchAsker interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
}

// relaunchLeases reads which incarnation holds a seat — the half of the
// coordination store a reader needs.
type relaunchLeases interface {
	Get(ctx context.Context, resource string) (*coord.Lease, error)
}

// relaunchTargets answers what a relaunch of a seat's turn would be launched
// on, from the node holding the seat: owner is this process's incarnation, which
// a seat's lease names; local this node's own pick; queue nil, or leases nil,
// a node that answers every question itself.
type relaunchTargets struct {
	owner  string
	queue  relaunchAsker
	leases relaunchLeases
	local  func(handle string) sandbox.RelaunchTarget
	budget time.Duration
	now    func() time.Time
}

// target is [Engine.RelaunchTarget]'s answer.
func (r relaunchTargets) target(ctx context.Context, seat uuid.UUID, handle string,
	archives []sandbox.SessionArchive) sandbox.RelaunchTarget {

	if r.queue == nil || r.leases == nil {
		return r.local(handle)
	}
	lease, err := r.leases.Get(ctx, coord.SeatResource(seat))
	switch {
	case err != nil:
		// UNKNOWN, never "held by nobody": an unreadable lease table says
		// nothing about where the seat is.
		return undecidedRelaunch(fmt.Sprintf("which node holds the seat could not be read: %v", err))
	case lease == nil || !lease.Live(r.now()):
		return undecidedRelaunch("no node holds the seat now, and the node that takes it decides what a " +
			"relaunch of its turns is launched on")
	case lease.Owner == r.owner:
		return r.local(handle)
	}
	return r.ask(ctx, lease.Owner, handle, archives)
}

// undecidedRelaunch is a target that cannot be said now, for why.
func undecidedRelaunch(why string) sandbox.RelaunchTarget {
	return sandbox.RelaunchTarget{Undecided: true, Why: why}
}

// ask puts the question to owner, the incarnation holding the seat, and reads
// its answer as the target it names.
func (r relaunchTargets) ask(ctx context.Context, owner, handle string,
	archives []sandbox.SessionArchive) sandbox.RelaunchTarget {

	node := config.IncarnationNode(owner)
	question := relaunchAsk{Version: relaunchWireVersion, Owner: owner, Handle: handle}
	for _, a := range archives {
		question.Archives = append(question.Archives, relaunchArchiveOf(a))
	}
	raw, err := json.Marshal(question)
	if err != nil {
		return undecidedRelaunch(fmt.Sprintf("the question for %s, which holds the seat, could not be "+
			"written: %v", node, err))
	}
	askCtx, cancel := context.WithTimeout(ctx, r.budget)
	defer cancel()
	replies, err := r.queue.Ask(askCtx, topics.HeldRelaunch, raw, 1)
	if err != nil {
		return undecidedRelaunch(fmt.Sprintf("%s, which holds the seat, could not be asked what a "+
			"relaunch is launched on: %v", node, err))
	}
	for _, body := range replies {
		var reply relaunchReply
		if json.Unmarshal(body, &reply) != nil || reply.Owner != owner {
			continue
		}
		return reply.target(question.Archives)
	}
	return undecidedRelaunch(fmt.Sprintf("%s, which holds the seat, did not say within %s what a "+
		"relaunch is launched on", node, r.budget))
}

// target is the reply as the target it names, its runner the holder's answers
// for the archives asked.
func (r relaunchReply) target(asked []relaunchArchive) sandbox.RelaunchTarget {
	answers := &answeredRestores{answers: make(map[relaunchArchive]relaunchRestore, len(asked))}
	for i, a := range asked {
		if i < len(r.Restores) {
			answers.answers[a] = r.Restores[i]
		}
	}
	return sandbox.RelaunchTarget{Harness: r.Harness, Placement: sandbox.Placement(r.Placement), Node: r.Node,
		Restorer: answers, Why: r.Why, Undecided: r.Undecided, NoKept: r.NoKept}
}

// answeredRestores is the holder's runner as its answers for the archives a
// reader asked about ([sandbox.ArchiveRestorer]). A pointer, so a target that
// carries it compares as any other does.
type answeredRestores struct {
	answers map[relaunchArchive]relaunchRestore
}

var _ sandbox.ArchiveRestorer = (*answeredRestores)(nil)

// Restores is the holder's answer for archive.
func (a *answeredRestores) Restores(archive sandbox.SessionArchive) (bool, string) {
	answer, ok := a.answers[relaunchArchiveOf(archive)]
	if !ok {
		return false, "the node holding the seat was not asked whether its runner restores this session"
	}
	return answer.Restores, answer.Why
}

// armRelaunchTargets makes this node an answerer for what a relaunch of a seat
// it holds would be launched on.
//
// EVERY NODE SERVES, in every mode and whether or not it serves the API, and
// only the addressed incarnation answers: a seat can be held by a node that
// serves no API, and answering is not publishing. After the node, whose
// incarnation a seat's lease names.
func (e *Engine) armRelaunchTargets(ctx context.Context) error {
	if e.backends == nil || e.backends.Queue == nil || e.node == nil {
		return nil
	}
	answerer := e.relaunchTargets()
	stop, err := e.backends.Queue.Serve(ctx, topics.HeldRelaunch, func(_ context.Context, raw []byte) ([]byte, error) {
		return answerer.answer(raw)
	})
	if err != nil {
		return fmt.Errorf("engine: answer what a relaunch of a seat's turn is launched on: %w", err)
	}
	e.stopRelaunchServe = stop
	return nil
}

// answer is one question, answered from this node's own pick where it is
// addressed to this process's incarnation, and with nothing where it is not.
func (r relaunchTargets) answer(raw []byte) ([]byte, error) {
	var question relaunchAsk
	if err := json.Unmarshal(raw, &question); err != nil {
		return nil, fmt.Errorf("engine: a relaunch target request this build cannot read: %w", err)
	}
	if question.Owner == "" || question.Owner != r.owner {
		return nil, errRelaunchNotAddressed
	}
	target := r.local(question.Handle)
	reply := relaunchReply{Version: relaunchWireVersion, Owner: r.owner, Harness: target.Harness,
		Placement: string(target.Placement), Node: target.Node, Why: target.Why, Undecided: target.Undecided,
		NoKept: target.NoKept}
	for _, a := range question.Archives {
		restores, why := target.Restores(a.archive())
		reply.Restores = append(reply.Restores, relaunchRestore{Restores: restores, Why: why})
	}
	return json.Marshal(reply)
}

// stopRelaunchTargets withdraws this node as an answerer, before the broker
// closes.
func (e *Engine) stopRelaunchTargets(ctx context.Context) {
	if e.stopRelaunchServe == nil {
		return
	}
	if err := e.stopRelaunchServe(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "relaunch_target_answerer_not_withdrawn", "error", err)
	}
	e.stopRelaunchServe = nil
}
