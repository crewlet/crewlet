// Package host is SMART HTTP FOR PERSONS AND CODING BOXES: the code host's git
// surface, the one landing every push takes from its pack to an applied row,
// the hook relay that hands git's ref updates to it, the engine writes it
// builds and lands the same way, and the wait that keeps an `ok` honest.
//
// # A push is committed by a record, and git is told so only from its row
//
// A person's `git push` reaches receive-pack in a SCRATCH repository whose one
// alternate is this node's cache — never in the cache, because git migrates a
// push's objects into the repository it runs in before any hook can refuse
// them, and a refused push's objects would then be served, and connected
// against, by a cache that is supposed to be a function of the rows
// (ADR-0054). The one hook, proc-receive, relays every ref update to the
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
// left to land; the stored size against code.MaxPackBytes; the repository's
// pack bound (code.MaxPacksPerRepo), a push carrying a pack refused
// compaction_behind only once the repository's row, read again, still holds
// the bound, so that a retry whose own sync finds the bound uploads nothing
// while the committer's decide stays the authority — a push whose sync found
// the repository below a bound its committer finds reached is refused there,
// its bundle stored and named by no row; a coding box's pack held to the
// engine's committer (below), before any byte of it is uploaded; the bundle
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
// (receive.fsckObjects) — and both run before the hook ever reaches the relay;
// for an engine write, its check of every object it names before it builds,
// and the same fsck (`index-pack --strict`) over every object it made.
// The landing reads neither: a push with no pack lists nothing, and a pack's
// listing walks commits, never their trees or blobs. A builder that hands over
// a scratch missing an object gets a row naming a history no cache can serve.
//
// The FORCE CHECK belongs to the landing because Command.Forced is formed
// there and nowhere else, and protected_force turns on it: a builder that
// formed its own would be judged by a check it chose. The landing PARSES THE
// PROTECTIONS ITSELF, from the synced row its scratch leans on, so no builder
// can judge a push against rules other than its own repository's as the
// push's sync read them. It has two callers: the relay, which reads git's
// commands off the hook and hands them over with what the receive handler
// already judged, and an ENGINE WRITE (Server.Write, below), a push the
// engine builds itself from objects it wrote into a scratch — a second caller
// of the same function, never a second copy of its steps (ADR-0008):
// TestEveryPushTakesTheOneLanding holds the proposal, the upload and the
// adoption to it.
//
// # An engine write is built beside the copy and lands as a push
//
// A seat's or a person's change made through a tool — files committed, a
// commit reverted or picked, a branch created or deleted — is BUILT on the
// git node (Server.Write, write.go; the build, build.go) and LANDED as a push
// (ADR-0062): judged on the copy synced to its caller's floor, and every
// refusal judged again once at the log's head before it is answered; built in
// a scratch named by its push id, by git's own plumbing over exactly the
// trees the change touches — never an index of the whole tree, which
// TestAnEngineCommitReadsNoWholeTree holds — under one unit of the node's
// seat git-work share and one budget of git run time
// (code.WriteBuildDeadline); its one self-contained pack indexed by the
// relay's own fsck (`index-pack --strict`); and handed to land with
// code.ViaEngine. Its push id is an unnamed derivation of the caller's
// operation (code.WritePushID), and its commit's author, committer and dates
// are pure functions of the operation and its parent — the author the acting
// principal as the Signer names it, the committer the engine, the author date
// the operation's second and the committer date never before the parent's —
// so a retry, on this node or another, is the same push and the same commit,
// and a second ask of one push id on this node joins the first.
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
// # Every answer that names a ref is read at its request's level; an object id has no level
//
// A person's v0 advertisement, v2 `ls-refs` and push advertisement read the
// code log through a barrier, so a node in a minority answers 503 rather than
// an old head; a coding box's are read at its run's session (below). A v2 capability advertisement names no ref and an object fetch
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
// of their own, never the cache's budget, a quarter of it one credential's: a
// spool takes its room as its client sends and holds the whole request with
// its head moved in from memory; while its body arrives it claims at most
// SpoolRequestBytes, and every grant keeps the bodies arriving able to finish
// oldest first, so a body waits only for a spool that ended or a body older
// than itself, and a slow client holds what it has sent and no turn. A request
// that finds no room WAITS ITS TURN for it rather than being refused, since
// git does not retry the lazy fetch a checkout makes: first come first served
// within its credential, the node's in arrival order across credentials, a
// request waiting to start holding its head in a place of the waiting room and
// nothing more, and a wait ending, unserved, a keepalive short of the minute
// a balancer is told to allow a quiet git request (spool.go) — which a cold
// sync of the requests ahead can outlast, so on a cold node where it does a
// token whose spooled requests pass its quarter fails the rest. The wants are read back from
// wherever the request is, as a stream, by the negotiation's parse and by the
// cache's check of what it lacks, and never held whole; and the request is
// GIVEN BACK when the answer's FIRST BYTE is written, since a retry over it
// happens only before that byte. For a fetch that first byte is its PACK's
// (body.go): upload-pack writes its opening line, then runs pack-objects, so
// the request and its spool are held through pack-objects' enumeration — at
// most a progress line or one keepalive (5 s) for a side-band client, and
// until the raw pack otherwise — never through the transfer its client paces.
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
// through the barrier first — but for the pack bound, whose push was received
// over a row read through the barrier already and is read again at SESSION
// floored there (land.go's behind): that read sees every compaction the copy
// answering it has applied, and one the repository's committer has applied
// and that copy not yet refuses this run and is met by the next — a barrier,
// which would see that one too, would append a record to the code log for
// every retry a repository at its bound meets.
//
// # A damaged cache is repaired under the request that found it
//
// Every git process the host runs over a repository's cache — an
// advertisement, upload-pack, the check of a fetch's wants, a push's
// receive-pack in its scratch — that fails before its first byte with damage
// the cache names (cache.DamageReason; only the shape, cache.ShapeReason, for
// a process whose directory merely has the cache as its alternate) has the
// cache REPAIRED — a shape, or derived files git could not read (a
// multi-pack-index, a commit-graph), in place with no download; a pack or an
// object in place first, each pack that fails fetched again over its files
// with one GET, and into the next incarnation only when neither that nor a
// scrub of the copy finds what to fetch again — and runs once more over the
// repaired one (repair.go): the cache is disposable, so damage it can name is
// its own to repair, never the client's to retry. A failure whose process
// began before the cache last wrote a repair into the copy is answered from
// the copy as it stands and run once more, and that answer is no repair: the
// run after it is repaired as though first. A FAILURE THE REQUEST REPAIRS IS
// THE REQUEST'S: every process run IN the incarnation names the request's
// reader of it on its Cmd (gitproc.Cmd.Repairer, cache.Synced.Repairer), so
// what it reports — a pack, an object, a hint, a crash — is held for this
// request's repair rather than making the copy's scrub due beside it (cache's
// report.go): a pack's or an object's repair consumes it, a shape's or a
// derived file's in place carries it to the copy the request runs again over
// (so the pack repair a retry escalates to answers it), and the request's
// release of the copy hands on whatever it did not repair (past its first
// byte, never asked, or put right by an in-place repair alone).
// Without it every one-pack repair raced a read of the whole copy, which also
// decided which of the two repaired the pack. DAMAGE IS READ FROM GIT'S FORMAT
// STRINGS, NEVER ITS WORDS: upload-pack copies its request's text into the
// messages it dies with ("unexpected line: '<arg>'"), so the pool classifies a
// run by the format of each trace2 report its process tree makes, which
// carries none of the request's text (gitproc) — a request quoting a damage
// sentence forces no rebuild, and damage upload-pack meets itself before its
// first byte (a want it cannot parse) is repaired under the request that met
// it, like any other process's. A damage git 2.56 names only through a bare
// "%s" — a corrupt packed object — is a HINT naming a pack, and repaired only
// once the cache has held that pack to its row and found it is not
// (cache.Cache.ConfirmHint); the same sentence is a row on 2.55, and
// its pack is confirmed the same way beside the damage the row names, so the
// repair is told which pack to fetch again (cache.Failure.Packs) rather than
// look for it. A FETCH'S FIRST BYTE IS ITS PACK'S: upload-pack
// writes its answer's first line before its pack-objects runs, and
// pack-objects meets damage after it — "invalid MIDX object position, MIDX is
// likely corrupt", over a multi-pack-index bitmap whose reverse index is
// damaged. So a fetch's answer is held until the pack begins (band.go): its
// first band-1 packet, or a band-2 line that is no report of git's
// (`fatal: `, `error: `, `warning: `, `BUG: `), which judges only a line's
// prefix and never what it names — a fetch whose pack-objects dies before its
// pack has shown the client nothing, and is repaired and run again like any
// other, where it was answered in git's words on every clone of the copy,
// never repaired. And every failure before the
// first byte is answered as what it is (refuse.go), identified by what failed
// and never by elimination: the code log failing (marked where it is met, so its
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
// A person, with their own machine token as the Basic password, pushes as that
// person, and a Tier A token or service account the identity directory binds
// to a human seat nobody else holds pushes as that seat's one holder; a
// credential bound to no seat fetches only; and every
// push is one operator_acted record on the receiving node, transport git — the
// third person transport — kept by the push's request from its admission and
// settled however the request ends (pushAudit), so a push that failed on its
// way to its run is recorded failed rather than not at all. WHO is asking is
// the API guard's answer, read off
// the request (iam.From): the seat is the principal's own, which the guard
// resolved through the identity directory and the running org, so this
// package looks nobody up a second time. It imports nothing from the API but
// its auth and paced leaves, and nothing from the engine or the estate: its
// seams (Estate, Audit) are declared here and filled by the engine, and its
// object store is the code.Putter that code.StoreBundle — the one store of a
// pack bundle, sealed on its way — is handed.
//
// What the admitted request may do is its GRANT (code.GrantOf, ADR-0058),
// built from ONE principal — the admission's principal(), the credential and
// the seat the guard resolved it to, the same value the push's session and
// proposal carry — and asked with no roster, since a person's grant reads no
// chart:
// maintain for a bound person, read for a token bound to nobody. The
// advertisement asks it first (Grant.PushRefusal), so a token bound to nobody
// is refused there, in git's words, before anything is sent; a push that
// skipped the advertisement is refused per command by Grant.RefRule, asked
// beside code.StatelessRefusal before the sync, with not_permitted and the
// grant's own sentence on every ref. Such a push is still recorded under the
// audit word `unbound` (grantRefusal), the class the dashboard and crewlet
// read for it, since its remedy is a binding in the identity directory rather
// than a grant.
//
// # A coding box is admitted by its run
//
// A seat's coding box is git's other client (ADR-0063), under /box/git/ on the
// public listener beside its other two edges, with its RUN'S credential: git's
// Basic user name BoxUser and a per-run token (BoxTokens, key domain
// BoxKeyDomain) as the password, handed to the box, with the rest of its git
// configuration, in the environment of the launch the token names. The API's
// guard never reads it, and the API's box route on a node serving no code host
// checks it through the admission's own credential step (ChallengeBox) before
// it tells the box why, since that reason is the deployment's state on a
// public route. admitBox (boxadmit.go)
// admits it by the run's ROW, read on every request (Runs, the engine's
// adapter over the fleet's pending store) — the token is a backstop of
// RunTokenTTL, the row the authority — three-valued, so a store that cannot be
// read is 503 and never a 401; matches the repository by the NAME the run was
// given before the code log is read, so a box guessing names costs the log no
// barrier; refuses a push of a seat a person stopped (Runs.Stopped: a detached
// run is not fenced by a pause); and holds the seat's own grant, judged by
// this node's current chart, to the run's (code.Grant.Within), judging none on
// a node with no chart or one behind the configuration. Its reads of refs are
// at the run's SESSION (admission.refs): every answer naming refs raises the
// run's floor to its copy before its first byte, and an applied push raises it
// to its row before `ok` (raiseFloor), because git builds a push's thin pack
// against the refs its own advertisement named and a balancer may send the two
// requests to two nodes. Everything after admission reads the admission alone
// — its grant, its freshness, its principal and its share — and never asks
// which admitter ran (TestNothingAfterAdmissionAsksWhichAdmitterRan); a box's
// push is no operator_acted, its push row naming its run.
//
// A BOX COMMITS NOTHING AS THE ENGINE. The engine's committer,
// `engine@<code.identity_domain>` (Signer.Committer), marks the commits the
// host built and authenticates nothing, and a box runs code nobody vouches
// for: so a box's push whose PACK holds a commit committed as it — compared
// folding case — that the repository does not already hold is refused
// forged_committer whole (landing.EngineCommitter, the admission's
// engineCommitter), before any byte of the pack is uploaded, so nothing is
// stored or proposed. The pack, not the commits its updates introduce: it is
// stored whole, with the objects of an update refused beside one that lands
// and any a client sent that no ref reaches, each the repository's once it
// lands (committedAs). A person's push is never read for its committers, and
// may carry the engine's commits as git carries any. A commit the repository
// already holds is no forgery — the engine's own commits on a branch deleted
// since a box checked it out — so each is asked of the copy the scratch leans
// on in one cat-file (forged), and a push may bring back maxEngineCommits of
// them, a longer history in parts.
//
// # A check's box is admitted by its attempt's row
//
// A box running one ATTEMPT of one check of one commit reaches the same
// routes with its CHECK'S credential: the Basic user name CheckUser and a
// token (CheckTokens, in BoxKeyDomain beside a run's and told from one by its
// subject) naming the attempt's key and the repository's name its box was
// given, valid for the check's timeout and code.CheckBoxGrace — a backstop,
// the attempt's ROW the authority (checkadmit.go). It FETCHES, over protocol
// v2, and nothing else; its URL's name is matched to the token's before the
// code log is read, so a box probing names costs the log nothing; the row is
// read stale, and through the barrier only where the stale copy holds no such
// attempt, an ENDED row refused at once however stale (a check's state only
// rises, so an ending is for good); and every read is at the CHECK'S SESSION,
// the position its attempt was queued at, which its commit's push precedes. It
// is shown NO REF — its advertisement v2's, its `ls-refs` answered empty — and
// fetches its commit by id, which protocol v2 serves of any object present.
// It acts as NOBODY, so its fetch of a `visibility: unit` repository is
// recorded by its attempt's row and never as a principal's read, and it is
// charged as a run keyed by its repository (`check:<id>`). The API's
// /box/code/, a check's box's reads of its commit by object id, admits the
// same way (AdmitCheck) and asks the code domain's git reads as the engine's
// own, held to that commit (code.GitQuery.Check).
//
// # Every transfer is charged to the credential that asked for it
//
// The same admission says whose TRANSFER every git process of the request is
// (admission.share, gitproc.Cmd.Share): both advertisements, upload-pack and
// receive-pack. The holder of a human seat is a PERSON, by the seat's handle —
// a person through any credential of theirs, so their laptop, scripts and
// browser are one principal, as their grant is one; or a Tier A token or
// service account the identity directory binds to a seat nobody else holds —
// and a credential acting as nobody is a MACHINE, by the credential's name
// (`token:<id>`, a service account's `pat:<id>`), never by the account behind
// it, so a token per CI runner is a turn per runner. A coding box is a RUN, by
// its launch, and the run class holds at most half the lane (gitproc's
// ShareClass.Cap), the one class the share does not let fill it. When the
// node's transfer slots are all held, gitproc's fair share hands a released
// one to the class furthest below its weighted share of what the lane has
// spent lately (a person 4, a machine 2, a box 1), then to the principal that
// spent least, each run
// charged the greater of its CPU and the slot-time it held: so a CI
// credential's storm of clones or lazy fetches waits behind every credential
// of its class that asked less, and a person's clone takes the next slot
// released while people have spent less lately than twice what the machines
// have — past that, the machines' third of the lane comes round first. No
// waiter waits longer than gitproc.SlotWait in any order, and the credential
// that spent most in its class may wait all of it and be answered busy where
// arrival order would have served it: its own burst is what the share holds
// back. That is also the whole answer to a blobless clone's lazy fetches: a
// blame of a file of a thousand commits is two hundred and one of them
// (BenchmarkLazyBlame), each an advertisement and an upload-pack charged to
// the credential that asked, with no limiter of the host's own beside the
// share.
//
// # What a request served is counted where its answer ends
//
// Each request adds its use of its repository to the cache's heat
// (cache.Heat), ONCE, however its answer ends (Server.served): an
// advertisement of upload-pack served is a FETCH — every git process that
// reads a repository opens with one, so a fetch of many negotiation rounds is
// one, and one that finds its client up to date is one too; a fetch transfer
// served to its end that offered no `have` is a CLONE (negotiation.clone): a
// fresh clone of any shape, and a partial clone's fetch of what it lacks, which
// git asks for the same way — so the blame above counts two hundred and one of
// each, all light; a PUSH is counted where it is recorded (audit), the one
// place every push the node admits reaches exactly once, however its request
// ends (pushAudit) — refused or failed at its advertisement, or at its POST
// whatever became of it; and every answer adds the bytes git's process wrote
// its client (sink.Sent). [Server.Status] reads it back — the node's cache, the
// hottest repositories of the hour and the day with their copies in service,
// and one repository asked about by id — and the engine answers its peers
// with it on topics.CodeStatus; this package imports no queue.
//
// # Who read a unit's repository is recorded where its pack was sent
//
// A fetch of a `visibility: unit` repository is recorded ONCE
// (Options.Fetched, types.CodeFetched via `git`) — the reader as its
// admission names it, and the bytes its answer carried — where the request
// ENDED WELL and its answer CARRIED A PACK, because what an audit asks is who
// was sent the repository's contents. The fetch sink's own answerScan, the one
// that decides when a held preamble may go, decides that the pack began, and
// scans on past preambleHoldBytes where the hold gives up — passively,
// buffering nothing and delaying no write, never a second pkt-line decoder
// (ADR-0008). So a stateless negotiation of several rounds is ONE read,
// counted by the round whose answer carried the pack, and its other rounds,
// an ls-refs, the advertisement and a fetch the node ended record nothing
// (TestAUnitRepositorysFetchIsRecorded, TestAFetchTheNodeEndedIsNoRecord);
// nor does any read of a `visibility: company` repository, whose readers are
// every seat by declaration. The engine files the read in the node's reads
// ledger (internal/code/readaudit), which publishes each reader's reads of a
// flush as one event of the node's own log OFF THE REQUEST'S PATH — no fetch's
// answer waits on the broker for its record — and the node's usage publisher
// derives the day's repo_read record from those (internal/usage).
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
// pool running git 2.47.3 — Debian trixie's release, which the engine image
// installed when these were taken, built from source — and every client the
// machine's git 2.43.0, all on one machine: one data node and two git nodes
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
// request: the packed-refs file a cache writes itself, in place of reftable,
// cost a delta sync tens of milliseconds there and the push nothing.
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
// Those figures predate the trace stream every git process now writes
// (gitproc), so its cost was measured against the build before it — six
// counts each alternated between the two trees, medians, git 2.47.3, on four
// vCPUs of a 2.1 GHz Xeon: a warm fetch (one push, one client) takes
// 10.5 → 12.0 ms of the node's git CPU (+15%, its four processes) and
// 49.7 → 52.3 ms at the median (+5%); eight clients at once 10.2 → 11.2 ms
// a fetch and 69.3 → 66.0 fetches a second (−5%); and a warm full clone of
// one client (the WARM CLONE below: 1.50 s, 0.87 s of the node's git CPU
// here) moved by less than its own run-to-run spread (0.81–0.91 s of CPU).
// Only those arms were re-measured: a process's fixed cost is the largest
// share of the smallest requests, and the clone shows it vanish in a large
// one.
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
// 100 000 files in a thousand directories, a 334 MB pack) is about eight to
// ten times the sync and the full, depth1+blobless+cone and treeless+cone
// clones, and less for the other two: the sync 13.4–13.8 s, past
// MidxRoundPacks so it writes the round's index — 0.14 s of it
// (op=round-index), the GETs the rest; full 52–54 s alone and 103–105 s each
// for eight, 35–38 s of CPU a clone; depth1+blobless+cone 20 s for 33.8 MB
// and 3.3–3.8 s of CPU, 29 s each for eight; treeless+cone 21 s and
// 3.8–4.0 s, 32 s each for eight; blobless+checkout 42–44 s — five to seven
// times — its lazy fetch 5 000 553 bytes of wants, spooled past the mebibyte
// held in memory, and 14–15 s of CPU, under four times, 87–107 s each for
// eight; and op=blob 66–68 ms and 45–46 ms of CPU, under twice the time and
// four times the CPU. Every cold figure but
// op=blob begins with the sync, so a balancer between a person and a node
// that has not served a repository sees that long before git's first byte.
//
// ONE CREDENTIAL'S SPOOLING IS CAPPED at a quarter of the room
// (SpoolCredentialBytes, 256 MiB on this lane of sixteen), and a request past
// it WAITS ITS TURN (spool.go). It was once answered 503 at once, and before
// that the fifth of one credential's bodies arriving was: of eight blobless
// checkouts of the 100 000-file tree under ONE token, each lazy fetch spooled,
// a checkout failed on that 503 — git does not retry a lazy fetch. Eight such
// fetches, 40 MB, now fit one token's quarter whole, so under one token they
// would not queue either; the arm's clients are people, one token each, because
// it measures eight clones at once, and
// TestBloblessCheckoutsBehindOneTokenAllSucceed is what holds that every
// checkout behind one token is served.
//
// A WARM CLONE (BenchmarkCloneWarm) is from a node holding the repository's
// bitmap, every push adopted as it landed, the pools on the node pin (2.56.0)
// serving with single-pack reuse (cache.ServeConfig). The tree history's full
// clone is sent 40.8 MB and takes 1.67–1.69 s at the median and 1.00–1.01 s
// of the node's CPU alone, 4.4–4.5 s and 1.10–1.13 s a clone eight at once,
// 11.2–11.4 s (p99 18.7–19.1 s) and 1.07–1.14 s thirty-two at once; the
// edits history's — 26 files of 4 KiB edited over 200 pushes — is sent
// 25.3 MB and takes 1.30–1.34 s and 0.59–0.61 s of CPU alone, 3.8–3.9 s and
// 0.63–0.67 s eight at once, 11.9–13.5 s (p99 18.2–19.4 s) and 0.71–0.72 s
// thirty-two at once. depth1+blobless+cone is 3.4 MB and 0.41–0.46 s of CPU
// a clone of the tree, 111 KB and 53–62 ms of the edits, however many arrive.
// So identical clones cost the node their count in CPU, which a cache of
// identical answers would make one: the figure a pack-objects response cache
// is decided on. The same benchmark run with multi-pack reuse sent the tree's
// full clone 43.9 MB for 0.96–0.99 s of CPU alone and the edits' 40.4 MB for
// 2.38–2.39 s (3.1–3.2 s at the median, 8.3–8.6 s eight at once), and
// thirty-two full clones at once of either were not all served — a clone
// refused 503 once it had queued for a transfer slot past gitproc.SlotWait
// on four cores — while its depth1+blobless+cone clone of the edits took
// 44–50 ms of CPU against single's 53–62: so single sends the warm tree 7%
// fewer bytes for about the same CPU and the warm edits 37% fewer for a
// quarter of it, which is why it is the value (cache's BenchmarkPackReuse
// holds the two side by side over the cache's own layout, and cache/serve.go
// says why).
//
// A LAZY BLAME (BenchmarkLazyBlame) — `git blame` in a blobless clone of a
// 200-line file that 1 000 commits rewrote a line at a time, by a token bound
// to nobody, from a node holding the bitmap, the pools and the client on the
// node pin (2.56.0), one count of three blames on four E-cores of an Intel
// Core Ultra 7 265U — is 201 lazy fetches, a version of the file each and
// each an advertisement and an upload-pack (402 transfer-lane runs), bringing
// 412.5 KiB and costing the node 1.1 s of CPU, about 5.5 ms a fetch, in 7.2 s
// of the client's wall clock. Every one is charged to the token's share,
// which is the whole bound on what such a storm costs anybody else.
//
// A CODING BOX'S CLONE (BenchmarkBoxClone, boxbench_test.go) — a box launched
// as the engine launches one, its steps run through its shell under the
// engine's box git environment, against a node holding the repository warm,
// git 2.56.0 at both ends unless named, on two E-cores of an Intel Core Ultra
// 7 265U, from memobj, the mean of ten clones (one at a gigabyte): the coding
// recipe of a 100 MiB history (44 800 commits) took 1.25 s (0.94 s on the
// client floor's 2.40.4) and was sent 16.7 MB — every commit and tree, 134 538
// objects — for 345 ms of the node's CPU, in two fetches, the clone's and the
// checkout's one lazy fetch of the tip's files (0.57 MB); with a cone of one
// directory 1.03 s and 16.3 MB; the check's recipe 0.13 s, 0.57 MB and 143
// objects. At a gigabyte (455 680 commits) the coding recipe took 14.6 s for
// 179 MB, 1.37 million objects and 4.3 s of the node's CPU, with a cone 13.3
// s, and the check's 0.99 s and 5.9 MB: a coding clone is its history's
// commits and trees, and a cone narrows only the files its checkout writes.
//
// A BOX'S OWN GIT IN A WIDE TREE (BenchmarkBoxWorktree), medians of ten: at
// 100 000 files of 4 KiB in directories of 48 a warm `git status` took 142 ms
// on git's own index settings, 99 ms on the engine's — index version 4 and the
// untracked cache, entries 6 and 7 of sandbox.BoxGit, which that cut adopted
// with the checkout 15.6 s against 15.4 — and 62 and 17 ms with git's
// monitor on, which the coding recipe therefore turns on where the box can
// keep it (its daemon's start, the first status, 417 ms); a one-file commit
// 172, 161 and 88 ms. At 10 000 files the status took 18, 15, 8.4 and 4.0 ms.
// A session of an agent's commands in a blobless clone — 200 rounds, its
// lazy fetches leaving 602 packs — never ran git's automatic gc, so gc.auto
// is left as git has it.
//
// BORROWING THE NODE'S COPY (BenchmarkBoxDissociate): a box's checkout and a
// build reading every file, by the coding recipe, took 1.14 s of a 100 MiB
// history and 13.0 s of a gigabyte (the box's git 1.03 and 12.2 s of CPU, the
// node's 0.36 and 3.7 s); by `clone --reference <the node's copy>
// --dissociate`, 2.59 and 47.8 s (the box's 3.4 and 58 s, the repack
// --dissociate makes being the whole history, and the node's 3 ms) — so a
// borrowed copy costs a box several times the clone it saves the node.
package host
