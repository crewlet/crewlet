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
//     copy has a commit-graph, which the maintainer writes into a copy first
//     (internal/code/cache's GRAPH FIRST): merge-base --is-ancestor does by
//     itself, and the listings ask rev-list for --topo-order (scan.go),
//     without which it reads every commit of the graph for a skewed tip.
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
//
// # Measured
//
// On four vCPUs of a 2.1 GHz Xeon with SHA instructions, Go 1.27.2, every
// pool running git 2.47.3 — the release the engine image installs, built
// from source (CREWLET_BENCH_GIT) — and every client the machine's git
// 2.43.0, all on one machine: one data node and two git nodes
// (codetest.SoloNode), the broker fsyncing every write as stream.sync's
// default does (SoloOptions.SyncAlways), the S3 fake answering each request
// 100 ms after it arrives at 80 MB/s a connection. Three counts of three
// seconds, the range given; the CREWLET_BENCH_LARGE arms one, as each says
// (bench_test.go). No real bucket was measured.
//
// A PUSH (BenchmarkPushSmall) of one commit of one 4 KiB file, onto a copy
// the receiving node had indexed whole before the arm (its own pushes
// gathering outside the index, 19–22 of them on average), is 94–99 ms as the
// person's git waits for it. The receiving node's request is 59–63 ms of it
// (crewlet.code.push.duration, applied): receive-pack and its index-pack, 32
// ms of transfer-lane CPU; the scans and the listing of the commits it
// introduces, 8.3–8.6 ms of read-lane CPU; the bundle's put; the proposal
// and the commit chain (internal/code's "What a push costs, measured"); and
// the wait for its row. Its advertisement, a request of its own, is 16–17
// ms, and the rest is git's client. Put to the S3 fake the request is
// 150–157 ms and the push 184–192 ms: the one request on a push's path that
// waits on the object store is its put, a first byte's worth. The OTHER git
// node's delta sync of the push — the packed-refs merge and one GET of the
// bundle — is 8.1–9.1 ms from memory and 111–113 ms from the fake.
//
// THE SERVER COST OF A PUSH — the rule a push is held to — is about 60 ms and
// 2.4 µs a live ref, and does not grow with a history whose commits are each
// newer than their parents: the request is 66–71 ms at a thousand live refs
// and 90–95 at ten thousand, against 59–63 at one — and 71, 97 and 303 ms at
// a thousand, ten thousand and a hundred thousand (code.MaxRefsPerRepo) over
// a gigabyte of history in 455 680 commits (one count). What the refs cost is
// receive-pack reading them (transfer CPU 41–44 ms at a thousand, 62 at ten
// thousand, 228 at a hundred thousand) and the listing of what a push
// introduces excluding every one (read CPU 10–11, 21–22 and 129 ms); the
// push's advertisement, a request before it, is 13–15, 23–26 and 136 ms,
// about 1.2 µs a ref, and the person's git 101–109, 172–182 and 1 960 ms for
// the whole push, most of it the client's own work over the refs it was
// shown. Each arm pushes to a node that read the repository whole and
// indexed it just before, so what the refs cost is not mixed with packs
// another arm left outside an index. The other node's sync, packed-refs
// merged as a stream, is 8.0–9.4, 11–12 and 42 ms — at the bound 14% of the
// request: the packed-refs a cache writes in place of reftable
// (gitproc.MinVersion) cost a delta sync tens of milliseconds there and the
// push nothing.
//
// A COMMIT DATED BEFORE ITS PARENT — a person's clock that is behind
// (gittest.Skewed) — is what makes a push pay for the history, measured over
// the same gigabyte (one count) on a node that read it whole:
//
//   - WITH NO COMMIT-GRAPH (every slot of the node's maintenance lane held, so
//     the copy is as a node that just read it whole leaves it), every walk
//     git makes goes by date and walks all 455 680 commits from such a tip:
//     the force check 2.3 s, the listing of what the push introduces 2.5 s,
//     the search for a blob's path 2.3 s and receive-pack's own connectivity
//     check 2.4 s — 5.0–5.5 µs a commit, against 3–9 ms from a tip dated
//     after its parent. The push's request is 6.9 s (4.5 s of read-lane CPU
//     and 2.2 s of transfer-lane) against 71 ms, and as git waits for it
//     14 s against 114 ms.
//   - WITH THE COMMIT-GRAPH the maintainer writes first (internal/code/cache's
//     GRAPH FIRST), the engine's own walks go by generation — the force
//     check's merge-base by itself, the two listings asked for in
//     --topo-order (scan.go) — and take 2.7, 4.2 and 7.7 ms from a skewed
//     tip, the same as from any other. What is left is receive-pack's own
//     connectivity check, which is GIT'S and takes no flag of ours: it walks
//     by date with a graph or without, reading the graph in place of the
//     commits, 0.30 s over the gigabyte (0.65 µs a commit) — the one cost of
//     a push that follows the repository however its node is maintained, and
//     bounded by gitproc.ReceivePackDeadline's history term rather than by
//     anything the engine can flag. The request is 374 ms against 65.
//   - THE PERSON'S OWN GIT walks too: a clone with no commit-graph of its own
//     checks a push is a fast-forward by date, so a push whose branch's
//     previous tip or new tip is dated before its parent cost the person's
//     git some 4.8 s of its own over the gigabyte (a 4.9 s push whose
//     request was 65 ms) — `git commit-graph write --reachable` in the clone
//     is what spares it, and nothing on the server can.
//
// Every one of those walks is bounded by the history as well as by the push
// (gitproc.HistoryWalk, 11 µs an object, twice the dearest per-commit cost
// above), so a skewed push into a large history is slow and never refused.
// The window in which a node walks by date lasts from the round that read a
// copy whole to the graph's own write, 3.4 s over a gigabyte
// of source history on a node with a slot free (internal/code/cache's
// BenchmarkMidx, step=graph).
//
// A WARM FETCH (BenchmarkFetchWarm) — a v2 ls-refs, linearizable, and a fetch
// of the one commit pushed since, from a cache whose maintainer had indexed
// every pack before the first fetch, each push since a pack outside that
// index (35–47 of them on average for one client, whose writer pushes every
// fifty milliseconds and so never stands still for the index to be written
// again) — is 45–50 ms at the median for one client (61–91 ms p99) and
// 10–12 ms of the node's git CPU. Eight clients at once are 73–76 fetches a
// second (90–95 ms at the median) and sixty-four 90–104, the p99 600–800 ms:
// the machine's four cores running every client's git beside the node's
// 9–11 ms a fetch. A repository of thirty-two pushes, every one in the index,
// costs a fetch 1–1.5 ms more CPU than one of a single push. The barrier is a
// linearizable read, 1.7–1.8 ms at the median on one data node and 4.8–5.5 ms
// at three replicas (internal/statelog's measured section): a thirtieth of a
// fetch, and a tenth.
//
// A COLD CLONE (BenchmarkCloneCold) — the first on a git node that never
// served the repository, its cold sync, round and first upload-pack in series
// before git's first byte — of two hundred pushes of fifty 3 000-byte files
// (a tree of 10 000 files in a hundred directories, a 31.5 MB pack), from the
// S3 fake, each client a person of their own:
//
//   - the cold sync is 1.39–1.45 s, two hundred GETs sixteen at a time, in
//     every shape and for one clone or eight, which share it;
//   - full is 5.5–5.7 s alone and 10.1–10.5 s each for eight at once,
//     3.7–3.9 s of the node's git CPU a clone: a node with no bitmap yet
//     walks and delta-searches the whole history for every clone;
//   - blobless+checkout is 6.2–8.3 s for 32.3 MB, its checkout's one lazy
//     fetch asking for 500 549 bytes of wants (fifty an object), held in
//     memory under RequestMemoryBytes, and 3.8–3.9 s of CPU; eight at once
//     take 13–20 s each;
//   - depth1+blobless+cone is 2.3–2.4 s for 3.4 MB and 0.38–0.40 s of CPU —
//     the sync over half of it — and 3.5–3.6 s each for eight;
//   - treeless+cone is 2.3–2.5 s for 3.4 MB and 0.40–0.43 s of CPU, and
//     3.5–3.8 s each for eight;
//   - op=blob, one lazy fetch of one blob into a blobless clone from a warm
//     node, is 37–39 ms and 11 ms of CPU.
//
// Ten times the tree (CREWLET_BENCH_LARGE, two counts: two thousand pushes,
// 100 000 files in a thousand directories, a 334 MB pack) is ten times every
// figure: the sync 13.4–13.8 s, past MidxRoundPacks so it writes the round's
// index — 0.14 s of it (op=round-index), the GETs the rest; full 52–54 s
// alone and 103–105 s each for eight, 35–38 s of CPU a clone;
// blobless+checkout 42–44 s, its lazy fetch 5 000 553 bytes of wants —
// spooled, past the mebibyte held in memory — and 14–15 s of CPU, 87–107 s
// each for eight; depth1+blobless+cone 20 s for 33.8 MB and 3.3–3.8 s of
// CPU, 29 s each for eight; treeless+cone 21 s and 3.8–4.0 s, 32 s each for
// eight; and op=blob 66–68 ms and 45–46 ms of CPU. Every cold figure but
// op=blob begins with the sync, so a balancer between a person and a node
// that has not served a repository sees that long before git's first byte.
//
// ONE CREDENTIAL'S SPOOLS ARE CAPPED (SpoolsPerCredential: four on this
// lane of sixteen), and a fifth is answered 503 at once (body.go): of eight
// blobless checkouts of the 100 000-file tree under ONE token, each lazy
// fetch spooled, a checkout failed on that 503 — git does not retry a lazy
// fetch — which is why the arm's clients are people. So a CI fleet behind
// one token running checkouts that large on a node this size does not run
// four at a time: every checkout past the fourth at once FAILS, and is not
// queued behind the four.
//
// A WARM CLONE (BenchmarkCloneWarm), the same tree from a node holding its
// bitmap — on 2.47.3, reused from the bitmap's preferred pack alone
// (gitproc.MultiPackReuse) — is 1.50–1.57 s full and 0.91–0.98 s of the
// node's CPU a clone however many arrive: eight at once 3.5–3.6 s at the
// median, thirty-two 8.4–9.0 s (p99 14–15 s) and 29–30 s of CPU for
// thirty-two identical answers. depth1+blobless+cone is 0.87–0.89 s and
// 0.37–0.38 s of CPU a clone, thirty-two at once 12 s of CPU. So identical
// clones cost the node their count in CPU, which a cache of identical
// answers would make one: the figure a pack-objects response cache is decided
// on, and it is the same with MULTI-PACK REUSE. With the pools on 2.48.0,
// which reuses from every pack of the bitmap verbatim, a full clone is
// 1.51–1.52 s alone and 1.03–1.04 s of CPU, 0.98–0.99 s a clone at
// thirty-two at once (31–32 s of CPU), and depth1+blobless+cone the same
// 0.37 s: reuse from the preferred pack alone costs this repository's
// clones nothing measurable — 2.48 spends some 6% more CPU on a full one —
// so the image's 2.47 gives up nothing, and the response cache's figure is
// the single-pack one.
package host
