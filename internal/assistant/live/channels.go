package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/words"
)

// THE TWO INSTRUCTION CHANNELS. A replay hands a peer a person's frames and a
// Stop ends a person's turn, each on the asking node's word that the person
// asked, so both are signed (internal/signedask) in request and reply domains
// of their own, bound by an id and a deadline the reply names. The SERVING
// node checks the owner the request names against the turn it holds, and a
// turn that is somebody else's is answered [AnswerNotYours], never served.
//
// Scattered to every node and answered only by the node the request names
// ([ReplayRequest.Node]): the turn's row says where it runs, and only that
// node can say it is NOT running it, which is how a follower tells a runner
// that restarted from one that is slow. One request names no node: a PROBE
// ([ReplayRequest.Probe]) asks every node the newest seq it numbered for a
// turn, so a node opening the turn's next segment numbers it after all of
// them, and a client that drew frames from one node never meets a number it
// has already seen from another.

// Answer is what a serving node answered a replay or a Stop.
type Answer string

const (
	// AnswerDone is the request carried out: the frames are in the reply,
	// or the turn was stopped.
	AnswerDone Answer = "done"
	// AnswerNotRunning is a node that holds no such turn: its runner
	// restarted, or it never ran here.
	AnswerNotRunning Answer = "not_running"
	// AnswerNotYours is a turn that is not the person's the request names.
	AnswerNotYours Answer = "not_yours"
	// AnswerUnverified is a request signed under a key this node does not
	// hold, for a turn the request says runs here: nothing was carried out,
	// and Unverified names the keys it holds.
	AnswerUnverified Answer = "unverified"
)

// Answers are every [Answer].
var Answers = []Answer{AnswerDone, AnswerNotRunning, AnswerNotYours, AnswerUnverified}

// Valid reports whether a is an answer this build knows.
func (a Answer) Valid() bool { return slices.Contains(Answers, a) }

// ReplayRequest asks the node running a turn for every frame after After.
// Ids only: what the person said never crosses the broker in a request.
type ReplayRequest struct {
	V        int       `json:"v"`
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
	Node     string    `json:"node"`
	Owner    uuid.UUID `json:"owner"`
	Turn     uuid.UUID `json:"turn"`
	After    uint64    `json:"after"`
	// Probe asks for no frames: only the newest seq the answering node
	// numbered for the turn ([ReplayReply.Last]). Node is empty, and every
	// node answers.
	Probe bool `json:"probe,omitempty"`
}

// Binding is the request's id and deadline (internal/signedask).
func (r ReplayRequest) Binding() (string, time.Time) { return r.ID, r.Deadline }

// ReplayReply is the running node's answer: the page of frames, encrypted
// under [ReplayAAD] in Page, where Answer is [AnswerDone].
type ReplayReply struct {
	V       int       `json:"v"`
	Request string    `json:"request"`
	Node    string    `json:"node"`
	Turn    uuid.UUID `json:"turn"`
	Answer  Answer    `json:"answer"`
	Page    string    `json:"page,omitempty"`
	// Last is a probe's answer: the newest seq the node numbered for the
	// turn. A number, never content.
	Last       uint64 `json:"last,omitempty"`
	Unverified string `json:"unverified,omitempty"`
}

// Answers is the id of the request the reply answers (internal/signedask).
func (r ReplayReply) Answers() string { return r.Request }

// StopRequest asks the node running a turn to stop it.
type StopRequest struct {
	V        int       `json:"v"`
	ID       string    `json:"id"`
	Deadline time.Time `json:"deadline"`
	Node     string    `json:"node"`
	Owner    uuid.UUID `json:"owner"`
	Turn     uuid.UUID `json:"turn"`
}

// Binding is the request's id and deadline (internal/signedask).
func (r StopRequest) Binding() (string, time.Time) { return r.ID, r.Deadline }

// StopReply is the running node's answer: the state the turn ended in, where
// Answer is [AnswerDone]. A state and nothing a person wrote.
type StopReply struct {
	V          int            `json:"v"`
	Request    string         `json:"request"`
	Node       string         `json:"node"`
	Turn       uuid.UUID      `json:"turn"`
	Answer     Answer         `json:"answer"`
	State      wire.TurnState `json:"state,omitempty"`
	Unverified string         `json:"unverified,omitempty"`
}

// Answers is the id of the request the reply answers (internal/signedask).
func (r StopReply) Answers() string { return r.Request }

// replayPage is one replay answer's frames, the plaintext [ReplayReply.Page]
// encrypts. More is a page cut at [ReplayPageBytes], and Ended a segment
// whose last frame is in this page.
type replayPage struct {
	Frames []wire.Frame `json:"frames"`
	More   bool         `json:"more,omitempty"`
	Ended  bool         `json:"ended,omitempty"`
}

