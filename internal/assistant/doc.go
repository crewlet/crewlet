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
// No package under internal/assistant imports the store, any domain's writer
// or reader other than its own (internal/assistantdomain), the operator
// surface, the question registry, the tool registry, the builtins, the MCP
// bridge, the agent turn context (whose principal falls back to a seat's) or
// the credential owner (internal/api/auth). The import walk in this package's
// tests fails the build on any of them, directly or through any package
// between.
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
