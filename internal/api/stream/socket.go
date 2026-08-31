package stream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
)

// MaxInFlightQueries bounds how many queries one socket may have running.
//
// Queries run CONCURRENTLY so a store scan cannot stall the live feed, but each
// can take a connection from a pool the engine shares — so an unbounded fan-out
// from one tab would starve the engine's own writes. Four covers the most one
// screen issues at once (the agent page opens with three) and makes a burst
// queue rather than pile up.
const MaxInFlightQueries = 4

// The error codes a query answer can carry. CODES, not prose: the client
// switches on the value, and a message there would make every new wording a
// case nobody handles.
const (
	CodeUnknownQuery = "unknown_query"
	CodeUnauthorized = "unauthorized"
	CodeQueryFailed  = "query_failed"
)

// ErrUnknownQuery and ErrUnauthorized are the two failures a query surface
// reports precisely; everything else is a query_failed.
var (
	ErrUnknownQuery = errors.New("stream: unknown query")
	ErrUnauthorized = errors.New("stream: query requires an operator")
)

// Query answers one client question.
//
// operatorID is empty for an unauthenticated socket. A query that needs one
// returns [ErrUnauthorized] rather than deciding for itself what to do about
// it, so the refusal reaches the client as a code it already handles.
type Query func(ctx context.Context, what string, params map[string]any, operatorID string) (any, error)

// request is one client-to-server frame.
type request struct {
	Kind   string         `json:"kind"`
	ID     int64          `json:"id"`
	What   string         `json:"what"`
	Params map[string]any `json:"params"`

	// Token rides the FRAME rather than the handshake for the
	// operator-only queries. A browser cannot set a header on a WebSocket
	// constructor, and a socket opened for anonymous reads still has to be
	// able to carry one credentialled question.
	Token string `json:"token"`
}

// Handler serves the dashboard's live socket.
//
// The credential is ?token= on the URL, because browsers cannot set headers on
// a WebSocket constructor. Non-browser clients may send Authorization instead,
// and should: a query string appears in proxy logs.
func Handler(guard *auth.Guard, svc *Service, query Query) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operatorID, ok := authenticate(guard, r)
		if !ok {
			// REFUSED BEFORE THE UPGRADE. Accepting a credential this
			// node rejects, purely to close it politely a moment later,
			// would let anyone open a socket here.
			//
			// It is NOT "close(1008) before accept", which is what this
			// comment used to claim and what the dashboard was written
			// against. A close code rides a close FRAME, so a handshake
			// that never completed cannot carry one: the browser reports
			// 1006 — indistinguishable from a stopped engine — and hides
			// the status, so a page cannot port-scan with a socket.
			//
			// The client therefore cannot learn this from the socket at
			// all. It re-asks over plain HTTP, where the status is
			// visible, and this route answers a GET without an Upgrade
			// header 401 (refused) or 426 (accepted, wrong protocol).
			// That pairing is load-bearing for the dashboard's token
			// gate — see `_probeRefusal` in static/dashboard/js/socket.js.
			// Real JSON, not http.Error: that sets text/plain AND
			// nosniff, so a JSON literal handed to it is the one
			// combination guaranteed to stop a strict client parsing
			// the body it is being sent.
			httpjson.Fail(w, http.StatusUnauthorized, "invalid_token")
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// The dashboard is served by this same process, so the
			// socket is same-origin. Anything else is a page the
			// operator did not open.
			OriginPatterns: nil,
		})
		if err != nil {
			log.Debug("stream_accept_failed", "error", err)
			return
		}
		serveSocket(r.Context(), conn, guard, svc, query, operatorID)
	})
}

// authenticate resolves the socket's operator, or refuses it.
//
// The socket is guarded exactly as the equivalent HTTP read is: under anonymous
// reads it opens without a credential, and under a closed posture it does not.
// A token that is PRESENT and wrong is refused either way — a client that sent
// one meant to be somebody.
func authenticate(guard *auth.Guard, r *http.Request) (string, bool) {
	candidate := r.URL.Query().Get("token")
	if candidate == "" {
		header := r.Header.Get("Authorization")
		const scheme = "bearer "
		if len(header) >= len(scheme) && strings.EqualFold(header[:len(scheme)], scheme) {
			candidate = strings.TrimSpace(header[len(scheme):])
		}
	}
	operatorID, authenticated := guard.Operator(candidate)
	if authenticated {
		return operatorID, true
	}
	if candidate != "" {
		return "", false
	}
	// No credential offered. The read posture decides.
	return "", !guard.Requires("/ws/stream", http.MethodGet)
}

