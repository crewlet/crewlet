# ADR-0031: Every state-log record is signed, and the framework verifies before a domain decodes

- **Status:** accepted
- **Authority:** `internal/statelog`
- **Enforced-by:** `internal/statelog.TestTheApplierIsHandedTheBodyAndTheTableTheFrame`, `internal/statelog.TestATamperedRecordIsRefusedPermanently`, `internal/engine.TestTheChangeFeedOpensTheFrameBeforeADomainDecodes`, `internal/estate.TestARequestTheFleetDidNotSignRunsNothing`, `internal/estate.TestARequestUnderAKeyThisNodeDoesNotHoldIsAskedElsewhere`, `internal/estate.TestAReplyTheRouterCannotBelieveIsNoAnswer`, `internal/estate.TestEnvelopeDomainsNeverVerifyAsEachOther`, `internal/signedask.TestARequestTheFleetDidNotSignIsBelievedInNothing`, `internal/signedask.TestAReplyIsBelievedOnlyWhenBoundToTheQuestion`, `internal/engine.TestARemovalTheFleetDidNotSignRemovesNothing`, `internal/engine.TestOnlyANoteTheFleetSignedReachesATurn`, `internal/engine.TestEveryInstructionChannelSignsInDomainsOfItsOwn`, `internal/sandbox.TestAnAnswerOpensOnlyAsTheAnswerToTheQuestionItWasSealedFor`, `internal/sandbox.TestAnAnswerWhoseSealDoesNotOpenIsHandedToNoTurn`, `internal/sandbox.TestAnAnswerUnderAKeyThisNodeDoesNotHoldWaitsForANodeThatHoldsIt`, `internal/engine.TestAnUnsealedNoteOnARowReachesNoModel`, `internal/engine.TestANoteSealedForAnotherTurnIsNotDelivered`, `internal/engine.TestANoteUnderAnUnheldKeyWaitsAndNamesTheHeldKeys`, `internal/engine.TestAnAnchorThatDoesNotOpenDecidesNoAuthority`
- **Cost-when-tried:** both readers shipped handing the frame on, and neither failed as a signature bug. The applier put the framed bytes on `Record.Payload`, which every domain decodes, so every record came back `invalid character 'c' looking for beginning of value` from inside the apply transaction — the position never moved, health read the node as BEHIND rather than broken, and it shed its seats to a peer that would have done the same. The change feed read the same bytes and acknowledged each one under its own rule for a record it cannot decode, so every notification in the company stopped.
- **Tag-status:** unreleased

## The decision

The broker has no authentication of its own. It binds no listener on the
default topology, but a fleet clusters its embedded members over a port, and a
record on a state log is not a message — it is what the next node APPLIES.
Rows every peer derives identically, a seat's environment, a person's grants.
An unsigned record on such a log is an instruction anyone who can reach the
cluster port may write.

So the FRAMEWORK signs, on the way to the appender, and the FRAMEWORK verifies,
before any domain decodes. A domain never sees the frame and never sees a byte
this fleet did not write. The key is Tier A's keyring (ADR-0011), which is why
a node with no keyring refuses to start rather than running one log signed and
another not.

The rule is stated for READERS of a log, not for appliers, and that is the
clause the tree keeps needing: the applier is not the only one. The change feed
is a second consumer over the same stream, and anything that later reads those
bytes is a third. Each opens the frame first, and what travels onward is the
BODY.

That has a shape in the types, and it is where both failures landed.
`Record.Payload` is the BODY, because every applier decodes it; the framed form
is a private field the framework keeps, because a retained record is
re-verified by the build that can finally read it rather than trusted because
an earlier build once looked at it. A reader handed one where it wanted the
other does not fail as a signature problem — it fails as its own decoder
rejecting every record in the company.

Two failures, and only one is recoverable. A key this node does not hold is a
fact about THIS NODE and changes when an operator adds the key, so the record
is RETAINED and reprocessed — the same disposition a record from a newer build
gets, because both mean "this build cannot read this yet". A MAC that fails
under a key the node DOES hold is a fact about the RECORD, no operator action
makes it true, and the applier stops.

## Why the obvious alternative is wrong

The obvious alternative is each domain signing its own records, beside the
encoding it already owns. That is one implementation per domain of a rule
identical for all of them, in files nothing compares — and the domain that
forgot would be indistinguishable from the ones that did not, because an
unsigned record looks exactly like a record from a build predating the rule.
ADR-0008 is the general form; this is the case where the failure is silent
rather than merely duplicated.

The second alternative is a bool. It collapses the two failures, and they have
opposite dispositions: a caller handed `false` has to guess whether to retain
the record or stop the node, and either guess is a real incident — retaining a
forged record applies whatever was written to follow it, and stopping on an
unknown key takes the fleet down for the length of a rotation.

The third is deriving the key from the whole keyring rather than naming one in
the frame. Then every node that had loaded a different key SET would derive a
different key and refuse every peer's records for the length of a rolling
rotation, which is precisely when a fleet is least able to absorb it.

## What this does not decide

