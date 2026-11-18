// Package engine assembles one process's participation in a company: the
// config, the org, the models, the tools, and the turn machinery that runs
// between them.
//
// It is the ENTANGLEMENT POINT, and the reason it is a package of small files
// rather than one large one. Collected into a single module this is thousands
// of lines whose two hardest passages — the inbox handler and the config
// apply — can only be read by scrolling past everything else. Here
// each of those is a package of its own (internal/agent/inbox, and the config
// plane), and what remains here is the WIRING: which concrete thing satisfies
// which seam.
//
// It is also where a turn is charged to the work it was on: which item a turn
// is on is decided in worksubject.go and what each of its segments charges that
// item in turnspend.go, because only this package holds the trigger, the
// runner's tally and the tracker's writer together. ADR-0022 is the decision.
//
// And it is where a person's note to a running turn is carried: in the first
// segment's box on the node running the turn until the turn launches its
// executor (steer.go), and from that launch to the turn's end sealed on the
// run's record, which any node files on, the run's bridge serves and the
// review reads (runsteer.go) — because only this package holds the turn, the
// run's record, the bridge and the fleet keyring together. ADR-0067 is the
// decision.
//
// And it is where a seat whose models are all at their usage limit is parked
// rather than retried into the limit (providerpark.go, on the machinery the
// budget park shares, seatpark.go), because only this package holds the
// dispatcher, the seat's inbox and the chain's verdict together. ADR-0064 is
// the decision.
//
// And it is where a turn failure a retry cannot cure (an image the runtime
// refuses, every model refusing its credential) pauses its seat on its second
// time in a row, until its settings change or a person resumes it
// (failurehold.go), and a conversation whose context does not fit is held
// (conversationhold.go). ADR-0117 is the decision.
//
// And it is where a wake a completed turn already read past, asking the seat
// nothing, is absorbed rather than run (seenthrough.go). ADR-0120 is the
// decision.
//
// And it is where a pull request a task waits on settles it, from the GitHub
// webhook or the poll of open ones, with no agent's turn (changesettle.go).
// ADR-0121 is the decision.
//
// And it is where a remote MCP server's OAuth grant is kept and renewed: the
// sign-in a person starts on one node and the vendor returns to another, the
// refresh token sealed in the secret store's engine rows, each node's access
// token held in its memory and renewed one node at a time under a fleet lease,
// and the grant given up at its vendor on a sign-out or a removal
// (mcpoauth.go), or the company's service identity signed in as by each node
// when the server needs a token, with nothing kept (mcpserviceidentity.go),
// because only this package holds the server's configuration, the fleet's
// stores and keyring and the servers it starts together. ADR-0075 is the
// decision.
//
// # The core runtime
//
// core.go splits what every data node runs from boot (the core) from the native
// half by what depends on a company, never by domain. The core is the state log
// for every registered domain, the node gate over every identity log, the
// identity estate's two sides and the loop deriving the party registry from the
// directory, published once by [New] on every node holding the estate, in every
// mode. The native half (whether the engine keeps a tracker and knowledge base
// at all, their read and write sides, the lexical index, the change feeds, the
// embedding duty) waits for the first company. A node with no company is
// exactly the one that must sign in a Tier A token's session: the dashboard's
// create-the-company path writes the first revision as that session, and the
// company's first person is invited under it once that revision declares their
// human seat (ADR-0032).
//
// The core runs every domain, not only the identity estate, because a split by
// domain breaks four things. A join replaces the whole replicated file and must
// run before any applier ([Engine.startStateLog], every domain or none), so
// starting the rest later would need a second join over a file whose identity
// rows are being applied. A snapshot names every registered domain or every
// joiner refuses it, so a partial node could donate nothing. The trim counts
// nodes per log from position heartbeats, so a node not running the tracker's
// log would pin its trim at zero while up. And every alarm, readiness gate and
// seat-admission rule would need two notions of a node running a log. Only the
// writers and readers over the tracker's and knowledge base's logs wait for the
// company.
//
// A node without `data` (ADR-0025) holds no estate and starts no core. It asks
// a data node through the estate's router for what it needs of the identity
// directory ([routedIdentity]: whose record a login names, who holds each human
// seat), and for the row a machine token is checked against and the seat a Tier
// A login binds ([Engine.MachineToken], [Engine.BoundSeat]). The chart and this
// node's Tier A tokens are applied to the answer on the asking side, by the
// functions a data node uses on its own rows, so neither a verifier nor a
// secret crosses. Its party registry reads the directory on the periodic net
// alone (DirectoryRefresh). The sign-in surface is served only where `ingress`
// is, which Tier A requires `data` beside, so every session is validated
// against rows the validating node holds: 503, never 401, on a node that is
// behind, and no cache in front.
//
// # Maintenance mode
//
// A stream's byte ceiling cannot be changed safely while anything publishes,
// and what retires a request the broker has already queued is the broker
// process restarting, not a client closing. So a capacity change is a
// fleet-wide restart into a mode that starts every broker and no publisher. The
// exclusion is structural, not observed: on the default topology the embedded
// broker binds no socket, so nothing outside these processes can reach it, and
// these processes start nothing that writes; there is no check to pass because
// nobody could publish.
//
// A normal-mode node must not start publishing into a fleet whose capacity
// operation is unresolved, and it establishes that with a handshake, not an
// absence check: it writes its own admission, re-reads the operation, and
// withdraws and refuses if one is there. Either order is then safe: a node that
// slipped between a coordinator's check and its own start has left a durable
// record that blocks the coordinator, where a silence would have let it
// proceed. A maintenance-mode node acknowledges instead, publishing its
// incarnation against the operation's current attempt, which the seal is
// established from. An acknowledgement from seal mode is evidence because that
// mode cannot write configuration, so the process making the claim cannot be
// the one holding the request it retires. Every boot into a maintenance mode
// first retires the admission a previous process of the node left, once the
// fleet's presence shows that process is gone, since a crashed process never
// withdrew its own.
//
// A maintenance-mode node keeps its presence lease: presence is membership, not
// work, and a window reads it three ways (the capacity operation's participant
// set, where a broker member holding no data is found nowhere else; the
// estate's routing of a stateless node's reads to a data node still serving
// them; and the fleet views an operator reads). So the seat host runs in every
// mode. It withholds what writes the company's records: seats, which the host
// never claims there, and writers (its own and the router's), which no surface
// is handed; and everything that would append on its own: the duties and
// schedulers the constructor's gate never starts, the object store's collector,
// which pins the estate with a barrier, and every barrier a read would append
// (statelog.RefuseMaintenance).
//
// # Broker membership
//
// An operator reads and changes the fleet's broker membership here
// (fleetbroker.go). Two records say who the members are and can disagree: each
// node advertises its broker kind on its presence lease, and the broker's
// metadata group counts its own voters. A member gone for good is the
// disagreement that matters: its presence lapses with its process while the
// group counts it in every election and create until it is removed, so a
// three-member fleet that lost two has no quorum and nothing on the presence
// side says why. Only a member holds the group, read in process; any other node
// asks a live member over `crewlet.fleet.broker.<member>`, which also carries a
// removal, because nats-server answers one only on the system account a member
// reaches in its own process.
//
// Every request and reply on that subject is signed (internal/signedask, its
// own key domains). The broker's admission vouches for a connection, never a
// message (a route's password crosses the network in clear, and an external
// cluster admits whom its operator chooses), so unsigned, anything the broker
// admitted could remove a voter past the grant, step-up, confirmation and
// live-member refusal that run on the asking node, and a forged listing is what
// an operator would remove a voter on. A member runs only a request the fleet
// signed, bound to an untaken id and a live deadline; one the fleet did not
// sign is refused and logged at error, and one under a key the member lacks is
// answered "did not run" so the asker tries the next member. Who asked travels
// inside the signature ([iam.ActorFor]), so `fleet_broker_member_removed` names
// a credential nothing on the broker could choose.
//
// Voters are matched to nodes by raft peer id (derived from the server name),
// never by name, which is only what the answering member happened to hear: a
// member lost while the survivors restarted is listed nameless and is removed
// by the id the listing shows. An external cluster's membership is its
// operator's, and both gestures say so.
//
// # The live relay
//
// A node whose copy of a domain is out of service still tells its dashboards
// what that domain's applier would have (liverelay.go). It stays in rotation
// and routes its reads of that domain to a sound peer ([localEstate.For]), but
// an applier that commits nothing announces nothing: its sockets would go quiet
// with no reason to reconnect elsewhere, and the artifact origin's verdict
// cache would keep a withdrawn share until it aged out. So its sockets hear a
// sound peer's applier for that domain, and its own announcements of that
// domain are withheld: two sources would push one movement twice, and the
// withheld one is the one nobody vouches for.
//
// The switch is per domain, because a fault is one log's (ADR-0079): relaying
// every domain for one domain's fault would silence sound copies, and a fleet
// in which each node held a different copy wrong would have no server to
// follow. The machinery is one ring, one sequence, one follower and one
// subscription per node; a relay per domain would cost a follower and a
// subscription per domain on every data node, for an exchange that runs only
// while some copy is wrong. Every switch, either way, re-reads that domain's
// screens, because what was committed between one source's last batch and the
// other's first is not replayed.
//
// A follower reads one server by that server's own sequence, under an epoch
// drawn at boot and again whenever one of the server's copies comes back into
// service. Log positions cannot serve: a batch is whatever one node's applier
// committed together, so two nodes' batches never line up, and an inbox
// movement carries no position. Another epoch, a sequence the ring no longer
// reaches, or another server is answered with a re-read. A server keeps a batch
// only while a follower can still ask for it (one round trip after its latest
// answer, or while an ask is held open), so with no copy out of service
// anywhere, every data node keeps nothing.
//
// The relay is signed although the read-only scatters are not (ADR-0031), on an
// internal/signedask channel with key domains of its own: what a follower takes
// is pushed unasked to every socket as a colleague's words, so an unsigned
// answer would let anybody on the broker put words in a room under anybody's
// name. Each answer is bound to an id drawn per ask, so a captured answer moves
// no other follower, and the server admits the ask too, so it seals its ring
// for no stranger. An answer the follower cannot believe is no answer: the
// transport carries no sender, so a forged reply that ended the ask would let
// anybody move every follower off every server. A believed answer passes the
// same per-socket filters an own applier's batch would ([chatlet.Visible],
// [artifactdata.Readable]). Signing does not encrypt; keeping the ports private
// is the operator's half (SECURITY.md).
//
// # Dead letters
//
// A wake feed keeps a record it handed back for an hour as a dead letter in the
// queue's dead-letter stream and replays it at its start (internal/changefeed);
// the notification service's inbound group keeps a delivery no member handled
// on its unhandled subject the same way, replayed by every node that reads it.
// A node touches those entries three ways: its feeds and the inbound replay
// drop through deadLetterLedger, an operator lists and discards feed entries
// through the same ledger (FeedDeadLetters; the inbound ones are never shown
// there), and the alarm reading every beat takes reads them through
// deadLetterWatch for `feed_dead_lettered` and `inbound_dead_lettered`. Only a
// replay reads a frame.
//
// ONE COUNT A BEAT, AND A LISTING ONLY WHEN IT MOVED. Every beat of every data
// node asks the queue one question per subtree (changefeed.DeadLettersUnder,
// inboundLettersUnder): its MARKS, each subject's count and the dead-letter
// stream's last sequence, which the broker's leader states in one request
// however many entries there are. While they are the marks the cached listings
// agree with, nothing else is read, and each cached listing is aged to now.
// Each subtree keeps its own agreed marks (letterTree), since a keep on one
// moves the last sequence the other reads. If the LAST SEQUENCE moved,
// something was kept, and every subject holding an entry now or at its last
// listing is relisted, headers only, not only one whose count changed:
// `CREWLET_DLQ` also keeps subscriptions' poison (`dlq.<topic>.<group>`), which
// moves the last sequence and no feed's count, and a keep and an expiry on one
// subject inside a beat leave its count unchanged. If ONLY A COUNT moved,
// entries can only have gone (an expiry from a subject's front, a drop from
// anywhere), each lowering its subject's count, so only those subjects are
// relisted.
//
// A LISTING SATISFIES THE MARKS ONLY WHEN IT AGREES WITH THEM. The marks are
// the leader's and a listing is a replica's, which can trail it. A listing
// whose count is not the marks' is still taken as the newest sight there is,
// but the marks are not recorded as agreed, so the next beat relists until one
// agrees; recorded on a lagging listing, the marks would never move again for
// it, and the alarm would stay short an entry or keep a dropped one and count
// its expiry a day late. A read that FAILS keeps the last listing that
// answered, aged to now: an entry is not gone because one read missed the
// broker, and an alarm cleared on a broker blip would only raise again.
//
// AN ENTRY THAT VANISHED AT ITS EXPIRY IS A WAKE LOST. The broker lets an entry
// go at its retention silently, and one that goes was replayed (its wake
// published), discarded (`crewlet.tracker.feed.dead_letter_discarded`) or
// expired (its wake lost). An entry the last listing held and this one does not
// is counted expired (`crewlet.tracker.feed.dead_letter_expired`, or the
// inbound group's metric) when it is past its expiry by this listing however
// many reads failed between, or was within a beat (statelog.AlarmInterval) of
// it when last seen, since the broker's clock and this node's differ. It is
// counted by whichever node first takes the claim on its going (goneKey), which
// a replay or discard that dropped it takes first (deadLetterLedger), so it is
// counted once fleet-wide and never for an entry somebody replayed or
// discarded, though every data node reads every beat. Not counted: an expiry
// across a restart of every node that saw it, since a node starts with no
// sight. A replay or discard whose claim could not be taken near the expiry may
// be counted by a peer: a count of losses errs towards a loss.
//
// # Budget holds on the pinned clock
//
// A meter holds a window it has seen full until that window turns over on the
// turn's pinned clock ([meter.Refused]). That is the hold's one edge: the
// counter is shared and a slot never rolls back (coord's package doc), so a
// peer whose clock leads this node's across a boundary, or one on a newer epoch
// after the company's timezone moved east, rolls the slot onto the next window
// before the turn's clock says the held one is over. The turn's next charge
// would land in that next window and could be admitted, and the meter still
// holds the refusal: for seconds of clock skew, or for hours after a timezone
// moved east.
//
// This is deliberate. A turn's rounds are counted on the calendar it was pinned
// to, and on that calendar the window is full; a round the counter places in a
// peer's next window is skew it already resolves in the closed direction (a
// round charged early costs the next window a round). A read before every hold
// would trade the turn's own calendar for whichever peer moved the slot. And a
// false hold fails closed as a refusal does: the turn ends budget_exhausted,
// the budget park reads the counter on the current calendar, finds nothing
// refusing and parks nothing, and the delivery runs again (or, for a turn that
// already wrote outside the engine, is recorded, as after a real refusal). It
// costs the turn's work so far, never a call billed and then refused.
//
// # Executor launch floor
//
// An executor launch is held to the room its first request needs
// (launchfloor.go); ADR-0066 is the decision. The static floor
// (turn_engine.executor_min_budget_tokens) is that request's fixed part: the
// harness's prompt, its tools and the seat's system prompt. Each brief the
// launch may hand its run (the session the turn kept, the previous job's own
// copy in its box, the fresh brief) holds it to that brief's own floor, counted
// at a token for every four bytes (harness EstimateTokens), and the launch is
// held to the largest. A refusal carries the floor it was refused below to
// whatever waits on it (Engine.budgetHold for a resume, Engine.budgetPark for a
// dispatch), and that wait asks for a window leaving less than THAT floor:
// reading the static floor instead, a refused resume found no window starved
// and was retried into the same refusal, and a parked delivery was NAKed until
// the broker dead-lettered it.
//
// A floor no window can leave room for is no floor to wait on. The most a
// window can leave a launch is its ceiling less what the launch's own turn has
// already charged there (windowRoom), never the ceiling itself. A session whose
// floor is past that room is not continued (launchFloor.fits): the launch takes
// the other session if that one fits, and otherwise starts on its fresh brief,
// which has nothing to be dropped for, so its floor past the room is HELD
// (launchFloor.fresh, launchFloor.held) at the static floor where that fits in
// the room, and at the room otherwise. Config holds every ceiling at or above
// the static floor (validateExecutorFloor), but cannot hold it above the static
// floor plus a brief plus the turn's own spend. It is never held at the room
// above the static floor: that waits for a window holding nothing but the
// turn's own spend, which a busy company's other seats may never leave, and a
// run that overspends and is reported (sandbox_spend_over_budget) is better
// than a launch that waits for ever.
//
// A floor within the room is waited for one turnover at most. Other seats spend
// in the company's window before the launch reads it, so a floor above the
// static one can be past what every window leaves at the launch's read, and
// waited for, it would be refused in every window. So the window a launch was
// refused in is kept for the work it refused, never for the seat, whose park
// release could hand it to another launch: on a dispatch in the seat's park on
// this node, under the ids of the delivery's events (floorWaits); on a resume
// on the job's record (sandbox.LaunchRecord.RelaunchRefusedIn). The next try of
// that work, in a later window, holds its fresh brief as one past the room
// (launchFloor.after) and drops a session this window still leaves no room for
// (launchFloor.stillShort). A seat that moves node between the two tries may
// wait one window more, and so may a record holding no window, which reads as
// never refused.
//
// A session the launch's model cannot carry is not continued either: a Claude
// Code run's window is its model's (sandbox.ClaudeCodeWindow), and a session
// kept under a larger window, continued on a member whose model has a smaller
// one, can be past what one request can send (launchFloor.carriedBy).
//
// THE CONTEXT A CONTINUED SESSION'S FLOOR HOLDS is its harness's own estimate
// of the session's next request where the capture or the job's record holds
// one, read at the rate of the model the member names (continuedContext over
// sandbox.NextRequest.At), and the last request's input only where neither
// does: never the larger of the two, since past a compaction no request has
// followed, the last request's input is the compacted-away conversation's.
// Both are the continued session's own: a kept capture's its stamp's
// (runner.SessionBasis.NextRequest), never a newer capture's of its job that
// the relaunch continued past, and the job's copy in its box its job's
// record's, else its newest capture's (runner.SessionBasis.CopyNextRequest).
//
// WHICH SESSION A LAUNCH CONTINUES is answered for ANY MEMBER OF THE CHAIN,
// never one picked by the probe (agentLauncher.ProbeSession): the launch picks
// its member once, as it launches (agentLauncher.member), and a member's plan
// can be spent or come back between the probe and that pick. Asked for one
// member, the probe answered for a member the launch then passed over,
// holding the launch to the floor of a session it would start fresh, or
// writing no continuation for a session its member could have taken up. So
// the briefs that continue the session are written wherever some member
// restores it, and the launch hands them on only where the member it picked
// runs the harness that wrote the session (runner.SessionBasis.Harness,
// agentLauncher.continuedFloor).
//
// # Seat credentials issued at use
//
// Some integrations never seal an agent's credential: the engine issues it,
// about an hour long, when one of the seat's values names it, from what the
// integration does hold (GitHub's installation token from the agent's own app
// key, githubtoken.go; Google Cloud's access token for the agent's own service
// account, googlecloudtoken.go; AWS's session), and renews it before it runs
// short in the seat's tool servers and in a running box's token files
// (issued.go). It is issued here because only this package holds a seat's
// configuration, its integrations' keys and its boxes together; ADR-0058 is the
// decision. Configuration names it as a `${VAR}` in the three places a seat's
// credentials are read: its tool servers' `role.mcp_env` ([seatSpecs]), its
// coding box's `role.sandbox.env` and setup steps ([Engine.runEnvironment],
// every executor launch), and the tool servers inside that box,
// `role.sandbox.mcp` ([sandboxMCP]). One contract, [seatIssuer]: every shared
// path (the seat's resolution, its tool servers' renewal, a running box's token
// files, the waiting and refused stories) asks the registry
// [Engine.seatIssuers], never a kind, so a new integration is a new
// implementation, not a copy of those paths.
//
// It is answered by the seat's own resolution only. [Engine.seatResolver] lays
// one [seatToken] per issued variable in front of the company's chain, each
// answering this seat's variable and no other; another seat naming it is told
// the variable is not set, and the company's shared servers, acting as no seat,
// never see it. That keeps an agent's identity with its agent by construction,
// not by a check somebody must remember. It is issued only when read, at the
// source's first lookup, so a seat that never names it costs its vendor no
// request and a pass naming it twice issues once; what was issued is held by
// the issuer's process-wide cache, never by the source past its read.
//
// When it cannot be issued, there are three stories, told apart as "Tool
// server credentials" tells the rest. (1) The integration
// cannot issue it yet: a person's step its card already asks for, or the
// integration's own wait (a grant its vendor is still applying). The variable
// is declared the integration's ([Engine.mintedFor]), so the value is held back
// as WAITING, like a provisioning pass's seal; a person's step releases it when
// the reconcile records the step and applies a revision, and the integration's
// own wait is asked again on the restart schedule ([seatToken.settling]). (2)
// The vendor refused the issue or could not be reached: a failure with a reason
// ([seatToken.unissued]), retried on the restart schedule, with each issuer's
// cache holding the refusal, so a lasting refusal costs one request per wait
// rather than one per poll, nudge, launch and delivery; [seatIssuer.refusal]
// says whether a wait clears it (not answering is a wait; refusing is a
// person's step, refused at every ask until taken). (3) The integration is not
// enabled: nothing issues anything, since switching the block off is the
// operator's gesture after a leak.
//
// In none of the three is an executor run refused: it starts without the value,
// said as its launch's `sandbox_env_unresolved`, because every turn of a seat
// is a run and a seat refused for a token would do no work at all. A token file
// it was launched without is the poll's to write once the token can be issued
// ([Engine.refreshTokenFiles]).
//
// # Token files in a running box
//
// An executor's environment is fixed when its box starts, and an issued
// credential in it is good for [runTokenLife]; a run is never stopped on a
// clock, so a long one would push with a token its vendor refuses halfway
// through. What can change under a running process is a FILE: a setup step
// whose file body is wholly `${VAR}` (provisioned at launch like any other) is
// rewritten with the token the issuer holds once it renews, and a credential
// helper reading it at use always reads a good token (the documented git-auth
// recipe, docs/concepts/code-sandbox.md). The rewrite REPLACES the file whole
// ([sandbox.Sandbox.WriteFile]): written in place it is empty, then half
// written, for as long as the write takes, and a push whose helper read it then
// authenticates with nothing.
//
// It runs on whichever waiter polls the box, so on both of a node's
// ([Engine.sandboxWaiterOptions]): a `direct` or `container` box by its
// launching node's waiter only, an `e2b` box by the fleet's, on the node
// holding its duty (ADR-0065). And before a job starts in a kept box
// ([sandbox.CoordinatorOptions.Refresh]): a box is paused while its turn's
// reviewer reads the job's work or the job waits on a person's answer, for up
// to [sandbox.DefaultPauseTTL], and no poll reaches a paused box, while the
// continuing job runs no setup and acts at once on an answer, before its own
// first poll.
//
// The file is asked of the box, not remembered: it is read back and rewritten
// only where it differs, so the duty can move and the engine restart with
// nothing carried over, and a quiet poll costs one read. A file the run was
// launched without (its token could not be issued, [Engine.runEnvironment])
// reads back as nothing, which differs from any token, so the first poll after
// it can be issued writes it. The step composition is the launch's own
// ([boxSetup]), read off the company current now, so a file a revision removed
// is no longer kept.
//
// A token is recorded before it is written. A renewed token is one the launch
// never saw, so no capture of the run's session would be scrubbed of it, and a
// transcript prints it (a helper's trace, a `cat` of the file, an AWS session
// token, which no redaction shape names). Each token is handed to record (the
// job's sealed set beside its row on the poll,
// [sandbox.Coordinator.RecordHanded]; the set the job will be sealed with at a
// launch) with its issuer's expiry BEFORE the first file is written, and a
// token record refuses is not written on that pass: either the store did not
// answer, and the next poll fifteen seconds later asks again, well inside the
// five minutes the shortest renewal (an AWS session's) leaves, or the job is no
// longer running and nothing more is written. So the job's set holds a token
// before its box does, and every capture of the job's session taken once it
// ends is scrubbed of it (internal/sandbox's handed.go). Never logged, never
// returned: the lines name the seat, the path and the expiry.
//
// # A running executor's session, saved as it works
//
// The sandbox coordinator saves a running executor job's session as it
// works (internal/sandbox's "A save is offered by the poll, from memory"), and
// this package wires it: both waiters offer it every box their poll finds
// running ([Engine.sandboxWaiterOptions]), each save is charged to the fleet
// store's ledger of its seat's saves ([Engine.coordinatorOptions]), and the
// interval is read off the revision this node runs
// ([executorKeep.SaveInterval]), since a cadence binds only the node driving
// the box. A drain saves each Direct job this node launched beside its wait
// for turns, since such a job goes with whatever stops this process, and
// joins those saves before it returns ([Engine.Drain],
// [sandbox.Coordinator.SaveHosted]), saying so while it waits.
//
// # MCP servers
//
// The company's MCP servers are put in front of its agents here (mcp.go,
// seatservers.go), because only this package joins config, which parses
// `mcp_servers:`, mcp, which spawns and supervises children, and tools, which
// holds a surface; the three know nothing of each other. A SHARED server is one
// child for the company, holding no seat's credentials and callable by every
// seat it grants: it is reconciled into each epoch's registry during the apply
// and stopped when a revision no longer declares it. A PER-ROLE server is a
// template: each granted seat gets its own child carrying that seat's
// credentials, tied to the seat's LEASE, not the epoch. This node spawns one
// only for a seat whose lease it holds and kills it on release, for two
// reasons, either enough alone: a fleet claims a slice of the company per node,
// so a node spawning every seat's children would run the whole company's
// processes once per node; and a child's credentials are one seat's identity,
// so a child outliving its seat's lease would let this node keep acting as a
// seat a peer now runs.
//
// Tied to the lease is not frozen at the claim: while the seat is held every
// apply reconciles its children against the new epoch and the secret snapshot
// that apply refreshed, and a child that would not start is retried. Both kinds
// follow one rule: a child whose resolved spec is unchanged keeps running, and
// one whose spec moved, a sealed or rotated credential included, is restarted.
// A claimed seat gets its OWN bridge, holding only its children, and its OWN
// registry, cloned from the epoch's surface, because two children of one
// template publish the same tool names, which collide in a registry and again
// in a bridge's catalogue (keyed by name across every server it runs, shadowing
// the loser): every seat would call whichever child won, acting invisibly under
// another seat's identity. It also makes teardown total: releasing a seat stops
// one bridge. [Engine.ToolsFor] is what a turn is built against.
//
// A server that will not start fails neither the apply nor the claim: it costs
// that server's tools and nothing else, the seat keeps its builtins and the
// other servers, and the operator surface shows the group missing rather than
// the builtins shrinking. Refusing the seat would take a company offline
// because one vendor's harness was slow to install.
//
// # Tool server credentials
//
// A tool server starts only with what its configuration references: every
// `${VAR}` its spec resolves (its own `env` or `headers`, its `url`, and for a
// seat's child of a per-role template the seat's `mcp_env` block, a unit's
// beneath it) resolves to a value first, whether it runs on this node, as a
// shared server or a seat's child, or in a harness's box. Handed an empty
// credential a server either fails, retried for a value that is not coming, or
// acts as nobody, or as its default identity, under the seat's name. A
// reference that resolves to nothing is one of three stories, told apart
// ([credentialGap]):
//
//   - A provisioning pass will mint it: the seat was claimed before the pass
//     that creates each agent's account and seals its credential had run (every
//     first boot, every seat a revision adds). The child is not started, and
//     that is no failure: the status record counts it as waiting, naming the
//     variable and the minting integration, logged once per pass at info. Only
//     a seat's values can be this, since a pass mints per seat.
//   - A pass would mint it, but not until a person acts: its own credential
//     does not resolve or is refused, an administrator must switch something
//     on, or the seat's account could not be made or was switched off. Waiting
//     would wait for ever behind a screen saying nothing is wrong, so it is a
//     failure whose sentence is what the integration's last pass recorded, read
//     as its card reads it, never re-derived ([mintStopped]).
//   - Nothing will: the operator's secret is missing, or a value names a
//     variable nobody holds. A failure naming the variable and the remedies:
//     supply it (for a seat's bot or account, the integration's provision
//     command too), or, where the server's own key may go without it, declare
//     the key optional.
//
// None is retried on a timer: a held-back server can change only when this
// node's secret snapshot does, and every change to it is followed by a pass
// over every seat this node holds and an apply's reconcile of the shared
// servers. A pass that seals asks for the seats' pass the moment it refreshes
// this node's snapshot ([refreshingSink.Flush]), whatever becomes of the
// re-activation it asks of every other node ([Engine.rebuildForSealedSecrets]);
// `crewlet secrets set` is followed by a reload, which is an apply. What a
// person does at an integration no pass here sees, so a child held back on one
// is judged again against the integration's last report on every pass of the
// status record's writer ([Engine.mcpStatus]).
//
// A key that may go without its value is declared, and left out. A host setting
// passed through by name (`NPM_CONFIG_REGISTRY: "${NPM_CONFIG_REGISTRY}"`) may
// rightly resolve to nothing, and nothing in a value can say so: a key in
// [config.MCPServer.OptionalKeys] whose value does not resolve is left out
// whole (never half a value, as "Bearer " looks set, and never an empty one)
// and holds nothing back. The list is the server's and governs a seat's block
// too, read after the layers merge, so a seat's unresolved value leaves the key
// out rather than handing back the template's, which for an identity key would
// start the seat's child as whoever the template names. The url is never
// optional. A declaration never skips the wait for a variable a pass mints for
// the seat (the child would act as nobody until the seal), and the failure
// never offers it for a seat's own block or a key whose variable the url needs.
// The shell's `${VAR:-}` is not the spelling: the reference grammar parses no
// shell expansion, so a sandbox setup step's script survives resolution
// untouched (internal/envref).
//
// It is decided here, not in internal/mcp, which is handed resolved values and
// never reads the secret store: there an unset token and a deliberately empty
// value are the same string, and neither the variable nor its minter is in
// hand. [resolveServer] is the one place a server's values are resolved;
// [serverSpec] and a harness's box are built from it, so no path starts a
// server around it.
//
// A seat's mailbox does not wait for a held-back child: a claim waits for what
// the first turn needs, nothing can be addressed to an identity the vendor does
// not have yet, and holding the mailbox would hold every message the seat could
// answer with the tools it has.
//
// # Testing an MCP server
//
// A person's "Test now" of an MCP server (on its page, and in the add flow
// before a save) runs where the server runs (mcpcheck.go): a connection of its
// own that walks its stages and lists its tools, never calling one (mcp.Test).
// A SHARED server runs on every node and each reaches it from where it is (its
// own resolver, network and PATH), so it is tested on every node holding a
// presence lease, each answering what it found. A PER-AGENT server runs as one
// seat with credentials only the seat's holder resolves (a GitHub token is
// issued for the seat on the node holding it, and no other node may act as the
// seat), so it is tested for one seat on the node that proves it holds the seat
// when the request arrives, as a restart is.
//
// The request is an instruction (the answering node starts a process or dials
// an address as the company, with the server's credentials), so it crosses a
// channel the fleet signs (mcpCheckChannel, internal/signedask), bound to an id
// and a deadline, answered only by a node it names (or the seat's holder) in
// this build's wire version; who may ask is decided where it arrived
// (internal/api/mcpapi). The channel is signed, not encrypted, so the entry a
// request sends and every value under test cross it sealed under the fleet
// keyring, each bound to the request (mcpEntryAAD, mcpValueAAD), never
// recorded.
//
// A draft is tested as a write would leave it: in the company it would join,
// with its own launch and values, the seats' credentials for it, and turned on
// whatever its `enabled` says; it is never recorded. A value is tested before
// it is stored with the entry it is for as the fleet's ACTIVE revision states
// it, sent with the request, so nodes that have not applied the entry's write
// yet test it rather than refuse it. A saved server's test is recorded as its
// latest (coord.MCPCheck), every sentence redacted and bounded, at most
// coord.MaxMCPCheckNodes nodes, removed by the apply that removes the server
// (clearRemovedMCPChecks); its tools answer the person who asked and are never
// stored.
//
// # Memory sync cycles
//
// A memory sync cycle (one every [memorySyncInterval], bounded by it) is one
// lap over its turns: each held seat's own pass in sorted order (what it wrote
// while held here, then its counterparty profiles, skills and onboarding
// marker, sent whole every pass), then one backlogs' turn carrying, with what
// the cycle has left, the rows this node held for each seat when it took it,
// its seats taking turns from backlogResume. A seat still being taken is
// carried by no cycle until its hydration completes (memsync.ErrNotHeld).
//
// The next cycle starts at the turn the bound ran out inside, which had only
// part of a cycle, or after it, wrapping round, when it was the cycle's first
// and so had the whole cycle ([nextStart]); at the first turn not reached when
// the bound ran out between two; at the first seat when every turn was had. The
// seats within the backlogs' turn follow the same rule over what the cycle left
// it. Two exceptions: a backlogs' turn cut after it moved a mark hands the next
// cycle to the first seat, or a backlog longer than a cycle (the whole store a
// restart leaves a data node to carry back) would take every other cycle's
// start and the seats' own rows would wait behind it. And a backlogs' turn that
// was not the cycle's first and moved nothing gives the next cycle whole to the
// seat it was cut in, even the turn's first: passed, a backlog whose next row
// needs more than the own passes leave would never move while another seat's
// drained.
//
// The backlogs take a turn of their own, not what each cycle leaves after the
// own passes: leftovers carry no backlog at all while the own passes fill
// cycles (a burst, crewlet/crewlet#1041; profiles and skills that fill a cycle,
// crewlet/crewlet#1040). The cost: under overload a cycle can start part-way
// through the seats, and the backlogs' turn then precedes the own passes of the
// seats after it.
//
// It is not the memory fill's rule ([memoryFill.resume], start at the cut seat
// wherever it sits): a turn that had the whole cycle would keep every cycle
// until it ran dry, every other turn waiting (18 000 rows at 100 ms a publish
// is half an hour). Nor always the turn after: a turn that runs a cycle out
// second would never go first, so of two seats each needing most of a cycle the
// second never has a whole one, and its whole-each-cycle tables and onboarding
// marker are never carried past the cut.
//
// The guarantee, with k the held seats whose own pass a cycle runs out in: no
// seat waits more than 2k + 2 cycles between two that reach its own pass (each
// cut turn holds the turns after it back at most two cycles, the backlogs' turn
// is the +2, and the first exception only shortens it). A seat whose own pass
// fits a cycle completes it at least once in every 2k + 3 cycles, one needing
// more has a whole cycle once in every 2k + 2, and a crash loses up to 2k + 3
// cycles' writes. The bounds are reached: of seats a, b, c, d, from b's turn,
// with own passes running out in c's alone twice and the backlogs' turn moving
// nothing in two, a waits four cycles, 2k + 2 for k = 1. A smaller bound needs
// per-turn state one resume cannot hold, and denying a cut turn its whole cycle
// is the turn-after rule, which starves.
//
// # Dispatch's order
//
// THE ORDER IS THE PRODUCT, and every stage's position was earned by a
// specific failure: internal/agent/inbox owns the guard sequence and the
// reasons. What [Dispatcher.Dispatch] adds around it is the two ledger reads
// and the one ledger write, and their placement is equally deliberate:
//
//   - the completion read comes AFTER every parking branch, so a parked
//     partition is never marked done, and BEFORE coalescing, so recorded
//     constituents drop out and only the remainder merges;
//   - the sandbox ANSWER OFFER comes after that read and before the merge, on
//     the ordinary path as well as the park, because a run parked on a
//     question leaves its seat free, so the reply that resumes it arrives
//     here as an ordinary message and must be claimed before a turn eats it.
//     What the offer answers is what happens to the delivery: spent on the
//     run it resumed, HANDED BACK while a run is still owed it, or passed on
//     to the ordinary route. It is the disposition that decides and never the
//     error beside it (see [Dispatcher.answered]);
//   - the conversation read comes after that, because it is keyed on a
//     conversation the surviving events name;
//   - the completion WRITE comes after the turn, and a turn that failed is
//     still recorded: the ledger answers "has this trigger been worked", not
//     "did the work succeed", and re-running a failing turn on every
//     redelivery is how one bad trigger becomes an infinite loop. A turn whose
//     PHASE broke is recorded too, but only when [turn.Abandon] gives a
//     reason (an outward write its own record shows, a panic, or a refusal
//     every redelivery would meet again); see [Dispatcher.abandon] for why
//     that is a different question from "did it fail", and why answering it
//     with `err != nil` alone replayed a turn's external writes up to
//     twenty-five times.
//
// # A failure of this node's is never the mail's own
//
// A seat's mail that fails is NAK'd for a retry wherever it fails: the requeue
// of a partition's tail that would not merge, the requeue of a park's delivery
// behind its hold, and a turn that failed without acting. All three answer
// through [nakFailure]: AS THIS NODE'S OWN ([queue.NakNode]) where
// [nodesFailure] says the failure is one, and as the mail's ([queue.Nak])
// otherwise, so a failure one of them keeps out of the poison is one the others
// keep out too. Both NAKs return the delivery on the same backoff behind its
// conversation's newer mail and spend the same budget; what differs is where a
// delivery whose budget is spent is kept ([queue.RetiredTo]). Counted as the
// mail's, an outage or a drain spending it puts a healthy message with its
// group's poison, which nothing replays; as the node's, it is kept with the
// group's unhandled deliveries, as one no member handled. So an outage, a queue
// no longer live, a stop or drain that ended the delivery's context, or a
// coordination store, a state log, a store or a disk that failed the turn is
// the node's. A dispatcher built with no requeue path NAKs that tail and that
// park as the mail's, for a wiring defect rather than a requeue that failed,
// and the engine builds none: it fills in [Dispatcher.Park] where its caller
// left it nil.
//
// A turn that failed is NAK'd only where nothing it did can be proven to have
// left the engine and nothing about the failure says it will recur; it comes
// back BEHIND the conversation's newer mail ([queue.OutcomeNak]), the spaced,
// budgeted retry a failing turn needs, which a deferral is not. What that order
// costs is closed where it would bite: a newer message's turn that was shown
// this one waiting in its thread answers the thread as it stands and records it
// as worked through, so this delivery is dropped when it comes round rather
// than answered a second time, out of order (workedthrough.go). It is the node's where the queue tells
// it so or it is a condition of the infrastructure: a turn a drain ended, one
// whose publish the broker did not answer, or one that could not read where its
// writes are minted from a coordination store that did not answer, failed for
// nothing the mail did, so a delivery it spends is never kept as poison.
//
// [nodesFailure] reports whether the failure of a delivery a seat was handed is
// this node's own or its infrastructure's rather than the work's. It is the one
// answer both handlers of a seat's deliveries NAK by, its mailbox's
// ([nakFailure]) and its sandbox control topic's ([controlOutcome]), and the
// one the sandbox coordinator counts a failed resume toward the give-up that
// ends its run by ([sandbox.CoordinatorOptions.NodesFailure]): only the run's
// own failures count. ONE ANSWER, because the same failure reaches all three: a
// turn and a resumed turn read where their writes are minted from the same
// coordination store ([rebaseFor]), and a copy of the rule at each handler let
// the control topic count that read's outage against the message while the
// mailbox kept it out of the poison.
//
// The node's, in four ways:
//
//   - THE QUEUE TELLS IT AS THE NODE'S ([queue.NodeFault]): a broker that did
//     not answer, a queue no longer live, a delivery whose context this
//     node's stop or seat move ended, the test internal/notify's wakes ask
//     too.
//   - A CONDITION OF THE INFRASTRUCTURE the engine runs on, which the queue
//     cannot tell: a coordination store that did not answer (a turn whose work
//     began past the operation ledger's horizon reads where its writes are
//     minted before it runs, [rebaseFor]), a state log, a data node or a
//     store that could not take the write, a file store, a disk, the fleet's
//     nodes holding different keys or builds. Read by the one classification
//     of conditions, which every tool's refusal reads and a failed turn's
//     record names a condition by ([builtin.ConditionKind];
//     [builtin.InfrastructureKind] decides each kind's side and says why),
//     never by a list of its own; and read off the failure rather than off
//     its record, so a provider's failure that wraps one (a harness whose
//     prompt a full disk could not take) is the node's though its record
//     names the provider's own kind. A coding sandbox's class is put on a
//     side by whether a mail can cause it: a provider that did not answer,
//     at its account's limit, wanting it paid for or refusing the company's
//     credentials, a setup download failing on the far side and a
//     configuration the deployment cannot start are the node's. A deadline,
//     an abandoned wait and a write the state log refused for what the write
//     itself is (`state_log_write_refused`) stay the work's.
//   - THE COORDINATION STORE'S SILENCE on a step of the sandbox coordinator's
//     ([sandbox.ErrStoreSilent]).
//   - A HAND-BACK MADE WHILE NO WAITER CAN ANNOUNCE ITS JOB
//     ([sandbox.ErrUnannounced]; see "While no waiter can announce a run"
//     below).
//
// The last two are marks the sandbox coordinator makes on a completion's
// claim, collection, park and refusal record and on its hand-backs, which only
// the seat's control topic delivers: no step a mailbox's turn asks of the
// coordinator, its executor's launch ([sandbox.Coordinator.Launch]), makes
// either. The mailbox reads them as the node's all the same, so the two
// handlers cannot come to disagree on one.
//
// NOT A RESUME ANOTHER NODE OR BUILD CAN MAKE ([sandbox.ErrResumeElsewhere]):
// the control topic hands that back as the node's as well ([controlOutcome]),
// but it is no failure of this node's, and the give-up counts it once no live
// node reads the version of the run's parked state ([Engine.fleetReadsParkedState]),
// or runs another build for a reply kind this build does not read
// ([Engine.anotherBuildLive], through
// [sandbox.CoordinatorOptions.ResumableElsewhere]), rather than never, which
// [nodesFailure] would make it. A mailbox's turn never meets it.
//
// # A refused completion is handed back by the condition it names
//
// [controlOutcome] is what a seat's control-topic delivery leaves with the
// broker, given what the coordinator made of it under the delivery's context.
// A completion the node's admission refused ([Engine.admitResume]) left its run
// unclaimed and running, so the delivery carries nothing that is lost with it,
// wherever a waiter can announce the run again (see the next section): the
// waiter announces a finished run again on its schedule for as long as its row
// is running and unheld, at most twenty poll intervals apart, five minutes at
// the default cadence ([sandbox.Waiter]). What a refusal does to the delivery
// is therefore keyed on the condition it names ([sandbox.TurnRefused]), never
// on which error caused it, so a condition a later change adds is handed back
// by the rule for its kind with nothing in [controlOutcome] to edit:
//
//   - A CONDITION THAT CLEARS BY WAITING (the seat's ownership not yet proven
//     again, and any limit a seat sits out) is ACKED. A Nak or a deferral each
//     spend one of the broker's deliveries ([queue.OutcomeDefer]), so handing a
//     healthy completion back on a wait would walk it to the dead-letter
//     boundary while nothing was wrong with it, and a deferral would also
//     quiesce the attachment, which nothing undoes. The coordinator keeps the
//     completion and re-fires it when the condition clears
//     ([Engine.readmitResumes]), and the waiter's next announcement is the
//     backstop for a clearing nothing signals, at most twenty poll intervals
//     after its last, beside the coordinator's own re-fire of what it keeps,
//     which no silent store keeps from coming: at the refusal's instant, and
//     never later than five minutes save for a completion the coordinator
//     holds off itself until that instant, refused after its claim, whose
//     re-fire waits for it. Before it hands [controlOutcome] the refusal, the
//     coordinator has written on the run's row that it owes the completion
//     ([sandbox.Coordinator.OnEvent]), which a restart or the seat's next
//     holder re-fires at its recovery; a record the store did not take
//     arrives as the store's silence alone, which is handed back as the
//     node's, never acknowledged.
//   - A DRAINING NODE ([sandbox.Draining]) DEFERS: the condition does not
//     clear on this node, which is leaving, so the attachment is quiesced for
//     the rest of the drain, as a mailbox's is, and the completion goes back to
//     the head of the topic for the seat's next holder. It costs one delivery,
//     as every handoff does, which the delivery budget is sized for, and no
//     write: a delivery handed on is never acknowledged, so the coordinator
//     records nothing owed for it. Named in [controlOutcome] alone, the drain's
//     refusal would cost a write per job, and one the store did not take would
//     come back as the store's silence, handed back to the node that is
//     leaving.
//   - A SEAT THIS NODE HAS LET GO OF ([sandbox.SeatLost]) DEFERS for the same
//     reason, and it is the condition a person's answer is refused on as well
//     as a completion: the delivery belongs to the seat's holder. Naked, it
//     came back here on every backoff while the attachment outlived the lease,
//     one delivery each, and an answer walked to the dead-letter subject; the
//     deferral hands it to the holder for one, and the release that follows
//     detaches the quiesced attachment, so a later claim attaches a fresh one.
//
// # While no waiter can announce a run
//
// THE WAITER IS THE BACKSTOP WHILE THE STORE ANSWERS THE BOX'S HOST, AND THE
// BROKER IS WHILE IT DOES NOT. While the coordinator keeps a clock on the store
// of the host of a run's box, or the run's record says what such a clock would
// (the job's own record of that host's silence, or the job lost with it and its
// round not yet carried on), that host's waiter reads the run's record through
// a store that does not answer it and announces nothing, and a completion the
// coordinator keeps for its condition is in this node's memory alone, which a
// restart or a seat let go of forgets. So the coordinator marks every delivery
// it hands back meanwhile [sandbox.ErrUnannounced], and a refusal so marked is
// NAKED, as the node's, whatever condition clears it, never acked: the
// coordinator still keeps it for the signalled re-fire, and renews the delivery
// before its last, so a restart, a release or a clearing nothing signals loses
// nothing, and the two arriving together resume the run once. A drain's and a
// seat let go of still DEFER it, which hands it on rather than acknowledging
// it.
//
// A DELIVERY THAT IS THAT HOST'S WAITER'S OWN ANNOUNCEMENT, made from a read of
// the run's record asked after the clock or the record began, which the
// announcement says, ends the clock, a claim of it clears the record, and it is
// acknowledged on its refusal as above: that waiter read the run's record
// through the store they say is silent, and announces the job again from there.
// One whose read was asked before ends nothing, however late it was made. The
// coordinator's renewals read no record and say no read, so it never takes one
// for that announcement (internal/sandbox's renew.go).
//
// WHILE THE COORDINATOR KEEPS A CLOCK ON THE STORE OF THE BOX'S HOST, EVERY
// HAND-BACK IS RENEWED BY IT, whatever the hand-back, and its renewal reaches
// [controlOutcome] as nil. The clock says that host's store alone did not
// answer the host's read of the run's row: a box on another node's host is
// driven by that node's waiter alone, which reads the row through the silent
// store and announces nothing, so the broker's redelivery is all that asks
// again, and its budget is shorter than the box timeout that bounds the wait.
// So the hand-back is NAK'd as any other until the delivery in hand has a few
// deliveries left, and then the coordinator announces the completion afresh
// and answers nil, and the delivery is ACKED: the fresh announcement carries
// the completion on, one in flight. A renewal the broker does not take answers
// the hand-back's error, and the delivery is NAK'd to try again on
// ([sandbox.Coordinator.OnEvent], internal/sandbox's renew.go).
//
// # Everything else on the control topic NAKs
//
// A completion this node could not settle (the store failing, a refusal that
// names no condition, a row the store says a newer lease owns while this node's
// lease still stands, which the heartbeat settles within a beat) comes back on
// the broker's backoff: to this node once its store recovers, or to the seat's
// next owner. Acked, it would wait for the waiter's next announcement instead,
// up to five minutes away.
//
// A ROW A NEWER LEASE OWNS IS A ROUTING FAILURE, never the message's
// ([sandbox.ErrSeatNotHeld] the coordinator did not name [sandbox.SeatLost],
// because this node's own lease still reads held: the record of a refusal
// [controlOutcome] would acknowledge, [sandbox.PendingStore.OweRefusal]'s,
// among them). Where the completion goes is what failed, and the heartbeat
// settles which node is right; NAK'd as the message's, each such delivery
// marked it failed as itself, and a budget spent so sent a healthy completion
// to the poison. It is the node's ([queue.NakNode]).
//
// A RUN THAT MOVED ON BEFORE THIS NODE COULD END IT ARRIVES AS NIL, and its
// delivery is ACKED: the coordinator settles it
// ([sandbox.Coordinator.OnCompleted]). The seat's next holder fenced the claim,
// and its recovery reaps it and announces the run, or somebody else ended it
// first: that party drives the run from its row, and a redelivery reaching it
// finds no running job to claim, so the delivery carries nothing any node
// needs. Handed back, it failed as the message's, a routing failure marked as
// the message's own. While no waiter can announce the job, the coordinator
// hands it back marked all the same, as every answer that settled nothing
// here, so it is the node's even then.
//
// A STORE'S SILENCE IS THIS NODE'S FAILURE, NEVER THE MESSAGE'S. A completion
// handed back because the coordination store did not answer a step
// ([sandbox.ErrStoreSilent]: a collection's, the record of a park's question,
// or the claim itself, or a claim refused while the store has yet to take the
// release of the last one, or the record of a refusal [controlOutcome] would
// acknowledge), and every delivery handed back while no waiter can announce the
// job ([sandbox.ErrUnannounced]: a renewal's NAK and a refusal's among them),
// is NAKED AS THE NODE'S ([queue.NakNode]): returned on the same backoff and
// spending the same budget, so the spacing and the budget the dead-letter
// inequality below holds are unchanged, but a delivery whose budget it spends
// is kept with the group's unhandled deliveries, never with its poison, the
// subject an operator reads as messages that failed as themselves.
//
// SO IS EVERY FAILURE THE QUEUE TELLS AS ITS NODE'S ([queue.NodeFault], the one
// test internal/notify's wakes are classified by too): a publish the broker did
// not answer or a queue no longer live refused, the announcement of a park's
// question among them, and any failure under a context that has ended, a
// collection this node's stop or seat move cut short ("interrupted") among
// them. AND SO IS A CONDITION OF THE INFRASTRUCTURE the engine runs on: a
// resumed turn that could not read where its writes are minted from a
// coordination store that did not answer ([rebaseFor]) is the same failure the
// seat's mailbox hands back as the node's, and one answer decides both
// ([nodesFailure]). AND SO IS A RESUME ANOTHER NODE OR BUILD CAN MAKE
// ([sandbox.ErrResumeElsewhere]: a parked state of a version this build does
// not read, a reply kind it does not know, a seat or a company that is not this
// node's): the completion waits for a node that can resume it, which says
// nothing of the message, and NAK'd as the message's, a roll that kept an older
// node holding the seat sent it to the poison. AND SO IS A CLAIM THE SEAT'S
// RECOVERY GATE REFUSED ([sandbox.ErrSeatRecovering]): a recovery pass of the
// seat is under way on this node, which claims none of its runs until the pass
// is done, a second or so, so the delivery comes back past it on the broker's
// backoff; it says nothing of the message, and acknowledged, the completion
// would wait for the waiter's next announcement. A refusal that names nothing
// to wait on is not one: it stays the message's.
//
// A seat's recovery pass reads a claim it finds as one whose node stopped, so
// none of the seat's runs is claimed on this node while the pass runs
// (internal/sandbox's recoverygate.go). The broker holds what was published to
// the seat's control topic while nothing here consumed it and delivers it the
// moment the topic is attached, which on a seat already held the node's
// admission does not refuse. So the seat's preparation hands the subscription
// to the pass, which makes it once the seat's recovery gate is closed
// ([sandbox.Coordinator.RecoverSeat]'s attach): such a delivery is refused its
// claim ([sandbox.ErrSeatRecovering]) and handed back as this node's failure,
// and the pass takes the run over as its last holder left it.
//
// A DRAIN'S CUT IS NOT ONE: it ends only the turn's admission context
// (internal/node's cut), never the delivery's, so a hand-back it interrupted
// reads as no failure of this node's and is NAK'd as the message's. Nothing
// turns on it: the cut fires only once the queue's own broker connection is
// closed for good, so no outcome of that delivery reaches the broker, and the
// message comes back to the seat's next holder when its ack wait lapses, as
// every delivery a lost connection held does. A refusal that waits is still
// acknowledged whatever the context, as above, unless a store's silence stands
// in front of it. A failure the completion itself caused (a resume that failed
// for what it ran, past its deadline among them, or a refusal that names
// nothing to wait on) stays the message's wherever a waiter announces the job
// again; while none can, it is marked and handed back as the node's with every
// other hand-back, since its delivery is then what carries the completion.
//
// THIS NODE'S OWN STORE'S SILENCE TAKES NOTHING MORE. A delivery that
// dead-letters while it lasts loses nothing: the waiter's schedule never stops
// while the run's row is running and unheld, and its next announcement is a
// fresh message with a fresh budget, due at most twenty poll intervals after
// the last. At production's cadence that is 300 s, inside the broker's delivery
// budget (about 601 s from a message's first delivery to its dead letter, never
// under about 481 s with its jitter), so while the waiter's own store answers,
// a fresh announcement is in flight before the last can dead-letter, and no
// dead letter the silence leaves is the completion's last copy
// (internal/sandbox's collectFailedAt, held by its
// TestAJobsNextAnnouncementIsOutBeforeItsLastCanDeadLetter).
//
// # A resume another node or build can make
//
// A parked run's reply kind comes OFF THE ROW ([resumeReply]). A value this
// build does not recognise is refused rather than defaulted: it was written by
// a build that knows a kind this one does not, and guessing at who is waiting
// is the half of the delivery question this engine exists to get right. The
// refusal is a ROUTING failure, like a state this build cannot decode, so the
// completion goes back for a peer that can read it
// ([sandbox.ErrResumeElsewhere]). An absent value is no kind at all (every
// launch writes one, `none` included) and is refused too, but as one no peer
// gets past ([sandbox.ErrResumeUnavailable]): no build writes it.
//
// The sandbox coordinator counts a resume this node cannot make for want of the
// build that wrote the run's state toward the resume give-up only once no other
// node can make it ([Engine.resumableElsewhere], its
// [sandbox.CoordinatorOptions.ResumableElsewhere]): while a live node other
// than this one reads the version of the run's parked state
// ([Engine.fleetReadsParkedState]) it counts nothing, and once none does it
// counts. ONLY THE VERSION IS READ (execstate.VersionOf), never the whole
// state, which a failure that counts nothing reads again on every attempt: a
// state of this build's version whose body this build refuses still names the
// version another node reads, and its body's own failure is met, and counted,
// where a node decodes it. A run whose parked state names no version (none
// parked, or bytes that are no state) is read by no node.
//
// A REPLY KIND THIS BUILD DOES NOT READ IS BOUNDED BY THE LIVE NODES' BUILDS
// instead ([Engine.anotherBuildLive]): the kind is a plain value on the run's
// row, no part of the parked state's version, so a build that adds one writes
// states every node of the fleet reads at that version. Read by the version, a
// rollback that left two nodes of this build would hold the seat for as long
// as both lived, each reading the other as able to resume the run and neither
// able to. While another build is live (the one that wrote the kind, mid-roll,
// or one whose heartbeat names no build or carries no status at all), the run
// waits for it; once every live node runs this build, none reads the kind, and
// the failure counts. A fleet that cannot be read is an error, which the
// coordinator counts as nothing and warns of (internal/sandbox's
// runsOwnFailure). A fleet left on two builds for good is the operator's,
// raised as fleet_builds_differ.
//
// # Every hold is taken for one attachment of the inbox
//
// Every HOLD on a seat's mailbox (a person's pause, a park, the no-model pause,
// the sandbox coordinator's) is keyed on the seat's ID, because the inbox a
// hold sits on is named by the id, and is TAKEN FOR THE ATTACHMENT of the inbox
// this node holds ([queue.Attachment]), the sandbox coordinator's for the one
// its decision was made for. [seatInboxes] records that attachment by the
// seat's id: AS THE SEAT IS TAKEN ([Engine.prepareSeat], handed it by the node
// before the attach) AND DROPPED AS IT IS RELEASED ([Engine.releaseSeat], after
// the node's detach ended it). So a hold decided while this node held the seat
// and asked after the detach names the attachment that ended, which the queue
// refuses ([queue.ErrAttachmentGone]), never the one this node makes if it
// takes the seat back: keyed by the subscription alone, such a hold was carried
// onto that attachment, and a seat retaken with nothing to hold it for attached
// held with nothing left to lift it. A hold of a seat this node does not hold
// names the zero attachment, which every hold refuses. Who has to agree on the
// record: this node alone, as the holds it names are this process's queue
// client's.
//
// The no-model pause's record ([modelHolds]) keeps the attachment its hold was
// taken for, which its lift names: a seat released since ended it, and the lift
// then lifts nothing, never the hold a later attachment of the inbox took
// under the same reason. A seat's release drops the records of its pause holds,
// a person's and the no-model one, as it forgets its parks
// ([Engine.forgetPauseHold], [Engine.forgetModelHold]): its detach dropped the
// holds, and a record left would name the ended attachment, which a later lift
// would lift nothing on and report lifted.
//
// # A lift takes back a hold taken in its window
//
// Each lift of a hold the engine records (a resume's, a park's release, the
// no-model release) drops its record under its lock and makes its queue call
// outside it, because the in-memory twin's resume drains synchronously into
// handlers that read the same state, and a delivery that finds the condition
// still there comes straight back to the park or the pause. A pause, a park or
// a no-model pause taken between the drop and the call (a delivery on another
// partition that met the condition, a delivery screened against a revision with
// no model, a pause folded from the watch) re-holds the same attachment, which
// a hold one per reason leaves unchanged on the queue, and records itself. The
// late lift then lifts the hold that one owns: the seat stays recorded held (a
// park's alarm still armed) with its inbox not held, until its next delivery
// meets the condition and holds it again, at the cost of one of that delivery's
// attempts.
//
// So each lift reads its state again under the lock once its queue call has
// returned, and takes back the hold of one recorded meanwhile
// ([Engine.liftPauseHold], [seatParks.reholdParked], [Engine.reholdModelHold]),
// for the attachment its record names: the one the seat holds now, or one its
// release has ended, which the queue refuses and the release's forget drops. A
// park taken once the queue has lifted the hold (the resumed mail meeting the
// condition again, which the in-memory twin hands over inside the resume) holds
// by itself, as does one taken after the read, and a lift after it reads again
// after its own call. An apply that adds a model releases a no-model pause
// taken after the read again.
//
// Each lift's line says what that read found (`held_again`, its detail saying
// the inbox stays held), since the pause or park whose hold that read took back
// can have logged its line before the lift's, which then would read as the
// seat's last word. A seat let go meanwhile has ended that attachment, or this
// node records none for it: the queue refuses the hold, or none is asked for,
// so the line says the hold is lifted, never that the inbox stays held for a
// seat this node no longer serves.
//
// A release whose kind of park says more of the seat than its hold (a provider
// park's probe and its notice) says it as the record is dropped, under the
// parks' lock, through [seatParks.releaseParking]'s `dropped`: a park of the
// seat is taken under that lock, so one taken after the drop says what it says
// of the seat after the release, and one taken before it replaced the record,
// which the release then finds and leaves alone. `dropped` makes no queue call
// and takes no lock a park's take holds across its own.
//
// # A seat parked until an instant
//
// The budget park (budgetpark.go) and the provider park (providerpark.go)
// share their machinery (seatpark.go). Both answer one condition the same way:
// a seat that cannot run a turn until a known instant (a token window turning
// over, a model's usage limit lifting) is not handed work it would only fail,
// and is not NAKed into the same refusal on a backoff until the broker
// dead-letters its mail. Instead:
//
//   - a pause hold is taken on its inbox, under the park's own reason, so
//     nothing more is delivered to it, and the delivery that found the
//     condition is DEFERRED, which keeps its place at the head of the inbox;
//   - the hold is lifted when the condition ends, and the mail is delivered
//     again in order, ONCE: the hold keeps the resumed attachment from being
//     handed anything until then;
//   - and the record of what the park was decided under (its basis) lets a
//     change of the company release it at once.
//
// WHAT ENDS THE CONDITION is the one place the two differ in kind. A window's
// end IS the end of the budget's condition, so each budget park arms its own
// alarm for it ([seatParks.take] with an instant). A model's limit is the
// limited entries', not the seat's, and its end is learned by asking them: one
// probe per limited chain, whatever number of seats are parked on it, which
// releases them all (providerpark.go). Such a park is taken with no instant and
// no alarm of its own. The held mail is never the question either way: a
// delivery handed back by a park spends one of its deliveries.
//
// What else differs between the two is the condition, the basis and what a park
// says about itself, which each file keeps. What is the same (the record, the
// alarm, the race between a release and a re-park, a seat given up, a node
// stopping) lives in seatpark.go once, so the two cannot drift apart on the
// parts that have to be right for both.
//
// A PARK OUTLIVES NO SEAT. The seat's release detaches its inbox, which drops
// every hold on it and ends the attachment each was taken for, and then forgets
// its parks; it waits for none of the turns that can park the seat (a lost
// lease detaches the inbox under a running delivery, a turn refused mid-flight
// parks once the turn has run, and a resume runs in no delivery at all, its
// review minutes long), so a park can be asked for in between, or after. Its
// hold names the attachment the seat held as it was asked, so one asked after
// the detach is refused and records no park, the forget drops the record of
// one it finds ([seatParks.forget]), and a park taken while the seat was held
// asks for the seat again once it is whole and undoes itself where the release
// took it meanwhile ([Engine.unparkReleased]). Left standing, such a park
// outlived the seat here: a provider park's probe went on calling the models
// and publishing the seat parked while a peer ran it, a budget park's alarm
// fired into a seat a peer ran, and either one's hold kept the inbox shut on
// this node with no park left to lift it. A budget park on a launch's refusal
// below its floor takes with it the window it recorded for that delivery: the
// record is the park's, and a node that holds no park of the seat keeps
// nothing of it, the seat's next holder recording its own. A released seat
// takes its alarm with it, and a stopping node disarms every alarm: an alarm
// left armed would fire into a seat a peer now serves, or into a queue client
// that has closed.
//
// A RELEASE AND A RE-PARK of one seat agree through the record: a park takes
// its hold and records it as one step under the parks' lock, and a release
// drops the record under that lock and lifts the hold outside it, then takes
// back the hold of a park recorded meanwhile (see "A lift takes back a hold
// taken in its window" above).
//
// HOLDS ARE KEYED BY REASON, so two parks on one inbox (a seat whose window is
// spent AND whose model is limited) are two holds, and neither release lifts
// the other's. WHO HAS TO AGREE ON A PARK: this node alone. It is a hold in
// this process's queue client and an alarm in its memory, decided again from
// shared facts on the first delivery after a restart or a move.
//
// # A provider park's release and its wait
//
// A provider park taken on a seat this node no longer holds is undone
// ([Engine.unparkReleased]): its hold lifted, its place on the chain's probe
// left, the seat said released and its wait forgotten, as the release does with
// a park it forgets ([Engine.forgetProviderPark]), WHETHER OR NOT ITS RECORD IS
// LEFT. The release's forget can land between the park's take and the undo,
// dropping the record and lifting the hold, and still come before the park
// joined the probe or said the seat parked: with no record to release, the
// place on the probe would stay, its alarm calling the models as the seat, and
// the seat would read parked on every screen while a peer ran it. AND THE WAIT
// EITHER WAY: that forget drops the seat's wait before the park's join, which
// begins one again; left, nothing ended it, so it outlived the seat here, and a
// park once this node held the seat again carried its start, a seat that had
// just met the limit reading as waiting since before it let go, past the
// fifteen minutes `llm_limit_waiting` is judged at.
//
// A park for a resume ([Engine.parkOnLimit]) is no delivery's, and the resume
// runs in no delivery, its review minutes long, so the seat can be released
// under it: the park's hold then names the attachment the release ended, which
// the queue refuses. A SEAT THIS NODE HAS LET GO OF is refused its park, said
// at debug rather than as a park that failed: the seat is no longer this node's
// to park, and the node that takes it asks its own models on its first
// delivery.
//
// A provider park's release ([Engine.releaseProviderParking]) TAKES THE SEAT
// OFF ITS PROBE AND NOTES ITS RELEASE AS THE RECORD IS DROPPED, under the
// parks' lock, and publishes the notice once the release has let go of it
// ([parkNotices.note]). A turn that meets the limit again in the release's
// window parks the seat on the same chain, and that park joins the chain's
// probe and notes the seat parked after its take, which the lock puts after
// the drop. Done after the queue's resume, and so after that park, the release
// took the seat off the probe the park had just joined and noted it released
// after the park had noted it parked, so the seat, whose hold the release takes
// back or the park took after the resume, stayed held with no probe left to
// release it, and every screen drew it released. The probes' and the notices'
// locks are taken under the parks' one there and nowhere the other way round.
//
// A seat's wait ([limitProbes.waits]) is when the seat began waiting, which the
// Models page and the `llm_limit_waiting` alarm measure from
// ([Engine.parkedByEntry]). THE SEAT'S, NOT A PROBE'S: an apply that brings
// new models releases every park decided under the old ones, and the seat's
// next delivery parks it again at once on the same limit, under a new probe.
// Dated by the probe, the wait would restart at every apply, so a seat stuck
// for an hour read as parked for seconds and the alarm that turns red at
// fifteen minutes went out and came back with every config change. A wait ends
// when the chain answers (or stops reporting a limit) or the seat leaves this
// node; a park that follows a release for any other reason within
// [parkCarryWindow] is the same wait, and so is one taken in a release's own
// window, which the release leaves standing on its record.
//
// A PARK TAKEN IN A RELEASE'S WINDOW KEEPS ITS WAIT ([Engine.unlessParked]). A
// release drops its record under the parks' lock and resumes the inbox outside
// it, and only then marks the wait released ([Engine.releaseProviderParking]),
// or, for a probe's answer, ends it ([Engine.endLimitProbe]). A turn that meets
// the limit again in between parks the seat again and joins a probe, carrying
// the wait's start, and the release's late mark or end would then land on the
// record that park stands on: marked released, a re-park of the standing seat
// on another chain more than [parkCarryWindow] later began a new wait; ended,
// the park's own release found no record to mark, so the seat dropped out of
// the waits the heartbeat reports between that release and its re-park, and
// the re-park began a new wait, though the seat had waited throughout. So the
// mark and the end settle the record only where no provider park of the seat
// stands; the seat's own release forgets the wait whatever stands, and so does
// the undoing of a park that release forgot before the park's join began the
// wait again ([Engine.forgetParkWait], [Engine.unparkReleased]). While the park stands, the Models page and `llm_limit_waiting`
// read the probe's own instant, which its join carried. THE READ IS UNDER THE
// PARKS' LOCK, which a park records itself under before it joins a probe: a
// park recorded before the read keeps the record for its join, and one recorded
// after it waits for the settle, which it then reads as a release or an end.
// The parks' lock is taken before the probes' one there as in the release's
// drop ([seatParks.releaseParking]), and nothing takes them the other way
// round.
//
// # A provider park's notices
//
// A seat's park, each renewal of it and its release are announced as events
// (`seat_provider_parked`, `seat_provider_released`), which every screen draws
// the seat's condition from (internal/api/livestate). Published on the path
// that decided them, once, best effort, a broker that refused cost a WARN per
// seat per decision and the notice itself: a screen went on drawing a seat
// parked after its release for as long as nothing else moved it. And a decision
// path that ran hot published on every pass: a probe re-armed for an instant
// already past renewed every seat's park as fast as its call returned, and one
// node logged 36 000 `seat_provider_park_not_published` lines in a three-minute
// broker outage. So a notice is handed to [parkNotices], which keeps the newest
// one per seat:
//
//   - ONCE PER CHANGE: a notice equal to the last one published for its seat
//     (or on its way) is dropped, so a renewal that moved nothing is no news.
//   - THE NEWEST WINS: a notice that arrives while an older one for the same
//     seat waits replaces it, so a seat parked and released during an outage
//     is published released, once, and never parked after it.
//   - PUBLISHED BY ITS CALLER while the broker answers, so what the caller
//     decided is published before it returns; one caller at a time drains, and
//     a seat's notices go out in the order they were decided. A caller that
//     decides one under a lock no publish may be made under notes it there and
//     publishes it once it has let go ([parkNotices.note]): the order it was
//     noted in is its place among the seat's notices.
//   - HELD AND RETRIED ON A BACKOFF once the broker refuses: no caller
//     publishes while they are held, and each attempt publishes every waiting
//     notice in turn and stops at the first refusal, since a broker that
//     refused one refuses the rest. An outage costs one attempt per step of
//     the backoff, however many decisions it spans.
//   - SAID PER OUTAGE, NOT PER NOTICE: one WARN when publishing first fails,
//     and one INFO when the held notices land, with how many and for how long.
//
// WHO HAS TO AGREE ON IT: nobody. The park is this node's and holds whether or
// not a screen hears of it; what this keeps is that the screen hears its latest
// state once the broker answers. A node that stops drops what it held: its
// seats are given up with it, and the node that takes one announces its own.
//
// # A person's pause, carried out by whichever node holds the seat
//
// A pause is a coordination record (coord.SeatPause): written by the node that
// served the person's request, read by every node. seatpause.go is the reading
// half, and it does four things with what it reads:
//
//   - HOLD THE INBOX. The moment a pause lands, this node takes the
//     [inbox.HoldSeatPaused] hold on the seat's mailbox, and lifts it the
//     moment the pause is lifted, so the seat's mail waits on the broker, in
//     order, and is delivered when somebody resumes it. Taken only for a seat
//     this node holds, because a hold is taken for one attachment of the
//     inbox and a node that does not hold the seat has none to name; a detach
//     drops it with the attachment, and the seat's release the record of it
//     ([Engine.forgetPauseHold]), which is why the seat's preparation on the
//     node that acquires a paused seat takes it again before the node attaches
//     the inbox ([Engine.acquireInbox]). Every hold is taken and recorded as
//     ONE STEP under the lock the pauses are folded under, so a resume finds
//     either no hold or the record of one. The lift is the one step that
//     leaves the lock for its queue call, and reads the pause again once the
//     call returns ([Engine.liftPauseHold]).
//   - ANSWER THE SCREENING. [inbox.Conditions.Paused] and PauseUnknown are
//     read off this cache, so a delivery that raced the hold is held and
//     parked rather than run, and one that arrives before this node has read
//     the pauses at all is deferred rather than guessed at.
//   - SKIP THE SCHEDULE. A paused seat's fires are recorded, not sent.
//   - STOP THE RUNNING WORK, when the pause asked to. The per-turn fence is
//     the seat's ownership fence composed with this cache: a pause carrying
//     StopRunning closes the running turn with [turn.ErrStoppedByPerson] at
//     its next round boundary, refuses any executor launch it would make
//     (the launch asks the same fence before it opens anything and again
//     once its row exists), and the dispatcher spends the trigger rather
//     than retrying it. The EXECUTOR a turn already launched has no rounds
//     the engine sees, so this node, where it holds the seat, ENDS it at
//     once ([Engine.stopSeatRuns]): its box reclaimed, its bridge session
//     closed, its spend charged, and the turn parked on it closed as stopped.
//     That is not the fence, which leaves a run alone when its seat merely
//     moves (see runstop.go). Runs parked on a person's question stay parked
//     behind the pause. And whatever of a run's is still in flight, its
//     calls are refused while the stop stands: its box's pushes by Gitlet
//     (host.Runs.Stopped, through stoppedBy), and every bridged call by
//     its bridge session's fence (mcpbridge.Session.Fence, the seat's fence,
//     which the stop closes through stopFor).
//
// KEYED ON THE SEAT'S ID. Every pause, every hold and every question there is
// about a seat's derived agent id (ADR-0013): the record is keyed on it, the
// mailbox a hold sits on is named by it, and the scheduler and the dispatcher
// ask by it. The handle appears only in log lines, resolved from the running
// company.
//
// WHO HAS TO AGREE ON IT. The record is the whole company's (coordination). The
// cache is THIS NODE's copy of it, derived from a watch, and nothing reads it
// but this process: a peer asks its own copy. It is never written back.
//
// THREE STATES, NOT TWO. Until the watch's first complete answer arrives this
// node does not know which seats are paused, and says so ([seatPauses.known]
// false): the screening defers, the scheduler dispatches (a paused seat's inbox
// holds a fire rather than running it), and nothing takes or lifts a hold. A
// watch that ends (the store went away) keeps the last complete answer rather
// than forgetting it: an unreachable store is not a resume.
//
// A PAUSED SEAT IS ATTACHED ALREADY HELD. [Engine.acquireInbox] records the
// attachment a node's acquisition makes of the seat's inbox
// ([Engine.attachInbox]) and, for a seat a person paused, takes the pause hold
// for it: the first step of the seat's preparation, which the node runs before
// it attaches the inbox ([node.Config.SeatReady]). A hold taken after the
// attach would be taken after the first delivery can arrive, and the mail the
// pause was holding would be the first thing this node ran. A seat whose pause
// state is not yet known is held by nothing: the screening defers its
// deliveries until it is. The record and the hold are ONE STEP under the lock
// every pause and resume is folded under, so a resume lands wholly before it
// (no pause, no hold) or wholly after it (a record, which its lift reads under
// the same lock). Recorded under the lock and taken by the node outside it, a
// resume between the two found the record, lifted a hold that was not there
// yet, and the hold the node took next was recorded nowhere, so the seat
// attached held with nothing left to lift it. The attachment is recorded under
// the same lock for the same reason: a pause folded there reads it
// ([Engine.takePauseHoldLocked]), and sees either this node's attachment, and
// holds it itself, or none, and leaves the hold to the acquisition. A hold
// that cannot be taken REFUSES the seat rather than attach it without one: a
// seat whose hold could not be taken would run the mail it exists to keep. A
// pause fails only on a client that has stopped.
//
// # The model alarms' inputs
//
// The model alarms (M15 of the Models design) judge each entry's condition
// across the fleet at the owner's fixed thresholds
// ([statelog.KindLLMCredentialExpiring] and the four beside it). MOSTLY
// FLEET-WIDE: the live nodes' status records' model rows (a limit met on one
// node, a refusal on another) and the secret store's write instants are the
// fleet's, as `backup_age` reads the fleet's one backup register, so a refusal,
// a limit, a wait and a stored credential's age read the same whichever node a
// person asks. Two inputs are this node's own reading, and say so: whether
// anything signs an entry in here, and its harness's token lifetime.
//
// A READ THAT FAILS CARRIES THE LAST GOOD ONE. The status records and the
// secret store are read on every evaluation, and a blip in either must not read
// as "no model sits out" or "no token expires": that would clear every standing
// model alarm on the blip and raise each again on the next tick, two log lines
// per entry per blip, and a health count that flickered. So each input that
// cannot be read is the last one that could, and the failure is logged. Only a
// node that has never read the records judges nothing about models.
//
// A LIVE NODE WHOSE RECORD IS NOT COUNTED KEEPS ITS LAST RECORD while its
// presence stands at the epoch it wrote it under ([coord.LiveNodeStatusesKept]),
// as the runs waiting for an operator do ([Engine.executorReading]): a record
// that aged out while its node lives is a node not saying, and read as one with
// no rows, a sit-out or an outage only that node held cleared for one
// evaluation and was raised again on the next. ALL BUT THIS NODE, whose own
// rows are its memory wherever no record of its own counts: it holds what that
// record is written from, and its last record kept in their place stood raised
// on a sit-out or an outage its memory no longer held, for as long as its
// writes failed.
//
// ONE LISTING OF THE RECORDS FEEDS THE MCP ALARMS TOO ([mcpConditions]), read
// once per evaluation, so a model's and a server's conditions are judged on
// the same records of every node, and a blip carries both on the last good
// listing.
//
// # The runs waiting for an operator
//
// The executor_resume_blocked alarm ([statelog.KindExecutorResumeBlocked])
// reads the runs waiting for an operator across the fleet. A finished job whose
// resume fails on a condition only an operator clears is counted by the
// executor coordinator of the node holding its seat, from that node's own
// memory of its attempts ([sandbox.Coordinator.ResumesBlocked]). That node may
// evaluate no alarm table at all: a satellite (`roles: [seats]`) runs no state
// log, and read from each node's own memory alone, the alarm would never be
// raised for a seat a satellite held, the one topology where nobody reads the
// node's log by default. So the node says it in its status record
// ([coord.NodeStatusRecord.ResumesBlocked]) and every data node raises it from
// the live records, as the model alarms are raised, this node's own runs read
// from its memory rather than from its last record. The reading sums the
// fleet's runs and names the one waiting longest, by its seat, the node holding
// it, its condition and how long.
//
// A LIVE NODE WHOSE RECORD IS NOT COUNTED KEEPS ITS LAST WORD while its
// presence stands at the epoch it said it under ([coord.LiveNodeStatusesKept],
// the model and MCP alarms' rule too): a record not yet rewritten, or one whose
// write failed and aged out while the node lives, is a node not saying, never
// one whose runs stopped waiting, and read as the second the alarm would clear
// on that pass and be raised again on the next. A node whose presence is gone,
// or moved to a new epoch, says nothing it said before: its seats move to
// holders that say their own, and a restarted node's memory of its attempts
// went with its process. And a listing that fails is the last one that
// answered, as the model alarms' is, so a blip neither clears the alarm nor
// raises it again on the next tick.
//
// # A note to a turn whose executor is out
//
// A person's note to a turn whose executor is out waits sealed on the run's
// record (runsteer.go, ADR-0067). Before a turn launches its executor, its
// notes wait in the box its first segment holds, on the node running it
// (steer.go). From the run's launch to the turn's end they wait on the RUN'S
// RECORD in the coordination store ([sandbox.RunSteer]): the whole company,
// now. The node a person's request reaches, the node serving the run's tool
// bridge, the node that resumes the turn and the node that finishes the run's
// ending can be four processes across a restart and a seat moving, and each
// already reads the record. So any node files a note there with one
// compare-and-swap, and nothing that holds a note lives in one node's memory
// alone: what a node keeps (a bridge session's view of the record, a held poll)
// is derived and rebuilt from it.
//
// # A note is sealed, because it is an instruction
//
// The coordination store decides between writers by compare-and-set and
// authenticates none of them (ADR-0031), and a note on a record is still a
// person's words a model will read as theirs. So every note is SEALED under the
// fleet keyring by the node that files it, in a key domain of its own
// ([SteerNoteDomain]), over the note bound to its turn and its id; and the
// record carries a sealed ANCHOR naming the seat the run serves
// ([SteerSeatDomain]), written by the launch, on which a person's authority to
// steer the turn is decided. Every reader opens before use: a note whose seal
// does not open is never delivered, carried or attributed (logged at ERROR as
// steer_note_unverified), and one sealed under a key this node does not hold
// waits, with a WARN naming the keys it holds; the precedent is a person's
// answer to a parked run (internal/sandbox, answerseal.go). The run's harness
// confirming such a note on this node is turned away to ask again, for a node
// that holds the key to record, and that is said once a note, at WARN
// (steer_read_key_unknown, [runSteers.waitsForItsKey]). An anchor this node
// cannot open decides nothing, and one sealed under a key it does not hold is a
// WARN naming the keys it holds (steer_seat_key_unknown), and steer_turn says
// only who fixes it, as a condition of the fleet ([runSteers.openSeat]). What
// is NOT sealed is the `read` marker, which only the store's own compare-and-set
// guards: a party on the broker could re-deliver a genuine note to its own
// turn, or hide one from in-run delivery, and the note still reaches the
// reviewer or expires, the residual ADR-0067 states.
//
// # Every read is on the record, so the record is the carry
//
// A note is handed to a model only by a write that records it: the launch's
// own for a brief ([sandbox.PendingStore.BeginLaunch]), a take for the
// reviewer ([sandbox.PendingStore.TakeNotes]), and a SERVE for a Claude Code
// hook ([sandbox.PendingStore.ServeHook]), whose note is recorded read only
// once the job's own session shows the hook handed it over, by the read that
// collects the job ([runSteers.confirmed]): a hook's answer can be lost, or
// given up by its harness, after its write lands. So the record's read notes
// are exactly the notes the turn has read, and a segment that resumes the
// turn starts from them ([runSteers.carried]), whichever node it is on.
//
// THE ORDER READ IS THE RECORD'S OWN: the place of the write that recorded each
// read ([sandbox.NoteRead.Order]), and among the reads one write recorded, the
// order the notes were filed in, never a read's time. The writes that record
// reads are made by whichever nodes launch the run, serve its bridge and resume
// the turn, each stamping its own clock, and a note is the later word: two
// clocks a second apart would otherwise hand the reviewer and every later brief
// a correction ahead of the note it corrects. THE WAITING NOTES GO BEFORE THE
// SORT, never into it: a comparison that answered "equal" for any pair with a
// waiting note in it would not be an order at all (a waiting note between two
// read ones would tie with both while they did not tie with each other), and
// the sort could leave the read notes out of the order they were read in. A
// record holds such a note whenever one waits while a later one is read: a
// seal this node cannot open yet, or a later note a harness took first.
//
// A RELAUNCH'S BRIEF ([rowSource.Brief], [steer.Briefing]) takes the notes the
// segment's lost takes recorded, where their settle answers now
// ([rowSource.settleLost]), which the launch's write records read by its job in
// their take's place, at that take's own place among the reads
// ([rowSource.lostTakes]); then the waiting notes that open, a peek, which the
// launch's own write records read; and every note the record marks unanswered,
// by id, read off the same record. It hands the place among the record's reads
// of every note this segment's takes handed over, those the brief carries among
// them ([rowSource.orders]), and of every note it peeks that a hook's serve
// placed ([sandbox.RunNote.Place]), so the brief tells the turn's notes in the
// order the record numbers them. A PEEKED NOTE A HOOK'S SERVE PLACED is read by
// the launch's write at that place, ahead of a correction a person filed after
// it and the job's session confirmed: handed with no place, it would be told
// after every read, the correction first. EVERY such note is placed at its
// place and not at its number among the turn's notes ([sandbox.RunNote.Seq]):
// a hook's lost answer can hand two notes, which share the one place its write
// took. A note no hook was served takes the write's own place, after every
// read, and the brief places none. THE MARKS ARE READ AS THE RECORD HOLDS THEM
// NOW, never only as the segment's claim read them: a take's settle is made off
// the path that took, and can mark a read after the segment resumed from the
// record, so the relaunch's briefs, built from the reads the turn carries,
// count the mark as the record does.
//
// # A take whose answer was lost is settled, never left saying it was read
//
// A write can land and its answer not: a hook's take is held to five seconds
// (internal/api/mcpbridge's steerTakeBudget), and a compare-and-swap the store
// answers after them, or one whose answer a broken connection lost, may still
// have landed. So every take names itself ([sandbox.NoteRead.Take],
// [sandbox.HookServed.Take]), and one whose call answered an error is SETTLED
// by that name, which also leaves its write, if it is still in flight, unable
// to land after. A hook's serve answered nothing is kept unanswered by its
// inbox ([hookMemory]), removed by the job's next serve in its own write, and
// settled OFF THE HOOK'S PATH ([runSteers.settleHook]): the note waits again,
// for the job's next boundary or the review. A settle off the path asks until
// its store answers, never given up by a clock, one loop a run, ended only by
// the node's teardown, after a last attempt at every take in a window of its
// own from the teardown's start
// ([steerSettleStopBudget]), beside the give-backs and never on the stop's
// allowance: a settle that runs out leaves a take unsettled, not a lease
// lapsing, and the run records' bucket can lose its quorum alone, so settles on
// the allowance would spend what the give-backs after them were owed. The
// teardown waits them out ([runSteers.stopWith], [runSteers.settleOnce],
// [runSteers.waitSettles]), and a take still unsettled past
// [steerSettlePatience] is said.
//
// A review's take settles ON ITS OWN PATH, in answer mode, and hands over what
// its write recorded, its round still waiting on it; where the store answers
// that settle no more than the take, the take is kept and settled again by the
// segment's next round or brief, standing meanwhile for a note waiting
// ([rowSource.Waiting]), and is marked only when the segment ends, in its path
// before the run's ending can read the record, and off it where the store
// answers that no more ([rowSource.Close]). An OpenCode confirmation's take
// settles on its own path too, in answer mode, never marking: its read is
// true, the harness having confirmed delivering the note, and only its report
// rode on the answer, so what it recorded is reported there.
//
// # An OpenCode confirmation is recorded once, and reported
//
// OpenCode's plugin is handed its notes by a poll and confirms each once
// OpenCode delivered it, and only the confirmation records the note read
// ([runInbox.Read]). A harness cut off between the two, or whose confirmation
// the store never took, leaves a session that may hold a note the record says
// waits, which the next launch's brief would tell it as one it had not read. So
// a poll answers a note only once the record says it was SERVED to the job
// ([sandbox.PendingStore.ServeNotes]), in a write before its answer
// ([runInbox.Wait]), and a launch that continues the job's session tells a
// served note as one it may already show ([servedNotes], internal/agent/runner;
// the resume carries the served notes' ids, [runner.Resume.Served]). A note
// whose hook's serve a cancel lost is told so too, unless its session proved it
// absent.
//
// A CONFIRMATION'S READ IS TRUE WHETHER OR NOT ITS ANSWER ARRIVES (OpenCode
// delivered the note), but its REPORT rides on that answer: a take whose write
// landed and whose answer was lost would publish nothing, the plugin's
// confirmation made again would find the note no longer waiting and skip it,
// and the run's ending, a step having read the note, would report nothing
// either, so no node said what became of it. Two things close that. THE TAKE
// NAMES ITSELF, and one that answered an error is SETTLED ON THE CONFIRMATION'S
// PATH ([runInbox.confirm]): what its write recorded is published delivered,
// and the confirmation answered done, whether or not the plugin would ever
// confirm again. And A NOTE NAMED THAT THE RECORD HOLDS READ AT A STEP OF THIS
// JOB (an earlier confirmation's, whose answer never reached the plugin) is
// published delivered all the same ([runInbox.confirmedBefore]), under the id
// that confirmation's report took and at its read's instant, so a note both
// reported is one record. A confirmation that is turned away (below) still
// reports first what its job read before, since the run's ending reports
// nothing of a note a step read.
//
// A CONFIRMATION THE INBOX CANNOT TAKE IS TURNED AWAY TO ASK AGAIN
// ([mcpbridge.ErrNoteUntaken]), never answered done: answered done, the plugin
// never confirms the note again, and the record keeps it served and unread,
// for the run's ending to report unconfirmed and a later launch to tell again
// as one the session may already show. Turned away, the confirmation made
// again reaches where it can be recorded. Every other note the read names is
// taken and reported before it is turned away. An inbox cannot take a note in
// three cases:
//
//   - IT SERVES NO JOB: closed, its opening's end having taken it after the
//     notes route found it serving (the run ended, the seat left the node, or
//     a recovery opened the job again with an inbox of its own), or not yet
//     bound, which no opening serves. Decided under the inbox's own lock,
//     which its Close takes ([runInbox.bound]): a read that found the inbox
//     serving records through the store, a write a later Close does not touch.
//   - ITS JOB'S RECORD TAKES NO READ OF IT ANY MORE, for a note that still
//     waits on the record once the take left it ([waitsUntaken]): the job's
//     completion claimed the run, moving its record out of
//     [steerTakeStatuses] while its session still serves, until the reclaim
//     closes it; or the run's ending is decided; or a later job carries it
//     on. The confirmation made again is recorded where the claim is handed
//     back and the run goes on in its box, and refused once the run is over.
//     A note named that no longer waits is no such note, whoever read it:
//     answered done, reported where this job's earlier read holds it.
//   - THIS NODE CANNOT OPEN THE NOTE FOR WANT OF ITS KEY: it still waits on
//     the record for the job, sealed under a key this node does not hold, in a
//     fleet whose nodes hold different keys (the run's seat moved, or a
//     recovery opened its session on another node) ([runSteers.waitsForItsKey]).
//     A take hands over only a note it opened, so this node's took no read of
//     it, and the note waits for a node that holds its key, as it does for
//     every reader ([runSteers.open]). The plugin confirms it again on its own
//     backoff, and the confirmation is recorded once it reaches a node that
//     holds the key; a run that ends first leaves the note served and unread
//     on its record, which its ending reports unconfirmed where the node
//     ending the run holds the key, and a node that does not records nothing
//     of it (steer_note_end_unrecorded, [runSteers.ended]). Each such note is
//     said once, at WARN with the keys this node holds, the first time any
//     confirmation is turned away for it, whatever else turned it away, and
//     never again however often the plugin asks: once a note, so a second
//     note is not hidden behind the first. A note this node cannot open that
//     waits no more, or waits for another job, is no such note.
//
// THE INBOX IS BOUND BEFORE THE BRIDGE FILES THE SESSION'S OPENING
// ([Engine.openBridge]). A request reaches the opening the moment the bridge
// files it: a recovery's box at once, with the token it already holds. And the
// bridge files it before it ends the job's opening this node held until then,
// an end that waits up to its bound (five seconds) for that opening's answers
// still on their way ([mcpbridge.Bridge.Open]). Bound only once Open had
// returned, the inbox would be served unbound all that while: a poll of it
// spinning, its wait handing back a channel that is always closed, and a
// confirmation turned away as one no inbox took. A session that does not open
// serves no notes, and the caller closes the inbox, which stops the view its
// bind started.
//
// THE SESSION NUMBERS ITS CALLS AFTER THE ONES THE LOG HOLDS
// ([sandbox.OpenedJob.LastSeq]): a recovery's after its own job's, and a job
// continuing one that asked a person after the asking job's, so the round's
// calls keep the one order its collection reads them in. AND READS EACH CALL'S
// ID UNDER THE KEY THE JOB NAMES ([sandbox.OpenedJob.CallIDMeta]): the one the
// runner starting the job's box declares its harness sends it under, for the
// launch, and this node's runner of the job's harness, for a run taken over
// while it works ([Engine.reopenAgentRun]), each set by [Engine.openBridge] on
// the session the bridge opens, never on the launch's own literal.
//
// # Who records what became of a note
//
// The node whose step read it: the runner for a round of the engine's loop or
// a launch's brief, the run inbox of the node serving the run's bridge for a
// read inside the run ([runInbox]), and a read of the job's box for a hook's
// serve its session showed taken ([runSteers.confirmed]), under ids derived
// from the turn and the note. And the node that finishes the run's ending, for every note no step
// reported reading when the turn is over, as the run's record knows it
// ([runSteers.ended], [sandbox.CoordinatorOptions.SteersEnded]): one still
// waiting, or read first by the brief of a job that never started, expired;
// one served to a harness that never confirmed it, unconfirmed, reported at
// the serve a hook was served it at ([noteEnd]); one a take marked unanswered
// recorded, missed, where no later launch whose job started told it again; and
// one a later launch's brief told again, its job started, delivered there
// ([steer.Retold]). Under ids derived from the ending, so a repeat of either is
// the same record. Never at a park: a question can wait days, and the turn that
// resumes reads what waited.
//
// A read marked unanswered is told again by the brief of every launch after it,
// whatever session the launch continues ([steer.Briefing.Unanswered], a mark
// landing after the segment resumed among them): the runner names each such
// note in its request ([runner.AgentRunRequest.Retold]), and the launch's write
// records it read by the job it opens, told again, at the marked take's own
// place among the reads, keeping the take's read beside the telling
// ([retoldNotes], [sandbox.NoteRead.Retold]) for a launch that fails after its
// write.
//
// # An item for one job is that job's alone
//
// A record can hold an item for one job beside a person's notes
// ([sandbox.RunNote.For]), which only the store's own writes file (a filing of
// a person's note that names a job is refused, [sandbox.ErrNoteKind]) and the
// next launch removes. So every reader of the record's waiting notes names the
// job it reads inside ([runSteers.waiting]): the run inbox the job it serves,
// and the review and a relaunch's brief none, which hands them no item. And
// every step that reads a person's notes passes an item over, whatever its
// read: the carry ([runSteers.carried]; an item is never carried, read or not,
// since what resumes the turn is not inside its job), the notes a continued
// session may already show ([servedNotes]), the marks a brief tells again
// ([unansweredNotes]), the record of a note delivered ([runSteers.delivered])
// and the ending ([sandbox.PendingRun.EndedNotes]).
//
// # A continued session's floor
//
// A launch's floor for a session it may continue is that session's own
// context with the brief that continues it: the turn's kept capture as that
// capture measured it (runner's SessionBasis.ContextTokens), never its job's
// newest capture's, and the job's own copy in its box as its newest capture
// measured it, kept or not (SessionBasis.CopyContext), or its record where
// larger.
package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// buildProviders turns the company's providers.llm map into the ordered
// registry the per-phase resolver reads.
//
// ORDER COMES FROM THE CONFIG DOCUMENT (config.Company.ProviderOrder): the
// last resort, "the first provider configured", over a Go map would differ
// between seats and restarts. NO PROVIDERS IS A NIL REGISTRY, not an error: an
// empty providers.llm validates, and the dispatcher holds work on it until a
// provider arrives ([Company.Models]).
//
// EVERY ENTRY ON THE NODE'S ONE [harness.Plans], which outlives the revision,
// so a rebuilt entry finds the windows its predecessor sat out; nil (a company
// built only to validate) gives each entry its own.
func buildProviders(c *config.Company, r *config.Resolver, plans *harness.Plans) (*phase.Registry, error) {
	order := c.Providers.ProviderOrder()
	if len(order) == 0 {
		return nil, nil
	}
	entries := make([]phase.Entry, 0, len(order))
	for _, key := range order {
		spec, ok := c.Providers.LLM[key]
		if !ok {
			// Cannot happen (ProviderOrder comes from the map), but a silent
			// skip would move a seat onto another model with nothing said.
			return nil, fmt.Errorf("engine: provider %q is in the order but not in the map", key)
		}
		p, err := BuildHarness(key, spec, r, plans)
		if err != nil {
			return nil, err
		}
		entries = append(entries, phase.Entry{Key: key, Provider: p})
	}
	return phase.NewRegistry(entries)
}

