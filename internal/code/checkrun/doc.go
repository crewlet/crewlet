// Package checkrun RUNS A REPOSITORY'S QUEUED CHECKS on a git node: every git
// node walks the queued attempts oldest first, claims the ones it ranks first
// for, makes each a box with no coding agent in it (internal/sandbox's
// AcquireBox), runs its command there and records how it ended — and a
// repository's HOME sweeps the attempts whose runner is gone.
//
// # Every git node runs checks, ranked per repository
//
// A check's attempt is queued on its repository's checks subject (internal/
// code's checks.go) and taken by whichever git node claims it first. Every git
// node walks the queue — a `checks_queued` read, the attempts oldest queued
// first ([code.ReadChecksQueued]) — and ranks each attempt by its repository's
// GIT ORDER (estate.Router.GitOrder: every live git node in the repository's
// rendezvous order, its home first): the node at rank r claims an attempt only
// once it has waited r × [ClaimStagger] since it was queued, so the home claims
// first, the runner-up a stagger later if the home did not, and so on — every
// node computing the same order from the same set, with no lease and no
// coordinator. The home holds the repository's warm copy, so its checks run
// beside it; a home whose cells are full, or that runs no container checks
// (git.max_checks 0), leaves its attempts to the next node a stagger later.
// REJECTED: home-only runners — a large company's checks are more boxes at
// once than one node's cores (the design's ~1 700 concurrent checks); a lease
// per check — some fifteen writes to the coordination store an attempt, where
// the claim is one record on the log the attempt already lives on.
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
// # Container cells under git.max_checks
//
// A container cell is a container on this node's own machine, a repository's
// build or tests running beside the code host's git, so at most git.max_checks
// of them run at once (Tier A: unset a quarter of the node's cores, at least
// one; 0 none). A runner with no free container slot claims no container
// check, and the next node of the attempt's order claims it a stagger later. A
// remote (e2b) cell runs on another machine and is not counted here.
//
// # Remote cells under the company's caps
//
// A remote cell's box is billed to the company's account with its provider,
// so it is bounded company-wide (code.checks.remote): at most max_running at
// once, each holding one of that many leased slots ([RemoteSlots]), and at
// most box_hours_per_day of box time started in a UTC day, charged a minute at
// a time to the fleet's shared counter. A runner takes its slot and its first
// minute BEFORE it claims, so a check the caps hold back is claimed by nobody
// and waits queued; a walk held back once tries no further remote check, a
// store that cannot say starts none, and a running box whose slot lapsed
// takes one again, past the cap where it must, so it is counted again.
// remote.go argues each choice.
//
// # What a check's box holds
//
// No coding agent, no model's sign-in, no subscription login, none of
// providers.sandbox.setup ([sandbox.Manager.AcquireBox] refuses each): a
// check runs a change's own code, and every credential beside it is one that
// code reads. Its environment is EXACTLY the company's check secrets its
// definition names (code.checks.env, each ${VAR} resolved when the box is
// made), what the check is (config.ReservedCheckEnv: CREWLET_CHECK_REPO, _SHA,
// _BASE, _NAME, _ATTEMPT and _API), and the code host's base and its check's
// token (CREWLET_GIT_URL, CREWLET_GIT_TOKEN) with git configured to present
// the token under the user `check` for that base alone — and no user.name or
// user.email, since a check acts as nobody. Its setup is the git floor's check
// and a checkout of its ONE COMMIT, depth one, blobless, with its cone
// (sandbox.CloneRecipe's single-sha) — or nothing at all for a check whose
// file says `checkout: none`, which reads its commit through /box/code/.
//
// # A run: the box, the command, the poll
//
// The box is made under [code.CheckBoxGrace] — the setup's budget, which the
// check's token outlives its timeout by — and its command started detached,
// its output and exit status written to files of the box's own. Every
// [CheckPoll] the runner refreshes the box's TTL, asks whether the command
// still runs, and reads the attempt's row: a row that has ENDED — cancelled
// for a head its change moved on from, ended by a person, or errored by the
// home's sweep — kills the box at once, and the runner records nothing more.
// Past the check's timeout — its file's timeout_seconds held to the company's
// code.checks.max_timeout_seconds, counted from the command's start — the box
// is killed and the attempt FAILED `timed_out`: the timeout is the check's
// own, so running past it is a measurement of the commit, never
// infrastructure's. An exit is passed for 0 and failed for any other status,
// with the last [code.CheckTailBytes] of its output on a failure. A box that
// could not be made is `errored box_failed`, a setup step that failed
// `errored setup_failed` — both infrastructure's, which the home queues again
// below code.AutoRequeueAttempts. The box is closed on every path, under a
// context its caller's cancellation does not reach.
//
// A CHECK SPENDS NO TOKENS. No model runs in its box and nothing it does is
// charged to any account: what it costs is its box's time, which its row
// records as its duration.
//
// # A runner that is gone
//
// A runner that dies holding attempts leaves their rows running, and nothing
// it would have done will happen. The repository's HOME sweeps them
// ([Sweeper], a worker of internal/code/home's loop, registered by the engine,
// which this package does not import): a running attempt whose runner is NOT
// IN THE REPOSITORY'S GIT ORDER — a node that left the fleet — or whose claim
// is older than its check's timeout, the setup's grace and [SweepPolls] polls — a
// runner that restarted, and so is in the order again without the run — is
// ended `errored runner_lost`, the box of a remote cell killed through its
// provider. A container box on the dead node is its host's own container
// runtime's, reclaimed there (the local backend's orphan reaping) rather than
// by any other node. The home queues such an attempt again, so a dead
// runner's check runs at most code.AutoRequeueAttempts attempts. A runner that
// was only slow and ends its attempt after the sweep replaces runner_lost
// with its measurement, which the row's join keeps.
//
// A RUNNER THAT STOPS — its node draining — ends each attempt it holds
// `errored runner_lost` itself before it closes the box, so the home queues it
// again at once rather than a sweep later.
//
// # Its imports
//
// internal/code/query, internal/code/compact, internal/code/host,
// internal/code/home and this package never import each other: each reaches
// the others' work through the estate's router. The sweep reaches the home's
// loop through the loop's own interface, which [Sweeper] satisfies, and a
// check's token reaches this package as a [Tokens] the engine adapts from the
// code host's.
package checkrun
