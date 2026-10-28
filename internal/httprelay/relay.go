package httprelay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("httprelay")

// Fabric is the half of the queue a relay needs: its ephemeral request and
// reply, which retains nothing — a relayed request is worth nothing once its
// client has gone.
type Fabric interface {
	Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error)
	Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error)
}

// Options configure [New].
type Options struct {
	// Fabric is the broker every node of the fleet reaches.
	Fabric Fabric

	// Name scopes the relay: its subjects are `crewlet.<name>.…` and its
	// two signed channels `<name>.open` and `<name>.frame`, so two relays
	// in one fleet never answer each other's exchanges. A single subject
	// token.
	Name string

	// Node is this node's id, the subject an entry addresses it by, and
	// Owner this process's incarnation, which an OPEN must name.
	Node  string
	Owner string

	// Keyring is the fleet's, every message is signed under.
	Keyring statelog.Keyring

	// Admit is what this process can do for a request addressed to it,
	// asked before its handler runs: [Serve] runs it, and the other two
	// are answered to the entry as they are.
	Admit func(r *http.Request) Verdict

	// Handler answers an admitted request, exactly as it would one handed
	// to it directly.
	Handler http.Handler

	// Now is the clock. Nil takes wall-clock time.
	Now func() time.Time
}

// Relay is one process's end of a relay: the holder of what it serves, and the
// entry of what it forwards.
type Relay struct {
	fabric  Fabric
	name    string
	node    string
	owner   string
	admit   func(*http.Request) Verdict
	handler http.Handler
	now     func() time.Time
	clock   clocks

	opens  *signedask.Channel[openRequest, openReply]
	frames *signedask.Channel[frameRequest, frameReply]
	taken  *signedask.Guard

	mu sync.Mutex
	// served is every exchange this process is answering, by id; ended is
	// every exchange it answered and has finished with, until an OPEN
	// repeated for it can no longer arrive — so a repeat is answered and
	// never run twice.
	served map[string]*exchange
	ended  map[string]time.Time
	// serving is the context every exchange runs under, cancelled when the
	// relay stops serving. Nil until [Relay.Serve].
	serving context.Context
}

