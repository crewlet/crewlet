// Package builtin holds the tools the engine itself ships.
//
// Every one of them speaks FOR a seat — asking a colleague, loading a skill
// this agent synthesized, recalling its own episodes, marking its own
// onboarding — which is why they take the turn rather than reading a handle
// out of their arguments. The seat comes from the surface the runner built
// (tools.SeatCallable); a model that spelled a different handle cannot become
// somebody else by asking.
//
// One registration per epoch, not per seat: the catalogue an agent is shown
// comes from the registry, so a per-seat registration would put N copies of
// every builtin in it — and the fact that varies per call is the CALLER, not
// the tool.
//
// # How a write is made once
//
// Every write these tools make is sent again sometimes — a turn redelivered
// after a crash, a call repeated after an `unknown`, a person pressing retry —
// and the state log collapses a repetition only when it arrives under the SAME
// operation id. So every id a write derives — the operation's, and a created
// task's, comment's, view's, checklist item's, page's, room's or chat
// message's own — comes from ONE identity per call, chosen by one rule:
//
//   - IN A TURN, the turn: its work key (or its run, for a turn with no
//     ledgerable trigger), the instant that unit of work began, the tool, a
//     digest of the call's own arguments and how many different calls the
//     run made first to the same tool, or to a tool that undoes it — its
//     FAMILY, which the domain owning the pair declares (chatlet.CallFamily:
//     joining a room and leaving it) — derived ONCE, as the call's key
//     ([callOpKey], [Actor.callKey], [Actor.OperationSince],
//     turnctx.CallLog.OrdinalAmong), and every id the call's writes carry is
//     derived from that key: by the knowledge base's and chat's stores, and by
//     these tools for the tracker ([opIDFor], [commentID]). A re-run makes the
//     same calls and derives the same ids; a different call, or the same one
//     after a different one — a leave between two joins included — is a new
//     operation.
//   - OUTSIDE A TURN, the request's or the call's own operation. A transport
//     that names its requests — the act transport and the HTTP write
//     surface — carries the `Idempotency-Key` a retry repeats, held to the
//     operation grammar and scoped to the principal by internal/api/opkey,
//     as [Actor.WorkKey], put there by [Actor.Keyed] and dated by the key's
//     own instant, a key that carries none refused (and as
//     [doclet.Actor.OpKey] for a page write, [chatlet.Actor.OpKey] for a chat
//     one), so a request is a unit of work exactly as a turn's trigger is
//     and every id is derived from it as a turn's are. The operator's
//     assistant over MCP names the call's OPERATION ([Actor.Operation])
//     instead: it brings back the `op_id` its call was answered with, or is
//     minted one; that operation carries the instant it was minted and NAMES
//     THE CALL — the tool and a digest of its arguments — so it is accepted
//     only with that call: the same call made again is the same operation,
//     and anything else under it is refused before a write. That is ONE rule
//     for every family of write tools — Worklet's, the artifacts', the
//     datastore's and Gitlet's ([operations]) — and only what a turn
//     or a key derives stays each family's own.
//   - WITH NEITHER — a tool that takes no `op_id`, called over MCP — each
//     write is fresh, since nothing will repeat it. The page and chat tools
//     are such tools, and so are write_project and write_work_catalogue,
//     whose writes restate what they declare, so a repeat under a new
//     operation already changes nothing a first attempt landed.
//
// There is deliberately no second key: a request key beside the operation, a
// seed beside the turn, would be a second answer to "is this the same write",
// free to disagree with the first. And a caller is told only ITS OWN way to
// repeat: an answer carries an `op_id` only where its caller holds an
// [Actor.Operation] ([withOperation], [sameCall]).
//
// # And what it answers when nobody knows
//
// A write whose outcome is `unknown` is answered as a FAILED result naming the
// operation to make again ([unknownWrite], [failedUnknown]) — never a receipt
// carrying `outcome: unknown` beside a key or a version, which a model reads as
// the write done and a person's screen would draw.
//
// # What the tracker's tools derive from a call's key
//
// The tracker's store is handed the ids its writes are published under, so
// its tools derive them from the call's key ([callOpKey]) rather than handing
// the key on, each under a namespace of its own ([opIDNamespace],
// [commentNamespace]) that keeps the ids apart from each other and from every
// store's derivation over the same key. A namespace is FIXED for the life of
// the format: a new one makes every retry straddling the change a second
// write.
//
// The operation id ([opIDFor]) is named `<verb>-<object>`, the object one
// grammar segment ([statelog.OpNamePart]), where the stores name theirs by the
// verb alone: under an [Actor.Operation] each write is a STEP whose name is
// its whole identity within the call, and one call writes one verb on several
// objects. A comment's id ([commentID]) and a created task's or view's
// ([createdTaskID], [createdViewID]) are UUIDv5s, not operation ids, because
// each keys its row apart from the operation writing it (a page's comment, by
// contrast, IS its operation: [statelog.KeyedObjectOpID]). So they derive from
// the key as the work began ([Actor.asBegun]): a rebase re-mints operations
// for a ledger that forgot them, and the row has not forgotten. Every id binds
// the call's digest beside the key, as the stores bind what a write says,
// since a request's key names the request rather than what it asked; in a
// turn the digest is already in the key.
//
// # The code tools
//
// The code tools (codetools.go and its siblings) read and write the company's
// Gitlet repositories on the server, never in a clone, from ONE list
// ([CodeTools]) a seat's registry and the operator catalogue share.
//
// Every read is judged where it is answered. The gate admits everybody
// (`authz.ClassEverybody`), since a repository's read grant is derived by the
// serving node from its own chart (ADR-0088) and the gate cannot ask it before
// the read. So each call names its caller ([CodeAskerOf]) and the answering
// node holds the read to that grant: a listing leaves out what the caller may
// not read, and a read of it is refused `not_permitted`. No argument names who
// reads.
//
// Every read is decided again for whoever woke the turn ([ceilingOf]): a seat
// reading in a turn somebody woke is their deputy, and would otherwise hand a
// `visibility: unit` repository to anybody who can message one of the unit's
// seats. The tool's row is decided for the requester ([authz.DecideWithin]),
// so a stranger, or a person who may not act now, reads nothing. A colleague
// seat's grant rides on every read ([gitlet.ReadQuery.CeilingSeat]) and is
// judged by the answering node ([gitlet.ErrAboveCeiling]), never on the turn's
// memo of the repository, which would keep reading for a colleague a later
// activation shut out. A person bounds nothing (every person maintains every
// repository, ADR-0085), and a call outside any turn reads for its caller
// alone. The ceiling only narrows: what is read is the seat's own read.
//
// Rows at the seat's level, objects by id. A repository and its refs are rows,
// read linearizably ([seatReadLevel]), so an answer includes every push that
// landed before the call. An object id names the same object on every node,
// so its read needs no barrier: it is asked at a SYNC level floored at this
// node's own pushes to the repository and at where each named ref last moved
// ([CodeDeps.gitFresh], [gitlet.RefsFloor]) — never at the checkpoint the
// resolve read the ref at, which sits past its own barrier and would make a
// current copy sync for nothing on every read by a ref. A turn pays one
// barrier per repository ([CodeDeps.repo]) and one per call that names refs,
// all resolved in one read ([CodeDeps.resolveRef]); a call naming commits by
// id pays none beyond the repository's.
//
// Text somebody else wrote is fenced. A file's lines (code_read, code_blame),
// a patch (code_diff) and a commit's subject (code_log, code_blame, up to
// [gitlet.MaxLogSubjectBytes]) are a committer's words a read by object id
// cannot attribute, so each goes out fenced at trust `external`, naming no
// writer (ADR-0040); every other string — a path, a ref name, a description,
// an author — is defused ([foreignRead]). The Gitlet node counts the fences
// and the defusing into a page's bytes (internal/gitlet/query's page.go).
//
// One page is one tool answer, sent as measured. A Gitlet node cuts a page at
// [gitlet.MaxGitAnswerBytes] of compact JSON, and these tools send it compact
// ([compactAnswer]): indented, a page of short entries would overrun the
// answer it was cut to fit. A row listing is cut here, whole rows, at the
// answer as sent ([foreignRead.fit]). A page the fence lengthens past the
// answer (text spelling fence markers) is refused naming a narrower read,
// never cut.
//
// # The company's servers
//
// ssh_read and ssh_operate reach a seat's servers for the company alone, and
// ssh_operate only once a person approved that one call (ADR-0107, ssh.go).
//
// # What an agent records in the atlas
//
// atlas_record and atlas_dispute are an agent seat's alone, and each is held
// to a tool call of the same turn: the engine reads the cited call's answer
// itself, derives the record's confidence from the cited tool, and lets an
// agent remove nothing and decide no meaning (ADR-0132, atlasrecord.go).
//
// # The one authorizer
//
// One authorizer gates both catalogues (a seat's registry and the operator's),
// built from authz.Decide over the running chart and wrapped round every tool
// ONCE, at registration: the action IS the tool's name, so there is exactly
// one place the check could be forgotten. A NIL AUTHORIZER REFUSES, the
// opposite of a nil skill gate and deliberately so: a missing skill gate is a
// company that locked nothing, a missing authority decision is a surface that
// shipped without wiring one. Register pushes Deps.Requesters into the
// artifacts, code and health deps as it pushes Authorize.
//
// What a tool is ABOUT is a table of ARGUMENT NAMES, not closures: a name can
// be walked against the tool's own schema, a closure only called with the
// arguments somebody thought of. A personal verb that takes no handle is about
// the caller. The few checks that need a stored row first (re-routing a task,
// a project's lead-only facets, archiving it, setting somebody's priorities)
// ask the same seam from inside the tool under a dotted action. A person verb
// that takes a name resolves it to a RECORD first and is decided on that; the
// directory is asked only after the verb was decided on the record nobody has
// resolved, so a caller who leads nobody is refused before any lookup, in the
// words a seat they do not lead gets, and the directory's own words go to the
// log, never into a refusal. That keeps the directory from being a roster.
//
// The gate REFUSES AN ARGUMENT A TOOL DOES NOT DECLARE ([ErrUndeclaredArgument]):
// a builtin reads its schema's properties and nothing else, so an undeclared
// one was dropped and the call answered as though it asked for less (a retired
// `owner` saved as a shared tab). It derives the principal from the turn when
// the context carries none, never over one already there. Its wrappers are
// POINTERS: a struct holding a func is uncomparable, and the engine's
// epoch-identity `==` over a seat's tools panicked on it.
//
// # The requester ceiling
//
// Inside a turn somebody woke, the gate binds the turn's requester ceiling to
// the call ([bindCeiling], from Deps.Requesters: composed at most once a call,
// and only when an action's row names an app or a family decides a relation).
// The authorizer then decides every action of a seat's call, the gate's and
// every one a tool asks inside itself, by authz.DecideWithinApps, so every app
// verb of every family is held to the apps whoever woke the turn uses
// (ADR-0109), refused in words naming the app they lack. The ceiling is
// composed in ONE place for every family ([ceilingOf]): the seat's own voice
// among a turn's senders is no stranger's (the outcome of its own act is its
// own work), and any other sender with no known requester is. A requester this
// node cannot compose is undecidable, never a turn nobody woke. A read-only
// builtin is told its turn as a tools.TurnBound, never made seat-scoped. A
// seat's decision no gate bound is REFUSED as undecidable, the closed end; the
// one production caller deciding a seat's call outside any turn, the offering
// question ([Admits], [Withheld]), says so with [OutsideAnyTurn].
//
// A colleague's ask carries whoever the asking turn works for ([workingForOf],
// from the bound ceiling, never the ask's arguments), and the woken turn reads
// it first ([WorkingForCeiling], exported for the one write the engine makes
// for a turn after it ended, so that close and every call compose one
// requester from one carrier): a chain of asks never widens (ADR-0109's
// amendment). Every actor a seat's turn builds ([actorFor], [pageActor],
// [chatActor]) states whom it writes for ([writtenForTurn]); a requester this
// node cannot compose refuses the call unavailable ([unattributed]), and the
// stores refuse an agent's actor that states nothing, so a new site cannot
// forget it. Every code request carries the actor's carrier beside its ceiling,
// stamped together by every sender, so the deciding node holds the two to each
// other. An engine write made as a seat outside any turn says so
// ([types.ForNobody]).
//
// # One operation rule for every family
//
// The tracker's, the artifacts' and the datastore's writes offer `op_id` on the
// operator's MCP alone, minted for the call it names, taken back with that call
// only, and refuse it wherever a turn or a request key already names the
// operation, in one set of sentences ([operations]); [OperatorTools] is the one
// place that says the surface holds its operations. Only what a turn or key
// DERIVES stays each family's own ([opIDFor], artifactCallNamespace), each
// namespace fixed for the life of the format: a per-family copy of the rule is
// where a skipped check makes a second write under an `op_id` promised to make
// nothing twice. Every keyed write, a page's and a chat gesture's included,
// takes its key from [callOpKey]; there is no per-domain copy.
//
// # The artifacts and datastore tools
//
// One tool per authority row (artifacts*.go): each reads the stored artifact
// and asks authz.DecideWithin itself, for the caller then for the requester
// ceiling, sparing only edits to an agent's own work. Every byte goes through
// internal/artifactupload; an executor's box is streamed through the box, never
// handed a URL. A project file copied in (`from_project_file`) is decided, read
// and refused as `read_project_file` reads it, through the one copy read
// ([WorkDeps.copyFrom]), a missing source being `invalid` (the caller's
// argument), its bytes read whole and held to the row's digest before any
// moves. No tool takes `force` or shares an agent's work with the whole
// company; a stale publish is answered with the live version, fenced. A FOLDER
// GRANTS NOTHING: an agent files (`move_artifact`: editor on the artifact and
// contributor on the destination, both through the ceiling) and makes
// subfolders but never reorganises a tree; a folder resolves only among
// folders the agent AND the requester see. The adopted brand
// (`get_company_brand`) is everybody's under every ceiling, its voice fenced
// under the `company` trust, and a logo reaches a version only as a
// `from_brand` slot the pipeline copies. The datastore tools (artifactdata*.go)
// share the head, the decision and the stamp and add a turn guardrail (1,000
// ops a segment, a retry not charged twice): a row is answered only when the
// requester could read it too, and a write is stamped with the lower of the
// agent's role and the requester's. Both put a seat in the artifact's presence
// room once the call was allowed.
//
// # The code tools' holds and writes
//
// The tools are offered only where the node serves Gitlet with the ceiling's
// halves wired ([CodeDeps.serves]: Gitlet, Requesters, Chart), so no surface
// reads for whoever messaged a seat what the seat itself may not. The caller is
// named by one conversion ([CodeAskerOf]) the REST reads share. A repository is
// resolved ONCE A TURN ([CodeDeps.repo]); every refs a call names go through
// ONE code.resolve_ref ([CodeDeps.resolveRef], one barrier, one moment), and
// [CodeDeps.refsOf] is the only gitlet.RefQuery in the package
// (TestOnlyTheSeamResolvesARefForATurn). A repository at the domain's bounds is
// answered: `code_get_repo` cuts its rules whole at the answer and pages the
// rest, `code_list_repos` lists past a repository whose rules alone exceed an
// answer rather than ending there. The change reads (codechanges.go) name the
// access each tool needs in ONE table ([changeToolNeeds], held both ways to
// [ChangeToolNames] and [ChangeWrites]); a call reads the change ONCE at the
// seat's level and every later read floors at that position, reads by object
// id at the floor of the records that moved the heads, decided by the code
// domain's own rules (gitlet.RevisionDiff, ObjectFloor, PlaceFindings) so a
// seat and a screen are shown one diff. A change's text names its writer, and
// every page is cut whole-row at one answer by that row's own cursor, compact.
//
// Every code write names ONE operation by the family rule, its object bounded
// (the repository's id, `new` for a create) so no name carries an id past
// statelog.MaxCallerOpIDBytes. A seat's push names its run; a person writes as
// their human seat; a token bound to nobody writes nothing
// ([gitlet.UnboundDetail]). In a turn somebody woke each write is decided again
// for them ([CodeDeps.writeCaller]); a colleague rides on the write as
// gitlet.WriteCeiling, a credential acting as no seat that holds the app as
// ceiling of nobody, and the ceiling is judged where the write is decided,
// never here. A seat's write is built at a session floored at its own writes,
// its turn's read and where each resolved ref last moved. A second write of a
// call (an open's ask of its reviewers, a reply's thread mark, a merge's
// `close_task`, apply_suggestions' resolving of findings) is a statelog.StepOpID
// step ([stepTwo], ONE byte: the operator's longest step already holds 125 of
// the id's 128), decided at or past the first's record, and its failure is said
// beside the answer, never as the first write's. A merge answers every unmet
// precondition, never the first alone; its branch deletion is the Gitlet node's
// to decide, so the tool reads no gate. A tier below write
// ([CodeWritesWithheld]) leaves off every write its grant cannot admit, judged
// on the tier because a role grant is an administrator's and a unit's lead sets
// its seats' tier. Refusals map onto classes every write surface already
// renders ([pushClass], [changeClass], [conversationClass], [checkRefused]);
// check rerun steps are named by the attempt they queue ([rerunStep]), never by
// their place in the call. A person-only write ([PurgeWrites], [ImportWrites],
// a check report) is `humanOnly` in its row and refused in the tool again;
// `code_import_repo`'s `credential` is a variable's NAME, made the `${NAME}`
// the record carries, never a token's value.
//
// # The holds on seat branches and scratch repositories
//
// Every resolution of a call's `repo` takes the turn's hold on a scratch
// repository before the call's first request ([HoldRepo]), recorded once per
// run and repository, never per work key, at the run's own derived instant
// (a record minted at a start every ledger has swept is answered `unknown` for
// good); the turn's lookups keep only resolutions whose hold was taken
// (TestOnlyTheSeamResolvesARepositoryForATurn). Every seat branch a turn names
// passes [HoldRef] before the request that reads or writes through it, a write's
// head resolved first because a write that moves nothing orders nothing
// (TestAChangeWriteHoldsTheSeatBranchItDependsOn,
// TestEveryCodeToolHoldsTheSeatRefsItNamesBeforeItsRequest); a hold is CONFIRMED
// once a turn (turnctx.Holds.Confirm). An executor's repositories are code
// reads too: a run is given none at launch and each `code_checkout` is
// resolved through [CodeDeps.ResolveRunRepos], the one resolution, read
// strictly since what it names is a grant, and a refusal is the checkout's own
// answer ([RefusedCheckout]), never a node failure. `code_search` and the check
// readers fence every hit, log and summary `external`, naming no writer, and
// page by a cursor naming its whole subject (a search's fingerprint and last
// key, a check attempt), so a page never re-runs the whole read and a cursor
// sent with other arguments is refused.
//
// # The health, atlas and session tools
//
// The health tools ([HealthTools]) JUDGE NOTHING: what is wrong is the alarm
// table's to say (ADR-0015), so neither ranks a node, and a node that
// evaluated no table says so rather than reading as healthy. Both page by
// node id and a page asks only its own nodes (asking the whole fleet per page
// cost N^2/limit answers); a seat's hot list is judged hottest first and only
// to the list's cut. They are admin's, a seat reads them only where its role
// carries the health grant, and [Withheld] asks [Admits] of every builtin so
// what a seat is offered and what it is admitted are one decision, only a
// REFUSAL withholding (a node that cannot decide offers the tool and lets the
// call decide).
//
// The atlas read tools narrow every answer to the caller with atlas.For
// (ADR-0127); the eleven atlas writes are a person's and the operator's alone,
// each read as its caller, planned, and recorded on the version the read saw,
// refused stale. A fact the settings, Gitlet or the engine declare is refused,
// never written beside.
//
// `list_sessions` and `read_session` (ADR-0073) take no seat: each reads the
// seat of the turn the call is bound to, so another seat's turn is answered as
// one nobody has. Every word is fenced by provenance.ExecutorSession through
// one door ([sessionLabel]); a page holds at most the rows that fit one 64 KiB
// answer at their worst, and a search says how many sessions it searched, never
// that nothing matches past them. Every call asks the switch of the node's own
// revision ([SessionDeps.Keeps]), so a launch that pinned an earlier revision
// stops at its next call.
//
// # The catalogue and what hands over words
//
// The catalogue files every built-in on one of ten groups and the gate its
// authority row asks. The gate is DECLARED rather than read off authz.ClassOf,
// which names and never decides for a caller to branch on, and a walk holds
// every declared gate to its row both ways, so a changed row turns the walk red
// until the catalogue agrees. Which tools hand a model somebody else's words is
// DERIVED, never listed: the plain encoder ([jsonAnswer]) refuses an answer
// holding a field a record declares somebody's words, [foreignRead] refuses one
// it did not fence, and a sweep holds the set of tools whose answers carry those
// words to `handsOverWords` in both directions. Every domain a surface writes is
// declared once ([nativeDomains], held both ways to estate.SurfaceWriteDomains),
// and never the executor log, whose writes are the engine's own record.
//
// # Whose failure a condition is
//
// A failed phase or turn records a condition under a kind ([ConditionKind]):
// the environment's, never the engine's own defect. A coding sandbox's class
// is [SandboxKindPrefix] and the class ([sandbox.Why]); a deadline is
// `timeout` and an abandoned wait `canceled`, the provider layer's own words.
//
// THE STATE LOG HAS TWO KINDS. [KindStateLogUnavailable] is the log, the node
// or the broker refusing a read or a write (the node evicted, behind or
// deferring, the log full or wrong, a broker that refused it), which another
// node, a later attempt or an operator gets past. [KindStateLogWriteRefused]
// is the log refusing a write for what the write itself is or names
// ([statelog.Reason.RefusesTheWrite]: a record too large for the broker, an
// object deleted for good, an operation id that names another write, an
// operation a later record undid), which every node and every attempt meets
// alike. One kind for both put a write the work asked for and no node could
// make beside an outage, on the node's side. [stateLogSaid] picks between
// them; every refusal but a write refused for what it is is the first.
//
// EVERY KIND IS THE INFRASTRUCTURE'S OR THE WORK'S, decided once
// ([InfrastructureKind]). A failure on the infrastructure (a store, a log, a
// broker, a disk, the fleet's nodes and what they hold) is counted as the
// failing node's, never the work's: a seat's mail that failed on one is
// handed back as the node's own (internal/engine's nakFailure), and a
// delivery it spends is kept with its group's unhandled deliveries rather
// than with its poison. Every kind the package declares is the
// infrastructure's but the ones a piece of work can cause: a write the state
// log refused for what the write itself is ([KindStateLogWriteRefused]: no
// node can make it and no wait lets one, and a different write, a split
// change, a fresh operation or an object not deleted, is what gets past it);
// and a deadline and an abandoned wait, a call that ran past its own wall
// clock or that its caller gave up on, which the length of what was asked
// decides as much as the node does. Anything that is no kind of a condition
// is on neither side. A record's kind does not always name the failure its
// mail is sided by: a provider's failure that wraps a condition (a harness
// whose prompt a full disk could not take) is recorded under the provider's
// own kind (internal/agent/runner's ErrorKind), while its mail is handed back
// as the node's.
//
// A CODING SANDBOX'S CLASS IS PUT ON A SIDE CLASS BY CLASS, by whether a mail
// can cause it ([sandboxClassIsTheNodes], each class with its reason), never
// as a block: a provider that did not answer or is at its account's limit
// spent a seat's backlog into its poison when every class was the work's.
// Every class a turn or a resume fails on is met where the engine starts a
// box for the seat (the provider's create, the company's setup recipe, the
// engine's probes of the box, a relaunch), and every request and command
// there is the deployment's or the engine's own; a command the seat's
// executor runs in its box ends with its own exit status (a timeout's 124, a
// signal's), never with a class (internal/sandbox's e2b.go). So a provider
// that did not answer, at the account's limit, wanting it paid for or
// refusing the company's credentials, a setup download failing on the far
// side and a configuration the deployment cannot start are each the node's;
// a class this build does not name is not, and a class added there is
// decided on its own.
//
// OF THE INFRASTRUCTURE'S, THE ONES ONLY AN OPERATOR CLEARS are decided there
// too ([OperatorKind]): this node's disk full, mounted read-only or failing to
// read or write, its limit of open files reached, or the fleet's nodes holding
// different keys or running different builds. No wait clears one: somebody
// has to act on the host, on the keys or on the upgrade. A finished job whose
// resume fails on one waits for the operator: the resume give-up counts no
// failure of the node's (internal/sandbox's resumeGivenUp), so the run and its
// seat wait for as long as the condition lasts, and nothing ends it but a
// turn's deadline. So it is said: while one such run waits, the node holding
// the seat says so in its status record (internal/sandbox's
// Coordinator.ResumesBlocked), and every data node raises
// `executor_resume_blocked` from there. Every other kind, one that clears when
// a store, a log or a broker answers again included, is not an operator's.
//
// EVERY KIND THIS PACKAGE DECLARES, AND EVERY CLASS THE SANDBOX NAMES, IS
// NAMED in [InfrastructureKind] on one side, and
// TestEveryConditionKindIsTheNodesOrTheWorks holds every declared and every
// recorded kind to exactly one side, so a new kind or class lands on the side
// its author chose rather than on a default.
package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

