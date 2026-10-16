// Package host is SMART HTTP FOR PERSONS: the code host's git surface, the
// one landing every push takes from its pack to an applied row, the hook
// relay that hands git's ref updates to it, and the wait that keeps an `ok`
// honest.
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
// # One landing, whoever built the push
//
// Everything the engine decides about a push once its objects are a pack in a
// scratch is ONE function, land (land.go), in this order: the repository's
// STATUS first — a deleted, purging or archived repository takes no push, so
// no bundle is ever stored for one; the force and stale checks, the stateless
// rules and the protections of what Forced changes; the blob scan over the
// push's own pack, while any command is left to decide, refusing only those,
// so a refusal that came first is the one the pusher reads; --atomic; nothing
// left to land; the stored size against code.MaxPackBytes; the bundle
// uploaded and sealed while the commits it introduces are listed, and the
// proposal held to every rule its committer will hold it to; the proposal
// itself, through the estate router; the outcome, `ok` only from the push's
// applied row; and the adoption of its pack into this node's cache, so the
// node that took a push does not download it back — unless a round that read
// its row had already begun that pack's GET, or a link fails
// (code_pack_adopt_failed; its next sync downloads it).
//
// What the landing does NOT check is that the scratch's objects are WHOLE, and
// that is its BUILDER'S: every command's new id present in the scratch or its
// alternate and connected to what the repository holds, and every object the
// builder wrote a valid one. For a push git received both are receive-pack's —
// its connectivity check, and its fsck of every object it took
// (receive.fsckObjects) — and both run before the hook ever reaches the relay.
// The landing reads neither: a push with no pack lists nothing, and a pack's
// listing walks commits, never their trees or blobs. A builder that hands over
// a scratch missing an object gets a row naming a history no cache can serve.
//
// The FORCE CHECK belongs to the landing because Command.Forced is formed
// there and nowhere else, and protected_force turns on it: a builder that
// formed its own would be judged by a check it chose. The landing PARSES THE
// PROTECTIONS ITSELF, from the synced row its scratch leans on, so no builder
// can judge a push against rules other than its own repository's as the
// push's sync read them. The relay is its one caller in this release — it
// reads git's commands off the hook and hands them over with what the receive
// handler already judged — and a push the engine builds itself from objects
// it wrote into a scratch is a second caller of the same function, never a
// second copy of its steps (ADR-0008): TestEveryPushTakesTheOneLanding holds
// the proposal, the upload and the adoption to it.
//
// # A push's walks go by generation, and are bounded by the history
//
// A push walks its repository's history up to four times: the force check's
// merge-base for each branch update, the listing of what it introduces, the
// search for a blob's path when one is over the limit, and receive-pack's own
// connectivity check. git walks commits newest first BY DATE wherever it has
// no generation number to stop on, so a commit dated before its parent — a
// person's clock that is behind — is a reason to walk every commit there is.
// Two rules keep that from costing a push more than it must, and from
// refusing it:
//
//   - EVERY WALK THE ENGINE ASKS FOR GOES BY GENERATION where the cache's
//     copy has a commit-graph, which the maintainer writes: merge-base
//     --is-ancestor does by itself, and the listings ask rev-list for
//     --topo-order (scan.go), without which it reads every commit of the
//     graph for a skewed tip.
//     Measured over a 200 000-commit history with one skewed commit on its
//     tip (git 2.47.3; 2.48.0 and 2.43.0 alike): the listing 110–150 ms in
//     date order with a graph against 5 ms by generation, and every walk
//     0.8–1.0 s with no graph, against 5 ms from a tip dated after its
//     parent.
//   - NO WALK IS BOUNDED BY THE PUSH ALONE. Each is given what a walk of the
//     whole history may cost (gitproc.HistoryWalk, an amount per live object
//     of the repository) beside what it was given for the push's own bytes,
//     because what the walk costs is set by the repository: bounded by the
//     push alone, a small skewed push into a repository of a couple of
//     million commits with no graph yet was refused by its own deadline —
//     ten seconds for the force check — rather than merely slow.
//
// RECEIVE-PACK'S CONNECTIVITY CHECK IS GIT'S, `rev-list --objects --stdin
// --not --all` with no flag the engine can add, and it walks by date with a
// graph or without — 0.85–0.90 s over the 200 000 commits from a skewed tip
// with none and 157 ms with one, where it reads the graph in place of the
// commits. It is the one cost of a push that grows with the repository
// however its node is maintained, and it is bounded by the history term the
// receive-pack process is given (gitproc.ReceivePackDeadline), never by
// anything the engine can flag.
//
// # Every answer that names a ref is linearizable; an object id has no level
//
// A v0 advertisement, a v2 `ls-refs` and a push's advertisement read the code
// log through a barrier, so a node in a minority answers 503 rather than an
// old head. A v2 capability advertisement names no ref and an object fetch
// names ids that mean the same bytes everywhere, so both take whatever the
// cache holds — and a fetch whose wants this node lacks takes ONE linearizable
// sync (the client's refs came from a node further ahead) before it is
// served. A fetch never names a ref: the host offers no ref-in-want, because
// git's own upload-pack and fetch-pack disagree on the order of a shallow
// fetch's sections once a client fetches by want-ref (cache.ServeConfig). The
// barrier is single-flighted per log and the cache's sync per repository, so a
// burst of CI clones of one repository costs one barrier and one sync.
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
// # A damaged cache is repaired under the request that found it
//
// Every git process the host runs over a repository's cache — an
// advertisement, upload-pack, the check of a fetch's wants, a push's
// receive-pack in its scratch — that fails before its first byte with damage
// the cache names (cache.DamageReason; only the shape, cache.ShapeReason, for
// a process whose directory merely has the cache as its alternate) has the
// cache REPAIRED — a shape in place, with no download, a pack or an object
// into the next incarnation — and runs once more over the repaired one
// (repair.go): the cache is disposable, so damage it can name is its own to
// repair, never the client's to retry. And every failure before the first
// byte is answered as what it is (refuse.go), identified by what failed and
// never by elimination: the code log failing (marked where it is met, so its
// sentence is never another failure's), the object store (the store's own
// side of a download, never the node's files beside it), the node ending
// git, git itself or an answer of git's the engine cannot read, a copy whose
// repair did not end its damage, this node's own disk — or a refusal of the
// engine's OWN call (the pool's: a key outside its allowlist, no deadline, a
// capture a call site must stream; the cache's: an argument it never takes;
// a read at a level no reader serves), a 500 that promises no retry, since
// none ends a call-site bug. Table tests hold every error gitproc and the
// cache export to a decided answer, both ways.
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
