// Package presence is the node's presence hub: who is in each artifact's room,
// what they are looking at, and whether they are editing.
//
// PRESENCE IS TOLD, NEVER STORED. A roster lives in the memory of the node
// holding the sockets and crosses between nodes on an ephemeral broadcast
// channel: no row, no log record, no coordination key. A heartbeat stored
// durably would be a replicated write per cursor move, kept for a bucket's age,
// about a fact worthless forty-five seconds after it stops being refreshed.
//
// A FRAME NAMES A PERSON, SO IT IS SEALED. A forged frame would put somebody in
// a room they are not in, so every frame a node broadcasts is sealed under the
// fleet keyring with a short validity inside the seal, and a frame the fleet did
// not seal shows nobody.
//
// Agents are present too: a tool session reading or writing an artifact
// announces itself like a person's tab does.
package presence
