# ADR-0142: The engine's own nodes, broker and object store are atlas entries read off each node's presence

- **Status:** accepted
- **Authority:** `internal/atlas/derive`
- **Enforced-by:** `internal/atlas/derive.TestTheFleetDrawsItsNodesItsBrokerAndItsObjectStore`, `internal/atlas/derive.TestANodesAddressIsAlwaysMarkedInternal`, `internal/atlas/derive.TestAnExternalBrokerAndAnS3BucketAreDrawnAsTheNodesNameThem`, `internal/atlas/derive.TestAFleetNobodyListedDrawsNothingAndASilentNodeIsStillDrawn`, `internal/atlas.TestANodesAddressIsShownToAnAdministratorAlone`, `internal/atlas.TestAFleetAddressNotMarkedPrivateIsRefused`, `internal/api/queries.TestANodesAddressIsAnsweredToAnAdministratorAlone`, `internal/engine.TestANodesSummaryOfItselfHoldsNoCredential`, `internal/engine.TestANodesAddressIsWhatItsFileNames`, `internal/coord.TestANodesInfraSummaryRoundTripsThroughItsPresenceRow`, `internal/coord.TestANodesInfraSummaryIsBounded`, `internal/queue/jetstream.TestAServerListsHostsCarryNoCredential`, `internal/advisor.TestTheEnginesOwnServiceIsNotAskedForAnOwner`
- **Cost-when-tried:** not tried. The atlas drew everything the company runs on except the thing drawing it: a company on three nodes, an external NATS cluster and an S3 bucket saw none of them on its map.
- **Tag-status:** unreleased

## The decision

**The engine is on its own map.** Four kinds of entry are computed on every read from the live nodes' presence rows, by the record source `fleet`, and stored nowhere ([ADR-0126](0126-the-atlas-computes-what-the-engine-knows-and-records-what-it-is-told.md)):

| Entry | Key | Relations |
|---|---|---|
| each live node, a server | `crewlet/<node id>` | none of its own |
| the event stream, a queue | `crewlet/broker` | `runs_on` each node whose broker is a member, or each server of an external cluster (a server keyed `nats/<host>`) |
| the object store, a bucket | `crewlet/objects` for the bucket in the fleet's own broker, `s3/<bucket>` with its region for S3 | none of its own |
| Crewlet itself, a service | `crewlet` | `runs_on` every node, `depends_on` the event stream and the object store |

No relation was added to the vocabulary: the service is what ties a bucket to the rest, and it is what a reader asks about ("what does the engine depend on?").

**Each node says what it runs on, on the presence row it already renews.** A node file is readable only on its own node, so each node publishes a summary (`coord.NodeInfra`, under `infra`) read once at boot: the host its peers reach it at, the hosts of an external broker's servers, the embedded cluster's name, and the object store's backend, bucket, region and endpoint host. Who has to agree on it is whichever node holds the lease, current value only, which is what a presence row is: it is written on every beat from memory, and it leaves with the node. How a node's broker takes part is not repeated in it; the row's profile already says.

**Hosts and names only.** Every URL of the node file is reduced to its host on the node, before anything is published: a NATS URL carries a token in either half of its userinfo, and an S3 endpoint can carry one too. The object store's keys, the keyring and the API tokens have no field in the summary. Each text is cut to 255 bytes and the servers to 16.

**A node's address is what its file names, never a guess.** What it advertises to the cluster (`stream.cluster.advertise`), else the interface its route listener binds (`stream.cluster.host`), else the one its API binds (`api.host`). An interface that means every one names no host, and a file that sets none says nothing: the node is drawn without an address.

**A node's address is an administrator's, and is always marked internal.** The addresses the fleet's records say (a node's, and an external broker's servers') are shown to a reader who holds `admin` and to nobody else: `atlas.For`, which every read of the atlas for a person goes through, leaves them out for every other holder, as the value shown and beside another source's, and an agent seat reads the atlas as no administrator. Each is claimed only through `atlas.FleetAddress`, which marks it private whatever network it is on, and the fold refuses a fleet address that is not marked so. Being private, it merges no two entries, so a node never becomes one entry with a server the company declares at the same address.

**The entries last as long as a live node says them.** No node, no claim. A node on a build older than the summary is still drawn, from its row's id and profile, without an address or an object store.

**Nobody is asked who owns the engine's own service.** The setup advisor's unowned finding ([ADR-0141](0141-the-advisor-lists-what-the-atlas-shows-needs-a-person-and-names-entries-only-to-a-holder.md)) skips a service only the engine's records name. Once a person says anything of it, it is asked like any other.

**A person cannot add these by hand.** A server, a queue or a bucket keyed `crewlet/` is named by the fleet (`atlas.Ref.EngineNamed`), as a server keyed `ssh/` is named by the settings.

## Why the obvious alternative is wrong

**Read the node file where the atlas is read.** Only the answering node's file is in reach, so a three-node fleet would draw one node, and a different one behind a load balancer on every refresh.

**A coordination record of its own for each node's summary.** It would need a sweep for the nodes that died and a second write beside the heartbeat. The presence row has an expiry already and is the one read every atlas read makes for health.

**Read the address off the machine.** A host name or the first interface is a guess: in a container it is the container's, behind a NAT it is unreachable. What the operator wrote for peers to dial is the engine's own record.

**Mark an address internal only where it is on a private network.** That is the atlas's reading for every other address, and it would publish a fleet's route address as an ordinary public one on a host with a public interface. Where the engine's own members reach each other is internal by what it is for.

**Show node addresses to every holder of the atlas.** A holder reads private addresses elsewhere in the atlas, but those are servers the company declared for its agents to operate. The fleet's own route addresses carry the company's whole event history between members and are of use to an operator alone.

**Leave the service out and add a relation from a bucket to a queue.** The vocabulary would grow a pair that reads oddly everywhere else (any bucket standing on any queue), to avoid drawing the one entry a reader starts from.

## What this does not decide

- **Health of a node on the map.** A node's posture and alarms stay on the fleet screens.
- **Backups.** Where the last verified backup is kept is not an entry.
- **Which screen shows the fleet.** Settings keeps its fleet screens; the atlas draws the same nodes as part of what the company runs on.