var log = logging.Get("agent.builtin")

// LookupColleagueTool is the tool's wire name.
const LookupColleagueTool = toolname.LookupColleague

// lookupColleague resolves a free-text query to one colleague, or to the list
// it might be.
//
// NEVER A GUESS: addressing the wrong colleague pulls the wrong person into the
// work and leaves the right one unaware, so an ambiguous query returns the
// candidates, labelled as candidates.
//
// env resolves the chart's `${VAR}` contacts ([Deps.Env]).
type lookupColleague struct{ env org.EnvLookup }

var _ tools.SeatCallable = (*lookupColleague)(nil)

func (t *lookupColleague) Name() string { return LookupColleagueTool }

func (t *lookupColleague) Description() string {
	return "Look up a colleague — AI agent or human teammate — by any " +
		"identifier: handle, role name, Slack user ID, Jira/Confluence " +
		"account ID, GitHub or GitLab username. Returns the canonical " +
		"handle, the seat kind (agent | human), and every known " +
		"cross-platform identity; human results add what they own and how " +
		"to reach them (a mention and an asynchronous reply — never " +
		A2AAskTool + ", which addresses agents only). Matching is " +
		"case-insensitive and falls back to partial and fuzzy matching, so " +
		"'ceo' finds the role 'Agent CEO'; when more than one colleague " +
		"matches, the candidate list is returned instead of a guess. Use " +
		"this before " + A2AAskTool + " or before @mentioning anyone."
}

