package memread

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

var log = logging.Get("learning.memread")

// HolderNone is `held_by` for a seat no node holds.
const HolderNone = "none"

// DefaultBudget is how long a read waits for the holder to answer.
//
// TWO SECONDS, the fleet read budget every other scatter in this engine is
// held to (eventfan's FleetReadBudget, the steer's ask): a round trip on the
// broker plus a handful of indexed reads against the holder's own store,
// which a healthy node answers in milliseconds. Past it the read says the
// holder did not answer rather than keep a screen waiting on a node that may
// be gone.
const DefaultBudget = 2 * time.Second

// WireVersion is the request and reply shape this build speaks.
//
// Evolution is ADDITIVE within a version, which is all a rolling upgrade
// needs: a reply carrying a key this build does not know decodes with it
// dropped, and a request carrying one is answered without it.
const WireVersion = 1

// ErrUnavailable is a read this node understood and could not answer now: the
// lease could not be read, the holder did not answer, or the seat is still
// arriving on the node that holds it. It clears by waiting, which is what a
// caller mapping it to a retry needs to know.
var ErrUnavailable = errors.New("memread: the seat's holder could not answer")

// Question is what one read asks.
type Question string

const (
	// QuestionMemory is a seat's memory: [Memory].
	QuestionMemory Question = "memory"
	// QuestionThreads is a seat's conversation ledger: [Threads].
	QuestionThreads Question = "threads"
)

// Valid reports whether this build can answer q.
func (q Question) Valid() bool { return q == QuestionMemory || q == QuestionThreads }

// Leases reads one seat's lease — the half of [coord.Backend] a read needs.
type Leases interface {
	Get(ctx context.Context, resource string) (*coord.Lease, error)
}

// Asker scatters one read — the half of [queue.EventQueue] a read needs.
type Asker interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
}

// Features says what one incarnation's build can do — the half of
// [coord.FeatureReader] a read needs.
type Features interface {
	OwnerFeature(ctx context.Context, owner string, feature coord.Feature) (bool, error)
}

// Server makes a process one of a subject's answerers.
type Server interface {
	Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// Reader answers a seat's memory from the node that holds the seat.
type Reader struct {
	// Owner is this process's lease owner — its incarnation — which is
	// what a seat's lease names and what a request is addressed to.
	Owner string

	// Local is this node's own stores.
	Local *Stores

	// Queue asks a peer. Nil is a node with no broker, which is the whole
	// fleet: its store is the only copy there is, and it answers every
	// read itself.
	Queue Asker

	// Leases reads which incarnation holds a seat. Required with Queue.
	Leases Leases

	// Features says whether the holding incarnation's build answers this
	// read at all ([coord.FeatureHeldRead]). Required with Queue: during a
	// rolling upgrade a seat can be held by a build that serves no such
	// subject, and asking it anyway waits out the whole [DefaultBudget] on
	// every poll for a reply that can never come — and then says the holder
	// "did not answer", which reads as a node in trouble rather than one
	// on an older build.
	Features Features

	// Attached is the seats this node has taken ALL the way — hydrated and
	// consuming — which is when its copy of a seat's memory is current.
	// Required with Queue.
	Attached func() []string

	// Budget bounds a peer's answer; zero is [DefaultBudget].
	Budget time.Duration

	// Now judges a lease's liveness; nil is the wall clock.
	Now func() time.Time
}

func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Reader) budget() time.Duration {
	if r.Budget > 0 {
		return r.Budget
	}
	return DefaultBudget
}

// Memory is one seat's memory, answered by its holder.
func (r *Reader) Memory(ctx context.Context, handle string, limit int) (Memory, error) {
	req := request{Version: WireVersion, Question: QuestionMemory, Handle: handle, Limit: limit}
	var out Memory
	holder, err := r.read(ctx, req, &out, func(ctx context.Context) (any, error) {
		return r.Local.Memory(ctx, handle, limit)
	})
	if err != nil {
		return Memory{}, err
	}
	if holder == "" {
		return emptyMemory(handle, HolderNone), nil
	}
	out.HeldBy = holder
	return out, nil
}

