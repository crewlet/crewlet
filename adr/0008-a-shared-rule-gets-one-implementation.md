# ADR-0008 — A rule more than one package needs gets exactly one implementation

- **Status:** accepted
- **Authority:** `internal/httpx`
- **Enforced-by:** `internal/httpx.TestNoClientSitsOnTheProcessGlobalPool`, `internal/queue/topics.TestNoPackageBuildsASubjectByHand`, `internal/schedule.TestOnlyOneClockReadsTheWallTime`, `internal/queue/jetstream.TestOnlyOnePlaceWritesARunningStreamsConfiguration`, `internal/agent/runner.TestTheReviewSchemaOffersExactlyWhatTheDecoderAccepts`
- **Measured:** twelve packages in this tree exist for no other reason. `internal/httpx` states the pattern outright: written down and checked by nobody, its rule had four prose copies and seven packages violating it.
- **Cost-when-tried:** every one of them is the forensic record. `internal/textcut` replaced four helpers that were four copies of one rule, already disagreeing on the easy half — two appended `…` and two appended `...` — while the hard half, that a plain `s[:n]` is invalid UTF-8 whenever a multi-byte rune straddles the cut, was got wrong by all four. `internal/whsec`'s rule, written twice, accepted a 16-byte key as a `${VAR}` reference and refused the identical key as a literal. `internal/jsprovision`'s five decisions were spelled separately, "each doc comment asserting it matched the other with nothing enforcing that it did". `internal/backoff`'s four copies of one doubling delay agreed on the answer and disagreed on how they reached it — a loop that exits at the ceiling, a shift by a count read off the wire, and two hand-spread intervals — which is the stage at which the fifth copy is the one that gets the overflow wrong. The review decision enum is the same shape a layer up and shows what the rule costs when only HALF of it is applied: the submission schema offered three decisions and the decoder validated three, and the decoder had already been moved onto the constants — which catches a rename, and leaves an addition reaching one list and not the other. A schema that offers a value the decoder refuses bounces a model that did exactly what the tool it was handed told it to do, and the model cannot see that the two lists differ.
- **Tag-status:** unreleased

## The decision

When more than one package needs one rule, the rule gets **one implementation
in one package**, and the other callers import it. Not a shared doc comment
asserting two copies agree; not a helper in each package with a comment
pointing at the other.

Twelve packages in this tree exist for exactly this reason and nothing else:
`textcut`, `whsec`, `jsprovision`, `httpx`, `api/httpjson`, `tokens`,
`procgroup`, `clientsource`, `runtoken`, `hostbox`, `solo`, `backoff`. Each of
their doc comments names the duplication as its reason for existing, and
several name what the divergence cost.

Where a second implementation genuinely cannot be removed — the dashboard is a
separate build in a separate language and cannot import a Go identifier — the
copies get a **gate** instead, which is what `internal/clientsource` is.

## Why the obvious alternative is wrong

The obvious alternative is a small private helper per package, which is cheaper
to write and reads as good encapsulation. It is wrong because the two copies
never diverge at the moment they are written; they diverge later, one commit at
a time, and the divergence is silent by construction — each package's tests
pass against its own copy.

The tell is in the doc comments of the copies themselves. Every duplicated rule
in this repository's history had at least one comment asserting that it matched
the other implementation. None of them had anything checking it, and each was
wrong by the time somebody looked.

`internal/textcut` is worth reading in full for the shape: the four copies
agreed that the rule existed, disagreed on the ellipsis character, and all four
got the byte-slicing wrong in a way that produced invalid UTF-8 a JSON encoder
substitutes, a model reads as a replacement character and a vendor rejects.

## What this does not decide

It does not say a shared rule must be a package. Two callers in one package
share a function; the threshold is a package boundary, because that is where a
private helper stops being visible and a second one starts being written.

It does not make every duplicated *value* a package. Where a constant has to
exist twice — the engine's and the dashboard's — the answer is the gate rather
than the extraction, and `internal/clientsource`'s doc argues why the check
belongs on the side that owns the value.
