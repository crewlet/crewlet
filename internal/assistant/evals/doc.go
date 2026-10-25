// Package evals runs the Dashboard Assistant without a node: a real
// [assistant.Service] over in-memory stand-ins for every seam it is given,
// driven by a scripted provider or by a real model, and the evaluation cases
// `crewlet assistant eval` reads and judges.
//
// # One harness, shipped
//
// It is PRODUCTION code, linked into the binary, and it never imports
// `testing` or net/http/httptest (which does). The engine's suites drive the
// same assembly through internal/assistant/assistanttest, a test-only package
// that wraps [Assemble] with a *testing.T, so there is exactly one scripted
// provider ([Model]) and one rig for every suite and every evaluation case.
// cmd/crewlet's TestTheBinaryLinksNoTestScaffolding holds the binary to that.
//
// # What it holds
//
//   - [Model] and [Round]: the scripted provider, one completion (prose, tool
//     calls, streamed deltas or an error) per round, recording every request.
//   - [Store]: the assistant's own domain in memory, both sides, keyed on the
//     owner as the real one is.
//   - [Dispatcher], [Revoker], [Grant], [Models], [Budget], [Audit], [Metrics]
//     and [Catalogue]: the seams' stand-ins; a rig takes the App's real
//     dispatcher and delegations in their place for internal/api's suites.
//   - [Rig] and [Assemble]: the service over all of them, a person and a
//     conversation of theirs.
//   - [Case], [LoadSuite] and [RunSuite]: the evaluation cases.
//
// # What an evaluation measures
//
// A case describes the company it is asked in as the answers its requests
// receive, and runs through the real orchestrator and the curated catalogue,
// so it measures what a model does with the tools it is offered: what it
// calls, what it proposes and what it says. It never measures the API's
// authority, which the deterministic suites in internal/api hold on every
// change. The case format is documented for operators in
// docs/reference/cli.md ("crewlet assistant eval").
package evals
