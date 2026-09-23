# ADR-0020 — A seat binding names the seat by the handle it was created under

- **Status:** accepted
- **Authority:** `internal/iamdomain`
- **Enforced-by:** `internal/iamdomain.TestASeatIsBoundByTheHandleItWasCreatedUnder`, `internal/notify.TestABindingFollowsItsSeatAndNeverTheAddressItWasMadeWith`
- **Cost-when-tried:** keyed on the handle typed at the bind, a rename made one seat claimable a second time under its new handle (two subjects that never contend, so two people acted as one seat); a removal's tombstone under the handle the leaver held withheld the seat's next holder for ever, because the bind that should have ended its say stamped only tombstones naming the new handle; and a new seat created on the freed handle inherited a suspended stranger's withholding while the renamed seat routed its suspended holder's accounts.
- **Tag-status:** unreleased

## The decision

Everything the identity directory records about a person's seat names the seat
by its IDENTITY — the handle it was created under, `chart.Seat.Origin`, the
same anchor ADR-0019 derives the agent id from — and never by the address an
administrator typed. That is the claim subject (`iam.seat.<identity>`), the
`seat_id` column, the seat a removal's tombstone records and the stamp a later
bind leaves on it, and every reading of them: the seat holders and bindings the
directory answers, the fleet's scatter answer to a satellite, the notify
registry's standing, the request path's seat table and the dangling-binding
rule.

A bind accepts any address the seat answers to and resolves it through the
chart's own resolution, inside the decide's snapshot, to the identity it
claims. Every reader turns an identity back into a seat through the chart's
IDENTITY lookup (`chart.Reader.SeatByIdentity`, `org.Role.Origin`) and never by
comparing it with a handle.

It rests on the chart never issuing an identity twice: no rename and no
creation may take one, and a removal tombstones it — `internal/chart` holds
that half.

## Why the obvious alternative is wrong

The obvious binding is the handle — it is what the administrator typed, what
the dashboard shows and what every authority rule reads. But a handle is an
ADDRESS, and an address is what a rename moves and what a creation can re-issue
(`internal/chart`'s claimant-wins rule for a retired alias). Every reader of a
handle-keyed binding had to reconstruct which seat it meant from a rename
trail, and the three failures above are three readers reconstructing it
differently. Resolving "live handles first" merely chose which of them to get
wrong.

The derived agent id is the other candidate, and it is wrong for this estate:
it is computed with the company NAME, which lives in the settings document and
moves on a company rename — every binding in the directory would then name a
seat nothing derives any more — and it is not on the chart row, so reading it
inside an identity decide would be a third cross-domain read.

Rewriting a binding when the chart renames its seat is the third: it makes the
chart's applier a writer of identity rows, or the identity decide a reader of
chart records it did not arbitrate, and the two logs have no shared order to
decide the rewrite by.

## What this does not decide

It does not decide which address a person ACTS under: that is the seat's
current handle, read off the row the identity names, per request.

It does not decide what a handle is, which addresses resolve, or when a retired
alias may be taken by another object — those are `internal/chart`'s and
`internal/org`'s rules.

It does not order a bind against a removal. The two are on two logs, the
residue is a named legal state, and ADR-0020 changes only which seat that
residue names.
