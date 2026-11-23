// Package checkrun RUNS A REPOSITORY'S QUEUED CHECKS on a Gitlet node: every Gitlet
// node walks the queued attempts oldest first, claims the ones it ranks first
// for, makes each a box with no harness in it (internal/sandbox's
// AcquireBox), runs its command there and records how it ended — and a
// repository's HOME sweeps the attempts whose runner is gone.
//
// # Every Gitlet node runs checks, ranked per repository
//
// A check's attempt is queued on its repository's checks subject (internal/
// gitlet's checks.go) and taken by whichever Gitlet node claims it first. Every Gitlet
// node walks the queue — a `checks_queued` read, the attempts oldest queued
// first ([gitlet.ReadChecksQueued]) — and ranks each attempt by its repository's
// GIT ORDER (estate.Router.GitOrder: every live Gitlet node in the repository's
// rendezvous order, its home first): the node at rank r claims an attempt only
// once it has waited r × [ClaimStagger] since it was queued, so the home claims
// first, the runner-up a stagger later if the home did not, and so on — every
// node computing the same order from the same set, with no lease and no
// coordinator. The home holds the repository's warm copy, so its checks run
// beside it; a home whose containers are all running, or that runs none
// (gitlet.max_checks 0), leaves its attempts to the next node a stagger later.
// REJECTED: home-only runners — a large company's checks are more boxes at
// once than one node's cores (the design's ~1 700 concurrent checks); a lease
// per check — some fifteen writes to the coordination store an attempt, where
// the claim is one record on the log the attempt already lives on.
//
// A WALK READS ONLY WHAT IT CAN CLAIM: the queue, one range oldest queued
// first, and only while this node can start a check now — its CURRENT
// catalogue makes containers and one is free — read no further once none is.
// A node with every container running reads nothing, however long the queue:
// read whole, every Gitlet node would page the queue to its end at every walk
// while it claimed nothing. A company that runs no check
// (config.Company.ChecksRefusal: it sets no providers.sandbox.local.image) has
// its queue read whole, every attempt ended `errored defs_invalid` by the node
// whose turn it is, saying why, whatever that node's catalogue.
//
// # The claim before the box
//
// A runner CLAIMS an attempt — a check_started record naming itself — BEFORE
// it makes a box, and makes one only where its claim holds: the checks writer
// refuses a second claim of an attempt another runner holds (`claimed`), and
// where two writers each landed one across a failover the join keeps one, so
// the runner whose claim the row does not keep reads it back and makes
// nothing. Every lost race costs a record, never a box. REJECTED:
// create-then-claim, which made a box for every lost race. Its box made, the
// holder records the box's handle on the attempt with a second check_started
// repeating its claim, which the writer sets once.
//
// # Containers under gitlet.max_checks
//
// A check runs in a container made from the company's
// providers.sandbox.local.image on this node's own machine: a repository's
// build or tests running beside Gitlet's git, so at most gitlet.max_checks of
// them run at once (Tier A: unset a quarter of the node's cores, at least one;
// 0 none). A runner with no container free claims no check, and the next
// node of the attempt's order claims it a stagger later.
//
// # What a check's box holds
//
// No harness, no model's sign-in, no subscription login, none of
// providers.sandbox.setup ([sandbox.Manager.AcquireBox] refuses each): a
// check runs a change's own code, and every credential beside it is one that
// code reads. Its environment is EXACTLY the company's check secrets its
// definition names (code.checks.env, each ${VAR} resolved when the box is
// made), what the check is (config.ReservedCheckEnv: CREWLET_CHECK_REPO, _SHA,
// _BASE, _NAME, _ATTEMPT and _API), and Gitlet's base and the path of the
// file holding its check's token (CREWLET_GIT_URL, CREWLET_GIT_ACCESS_FILE),
// with git configured to present it under the user `check` for that base
// alone — and no user.name or user.email, since a check acts as nobody. Both
// bases, Gitlet's and _API's, are written as the container dials them: one
// naming the engine host's loopback, under the runtime's name for the host
// ([sandbox.Manager.EngineHost]). Its setup writes that file (the token is in
// no variable), checks the git floor and checks out its ONE COMMIT, depth one,
// blobless, with its cone (sandbox.CloneRecipe's single-sha) — or no checkout
// at all for a check whose file says `checkout: none`, which reads its commit
// through /box/gitlet/.
//
// # A run: the box, the command, the poll
//
// The box is made under [gitlet.CheckBoxGrace] — the setup's budget, which the
// check's token outlives its timeout by — and its command started detached,
// its output and exit status written to files of the box's own. Every
// [CheckPoll] the runner reads the attempt's row and asks whether the command
// still runs: a row that has ENDED — cancelled
// for a head its change moved on from, ended by a person, or errored by the
// home's sweep — kills the box at once, and the runner records nothing more.
// Past the check's timeout — its file's timeout_seconds held to the company's
// code.checks.max_timeout_seconds, counted from the command's start — the box
// is killed and the attempt FAILED `timed_out`: the timeout is the check's
// own, so running past it is a measurement of the commit, never
// infrastructure's. An exit is passed for 0 and failed for any other status,
// with the last [gitlet.CheckTailBytes] of its output on a failure, and the
// tests its JUnit report counted on either. A failed run's output lines are
// counted in its box over the bytes its tail ends at, and an ending a data
// node refused for those counts, which nothing ran, is sent again without them
// ([Options.Unread]). A box that could not be made is `errored box_failed`, a
// setup step that failed `errored setup_failed` — both infrastructure's, which
// the home queues again below gitlet.AutoRequeueAttempts. The box is closed on
// every path, under a context its caller's cancellation does not reach.
//
// A CHECK SPENDS NO TOKENS. No model runs in its box and nothing it does is
// charged to any account: what it costs is its box's time, which its row
// records as its duration.
//
// # What leaves the box is redacted before it is cut or kept
//
// The box holds the check's secrets and its token, which its command can
// print. So every text an ending carries out of it — its summary, each
// failure's message and test id, its output's tail and its kept log — is
// redacted (internal/redact) BEFORE it is cut to its bound or kept: cut first,
// a credential the cut splits is no shape the redaction knows. A bound cut at
// the read is read past by what the redaction reads around a place (a
// failure's body by redact.MaxReach). A test id is redacted because a
// parametrised test's spells its values; a failure's path is kept as written.
//
// The tail and the log are an output's end read from inside it, so each is
// read redact.LookBack past its bound and shows only what those bytes settle
// (redact.SettledEnd), opening on a line: the tail is cut (TailCut) only where
// its last line is longer than it, and the lines it numbers are those it
// shows, a key's block one marker.
//
// THE KEPT LOG IS STREAMED from the box's end through one redact.Stream into
// the store, so output of any size costs the stream's windows and the read's
// transfer, never the whole output. A stream holds neither of SettledEnd's two
// redactions whole, so where the log begins is found over the read's first two
// LookBacks (its context and as much past it), and the stream's redaction is
// dropped up to there, the dropped bytes checked against the head's: exact
// where the two redactions part within those bytes, which is LookBack's own
// promise. A marker can be longer than what it replaced, so a read of the
// bound can redact to more than it: the stream hands the store at most the
// bound, and past it the rest is counted, the same bytes read again (refused
// if the output moved meanwhile) and the redaction dropped to the first line
// that leaves the rest within the bound. A log that cannot be kept so, or that
// the stream refuses (redact.ErrDecidedEarly), is not kept, its summary saying
// why.
//
// # A runner that is gone
//
// A runner that dies holding attempts leaves their rows running, and nothing
// it would have done will happen. The repository's HOME sweeps them
// ([Sweeper], a worker of internal/gitlet/home's loop, registered by the engine,
// which this package does not import): a running attempt whose runner is NOT
// IN THE REPOSITORY'S GIT ORDER — a node that left the fleet — or whose claim
// is older than its check's timeout, the setup's grace and [SweepPolls] polls — a
// runner that restarted, and so is in the order again without the run — is
// ended `errored runner_lost`. Its container is on the runner's own node,
// which no other node's runtime reaches, so the sweep ends the row and kills
// nothing: that node's runner reclaims the box (below). The home queues such
// an attempt again, so a dead
// runner's check runs at most gitlet.AutoRequeueAttempts attempts. A runner that
// was only slow and ends its attempt after the sweep replaces runner_lost
// with its measurement, which the row's join keeps.
//
// A RUNNER THAT STOPS — its node draining — ends each attempt it holds
// `errored runner_lost` itself before it closes the box, so the home queues it
// again at once rather than a sweep later.
//
// # A box its attempt left
//
// A box outlives its run where the runner's process ends without closing it (a
// crash, a kill): a container on the runner's node running `sleep infinity`,
// holding its disk and memory, and perhaps still the check's command. Nothing
// on the box ends it, since no box has a timer, and the node's orphan reaper
// cannot judge it: a container exists for as long as it is left, and the
// reaper reads no attempt's row. So a check's box RECORDS ITS ATTEMPT
// (sandbox.Spec.Check, which sandbox.Manager.AcquireBox requires), the reaper
// leaves every box that records one, and the node's runner reclaims them: at
// its start and at each walk, every check box on its node whose attempt it
// does not run itself and whose row has ended, runs in another box or is gone,
// or whose record names no attempt it reads, is killed through the backend its
// record names (Local's Kill: `docker rm -f`, then, once the runtime lists the
// container no more, its directory) and logged once. A box whose close at its
// run's end could not remove its container is left the same way, recorded, as
// is one a failed acquire made and never handed over (its create's answer
// lost, or its teardown refused), whose attempt ends `errored box_failed`. A
// row still running in the box, or naming none yet (its box still being made),
// keeps it: a predecessor's attempt is ended first, by the home's sweep above,
// and its box reclaimed at a walk after. Only the node can do it, since no
// other node's runtime reaches its containers.
//
// Each judgement reads the attempt's row at `linearizable`, since a kill is
// final and a copy behind the claim holds no row; a walk judges at most
// [ReclaimPerWalk] boxes, and a box kept, or one not judged or killed, waits a
// poll for the next, so a box left costs the reads a running check does. A
// kill that fails keeps the box, listed by its record, for that next judgement;
// one the runner's stop cut short would leave it to the runner's next start,
// so the kill runs under a context the stop does not reach.
//
// # Its imports
//
// internal/gitlet/query, internal/gitlet/compact, internal/gitlet/host,
// internal/gitlet/home and this package never import each other: each reaches
// the others' work through the estate's router. The sweep reaches the home's
// loop through the loop's own interface, which [Sweeper] satisfies, and a
// check's token reaches this package as a [Tokens] the engine adapts from
// Gitlet's. The engine builds the runner, and decides where one runs and when
// it stops (internal/engine's codechecks.go).
package checkrun
