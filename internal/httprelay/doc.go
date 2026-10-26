// Package httprelay carries one HTTP exchange from the node that received it to
// the process that holds what answers it, over the queue's ephemeral request
// and reply.
//
// # What it is for
//
// Some requests can be answered by one process only, because what answers them
// is live state in that process rather than fleet state any node can read: a
// detached run's tool session is the seat's live tool surface, in the process
// holding the seat (internal/api/mcpbridge). The request still arrives wherever
// the network put it — a load balancer in front of several nodes, or the node a
// box was launched from after its seat moved to a peer. This package hands the
// whole exchange to the holder and streams its answer back, so the node that
// received the request answers it exactly as the holder would have, an event
// stream included.
//
// # Over the broker, never a proxy to the holder's address
//
// The nodes of a fleet have no HTTP address for each other, and several have
// none at all: a node without the ingress role serves its probes and nothing
// else, and a satellite at the edge of a network accepts no inbound connection.
// What reaches every node is the broker — the fabric the estate router, the
// steer channel and the memory read already ride — so an exchange goes there.
// A reverse proxy would need an address per node that every other node can
// dial, which is a deployment fact nothing records and many deployments do not
// have.
//
// # The shape of one exchange
//
// The node that received the request (the ENTRY) first serves a subject of the
// exchange's own, then sends the request to the holder's node subject (OPEN).
// The holder decides whether it holds what the request needs ([Verdict]), runs
// the request through its own handler, and PUSHES the response back as frames
// on the exchange's subject: the status and header once, then the body as it is
// written, each frame acknowledged after the entry has written it to its
// client. A frame is pushed again until it is acknowledged, and the entry
// writes each sequence number once, so a lost frame or a lost acknowledgement
// costs a repeat and never a gap or a duplicate.
//
// STOP AND WAIT, deliberately. One frame is in flight per exchange, and the
// next is sent only once the entry's client has taken the last: that is the
// back-pressure that keeps a large answer from sitting whole in either process,
// and the holder's handler blocks on its own writes once a frame's worth is
// waiting. The cost is a broker round trip per frame, which an event stream's
// sparse, small events never notice.
//
// A SUBJECT PER EXCHANGE, because the queue answers at most
// [queue.MaxConcurrentAnswers] requests of one registration at once and an
// event stream lives for as long as its run. A stream's frames answered on one
// registration per node would hold one of its slots for hours each, and the
// sixty-fifth stream would wait behind them. The holder's node subject carries
// only OPEN and CLOSE, which are answered at once.
//
// # Both sides know when the other has gone
//
// A holder whose response has nothing new to say pushes an empty frame every
// [Heartbeat], so an idle event stream is still a conversation: an entry that
// hears nothing for [Idle] treats the holder as gone and ends its response —
// abruptly where the status has been written, so the client sees a broken
// stream rather than a complete one that was cut. A holder whose frames go
// unacknowledged for [Idle] treats the entry as gone and cancels the exchange's
// request. An entry whose client hangs up sends CLOSE, and the holder cancels
// at once.
//
// # Every message is signed
//
// The broker authenticates nothing (ADR-0031), and both directions are
// instructions: OPEN has the holder run a request as whoever its credential
// names, and a frame is written to a client as the holder's answer. So both
// ride a [signedask.Channel] under the fleet's keyring, each request bound to
// an id and a deadline, each acknowledgement bound to the frame it answers.
package httprelay