// addressed is what both channels' requests share: the binding and where
// they are addressed.
type addressed interface {
	signedask.Request
	target() (node string, turn uuid.UUID)
}

// binder is a request [ask] binds to a fresh id and its deadline.
type binder[Q addressed] interface {
	*Q
	bind(id string, deadline time.Time)
}

func (r *ReplayRequest) bind(id string, deadline time.Time) { r.ID, r.Deadline = id, deadline }
func (r *StopRequest) bind(id string, deadline time.Time)   { r.ID, r.Deadline = id, deadline }

func (r ReplayRequest) target() (string, uuid.UUID) { return r.Node, r.Turn }
func (r StopRequest) target() (string, uuid.UUID)   { return r.Node, r.Turn }

// reply is what both channels' replies share.
type reply interface {
	signedask.Reply
	from() (node string, turn uuid.UUID)
}

func (r ReplayReply) from() (string, uuid.UUID) { return r.Node, r.Turn }
func (r StopReply) from() (string, uuid.UUID)   { return r.Node, r.Turn }

// ask scatters one request and answers the first reply from the node and
// about the turn it named ([askEach]). answered is false when none did
// inside the budget.
func ask[Q addressed, R reply, PQ binder[Q]](ctx context.Context, f *Fleet,
	ch *signedask.Channel[Q, R], subject string, req PQ) (R, bool, error) {

	var got R
	answered := false
	node, _ := (*req).target()
	err := askEach(ctx, f, ch, subject, req, func(rep R) bool {
		if from, _ := rep.from(); from != node {
			return true
		}
		got, answered = rep, true
		return false
	})
	return got, answered, err
}

// askEach scatters one request bound to a fresh id and the ask's deadline,
// and hands each reply the fleet signed for that id, about the turn it named,
// to each until it answers false or the budget ends. An error is an ask that
// could not be made, which sent nothing.
func askEach[Q addressed, R reply, PQ binder[Q]](ctx context.Context, f *Fleet,
	ch *signedask.Channel[Q, R], subject string, req PQ, each func(R) bool) error {

	askCtx, cancel := context.WithTimeout(ctx, f.askBudget)
	defer cancel()
	deadline, _ := askCtx.Deadline()
	req.bind(signedask.NewID(), deadline)
	id, _ := (*req).Binding()
	_, turn := (*req).target()
	sealed, err := ch.SealRequest(*req)
	if err != nil {
		return fmt.Errorf("live: sign the request: %w", err)
	}
	err = f.broker.AskEach(askCtx, subject, sealed, func(raw []byte) bool {
		rep, why, openErr := ch.OpenReply(raw, id)
		switch {
		case openErr != nil:
			f.log.WarnContext(ctx, "assistant_fleet_reply_unread", "subject", subject,
				"turn", turn, "error", openErr)
			return true
		case why != "":
			f.log.WarnContext(ctx, "assistant_fleet_reply_refused", "subject", subject,
				"turn", turn, "detail", why)
			return true
		}
		if _, about := rep.from(); about != turn {
			return true
		}
		return each(rep)
	})
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("live: ask %s: %w", subject, err)
	}
	return nil
}

// LastSeq is the newest seq any node of the fleet numbered for owner's turn,
// and the node that holds it, zero and empty where none did: the floor a
// node opening the turn's next segment numbers after ([Hub.Open]), and where
// a stream of a turn no record names the newest segment of is answered. Every node the roster counts is asked at once,
// and the probe ends as soon as each has answered; a roster that cannot be
// read leaves it to wait out its budget for whoever answers.
func (f *Fleet) LastSeq(ctx context.Context, owner, turn uuid.UUID) (string, uint64, error) {
	nodes, err := f.roster.Nodes(ctx)
	if err != nil {
		f.log.WarnContext(ctx, "assistant_probe_roster_unread", "turn", turn, "error", err,
			"detail", "the probe waits out its budget for every node that answers")
		nodes = nil
	}
	req := ReplayRequest{V: FleetWireVersion, Owner: owner, Turn: turn, Probe: true}
	heard := map[string]bool{}
	var (
		holder string
		last   uint64
	)
	notYours := false
	err = askEach(ctx, f, f.replays, topics.AssistantReplay, &req, func(rep ReplayReply) bool {
		heard[rep.Node] = true
		switch rep.Answer {
		case AnswerDone:
			if rep.Last > last {
				holder, last = rep.Node, rep.Last
			}
		case AnswerNotYours:
			notYours = true
		}
		return len(nodes) == 0 || len(heard) < len(nodes)
	})
	switch {
	case err != nil:
		return "", 0, err
	case notYours:
		return "", 0, ErrNotYours
	}
	return holder, last, nil
}

