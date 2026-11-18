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
