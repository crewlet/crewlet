// Package jsapitest stands between a stream's LEADER READS and the broker
// that answers them, so a case can make the broker do what a re-electing
// group and a backed-up API queue do to them: drop a read without a word, and
// answer one late.
//
// # Why at the broker, and not a seam in the reader
//
// Because what a case has to hold is that every read only a stream's leader
// answers — coordination's single-key reads, a certified listing's index and
// its certifying reads, the lease gates' stream reads, the object store's
// metadata reads, the write fence's floor read, and a consumer's info, which
// only the consumer's own leader answers — reaches the broker through
// the one hedged read in [jsprovision.ReadWith], and a seam in the reader is
// exactly the thing that would let a caller build its own. A broker that drops
// the request proves it for any reader, whatever it was built with.
//
// The one seam there is sits BELOW the hedge, for what no broker can give: a
// [jsapi.Meta] puts its pages on a client that answers requests itself, so a
// case can answer them inside a synctest bubble, where a claim about the
// clock — the bound a page was given, the instant a listing ended — is exact,
// and where no broker's reply can reach a request. Every term, bound and
// re-ask above it is the reader's own, and a case answering through it holds
// that the pages it asked about reached it; whether a read is asked again
// across a broker that drops it is proved here.
//
// # How
//
// A subject MAPPING in the account: every request on one leader read's API
// subject — [MsgGet], [StreamInfo] or [ConsumerInfo] of a stream, or one of
// the metadata leader's: [StreamNames], [ConsumerList], [AccountInfo] — is
// mapped to a subject only this package listens on, so it reaches the case
// rather than JetStream: held, or never answered at all. Releasing removes the mapping, and every later
// read reaches the stream's leader again; a held read is then sent on to the
// leader with its own reply subject, which is a reply arriving late to the
// request that asked.
//
// A consumer's CREATION is held the same way ([ConsumerCreate]): the broker
// answers it only once it has found where the consumer begins, which for a
// listing of every subject's newest message means reading the stream's blocks.
// And so are a consumer's PULLS ([ConsumerNext]), which a cluster drops for a
// consumer just made as it drops a leader read.
//
// It intercepts the ACCOUNT's own API (`$JS.API.>`) of a server with no
// JetStream domain. A domain's API reaches JetStream through mappings of its
// own, which the broker consults before any added after them, so a case reads
// through [jsapi.Account] on a server it started without one.
package jsapitest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// MsgGet is the account API subject a read of one of stream's messages is
// asked on ([jsapi.Leader]).
func MsgGet(stream string) string { return fmt.Sprintf(server.JSApiMsgGetT, stream) }

// StreamInfo is the account API subject stream's info is asked on.
func StreamInfo(stream string) string { return fmt.Sprintf(server.JSApiStreamInfoT, stream) }

// ConsumerInfo is the account API subject the info of stream's consumer is
// asked on — which only the consumer's own leader answers, as only a stream's
// leader answers its info.
func ConsumerInfo(stream, consumer string) string {
	return fmt.Sprintf(server.JSApiConsumerInfoT, stream, consumer)
}

// StreamNames is the account API subject the stream names are listed on —
// which only the metadata leader answers ([jsapi.Meta]).
func StreamNames() string { return server.JSApiStreams }

// ConsumerList is the account API subject stream's consumers are listed on —
// which only the metadata leader answers ([jsapi.Meta]).
func ConsumerList(stream string) string { return fmt.Sprintf(server.JSApiConsumerListT, stream) }

// AccountInfo is the account API subject the account's JetStream limits are
// read on — which only the metadata leader answers.
func AccountInfo() string { return server.JSApiAccountInfo }

// ConsumerCreate is the account API subjects a consumer of stream is made on
// under a name, whatever its filter, as the client names every consumer it
// makes: answered once the consumer's leader has found where it begins.
func ConsumerCreate(stream string) string {
	return fmt.Sprintf(server.JSApiConsumerCreateT, stream) + "." + fullWildcard
}

// ConsumerNext is the account API subjects every consumer of stream is pulled
// on, whatever its name: the request for its next messages, answered by the
// consumer's own server with deliveries to the request's reply subject.
func ConsumerNext(stream string) string {
	return fmt.Sprintf(server.JSApiRequestNextT, stream, fullWildcard)
}

// fullWildcard is the last token of a subject that stands for every subject
// with the tokens before it ([ConsumerCreate], [ConsumerNext]).
const fullWildcard = ">"

// heldSeq makes every interception's subject its own, so two in one process
// never hear each other's reads.
var heldSeq atomic.Uint64

// Read is one leader read the broker handed to the case rather than to the
// stream's leader.
type Read struct {
	// N is the read's place among those this interception held, from 1.
	N   int
	msg *nats.Msg
	h   *Held
	// asked is the subject the read was asked on, which [Read.Answer] sends
	// it on to: the interception's own, or one it stands for ([ConsumerCreate]).
	asked string
}

// Reply is the subject the reader listens on for this read's answer — what
// tells one request of a read from another.
func (r *Read) Reply() string { return r.msg.Reply }

// Answer sends the read on to the stream's leader, which answers the reply
// subject the reader is listening on — late, by however long the case held
// it. Only after [Held.Release]: before it, the broker would map the read
// straight back here.
func (r *Read) Answer() error {
	r.h.mu.Lock()
	released := r.h.released
	r.h.mu.Unlock()
	if !released {
		return fmt.Errorf("jsapitest: read %d answered while reads on %s are still held — "+
			"release them first, or the broker maps it straight back", r.N, r.h.subject)
	}
	return r.h.nc.PublishMsg(&nats.Msg{Subject: r.asked, Reply: r.msg.Reply,
		Header: r.msg.Header, Data: r.msg.Data})
}

