// Package partmap is the ESTATE MAP: which data nodes hold each partition of
// the replicated estate, the lease a data node says what it holds on, and the
// pure rules the map's maintainer moves the holders by.
//
// Under a partitioned layout the estate is no longer whole on every data node.
// It is divided into partitions (internal/statelog's vocabulary: a space, an
// index, one log per domain), and each partition is held by as many of them as
// the company asks for. Which ones is a question the whole company has to
// answer the same way now — every router in the fleet routes a partition's
// reads and writes to its servers, and a data node joins or leaves a partition
// because the map says so — so the map is ONE coordination record
// (coord.EstateMaps, ADR-0003), changed only by compare-and-set and watched by
// every node. Under layout 0 there is no map: every data node holds the one
// partition whole, and a map at layout 0 is refused.
//
// Two halves share the record, each with its own reader. The TARGET is where
// each partition should be — a pure function of the members, their shares and
// the operator's moves, drawn by internal/placement under the estate map's own
// salt. The HOLDER TABLE is where each partition is — who is joining it,
// serving it or leaving it, and since which epoch — which is what routers
// route by. The maintainer moves the second toward the first, one
// make-before-break step at a time.
//
// # Who is in the map is internal/membership's
//
// The lifecycle of a member — absence counted in the maintainer's own ticks,
// never timed on its clock, removal after forty of them and never onto
// nothing, probation for a node seen back, an operator's out and hold, the
// company's copies stamped with the activation they came from — is the object
// map's, written once in internal/membership and run here unchanged. Its
// input is the live `estate:` leases (coord.ClassEstate), never presence: the
// lease is released LAST in a drain, after the node has stopped serving every
// partition, so a draining node never reads as one that has gone.
//
// # A lease that does not say it is healthy is not healthy
//
// A lease's health is read three-valued — healthy, failed, or not said — and a
// node that does not say its store is healthy is counted exactly as a failed
// one: absent for that tick, and removed if it goes on not saying for the
// grace. A node counted absent is placed on nothing new — no join is named on
// it — its word vouches for no copy's retirement, and nothing it says makes it
// a holder or a server: no copy it reports is adopted, no join of its is
// promoted and no leave of its is taken back, whatever its lease says of a
// partition, since an absent node has no lease to say it with. What it holds
// already stays as it is until it is able again or membership removes it, and
// its word that it is GIVING a copy up is still taken — the one direction in
// which the word of a store nobody may trust costs nothing. The object
// store reads its own lease the other way round, an unsaid health as one that
// has not failed, and that is right THERE: it had members before it had a
// health report, and the reading kept them placed on. The estate lease was
// born with the field, so a lease without it is a writer's bug or a build that
// renamed it, and placing partitions on a node that has not said it can hold
// them trades a stalled join for a guess. A node that runs another layout than
// the map's is counted the same way: its partitions are another layout's, and
// every name it reports means something else here.
//
// # One draw over every partition
//
// The map draws every partition of every space from ONE draw — one share per
// member — rather than one draw per space. The layout sizes every partition
// log alike: the pages space has a quarter of the tracker's partitions
// because its log grows at a quarter of the rate (engine.DefaultPagesPartitions),
// so a partition is one unit of load whatever its space, and a member's share
// of partitions is its share of the estate. One draw also gives the balance
// every partition as a group — 321 at the default layout — where a draw per
// space would split them 256, 64 and 1 and leave the smaller spaces too
// coarse to balance at all. No member carries one space at the expense of
// another by construction: a partition's ranking is its own seed's, and
// nothing in it knows which space the seed came from —
// TestOneDrawDealsEverySpaceAlike holds that.
//
// # Balanced, to what the partition count can promise
//
// Straw2 is proportional for a partition's first copy only, so a member of
// twice the weight holds less than twice the partitions unless the shares are
// balanced — the object map's lesson, and the reason a weight means what it
// says here too. But a balance promises its tolerance only where it spans a
// copy and a half of every member's target, and the estate map can never add
// partitions the way the object map adds groups: at the default layout a fleet
// past about a dozen data nodes has targets too small for two percent. So a
// balance here asks for the finest tolerance the count promises for the fleet
// it balances (placement.Draw.Reachable) and converges to it, where asked for
// two percent it would run every round on every change and move partitions in
// search of an evenness no copy count can meet. It balances when what the map
// places changed — its members, their weights, the copies, the label — and at
// no other time; Converged false is a measurement, and nothing alarms on it.
//
// # The epoch counts the holder table
//
// [Map.Epoch] moves when a holder is added, changes state or is removed, and
// at no other time: a change of members, shares, copies or moves moves the
// TARGET, and the epoch follows only when the holders converge on it. That is
// the one meaning every comparison of an epoch is written against. A server
// asked for a partition it does not serve answers with its map epoch, and an
// asker with an older one knows its routing is stale — routing reads the
// holder table and nothing else. A holder's [Holder.Since] is the epoch at
// which it entered its state, and a node's lease names the epoch it last
// ACTED on, so "has this node read the map that made it a server" is one
// comparison, within one [Map.Generation].
//
// # Converging the holders, make before break
//
// Each tick, for every partition, in this order ([Next]):
//
//   - A holder whose node membership removed is dropped. Its register row
//     still pins the partition's logs until an operator evicts it.
//   - A serving holder whose lease says it is draining or released is leaving
//     on its own, and the map says so: a drain is never reversed — the last
//     server's included, since the map never RETIRES the last server but
//     cannot keep routing to one that has stopped serving. So is a
//     joiner that says so having read the map that named it — it gave the
//     join up — but never one that has not: that is an earlier tenure's
//     word, and a rejoin read as a leave would never finish.
//   - A leaving holder whose lease says released, or lists what it holds
//     without the partition, at an epoch at least its Since, is removed —
//     the release is this leave's, not an older tenure's, and a node told to
//     leave a partition it never adopted has nothing to release.
//   - A joining holder whose lease says serving, at an epoch at least its
//     Since, is promoted: the node has itself established its copy, and has
//     read the map that named it. This and the two rules after it take the
//     word of a node the tick counts present and healthy, and no other.
//   - A node whose lease reports the partition and which the map does not list
//     is ADOPTED — after the cutover, a restore, or a node that came back
//     with its files. An established copy is added serving, wanted or not:
//     it restores a copy with no transfer, and one the target does not want
//     is then let go like any other server, under the two conditions below,
//     never at once. A copy still being built is added joining where the
//     target wants it and leaving where it does not, so it releases through
//     the leave and the release fences anything it might still publish. A
//     node never deletes a partition file on its own, so the maintainer and
//     a node cannot reach opposite conclusions about one.
//   - A leaving holder the target wants again, and whose lease still says it
//     serves, is serving again — the cheapest copy there is. So is one the
//     target does not want, while NOBODY ELSE SERVES the partition, whose
//     lease still says it serves having read the map that made it a leaver:
//     the last server of a partition whose drain a restart cut short, its
//     own check of the leave refusing it. It is the partition's only copy,
//     and left leaving it would be one routers have nowhere to send the
//     partition to and a joiner no donor to fetch from; serving, it is let
//     go like any server, once the target serves.
//   - A joiner the target moved away from before it served is withdrawn at
//     once: it served nothing, and left to finish, one stuck with no donor
//     would hold its node's one join for ever.
//   - A serving holder the target no longer names is retired — marked
//     leaving — only under BOTH of ADR-0019's conditions, adapted to a map
//     with one writer: (a) every node of the partition's target is serving
//     in the map AND says so on its own lease, which the tick counts present
//     and healthy, and (b) each of those leases names a map epoch at least
//     the one that made it a server, so none of them is, on an older map it
//     has not replaced, about to leave itself. Without (b) two copies can
//     each vouch for the other on different maps, and both are dropped. An
//     empty target vouches for nothing, and the last server is never
//     retired.
//   - Every target node not yet holding the partition joins it, once the
//     tick counts it present and healthy: a join named on a node that is
//     away is one it cannot start, and the trim counts a joiner's tail from
//     nothing. A join into a partition somebody serves is a snapshot
//     transfer, and a node takes at most [MaxJoinsPerNode] of those at once
//     across the whole map; a join into one nobody serves — every partition
//     of a new deployment — has no donor to wait for and is not rationed.
//
// # Pure, and unknown is the caller's
//
// [Next] and the gestures are pure functions of values, so every rule is
// tested without a store. What they cannot be handed is an UNKNOWN: a map that
// could not be read, a lease listing that failed, a company that could not be
// resolved. Whatever runs [Next] changes nothing on a tick whose inputs it could
// not read (ADR-0005), and so never releases or removes on one.
//
// # The maintainer, and what it writes under layout 0
//
// [Maintainer] is what runs [Next]: once a tick, while its node holds the
// map's duty (`worker:estate-map`), reading the live estate leases, the stored
// map and the company, and writing the answer back by compare-and-set — the
// object map's maintainer over this record. Before it writes a FIRST map it
// has the layout's logs created ([Provisioner]), because a node named to join
// a partition opens its logs, and a map naming holders of logs nobody created
// would name joins that cannot start.
//
// Under layout 0 — the single-file layout, and the only one this build runs —
// it writes NOTHING: there is no map, [Next] creates none for layout 0, and a
// tick reads its three inputs and leaves the store as it found it. The duty's
// own lease is the only record its holder keeps.
//
// # The view
//
// [View] is what every other reader holds: the map WATCHED — a change reaches
// a node when it lands, since routers route by it and a node joins a
// partition because it names it — and confirmed by a read every [ViewConfirm],
// beside a watched listing of the estate leases (coord.LeaseView). It answers
// which layout the fleet runs and who serves a partition, three-valued, and
// under layout 0 it answers that the one partition is served by every live
// estate lease that serves it. Routing may use it at any age; anything that
// decides asks [View.Fresh], which is false once either half was last
// confirmed more than statelog.FloorCacheStale ago.
package partmap