// New builds a relay, or refuses options that could not make one.
func New(opts Options) (*Relay, error) {
	switch {
	case opts.Fabric == nil:
		return nil, errors.New("httprelay: a relay needs the broker it rides")
	case opts.Name == "" || opts.Node == "" || opts.Owner == "":
		return nil, errors.New("httprelay: a relay needs its name, its node and its incarnation")
	case opts.Admit == nil || opts.Handler == nil:
		return nil, errors.New("httprelay: a relay needs what admits a request and what answers it")
	}
	opens, err := signedask.New[openRequest, openReply](opts.Name+".open", opts.Keyring)
	if err != nil {
		return nil, fmt.Errorf("httprelay: %w", err)
	}
	frames, err := signedask.New[frameRequest, frameReply](opts.Name+".frame", opts.Keyring)
	if err != nil {
		return nil, fmt.Errorf("httprelay: %w", err)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Relay{
		fabric: opts.Fabric, name: opts.Name, node: opts.Node, owner: opts.Owner,
		admit: opts.Admit, handler: opts.Handler, now: now, clock: defaultClocks,
		opens: opens, frames: frames, taken: signedask.NewGuard(),
		served: map[string]*exchange{}, ended: map[string]time.Time{},
	}, nil
}

// Owner is the incarnation this relay answers for.
func (rl *Relay) Owner() string { return rl.owner }

// Domains is the four key domains this relay signs under — its OPEN's request
// and reply, and its FRAME's — for the gate that holds every signed channel's
// domains apart from every other's and from every state log's.
func (rl *Relay) Domains() []string {
	openRequest, openReply := rl.opens.Domains()
	frameRequest, frameReply := rl.frames.Domains()
	return []string{openRequest, openReply, frameRequest, frameReply}
}

// Serve makes this process a holder: it answers OPEN and CLOSE on its node's
// subject until the returned Unsubscribe runs, which also cancels every
// exchange it is answering.
func (rl *Relay) Serve(ctx context.Context) (queue.Unsubscribe, error) {
	serving, stopAll := context.WithCancel(context.WithoutCancel(ctx))
	rl.mu.Lock()
	rl.serving = serving
	rl.mu.Unlock()
	stop, err := rl.fabric.Serve(ctx, nodeSubject(rl.name, rl.node), rl.answer)
	if err != nil {
		stopAll()
		return nil, fmt.Errorf("httprelay: serve %s: %w", rl.node, err)
	}
	return func(ctx context.Context) error {
		err := stop(ctx)
		stopAll()
		return err
	}, nil
}

// answer is the holder's side of the node subject.
func (rl *Relay) answer(ctx context.Context, raw []byte) ([]byte, error) {
	admitted := rl.opens.Admit(raw, rl.taken, rl.now())
	switch admitted.Disposition {
	case signedask.Admitted:
	case signedask.KeyUnknown:
		// THIS NODE LACKS THE KEY, which is an operator's to fix: the
		// entry is told so, and finds the holder again rather than
		// waiting out a silence.
		if admitted.ID == "" {
			return nil, errors.New("httprelay: an open under a key this node does not hold, naming no id")
		}
		return rl.opens.SealReply(openReply{Request: admitted.ID, Verdict: Elsewhere, Why: admitted.Why})
	case signedask.Tampered:
		log.ErrorContext(ctx, "relay_request_refused", "relay", rl.name,
			"disposition", string(admitted.Disposition), "detail", admitted.Why)
		return nil, errors.New("httprelay: a request the fleet did not sign")
	default:
		return nil, fmt.Errorf("httprelay: a request not to be taken: %s", admitted.Why)
	}
	q := admitted.Request
	reply := func(v Verdict, why string) ([]byte, error) {
		return rl.opens.SealReply(openReply{Request: q.ID, Verdict: v, Why: why})
	}
	switch q.Kind {
	case kindClose:
		rl.cancel(q.Exchange)
		return reply(Serve, "")
	case kindOpen:
	default:
		return nil, fmt.Errorf("httprelay: an unknown request kind %q", q.Kind)
	}
	if q.Owner != rl.owner {
		return reply(Elsewhere, "this node is a different incarnation from the one the asker named")
	}
	if q.Request == nil || q.Exchange == "" {
		return nil, errors.New("httprelay: an open carrying no request or no exchange")
	}
	if rl.known(q.Exchange) {
		// A REPEATED OPEN — the first one's reply was lost — is answered
		// as the first was, and the exchange is not started twice.
		return reply(Serve, "")
	}
	// THE EXCHANGE OUTLIVES THIS ANSWER: the OPEN is answered as soon as
	// the exchange starts, and its request runs on, under the relay's own
	// lifetime, for as long as the response does.
	req, cancel, err := rl.rebuild(q.Request) //nolint:contextcheck // the request's context is the exchange's, not this answer's
	if err != nil {
		return nil, err
	}
	if v := rl.admit(req); v != Serve {
		cancel()
		if !v.Valid() {
			v = Elsewhere
		}
		return reply(v, "")
	}
	if !rl.start(q.Exchange, req, cancel) {
		// STARTED BY A REPEAT that raced this one, or the relay stopped.
		cancel()
	}
	return reply(Serve, "")
}

// known reports whether an exchange is being answered or was answered lately.
func (rl *Relay) known(id string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.forgetLocked()
	_, live := rl.served[id]
	_, done := rl.ended[id]
	return live || done
}

// forgetLocked drops the finished exchanges no repeated OPEN can still name:
// an entry repeats its OPEN for [openTries] × [openWait] at most, and a
// finished exchange is remembered for [Idle], far past that.
func (rl *Relay) forgetLocked() {
	cutoff := rl.now().Add(-rl.clock.idle)
	for id, at := range rl.ended {
		if at.Before(cutoff) {
			delete(rl.ended, id)
		}
	}
}

// rebuild is the request a holder's handler is handed: the entry's, on a
// context the exchange owns.
func (rl *Relay) rebuild(in *Request) (*http.Request, context.CancelFunc, error) {
	rl.mu.Lock()
	base := rl.serving
	rl.mu.Unlock()
	if base == nil {
		return nil, nil, errors.New("httprelay: an open reached a relay that is not serving")
	}
	ctx, cancel := context.WithCancel(base)
	req, err := http.NewRequestWithContext(ctx, in.Method, in.URI, bytesReader(in.Body))
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("httprelay: rebuild the request: %w", err)
	}
	req.Header = in.Header.Clone()
	if req.Header == nil {
		req.Header = http.Header{}
	}
	req.Host, req.RemoteAddr = in.Host, in.RemoteAddr
	req.RequestURI = in.URI
	req.ContentLength = int64(len(in.Body))
	return req, cancel, nil
}

