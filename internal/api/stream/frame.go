package stream

import "encoding/json"

// FramePosture is how one connected client is being served, and it is the ONE
// thing that decides the bytes its frames carry: two clients in the same
// posture receive byte-identical frames from one encode, and a client in
// another posture receives whatever that posture allows instead. That is what
// lets [Hub.Broadcast] marshal an envelope once per posture rather than once
// per tab.
//
// # It is FramePosture, and there are two other "postures" it is not
//
// The name is deliberately long, because "posture" is already taken twice in
// this engine and both of the others are a different closed set:
//
//   - THE NODE POSTURE — `serve`, `wait`, `shed`, `isolated`, `stuck` — is
//     what a node concluded about its own config lag, carried on the health
//     envelope (`internal/api/health.go`) and read by /ready. It describes a
//     PROCESS. This describes ONE SOCKET, and it is derived FROM that one:
//     see [Options.Posture].
//   - THE ENROLMENT STAGE a principal moves through is arriving in the
//     identity work under the bare name `Posture`. Two closed sets under one
//     name is how a later change maps the wrong one — a `Posture` value from
//     over there would compile perfectly well into a frame decision here — so
//     this type carries the word `Frame` and never sheds it.
//
// THE ZERO VALUE IS INVALID. An empty posture is a client nobody decided about,
// and the two ways to read it are opposite (serve it everything, or serve it
// nothing), so it is refused rather than guessed: [FramePosture.Valid] is
// false for it and [Client.SetPosture] will not take it.
type FramePosture string

const (
	// FrameLive is a client on a node that can serve: every push reaches
	// it and every query it asks is run.
	FrameLive FramePosture = "live"

	// FrameDegraded is a client on a node that CANNOT serve — its copy of
	// the company is wrong rather than merely behind, so what it would
	// push is not the company's state.
	//
	// The socket STAYS OPEN. A dashboard whose socket closes reconnects on
	// a backoff for as long as the node is degraded, learns nothing from
	// any attempt, and shows "retrying" — which is strictly worse than a
	// socket that is open and telling it, in the health frame it keeps
	// receiving, exactly what is wrong. So the keepalives continue, the
	// pushes stop, and a query is refused with [CodeUnavailable] rather
	// than answered out of a copy the fleet has abandoned.
	FrameDegraded FramePosture = "degraded"
)

// framePostures is how many there are. It is the width of the per-broadcast
// encode cache, which is an ARRAY rather than a map: a broadcast is the
// engine's hot path and allocating a map to hold at most two entries would
// cost more than the second encode it saves.
const framePostures = 2

// Valid reports whether p is one of the declared postures. False for the zero
// value — see [FramePosture].
func (p FramePosture) Valid() bool {
	switch p {
	case FrameLive, FrameDegraded:
		return true
	default:
		return false
	}
}

// index is p's slot in the per-broadcast encode cache.
//
// Total over the valid set and −1 for everything else, so a caller that did
// not check [FramePosture.Valid] first cannot index the array with a value
// this switch does not know.
func (p FramePosture) index() int {
	switch p {
	case FrameLive:
		return 0
	case FrameDegraded:
		return 1
	default:
		return -1
	}
}

// Delivers reports whether a frame of this kind reaches a client in this
// posture.
//
// A DIRECT frame always passes, in every posture. It is the answer to
// something this client did — its snapshot, its pong, its query's result or
// refusal — and a posture that swallowed those would leave a client waiting
// for an answer that is never coming, which is the one failure a socket cannot
// report. The degraded refusal itself is a direct frame.
//
// Of the fanned-out kinds a degraded client keeps only [KindHealth]: it is the
// keepalive AND the explanation, so the one frame that still arrives is the
// one that says why the others stopped. Everything else is derived from this
// node's copy of the company, which is what being degraded means it cannot
// vouch for.
func (p FramePosture) Delivers(kind string) bool {
	switch p {
	case FrameLive:
		return RouteOf(kind).Valid()
	case FrameDegraded:
		return RouteOf(kind) == RouteDirect || kind == KindHealth
	default:
		return false
	}
}