// log is the package logger.
var log = logging.Get("engine")

// restoreBundles restores every harness entry's configured credential bundle
// where its state directory holds no login yet
// ([harness.Provider.RestoreConfiguredBundle]).
//
// Taken only by boot and apply, never by [NewCompany]: `crewlet validate` must
// neither write a login nor wait on the login's lock. It runs before anything
// calls the epoch's models (before publication; on an apply before
// [Engine.prepareReflection]), and on an apply only after every stage that can
// refuse the revision ([Engine.Apply]), so a refused one leaves no login in the
// running epoch's state directory. A failure is only WARNED (a node can be
// logged in by hand); an entry `crewlet llm logout` emptied
// ([harness.ErrLoggedOut]) is warned as `harness_bundle_withheld`.
func (c *Company) restoreBundles(ctx context.Context) {
	if c == nil || c.Models == nil {
		return
	}
	for key, provider := range c.Models.All() {
		cli, ok := provider.(*harness.Provider)
		if !ok {
			continue
		}
		err := cli.RestoreConfiguredBundle(ctx)
		switch {
		case err == nil:
		case errors.Is(err, harness.ErrLoggedOut):
			log.WarnContext(ctx, "harness_bundle_withheld", "provider", key, "detail", err.Error())
		default:
			log.WarnContext(ctx, "harness_bundle_restore_failed", "provider", key, "error", err)
		}
	}
}

