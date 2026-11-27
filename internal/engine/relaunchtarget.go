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
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// What a launch of a seat's turn would be launched on (a relaunch of a live
// turn, or a conversation's next turn) is the holder's decision
// ([sandbox.RelaunchTarget]): the member turns on that node's own bench of spent
// plans and the revision it runs. So a reader asks it: itself where it holds the
// seat, else the incarnation the seat's lease names, over a scatter only that
// incarnation answers ([topics.HeldRelaunch]), as internal/learning/memread
// reads a seat's memory. Each session asked names its turn, answered for the
// revision that turn's relaunch runs on (a resumed segment's pinned
// [resumeInput.Company], else [Engine.relaunchCompany]); a conversation's next
// turn names NO turn, as the ended turn's id could answer for a still-pinned
// revision it never runs on. AN UNKNOWN IS NEVER A NO: an unreadable lease, an
// unheld seat and a silent holder (stopped, or too old) are each undecided.

// relaunchTargetBudget bounds a reader's wait for the holder's answer: the
// fleet read budget every scatter here answers inside
// ([eventfan.FleetReadBudget]); a healthy node answers from memory in
// milliseconds.
const relaunchTargetBudget = eventfan.FleetReadBudget

// relaunchWireVersion is the request and reply shape this build speaks.
// Evolution is ADDITIVE within a version: an unknown key is dropped on decode,
// which is all a rolling upgrade needs.
const relaunchWireVersion = 1

// relaunchArchive is a session's archive as a request names it: what a runner's
// restore reads of it.
type relaunchArchive struct {
	Harness   string `json:"harness"`
	SessionID string `json:"session_id"`
	Entry     string `json:"entry,omitempty"`
	Format    int    `json:"format"`
	Version   string `json:"version"`
}

func relaunchArchiveOf(a sandbox.SessionArchive) relaunchArchive {
	return relaunchArchive{Harness: a.Harness, SessionID: a.SessionID, Entry: a.Entry, Format: a.Format,
		Version: a.Version}
}

func (a relaunchArchive) archive() sandbox.SessionArchive {
	return sandbox.SessionArchive{Harness: a.Harness, SessionID: a.SessionID, Entry: a.Entry, Format: a.Format,
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
// pick for that session's turn (Node the holder itself, since only the node a
// box was launched on continues in it), and whether the picked member's runner,
// of the holder's build, which a rolling upgrade can make another than the
// asker's, restores that session's archive.
type relaunchTargetReply struct {
	Harness   string `json:"harness,omitempty"`
	Placement string `json:"placement,omitempty"`
	Node      string `json:"node,omitempty"`
	// Entry and LaunchRouted are the picked member's
	// ([sandbox.RelaunchTarget.Entry]); an older holder's answer names
	// neither, which holds the box's copy to no entry it ran under.
	Entry        string `json:"entry,omitempty"`
	LaunchRouted bool   `json:"launch_routed,omitempty"`
	// At is the holder's clock as it answered ([sandbox.RelaunchTarget.At]).
	At          time.Time         `json:"at,omitzero"`
	Why         string            `json:"why,omitempty"`
	Undecided   bool              `json:"undecided,omitempty"`
	NoKept      string            `json:"no_kept,omitempty"`
	Restores    bool              `json:"restores,omitempty"`
	RestoreWhy  string            `json:"restore_why,omitempty"`
	RestoreLoss executorkeep.Loss `json:"restore_loss,omitempty"`
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

// RelaunchTargets is what a launch of each asked session would be launched on
// now (a relaunch of the ask's turn, or a conversation's next turn for an ask
// naming none), one target per ask in order, as the seat's holder picks it:
// this node's own pick where it holds the seat ([Engine.localRelaunchTarget]),
// else the holder's answer, asked once over the broker for every ask, with
// whether that member's runner restores the ask's archive. The read surface
// decides each row of a seat's sessions by it (internal/api/queries'
// `continuable`, [sandbox.ReadContinuityOf]). Never an error: what cannot be
// said now is a target that says so.
func (e *Engine) RelaunchTargets(ctx context.Context, seat uuid.UUID, handle string,
	asks []sandbox.RelaunchAsk) []sandbox.RelaunchTarget {

	return e.relaunchTargets().targets(ctx, seat, handle, asks)
}

// relaunchTargets is this node as it asks, and answers, what a relaunch of a
// seat's turn would be launched on.
func (e *Engine) relaunchTargets() relaunchTargets {
	r := relaunchTargets{local: e.localRelaunchTarget, budget: relaunchTargetBudget}
	if e.node == nil {
		return r
	}
	r.owner = e.node.Owner()
	// A NODE WITH NO BROKER IS THE FLEET, holding every seat: it answers
	// every question itself.
	if e.backends != nil && e.backends.Queue != nil && e.backends.Coord != nil {
		r.queue, r.leases = e.backends.Queue, e.backends.Coord
	}
	return r
}

// holderAsker scatters one question for the node holding a seat: the half of
// the broker a reader needs for what a relaunch is launched on, here, and for
// whether a park holds the seat's inbox (parkheld.go).
type holderAsker interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
}

// holderLeases reads which incarnation holds a seat: the half of the
// coordination store a reader needs, for the same two questions.
type holderLeases interface {
	Get(ctx context.Context, resource string) (*coord.Lease, error)
}

// relaunchTargets answers what a relaunch of a seat's turn would be launched
// on, from the node holding the seat: owner is this process's incarnation, as
// a seat's lease names it; local this node's own pick for a turn; a nil queue
// or leases is a node that answers every question itself.
type relaunchTargets struct {
	owner  string
	queue  holderAsker
	leases holderLeases
	local  func(handle, turn string) sandbox.RelaunchTarget
	budget time.Duration
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
	holder, err := coord.Holder(ctx, r.leases, coord.SeatResource(seat))
	switch {
	case err != nil:
		// UNKNOWN, never "held by nobody": an unreadable lease says nothing
		// about where the seat is.
		return undecidedRelaunches(len(asks), fmt.Sprintf("which node holds the seat could not be read: %v", err))
	case holder == "":
		return undecidedRelaunches(len(asks), "no node holds the seat now, and the node that takes it decides "+
			"what a launch of its turns is launched on")
	case holder == r.owner:
		return r.locals(handle, asks)
	}
	return r.ask(ctx, holder, handle, asks)
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

// targets is the reply as one target per session asked, each one's runner the
// holder's answer for that session's archive. A session the reply has no answer
// for (a holder's answer cut short) cannot be said.
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
			Node: t.Node, Entry: t.Entry, LaunchRouted: t.LaunchRouted, At: t.At, Why: t.Why, Undecided: t.Undecided,
			NoKept: t.NoKept,
			Restorer: &answeredRestore{archive: session.Archive,
				answer: sandbox.SessionContinuity{Continues: t.Restores, Loss: t.RestoreLoss, Why: t.RestoreWhy}}}
	}
	return out
}

