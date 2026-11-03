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

// WHAT A LAUNCH OF A SEAT'S TURN WOULD BE LAUNCHED ON — a relaunch of a live
// turn, or the next turn of a conversation whose last turn left a session —
// answered by the node holding the seat ([sandbox.RelaunchTarget]).
//
// # Asked of the holder, never decided where the question lands
//
// Whether a launch would continue a turn's session turns on the member of the
// seat's executor chain it is launched on, and which member that is turns on the
// plans the node holding the seat has met spent — its own bench, which no node
// shares (internal/providers/llm/harness) — and on the company revision that
// node runs, which says whether sessions are kept. A launch is decided on that
// node. So a reader of a seat's sessions, on whichever node serves the API, asks
// it: this node where it holds the seat, and otherwise the incarnation the
// seat's lease names, over an ephemeral scatter only that incarnation answers
// ([topics.HeldRelaunch]) — as a seat's memory is read
// (internal/learning/memread).
//
// # Per turn, and for the next turn by none
//
// The chain a launch picks its member from is the one of the company revision
// it is launched under, and for a relaunch that is the turn's: a resumed
// segment of the turn running on the holder — a review, whose send-back
// relaunches the round, or a round carried on — relaunches on the revision it
// was admitted under, pinned for the whole segment ([resumeInput.Company]),
// which an apply since does not move; any other turn's next segment is
// admitted under the revision the holder runs when it comes
// ([Engine.relaunchCompany]). So the question names, for each session it asks
// about, the turn it is of, and the holder answers each with the pick of the
// revision that turn's relaunch would run on. A CONVERSATION'S NEXT TURN is
// admitted under the revision the holder runs when it comes, too, so the
// session a reader asks about for it — the head of its conversation — is
// asked by NO turn: by the ended turn's own id, a segment of it still pinned
// in the moments before it returns would answer for a revision the next turn
// never runs on.
//
// # What crosses
//
// For each session asked about: the holder's pick — its member's harness, the
// cell that member's run lands in, the holder itself, since a box on an engine
// host is continued in only by the node it was launched on — why there is no
// member or why none can be said now, whether the company keeps sessions, and
// whether that member's runner restores the session's archive. The runner is
// the holder's, of the holder's build, which a rolling upgrade can make another
// than the asker's.
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

// relaunchArchive is a session's archive as a request names it: what a runner's
// restore reads of it.
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

// relaunchSession is one session a request asks about: the turn a relaunch
// would be of, and the archive the turn's row names.
type relaunchSession struct {
	Turn    string          `json:"turn"`
	Archive relaunchArchive `json:"archive"`
}

// relaunchAsk is one reader's question, addressed to the incarnation the
// seat's lease named: only that process answers, so a holder that restarted
// since is silent rather than answered for by its successor.
type relaunchAsk struct {
	Version  int               `json:"v"`
	Owner    string            `json:"owner"`
	Handle   string            `json:"handle"`
	Sessions []relaunchSession `json:"sessions,omitempty"`
}

// relaunchTargetReply is the holder's answer for one session asked about: its
// pick for that session's turn, and whether the picked member's runner
// restores that session's archive.
type relaunchTargetReply struct {
	Harness    string `json:"harness,omitempty"`
	Placement  string `json:"placement,omitempty"`
	Node       string `json:"node,omitempty"`
	Why        string `json:"why,omitempty"`
	Undecided  bool   `json:"undecided,omitempty"`
	NoKept     string `json:"no_kept,omitempty"`
	Restores   bool   `json:"restores,omitempty"`
	RestoreWhy string `json:"restore_why,omitempty"`
}

// relaunchReply is the holder's answer. It names its owner because a
// scatter's replies carry no sender; Targets answers the sessions asked, in
// the order they were asked.
type relaunchReply struct {
	Version int                   `json:"v"`
	Owner   string                `json:"owner"`
	Targets []relaunchTargetReply `json:"targets,omitempty"`
}

// errRelaunchNotAddressed is a request for another incarnation: answered with
// nothing, since its asker waits on the one it named.
var errRelaunchNotAddressed = errors.New("engine: the relaunch target request is addressed to another incarnation")

