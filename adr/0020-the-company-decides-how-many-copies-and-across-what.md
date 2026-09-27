# ADR-0020 — The company decides how many copies of a file are kept, and across what

- **Status:** accepted
- **Authority:** `internal/objstore/upkeep`
- **Enforced-by:** `internal/objstore/upkeep.TestOnlyTheNewestCompanySetsTheCopies`, `internal/engine.TestTheMapTakesTheCompanyStampedWithItsActivation`, `internal/objstore/placement.TestCopiesAreSpreadAcrossFailureDomains` and `internal/objstore/placement.TestPlacementIsPinnedAcrossBuilds`, and end to end `internal/e2e.TestTheMapsCopiesAreTheCompanys`
- **Measured:** 256 fixed placement groups left the fullest of thirty data nodes holding about one and a half times its fair share, and of two hundred more than two and a half times. Unbalanced straw2 at three copies over ten members gives a weight-4 node about 2.8 times a weight-1 node's copies rather than four; the balanced shares hold a member within 2% of its weight wherever 2% is at least a copy and a half of its target — every fleet within that converged in at most 32 rounds, a median of five — which the group count's sizing gives every member of an equal-weight fleet and every member down to about three quarters of the mean weight, unless a failure domain is capped (twelve equal members in zones of one, one and ten at three copies target 51.2 copies each). Below it a copy count within 2% may not exist: nine members of weight 64 and one of weight 1 at three copies over 512 groups leave the light one about 13% off, and the balance stops at sixty rounds reporting it did not converge. Doubling the group count re-places 49.5% of the slots over ten members. A layout — every group's holders — takes 12 ms at 50 members over 2048 groups and 185 ms at 200 over 8192.
- **Cost-when-tried:** the map took its copy count from whichever node held its duty — that node's own `stream.replicas` — so a node left at that field's default of one set every group to one copy the moment the duty landed on it, and every other copy then read as placed elsewhere, which is exactly what the collectors delete.
- **Tag-status:** unreleased

## The decision

How many copies of every chunk the fleet keeps, and which node label those
copies are spread across, are the COMPANY's: `objects.replicas` and
`objects.failure_domain` in Tier B. The node holding the `object-map` duty
applies them from the company it runs, stamped with that company's activation
instant, and never applies an activation older than the one the map was last
set by — so a holder a revision behind cannot set the map back, and the duty
moving cannot move the answer. Nothing a node configures for itself decides
either value.

What a node DOES say about itself — the share it offers
(`store.objects.weight`), its own value of the domain label (`node.labels`),
and whether its store can hold chunks at all — rides a lease the object store
claims for itself, `objects:{node}` (`coord.ClassObjects`), rather than the
seat host's presence.

The map carries every input a placement needs — the copies, the label, each
member's domain and a balanced SHARE per member — so a placement stays a pure
function of a chunk's hash and one record, evaluated identically on every
node. A group's copies go to members with distinct values of the label while
the fleet has as many values as copies, and to the best remaining members when
it has fewer: fewer domains than copies is reported, never refused, because a
map that refused to place would lose writes to guard against a failure it can
no longer avoid.

This binds four packages that each hold a quarter of it — the Tier B field and
its validation in `internal/config`, the per-tick read of the company and the
objects lease in `internal/engine`, the maintainer that applies them in
`internal/objstore/upkeep`, and the draw that spreads by domain in
`internal/objstore/placement` — which is why it is a record rather than a
package doc.

## Why the obvious alternatives are wrong

**A node's own setting** — `stream.replicas`, or a `store.objects.replicas`
beside the weight — is a per-HOLDER value the moment the duty that reads it
moves, and the duty moves on a lease. Two nodes configured differently make
the copy count flip with every handover, each flip re-placing a share of the
company's files, and the smaller of the two is the one the collectors act on.
The broker's count is a different decision in any case: how many copies of
the LOG the cluster keeps, bounded by JetStream at five and by the cluster's
own members, where a file's copies are about data nodes and failure domains.

**Membership off presence** is what the map read first, and it cost twice. A
shutdown drain releases presence at its first step while the node is still
serving every chunk it holds, so a drain longer than the map's grace moved the
node's whole share to the others and back. And presence carries what the node
was configured with, so a node whose objects volume had failed stayed placed on
for ever.

**A float logarithm** is what [ADR-0019](0019-the-estate-names-an-object-and-a-map-places-its-bytes.md)
refused straw2 for, and the objection stands for the float: `math.Log` is
per-architecture assembly, and two nodes a last bit apart place one group on
different holders. It was never an objection to the formula. Straw2 with its
logarithm in integer fixed point — which is how Ceph computes `crush_ln`, for
the same reason — is exact on every CPU, takes one draw per member where "a
node of weight *w* draws *w* times" took *w*, and accepts a non-integer share,
which is what a balance produces.

**Fixed placement groups** could not serve every fleet: a node's share is a
count of groups, and a count of a few is mostly noise — the measurement above.
The group count grows instead, one bit at a time, with seeds chosen so a
split's lower half keeps its parent's holders exactly, so a doubling re-places
half the data rather than all of it.

**Weights used directly as draw weights** are proportional for the first copy
only: the second and third are drawn among whoever is left, so a large node's
later copies can only land on groups it does not already hold. The map carries
the shares a balance found instead, computed once by the duty holder and
stored, so no two nodes ever compare a floating-point result.

## What this does not decide

How many copies the BROKER keeps of its streams and buckets — that is
`stream.replicas`, a property of the log. Where anything but a file's bytes
lives: the estate is still whole on every data node. The two rules deletion
obeys, which are ADR-0019's. And which label is the right failure domain: the
engine cannot tell whether two hosts share a rack, so it spreads across the
label it is told to and says when it cannot.