// answeredRestore is the holder's runner as its answer for the one archive a
// reader asked about with a turn ([sandbox.ArchiveRestorer]). A pointer, so a
// target that carries it compares as any other does.
type answeredRestore struct {
	archive relaunchArchive
	answer  sandbox.SessionContinuity
}

var _ sandbox.ArchiveRestorer = (*answeredRestore)(nil)

// Restores is the holder's answer for archive.
func (a *answeredRestore) Restores(archive sandbox.SessionArchive) sandbox.SessionContinuity {
	if relaunchArchiveOf(archive) != a.archive {
		return sandbox.SessionContinuity{Loss: executorkeep.LossBoxRefused,
			Why: "the node holding the seat was not asked whether its runner restores this session"}
	}
	return a.answer
}

// armRelaunchTargets makes this node an answerer for what a relaunch of a seat
// it holds would be launched on.
//
// EVERY NODE SERVES, in every mode and whether or not it serves the API, and
// only the addressed incarnation answers: a node serving no API can hold a
// seat, and answering is not publishing. Armed after the node, whose
// incarnation a seat's lease names.
func (e *Engine) armRelaunchTargets(ctx context.Context) error {
	if e.backends == nil || e.backends.Queue == nil || e.node == nil {
		return nil
	}
	answerer := e.relaunchTargets()
	// AT THE BOUND OF WORK DONE NOW ([queue.ConcurrentAnswers]): an answer
	// reads this node's own chain and bench and waits on nothing, so no slot
	// is held through a wait.
	stop, err := e.backends.Queue.Serve(ctx, topics.HeldRelaunch, queue.ConcurrentAnswers,
		func(_ context.Context, raw []byte) ([]byte, error) {
			return answerer.answer(raw)
		})
	if err != nil {
		return fmt.Errorf("engine: answer what a relaunch of a seat's turn is launched on: %w", err)
	}
	e.stopRelaunchServe = stop
	return nil
}

// answer is one question, answered from this node's own pick for each turn
// where it is addressed to this process's incarnation, and with nothing where
// it is not.
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
		restores := target.Restores(session.Archive.archive())
		reply.Targets = append(reply.Targets, relaunchTargetReply{Harness: target.Harness,
			Placement: string(target.Placement), Node: target.Node, Entry: target.Entry,
			LaunchRouted: target.LaunchRouted, At: target.At, Why: target.Why,
			Undecided: target.Undecided, NoKept: target.NoKept, Restores: restores.Continues,
			RestoreWhy: restores.Why, RestoreLoss: restores.Loss})
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
