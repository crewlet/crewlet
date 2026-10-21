# ADR-0031: Every state-log record is signed, and the framework verifies before a domain decodes

- **Status:** accepted
- **Authority:** `internal/statelog`
- **Enforced-by:** `internal/statelog.TestTheApplierIsHandedTheBodyAndTheTableTheFrame`, `internal/statelog.TestATamperedRecordIsRefusedPermanently`, `internal/engine.TestTheChangeFeedOpensTheFrameBeforeADomainDecodes`, `internal/estate.TestARequestTheFleetDidNotSignRunsNothing`, `internal/estate.TestARequestUnderAKeyThisNodeDoesNotHoldIsAskedElsewhere`, `internal/estate.TestAReplyTheRouterCannotBelieveIsNoAnswer`, `internal/estate.TestEnvelopeDomainsNeverVerifyAsEachOther`, `internal/signedask.TestARequestTheFleetDidNotSignIsBelievedInNothing`, `internal/signedask.TestAReplyIsBelievedOnlyWhenBoundToTheQuestion`, `internal/engine.TestARemovalTheFleetDidNotSignRemovesNothing`, `internal/engine.TestOnlyANoteTheFleetSignedReachesATurn`, `internal/engine.TestEveryInstructionChannelSignsInDomainsOfItsOwn`, `internal/engine.TestARelayedAnswerTheFleetDidNotSignNeverReachesASocket`, `internal/engine.TestARelayServerAnswersOnlyAnAskTheFleetSigned`, `internal/httprelay.TestAnOpenTheFleetDidNotSignRunsNothing`, `internal/assistant/live.TestAFrameTheFleetDidNotSealIsDropped`
- **Cost-when-tried:** both readers shipped handing the frame on, and neither failed as a signature bug. The applier put the framed bytes on `Record.Payload`, which every domain decodes, so every record came back `invalid character 'c' looking for beginning of value` from inside the apply transaction — the position never moved, health read the node as BEHIND rather than broken, and it shed its seats to a peer that would have done the same. The change feed read the same bytes and acknowledged each one under its own rule for a record it cannot decode, so every notification in the company stopped.
- **Tag-status:** unreleased

## The decision

The broker has no authentication of its own. It binds no listener on the
default topology, but a fleet clusters its embedded members over a port, and a
record on a state log is not a message — it is what every data node APPLIES.
Rows every data node derives identically, a seat's environment, a person's
grants.
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
and they are these:

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
  name;
- **the box bridge's relay**: a box's tool call lands on any node that serves
  the API, and that node relays it to the node holding the run's session
  ([ADR-0056](0056-a-box-reaches-its-run-through-any-node.md)), which runs it
  AS whoever the call's token names and writes the answer back as frames the
  first node hands to the box as the holder's. Both directions are
  instructions: an unsigned OPEN calls a running seat's tools as that seat,
  and an unsigned frame answers a box in its holder's place;
- **a person's assistant turn on another node**: a node following or stopping
  a Dashboard Assistant turn asks the node running it for the frames its
  client missed, or to stop it, and the running node carries either out for
  the person the request names, on the asking node's word that they asked
  ([ADR-0053](0053-an-assistant-conversation-belongs-to-its-owner.md)). So an
  unsigned request stops anybody's turn, and an unsigned answer ends one in
  its runner's place.

Each signs every request and every reply under the same keyring, in key
domains of its own (`estate`, `fleet.broker`, `seat.steer`, `mcpbridge.open`,
`mcpbridge.frame`, `assistant.replay` and `assistant.stop`, each split into a
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
`internal/signedask`, for ADR-0008's reason: every channel keeping its own copy
of the binding and the dispositions is how one of them would come to believe a
reply it never asked for.

The scatters that only READ — the fleet's history, a search slice, a held
seat's memory, a sandbox's live tail — are not instructions and are not signed
by this decision; what a party on the broker can do there is what it can do on
any subject it reaches, and keeping both ports private is the operator's half
(SECURITY.md). The exemption is for an answer that is READ: handed back to the
one caller that asked, as the answer to the question it put, and asked again
the next time it is wanted. A scatter whose answer a node DISPLAYS as another
node's content of its own accord — pushes, unasked, to every screen it holds as
what somebody just said, wrote or is doing, with no question of the viewer's
behind it and nothing but a later re-read to correct it — is not exempt,
however much it only reads, because a forged answer there is not a wrong reply
to one question but words shown under somebody's name to everybody watching;
the amendments below are the channels that rule reaches.

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

## Amendment: the live relay is sealed too

The live relay (internal/engine, `liverelay.go`) is a scatter that only reads:
a data node whose own copy is out of service asks a sound peer what that peer's
appliers committed, so its sockets hear the company while its own appliers are
not trusted. It shipped unsigned under the exemption above, and it is not one
of its readers: what a follower takes from an answer is pushed to every socket
on its node as the peer's appliers' work — a chat batch's excerpts and the
people it mentions, a datastore batch's documents whole — so anybody who could
reach the cluster or the leaf port could answer a follower before its server
did and show words in a room under anybody's name, or values in a shared
table, on every screen of that node until each read the rows again.

So the relay is a request-and-reply channel of `internal/signedask`, in key
domains of its own (`live.relay.request` and `live.relay.reply`): every ask is
signed and bound to an id drawn for it and a deadline, a server answers from
its ring only an ask the fleet signed and has not answered before, and a
follower believes only an answer signed for the ask it sent. A channel rather
than the one-way form presence uses, because the relay is an ask and one
answer to it, and an answer is worth believing only as the answer to THIS ask:
one way, a captured answer would be believed by any follower asking inside its
validity. An answer a follower cannot believe is no answer — passed over while
the follower goes on waiting for its server's own, since the transport names no
sender and a forged reply that ended the ask would let anybody move every
follower off every server — and a server that never sends a believable one is
a silent peer, which the relay already handles by asking another.

## Amendment: the assistant's frames are sealed too

A Dashboard Assistant turn streams to its person from the node running it, and
a person whose stream lands on another node follows the turn there: the running
node broadcasts each frame on the turn's own subject, and every node following
it streams what it hears to the person
([ADR-0053](0053-an-assistant-conversation-belongs-to-its-owner.md)). What a
node does with such a frame is show it to the person as their own turn, so a
forged one would put words in their conversation. Every frame is sealed one
way, as presence is, in a key domain of its own (`assistant.frame`) with a
short validity inside the seal, through the same `internal/signedask`
implementation, and a frame that does not open is dropped. It is encrypted
under the keyring's AEAD before it is sealed, with the turn as associated
data, because it carries what the person asked and what their reads returned,
and a seal hides nothing. The replay that fills a follower's gaps and the Stop
are instruction channels, listed above.
