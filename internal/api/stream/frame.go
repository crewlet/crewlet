package stream

import "encoding/json"

// FramePosture is how one client is served, the ONE thing deciding the bytes
// its frames carry: one encode per posture, not per tab ([Hub.Broadcast]).
//
// The name is long because "posture" is already two other closed sets: THE
// NODE POSTURE (`serve`, `wait`, `shed`, `isolated`, `stuck`, read by /ready)
// describes a PROCESS, while this describes ONE SOCKET and derives from it
// ([Options.Posture]); THE ENROLMENT STAGE is arriving under the bare name
// `Posture`, and a value of the wrong set would compile into a frame decision.
//
// THE ZERO VALUE IS INVALID: a client nobody decided about has two opposite
// readings (serve everything, serve nothing), so [FramePosture.Valid] is false
// for it and [Client.SetPosture] refuses it.
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
// A DIRECT frame always passes, in every posture: it answers something this
// client did (snapshot, pong, a query's result or refusal), and swallowing it
// would leave a client waiting for an answer never coming, a failure a socket
// cannot report. The degraded refusal itself is a direct frame.
//
// Of the fanned-out kinds a degraded client keeps only [KindHealth], the
// keepalive AND the explanation of why the others stopped. The rest derives
// from this node's copy of the company, which a degraded node cannot vouch for.
func (p FramePosture) Delivers(kind Kind) bool {
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
// has four genuinely different shapes and the difference is not inspectable
// from a kind's name. A kind with no route would fall to whichever branch the
// switch ended on, which, for a per-seat or per-artifact frame, is every tab
// in the company.
type Route string

const (
	// RouteBroadcast reaches every connected client whose posture takes
	// the kind. The company's own state: what one tab sees, every tab
	// sees.
	RouteBroadcast Route = "broadcast"

	// RouteRoom reaches only the clients in the frame's own room, through
	// the hub's room index: a seat's room (the sockets whose watch of that
	// seat was allowed) or an artifact's (the sockets whose join of it was
	// allowed). The frame is built for its room ([PushSeat],
	// [PushArtifact]), and a frame of such a kind with no room on it is
	// DROPPED rather than fanned out: broadcasting a targeted frame is the
	// failure mode this route exists to make impossible.
	RouteRoom Route = "room"

	// RouteDirect goes to one client, from that client's own goroutine —
	// its snapshot, its pong, its query answers. Never broadcast:
	// [Hub.Broadcast] refuses one, because a result carrying another
	// tab's correlation id is an answer to a question it never asked.
	RouteDirect Route = "direct"

	// RouteViewer reaches only the clients a per-recipient decision
	// admits — the chat hub's, which asks the room's own visibility
	// ([chatlet.Visible]) for each viewer, or the reader's own seat for a
	// read state — and leaves by the one fan-out every push takes. NEVER
	// fanned out by [Hub.Broadcast], which refuses one: a chat frame
	// broadcast is a private room delivered to every open tab.
	//
	// NOT [RouteRoom], whose seat rooms are the `watch` frame's: a lead may
	// watch a report's seat, so a person's read state routed through one
	// would be handed to their lead.
	RouteViewer Route = "viewer"
)

// Valid reports whether r is one of the declared routes. False for the zero
// value, which is what [RouteOf] answers for a kind with no entry.
func (r Route) Valid() bool {
	switch r {
	case RouteBroadcast, RouteRoom, RouteDirect, RouteViewer:
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
func RouteOf(kind Kind) Route { return routes[kind] }

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
	kind Kind
	raw  []byte
}

// Kind is the push kind this frame carries, kept beside the bytes so the hub
// and its tests can route and assert without decoding.
func (f *Frame) Kind() Kind { return f.kind }

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
//
// ESCAPED, with encoding/json's default, for the reason internal/api/httpjson
// gives under "Escaped at the one writer, read by decoders": every client
// decodes a frame, and escaping at the one writer is what spells a frame alike
// whatever its payload embeds.
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