func (t *lookupColleague) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type": "string",
				"description": "Any colleague identifier: handle, role name " +
					"(or part of one), or an id on any connected platform",
			},
		},
		"required": []any{"query"},
	}
}

// Call without a turn cannot resolve anyone (the corpus is the turn's pinned
// org); it answers a refusal rather than an error, since the model's request
// was reasonable.
func (t *lookupColleague) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *lookupColleague) CallForTurn(_ context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	query := strings.TrimSpace(argString(args, "query"))
	if query == "" {
		return failed(LookupColleagueTool + " needs a `query`: a handle, a role name, or an id on any connected platform."), nil
	}
	if turn == nil || turn.Org == nil {
		return refusedBy(tools.RefusalUnavailable, waitless(),
			"No organization is in scope, so there is nobody to look up."), nil
	}

	seats := Corpus(turn.Org, t.env, turn.WithholdsContacts)
	found := colleague.Resolve(query, seats)
	safe := clip(query)

	switch {
	case len(found) == 0:
		log.Debug("lookup_colleague_no_match", "query", safe, "corpus", len(seats))
		// NOT FOUND: the colleague is what this call is about, unlike
		// a2a_ask's `target`, where it is an argument to correct.
		return refused(tools.RefusalNotFound, fmt.Sprintf(
			"No colleague matches %q. Known handles: %s.",
			safe, strings.Join(allHandles(seats), ", "))), nil

	case len(found) == 1:
		return tools.Result{Output: describe(found[0].Seat)}, nil
	}

	// The word "ambiguous", so the model retries with a handle rather than
	// picking the first row.
	var b strings.Builder
	fmt.Fprintf(&b, "%q is ambiguous — %d colleagues match. Call "+LookupColleagueTool+" "+
		"again with one of these handles:\n", safe, len(found))
	// EVERY candidate, uncapped: a handle the model cannot see is one it
	// cannot retry with. The org chart bounds the list.
	for _, c := range found {
		fmt.Fprintf(&b, "  - %s (%s, %s)", c.Seat.Handle, c.Seat.Name, c.Seat.Kind)
		if label := c.Method.Label(); label != "" {
			fmt.Fprintf(&b, " — %s", label)
		}
		b.WriteString("\n")
	}
	return failed(b.String()), nil
}