// BuildHarness constructs one providers.llm entry's provider.
//
// Exported for `crewlet llm`, which must report on the SAME provider the
// engine builds, not a lookalike that drifts.
//
// Missing credentials still BUILD: every call then fails with the harness's
// own "not authenticated", which `crewlet llm doctor` explains, rather than
// one login taking the company down at boot. The entry holds ONE login and at
// most one token or API key: a second key is a second fallback entry.
//
// plans is the node's registry of sign-ins ([harness.Config.Plans]); nil
// gives the entry one of its own.
func BuildHarness(key string, spec config.LLMProvider, r *config.Resolver,
	plans *harness.Plans) (*harness.Provider, error) {
	stateDir, err := spec.ResolvedStateDir(key)
	if err != nil {
		return nil, fmt.Errorf("engine: provider %q: %w", key, err)
	}
	// The model, RESOLVED HERE: Tier B stores "${VAR}" verbatim, and the
	// harness would get the literal reference on --model.
	//
	// A REFERENCE THAT RESOLVED TO NOTHING is refused, naming the variable:
	// unlike a missing credential, whose sign-in error names it, a missing
	// model silently runs the harness's default, a model nobody chose.
	// [config.Resolver.Missing] counts an empty answer as absent (Expand
	// would not report it) and tests every reference, so `${PROVIDER}/sonnet`
	// with PROVIDER unset is refused too; the blank check catches a value
	// that is all white space.
	model := strings.TrimSpace(r.Value(spec.Model))
	if missing := r.Missing(spec.Model); len(missing) > 0 || model == "" {
		if len(missing) == 0 {
			// Every reference answered, with white space: config refuses
			// a blank written field, so the blank came from them.
			missing = envref.Names(spec.Model)
		}
		return nil, fmt.Errorf(
			"engine: provider %q: model is %q and %s resolved to nothing — unset, "+
				"or set to an empty value. Set it in the environment, or with "+
				"`crewlet secrets set`",
			key, spec.Model, strings.Join(missing, ", "))
	}
	// RESOLVED HERE for the same reason.
	env, missing := r.Map(fmt.Sprintf("providers.llm.%s.env", key), spec.Env)
	for _, ref := range missing {
		// Warned, not refused: an unset optional tuning variable must not
		// take the company down.
		log.Warn("harness_env_unresolved", "provider", key,
			"path", ref.Path, "variables", strings.Join(ref.Names, ","))
	}

	// Validation loads this profile too (LLMProvider.validateProfile), so a
	// failure means an unvalidated document: refused rather than built on
	// whatever CredentialSources made of it.
	if _, err := harnessprofile.Load(spec.HarnessName(), spec.Overrides); err != nil {
		return nil, fmt.Errorf("engine: provider %q: %w", key, err)
	}

	// Every credential falls back to a CONVENTIONAL name
	// ([config.LLMProvider.CredentialSources], the rule the Secrets screen's
	// account also follows), and the resolver reads the secret store BEFORE
	// the environment, so the token `crewlet llm login <key> -capture-token`
	// stores is found with no ${VAR}. The API key's convention is the
	// harness's own variable (ANTHROPIC_API_KEY for Claude Code), so an
	// exported key needs no YAML change; it reaches the child only under
	// auth.mode api-key ([harness.Auth]).
	sources := spec.CredentialSources(key)
	auth := harness.Auth{
		Mode:            harness.AuthMode(spec.ResolvedAuthMode()),
		Token:           resolveOrConvention(r, sources.Token.Value, sources.Token.Fallback),
		TokenConfigured: spec.Auth.Token != "",
		APIKey:          strings.TrimSpace(resolveOrConvention(r, sources.APIKey.Value, sources.APIKey.Fallback)),
	}
	bundle := resolveOrConvention(r, sources.Bundle.Value, sources.Bundle.Fallback)
	bundleConfigured := spec.Auth.CredentialBundle != ""

	return harness.New(harness.Config{
		Key:                        key,
		Model:                      model,
		Effort:                     spec.Effort,
		Harness:                    spec.HarnessName(),
		StateDir:                   stateDir,
		Overrides:                  spec.Overrides,
		Timeout:                    time.Duration(spec.Timeout() * float64(time.Second)),
		MaxConcurrent:              spec.Concurrency(),
		Env:                        env,
		Auth:                       auth,
		CredentialBundle:           bundle,
		CredentialBundleConfigured: bundleConfigured,
		Plans:                      plans,
	})
}

// resolveOrConvention resolves a configured ${VAR} reference, or falls back to
// looking up a conventional variable name.
//
// The fallback is a LOOKUP, not an expansion: Value would return the bare
// name itself as if it were the credential.
func resolveOrConvention(r *config.Resolver, configured, conventional string) string {
	if configured != "" {
		return r.Value(configured)
	}
	if conventional == "" {
		return ""
	}
	return r.Lookup(conventional)
}