// ServesQueries reports whether a client in this posture may have its
// questions run.
//
// A degraded node REFUSES rather than answers, and the refusal is
// [CodeUnavailable] — "this node understood you and cannot answer YET", the
// one code the dashboard must never flatten into an empty result. Answering
// from a copy the fleet has abandoned is the failure this exists to stop: "no
// such work item" is something a person acts on.
func (p FramePosture) ServesQueries() bool { return p == FrameLive }

// Route is how a push kind reaches the clients it is for.
//
// A CLOSED SET WITH A TOTAL TABLE behind it ([RouteOf]), because the fan-out
// has three genuinely different shapes now and the difference is not
// inspectable from a kind's name. A kind with no route would fall to whichever
// branch the switch ended on — which, for a per-seat frame, is every tab in
// the company.
type Route string

const (
	// RouteBroadcast reaches every connected client whose posture takes
	// the kind. The company's own state: what one tab sees, every tab
	// sees.
	RouteBroadcast Route = "broadcast"

	// RouteSeat reaches only the clients watching the frame's own seat,
	// through the hub's seat index. The frame names that seat in
	// [Envelope.Seat], and a frame of such a kind with no seat on it is
	// DROPPED rather than fanned out — broadcasting a targeted frame is
	// the failure mode this route exists to make impossible.
	RouteSeat Route = "seat"

	// RouteDirect goes to one client, from that client's own goroutine —
	// its snapshot, its pong, its query answers. Never broadcast:
	// [Hub.Broadcast] refuses one, because a result carrying another
	// tab's correlation id is an answer to a question it never asked.
	RouteDirect Route = "direct"
)

// Valid reports whether r is one of the declared routes. False for the zero
// value, which is what [RouteOf] answers for a kind with no entry.
func (r Route) Valid() bool {
	switch r {
	case RouteBroadcast, RouteSeat, RouteDirect:
		return true
	default:
		return false
	}
}

// RouteOf is how a kind reaches a client, or the invalid zero Route for a kind
// with no entry on the table.
//
// THE TABLE IS TOTAL OVER THE FROZEN KIND LIST, and
// TestEveryPushKindHasARoute is what holds it there by walking this package's
// own source for the Kind… constants — so a kind added in hub.go without a
// route fails the build rather than reaching whichever default a switch
// happened to have.
func RouteOf(kind string) Route { return routes[kind] }

// Frame is one encoded envelope, SHARED by every client it is delivered to.
//
// The bytes are marshalled once per (kind, posture) by [Hub.Broadcast] and
// handed to every client in that posture. Each client's writer then writes
// what it was given, instead of marshalling the same envelope again: with N
// dashboards open, one event used to cost N encodes of identical JSON, on N
// goroutines, for every push.
//
// IT IS IMMUTABLE ONCE BUILT. [Frame.Raw] hands back the shared slice rather
// than a copy — copying it per client would give back exactly what the shared
// encode saved — so nothing may write into it.
type Frame struct {
	kind string
	raw  []byte
}

// Kind is the push kind this frame carries, kept beside the bytes so the hub
// and its tests can route and assert without decoding.
func (f *Frame) Kind() string { return f.kind }

// Raw is the encoded frame, exactly as it goes on the wire.
//
// SHARED AND READ-ONLY — see [Frame]. A caller that needs to modify it copies
// it first.
func (f *Frame) Raw() []byte { return f.raw }

// EncodeFrame marshals an envelope into the frame every client in one posture
// receives.
//
// Here rather than at the transport so the bytes a test asserts about are the
// bytes a browser receives, and so one encode can serve many clients.
func EncodeFrame(env Envelope) (*Frame, error) {
	raw, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return &Frame{kind: env.Kind, raw: raw}, nil
}

// Encode serializes an envelope for the wire.
//
// The bytes [EncodeFrame] wraps, for a caller that wants only them.
func Encode(env Envelope) ([]byte, error) { return json.Marshal(env) }
