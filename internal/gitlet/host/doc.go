// Package host is SMART HTTP FOR PERSONS AND CODING BOXES: Gitlet's git
// surface, the one landing every push takes from its pack to an applied row,
// the hook relay that hands git's ref updates to it, the engine writes it
// builds and lands the same way, and the wait that keeps an `ok` honest.
//
// # A push is committed by a record, and git is told so only from its row
//
// A person's `git push` reaches receive-pack in a SCRATCH repository whose one
// alternate is this node's cache, never in the cache: git migrates a push's
// objects into the repository it runs in before any hook can refuse them, and a
// refused push's objects would then be served by a cache that is a function of
// the rows (ADR-0084). The one hook, proc-receive, relays every ref update to
// the engine; the engine checks what only it can (a forced update under a
// protection, a blob over the limit), uploads the push's one pack bundle to the
// object store, and proposes the push to the repository's committer through the
// estate router. git is answered `ok` for a ref only from
// the push's own APPLIED row: a durable record a gate then drops on every node
// is not a push that landed, so `pending` is waited on, at most
// statelog.StallGrace, and never reported `ok`.
//
// # One landing, whoever built the push
//
// Everything the engine decides about a push once its objects are a pack in a
// scratch is ONE function, land (land.go), in this order: the repository's
// STATUS (a deleted, purging or archived repository takes no push, so no bundle
// is stored for one); the force and stale checks, the stateless rules and the
// protections; the blob scan over the push's own pack, refusing only commands
// still left to decide, so the refusal that came first is the one the pusher
// reads; --atomic; nothing left to land still records its attempt (gitlet.Attempted); the stored size against gitlet.MaxPackBytes; the repository's
// pack bound (gitlet.MaxPacksPerRepo), refused compaction_behind only once the
// row, read again, still holds it, while the committer's decide stays the
// authority; a coding box's pack held to the engine's committer (below) before
// any byte is uploaded; the bundle uploaded and sealed while the commits it
// introduces are listed; the proposal; the outcome, `ok` only from the applied
// row; and the adoption of its pack into this node's cache, so the node that took
// a push does not download it back.
//
// The landing does NOT check that the scratch's objects are WHOLE: that is its
// BUILDER'S: for a push git received, receive-pack's connectivity check and fsck,
// before the hook reaches the relay; for an engine write, its check of every
// object it names and `index-pack --strict` over every object it made. A builder
// that hands over a scratch missing an object gets a row naming a history no
// cache can serve.
//
// The FORCE CHECK belongs to the landing because Command.Forced is formed there
// and nowhere else, and protected_force turns on it: a builder forming its own
// would be judged by a check it chose; so it PARSES THE PROTECTIONS ITSELF, from
// the synced row its scratch leans on. Its two callers, the relay and an ENGINE
// WRITE, never hold a second copy of its steps (ADR-0008):
// TestEveryPushTakesTheOneLanding holds the proposal, the upload and the
// adoption to it.
//
// # An engine write is built beside the copy and lands as a push
//
// A change a seat or person makes through a tool (files committed, a commit
// reverted or picked, a branch created or deleted) is BUILT on the Gitlet node
// (Server.Write, write.go) and LANDED as a push (ADR-0092): judged on the copy
// synced to its caller's floor, every refusal judged again once at the log's
// head before it is answered; built in a scratch named by its push id by
// git's plumbing over exactly the trees the change touches, never an index of
// the whole tree (TestAnEngineCommitReadsNoWholeTree), under one unit of the
// node's seat git-work share and one budget (gitlet.WriteBuildDeadline); and
// handed to land with gitlet.ViaEngine. Its push id (gitlet.WritePushID) and its
// commit's author, committer and dates are pure functions of the caller's
// operation and the parent, so a retry, on this node or another, is the same
// push and commit, and a second ask of one push id on this node joins the first.
//
// A CHANGE'S MERGE (merge.go) is built onto the target's tip but LANDED AS A
// MERGE RECORD (landing.Merge), authored by who merged (a squash by the
// change's owner, each other author a Co-authored-by trailer), where a push's
// proposal goes, and built again onto a target that moved at most
// gitlet.MergeRecomputes times, all within gitlet.MergeBuildDeadline; a target
// merging through its queue takes an enqueue. A CHANGE'S REBASE (rebase.go)
// lands as one forced push of its source, under gitlet.RebaseBuildDeadline.
//
// # A push's walks go by generation, and are bounded by the history
//
// A push walks its repository's history up to four times (the force check's
// merge-base, the listing of what it introduces, a blob's path search, and
// receive-pack's connectivity check). Where git has no generation number to stop
// on it walks BY DATE, so a commit dated before its parent (a person's clock
// behind) means walking every commit. Two rules:
//
//   - EVERY WALK THE ENGINE ASKS FOR GOES BY GENERATION where the cache's copy
//     has a commit-graph, which the maintainer writes into a copy first
//     (internal/gitlet/cache's GRAPH FIRST): merge-base --is-ancestor does by
//     itself, and the listings ask rev-list for --topo-order (scan.go)
//     (TestAPushsWalksGoByGeneration).
//   - NO WALK IS BOUNDED BY THE PUSH ALONE. Each is given what a walk of the
//     whole history may cost (gitproc.HistoryWalk, per live object) beside what
//     the push's bytes earn, because the walk's cost is set by the repository, so
//     a small skewed push into a large history is slow and never refused.
//
// RECEIVE-PACK'S CONNECTIVITY CHECK IS GIT'S, with no flag the engine can add,
// and walks by date with a graph or without: bounded by the history term of
// gitproc.ReceivePackDeadline.
//
// # Every answer that names a ref is read at its request's level; an object id has no level
//
// A person's v0 advertisement, v2 `ls-refs` and push advertisement read the code
// log through a barrier, so a node in a minority answers 503 rather than an old
// head; a coding box's are read at its run's session (below). A v2 capability
// advertisement names no ref and an object fetch names ids that mean the same
// bytes everywhere, so both take whatever the cache holds, and a fetch whose
// wants this node lacks takes ONE linearizable sync first; the barrier is single-flighted per log and the sync per repository. A check's box learned
// its want from its attempt, so its wants are looked for at its check's session
// . The host offers no ref-in-want, because git's upload-pack
// and fetch-pack disagree on the order of a shallow fetch's sections once a
// client fetches by want-ref (cache.ServeConfig).
//
// # A fetch request is spooled, and bounded by the repository it names
//
// git's http-backend buffers a fetch request whole, up to 10 MiB, which a
// partial clone breaks: a checkout's ONE lazy fetch is fifty bytes a want. So a request is held in memory AS LARGE AS IT IS, up to
// a mebibyte (RequestMemoryBytes), charged to a node-wide allowance; the rest is
// SPOOLED to an unnamed file on the cache's volume (cache.Spool), which the
// kernel removes however the request ends. It is bounded by the most a request
// to ITS repository can legitimately name, fifty bytes for each live object and
// ref beside the mebibyte (RequestBound). Spools draw on an allowance of their
// own, never the cache's budget, a quarter of it one credential's
// (SpoolCredentialBytes; TestBloblessCheckoutsBehindOneTokenAllSucceed), and a
// body claims at most SpoolRequestBytes while it arrives. A request that finds no
// room WAITS ITS TURN rather than being refused, since git does not retry a
// checkout's lazy fetch: first come first served within its credential, in
// arrival order across credentials, a wait ending, unserved, short of the minute
// a balancer allows a quiet git request (spool.go). The request is
// GIVEN BACK when the answer's FIRST BYTE is written, since a retry happens only
// before it; for a fetch that byte is its PACK's (body.go), so it is held through
// pack-objects' enumeration, never through the transfer its client paces.
//
// # A refusal is decided before the bytes, and said in git's own words
//
// Everything that can be refused without objects is refused at the advertisement
// (a token bound to nobody, an archived or deleted repository), where git prints
// the sentence as `fatal: remote error: ...`; on a POST it is a per-ref report,
// `! [remote rejected] ... (<sentence>)`, written only after the request body is
// read to its end, because a response sent while git is still uploading is lost
// to a broken pipe. Refusals are never decided on a stale read: anything that
// would refuse is read again through the barrier first, but for the pack bound,
// read again at SESSION floored there (land.go's behind), since a barrier would
// append a record to the code log for every retry a repository at its bound meets.
//
// # A damaged cache is repaired under the request that found it
//
// Every git process the host runs over a repository's cache that fails before
// its first byte with damage the cache names (cache.DamageReason; only
// cache.ShapeReason where the cache is merely the alternate) has the cache
// REPAIRED and runs once more over it (repair.go): the cache is disposable, so
// damage it can name is its own to repair, never the client's to retry. A shape
// or a derived file is repaired in place; a pack or an object is fetched again. A failure whose process began before the cache last wrote a repair is run once more and is no repair.
//
// A FAILURE THE REQUEST REPAIRS IS THE REQUEST'S: every process run IN the
// incarnation names the request's reader on its Cmd (gitproc.Cmd.Repairer,
// cache.Synced.Repairer), so what it reports is held for this request's repair
// rather than making the copy's scrub due beside it (cache's report.go). DAMAGE
// IS READ FROM GIT'S FORMAT STRINGS, NEVER ITS WORDS: upload-pack copies its
// request's text into the messages it dies with, so the pool classifies a run by
// the format of each trace2 report its process tree makes (gitproc), and a
// request quoting a damage sentence forces no rebuild. A damage git names only
// through a bare "%s" (a corrupt packed object) is a HINT naming a pack, repaired
// only once the cache has held that pack to its row (cache.Cache.ConfirmHint).
//
// A FETCH'S FIRST BYTE IS ITS PACK'S: pack-objects meets damage after
// upload-pack has written its answer's first line, so a fetch's answer is held
// until the pack begins (band.go): its first band-1 packet, or a band-2 line that
// is no report of git's (`fatal: `, `error: `, `warning: `, `BUG: `), judging
// only a line's prefix, so a fetch whose pack-objects dies first is repaired.
//
// Every failure before the first byte is answered as what it is (refuse.go),
// identified by what failed, never by elimination; a refusal of the engine's OWN
// call is a 500 that promises
// no retry, since none ends a call-site bug. Table tests hold every error gitproc
// and the cache export to a decided answer, both ways.
//
// # Who pushes
//
// A person, with their own machine token as the Basic password, pushes as that
// person, and a Tier A token or service account bound to a human seat nobody
// else holds as that seat's holder; a credential bound to no seat fetches only;
// every push is one operator_acted record on the receiving node, transport git, kept from admission and settled however the request ends
// (pushAudit), so a push that failed on its way is recorded failed rather than at
// all. WHO is asking is the API guard's answer (iam.From), never looked up a
// second time here. This package imports nothing from the engine or the estate:
// its seams (Estate, Audit) are declared here.
//
// What the admitted request may do is its GRANT (gitlet.GrantOf, ADR-0088), built
// from ONE principal (the admission's principal()) and asked with no roster,
// since a person's grant reads no chart. The advertisement asks it first
// (Grant.PushRefusal); a push that skipped it is refused per command by
// Grant.RefRule, and recorded under the audit word `unbound` (grantRefusal),
// since its remedy is a binding in the identity directory, not a grant.
//
// # A coding box is admitted by its run
//
// A seat's coding box is git's other client (ADR-0093), under /box/git/, with its
// RUN'S credential: Basic user name BoxUser and a
// per-run token (BoxTokens, key domain BoxKeyDomain) as the password. The API's
// guard never reads it. admitBox (boxadmit.go) admits it by the run's ROW, read
// on every request (Runs), the token a backstop of RunTokenTTL; three-valued, so
// a store that cannot be read is 503, never a 401; matches the repository by the
// NAME the run was given before the code log is read, so a box guessing names
// costs the log no barrier; refuses a push of a seat a person stopped; and holds
// the seat's own grant, judged by this node's current chart, to the run's
// (gitlet.Grant.Within) and, for a run a colleague seat woke (BoxRun.Ceiling), to
// that colleague's (gitlet.Grant.Under). Its reads of refs are at the run's
// SESSION (admission.refs): an applied push raises the floor to its row before
// `ok` (raiseFloor), because git builds a push's thin pack against the refs its
// own advertisement named and a balancer may send the two requests to two nodes.
// Everything after admission reads the admission alone and never asks which
// admitter ran (TestNothingAfterAdmissionAsksWhichAdmitterRan); a box's push is no
// operator_acted, its push row naming its run.
//
// A BOX COMMITS NOTHING AS THE ENGINE. The engine's committer,
// `engine@<code.identity_domain>` (Signer.Committer), marks the commits the host
// built and authenticates nothing, and a box runs code nobody vouches for: so a
// box's push whose PACK holds a commit committed as it that the repository does
// not already hold is refused forged_committer whole (landing.EngineCommitter),
// before any byte is uploaded. The pack is read, not the commits its updates
// introduce, because it is stored whole; a commit at a time (one cat-file each,
// header only), so the read never holds the pack's sum of commit text, which a
// box may inflate far past the pack's bytes. A person's push is never read for
// its committers. A commit the repository already holds is no forgery (the
// engine's own commits on a branch deleted since a box checked it out), so each
// is asked of the copy, and a push may bring back maxEngineCommits of them.
//
// # A check's box is admitted by its attempt's row
//
// A box running one ATTEMPT of one check of one commit reaches the same routes
// with its CHECK'S credential: Basic user name gitlet.CheckUser and a token
// (CheckTokens, in BoxKeyDomain), a backstop of the check's timeout plus
// gitlet.CheckBoxGrace, the attempt's ROW the authority (checkadmit.go). It
// FETCHES, over protocol v2, and nothing else; the row is read stale, and through
// the barrier only where the stale copy holds no such attempt, an ENDED row
// refused at once however stale (a check's state only rises); and every read is
// at the CHECK'S SESSION, the position its attempt was queued at, which its
// commit's push precedes. It is shown NO REF and fetches its commit by id. It
// acts as NOBODY, so its fetch of a `visibility: unit` repository is recorded by
// its attempt's row, not as a principal's read. The API's /box/gitlet/ admits the
// same way (AdmitCheck), its reads held to that commit (gitlet.GitQuery.Check).
//
// # A file's bytes and a commit's ZIP are served inert beside git
//
// download.go answers a person's two downloads under /git/ in the transfer lane,
// a large ZIP in one of the node's archive places, inert (ADR-0153).
//
// # Every transfer is charged to the credential that asked for it
//
// The same admission says whose TRANSFER every git process of the request is
// (admission.share, gitproc.Cmd.Share). The holder of a human seat is a PERSON,
// through any credential of theirs; a credential acting as nobody is a MACHINE,
// by the credential's name, so a token per CI runner is a turn per runner. A box
// is a RUN (a check's box one keyed `check:<id>`), and the run class holds at
// most half the lane. When the node's transfer slots are all held, gitproc's fair
// share hands a released one to the class furthest below its weighted share of
// what the lane has spent lately (person 4, machine 2, box 1), then to the
// principal that spent least, each run charged the greater of its CPU and the
// slot-time it held. So a CI credential's storm of clones or lazy fetches waits
// behind every credential of its class that asked less, and the credential that
// spent most may be answered busy after gitproc.SlotWait where arrival order
// would have served it: its own burst is what the share holds back. It is the
// whole answer to a blobless clone's lazy fetches, with no limiter of the host's
// own beside it.
//
// # What a request served is counted where its answer ends
//
// Each request adds its use of its repository to the cache's heat (cache.Heat),
// ONCE, however its answer ends (Server.served): an upload-pack advertisement
// is a FETCH; a fetch served to its end that offered no `have` a CLONE
// (negotiation.clone); a PUSH is counted where recorded (pushAudit).
//
// # Who read a unit's repository is recorded where its pack was sent
//
// A fetch of a `visibility: unit` repository is recorded ONCE (Options.Fetched,
// types.CodeFetched via `git`), the reader as its admission names it, where the
// request ENDED WELL and its answer CARRIED A PACK, because what an audit asks is
// who was sent the repository's contents. The fetch sink's own answerScan decides
// that the pack began, passively, never a second pkt-line decoder (ADR-0008). So
// a stateless negotiation of several rounds is ONE read, and an ls-refs, the
// advertisement and a fetch the node ended record nothing
// (TestAUnitRepositorysFetchIsRecorded); nor
// does any read of a `visibility: company` repository, whose readers are every
// seat by declaration. The read is filed in the node's reads ledger
// (internal/gitlet/readaudit), which publishes it OFF THE REQUEST'S PATH.
//
// # The hook relay cannot end the process
//
// The relay runs outside net/http's per-handler recovery and parses bytes a
// pusher controls. So every relay connection's goroutine turns a panic into that
// push's refusal, the engine still running.
//
// # Measured
//
// The figures a constant rests on (git 2.56.0, two shared cores, one run each):
//
//   - A skewed commit with no commit-graph costs a walk 4.5-5.0 us a commit, so
//     gitproc.HistoryWalk is 11 us an object.
//   - A rebase of gitlet.MaxRebaseCommits commits builds in 3.1 s, so
//     gitlet.RebaseBuildDeadline's forty seconds hold it: PROVISIONAL until a
//     production p99 moves it.
//   - A chain pack at gitlet.MaxChainObjectBytes takes 2.1 s, so
//     gitlet.ChainPackDeadline's ten seconds hold it.
package host
