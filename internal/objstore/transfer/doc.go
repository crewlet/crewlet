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
// # Where a write goes
//
// To the chunk's UP SET, in parallel, and past a member that does not answer
// to the next member of the same ranking ([placement.Map.Ranked]) — so a
// holder that is down costs a copy in a different place rather than a copy
// fewer, and a reader looking down the same ranking finds it. A write is
// reported stored at a QUORUM ([placement.Map.Quorum]); fewer is an error the
// caller surfaces, never a success with a footnote.
//
// # Where a read looks
//
// This node's own disk first, then the ranking in order: the up set, then
// wherever a displaced write or an older map left a copy. A member that did
// not answer is asked last for a while ([suspectFor]), so a dead holder costs
// one timeout per client rather than one per chunk of every file.
package transfer
