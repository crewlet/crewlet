package httprelay

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/signedask"
)

// Outcome is what became of one [Relay.Forward].
type Outcome string

const (
	// Served means the holder took the request, and its response was written —
	// whole, or cut where the holder or the client went away.
	Served Outcome = "served"
	// Rerouted means the holder answered [Elsewhere]. Nothing was written.
	Rerouted Outcome = "rerouted"
	// Refused means the holder answered [Refuse]. Nothing was written.
	Refused Outcome = "refused"
	// Unanswered means no OPEN was answered and no frame arrived. Nothing was
	// written, and nothing is known to have run.
	Unanswered Outcome = "unanswered"
	// Lost means the holder took the request and stopped answering before it
	// wrote a status. Nothing was written, and the request MAY have run —
	// so it is not one to offer to another holder.
	Lost Outcome = "lost"
)

// Forward relays req to the incarnation owner, and writes its response to w.
//
// ctx is the client's request's: the exchange lasts as long as the response
// does — an event stream for as long as its client keeps it — and ends when the
// client goes. What Forward bounds itself is the OPEN, which it sends
// [openTries] times under one exchange id before it answers [Unanswered].
//
// A response the holder stops answering after its status was written is cut
// by panicking with [http.ErrAbortHandler], which is how a handler tells its
// server to drop the connection rather than end the response as though it
// were complete: a client reading a cut stream must see that it was cut.
func (rl *Relay) Forward(ctx context.Context, w http.ResponseWriter, req *Request, owner string) (Outcome, string) {
	id := signedask.NewID()
	in := &inbound{rl: rl, id: id, frames: make(chan delivery), stopped: make(chan struct{}),
		guard: signedask.NewGuard()}
	// SERVED BEFORE THE OPEN GOES OUT, so the holder's first frame — which
	// can follow its reply within a microsecond — has an answerer waiting.
	stop, err := rl.fabric.Serve(ctx, exchangeSubject(rl.name, id), in.answer)
	if err != nil {
		return Unanswered, "this node could not serve the exchange's subject: " + err.Error()
	}
	// WITHDRAWN AFTER the answers in flight are let go (stopped is closed
	// first: deferred calls run last-registered first), since the
	// withdrawal waits for them.
	defer func() { _ = stop(context.WithoutCancel(ctx)) }()
	defer close(in.stopped)

	node := config.IncarnationNode(owner)
	verdict, why := rl.open(ctx, node, owner, id, req, in)
	switch verdict {
	case Elsewhere:
		return Rerouted, why
	case Refuse:
		return Refused, why
	case "":
		// CLOSED ALL THE SAME: an OPEN that went unanswered may still have
		// started the exchange, its reply lost or the client gone before it
		// arrived, and nobody else will tell the holder that nothing reads
		// the response. Unsent, the request would run on until [Idle].
		rl.sendClose(context.WithoutCancel(ctx), node, owner, id)
		return Unanswered, why
	}
	closeIt := func() { rl.sendClose(context.WithoutCancel(ctx), node, owner, id) }
	if !in.pump(ctx, w, closeIt) {
		return Lost, "the holder took the request and did not answer for " + rl.clock.idle.String()
	}
	return Served, ""
}

// open sends one exchange's OPEN until it is answered, a frame arrives, or
// [openTries] have gone unanswered; the empty verdict is the last.
func (rl *Relay) open(ctx context.Context, node, owner, id string, req *Request, in *inbound) (Verdict, string) {
	why := "the holder answered no open in " + (openTries * rl.clock.openWait).String()
	for range openTries {
		if ctx.Err() != nil {
			return "", "the client went away"
		}
		v, answered, reason := rl.ask(ctx, node, openRequest{
			Kind: kindOpen, Owner: owner, Exchange: id, Request: req,
		})
		if answered {
			return v, reason
		}
		// A FRAME IS AN ANSWER TOO: the holder started the exchange and its
		// reply was lost.
		if in.arrived.Load() {
			return Serve, ""
		}
		if reason != "" {
			why = reason
		}
	}
	if in.arrived.Load() {
		return Serve, ""
	}
	return "", why
}

// sendClose tells the holder its exchange's client has gone, whether or not
// the holder has started it ([Relay.cancel]). Best effort: an entry that
// cannot say so leaves the holder to find out at [Idle].
func (rl *Relay) sendClose(ctx context.Context, node, owner, id string) {
	rl.ask(ctx, node, openRequest{Kind: kindClose, Owner: owner, Exchange: id})
}

