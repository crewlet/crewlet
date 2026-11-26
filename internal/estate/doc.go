// Package estate routes every node's reads and writes of the replicated estate
// to a data node whose copy of it serves.
//
// The decision it carries is ADR-0025: a node without the `data` role reaches
// the estate through a node that holds it, and every rule below is one clause
// of what makes that sound. Its amendment: the client that node ran became a
// ROUTER that every node runs, and a data node's own seats reach its own copy
// through the same path, with the same floors. EVERY DATA NODE HOLDS THE WHOLE
// ESTATE and SERVES it over the broker ([Serve]); every node's tools, the
// engine's record of its runs ([ExecutorWrites]) and the operator's surfaces get
// facades of one [Router], which answers in-process where this node's copy serves
// and asks another data node otherwise. The operator included: read off the
// node's copy, it was answered from one the node had stopped serving.
//
// # One node per request, never a scatter
//
// The request goes to exactly one node, `crewlet.estate.<node>` (or a push's
// proposal subject, below). A scatter would run a write on every data node, and a
// queue group would let the BROKER pick the answerer, so the router could not say
// which node it asked when no answer comes: a retry on "whoever answers next" is
// only safe where the write itself says so. So the router picks: this node first
// where its copy serves, then the node that answered the domain last, then
// [internal/rendezvous]'s order of this node's id, a node whose copy said out of
// service after the rest and a silent node last (reported by [Placement.Unanswered]
// only after a wait of the router's own, since a caller that hung up said nothing
// about it). The [Placement] answers from a WATCHED view of the presence leases,
// never a per-call read of the coordination store, and says UNKNOWN rather than an
// empty or stale fleet. A push is asked in its repository's order, a read of git
// objects in its order over the Gitlet nodes, and a data node asks its own copy
// at its place in a key's order, never ahead of the node the key prefers; the
// failover rule is stated per operation (see [opClass]).
//
// # What makes a retry safe, per operation
//
// A READ is safe to ask anywhere. A TRACKER write carries an operation id the
// tool minted once per caller-visible operation, and the tracker's ledger answers
// a repeat with what the first copy wrote, so a write left unanswered, or answered
// UNVOUCHED (the ledger cannot say whether it landed), is asked of the next data
// node. A PAGE write repeats as the same operation only where the CALLER brought a
// key ([doclet.Actor.OpKey]), from which the store derives the operation id; with
// none a repeat is a second write or a refusal of its own first copy, so an
// unanswered unkeyed page write is NEVER repeated and is reported as
// [ErrOutcomeUnknown]. A CHAT write is the page write's case
// ([chatlet.Actor.OpKey]); a PUSH carries its push id and an EXECUTOR write the id
// its log derives, and both move on as a tracker write does.
//
// # A push is asked of its repository's committer
//
// Every other operation is asked of THIS node first, then of the node that
// answered last: any data node's copy answers equally. A push's decide is a
// compare-and-set on the repository's ONE subject, and committers each holding
// part of a burst lose rounds to each other, every lost round costing a re-decide
// and a re-append of the WHOLE batch. So `code.propose` is asked of the
// repository's COMMITTER, the first node in [rendezvous.Order] of the repository's
// id over every live data node (which every node computes alike), and of its
// runner-up when it runs nothing. No sticky node: the key decides. Two nodes with
// different views of the fleet both commit SAFELY, only less efficiently.
//
// Keyed by the repository the same way, each under its asker's operation id (from
// which derived ids come, so no retry repeats the effect): `code.compact`, which
// waits in the repository's one queue so everything that moves its packs meets on
// one node; `code.expunge`; `code.change`; `code.queue`; `code.merge`, one record
// the merge gate decides from one snapshot (ADR-0096); `code.conversation`,
// whose change writer publishes on the change's own subject; and `code.checks`,
// whose checks writer decides one write per repository at a time ([op.keyedBy]). A
// committer that refuses before appending anything ran nothing, so the runner-up
// is asked with the same push id and the refuser is asked LAST for the next
// [suspectFor]. A committer answers INSIDE THE ATTEMPT that asked it, busy and
// undecided once a batch and its answer no longer fit, so an asker never hands a
// push to the runner-up while the first committer is still deciding it. Proposals
// are answered on a LANE OF THEIR OWN (`crewlet.estate.<node>.proposals`, only the
// operations declared on it, [op.answeredOn]) because a proposal WAITS holding its
// answer slot, and queued pushes on the shared registration took every slot.
//
// A node that answers "I did not run it" executed nothing, so every class moves
// on, as it does from a READ a copy refuses for a reason about ITSELF
// ([statelog.ReadRefusal.Elsewhere]). `log_full`, `broker_refused`,
// `broker_stalled` and `maintenance` hold for every node alike and are answered
// from the first node that gives them. A WALK PAYS FOR A READ ONCE: a refusal says
// what the read had paid for ([statelog.Refused.Spent]) and every node asked after
// is handed that ([request.Spent], [statelog.WithSpent]), so none re-waits a
// position already waited out. When no data node runs the operation it is refused
// as [ErrUnserved], never an empty answer, which would say the company has none of
// what was asked for. A WRITE some node never answered may have run, so the walk
// ends in [ErrOutcomeUnknown] and the same operation id is the retry.
//
// # Who originates a write: a surface, or the engine
//
// The class says what a failover may repeat; the ORIGIN, who may write
// ([opOrigin]). Every write but one log's is a SURFACE's ([SurfaceWriteDomains]),
// Gitlet's own work too (a compaction, a mirror's sync, a queue's merge): it
// carries out what somebody asked of a repository, so its refusals are that
// repository's.
// The executor log's writes are the ENGINE's, which no surface may make (ADR-0073): they go through a writer
// facade the engine alone holds ([ExecutorWrites]), and two gates hold the class:
// every write on that log is declared the engine's, and no package that serves a
// person or a model names that facade or the domain's writer. Its reads are
// ordinary ([ExecutorReads]).
//
// # A domain's copy that is WRONG is out of service; one that LAGS is a worse node
//
// A domain's copy that is wrong (applier halted, node evicted from its log, rows
// below that log) answers none of that domain's operations
// ([LocalBackends.For] false): a peer is told `out_of_service` and the node's own
// seats' calls go to other data nodes. Every other domain's copy keeps answering,
// because routing is on an operation's OWN domain. An OPERATION that reads another
// domain's rows declares it ([op.alsoReads]) and runs only where every copy it
// reads serves. A search's semantic half, over the vectors, is best effort and
// never routes ([search.Copies]). This is ADR-0079, which refines ADR-0025's
// amendment to one domain at a time; the copy a SEAT is served by is every domain
// that gates seat admission at once ([SeatCopy]).
//
// Whether a copy answers ([Backend.Answers]) is its distance from its log, not its
// correctness: the floors hold an answer to what the caller must see. So a lagging
// copy is passed over only for one that does not lag, and asked again, told to take
// the request anyway ([request.AcceptLagging]), once every other has run nothing.
//
// # A node behind the company's configuration is a worse node too
//
// A data node the control plane has CONFIRMED behind the epoch the fleet
// activated (shed or stuck, never on lag alone, which every rollout produces;
// [Backend.Current]) gives up its seats and goes on serving the estate: its copy is
// sound. What it supplies from its own epoch is not: the chart a tracker write
// resolves by, the unit a read renders, a search's scope, the limits a push is
// decided under. Each is a VALUE whose stale reading every reader already meets
// when a revision changes one, so the node is a worse node to ask, asked again,
// told to answer anyway, when none else runs the operation ([worse]). It is the
// WORSE of the two: the last resort asks every merely lagging copy first, then the
// nodes behind the configuration ([Router.lastResort]), an order of the REASONS.
// Refused instead, a fleet whose only data node failed an apply answers
// nothing. It covers EVERY operation, because a list of which operation reads which seam is
// a second list to keep in step with the decides. It answers `behind_config`
// ([unservedBehindConfig]), because unlike a lag it LASTS, so the asker REMEMBERS it
// for thirty seconds ([Router.markBehindConfig]) and asks the node only as the last
// resort, a keyed operation asking the runner-up.
//
// A GRANT judged from the chart is different: answered anyway by the last resort,
// it would be decided by the revision that may have revoked it. So an operation
// that judges one DECLARES it ([op.judgesGrant]), and a node behind the
// configuration refuses it even asked last, `behind_config` saying it decides no
// grant, so the walk ends unserved. That is the HARD ARM; the window a stale grant
// is judged in is bounded by the control plane's confirmation, about a minute.
// TestEveryOperationThatJudgesAGrantIsDeclared and the engine's
// TestEveryEpochSeamAServingDecideReadsIsClassified hold it.
//
// # A repository's git work is asked of its home Gitlet node
//
// An operation on a repository's git OBJECTS (a read by object id, a MERGE
// PREVIEW, [opCodeMergePreview], and an ENGINE WRITE, a commit built beside a copy
// of them and landed as a push, ADR-0092) is answered by a node with the `gitlet`
// role from its own cache, never by a data node's copy. That is ADR-0089, amending
// ADR-0025: the walk has a second placement, every live Gitlet node
// ([RouterOptions.Git]), keyed by repository ([op.keyedOnGit]), and such an
// operation names the git half rather than a domain's copy ([GitCopy]), so no copy
// verdict, dispatch floor or lag gate holds it back; its floor travels in its
// arguments. It is asked in rendezvous.Order(repository, every live Gitlet node):
// the repository's HOME first, its runner-up next, the keyed order's demotions, no
// sticky node, on lanes of their own ([laneGitReads], [laneGitWrites]). The home is
// a preference, never ownership: no lease, no epoch. A Gitlet node that cannot
// answer inside its budget answers `behind` ([reply.Unready]) and is not demoted.
// ONE node a walk FILLS a copy at a time (gitlet.GitQuery.Home), so a repository
// nobody holds costs one download; the fill moves on only past a node bringing no
// copy up for the operation, so a home that will not serve for a lasting reason
// never leaves its repository unreadable ([Router.routeGit]). Every attempt is
// SERVED UNTIL ITS ANSWER IS DUE (gitlet.AnswerBy), so a busy Gitlet node is heard
// saying so rather than taken for a gone one.
//
// AN ENGINE WRITE IS ONE PUSH WHICHEVER GITLET NODE BUILDS IT ([opCodeGitWrite]):
// its push id and commit are pure functions of its caller's operation
// (gitlet.WritePushID), so the walk asks the next node after one that ran nothing
// or went silent. Its attempt is the router's own ([Router.budgetFor]): a turn's
// minutes waited out on a wedged home would leave its runner-up never asked.
//
// [Router.Home] is the first node of the same order with NO demotion, UNKNOWN,
// never a guess, where who serves git is unreadable. [Router.Maintainer] places a
// repository's MAINTENANCE (compaction, publication of its layers) on the home
// unless it cannot hold the work, else on the first node of the order whose
// ADVERTISED background budget holds the work's charge; a node whose row carries no
// figure is NOT SAYING, never a node with nothing to give, so NOTHING MOVES PAST
// THE HOME ON MISSING INFORMATION. Both of internal/gitlet/compact's loops ask by
// one charge, the largest of their steps', so both writers of a repository's packs
// run on one node (ADR-0091).
//
// # What a git attempt sums
//
// [GitReadAttempt] and [GitWriteAttempt] each sum a Gitlet node's waits at their
// WHOLE budgets, never p99s: a node that cannot fit a wait says `behind` while its
// asker still listens. The
// read lane's queue and the short processes that find what a read names are LEFT
// OUT: at their whole bounds the attempt would be minutes, and a read they hold
// past its due instant is answered unready (`share`) so the walk asks the next
// node. An engine write's attempt includes [minWriteAttempt] (the proposal's own,
// which the committer answers busy with any less: gitlet.MinProposeAttempt) and
// gitlet.WriteUploadAllowance.
//
// # Read-your-writes: ONE floor table per node, consulted for local reads too
//
// The next read may be answered by another data node than the one that took the
// write, or by this node before it has applied it. So each node keeps ONE
// [Session]: a HIGH-WATER per stream of the furthest position any write it made
// reached. Every request carries the floor on the log of the operation's own domain
// ([opSpec.floorStream]), and whichever node answers, this node's own copy
// included, waits for its applier to reach it, bounded by [statelog.ReadBudget],
// and says it is behind rather than answer from before a write this node was told
// landed. Every wait a node answering ANOTHER node makes ends gitlet.ReplyMargin
// short of the asker's deadline ([statelog.WithReplyMargin]), because a `behind`
// sent after the asker stopped waiting is heard as a gone node. Floors on another
// DOMAIN's log are never waited for: no operation reads another domain's rows AT A
// POSITION ([ready]). The tracker's and knowledge base's searches carry no floor but
// name their domain, so a node whose copy is out of service runs none. CHAT'S
// carries one, because a seat's own join just wrote the membership that decides its
// rooms ([ChatSearchAnswer.Building]).
//
// # The router's timing
//
// Every wait a serving node makes before it answers ends a reply's margin short of
// the attempt, so a node that runs out of time says so (`behind`, `no_quorum`,
// `broker_stalled`, `pending`) while its asker still listens.
// [NewRouter]
// refuses, naming the field and the arithmetic, an attempt too short to do its job
// ([minReadAttempt], [minWriteAttempt], [minGitWriteAttempt]). A field left ZERO
// or negative is refused, never read as a default: a zero attempt is no bound ([attemptContext]
// takes the caller's deadline unchanged), and a zero admission budget ([opPing])
// reads as a fleet with no data node.
//
// # Every envelope is signed, and bound to its question
//
// A data node runs a tracker write AS THE ACTOR ITS REQUEST NAMES and answers a
// chat read FOR THE VIEWER IT NAMES, so an unsigned request was ADR-0031's
// unsigned record one hop earlier. Every request and reply is signed under the
// fleet keyring ([recordseal.Signer], [Envelopes]), the rule internal/signedask
// keeps: a request that does not verify is never dispatched, and a key a node does
// not hold is "did not run". A request carries an id and a deadline under the
// signature, so a replay is refused; a reply names its request; a reply it cannot believe is no answer, never
// "did not run", so a once-write met with one is [ErrOutcomeUnknown]. Its wait
// crosses as a duration, counted from arrival on the serving node's clock
// ([request.BudgetMS]): an additive field, so a mixed fleet serves both ways.
//
// # What does not cross the wire, and who supplies it
//
// Everything in-process by nature (the chart seams a query carries,
// [worklet.Units]; the lead map a dependency consults, [worklet.Leads]; the org a
// knowledge search is scoped by) is supplied by the SERVING node from its own
// current epoch ([ServerSeams]). The gate in wire_test.go walks every type an
// operation carries and fails on any field that would silently not arrive, unless
// it is named, with its reason, in [carriedByServer].
//
// THE IDENTITY DIRECTORY IS THE OPPOSITE CASE. A data node answers FACTS about a
// login or a seat ([IdentityReader]); the verdict is the asking node's, since the
// chart a binding is resolved against and the Tier A tokens it declares are its
// own. A binding crosses NARROWED ([Binding]): credential verifiers and grants
// never reach the broker. A MACHINE TOKEN'S ROW crosses narrowed differently
// ([TokenFacts]), because the asker decides on nearly all of it. The VERIFIER is
// withheld and the presented SECRET never leaves the asker: it sends the token's id
// and a nonce, and gets back a MAC of the verifier under a key both nodes derive
// from the fleet keyring, matched against the MAC of the verifier the presented
// token implies. So credential.CheckToken decides what it decides on a data node,
// over a row nobody on the broker can test a guess against.
//
// CHAT'S VIEWER IS THE MIDDLE CASE. The asking node decides who the viewer is; the
// SERVING node decides which rooms that viewer may read, from membership judged in
// the read's own snapshot, so [ChatQuery] has no field for a visible set. What a
// caller may do BEYOND membership crosses as the DECISION
// ([chatlet.MessageAuthority], [chatlet.ChannelAuthority], [removeCommentArgs]).
//
// # A request is read whole, or runs nowhere
//
// A rolling upgrade mixes builds, and a data node that decoded a newer build's
// request ran it without every key it did not declare. So an operation's INPUTS,
// its arguments and the actor it runs as, cross under one rule
// (wirerule.go, ADR-0025's amendment): the router leaves every field at its zero
// off the wire ([encodeInput]), so a field a later build adds is on a request only
// where the request sets it, and its zero means what a build without it does
// ([define] states the rule); and a serving node refuses every member its build does not
// declare, at every depth ([decodeInput]), before it waits for a floor or runs
// anything, answering [ErrUnknownKeys]. A read carrying a newer argument is
// refused alike, never answered with the argument ignored. The router reads that
// as a node that RAN NOTHING, whatever the class, and asks the next
// ([Router.settle], [unreadHere]): a keyed operation asks the refusing node last
// for requests carrying what it refused, and those alone ([Router.markUnread]),
// and a refusing home passes its fill to the runner-up ([passesFill]). The answer of a read a
// caller carries into another request ([op.answeredWhole]) crosses under the same
// rule the other way ([opArtifactVersionCarried]); a value a caller hands back as
// it took it ([gitlet.CheckKey], [statelog.Position]) stays outside it. A write's answer is never refused once the write ran, and a member a later build adds to an input needs its own plan for two builds.
//
// # An answer is one message
//
// A reply past the transport's message is refused whole ([server.encodeReply]),
// so every read declares how large its answer grows ([answerKind]): one that
// answers rows reads them to its bound through statelog.RowsOf and says where
// it stopped the way it says any page is short.
//
// # Errors keep their identity
//
// A tool decides what to tell a model by asking errors.Is and errors.As, so an
// error crosses as its message plus every registered sentinel it matches and every
// registered type it carries (see errors.go), and the far side rebuilds an error
// that answers the same questions. A sentinel or error type added to the packages
// whose errors cross is a build failure until it is registered. AN UNEXPORTED
// ERROR TYPE IS A BUILD FAILURE TOO: the far side rebuilds a type from its
// exported fields, so it would arrive as its message alone, and each package's
// Sentence would reach its reader with "worklet:" or "doclet: invalid:" in front.
package estate
