// Package executorkeep is what keeping an executor's work is said in, apart
// from the log that records it (ADR-0073): how a turn ENDED ([Ending]), the
// bounds a kept SESSION is held to ([SessionMaxBytes], [SealedMaxBytes]), the
// bounds of what a box names of its session — the harness, its release, its
// session id and the paths it was worked in ([HarnessMaxBytes],
// [HarnessVersionMaxBytes], [SessionIDMaxBytes], [PathMaxBytes]) — and of the
// turn it was taken in ([TurnIDMaxBytes]), and the shapes it is sealed and
// uploaded as ([SealedSpool], [SealedObject]).
//
// # Why it is not internal/executordomain's
//
// The executor log's domain (internal/executordomain) is the authority on its
// records: it validates them, publishes them and applies them, and so links the
// state log's whole graph. Its WRITER is the sandbox (internal/sandbox), which
// takes a session out of a box and decides how a lost or stopped run's turn
// ended — and the sandbox links no state log (its dependency gate), because
// everything that drives a box would then carry the log's graph and decide
// nothing more. The vocabulary the two must agree on byte for byte — the
// ending a run's record owes until the log takes it, the bound a take stops
// at, the sealed spool the engine's store hands back — is therefore here, in a
// leaf that links no state log, no configuration and no code host, and BOTH
// import it: one declaration, never a copy per side, because a bound written
// twice is one the two sides disagree about, and the side that lost would be a
// record the log refuses `record_too_large` after the turn already ended.
//
// What the sandbox asks of the log itself — a capture's record, an end's, and
// how each was answered — is said in the sandbox's own words
// (sandbox.SessionWrites), and the engine translates them to the log's.
package executorkeep
