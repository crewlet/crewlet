# ADR-0012: A wake is derived from a durable record, never published by the writer

- **Status:** accepted
- **Authority:** `internal/changefeed`
- **Enforced-by:** `internal/changefeed.TestAnUnreachableClaimStorePublishesAnyway`, and for the amendment below `internal/changefeed.TestARecordAPeerCanReadIsHandedBackWithABackoffNeverAcked`, `internal/changefeed.TestARecordHandedBackForAnHourIsKeptWholeAndOnlyThenAcked`, `internal/changefeed.TestADeadLetterThatCannotBeKeptLeavesItsRecordHandedBack`, `internal/changefeed.TestAReplayDerivesAKeptRecordsWakeOnceAndDropsItsEntry`, `internal/engine.TestAFrameUnderAKeyThisNodeLacksIsDeferredToANodeThatHoldsIt`, `internal/engine.TestANodeThatStartsAbleToReadADeadLetterDerivesItsWakeAndDropsIt`, `internal/queue/jetstream.TestAFeedsDeadLetterIsKeptInTheDeadLetterStreamForTheEventRetention`, for the reads of an entry `internal/queue/jetstream.TestAListingMovesNoFrameAndLeavesNoConsumer`, `internal/queue/jetstream.TestAWakeFeedsLargestEntryIsKept`, `internal/changefeed.TestARuneAboveTheBMPIsSpelledAsItsSurrogatePair`, `internal/engine.TestABeatWhoseMarksDidNotMoveListsNothing`, `internal/engine.TestAKeepAndAnExpiryInOneBeatAreBothSeen`, `internal/engine.TestAnEntryThatGoesWhileNothingIsKeptLeavesTheReading` and `internal/engine.TestAListingThatDisagreesWithTheMarksIsListedAgain`, and for an operator's discard `internal/engine.TestADiscardDropsTheEntryItNamesAnswersItAndLogsAndCountsIt` and `internal/engine.TestADiscardIsAccountedSoNoNodeCountsItAsAWakeTheRetentionLost`
- **Cost-when-tried:** both failure modes were already in the tree when this was written. The Mattermost socket path SWALLOWS a failed publish — a wake lost with a logged line and no retry — and the webhook path is correct only because the vendor retries it, which is a property of somebody else's product rather than of this engine.
- **Tag-status:** unreleased

## The decision

When a committed change has to wake somebody — a seat, a notification, a
digest — the wake is derived by a consumer that OUTLIVES the writer, reading a
durable, never-compacted record. It is not published by the writing goroutine
as a courtesy after its transaction commits.

The mechanism is `internal/changefeed`: the record is committed, a fleet-wide
durable consumer over that estate delivers it, and whichever node takes the
delivery derives the wake. A node that dies between the commit and the wake
costs a redelivery, not a lost wake, because the unacked delivery is still
there.

Two properties follow and are part of the decision. It is a consumer GROUP
rather than a singleton duty, so a lease flap cannot stall a whole company's
notifications for stateless work. And there are TWO dedupe layers, each
covering the other's gap: a claim on the change id collapses a redelivered
message and **fails open**, because a coordination store that blinked must not
silently stop notifications; and a deterministic per-recipient wake id lets the
inbox and the completion ledger collapse whatever the open claim let through.

## Why the obvious alternative is wrong

The obvious alternative is to publish the wake where the change is written. It
is one line, it is in the frame that already knows everything the wake needs,
and it is what every path in this tree did before the feed existed.

It is wrong because the publish is a second, unprotected operation after a
transaction has already committed. The write succeeded; whether anybody hears
about it now depends on the process surviving the next few milliseconds and on
the broker being reachable in them. There is no transaction spanning the two —
there cannot be, since one is SQL and the other is a broker — so the only
honest options are to fail the write when the publish fails (which loses a
committed change to a transient broker blip) or to log and continue (which
loses the wake). The tree has one of each, which is what makes the argument
forensic rather than theoretical.

Making the durable record the retry removes the choice. Nothing needs to
succeed at the moment of the write except the write.

## What this does not decide

It does not say what a wake IS — that is the recipient's vocabulary, and a
seat's inbox, a digest and a notification differ. It does not say which estates
have feeds: `Source` names one as a string and an `Opener` opens a consumer
over it, precisely so a bucket family and a state machine's log can both be one
without either learning the other's shape.

It says nothing about the OTHER direction. A wake that a person triggers
directly — a chat message, a webhook delivery — arrives as an event on the
seat's own mailbox and is not a derived wake at all; what this record governs
is the wake that would otherwise be a courtesy call after a commit.

## Amendment — a node's alarm transition is the record its wake derives from

A wake whose record is itself a publish — a node's alarm transition, which no
commit precedes — is derived from the delivery that publish writes:
[ADR-0095](0095-a-nodes-alarm-transition-is-the-record-its-wake-derives-from.md).
The node's tracker hands each transition to an outbox that publishes it on the
inbound topic until the broker takes it, and the notify spine's fleet-wide
consumer group derives the wakes from the delivery, so the durable record is
the inbound stream's message and nothing trails a commit. With one publisher
there is no change to claim, so the deterministic per-recipient wake id —
derived from the transition's own id — is the layer that collapses a
republish; the decision above holds through it unchanged.

