// Package executorkeep is what keeping an executor's work is said in, apart
// from the log that records it (ADR-0073): how a turn ENDED ([Ending]), the
// bounds a kept SESSION is held to ([SessionMaxBytes], [SealedMaxBytes],
// [SessionKeptMaxBytes]), the bounds of what a box names of its session — the
// harness, its release, its session id and the paths it was worked in
// ([HarnessMaxBytes], [HarnessVersionMaxBytes], [SessionIDMaxBytes],
// [PathMaxBytes]) — and of the turn it was taken in ([TurnIDMaxBytes]), the
// shapes it is sealed and uploaded as ([SealedSpool], [SealedObject],
// [MediaSpool], [MediaObject]), and the frames that keep its media apart
// ([FrameText], [FrameMedia]).
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
// leaf that links no state log, no configuration and nothing of Gitlet, and BOTH
// import it: one declaration, never a copy per side, because a bound written
// twice is one the two sides disagree about, and the side that lost would be a
// record the log refuses `record_too_large` after the turn already ended.
//
// What the sandbox asks of the log itself — a capture's record, an end's, and
// how each was answered — is said in the sandbox's own words
// (sandbox.SessionWrites), and the engine translates them to the log's.
//
// # The views a launch continues a session by
//
// [TurnRow] (a kept turn's row) and [ConversationHead] (the head a turn's
// first launch freezes) live here for the same reason: the sandbox restores
// from them and links no state log. Each has ONE maker, executordomain's
// Session.Kept and Head.Kept, which the engine's restore and the read surface
// both call, so the fields a relaunch decides on are copied in one place and a
// reader is told what the launch would decide. [Excerpt] redacts before it cuts
// (a credential cut in half leaves a head no later redaction recognises), so
// every reader sees the same words. The log refuses a row past
// [HarnessVersionMaxBytes] or [SessionIDMaxBytes], and the sandbox holds a
// launch's record to them (sandbox.ValidRelease, sandbox.ValidSessionID).
//
// # A session not continued
//
// [Loss] is the one vocabulary for why a run did not continue the newest
// session its turn or conversation left, and [NotContinued] the whole verdict:
// the loss, what the run did instead, the turns either side and the engine's
// detail. HERE, NOT IN EITHER SIDE, because the sandbox stamps a capture's
// refusal with its loss, keeps a session a job spent with how ([SpentSession])
// and decides a launch's verdict, the executor log keeps all three on a turn's
// end ([Unkept.Loss], [Ending.SpentSessions], [Ending.NotContinued]) and a
// retired session's loss on every row it is retired from, and the engine's
// meter, its probe and every surface say them; a word spelled twice
// is one the screens and the objective disagree about. [Loss.Fault] splits
// what burns the session_carry objective (internal/objectives) from the five
// losses the engine chose, and [NotContinued.Sentence] is the one wording,
// which the dashboard copies under a gate (internal/api/queries).
//
// A NEWER BUILD'S LOSS IS A VALUE: held to its bound alike by the log's writer
// and its reader ([Loss.Within]), said as unnamed, counted a fault. A rolling
// upgrade mixes builds, and this one relays a loss a newer node stamped on a
// run's row, so refusing what it does not name would lose that turn's end. Only
// where this build originates a loss is it held to [Losses], by
// internal/sandbox's gate that every loss it says is a constant named here.
package executorkeep