// cancel ends one exchange this process is answering, at its entry's word.
func (rl *Relay) cancel(id string) {
	rl.mu.Lock()
	ex := rl.served[id]
	rl.mu.Unlock()
	if ex != nil {
		ex.cancel()
	}
}

// --- the holder's half of one exchange --------------------------------------

// exchange is one request a holder is answering.
type exchange struct {
	id     string
	w      *writer
	cancel context.CancelFunc
}

// start runs one admitted request and pushes its response. It reports false
// when the exchange was already started.
func (rl *Relay) start(id string, req *http.Request, cancel context.CancelFunc) bool {
	ex := &exchange{id: id, w: newWriter(req.Context()), cancel: cancel}
	rl.mu.Lock()
	if _, dup := rl.served[id]; dup {
		rl.mu.Unlock()
		return false
	}
	rl.served[id] = ex
	rl.mu.Unlock()

	go func() { //nolint:contextcheck // the handler runs under the exchange's own context, which outlives the OPEN that started it
		defer ex.w.finish()
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			// A HANDLER THAT ABORTS ITS RESPONSE ON PURPOSE is a cut
			// response, as a server reads it, and not a defect.
			if err, ok := r.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				return
			}
			log.ErrorContext(req.Context(), "relay_handler_panicked", "relay", rl.name,
				"exchange", id, "panic", r)
			ex.w.failed()
		}()
		rl.handler.ServeHTTP(ex.w, req)
	}()
	go rl.push(req.Context(), ex)
	return true
}

// push sends one exchange's response to its entry, frame by frame, each
// pushed until it is acknowledged.
func (rl *Relay) push(ctx context.Context, ex *exchange) {
	defer rl.finish(ex)
	subject := exchangeSubject(rl.name, ex.id)
	heard := rl.now()
	sentHeader := false
	for seq := 1; ; seq++ {
		f, ok := ex.w.next(ctx, rl.clock.heartbeat, !sentHeader)
		if !ok {
			return
		}
		f.Exchange, f.Seq = ex.id, seq
		var ack frameReply
		for {
			var answered bool
			ack, answered = rl.pushOnce(ctx, subject, f)
			if answered {
				heard = rl.now()
				break
			}
			if ctx.Err() != nil {
				return
			}
			if rl.now().Sub(heard) > rl.clock.idle {
				// THE ENTRY IS GONE — a node down, a partition — and
				// nothing will read the rest of this response.
				log.WarnContext(ctx, "relay_entry_lost", "relay", rl.name, "exchange", ex.id,
					"detail", "no frame was acknowledged for "+rl.clock.idle.String()+
						"; the request is cancelled")
				return
			}
		}
		if ack.Closed {
			return
		}
		ex.w.sent(len(f.Body))
		if f.Status != 0 {
			sentHeader = true
		}
		if f.End {
			return
		}
	}
}

// pushOnce pushes one frame and waits up to [pushWait] for its
// acknowledgement.
func (rl *Relay) pushOnce(ctx context.Context, subject string, f frameRequest) (frameReply, bool) {
	f.ID, f.Deadline = signedask.NewID(), rl.now().Add(rl.clock.pushWait)
	sealed, err := rl.frames.SealRequest(f)
	if err != nil {
		log.ErrorContext(ctx, "relay_frame_unsealed", "relay", rl.name, "error", err.Error())
		return frameReply{}, false
	}
	askCtx, done := context.WithTimeout(ctx, rl.clock.pushWait)
	defer done()
	replies, err := rl.fabric.Ask(askCtx, subject, sealed, 1)
	if err != nil {
		// THE BROKER, not the entry: waited out here so a broker that is
		// down costs this exchange its idle bound rather than a spin.
		<-askCtx.Done()
		return frameReply{}, false
	}
	for _, raw := range replies {
		ack, _, err := rl.frames.OpenReply(raw, f.ID)
		if err == nil && ack.Seq == f.Seq {
			return ack, true
		}
	}
	return frameReply{}, false
}

// finish retires an exchange: its request cancelled, and its id remembered
// until a repeated OPEN for it can no longer arrive.
func (rl *Relay) finish(ex *exchange) {
	ex.cancel()
	rl.mu.Lock()
	delete(rl.served, ex.id)
	rl.ended[ex.id] = rl.now()
	rl.mu.Unlock()
}