It does not make the log confidential. A frame authenticates; it does not
encrypt, and the records are readable by anything that can reach the stream.
Encryption at rest is the company document's, under the same keyring, and is
a separate decision.

It does not extend to the event queue's own envelope, which carries data rather
than instructions and round-trips unknown types by ADR-0006 — a custody batch
included, which carries a node without data's events to the data node that
keeps them (ADR-0025) and is an event envelope like any other — nor to the
coordination store, whose writes are guarded by a compare-and-set rather than
by a signature.

It DOES extend past the logs to every channel whose messages are
instructions — a request a node CARRIES OUT over the queue's ephemeral verbs —
and there are three:

- **the estate service**: a node without data reaches the estate through a node
  that holds it (ADR-0025), and that node runs a write AS the actor the request
  names, so an unsigned request is the same instruction anyone who can reach
  the cluster port may write, one hop before the record;
- **the fleet broker's membership**: a member removes a voter from the broker's
  metadata group through its own system account, the one action the design
  keeps off the account every anonymous connection is bound to — so an unsigned
  request changes the quorum every election runs on, past every check the
  operator's route makes on the asking node;
- **a note to a running turn**: the running node offers a person's note to the
  turn AS that person, on the asking node's decision that they may steer it —
  so an unsigned request puts words in front of a running agent under anybody's
  name.

Each signs every request and every reply under the same keyring, in key
domains of its own (`estate`, `fleet.broker`, `seat.steer`, each split into a
`.request` and a `.reply`), so a frame lifted off one channel — or off a log —
never verifies on another; each binds a request to an id and a deadline inside
the signature and a reply to the request it answers, because a signature says
the fleet wrote the bytes and not that it wrote them now or for this question;
and each judges what arrives by the same two failures with the dispositions a
channel allows: a key the receiving node does not hold answers that the request
DID NOT RUN, so the asker moves on to a node that holds it rather than counting
a refusal — or, where only one node can take it, as only the node running a
turn can take its note, tells the person the keyring is what to fix — and a MAC
that fails under a key it holds is refused and logged at error, because no
holder will ever accept it. The rule has ONE implementation,
`internal/signedask`, for ADR-0008's reason: three channels each keeping their
own copy of the binding and the dispositions is how one of them would come to
believe a reply it never asked for.

The scatters that only READ — the fleet's history, a search slice, a held
seat's memory, a sandbox's live tail — are not instructions and are not signed
by this decision; what a party on the broker can do there is what it can do on
any subject it reaches, and keeping both ports private is the operator's half
(SECURITY.md).

It does not decide what a reader does once verification fails — only that it
must not decode. The change feed acknowledges and derives no wake; the applier
retains or stops. Both follow from what each can do with a record it cannot
attribute, and neither is forced by this record.

## Amendment: presence frames are sealed too

Signing reaches one channel that is not a state log. An artifact presence frame
([ADR-0038](0038-presence-is-told-never-stored.md)) names a person, and its
reader acts on it by showing that person in a room, so a forged one is not
harmless the way a forged read answer is. Every presence frame a node
broadcasts is sealed one way under the fleet keyring, in a key domain of its
own, with a short validity inside the seal, through the same
`internal/signedask` implementation (which gains a one-way form beside request
and reply); a frame that does not open shows nobody.

## Amendment: an answer to a parked run is sealed on its row

The coordination store stays outside the rule as a store: its writes are the
coordinator's bookkeeping, decided between writers by compare-and-set. One
value it holds is not bookkeeping. A person's answer to a coding run parked on
a question waits on the run's row until a node resumes the run, and the resumed
turn hands it to the run's coding CLI as that person's words and names who gave
them — an instruction, the same as a note to a running turn. So the node that
records it seals it under the fleet keyring, in a key domain of its own
(`seat.run.answer`), bound to the run, the question it answers and the
deliveries it was made of, and the row carries the words and the answerer only
inside that seal. Every reader opens it before using anything of it, with the
two failures' dispositions: a key the node does not hold leaves the answer
recorded and owed until a node that holds it takes the seat, and a seal that
does not open as an answer to that question is never handed to a turn or
attributed to anybody — it is logged at error and let go of, and the run waits
on its question again. The detail is `internal/sandbox`'s (answerseal.go).

## Amendment: a note to a coding run is sealed on its record

A person's note to a turn whose coding run is out waits on the run's record in
the coordination store ([ADR-0066](0066-a-note-to-a-coding-run-waits-sealed-on-its-record.md)),
and is still an instruction: the run's CLI or the review reads it as that
person's words. So the node that files it seals it under the fleet keyring, in
a key domain of its own (`seat.steer.note`), bound to the turn and the note's
id, and the record names the seat a person's authority is decided on only
inside a seal of its own (`seat.steer.seat`). Every reader opens both before
using either, with the two failures' dispositions: a key the node does not hold
leaves the note waiting, with a warning naming the keys the node holds, and
refuses a decision on the anchor as unavailable; a seal that does not open, or
opens as another turn's or another note's, is never handed to a model, carried
or attributed, and is logged at error. The detail is `internal/engine`'s
(runsteer.go).
