// Package transfer moves chunks between the nodes of a fleet: the server a
// data node answers for its own chunks on, the client every node writes and
// reads through, and the cache of the placement map both of them place by.
//
// # Over the broker, because it is the one channel every node has
//
// Nodes are not assumed to reach each other directly — the embedded broker's
// routes are the only links a fleet is configured with — so a chunk travels as
// an ephemeral request on the addressed node's own subject
// (`crewlet.objects.<node>`), the scatter verb with one answerer. Nothing is
// retained: a put that went unanswered is a put that did not happen, and the
// writer tries the next member.
//
// # Binary, not JSON
//
// A request or reply is a four-byte header length, a JSON header and the raw
// bytes. A chunk carried inside JSON would be base64 — a third larger on every
// hop and an encode and decode of a mebibyte on each end — for no reader that
// needs it: nothing but this package ever looks inside one.
//
// # Placed by a layout computed once per map
//
// Every chunk's holders are read off the map's LAYOUT ([placement.Layout]),
// which the [Cache] computes once when it installs a map, rather than drawn
// again per chunk: a request for a thousand hashes would otherwise draw a
// thousand groups, and at a large fleet that had a server spending seconds on
// one. The map's full ranking of a group is computed only when a request runs
// past the up set.
//
// # Where a write goes
//
// To the chunk's UP SET, in parallel, and past a member that does not answer
// or refuses to the next member of the same ranking ([placement.Map.Ranked]) —
// so a holder that is down costs a copy in a different place rather than a
// copy fewer, and a reader looking down the same ranking finds it. Never to a
// member the map does not place on: one that is OUT is being emptied, and a
// copy put there is one more the fleet has to move off it; one on PROBATION
// has just come back from being removed and goes straight back at its next
// missed tick, so a quorum counted on it would be counted on the member the
// fleet has least reason to. A member whose own store is full or failed
// refuses by name, so the writer moves on at once. A write is reported stored
// at a QUORUM ([placement.Map.Quorum]); fewer is an error the caller surfaces,
// never a success with a footnote.
//
// # Where a read looks
//
// This node's own disk first, then the ranking in order: the up set, then
// wherever a displaced write or an older map left a copy, the members the map
// does not place on last. A member that did not answer is asked last for a
// while ([suspectFor]), so a dead holder costs one timeout per client rather
// than one per chunk of every file.
//
// A read that finds nothing says which of two things is true, because only
// one of them is a lost chunk: [ErrNotFound] when every member ANSWERED that it
// does not hold it, [ErrUnreachable] when any member could not say — it did
// not answer, refused, or could not read its own copy.
package transfer
