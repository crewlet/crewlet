// Package live is a Dashboard Assistant turn's live stream on the node that
// runs it, and how a client attached to another node follows it.
//
// # Told, never stored
//
// A turn's frames are worthless a minute after it ends, so they are kept in
// memory on the running node, in a bounded per-turn frame log (2000 frames or
// 4 MiB, whichever is first; deltas are coalesced at 200 ms, so a two-minute
// turn writes a few hundred), and never in a store. What the person must see
// after a reload is the transcript internal/assistantdomain records, not this
// log. That is the presence rule (ADR-0038) applied to a stream.
//
// # A slow client is disconnected, never dropped from
//
// A transcript rebuilt from deltas cannot tolerate a silently dropped frame,
// which is why the stream is not the dashboard socket and its drop-oldest
// queue. A subscriber whose buffer passes 512 frames is closed, and its client
// resumes from its last Last-Event-ID; the log replays every frame after it.
//
// # Across a fleet
//
// Every frame is also broadcast on the queue's ephemeral verbs under a subject
// of the turn's own, sealed under the fleet keyring in a key domain of its own,
// and a frame the fleet did not seal is dropped. A node asked to stream a turn
// it is not running listens there and asks the running node for the frames it
// missed, and a Stop pressed on another node reaches the running one, each over
// a signed instruction channel (internal/signedask's `assistant.replay` and
// `assistant.stop`), whose server checks the turn's owner before it answers.
// A runner that died is answered by the persisted transcript as a snapshot,
// and the turn is closed interrupted by exactly one node.
package live
