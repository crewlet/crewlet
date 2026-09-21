# ADR-0018 — Every state-log record is signed, and the framework verifies before a domain decodes

- **Status:** accepted
- **Authority:** `internal/statelog`
- **Enforced-by:** `internal/statelog.TestATamperedRecordIsRefusedPermanently`, `internal/statelog.TestAKeyringThatCannotSignIsRefusedAtConstruction`, `internal/engine.TestTheChangeFeedOpensTheFrameBeforeADomainDecodes`
- **Cost-when-tried:** the change feed shipped as the second reader of a signed log without the frame, for one commit. Every record in the company came back `invalid character 'c' looking for beginning of value`, the feed's own rule acknowledged and skipped each one, and every notification stopped while the applier beside it verified and applied the same bytes.
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
Encryption at rest is the company document's, under the same keyring
(`crewlet config seal`), and is a separate decision.

It does not extend to the event queue's own envelope, which carries data rather
than instructions and round-trips unknown types by ADR-0006, nor to the
coordination store, whose writes are guarded by a compare-and-set rather than
by a signature.

It does not decide what a reader does once verification fails — only that it
must not decode. The change feed acknowledges and derives no wake; the applier
retains or stops. Both follow from what each can do with a record it cannot
attribute, and neither is forced by this record.