// ask sends one request to a holder's node subject and waits up to [openWait]
// for its reply.
func (rl *Relay) ask(ctx context.Context, node string, q openRequest) (Verdict, bool, string) {
	q.ID, q.Deadline = signedask.NewID(), rl.now().Add(rl.clock.openWait)
	sealed, err := rl.opens.SealRequest(q)
	if err != nil {
		return "", false, "the open could not be signed: " + err.Error()
	}
	askCtx, done := context.WithTimeout(ctx, rl.clock.openWait)
	defer done()
	replies, err := rl.fabric.Ask(askCtx, nodeSubject(rl.name, node), sealed, 1)
	if err != nil {
		<-askCtx.Done()
		return "", false, "the broker refused the open: " + err.Error()
	}
	for _, raw := range replies {
		rep, why, err := rl.opens.OpenReply(raw, q.ID)
		if err != nil || !rep.Verdict.Valid() {
			if why != "" {
				log.WarnContext(ctx, "relay_reply_refused", "relay", rl.name, "node", node, "detail", why)
			}
			continue
		}
		return rep.Verdict, true, rep.Why
	}
	return "", false, ""
}

// --- the entry's half of one exchange ---------------------------------------

// inbound is one exchange an entry is relaying: the frames the holder pushes,
// each handed to the client's handler and acknowledged once it is written.
type inbound struct {
	rl    *Relay
	id    string
	guard *signedask.Guard

	// frames hands each new frame to [inbound.pump], which answers on the
	// delivery's own channel once it has written it.
	frames chan delivery
	// stopped is closed once the pump has returned: nothing more is
	// written, and every frame is answered as closed.
	stopped chan struct{}
	// arrived is set by the first frame, which is proof the holder took
	// the exchange whatever became of its reply to the OPEN.
	arrived atomic.Bool

	// mu orders the frames: a frame pushed again while its first copy is
	// still being written waits for it, and is then acknowledged unwritten.
	mu sync.Mutex
	// written is the last frame written to the client, and closed says
	// the entry wants nothing more.
	written int
	closed  bool
}

type delivery struct {
	f    frameRequest
	done chan bool
}

// answer takes one frame off the exchange's subject.
func (in *inbound) answer(ctx context.Context, raw []byte) ([]byte, error) {
	admitted := in.rl.frames.Admit(raw, in.guard, in.rl.now())
	if admitted.Disposition != signedask.Admitted {
		if admitted.Disposition == signedask.Tampered {
			log.ErrorContext(ctx, "relay_frame_refused", "relay", in.rl.name, "exchange", in.id,
				"detail", admitted.Why)
		}
		return nil, fmt.Errorf("httprelay: a frame not to be taken: %s", admitted.Why)
	}
	f := admitted.Request
	if f.Exchange != in.id {
		return nil, errors.New("httprelay: a frame for another exchange")
	}
	in.arrived.Store(true)
	in.mu.Lock()
	defer in.mu.Unlock()
	ack := func() ([]byte, error) {
		return in.rl.frames.SealReply(frameReply{Request: f.ID, Seq: f.Seq, Closed: in.closed})
	}
	switch {
	case f.Seq <= in.written:
		// A REPEAT of a frame already written, whose acknowledgement was
		// lost or late.
		return ack()
	case f.Seq != in.written+1:
		return nil, fmt.Errorf("httprelay: frame %d after %d", f.Seq, in.written)
	case in.closed:
		return ack()
	}
	d := delivery{f: f, done: make(chan bool, 1)}
	select {
	case in.frames <- d:
	case <-in.stopped:
		in.closed = true
		return ack()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case ok := <-d.done:
		in.written = f.Seq
		in.closed = in.closed || !ok
	case <-in.stopped:
		in.closed = true
	}
	return ack()
}

// pump writes the holder's frames to the client until the response ends, the
// client goes, or the holder stops answering. It reports false for a holder
// lost before it wrote a status, which leaves w untouched.
func (in *inbound) pump(ctx context.Context, w http.ResponseWriter, closeIt func()) bool {
	idle := time.NewTimer(in.rl.clock.idle)
	defer idle.Stop()
	flush := http.NewResponseController(w)
	wrote := false
	for {
		select {
		case d := <-in.frames:
			ok := true
			if d.f.Status != 0 && !wrote {
				maps.Copy(w.Header(), withoutHopByHop(d.f.Header))
				w.WriteHeader(d.f.Status)
				wrote = true
			}
			if len(d.f.Body) > 0 {
				if wrote {
					_, err := w.Write(d.f.Body)
					ok = err == nil
				} else {
					// A BODY BEFORE ITS STATUS is a holder this
					// build does not understand; nothing it says
					// can be written.
					ok = false
				}
			}
			if ok && wrote {
				// BEST EFFORT, as a server's own flush is: a client
				// that has gone fails the next write, or ends ctx.
				_ = flush.Flush()
			}
			d.done <- ok
			if !ok {
				closeIt()
				return true
			}
			if d.f.End {
				return true
			}
			idle.Reset(in.rl.clock.idle)
		case <-ctx.Done():
			closeIt()
			return true
		case <-idle.C:
			log.WarnContext(ctx, "relay_holder_lost", "relay", in.rl.name, "exchange", in.id,
				"detail", "no frame arrived for "+in.rl.clock.idle.String()+"; the response is cut")
			closeIt()
			if wrote {
				panic(http.ErrAbortHandler)
			}
			return false
		}
	}
}
