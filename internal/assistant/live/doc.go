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
// # Numbered without a hole, across segments
//
// A frame takes its seq only once it validates, so a client never reads a gap
// for a frame that was never sent. A turn that parks on a proposal ends its
// segment's log, and the approval that resumes it may come minutes later; the
// [Hub] keeps an ended log's frames for [EndedRetention] (a reconnect) and the
// turn's last seq for as long as a turn may wait, so the resumed segment's
// frames continue the numbering its client already drew.
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
// of the turn's own, encrypted under the fleet keyring's AEAD with the turn in
// its associated data and sealed one way in a key domain of its own
// (`assistant.frame`), so reaching the broker reads no conversation, and a
// frame the fleet did not seal, or captured off another turn, is dropped
// (broadcast.go). A node asked to stream a turn it is not running listens
// there and asks the running node for the frames it missed, and a Stop pressed
// on another node reaches the running one, each over a signed instruction
// channel (internal/signedask's `assistant.replay` and `assistant.stop`, the
// replayed pages encrypted the same way), whose server checks the turn's owner
// before it answers (channels.go). A runner that died is answered by the
// persisted transcript as a snapshot, and the turn is closed interrupted by
// exactly one node.
//
// # One numbering across the fleet
//
// A turn's seq only grows, across its segments AND across the nodes that
// tell them: an approval may resume a parked turn, or emit its page action, on
// a node that never numbered it, and a client that drew frames from one node
// drops any frame numbered at or below the last it saw. So a node opening a
// segment of a turn asks the fleet the newest seq any node numbered for it (a
// probe on `assistant.replay` every node answers from its [Hub]) and numbers
// after it, never below the last seq the turn's parked record says it told.
// The same probe names the node holding a turn's newest frames, which is
// where a stream of a turn no running segment answers is followed.
//
// All of this is the assistant's [Fleet] half, which a node without a broker
// does not build: there, a stream of a turn running elsewhere is answered from
// the transcript and a Stop of one is unavailable.
package live
