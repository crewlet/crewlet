// Package presence is the node's presence hub: who is in each artifact's room,
// what they are looking at, and whether they are editing.
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
// domain [Channel]) with [artifacts.PresenceFrameValidity] inside the seal, and
// a frame the fleet did not seal shows nobody. Identity is the node's: a socket
// says only what it is doing and where; the seat and kind beside its session
// are what its node resolved its credential to.
//
// # How a room stays right
//
//   - A NODE LISTENS on an artifact's subject only while one of its sockets is
//     in that room, so a fleet with an artifact open on two nodes moves its
//     presence between those two. One goroutine (the reconciler) listens and
//     withdraws, so the two never race for one room.
//   - CHANGES COALESCE: a room fans out at most
//     [artifacts.PresenceCoalesceRate] times a second, to this node's sockets
//     (the difference between what they were shown and what the room presents
//     now) and to the fleet (this node's members that changed or left).
//   - EVERY [artifacts.PresenceRebroadcast] each node broadcasts every member
//     it holds in every room, in full, and a peer replaces what it held from
//     that node with it, so a lost leave costs one rebroadcast. A member not
//     heard for [artifacts.PresenceExpiry] (three missed rebroadcasts) is
//     dropped: a node that dies takes its viewers with it, with nothing to
//     clean up.
//   - A NODE GAINING ITS FIRST SOCKET in a room asks its peers once, within the
//     fleet's read budget, rather than leaving a newcomer to wait a rebroadcast
//     interval to see who is there. The answers are applied once the budget is
//     spent, after the peers' broadcasts may already have said more, so a
//     member changes only on a frame newer than the one that last said it, and
//     a late answer fills in what the room has not heard since and undoes
//     nothing.
//   - A NODE STOPPING tells the fleet its members left before it goes.
//
// # Large rooms present the most recent fifty
//
// Past [artifacts.PresenceRoomTop] members a room presents the fifty most
// recently active (by the instant each last changed what it is doing or where)
// and counts the rest by kind; it presents everybody again only below
// [artifacts.PresenceRoomFull]. The hysteresis keeps a room hovering at the
// threshold from flipping on every join, and the bound keeps a room of five
// hundred from sending every diff to every socket. Nobody is invisible: every
// viewer is a member, presented or counted.
//
// # Agents are present too
//
// An agent's tool session reading or writing an artifact touches its room
// ([Hub.Touch]): the seat appears as an agent, viewing or editing, focused on
// the collection and document the call named, and leaves the room
// [artifacts.PresenceExpiry] after its last call, since an agent's calls are
// its heartbeats. An agent's member is broadcast like a socket's, but a node
// whose only members in a room are agents does not listen on it: an agent has
// no screen to show the room on.
//
// # Who may be in a room is not decided here
//
// The socket decides a join (reading the artifact is the rule) and decides it
// again whenever access to the artifact narrows; the hub keeps who is present
// and nothing about who may be.
package presence