// Held is an interception of the leader reads asked on one subject.
type Held struct {
	acc     *server.Account
	nc      *nats.Conn
	subject string
	reads   chan *Read

	// done ends with the case, and with it every goroutine this
	// interception started.
	done context.Context

	mu       sync.Mutex
	released bool
	n        int
}

// Hold maps every leader read asked on subject — one of this package's API
// subjects — by a client of srv's global account to the case, until
// [Held.Release] or the case ends.
//
// srv must have no JetStream domain — see the package doc. signIn is how the
// interception's own connection authenticates to srv, where srv requires it
// (a token, say).
func Hold(t testing.TB, srv *server.Server, subject string, signIn ...nats.Option) *Held {
	t.Helper()
	if cfg := srv.JetStreamConfig(); cfg != nil && cfg.Domain != "" {
		t.Fatalf("jsapitest: the server serves the JetStream domain %q, whose own "+
			"mappings the broker consults first — start it without one", cfg.Domain)
	}
	nc, err := nats.Connect("", append([]nats.Option{nats.InProcessServer(srv)}, signIn...)...)
	if err != nil {
		t.Fatalf("jsapitest: connect the interception: %v", err)
	}
	done, end := context.WithCancel(context.Background())
	h := &Held{
		acc:     srv.GlobalAccount(),
		nc:      nc,
		subject: subject,
		reads:   make(chan *Read, 64),
		done:    done,
	}
	held := fmt.Sprintf("jsapitest.held.%d", heldSeq.Add(1))
	// A FULL WILDCARD is mapped onto one of the interception's own, since a
	// mapping keeps the tokens it stands for, and those tokens name the
	// subject each request was asked on.
	prefix, wild := strings.CutSuffix(subject, "."+fullWildcard)
	dest := held
	if wild {
		dest = held + "." + fullWildcard
	}
	if _, err := nc.Subscribe(dest, func(msg *nats.Msg) {
		asked := subject
		if wild {
			asked = prefix + strings.TrimPrefix(msg.Subject, held)
		}
		h.mu.Lock()
		h.n++
		r := &Read{N: h.n, msg: msg, h: h, asked: asked}
		h.mu.Unlock()
		select {
		case h.reads <- r:
		default:
			t.Errorf("jsapitest: more than %d leader reads on %s held and not "+
				"taken by the case", cap(h.reads), subject)
		}
	}); err != nil {
		end()
		nc.Close()
		t.Fatalf("jsapitest: listen for the held reads: %v", err)
	}
	if err := nc.Flush(); err != nil {
		end()
		nc.Close()
		t.Fatalf("jsapitest: flush the interception's subscription: %v", err)
	}
	if err := h.acc.AddMapping(subject, dest); err != nil {
		end()
		nc.Close()
		t.Fatalf("jsapitest: map the leader reads on %s: %v", subject, err)
	}
	t.Cleanup(func() {
		end()
		h.Release()
		nc.Close()
	})
	return h
}

// Next is the next leader read the broker held, or ctx's error.
func (h *Held) Next(ctx context.Context) (*Read, error) {
	select {
	case r := <-h.reads:
		return r, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("jsapitest: no leader read on %s was held: %w", h.subject, ctx.Err())
	}
}

// Release removes the interception: every leader read sent after it reaches
// the stream's leader, and a read held before it may be [Read.Answer]ed.
func (h *Held) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.released {
		return
	}
	h.released = true
	h.acc.RemoveMapping(h.subject)
}

// Count is how many leader reads the broker has held so far.
func (h *Held) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// Fence is how many leader reads the broker held, and the case had not yet
// taken, among those nc sent before Fence was called — every one of them,
// however late the broker hands it over, which [Held.Count] read straight
// after the read that sent them cannot promise.
//
// A FENCE, through the reads' own path: a request of Fence's own, published
// on nc to the held subject, reaches the interception after every message nc
// published before it, since a subscriber hears one connection's messages in
// the order they were published, and Fence takes each read until the fence.
// So nc must be the connection the reads were sent on, and the reads must
// still be held: a fence sent after [Held.Release] reaches the stream's
// leader instead, and Fence waits for ctx. The fence is counted by
// [Held.Count] and taken here.
func (h *Held) Fence(ctx context.Context, nc *nats.Conn) (int, error) {
	fence := fmt.Sprintf("jsapitest.fence.%d", heldSeq.Add(1))
	if err := nc.PublishRequest(h.subject, fence, nil); err != nil {
		return 0, fmt.Errorf("jsapitest: send the fence on %s: %w", h.subject, err)
	}
	taken := 0
	for {
		r, err := h.Next(ctx)
		if err != nil {
			return taken, fmt.Errorf("jsapitest: the fence on %s never arrived, after %d reads: %w",
				h.subject, taken, err)
		}
		if r.Reply() == fence {
			return taken, nil
		}
		taken++
	}
}

// DropFirst makes the broker DROP the next n leader reads asked on subject —
// each reaches a subscriber that never answers, as a member that still
// believes in a leader its group just lost drops one — and lets every read
// after them through to the stream's leader. signIn is [Hold]'s.
func DropFirst(t testing.TB, srv *server.Server, subject string, n int, signIn ...nats.Option) *Held {
	t.Helper()
	h := Hold(t, srv, subject, signIn...)
	go func() {
		for range n {
			if _, err := h.Next(h.done); err != nil {
				return
			}
		}
		h.Release()
	}()
	return h
}