// Corpus is every addressable seat in an org: agents AND humans, since a
// lookup answers "who do I talk to".
//
// lookup resolves a person's `${VAR}` contact ids
// ([org.HumanContact.ResolvedIdentities]); a node passes the same chain its
// notification routing reads (secret store, then environment). Nil reads the
// process environment.
//
// withheld is the identity directory's reading the party registry was built
// from: a withheld human keeps the seat, marked [colleague.Seat.Withheld], but
// loses every external id, so an id from their accounts matches nobody, as an
// inbound message from them does. Nil withholds nothing.
func Corpus(o *org.Organization, lookup org.EnvLookup, withheld func(handle string) bool) []colleague.Seat {
	if o == nil {
		return nil
	}
	var out []colleague.Seat
	for role := range o.AllRoles() {
		seat := colleague.Seat{
			Handle: role.Handle(), Name: role.Name, Kind: string(role.Kind),
			External: map[string]string{},
		}
		if seat.Kind == "" {
			seat.Kind = string(org.KindAgent)
		}
		if role.IsHuman() && withheld != nil && withheld(seat.Handle) {
			seat.Withheld = true
			out = append(out, seat)
			continue
		}
		for _, id := range role.Contact.ResolvedIdentities(lookup) {
			// Every transport here is a place a message can be sent: this
			// map is both the exact-id index and what describe renders.
			// Keyed by transport, so an exact-id query matches whichever
			// platform the id came from.
			seat.External[string(id.Transport)] = id.ExternalID
		}
		out = append(out, seat)
	}
	return out
}