// Threads is one seat's conversation ledger, answered by its holder.
func (r *Reader) Threads(ctx context.Context, handle, conversation string, limit int) (Threads, error) {
	req := request{
		Version: WireVersion, Question: QuestionThreads, Handle: handle, Limit: limit,
		Conversation: conversation,
	}
	var out Threads
	holder, err := r.read(ctx, req, &out, func(ctx context.Context) (any, error) {
		return r.Local.Threads(ctx, handle, conversation, limit)
	})
	if err != nil {
		return Threads{}, err
	}
	if holder == "" {
		return emptyThreads(handle, HolderNone), nil
	}
	out.HeldBy = holder
	return out, nil
}

// read routes one question to the seat's holder and decodes its answer into
// out, returning the holder's NODE id — or "" when no node holds the seat,
// with out untouched.
func (r *Reader) read(ctx context.Context, req request, out any,
	local func(context.Context) (any, error),
) (string, error) {
	answerHere := func() (string, error) {
		answer, err := local(ctx)
		if err != nil {
			return "", err
		}
		// THROUGH THE SAME ENCODING a peer's answer takes, so a local
		// answer and a remote one are one shape by construction.
		body, err := json.Marshal(answer)
		if err != nil {
			return "", fmt.Errorf("memread: encode an answer: %w", err)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return "", fmt.Errorf("memread: decode an answer: %w", err)
		}
		return NodeOf(r.Owner), nil
	}
	if r.Queue == nil {
		return answerHere()
	}
	lease, err := r.Leases.Get(ctx, coord.SeatResource(req.Handle))
	if err != nil {
		// UNKNOWN, never "held by nobody": an unreadable lease table says
		// nothing about where the seat is.
		return "", fmt.Errorf("%w: which node holds %s could not be read: %w",
			ErrUnavailable, req.Handle, err)
	}
	if lease == nil || !lease.Live(r.now()) {
		return "", nil
	}
	if lease.Owner == r.Owner {
		if !slices.Contains(r.Attached(), req.Handle) {
			return "", fmt.Errorf("%w: this node is taking %s and its memory is still "+
				"arriving — ask again in a moment", ErrUnavailable, req.Handle)
		}
		return answerHere()
	}
	return r.ask(ctx, req, lease.Owner, out)
}

// ask puts one read to the incarnation holding the seat — once its build is
// known to answer one.
//
// THE HOLDER'S OWN BUILD IS ASKED ABOUT, BY OWNER: the request goes to the
// incarnation this lease read named, so a second read of the lease could name a
// different process than the one asked. Three answers, three outcomes: it
// answers (ask), it definitely cannot (unavailable now, naming its build), and
// nothing says (unavailable, retry) — the last never read as either of the
// others, because a heartbeat blip is not an upgrade.
func (r *Reader) ask(ctx context.Context, req request, owner string, out any) (string, error) {
	node := NodeOf(owner)
	answers, err := r.Features.OwnerFeature(ctx, owner, coord.FeatureHeldRead)
	if err != nil {
		return "", fmt.Errorf("%w: whether %s, which holds %s, can answer could not be read: %w",
			ErrUnavailable, node, req.Handle, err)
	}
	if !answers {
		return "", fmt.Errorf("%w: %s holds %s and runs an older build that cannot answer a "+
			"read of a seat's memory — it can once that node is upgraded",
			ErrUnavailable, node, req.Handle)
	}
	req.Owner = owner
	raw, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("memread: encode a request: %w", err)
	}
	askCtx, cancel := context.WithTimeout(ctx, r.budget())
	defer cancel()
	replies, err := r.Queue.Ask(askCtx, topics.HeldRead, raw, 1)
	if err != nil {
		return "", fmt.Errorf("%w: %s could not be asked: %w", ErrUnavailable, node, err)
	}
	for _, body := range replies {
		var rep reply
		if json.Unmarshal(body, &rep) != nil || rep.Owner != owner {
			continue
		}
		if rep.Error != "" {
			return "", fmt.Errorf("%w: %s holds %s and answered: %s",
				ErrUnavailable, node, req.Handle, rep.Error)
		}
		if err := json.Unmarshal(rep.Answer, out); err != nil {
			return "", fmt.Errorf("memread: %s's answer about %s could not be read: %w",
				node, req.Handle, err)
		}
		return node, nil
	}
	return "", fmt.Errorf("%w: %s holds %s and did not answer within %s",
		ErrUnavailable, node, req.Handle, r.budget())
}

