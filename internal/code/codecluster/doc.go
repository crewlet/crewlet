// Package codecluster is THE CODE HOST ON THREE DATA NODES, routing to one
// another through the production estate router — the topology every claim
// about a committer on another node needs, and the one place they are made
// without an engine: a push decided by the repository's committer whichever
// member received it, a committer that dies after appending, an evicted
// writer, a committer that stops, the crash matrix on a fleet
// (codetest.RunFleet), on the broker's object store and on S3.
//
// It has no exported surface and no code of its own; its tests are the whole
// of it.
//
// # The topology is the engine's, three times over
//
// Each member is a member of one three-member broker cluster
// (jetstreamtest.StartCluster, every stream at three replicas, so a record one
// member acknowledged is one every member will see), a data node composed as
// the engine composes the code domain (codetest.StartDataNode) and a git node
// over it (codetest.StartGitNode) — the same two blocks the one-data-node
// topology is built from, so the two topologies certify one composition. What
// is between the members is the production estate.Router and estate.Serve:
// every git node's host and cache reach the estate through its own member's
// router, which answers in-process where its member's copy serves and asks a
// peer otherwise, and asks a push of its repository's committer — the first
// of rendezvous.Order over the running members, which is also what the
// suite's Committer answers. The placement is the members whose presence is
// live: a killed member stays listed — a process that dies gives nothing back
// — so its peers' routers meet it, hear nothing and report it, while a
// stopped member gives its presence back first, as the engine's drain does.
// How the engine's watched presence view learns of either is the engine's to
// test.
//
// # The commit chain, measured
//
// BenchmarkCommitChain measures one repository's commit chain here, where
// the router hop is the production one, and beside it on one member of a
// broker of its own: every push a ref created with no pack — but in its mix
// arms (below), whose packs are named and never stored — so the figure is the
// chain and never the object store. On four vCPUs of a 2.1 GHz Xeon with
// every write fsynced (three counts, internal/code's "What a push costs,
// measured" has the rest) — the three members sharing the machine's cores
// and its ONE disk, so a record's three fsyncs queue on one device and its
// copies cross loopback, where three machines each sync their own and cross
// a network — a lone push is 10.5–11.1 ms at the median on three members,
// 86–89 a second, and about 1 ms more through a member that is not the
// committer; sixty-four at once are 2 050–2 090 pushes a second to the
// committer and 1 270–1 390 through another member, two records each; a
// committer's whole queue, 1 024 at once, drains in a third of a second in
// records of sixty-four; and pushes that share no repository are a record
// each, so sixty-four at once over sixty-four repositories commit
// 201–225 records — and pushes — a second across the three members, eight
// times the records in flight for under half again the rate: one log's
// record rate, not any one committer, bounds them, and neither group commit,
// which never puts two repositories in one record, nor a member more, since
// one log has one leader, raises it.
//
// Its mix arms put a busy repository's compactions in the same chain:
// sixty-four pushes at once, each naming a pack (mix=packs), and the same
// with one compaction of every pack landed proposed beside a batch for every
// three hundred pushes (mix=compaction) — internal/code/compact's busy
// repository, a pass a minute at three hundred pushes a minute. The
// committer queues a compaction among the pushes and decides it alone, its
// decide reading its inputs' rows where a batch reads its pushes', so the two
// arms should differ by about a record's slot a compaction: the claim the
// pair is there to measure.
//
// # Why a package of its own
//
// Three members, each embedding a NATS server, in one process form a quorum,
// and a package that does runs in the solo half (`go doc ./internal/solo`).
// internal/code/codetest, which holds the suite, imports no cluster, so the
// packages that run the one-data-node suite keep sharing a runner.
package codecluster