## Amendment — a record a node cannot read is deferred to a peer, and dead-lettered after an hour

The durable consumer is ONE GROUP for the fleet, so an acknowledgement is the
fleet's: a record one node acknowledges is a record no node sees again. The
feed acknowledged every record it could not read — a version or an envelope a
newer build writes, a frame sealed under a key the node did not hold — as
though not reading it were a decision, and so lost the wake a node on the newer
build, or one holding the key, would have derived (#838). `ActorApps` records
lost their wakes that way on every older node of a roll.

Such a record is now DEFERRED: handed back for redelivery, never acknowledged
bare, whether the opener met it (an unknown key, a newer envelope) or the
translator did (its future-version arm answers `changefeed.Defer`, never "no
wake"). It is handed back with a BACKOFF — two seconds, doubling with every
delivery, to the domain consumer's thirty-second ack window — so a moment with
only older nodes up does not spin the consumer. A record no build on the node
can translate is handed back the same way, still counted as `feed_unreadable`.

The broker caps no redelivery of a domain feed's consumer (`MaxDeliver: -1`, so
a budget never drops a wake silently), and a deferral cannot circle for ever:
pinned nats-server v2.15.2 keeps a delayed NAK in the consumer's pending set
(`server/consumer.go:3297-3313`), stops delivering new messages once that set
reaches `MaxAckPending` (`5019-5022`), and gives a consumer that sets none 1000
(`604`, `697-705`) — so a thousand records no node will read would stall every
new wake of the domain. The feed therefore bounds it: a record handed back for
an hour from its stored instant is DEAD-LETTERED — kept in `CREWLET_DLQ` on
`dlq.changefeed.<source>.<group>.<digest>` as a self-contained entry, its
sealed frame the entry's body so it needs its log for nothing, and what the
feed recorded of the record — where it was delivered from, why no node read
it — the entry's one header, `Crewlet-Dead-Letter`, a line of compact JSON in
ASCII (a rune past it escaped, one above U+FFFF as its surrogate pair) — and
only then acknowledged; a keep that fails hands the record back again. An hour
is a judgment call stated as one: an order of magnitude past a roll's minutes
(ADR-0109) and the keyring runbook's restart of every node. The entry is kept
for the dead-letter stream's age, `stream.event_retention_hours`, and then
expires, its wake with it, counted once across the fleet as
`crewlet.tracker.feed.dead_letter_expired`. The alarm `feed_dead_lettered` is
raised per log from the first entry, naming each entry's age and the time it
has left.

NOTHING BUT A REPLAY READS A FRAME. The alarm's reading runs on every beat of
every data node, and a stalled roll can dead-letter thousands of records, each
as large as its log admits, so it asks the dead-letter stream's leader one
question a beat — each feed subject's count of entries and the stream's last
sequence (`DeadLetterMarks`) — and lists a feed's entries, by their headers
alone, only when that answer moved: every feed subject that holds or held
entries when the last sequence moved, since a subscription's poison moves it
too and a keep beside an expiry on one subject leaves its count where it was,
and otherwise only a subject whose count moved, since nothing but a keep
moves the last sequence and every going lowers a count. The pinned server has
no headers-only read of one message, so a listing is a headers-only consumer,
which reads one replica and may be a moment behind the leader: a listing is
taken as current only once its counts are the leader's, and listed again on
the next beat until they are. An operator's listing and discard read headers
too; a replay reads each frame it acts on by its sequence, from the leader.

What clears an entry is a REPLAY, an operator's DISCARD, or its expiry. A
feed's start lists its domain's dead letters and opens each frame exactly as a
delivery is opened — verified, its envelope read, the translator asked — and
publishes what it can now read through the same claim a delivery passes, then
drops the entry; an entry it still cannot read is left for a node that can, and
a frame the fleet did not write is dropped. The start is the moment that
matters, because what makes an entry readable arrives with a restart: a newer
build, or a key, since a node's verifier is built once at boot.

An entry no build will ever read — a record sealed under a key retired from
every node, one no build translates and none will — would keep
`feed_dead_lettered` raised for the whole retention, hiding the next entry
behind a loss already accepted. So an operator who has decided its wake can be
lost DISCARDS it: `GET /feeds/dead-letters` lists every wake feed's entries by
the broker's sequence, never their frames, and
`POST /feeds/dead-letters/{id}/discard?confirm={id}` (`crewlet feeds discard`),
admin's at a recent proof, drops the entry from whichever feed's subject holds
it — never one on another `dlq.` subject, a subscription's poison — through
the same ledger a replay's drop passes. That ledger takes the claim the expiry
count takes, so no node counts a discarded entry as one the retention lost; the
discard is logged naming who pressed it (`changefeed_dead_letter_discarded`)
and counted as `crewlet.tracker.feed.dead_letter_discarded`, and one a replay,
the expiry or another discard accounted for first removed nothing and says so.

A TAMPERED frame — a MAC that fails under a key the node holds — is still
acknowledged and skipped: no node should act on a frame this fleet did not
write. ADR-0031's sentence on the feed is corrected to say so.