// askReplay asks node for owner's frames after seq after, and opens the page.
func (f *Fleet) askReplay(ctx context.Context, owner, turn uuid.UUID, node string,
	after uint64) (replayPage, Reach, error) {

	req := ReplayRequest{V: FleetWireVersion, Node: node, Owner: owner, Turn: turn, After: after}
	rep, answered, err := ask(ctx, f, f.replays, topics.AssistantReplay, &req)
	if err != nil {
		return replayPage{}, "", err
	}
	if !answered {
		return replayPage{}, f.absent(ctx, node), nil
	}
	switch rep.Answer {
	case AnswerDone:
	case AnswerNotRunning:
		return replayPage{}, ReachGone, nil
	case AnswerNotYours:
		return replayPage{}, "", ErrNotYours
	case AnswerUnverified:
		return replayPage{}, "", fmt.Errorf("live: node %s could not verify the replay: %s",
			node, rep.Unverified)
	default:
		return replayPage{}, "", fmt.Errorf("live: node %s answered the replay %q, which this "+
			"build does not know", node, rep.Answer)
	}
	plain, err := f.cipher.Decrypt(rep.Page, ReplayAAD(turn, req.ID))
	if err != nil {
		return replayPage{}, "", fmt.Errorf("live: node %s's replay of turn %s does not open "+
			"under this request: %w", node, turn, err)
	}
	var page replayPage
	if err := json.Unmarshal([]byte(plain), &page); err != nil {
		return replayPage{}, "", fmt.Errorf("live: decode node %s's replay: %w", node, err)
	}
	for _, fr := range page.Frames {
		if fr.TurnID != turn {
			return replayPage{}, "", fmt.Errorf("live: node %s's replay of turn %s carries "+
				"a frame of turn %s", node, turn, fr.TurnID)
		}
		if err := fr.Validate(); err != nil {
			return replayPage{}, "", fmt.Errorf("live: node %s's replay: %w", node, err)
		}
	}
	return page, ReachServed, nil
}

// admitted opens one request this node was sent, and reports the request to
// carry out, or a reply to send without carrying anything out, or neither
// (the request is not this node's to answer, or not the fleet's).
func admitted[Q addressed, R signedask.Reply](ctx context.Context, f *Fleet, ch *signedask.Channel[Q, R],
	seen *signedask.Guard, raw []byte, unverified func(Q, string) R) (Q, *R, error) {

	var zero Q
	a := ch.Admit(raw, seen, f.now())
	switch a.Disposition {
	case signedask.Admitted:
		// A request names this node, or none: a probe every node answers.
		if node, _ := a.Request.target(); node != f.node && node != "" {
			return zero, nil, errNotThisNode
		}
		return a.Request, nil, nil
	case signedask.KeyUnknown:
		// UNTRUSTED, read for the one thing the answer needs: whether
		// the request names this node, so only that node says why it
		// took nothing and every other stays silent.
		if node, _ := a.Request.target(); node != f.node || a.ID == "" {
			return zero, nil, errNotThisNode
		}
		f.log.WarnContext(ctx, "assistant_fleet_request_refused", "disposition",
			string(a.Disposition), "detail", a.Why+"; nothing was carried out")
		rep := unverified(a.Request, fmt.Sprintf("the node running the turn does not hold "+
			"the key the request is signed under (it holds %s); add the key to its "+
			"secrets.keys", words.And(ch.HeldKeys())))
		return zero, &rep, nil
	case signedask.Tampered:
		f.log.ErrorContext(ctx, "assistant_fleet_request_refused", "disposition",
			string(a.Disposition), "detail", a.Why+"; nothing was carried out")
		return zero, nil, fmt.Errorf("live: a request the fleet did not sign")
	}
	f.log.WarnContext(ctx, "assistant_fleet_request_refused", "disposition",
		string(a.Disposition), "detail", a.Why)
	return zero, nil, fmt.Errorf("live: a request not to be taken: %s", a.Why)
}

// errNotThisNode is a request addressed to another node: answered by nobody
// here, which its asker reads as this node not being the one.
var errNotThisNode = errors.New("live: the request names another node")

// serving is the started fleet's server, or nil before [Fleet.Start].
func (f *Fleet) serving() Server {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.server
}

