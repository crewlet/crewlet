// Package host is SMART HTTP FOR PERSONS: the code host's git surface, the
// receive pipeline every push takes, the hook relay that hands git's ref
// updates to the engine, and the wait that keeps an `ok` honest.
//
// # A push is committed by a record, and git is told so only from its row
//
// A person's `git push` reaches receive-pack in a SCRATCH repository whose one
// alternate is this node's cache — never in the cache, because git migrates a
// push's objects into the repository it runs in before any hook can refuse
// them, and a refused push's objects would then be served, and connected
// against, by a cache that is supposed to be a function of the rows
// (ADR-0036). The one hook, proc-receive, relays every ref update to the
// engine over a unix socket; the engine checks what only it can (a forced
// update under a protection, a blob over the limit), uploads the push's one
// pack bundle to the object store, and proposes the push to the repository's
// committer through the estate router. git is answered `ok` for a ref only
// from the push's own APPLIED row: a durable record a gate then drops on every
// node is not a push that landed, so `pending` is waited on — at most
// statelog.StallGrace — and never reported as `ok`.
//
// # Every answer that names a ref is linearizable; an object id has no level
//
// A v0 advertisement, a v2 `ls-refs`, a push's advertisement and a `want-ref`
// read the code log through a barrier, so a node in a minority answers 503
// rather than an old head. A v2 capability advertisement names no ref and an
// object fetch names ids that mean the same bytes everywhere, so both take
// whatever the cache holds — and a fetch whose wants this node lacks takes ONE
// linearizable sync (the client's refs came from a node further ahead) before
// it is served. The barrier is single-flighted per log and the cache's sync per
// repository, so a burst of CI clones of one repository costs one barrier and
// one sync.
//
// # A fetch request is spooled, and bounded by the repository it names
//
// git's own http-backend buffers a fetch request whole, up to 10 MiB, and a
// partial clone breaks that: a checkout collects every blob it lacks into ONE
// lazy fetch, at fifty bytes a want, so a blobless clone of a tree past about
// 210 000 files sends more than 10 MiB. So a request is held in memory AS
// LARGE AS IT IS, up to a mebibyte — a clone's negotiation costs the few
// kilobytes it is — charged to a node-wide allowance, so what requests waiting
// for their sync and their transfer slot hold is bounded by the node rather
// than by how many arrive; and the rest, or all of what the allowance cannot
// hold, is SPOOLED to an unnamed file on the cache's volume (cache.Spool),
// which the kernel removes when the request ends however it ends. The request
// is bounded by the most a request to ITS repository can legitimately name —
// fifty bytes for each object its live packs hold and each live ref, beside
// the mebibyte (RequestBound) — and the spools draw on a node-wide allowance
// of their own, never the cache's budget, of which one credential holds a
// quarter at most, in bytes and in spools. The wants are read back from
// wherever the request is, as a stream, by the negotiation's parse and by the
// cache's check of what it lacks, and never held whole; and the request is
// GIVEN BACK the moment upload-pack has read it and started its answer, since
// a retry over it happens only before that byte — never held through a
// transfer its client paces.
//
// # A refusal is decided before the bytes, and said in git's own words
//
// Everything that can be refused without objects is refused at the
// advertisement (a token bound to nobody, an archived or deleted repository),
// where git prints the sentence as `fatal: remote error: …`; on a POST it is a
// report git prints per ref as `! [remote rejected] … (<sentence>)` — written
// only after the request body is read to its end, because a response sent
// while git is still uploading is lost to a broken pipe (measured). Refusals
// are never decided on a stale read: anything that would refuse is read again
// through the barrier first.
//
// # Who pushes
//
// A person, with their own Tier A token as the Basic password: a token bound to
// a human seat (contact.crewlet_operator_id) pushes as that person, a token
// bound to nobody fetches only, and every push is one operator_acted record on
// the receiving node, transport git — the third person transport. This package
// imports nothing from the API but its auth and paced leaves, and nothing from
// the engine or the estate: its seams (Estate, Uploader, Persons, Audit) are
// declared here and filled by the engine.
//
// # The hook relay cannot end the process
//
// The relay's accept loop and its connections run outside net/http's
// per-handler recovery, and what they parse — the hook's handshake, git's
// proc-receive protocol — is bytes a pusher controls. So every relay
// connection's goroutine turns a panic into that push's refusal: an error to
// its hook, the stack logged once, the engine still running. The framing and
// the parser are fuzzed (fuzz_test.go), seeded from what real git sends.
package host
