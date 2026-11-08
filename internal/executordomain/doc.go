// Package executordomain is the state log's record of each seat's EXECUTOR
// SESSIONS (ADR-0073): one row per (seat handle, turn) whose executor captured
// its harness's session, naming the sealed object in the fleet's object
// store that holds it (ADR-0026), or whose first launch recorded its read (a
// `begin`), on the `CREWLET_EXECUTOR_LOG` stream and applied into every data
// node's replicated estate.
//
// # Who has to agree on it
//
// A turn's relaunch may run on any node, in any box, and continues the session
// the turn's earlier launch left; and a conversation's HEAD — the session its
// next turn continues, on whichever node that turn runs — is read over the same
// rows. So the name of a
// session is the WHOLE FLEET's, identically, which is the replicated estate,
// which only this domain's applier writes. The bytes are not here at all. A
// session is tens of mebibytes, so it lives in the object store as one sealed
// object per capture under a key the store mints, and a row names it; the
// collector keeps every object a row names and deletes one no row names at its
// first pass once both the object's upload and its key's minting are more than
// a day old (ADR-0027). NOT a day after the row stops naming it, as the
// domain's first migration says: an object a day old that the last row stops
// naming is gone within the hour, which is why a session a live turn still
// means to continue is held by a row rather than trusted to linger (see "A
// turn's first read holds its base"). The rows are a REQUIRED reference
// ([SessionObjectReferences]): the collector's audit reports a session whose
// object is gone, and every backup carries the sessions the rows name, reading
// this log past its store copy through the applier's own decoder for the
// captures a restore replays. Not a volume per node (a box on another node, or
// on e2b, could not reach it, and the collector, the audit and the backup could
// not see it), not the memory changelog (a compacted subject per seat cannot
// carry a conversation's ordering rules), and not the coordination store (a
// value of mebibytes in a bucket every lease shares).
//
// # One row per turn, never per conversation
//
// A conversation's HEAD is, of its ended turns whose rows still name a session,
// the one that read the conversation last — what its next turn continues
// ([Head]) — but that is a READ over rows that each belong to one turn, never a
// conversation row a later turn's capture takes over: a row that took another
// turn's object would be an object carry, which ADR-0027 (as ADR-0036 amends
// it) allows only inside one artifact. So a turn's capture only ever writes its
// own row, and a conversation's earlier rows go when a later turn's `end`
// ABSORBS them.
//
// # Subjects, scopes and the five records
//
// Every record about a conversation is ARBITRATED ON THE CONVERSATION
// ([ConversationSubject]: the seat's handle and a digest of the conversation's
// key, or of the turn's own id for a turn with no conversation), and decided
// from one snapshot of it, so the rules over a whole conversation — its head,
// its absorption, its work-key dedupe — are decided in log order on one
// subject. Every record DECLARES the conversation as its scope under the
// seat's path, so a seat-wide record contains it and a node that defers one
// record defers every later record its rows could change ([statelog]'s
// deferral contract).
//
//   - `begin` records a turn's first read of its conversation — where it read
//     the head and the turns the read handed out — and the BASE that read
//     froze, which the turn's row then holds (see "A turn's first read holds
//     its base"). A turn's first read stands: a begin landing on a row that
//     holds one already writes nothing.
//   - `capture` names a new object for a live turn. It is refused at the
//     decide for an object minted longer than objstore.RecordWithin ago by the
//     deciding node's clock, for an ended or tombstoned turn, and for a
//     (launch, capture) ordinal pair not newer than the row's; the applier
//     repeats the deterministic guards. A seat keeps at most the BYTE BUDGET
//     the record carries (the writer reads it from the fleet's revision,
//     `turn_engine.conversation_session.max_bytes_per_seat`): past it the
//     applier releases the objects of the seat's oldest ENDED turns, never a
//     live one, keeping their endings, and a capture that cannot fit even
//     then is refused. Because a release may reach any conversation of the
//     seat, a capture's scope is the SEAT.
//   - `end` stamps the turn ended with the engine's account of how it ended
//     ([executorkeep.Ending]) and tombstones it. With no row (nothing was captured) it
//     writes an ended row naming no object, so the next turn is still told how
//     this one ended. With a row that LANDED A CAPTURE it ABSORBS exactly the
//     ended turns its first launch's read handed out ([Head.Absorbs], which
//     the turn's begin or its captures carry), and DEDUPES on a non-empty
//     work key, merging what the older attempt delivered; a turn whose row is
//     its begin's alone, or whose capture goes with this end, holds no session
//     that could stand in for what it read, and absorbs and dedupes nothing,
//     as a turn with no row does not.
//     Neither takes a row a live turn holds. The end is authoritative for the
//     turn's work key where its captures carried none. And where the turn's
//     last word was not kept — its newest capture NOT KEPT
//     ([executorkeep.Ending.Unkept]: refused, or never answered), or the turn
//     going on past what it kept in any other way, a stop or a loss whose box
//     was gone taking nothing more — the end says the turn WENT ON PAST the
//     session its row names ([executorkeep.Ending.WentOn], record version 4),
//     and the row KEEPS whatever it names: a prefix of what the turn did, told
//     as one, holds more than the conversation's previous head or a fresh
//     session would, and the row says it is not the turn's last word
//     ([Session.WentOn]), which the conversation's next turn is told. An end
//     stamped below version 4 is applied by the rule it was written under,
//     which takes that older session off the row and absorbs nothing: a
//     record's meaning is fixed by its stamp ([wentOnVersion]). Each session it
//     names as having run out of context goes too (see "A session that ran out
//     of context is retired").
//   - `delete` removes one turn's row and tombstone past the company's
//     retention (the duty passes the cutoff, and the apply compares the row's
//     own instant against it), or, on a seat's subject, the record of where a
//     removed seat's rows were removed, past the same retention.
//   - `seat_delete` removes every row of a seat the company removed,
//     tombstones its live turns, and records the position it applied at.
//
// # What a conversation still holds is what no ended turn was shown
//
// A turn's dispatch reads the conversation ([Reader.Head]): its HEAD (of the
// ended turns whose rows still name a session, the one that read the
// conversation last, which the turn's launches continue) and every OTHER ended
// turn the conversation still holds, which the turn is told of — and, of those
// that still name a session, the SIBLINGS. The turns that read handed out in
// full and the turn's brief told are carried on the turn's begin and its
// captures, and the turn's end absorbs exactly them. So every ended row a
// conversation holds is one no ended turn was shown, and the next read shows
// every one of them, whether it ended before the head or after it: a turn
// parked on a person's answer, beside which a later turn read the conversation
// and ended, ends after that turn, and is told and offered to the turn after
// both beside the head the later one left. A read bounded at [MaxEndings]
// leaves the oldest out, and they stay for the turn after it rather than being
// absorbed by an end that was never shown them.
//
// # The head is the turn that read the conversation last
//
// Of a conversation's ended turns whose rows still name a session, the HEAD is
// the one that read the conversation last, the newest end among turns that read
// it at one position, and a turn that read nothing (its read failed) after
// every turn that read ([Head]). Never simply the newest end: every turn's
// first launch continues the head its read named, so a conversation's sessions
// are a LINE, each continuing the one before, and a turn that read early and
// ended late — parked on a person's question while later turns ran, as a
// schedule's fires do, none of them ever its answer — continued none of the
// sessions those later turns left.
//
// THE SEAT BUDGET STILL RELEASES THE OLDEST END FIRST ([seatPlan]), so under
// budget pressure it can release a conversation's head before a STALE SIBLING,
// a turn that read the conversation earlier and ended later, and the next turn
// then continues, of the sessions the conversation still keeps, the one that
// read it last, and is told the released head's ending as it is told every
// other. The release does not follow the head's order because the applier
// decides it: in a rolling upgrade, a node releasing by end beside one
// releasing by read would release different sessions for one capture, and
// their copies of the estate would diverge. The head is a read's alone — two
// builds answering two heads hand two turns two bases, each recorded on the
// log as that turn's read — so its order moved with nothing to diverge.
//
// # A turn's first read holds its base
//
// A turn's launches continue the BASE its first read froze — the head it was
// handed — for as long as the turn has kept no session of its own: its first
// launch, and a first job lost with its box, before any capture, relaunched
// into that same base, on any node (internal/sandbox's restore.go). The first
// launch publishes the `begin` below as soon as the run's record that froze the
// read exists. Every release of an ended row could take
// the base from under that turn: another turn's end that was handed the same
// head absorbs it, an end of the same unit of work dedupes it, a capture of
// any conversation of the seat releases it past the budget, and the retention
// deletes it. The run record that knows the base lives in the coordination
// store, which the applier cannot read and a writer's decide reads racily, so
// the hold is put where every release is decided: on the log, as the turn's
// OWN ROW.
//
// So the `begin` writes the turn's row naming the base, and while that row is
// live and has landed no capture it HOLDS the base: an end's absorption and
// dedupe and the retention's delete each ask, of each row they are about to
// take, whether another live turn holds it, and the budget's release reads the
// seat's holds once before it walks the seat's ended sessions; each skips a
// held row, in log order. A begin and every end and delete of the
// conversation are arbitrated on one subject, so whichever lands first
// decides: an end first leaves the begin no base to hold, and a begin first
// is skipped by the end. A capture of another conversation is not arbitrated
// against the begin, but its apply reads the holds in log order, and its
// seat-wide scope contains the begin's, so a node that defers one defers the
// other.
//
// THE HOLD LASTS UNTIL THE HOLDER'S FIRST LANDED CAPTURE, OR ITS END. From its
// first capture on, a turn's launches continue its own session, which holds
// everything the base did: that very capture may release the base, which is
// what lets a seat at its budget's floor take a continued turn's first capture
// at all, where a hold until the end would have to fit a live session and its
// base together. A holder whose run is lost with no end recorded is ended
// `lost` within [LostAfter], so a hold outlives its holder by at most a day
// and a pass of the retention duty. A skipped row stays an ordinary ended row,
// which the holder's own end absorbs in its turn, as does the next end whose
// read showed it — at the cost, while the hold lasts, of a later read showing
// it a second time.
//
// The siblings a read offers are not held, and opening one never holds it: a
// hold on a session a run opens would be a write the model's read triggers,
// which ADR-0073 bars. A read of a sibling released meanwhile says so instead.
//
// # A session that ran out of context is retired
//
// Another launch of it fails the same way, so an end naming it takes it off every
// ended row of the conversation that names it, its own and held rows included, and
// [Session.RetiredBy] says so. An end whose own row loses its session absorbs
// nothing, so what it read is told again. ADR-0073's amendment has the reasoning.
//
// # A removed seat stays removed
//
// The mailbox retirement removes a removed seat's rows with one `seat_delete`,
// always published, which every node applies in log order. Its position is
// recorded, and every capture and end carries the removal its decide saw
// ([Capture.SeenRemoval]): one decided before a removal it did not see lands
// after it as nothing. That is what refuses the turn a tombstone cannot — one
// that had no row yet when its seat was removed — while a capture of the
// seat's next tenure, decided once its node applied the removal, lands.
//
// The eviction, the generation and the barrier are the framework's records.
// THE TOMBSTONE IS A GUARD, NOT AN APPLY GATE, and deliberately: every record
// it refuses is about the same turn, so within the conversation scope the
// tombstoning record declared, and a node that cannot decode that record
// defers every later one about the turn as well. Only a node's eviction stops
// an applier that cannot read it.
//
// # Sealed, bounded, and gone with the conversation
//
// A session's bytes are a harness's transcript: whatever the run read and
// wrote. So the object is sealed under the fleet keyring with the streaming
// format internal/secrets keeps for values too large to hold
// ([secrets.StreamSealer]), its associated data binding it to
// `executor/<seat handle>/<turn id>/<object key>` ([AAD]), so a capture
// streams box → seal → put and a restore streams get → open → box, each
// holding two chunk buffers rather than the session. What a restore hands out
// is provisional until it reads to the end ([SessionStore.Open]). An object
// that is missing, corrupt or sealed under a key the keyring no longer holds
// is [ErrUnreadable], which a restorer reads as no session. Every bound is
// this package's, but for the ones the sandbox that takes a session stops at
// too — a session's archive and its sealed object, and an ending's fields —
// which are internal/executorkeep's, the leaf both read because the sandbox
// links no state log; and the largest record they admit is held under the
// domain's declaration ([MaxRecordBytes]).
//
// # How long a row lives
//
// Until the company's conversation retention passes its own last instant
// (`turn_engine.conversation_session.retention_days`, read off the fleet's
// revision by the engine's retention duty, which never ages out a turn whose
// run is still live), or until the mailbox retirement removes its seat. The
// retention takes a row whether or not its turn ended — a turn with no end and
// no live run is one whose end was lost — and takes its tombstone with it,
// which is a departure from the rule that a delete leaves one: by the retention
// no record decided before the delete can still land ([removeTurn]). A turn
// whose end was lost holds its session against the seat's budget, or the base
// its begin named against every release but a retirement, until the retention
// duty ends it as lost, a day after its last record — its newest capture, or
// its begin — with no run of it live (internal/engine's executor source). A
// person's removal does not reach these rows in this release, as it did not
// reach the conversation ledger they replace (ADR-0073).
//
// # A seat searches its own rows by their words
//
// A seat's run finds an earlier session of its own by the words of its row —
// what the turn was asked, the conversation it served, and its ending's prose
// ([Reader.Search]) — in one read of the node that holds the rows, which
// answers its hits alone, so no row is shipped to be filtered and no object is
// opened. Every term is matched whole, as internal/textindex's analyzer makes
// it, against what the row's one redaction leaves ([Session.Redacted]), newest
// begun first, at most [SearchScanRows] rows a page; and a page says how many
// it examined and whether it reached the seat's oldest, so one that stopped
// short is never read as one that found nothing. A transcript is searched one
// session at a time, by a read of that session.
package executordomain