// answerReplay is the replay channel's answerer.
func (f *Fleet) answerReplay(ctx context.Context, raw []byte) ([]byte, error) {
	req, early, err := admitted(ctx, f, f.replays, f.replayTaken, raw,
		func(q ReplayRequest, why string) ReplayReply {
			return ReplayReply{V: FleetWireVersion, Request: q.ID, Node: f.node, Turn: q.Turn,
				Answer: AnswerUnverified, Unverified: why}
		})
	switch {
	case err != nil:
		return nil, err
	case early != nil:
		return f.replays.SealReply(*early)
	}
	out := ReplayReply{V: FleetWireVersion, Request: req.ID, Node: f.node, Turn: req.Turn}
	server := f.serving()
	if server == nil {
		return nil, errors.New("live: the fleet half is not started")
	}
	if req.Probe {
		// EVERY NODE ANSWERS A PROBE, one that knows nothing of the turn
		// included, so the asker can stop as soon as the roster has.
		last, probeErr := server.LastHere(ctx, req.Owner, req.Turn)
		switch {
		case errors.Is(probeErr, ErrNotRunning):
			out.Answer = AnswerNotRunning
		case errors.Is(probeErr, ErrNotYours):
			out.Answer = AnswerNotYours
		case probeErr != nil:
			return nil, fmt.Errorf("live: probe turn %s: %w", req.Turn, probeErr)
		default:
			out.Answer, out.Last = AnswerDone, last
		}
		return f.replays.SealReply(out)
	}
	frames, ended, err := server.ReplayHere(ctx, req.Owner, req.Turn, req.After)
	switch {
	case errors.Is(err, ErrNotRunning):
		out.Answer = AnswerNotRunning
	case errors.Is(err, ErrNotYours):
		f.log.WarnContext(ctx, "assistant_fleet_request_refused", "turn", req.Turn,
			"disposition", "not_yours", "detail", "a replay named a person the turn is not "+
				"for; nothing was sent")
		out.Answer = AnswerNotYours
	case err != nil:
		// A FAULT this node cannot answer through, answered by nobody: the
		// asker hears a runner that did not answer, which the roster
		// tells from one that is gone.
		return nil, fmt.Errorf("live: replay turn %s: %w", req.Turn, err)
	default:
		page := pageOf(frames, ended)
		plain, err := json.Marshal(page)
		if err != nil {
			return nil, fmt.Errorf("live: encode the replay of turn %s: %w", req.Turn, err)
		}
		enc, err := f.cipher.Encrypt(string(plain), ReplayAAD(req.Turn, req.ID))
		if err != nil {
			return nil, fmt.Errorf("live: encrypt the replay of turn %s: %w", req.Turn, err)
		}
		out.Answer, out.Page = AnswerDone, enc
	}
	return f.replays.SealReply(out)
}

// pageOf cuts frames to one page of at most [ReplayPageBytes] of encoded
// frames, the first frame always included, and says whether more follow.
func pageOf(frames []wire.Frame, ended bool) replayPage {
	size := 0
	for i, fr := range frames {
		encoded, err := json.Marshal(fr)
		if err == nil {
			size += len(encoded)
		}
		if i > 0 && size > ReplayPageBytes {
			return replayPage{Frames: frames[:i], More: true}
		}
	}
	return replayPage{Frames: frames, Ended: ended}
}

// answerStop is the stop channel's answerer.
func (f *Fleet) answerStop(ctx context.Context, raw []byte) ([]byte, error) {
	req, early, err := admitted(ctx, f, f.stops, f.stopTaken, raw,
		func(q StopRequest, why string) StopReply {
			return StopReply{V: FleetWireVersion, Request: q.ID, Node: f.node, Turn: q.Turn,
				Answer: AnswerUnverified, Unverified: why}
		})
	switch {
	case err != nil:
		return nil, err
	case early != nil:
		return f.stops.SealReply(*early)
	}
	out := StopReply{V: FleetWireVersion, Request: req.ID, Node: f.node, Turn: req.Turn}
	server := f.serving()
	if server == nil {
		return nil, errors.New("live: the fleet half is not started")
	}
	state, err := server.StopHere(ctx, req.Owner, req.Turn)
	switch {
	case errors.Is(err, ErrNotRunning):
		out.Answer = AnswerNotRunning
	case errors.Is(err, ErrNotYours):
		f.log.WarnContext(ctx, "assistant_fleet_request_refused", "turn", req.Turn,
			"disposition", "not_yours", "detail", "a stop named a person the turn is not "+
				"for; nothing was stopped")
		out.Answer = AnswerNotYours
	case err != nil:
		return nil, fmt.Errorf("live: stop turn %s: %w", req.Turn, err)
	default:
		out.Answer, out.State = AnswerDone, state
	}
	return f.stops.SealReply(out)
}