// NodeOf is the node an incarnation belongs to: the stable id before the
// incarnation's own suffix (config.NewIncarnation writes `<node>:<uuid>`),
// which is what a reader knows the node by.
func NodeOf(owner string) string {
	node, _, _ := strings.Cut(owner, ":")
	return node
}

// request is one read, addressed to the incarnation the asker's lease read
// named.
type request struct {
	Version      int      `json:"v"`
	Question     Question `json:"q"`
	Handle       string   `json:"handle"`
	Owner        string   `json:"owner"`
	Limit        int      `json:"limit,omitempty"`
	Conversation string   `json:"conversation,omitempty"`
}

// reply is the addressed incarnation's answer, or why it could not give one.
// It names its owner because a scatter's replies carry no sender.
type reply struct {
	Version int             `json:"v"`
	Owner   string          `json:"owner"`
	Answer  json.RawMessage `json:"answer,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// errNotAddressed is a request for another incarnation. The serve answers
// nothing for it: the asker is waiting on the one it named.
var errNotAddressed = errors.New("memread: the read is addressed to another incarnation")

// Serve makes this node an answerer for reads about the seats it holds.
//
// EVERY NODE SERVES and only the addressed incarnation answers. One that is
// addressed and has not attached the seat says so rather than answering from a
// copy that is still arriving — or that is left over from a tenure that ended.
func Serve(ctx context.Context, q Server, owner string, attached func() []string,
	local *Stores,
) (queue.Unsubscribe, error) {
	return q.Serve(ctx, topics.HeldRead, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req request
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("memread: a request this build cannot read: %w", err)
		}
		if req.Owner != owner {
			return nil, errNotAddressed
		}
		refuse := func(why string) ([]byte, error) {
			return json.Marshal(reply{Version: WireVersion, Owner: owner, Error: why})
		}
		if !req.Question.Valid() {
			return refuse(fmt.Sprintf("this node's build cannot answer a %q read", req.Question))
		}
		if !slices.Contains(attached(), req.Handle) {
			return refuse("this node does not have " + req.Handle + " attached — it is " +
				"taking the seat or letting it go")
		}
		var (
			answer any
			err    error
		)
		switch req.Question {
		case QuestionMemory:
			answer, err = local.Memory(ctx, req.Handle, req.Limit)
		case QuestionThreads:
			answer, err = local.Threads(ctx, req.Handle, req.Conversation, req.Limit)
		}
		if err != nil {
			log.WarnContext(ctx, "held_read_failed", "question", string(req.Question),
				"seat", req.Handle, "error", err)
			return refuse("its store could not be read: " + err.Error())
		}
		body, err := json.Marshal(answer)
		if err != nil {
			return nil, fmt.Errorf("memread: encode an answer: %w", err)
		}
		// A REPLY THE TRANSPORT CANNOT CARRY IS NOT SENT AT ALL, and the
		// asker would read the broker's refusal as a holder that never
		// answered. Every collection is paged, so this is a guard against
		// a bug rather than a size a seat reaches — and it says so.
		out, err := json.Marshal(reply{Version: WireVersion, Owner: owner, Answer: body})
		if err != nil {
			return nil, fmt.Errorf("memread: encode a reply: %w", err)
		}
		if len(out) > queue.MaxPayloadBytes {
			return refuse(fmt.Sprintf("its answer is %d bytes, past the transport's %d",
				len(out), queue.MaxPayloadBytes))
		}
		return out, nil
	})
}