// describe renders one resolved colleague.
//
// A human's entry says how to reach them and that a2a_ask is not the way: an
// agent that tries it waits on a channel no person watches.
func describe(s colleague.Seat) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n  handle: %s\n  kind: %s\n", s.Name, s.Kind, s.Handle, s.Kind)
	if len(s.External) > 0 {
		for _, transport := range sortedKeys(s.External) {
			fmt.Fprintf(&b, "  %s: %s\n", transport, s.External[transport])
		}
	}
	switch {
	case s.Kind == string(org.KindHuman) && s.Withheld:
		// Not why: a person's standing is not the agent's business, only
		// that nothing it sends will arrive.
		b.WriteString("  reach them: they cannot be reached on any chat or " +
			"code-host account right now, so there is nobody to mention or " +
			"message. Work for this seat goes in the PM tool, assigned to " +
			"the seat. " + A2AAskTool + " addresses AGENTS only and will not reach them.\n")
	case s.Kind == string(org.KindHuman):
		b.WriteString("  reach them: mention them on a shared surface and " +
			"continue without waiting — a person answers asynchronously. " +
			A2AAskTool + " addresses AGENTS only and will not reach them.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// allHandles is every seat's handle, sorted.
//
// Uncapped: a model that missed reads it for the handle it meant, and
// "...and 7 more" withholds the answer.
func allHandles(seats []colleague.Seat) []string {
	out := make([]string, 0, len(seats))
	for _, s := range seats {
		out = append(out, s.Handle)
	}
	sortStrings(out)
	return out
}
