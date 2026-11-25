# ADR-0161: A link carries one window, both ways, sized to a request's term

- **Status:** accepted
- **Authority:** `internal/jsapi`
- **Enforced-by:**
  - `internal/jsapi.TestALinksWindowHoldsBothDirectionsWithinARequestsTerm`
  - `internal/objstore/natsobj.TestARequestBehindAnUploadAndADownloadIsAnsweredInsideItsTerm`
  - `internal/engine.TestANodesTransfersShareOneWindowAcrossItsLink`
  - `internal/learning/memsync.TestAPublishAcrossALinkHoldsItsRowsInFlightToTheLinksWindow`
- **Measured:** the window is 139,810 bytes, four seconds at the floor of 30 s a MiB; one piece of an upload takes 132,096 bytes of it and one of a download 135,168, so one crosses at a time whichever way it goes; through a leaf across 100 ms at 100 Mbit/s a lone download ran at 1.04 MB/s against 2.3 with four pieces in hand, and a download and an upload at once at 0.55 each against 2.2 and 1.0 (`internal/objstore/natsobj`'s package doc, "What one window costs").
- **Cost-when-tried:** each direction was windowed alone (#1359): the object store's puts outbound and a memory replay's pulls inbound, while a download held four pieces in hand unwindowed. A request queued behind a put and answered behind a pull waited 7.75 s at the floor, and behind a download's hand 15 s more, past its 5 s term.
- **Tag-status:** unreleased

## The decision

A node that reaches its broker across a link (a leaf's link to the members, or a dial to an
external cluster) has **one window for that link** (`jsapi.Window`, of `LinkWindow` bytes), owned by its queue's connection
(`internal/queue/jetstream`'s `Queue.LinkWindow`) and handed by the engine to every bulk sender
on the link, whichever connection it rides: the object store's puts, downloads and walks
(`internal/objstore/natsobj`) and the memory sync's replay pulls and published rows
(`internal/learning/memsync`).

- **The window holds both directions together.** A take is what its transfer puts on the link,
  in bytes, given back when those bytes have crossed or never will: a put's piece or a memory
  row at its acknowledgement, a download's piece when it arrives, a walk's or a replay's pull at
  its end.
- **It is sized to a request's term**: what the floor carries in `RequestTerm` less one read
  term, never a lease's heartbeat and never a measured rate.
- **First come, first served.** A take larger than the window (a replay pull of one row at the
  transport's bound, a leader's read of a whole piece) is let out alone once the window is empty.
- **A waiter takes no stall of its own.** Every holder is bounded by its own, and its silence is
  the link's (ADR-0155).
- **A read across a link asks its own pulls**, one piece a pull for a download, since a client
  iterator that pulls on its own threshold cannot be admitted to a window. Each pull ends at its
  consumer's end (`jsapi.Pulls.AskToEnd`): one held there to its expiry held a walk's whole window
  past every ranged read and listing.
- **A member crosses no link** and has no link window: its puts share a window of their own,
  sized by throughput, and its reads keep the client's hand.

## Why the obvious alternative is wrong

**A window for each direction.** A request's bytes wait for what the node sent before it, and its
answer for what the link brings back before it. Two windows, each sized to the term alone, put a
request behind both past its term at the floor.

**A window sized to the link's measured rate.** It recovers a fast link's throughput, and after a
drop to the floor every request sits past its term while a window sized for the faster rate
drains.

**Every transfer windowed by its own package.** Each would size its window to the whole term,
and the sum of them is what a request waits behind.

## What this does not decide

A read that gives up while its pull is still open (its caller gone, its consumer lost) gives its
take back at once, though what the server already sent for the pull may still cross: at the
floor a request sent then can wait behind two pieces, past its term, once. Holding the take until
the pull's bytes are known to have crossed is not decided here.

What crosses a link unwindowed: an event delivered to a seat, a fleet record, an estate answer,
each whole up to the transport's bound. A request behind one of them waits for it, and a wait's
silence is judged by the link's movement (ADR-0155). Nor what a write across a link is given
beyond its term (`jsapi.WriteBound`).
