// Package assistant is the Dashboard Assistant's orchestrator: a person's
// question, answered by a model through the engine's own tool loop, with every
// read made as that person and every change prepared for them to approve.
//
// # It acts as the person and is nobody of its own (ADR-0050)
//
// The assistant has no identity in product data. It is not a seat: it holds
// no lease, has no roster entry, writes no diary and is charged to no work
// item, so none of the agent runtime's turn machinery is used (internal/agent's
// turn, runner, ledgers, prefetch and turn context). It is a new caller of
// internal/agent/toolloop with a Surface of its own, over the curated
// catalogue (internal/assistant/catalog), whose every call is an ordinary
// request through the API's one handler chain (internal/assistant/exec),
// carrying a grant derived from the person's live session that the guard
// re-composes into that person, from their identity rows, on every request.
//
// So no component on this path holds a credential that opens product data.
// What this package holds is an exec.Grant for at most one turn, which it can
// name and time but never present: internal/api/auth mints it, keeps its
// token and is the only sender. A parked turn holds none; the approval that
// resumes it derives a new one from the approving request.
//
// # What it never imports, and the walk that holds it
//
// No package under internal/assistant imports the store, the state-log
// framework, the fleet's secrets, any domain's writer or reader other than its
// own (internal/assistantdomain), the operator surface, the question registry,
// the tool registry, the builtins, the MCP bridge (which acts as a seat), the
// agent turn context (whose principal falls back to a seat's), the credential
// owner (internal/api/auth) or the engine. The import walk in this package's
// tests fails the build on any of them, directly or through any package
// between.
//
// # Starting a turn
//
// [Service] starts a turn only once the person may use the assistant
// (iam.Assistable, the predicate the guard derives grants by), their load
// allows another (two running, twenty a minute), the company's windows have
// room (toolloop.Refusal on the turn's meter, before any round) and the model
// chain resolves. The same key is the same turn ([TurnID], derived from the
// request's scoped Idempotency-Key), and a grant no runner takes is revoked on
// the way out. A failure of its records, counter or authority table is that
// store's own error, which the HTTP surface tells as a condition or a fault; a
// write of unknown outcome is returned as one. [Unavailable] is kept for its
// own reasons (no chain it can build, a node shutting down, a runner it cannot
// reach) and carries no wait.
//
// # The prompt and the surface
//
// The prompt is built in cache order (prompt.go): rules stamped
// [PromptVersion], provenance.StandingInstruction, the company clock, the
// person and the writes they may propose, the last twelve messages with cards
// as one-line summaries, then the question after the page context, fenced as
// page_context. The surface refuses an identity argument or one its schema does
// not declare before anything is sent, holds every request to a route its tool
// declares, sends only through the dispatcher on the turn's read grant,
// redacts an answer before it is carded, and fences and cuts what the model
// reads to 16 KiB. A write offers itself only where a Proposer is wired, and a
// 401 on the turn's own grant ends the turn grant_ended.
//
// # Segments, resume and stop
//
// A segment ends parked, completed, stopped, failed or out of budget, recorded
// through the consumer-defined Records (the engine adapts the domain's writer,
// deriving every operation from the gesture's key), streamed through live and
// audited as assistant_turn_completed. The package is the approval path's
// approval.Turns: Resume takes a parked loop back up on the deciding request's
// own grant, answering the suspended call with what became of the proposal
// (the engine's words, the route's answer fenced as an API answer, a page's
// report fenced as the page's). Each taints the resumed segment as a step's
// read does, beside the taint the park kept, and the proposal card is updated
// in place by its first card id. A screen read (catalog.ReadScreenTool) parks
// too: its action is sent only once the park lands, and the page's report,
// carrying the screen as it is now, resumes the turn with no card. Emit
// streams a page action to a turn that stays parked. A Stop of a parked turn declines what it waits on through
// approval.DeclineTurn, bound with BindDecliner because the approver is built
// over the service, under the Stop's own key; Stop decides from the turn's
// record before a local runner, since a runner that just parked is still
// tearing down. Render is the compliance read's rendering, the owner's own
// rule.
//
// The answer streams: every round is asked to stream, settled lines are drawn
// as written, and each change is one llm.Reconcile of the drawn text against
// what the drawer should hold.
//
// # Across a fleet
//
// fleet.go is the running node's half of live's channels (ReplayHere,
// StopHere, each refusing a person the turn is not for); FleetRemote hands it
// the fleet half, answering another person's turn as not found. A stream of a
// turn whose runner is gone closes it interrupted under an operation every node
// derives alike, so of two nodes noticing one records it. A person's turn
// starts are counted on the fleet's counter (TurnRate) beside the node's own
// rate, which holds them while the counter cannot be read. A segment is
// numbered after the newest seq the fleet holds and never below the parked
// record's, and a stream is answered by the node holding the frames its client
// has not drawn. Frames a log no longer holds are answered by a snapshot read
// from that one turn's records, one frame cut to wire.FrameMaxBytes
// (snapshot.go) like every other, so a peer's replay page always fits the
// broker.
//
// # A turn's end is settled
//
// settle.go retries the runner's close under the same operation until it lands,
// the turn is found already ended or its conversation is gone, within the frame
// log's retention. An interrupted close that did not land is the stream's
// error, never an interrupted the record does not say. Everything a resume can
// fail on is read before its resume record lands.
//
// # One model, the API as its read surface, no deep work (ADR-0163)
//
// Ask Crewlet runs on the one entry `assistant.model` names, under bounds that
// are constants, with the harness's own web and local tools turned off when
// the engine builds the run, whatever the entry says. It reads through the
// generic read of the API's own routes, hands a task too large for a turn to
// an agent as a work item, and reads the web only through the engine, at
// addresses the conversation was given. ADR-0163 supersedes the deep work and
// listed hosts described below (ADR-0054, ADR-0151).
//
// # A write is a proposal
//
// A write tool's call is never sent by the turn. It becomes a proposal
// rendered from a server-side preview and bound to its digest and the object's
// revision (internal/assistant/proposal), the loop suspends with the call
// unanswered, and only the person's approval, made from their own browser,
// sends it, once (ADR-0051).
//
// # One model, and no tool of its own
//
// A turn runs on the one model the company assigns (`assistant.model`), as a
// helper's run whose harness keeps no tool of its own, and hands no task to a
// harness running tools of its own (ADR-0054's deep work is gone): a task that
// needs a shell, files or many hours becomes a task proposed for one of the
// company's agent seats. The images a question carries are handed to the model beside
// its prompt, where its harness takes them.
//
// # The web, only through the engine (ADR-0151)
//
// A turn reads the web only through the engine's own reads, from hosts the
// company lists; the harness's own web tools stay denied.
package assistant