// serveSocket runs one connection until it closes.
func serveSocket(ctx context.Context, conn *websocket.Conn, guard *auth.Guard,
	svc *Service, query Query, operatorID string,
) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	client := NewClient()
	// REGISTERED BEFORE THE SNAPSHOT. See Hub.Register: the overlap is
	// deduped by the client, and the gap the other order leaves is not.
	svc.Hub().Register(client)
	defer svc.Hub().Unregister(client)

	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		defer cancel()
		writeLoop(ctx, conn, client)
	}()

	client.send(Push(KindSnapshot, svc.Snapshot(), time.Now().UTC()))
	readLoop(ctx, conn, guard, client, query, operatorID)

	// Unregister closes the client's queue, which is what ends the writer.
	svc.Hub().Unregister(client)
	writer.Wait()
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// writeLoop drains this client's queue onto the socket.
//
// One writer per connection, and it is the ONLY thing that writes: a WebSocket
// connection permits one concurrent writer, so a query answered on its own
// goroutine goes through this queue rather than to the socket directly.
func writeLoop(ctx context.Context, conn *websocket.Conn, client *Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case env, open := <-client.Out():
			if !open {
				return
			}
			raw, err := Encode(env)
			if err != nil {
				// A frame that cannot be encoded is this server's bug,
				// not the client's. Dropping it keeps the socket alive
				// for every other kind rather than tearing down a
				// working dashboard over one malformed push.
				log.ErrorContext(ctx, "stream_encode_failed", "kind", env.Kind, "error", err)
				continue
			}
			// A DEADLINE PER WRITE. A TCP peer that has vanished
			// without a FIN — a laptop lid closed, a NAT entry
			// dropped, a mobile network handing off — leaves this
			// Write blocked until the kernel gives up, which is
			// minutes with default keepalives. Until then the
			// goroutine, the client's queue and the hub registration
			// all stay live, so a page nobody is reading holds a slot
			// on every broadcast.
			//
			// THIRTY SECONDS, not a few: QueueDepth above decides
			// deliberately that a slow tab must not be disconnected
			// ("a visible failure for a reader who did nothing
			// wrong"), so this deadline is here to tell GONE from
			// SLOW and nothing else. A tighter one would sever a
			// mobile tab mid-snapshot and contradict that decision.
			writeCtx, cancelWrite := context.WithTimeout(ctx, writeTimeout)
			err = conn.Write(writeCtx, websocket.MessageText, raw)
			cancelWrite()
			if err != nil {
				return
			}
		}
	}
}

// readLoop handles client frames until the socket closes.
func readLoop(ctx context.Context, conn *websocket.Conn, guard *auth.Guard,
	client *Client, query Query, operatorID string,
) {
	// The concurrency bound, as a token pool. Queries run on their own
	// goroutines so a store scan cannot stall the live feed, and a burst
	// past the bound queues here rather than piling into the engine's
	// connection pool.
	slots := make(chan struct{}, MaxInFlightQueries)
	var running sync.WaitGroup
	defer running.Wait()

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var req request
		if err := decodeRequest(raw, &req); err != nil {
			// Unparseable input from a client is not a reason to drop a
			// socket that is otherwise working.
			log.DebugContext(ctx, "stream_bad_frame", "error", err)
			continue
		}
		switch req.Kind {
		case "ping":
			client.send(Envelope{Kind: KindPong})
		case "query":
			if query == nil {
				client.send(queryError(req, CodeUnknownQuery))
				continue
			}
			running.Add(1)
			slots <- struct{}{}
			go func(req request) {
				defer running.Done()
				defer func() { <-slots }()
				runQuery(ctx, guard, client, query, req, operatorID)
			}(req)
		default:
			// Unknown kinds are ignored, which is what makes new ones
			// additive on both ends.
		}
	}
}

// runQuery answers one question onto the client's own queue.
func runQuery(ctx context.Context, guard *auth.Guard, client *Client, query Query,
	req request, operatorID string,
) {
	// A frame-carried token upgrades THIS query only. It is how a socket
	// opened for anonymous reads asks one operator-only question without
	// reconnecting.
	id := operatorID
	if req.Token != "" {
		if resolved, ok := guard.Operator(req.Token); ok {
			id = resolved
		}
	}
	data, err := query(ctx, req.What, req.Params, id)
	switch {
	case err == nil:
		client.send(Envelope{Kind: KindResult, ID: req.ID, What: req.What, Data: data})
	case errors.Is(err, ErrUnknownQuery):
		client.send(queryError(req, CodeUnknownQuery))
	case errors.Is(err, ErrUnauthorized):
		client.send(queryError(req, CodeUnauthorized))
	default:
		// The reason reaches the LOG, not the client. A query failure can
		// carry a database path or a driver's own message, and the socket
		// is the one surface an unauthenticated reader may be holding.
		log.WarnContext(ctx, "stream_query_failed", "what", req.What, "error", err)
		client.send(queryError(req, CodeQueryFailed))
	}
}

func queryError(req request, code string) Envelope {
	return Envelope{Kind: KindError, ID: req.ID, What: req.What, Error: code}
}

// decodeRequest parses one client frame.
//
// Strict about SHAPE and lenient about content: a frame that is not an object
// is not a request, but an unknown field on one that is costs nothing to
// ignore — which is what makes a newer client safe against an older server.
func decodeRequest(raw []byte, req *request) error {
	return json.Unmarshal(raw, req)
}
