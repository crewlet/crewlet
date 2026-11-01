// Package estate routes every node's reads and writes of the replicated estate
// to a data node whose copy of it serves.
//
// The decision it carries is ADR-0025: a node without the `data` role reaches
// the estate through a node that holds it, and every rule below is one clause
// of what makes that sound. Its amendment: the client that node ran became a
// ROUTER that every node runs, and a data node's own seats reach its own copy
// through the same path, with the same floors.
//
// A node WITHOUT the `data` role keeps nothing that has to outlive it: its
// store is scratch, its broker is a leaf with JetStream off, and it holds no
// copy of the tracker's, the knowledge base's or the identity directory's rows.
// Its seats still read and write the first two, constantly — every board, every
// task, every page a tool touches — and ask the third whose record a login names
// and who holds each human seat, and a node that serves requests asks it what a
// presented machine token is checked against, so something has to answer them.
// EVERY DATA NODE HOLDS THE WHOLE ESTATE
// and SERVES it over the broker ([Serve]), and every node's tools are handed
// facades of one [Router], which answers in-process where this node's copy
// serves and asks another data node otherwise. So are the operator's surfaces
// — the API's tracker, knowledge-base and usage reads and the operator's own
// MCP — for the reason a data node's own seats are: read straight off the
// node's copy, a node whose copy of a domain was out of service answered its
// operator from the very copy it had stopped serving its seats from.
//
// # One node per request, never a scatter
//
// The request goes to exactly one node — `crewlet.estate.<node>`, or for a
// push's proposal that node's proposal subject (below) — over the queue's
// ephemeral request/reply verbs. A scatter would run a write on every
// data node, and a queue group would let the BROKER pick which node answers,
// which leaves the router unable to say which node it asked when the answer
// does not come: a failover that retries a write on "whoever answers next" is
// only safe where the write itself says so. So the router picks the node —
// this node first where its copy serves, then the node that answered the
// operation's domain last, then the order [internal/rendezvous] gives this
// node's own id, with a node whose copy of that domain said it is out of
// service after the rest and a node that went silent last; a push alone is
// asked in its repository's order instead, and a read of a repository's git
// objects in its order over the git nodes (below) — and the failover rule is
// stated per operation (see [opClass]).
//
// # What makes a retry safe, per operation
//
// A READ is safe to ask anywhere. A TRACKER write carries an operation id the
// tool minted once per caller-visible operation, and the tracker's ledger
// answers a repeat with what the first copy wrote — so a write that went
// unanswered is asked again of the next data node, which is exactly the
// lost-acknowledgement case that id exists for; and so is one a node answered
// UNVOUCHED, whose ledger cannot say whether it landed while another node's
// may. A PAGE write is the same operation on a repeat only where the CALLER
// brought a key — its actor's operation key ([pages.Actor.OpKey]), or for a
// remark made from a turn the turn's own ([pages.NewComment.TurnKey]) — from
// which the store derives the operation id, bound to what the write says. With
// none the store mints an id per call, so a repeat is a second write (a second
// comment) or a refusal of its own first copy (a create whose title is now
// taken): an unanswered unkeyed page write is NEVER repeated, and is reported
// as [ErrOutcomeUnknown] — the honest answer, and the one a tool turns into
// "read the page before writing again". A PUSH carries its push id, a UUIDv7
// the receiving node minted once, which the committer's decide collapses onto
// the push's row — so it moves on as a tracker write does.
//
// # A push is asked of its repository's committer
//
// Every other operation is asked of THIS node first and then of the node that
// answered last: any data node's copy answers it equally, and the sticky node
// is the one most likely to hold this node's writes. A push is different. Its
// decide is a compare-and-set on the repository's ONE subject, and every node
// that decides pushes to one repository contends on it: two committers each
// holding part of a burst lose rounds to each other, and every round one
// loses costs it a re-decide and a re-append of its WHOLE batch, where one
// committer holding the whole burst loses none and commits it in a few
// records (the code domain's group commit). So `code.propose` is asked of the
// repository's COMMITTER — the first node in [rendezvous.Order] of the
// repository's id over every live data node, which every node computes alike
// from the same set — and of that repository's own runner-up when it runs
// nothing. No sticky node: the key decides, not the last answerer. A committer
// that dies hands each of its repositories to that repository's runner-up,
// spread across the survivors. Two nodes with different views of the fleet
// both commit SAFELY — the subject's compare-and-set lets one record land per
// round and push ids collapse duplicates — only less efficiently, until their
// views agree.
//
// A COMPACTION'S PROPOSAL (`code.compact`) is asked of the same committer in
// the same order, keyed by the same repository: it waits in the repository's
// one queue beside the pushes and is decided there as a record of its own, so
// everything that moves a repository's packs meets on one node, and an
// unanswered one is asked of the runner-up under its own compaction id, which
// the domain's ledger collapses as it does a push id. An EXPUNGE RECORD'S
// PROPOSAL (`code.expunge`) — a person's purge request, or the begin or the
// commit a repository's maintainer built — is asked the same way, under the
// record's own operation id, and so is a WRITE OF A CHANGE'S LIFECYCLE
// (`code.change`) — an open, an edit, a draft's state, a retarget, a close or
// a reopen — under its asker's operation id, from which an open's change id
// is derived, so no retry opens a second change; and so is a WRITE OF A
// TARGET'S MERGE QUEUE (`code.queue`) — an enqueue, decided by the change's
// merge gate, or a dequeue — under its asker's operation id, from which an
// enqueue's entry id is derived, so no retry queues a change twice; and so is
// a change's MERGE (`code.merge`) — one record its merge gate decides from one
// snapshot, which merges the change and moves its target (ADR-0066) — under
// its builder's operation id, from which the merge's push id is derived, so
// no retry lands a branch twice. A WRITE
// OF A CHANGE'S CONVERSATION (`code.conversation`) — an ask for review, a
// comment or a thread's resolution — is keyed by the same repository and
// asked of the same node, whose change writer publishes it on the change's
// own subject rather than in the committer's queue, under its asker's
// operation id, from which a comment's id is derived.
//
// These are the operations keyed on the DATA nodes (a read of a repository's
// git objects is keyed on the git nodes: below); [op.keyedBy] is the
// declaration, and the walk is the ordinary one in every other respect — the
// floors, the verdict of the operation's own domain, the lagging last resort,
// and the class rule for a node that may have run it. A data node asks its
// own copy at its place in the key's order, in-process, never ahead of the
// node the key prefers; and a committer that refuses before it appends anything — stopping,
// a node in a maintenance mode, a refusal about this node — ran nothing, so
// the runner-up is asked with the same push id, and the refusing node is asked
// LAST for pushes for the next [suspectFor]: a keyed order has no sticky node
// to move the next push off it, and what made it refuse outlasts one request.
// A committer answers INSIDE THE
// ATTEMPT that asked it, whose end the request carries: it starts a push's
// batch only while a whole batch and its answer still fit before that end, and
// answers it busy, undecided, the moment they do not — so an asker never hands
// a push to the runner-up while the first committer is still deciding it.
//
// And a data node answers proposals, a push's and a compaction's, on a LANE OF
// THEIR OWN — its own subject, `crewlet.estate.<node>.proposals`, and its own
// answer slots — because a proposal WAITS: queued behind its repository's
// earlier pushes, then for its batch, it holds the slot it was answered
// through for as long as its asker's attempt, and a registration's requests
// wait behind every answer holding one of its slots. On the one registration
// every other operation is answered through, sixty-four queued pushes held
// every slot the node had: it answered no read, no other write and no git
// fetch's resolve until a batch ended, its readers marked it suspect for every
// domain, and an election that stalled the code log spread that across every
// committer with pushes in flight. The lane is as wide as one repository's
// committer holds (its queue and the batch ahead of it), so a burst meets the
// committer's own bounds — busy at once — never the lane's. A lane answers only the operations declared on it
// ([op.answeredOn]) and says so to any other, so the separation does not
// depend on every asker keeping it.
//
// A node that answers "I did not run it" — its copy of the operation's domain
// is out of service, it runs no native backend, that copy lags its log, the
// node is behind the company's configuration, it could not reach the caller's
// floor in time, or it could not verify the request — did not execute
// anything, so every class moves on from it. So does a READ a copy refuses for
// a reason about ITSELF ([statelog.ReadRefusal.Elsewhere]): its applier did
// not reach the barrier a linearizable read appended (`behind`), it stalled,
// it cannot decode a record, it is evicted or below the floor, it lost its
// broker or its quorum — another
// node's copy may serve the read at once, so the next one is asked and the
// refusal is the answer only where nobody does better. Returned as the answer,
// a git fetch through a data node whose applier fell a second behind was told
// 503 while two peers could serve it. `log_full`, `broker_refused`,
// `broker_stalled` and `maintenance` hold for every node alike — every node
// appends its barrier to the one stream, through its one member where it has
// one, and every node of a fleet in a capacity window is in one — and are
// answered from the first node that gives them.
//
// A WALK PAYS FOR A READ ONCE, whatever its length: a refusal says what the
// read had paid for ([statelog.Refused.Spent]) — the read index its barrier
// established, the position it waited its whole bound for, a log its barrier
// ran out on — and every node asked after it is handed that ([request.Spent],
// [statelog.WithSpent]): it waits for the read index rather than append a
// barrier of its own, waits for no position already waited out (a floor
// included, which a node behind it answers [unservedBehind] for), and appends
// no second barrier to a log whose barrier ran its whole budget — a log
// without a quorum, or, where one member stores it, that member's stalled
// write ([statelog.Spent.Unconfirmed]). Asked afresh, every node of a
// fleet one burst put behind together appended a barrier to the log they were
// all behind on and waited a whole read budget for it, so one final `behind`
// cost a barrier and two seconds per data node.
//
// When no data node runs the operation, it is refused as [ErrUnserved],
// naming who was asked and what each said — never an empty answer, which would
// say the company has none of what was asked for.
//
// # A domain's copy that is WRONG is out of service; one that LAGS is a worse node
//
// A domain's copy that is wrong — its applier halted, the node evicted from
// its log, its rows below that log — answers none of that domain's operations
// ([LocalBackends.For] false for it): a peer asking for one is told
// `out_of_service`, and the node's own seats' calls on that domain are
// answered by the other data nodes. Every other domain's copy keeps answering
// here, because an operation's routing is on its OWN domain: the rows it
// answers from are that domain's. The reads that cross into a second domain's
// rows say so. An OPERATION that reads another domain's rows declares it
// ([op.alsoReads]) — a page's backlinks, which name the tasks linking to it —
// and runs only where every copy it reads serves. A search's semantic half,
// over the vectors, is best effort and never routes: the search is answered
// from the copy of its own domain, and a participant whose VECTORS copy is
// out of service asks that verdict itself, in the scanner, and answers
// without the half, saying so ([search.Copies]). And the seats were never the
// problem, the copy was, so the node keeps them. This is ADR-0049, which
// refines ADR-0025's amendment to one domain at a time; the copy a SEAT is
// served by — what admission asks of — is every domain that gates seat
// admission at once ([SeatCopy]).
//
// Whether a copy answers requests ([Backend.Answers]) is a different question,
// asked of the operation's own domain too: its distance from its log, not its
// correctness. The floors and the read's
// own level hold an answer to what the caller must see, and a write is decided
// from the authority's own snapshot and arbitrated by the broker. So a copy
// that lags is passed over only for one that does not, and asked again — told
// to take the request anyway ([request.AcceptLagging]) — once every data node
// whose copy does not lag has run nothing. A single data node a burst put past
// the snapshot slack would otherwise refuse its own seats until it caught up,
// and a fleet the same burst put behind together would refuse everybody's.
//
// # A node behind the company's configuration is a worse node too
//
// A data node the control plane has CONFIRMED behind the epoch the fleet
// activated — shed or stuck, never on lag alone, which every rollout produces
// ([Backend.Current]) — gives up its seats, its schedules and its inbound work,
// and goes on serving the estate: its copy is sound. What it supplies from its
// own epoch (below) is not: the chart a tracker write resolves a people field
// and a lead by, the unit a read renders, a search's scope, the limits a push
// is decided under are the configuration the fleet replaced. Each is a VALUE
// whose stale reading every reader already meets when a revision changes one,
// so the node is a worse node to ask, as a lagging copy is — passed over for
// every data node that is neither, and asked again, told to answer anyway,
// when none of them runs the operation ([worse]). It is the WORSE of the two:
// a copy that only lags answers held to the caller's floors and under the
// current configuration, so the last resort asks every such copy first and
// the nodes behind the configuration after them, each in the order the walk
// reached it ([Router.lastResort]) — an order of the REASONS, the same on the
// request that learns a node is behind as on every one after. Asked in the
// order the walk met the refusals, the request that learned a committer was
// behind sent it the push while every later one sent the lagging runner-up
// its pushes, two nodes committing one repository in every window.
//
// Refused instead, a fleet whose only data node failed an apply — a
// stateless node beside one data node applies what the data node could not —
// would answer none of its tracker, pages or usage operations. It covers
// EVERY operation rather than the ones that read the chart, because a list of
// which operation reads which seam is a second list to keep in step with the
// decides, and one left off it would serve a stale chart first.
//
// It answers in a reason of its own, `behind_config` ([unservedBehindConfig]),
// because unlike a lag it LASTS — until the node's apply succeeds, and for a
// stuck node until a person acts — so the asker REMEMBERS it for thirty
// seconds ([Router.markBehindConfig]) and meanwhile does not ask the node at
// all: it comes to it only as the last resort, told at once to answer anyway,
// and a keyed operation asks the repository's runner-up instead. So each
// asker pays one refusal per data node behind the configuration every thirty
// seconds — the one that tells it, or tells it again once the window ends —
// and nothing else: an operation a current data node runs never reaches the
// node behind, and one only the node behind can run is one ask. Forgotten
// between requests, as a lag is, every operation of every asker paid one
// refusal per data node behind for as long as they stayed behind — on a
// stateless node beside one stuck data node every estate call was two
// requests — and every push to a repository such a node commits for sent it
// the whole proposal before the runner-up was asked.
//
// A GRANT judged from the chart is different — answered anyway by the last
// resort, it would be decided by the revision that may have revoked it — so
// an operation that judges one DECLARES it ([op.judgesGrant]: a code read or
// a ref resolve a seat asks, whose grant on a repository is the chart's, a
// seat's write of a change, which its grant decides whether it may make, and a
// write of a change's conversation the chart judges — a seat's, or an ask of
// reviewers by anybody, each reviewer judged by the chart), and
// a node behind the configuration refuses it even asked last, `behind_config`
// saying it decides no grant: the soft arm sends the walk to every current
// data node first, the last resort asks this node again, it refuses again,
// and the walk ends unserved. That is the HARD ARM, and the bound on the
// window a stale grant is judged in is the control plane's confirmation —
// about a minute. TestEveryOperationThatJudgesAGrantIsDeclared holds every
// operation whose arguments carry an asker to the declaration, and the
// engine's TestEveryEpochSeamAServingDecideReadsIsClassified holds every seam
// a serving decide reads to a class, so no grant reaches a decide unseen.
// Admission's ping is held back by neither preference, nor by an asker's
// memory of the second ([op.ungated]): it asks the copy's own seat admission
// and routes no later operation.
//
// # A repository's git work is asked of its home git node
//
// An operation on a repository's git OBJECTS — a read by object id of a tree,
// a file, a diff, a MERGE PREVIEW of two commits ([opCodeMergePreview], a read
// too), and an ENGINE WRITE, a seat's or a person's commit built beside a copy
// of them and landed as a push (ADR-0062) — is answered by a node with the
// `git` role from its own cache, never by a data node's copy.
// That is ADR-0059, amending ADR-0025: the router's walk has a second
// placement beside the data nodes, every live git node ([RouterOptions.Git]),
// keyed by repository ([op.keyedOnGit]), and such an operation names the git
// half rather than a domain's copy ([GitCopy]), so no copy verdict, no
// dispatch floor and no lag gate holds it back — its floor travels in its
// arguments and is the git node's cache's sync level. It is asked in
// rendezvous.Order(repository, every live git node) — the repository's HOME
// first, its runner-up next, the keyed order's demotions and no sticky node,
// this node at its place where its git half is built ([LocalGit], resolved per
// request, since the code host is built at boot or at the first apply that
// hosts code) — on lanes of their own, the reads' and the writes'
// ([laneGitReads], [laneGitWrites]), which a git node registers from boot. The
// home is a preference, never ownership: no lease, no epoch. A git node that
// cannot answer inside its budget — its copy still syncing, its share full, or
// every engine write its writes' lane holds already held, the next answered at
// once ([gitWritesHeld]) — answers `behind`, its reason beside it
// ([reply.Unready]), and is not demoted, so its askers come back once it is
// warm; and ONE node a walk FILLS
// a copy at a time: the walk stamps its effective home's attempt
// (code.GitQuery.Home, code.EngineWrite.Home), and every other git node holding
// no copy answers at once that it holds none, so a repository nobody holds
// costs the fleet one download rather than one a git node. The fill moves to
// the next node of the walk only past one bringing no copy up for the
// operation — behind its configuration for a seat, failing on its own, no code
// host — so a home that will not serve a read for a reason that lasts never
// leaves its repository unreadable ([Router.routeGit]); a home at its share
// keeps it, since the share is the node's and not the repository's. Every git
// request is a signed estate request like any other, and every attempt — a
// peer's, or this node's own git half in process — is held to its git attempt
// and SERVED UNTIL ITS ANSWER IS DUE, a reply's margin before the attempt ends
// (code.AnswerBy), so a busy git node is heard saying so rather than taken for
// a gone one, and an operation is answered alike whichever node asked it
// ([GitReadAttempt], [GitWriteAttempt]).
//
// AN ENGINE WRITE IS ONE PUSH WHICHEVER GIT NODE BUILDS IT ([opCodeGitWrite]):
// its push id is derived from its caller's operation (code.WritePushID), and
// its commit is a pure function of that operation and its parent, so the walk
// asks it again of the next git node after one that ran nothing or went
// silent, as an idempotent write — the next node builds the same push, and is
// answered from its row where the silent one landed it — and two nodes that
// both took themselves for its home build one push, which the repository's
// committer lands once. Its attempt on one git node is the sum of every wait
// its stages make at their whole budgets ([GitWriteAttempt], 82 s), which the
// git node fits the write into — answering busy, before its upload, a write
// whose proposal would no longer fit — so the attempt is the router's own
// whatever deadline its caller gives ([Router.budgetFor]): a turn's minutes
// waited out on a wedged home would leave its runner-up never asked.
//
// [Router.Home] names that home itself — the node a repository's reads and
// engine writes are asked of, and the one its maintenance falls to on a fleet
// of alike nodes: the first node of the same rendezvous order with NO demotion
// — one function builds both, so on a quiet fleet a repository's reads, its
// engine writes and its own git work meet on one git node — and UNKNOWN,
// never a guess, where who serves git cannot be read. [Router.HomeOf] answers
// which of many repositories this node is the home of off ONE reading of who
// serves git, three-valued as Home is — the set, or unknown for every one —
// for a caller that judges many at once: the backstop pass of the home's
// change work (internal/code/home), over every repository its workers list,
// which asked a repository at a time would read the presence view again for
// each. A home that went quiet is still the home; work two nodes of a split
// presence view both do is work its repository's writers take once.
//
// [Router.Maintainer] names the git node a repository's MAINTENANCE — the
// compaction of its packs, the publication of its layers — is placed on, which
// is the home unless the home cannot hold the work: the first node of the same
// order, still with no demotion, whose ADVERTISED background budget (each git
// node's maintenance budget, which its presence row carries on every beat
// beside its status, so a beat whose status was dropped still says it) holds
// the work's charge, and the home where none does. ONE LISTING answers the
// whole question ([GitPlacement.Listing]: the git nodes and what each says,
// read together), so a node is weighed by the listing that names it, and a
// node that listing does not carry — this one included, wherever its own git
// half is built — is GONE, never a node not saying. So on a fleet of alike
// nodes the maintainer is the home and nothing moves, and on one with a single
// large git node every repository only it can hold is maintained there.
// NOTHING MOVES PAST THE HOME ON MISSING INFORMATION: a node
// whose row carries no figure is NOT SAYING, never a node with nothing to
// give, and is passed over where it is a runner-up — kept in its place, a
// small runner-up not saying would take a large node's repositories, each
// starting with a cold sync of its whole history only for its own admission to
// refuse the work — and keeps the repositories it is home to, as a quiet home
// keeps them, since the home holds their copies. [Router.MaintainerOf]
// answers which of many repositories, each by its own charge, this node
// maintains, off ONE listing for them all — three-valued: the set, or the
// listing's error for every one, on which a node does none of a maintainer's
// work — for a caller that judges many at once: internal/code/compact's two
// loops, the compaction of a repository's packs and the publication of its
// layers, whose passes over their listings read the presence view again for
// every repository they asked about one at a time. Both ask by one charge,
// the largest of their steps', so both writers of a repository's packs run
// on one node (ADR-0061), and a maintainer two nodes both believe in is one the
// committer takes once, as a home's work is.
//
// # Which data nodes to ask is answered from memory
//
// Every request asks the [Placement] who holds the estate, so the placement
// must answer from what the node already knows: listing the fleet's presence
// leases per request was an O(fleet) read of the coordination store, across the
// leaf link, on every tool call. The engine's placement is every live data
// node, from a WATCHED view of the presence leases that lists once per
// heartbeat and answers UNKNOWN rather than an empty or stale fleet once its
// last listing is older than a lease survives; a node it named that went silent
// is reported ([Placement.Unanswered]) so it drops out at once — silent through
// a wait of the router's own, because a caller that hung up or gave a request
// less than an attempt has said nothing about the node it was asking
// ([Router.ask]). An unknown placement fails the request with the placement's
// own reason: "give a node the data role" is the wrong remedy for a fleet whose
// coordination blinked.
//
// # Read-your-writes: ONE floor table per node, consulted for local reads too
//
// A write goes to the fleet's log, and the next read may be answered by a
// different data node than the one that took the write — or by this node,
// which may not have applied it yet. So each node keeps ONE [Session]: a
// HIGH-WATER per stream of the furthest position any write it made (or any
// wait it was asked for) reached. Every request carries the floor on the log
// of the operation's own domain ([opSpec.floorStream]), and whichever data
// node answers — this node's own copy included — waits for its applier to
// reach it, bounded by [statelog.ReadBudget], and says it is behind rather
// than answer from before a write this node has already been told landed. A
// node answering ANOTHER node says so while that node is still listening:
// every wait it makes before it answers — this one, a read's barrier and the
// position it established, a write's own record — ends code.ReplyMargin short
// of the asker's deadline ([statelog.WithReplyMargin]), because a `behind`
// sent after the asker stopped waiting is heard as a node that is gone. A
// data node's own copy is held to its floors like any other because its seats
// may have written through another data node while its copy was out of
// service. Floors on another DOMAIN's log are never waited for, though the
// estate carries every domain's log: no operation reads another domain's rows
// AT A POSITION — the reads that cross into one, a search's semantic half and
// a page's backlinks, are of indexes no log position describes — so that
// applier's lag is not one the operation should wait out ([ready]).
//
// The searches carry no floor at all: they read an index each node's own walk
// maintains behind its applier, which no log position describes. They still
// name the domain whose rows that index is over, so a node whose copy of it is
// out of service runs none of them.
//
// # Every envelope is signed, and bound to its question
//
// A data node runs a tracker write AS THE ACTOR ITS REQUEST NAMES, so an
// unsigned request was ADR-0031's unsigned record one hop earlier: anyone who
// could reach the broker could write as any seat, and the log would carry it
// signed by the data node that obeyed. So every request and every reply is
// framed and signed under the fleet keyring, by the state log's own
// [statelog.Signer] under domains of the estate's own, and read with its
// verdicts ([Envelopes]) — the rule internal/signedask keeps for every channel
// a node carries a request out on, this one, the fleet broker's membership, a
// note to a running turn and a code search alike: a request that does not verify is never
// dispatched, the actor a write runs as is the one inside the signed body, and
// a key a node does not hold is "did not run" — the router moves on to a node
// that holds it. A request carries an id and a deadline under the signature, so a
// data node refuses a replay of one; a reply names the request it answers, so
// an asker believes no answer to somebody else's question. A reply it cannot
// believe is no answer — never "did not run" — so a once-write met with one is
// [ErrOutcomeUnknown].
//
// # What does not cross the wire, and who supplies it
//
// Everything that is in-process by nature — the chart seams a query carries
// ([tracker.Units]), the lead map a dependency consults ([tracker.Leads]), the
// org a knowledge search is scoped by — is supplied by the SERVING node from
// its own current epoch ([ServerSeams]). Every node reads the same activation,
// so the two differ only for the instant an apply is landing, which is the same
// window every surface already has — and on a node confirmed behind the
// company's configuration, which is asked last for that reason (above). The
// gate in wire_test.go walks every type
// an operation carries and fails on any field that would silently not arrive,
// unless it is named, with its reason, in [carriedByServer].
//
// THE IDENTITY DIRECTORY IS THE OPPOSITE CASE, and for the same reason. What a
// data node answers about a login or a seat is FACTS — the row and how far it
// could vouch for it ([IdentityReader]) — and the verdict is the asking node's:
// the chart a binding is resolved against and the Tier A tokens it declares
// are its own, because its request path is where a person verb is decided. And
// a binding crosses NARROWED ([Binding]): the row's credential verifiers and
// grants never reach the broker, since the asker decides nothing on either.
//
// A MACHINE TOKEN'S ROW CROSSES NARROWED TOO, and narrowed differently
// ([TokenFacts]), because there the asker decides on nearly all of it: the
// grants the mint carried and the owner's current ones are what the token acts
// with, and the expiry, the revocation, the epochs and the applied position
// are what end it or make its absence three-valued — so those cross whole.
// The VERIFIER is the one value withheld, and the presented SECRET never
// leaves the asker: what is asked is the token's id, a nonce drawn for that
// question alone and the keyring entry to answer under, and what crosses in
// the verifier's place is a MAC over it and the nonce under a key both nodes
// derive from the fleet keyring. The asker MACs the verifier the presented
// token implies the same way and takes it as the row's verifier only where the
// two agree, so credential.CheckToken decides there exactly what it decides on
// a data node, over a row neither the broker nor anybody on it can test a
// guess against — and an answer MAC'd for one question verifies under no
// other's nonce.
//
// # Errors keep their identity
//
// A tool decides what to tell a model by asking errors.Is and errors.As — "no
// such task", "stale version", a tag clash, a refusal with a reason. A string
// would answer none of them, so an error crosses as its message plus every
// registered sentinel it matches and every registered type it carries (see
// errors.go), and the far side rebuilds an error that answers the same
// questions. A sentinel or an error type added to the packages whose errors
// cross — this one's, the tracker's, the knowledge base's, the state log's and
// the identity estate's with the vocabulary its answers are judged in — is a
// build failure until it is registered. An operation answered in-process returns its own error,
// which answers every question without the wire's help.
package estate
