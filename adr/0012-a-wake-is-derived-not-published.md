# ADR-0012 — A wake is derived from a durable record, never published by the writer

- **Status:** accepted
- **Authority:** `internal/changefeed`
- **Enforced-by:** `internal/changefeed.TestAnUnreachableClaimStorePublishesAnyway`
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
