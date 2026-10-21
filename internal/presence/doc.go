// Package presence is the node's presence hub: who is in each room, what they
// are looking at, and whether they are editing.
//
// # A room is named by its kind and its id
//
// [Room] is the KIND of thing a room is the presence of and that thing's id,
// and everything below holds for every kind alike: the frames, the sealing,
// the coalescing, the expiry, the large-room bound and the agents. What a kind
// owns is where its members can be looking ([Focus], checked per kind by
// [CheckFocus]), and the decision of who may be in one of its rooms, which is
// never this package's. An artifact's room and a chat room are the two kinds
// this build keeps ([RoomArtifact], [RoomChat]).
//
// # Presence is told, never stored
//
// A roster lives in the memory of the node holding the sockets and crosses
// between nodes on the queue's ephemeral Broadcast and Listen: no row, no log
// record, no coordination key. A heartbeat stored durably would be a
// replicated write per cursor move, kept for a bucket's age, about a fact
// worthless forty-five seconds after it stops being refreshed. Who has to agree
// on it is nobody: each node knows what its own sockets told it and what its
// peers last broadcast, and that is the whole of the state. The hub's broker
// seam ([Queue]) holds the two ephemeral pairs and no durable verb, so no path
// here can write a frame anywhere it would be kept.
//
// # A frame names a person, so it is sealed
//
// A forged frame would put somebody in a room they are not in, so every frame
// a node sends is sealed one way under the fleet keyring ([Wire], the key
// domain [Channel]) with [FrameValidity] inside the seal, and a frame the
// fleet did not seal shows nobody. The room a frame is about is inside the seal
// too, so a frame about one room can never be shown as another's. Identity is
// the node's: a socket says only what it is doing and where; the seat and kind
// beside its session are what its node resolved its credential to.
//
// # How a room stays right
//
//   - A NODE LISTENS on a room's subject only while one of its sockets is in
//     that room, so a fleet with a room open on two nodes moves its presence
//     between those two. One goroutine (the reconciler) listens and
//     withdraws, so the two never race for one room.
//   - CHANGES COALESCE, on two clocks: a room fans out to this node's
//     sockets at most [CoalesceRate] times a second (the difference between
//     what they were shown and what the room presents now, at most
//     [FrameMembers] members re-presented a frame, the most recent first),
//     and this node tells the fleet what its own members did there at most
//     [BroadcastRate] times a second. Apart, so a cursor crossing nodes waits
//     one short window on each side rather than two long ones.
//   - EVERY [Rebroadcast] each node broadcasts every member it holds in every
//     room, in full, and a peer replaces what it held from that node with it,
//     so a lost leave costs one rebroadcast. A member not heard for [Expiry]
//     (three missed rebroadcasts) is dropped: a node that dies takes its
//     viewers with it, with nothing to clean up.
//   - A NODE GAINING ITS FIRST SOCKET in a room asks its peers once, within the
//     fleet's read budget, rather than leaving a newcomer to wait a rebroadcast
//     interval to see who is there. Each answer is applied as it arrives
//     (queue.EventQueue.AskEach), so the room shows a peer's members within
//     that peer's round trip rather than once the whole budget is spent. An
//     answer can still be overtaken by the peer's broadcasts, so a member
//     changes only on a frame newer than the one that last said it, and a
//     late answer fills in what the room has not heard since and undoes
//     nothing.
//   - A JOINER IS HANDED ITS ROSTER IN THE ROOM'S DELIVERY ORDER ([Hub.Join],
//     [Hand]): under the one lock the fan-outs hold while they are in their
//     sinks, so every fan-out before the roster is one it holds and every
//     one after reaches the socket after it. A roster answered to the
//     caller instead was sent whenever the caller got to it, and a fan-out
//     taken after it could arrive first and be undone by it.
//   - A NODE STOPPING tells the fleet its members left before it goes.
//
// # Large rooms present the most recent fifty
//
// Past [RoomTop] members a room presents the fifty most recently active (by the
// instant each last changed what it is doing or where) and counts the rest by
// kind; it presents everybody again only below [RoomFull]. The hysteresis
// keeps a room hovering at the threshold from flipping on every join, and the
// bound keeps a room of five hundred from sending every diff to every socket. Nobody is invisible: every
// viewer is a member, presented or counted.
//
// # Agents are present too
//
// An agent's tool session reading or writing an artifact touches its room
// ([Hub.Touch]): the seat appears as an agent, viewing or editing, focused on
// the collection and document the call named, and leaves the room [Expiry]
// after its last call, since an agent's calls are its heartbeats. An agent's
// member is broadcast like a socket's, but a node whose only members in a room
// are agents does not listen on it: an agent has no screen to show the room
// on.
//
// # Who may be in a room is not decided here
//
// The socket decides a join (reading the artifact is an artifact room's rule)
// and decides it again whenever access narrows; the hub keeps who is present
// and nothing about who may be.
//
// Presence is told, never stored: ADR-0038, whose authority this package is.
package presence