// RelaunchTargets is what a launch of each session asked about would be
// launched on now — a relaunch of the turn an ask names, or a conversation's
// next turn for an ask that names none — one target per ask in the order
// asked, as the node holding the seat picks it — this node's own pick where it
// holds the seat ([Engine.localRelaunchTarget]), and otherwise the holder's
// answer, asked over the broker once for every ask — with whether that
// member's runner restores the ask's archive: what the read surface decides
// each row of a seat's sessions by (internal/api/queries' `continuable`,
// [sandbox.ReadContinuityOf]). Never an error: what cannot be said now is a
// target that says so.
func (e *Engine) RelaunchTargets(ctx context.Context, seat uuid.UUID, handle string,
	asks []sandbox.RelaunchAsk) []sandbox.RelaunchTarget {

	return e.relaunchTargets().targets(ctx, seat, handle, asks)
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
// a seat's lease names; local this node's own pick for a turn; queue nil, or
// leases nil, a node that answers every question itself.
type relaunchTargets struct {
	owner  string
	queue  relaunchAsker
	leases relaunchLeases
	local  func(handle, turn string) sandbox.RelaunchTarget
	budget time.Duration
	now    func() time.Time
}

// targets is [Engine.RelaunchTargets]' answer.
func (r relaunchTargets) targets(ctx context.Context, seat uuid.UUID, handle string,
	asks []sandbox.RelaunchAsk) []sandbox.RelaunchTarget {

	if len(asks) == 0 {
		return nil
	}
	if r.queue == nil || r.leases == nil {
		return r.locals(handle, asks)
	}
	lease, err := r.leases.Get(ctx, coord.SeatResource(seat))
	switch {
	case err != nil:
		// UNKNOWN, never "held by nobody": an unreadable lease table says
		// nothing about where the seat is.
		return undecidedRelaunches(len(asks), fmt.Sprintf("which node holds the seat could not be read: %v", err))
	case lease == nil || !lease.Live(r.now()):
		return undecidedRelaunches(len(asks), "no node holds the seat now, and the node that takes it decides "+
			"what a launch of its turns is launched on")
	case lease.Owner == r.owner:
		return r.locals(handle, asks)
	}
	return r.ask(ctx, lease.Owner, handle, asks)
}

// locals is this node's own pick for each ask.
func (r relaunchTargets) locals(handle string, asks []sandbox.RelaunchAsk) []sandbox.RelaunchTarget {
	out := make([]sandbox.RelaunchTarget, len(asks))
	for i, a := range asks {
		out[i] = r.local(handle, a.Turn)
	}
	return out
}

// undecidedRelaunches is n targets that cannot be said now, for why.
func undecidedRelaunches(n int, why string) []sandbox.RelaunchTarget {
	out := make([]sandbox.RelaunchTarget, n)
	for i := range out {
		out[i] = sandbox.RelaunchTarget{Undecided: true, Why: why}
	}
	return out
}

// ask puts the question to owner, the incarnation holding the seat, and reads
// its answer as the targets it names.
func (r relaunchTargets) ask(ctx context.Context, owner, handle string,
	asks []sandbox.RelaunchAsk) []sandbox.RelaunchTarget {

	node := config.IncarnationNode(owner)
	question := relaunchAsk{Version: relaunchWireVersion, Owner: owner, Handle: handle}
	for _, a := range asks {
		question.Sessions = append(question.Sessions, relaunchSession{Turn: a.Turn,
			Archive: relaunchArchiveOf(a.Archive)})
	}
	raw, err := json.Marshal(question)
	if err != nil {
		return undecidedRelaunches(len(asks), fmt.Sprintf("the question for %s, which holds the seat, "+
			"could not be written: %v", node, err))
	}
	askCtx, cancel := context.WithTimeout(ctx, r.budget)
	defer cancel()
	replies, err := r.queue.Ask(askCtx, topics.HeldRelaunch, raw, 1)
	if err != nil {
		return undecidedRelaunches(len(asks), fmt.Sprintf("%s, which holds the seat, could not be asked "+
			"what a launch of its turns is launched on: %v", node, err))
	}
	for _, body := range replies {
		var reply relaunchReply
		if json.Unmarshal(body, &reply) != nil || reply.Owner != owner {
			continue
		}
		return reply.targets(node, question.Sessions)
	}
	return undecidedRelaunches(len(asks), fmt.Sprintf("%s, which holds the seat, did not say within %s "+
		"what a launch of its turns is launched on", node, r.budget))
}

// targets is the reply as the targets it names, one per session asked, each
// one's runner the holder's answer for that session's archive. A session the
// reply holds no answer for — a holder whose answer was cut short — cannot be
// said.
func (r relaunchReply) targets(node string, asked []relaunchSession) []sandbox.RelaunchTarget {
	out := make([]sandbox.RelaunchTarget, len(asked))
	for i, session := range asked {
		if i >= len(r.Targets) {
			out[i] = sandbox.RelaunchTarget{Undecided: true, Why: fmt.Sprintf("%s, which holds the seat, "+
				"did not answer for this session", node)}
			continue
		}
		t := r.Targets[i]
		out[i] = sandbox.RelaunchTarget{Harness: t.Harness, Placement: sandbox.Placement(t.Placement),
			Node: t.Node, Why: t.Why, Undecided: t.Undecided, NoKept: t.NoKept,
			Restorer: &answeredRestore{archive: session.Archive,
				answer: relaunchRestore{Restores: t.Restores, Why: t.RestoreWhy}}}
	}
	return out
}

// relaunchRestore is the holder's runner's answer for one archive.
type relaunchRestore struct {
	Restores bool
	Why      string
}

// answeredRestore is the holder's runner as its answer for the one archive a
// reader asked about with a turn ([sandbox.ArchiveRestorer]). A pointer, so a
// target that carries it compares as any other does.
type answeredRestore struct {
	archive relaunchArchive
	answer  relaunchRestore
}

var _ sandbox.ArchiveRestorer = (*answeredRestore)(nil)

// Restores is the holder's answer for archive.
func (a *answeredRestore) Restores(archive sandbox.SessionArchive) (bool, string) {
	if relaunchArchiveOf(archive) != a.archive {
		return false, "the node holding the seat was not asked whether its runner restores this session"
	}
	return a.answer.Restores, a.answer.Why
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

// answer is one question, answered from this node's own pick for each turn
// asked about where it is addressed to this process's incarnation, and with
// nothing where it is not.
func (r relaunchTargets) answer(raw []byte) ([]byte, error) {
	var question relaunchAsk
	if err := json.Unmarshal(raw, &question); err != nil {
		return nil, fmt.Errorf("engine: a relaunch target request this build cannot read: %w", err)
	}
	if question.Owner == "" || question.Owner != r.owner {
		return nil, errRelaunchNotAddressed
	}
	reply := relaunchReply{Version: relaunchWireVersion, Owner: r.owner}
	for _, session := range question.Sessions {
		target := r.local(question.Handle, session.Turn)
		restores, why := target.Restores(session.Archive.archive())
		reply.Targets = append(reply.Targets, relaunchTargetReply{Harness: target.Harness,
			Placement: string(target.Placement), Node: target.Node, Why: target.Why,
			Undecided: target.Undecided, NoKept: target.NoKept, Restores: restores, RestoreWhy: why})
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
