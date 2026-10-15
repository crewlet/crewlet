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
// # Why a package of its own
//
// Three members, each embedding a NATS server, in one process form a quorum,
// and a package that does runs in the solo half (`go doc ./internal/solo`).
// internal/code/codetest, which holds the suite, imports no cluster, so the
// packages that run the one-data-node suite keep sharing a runner.
package codecluster
