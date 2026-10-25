// Package codetest is THE ONE SUITE EVERY CODE-HOST TOPOLOGY PASSES — real git
// against the real HTTP handlers, through a real code log — and the blocks
// every topology is built from. A twin that agreed only with itself would
// certify nothing; so every case asserts what a person's git prints and what
// every cache then holds, never what a fake recorded.
//
// # The topology is the engine's own composition
//
// A data node (StartDataNode) is built the way the engine builds one domain
// (internal/engine's statelog.go), over a broker client it is handed: the code
// log provisioned on it, the replicated estate a real applier writes, the
// write authority with the domain's own fence and gates, the read authority
// with a REAL barrier — so `linearizable` is the read a node serves — the
// committer, the lifecycle store and the retention pass. A git node
// (StartGitNode) is a cache, a git pool, a code host and an HTTP server that
// mounts the host's three routes behind the API's own guard, so a person's
// git meets the 401 challenge and Basic credential it meets in production.
// SoloNode is one data node on an embedded broker of its own and one or more
// git nodes over its estate; internal/code/codecluster builds three members
// from the same two blocks, so the two topologies certify one composition.
// Nothing here imports a cluster: a package that runs the one-data-node suite
// shares a runner with every other (internal/solo's roster).
//
// # A case is a person's git, and what every cache then holds
//
// Run runs every case one data node can run, each on a fleet of its own, R1
// first: the cache's own tests run against a fake of the snapshot contract,
// so the first case through the real reader is the one that rebuilds a cache
// from rows alone, and a drift between that fake and the reader fails there,
// named, before any push case muddies it. After every case each git node's
// cache, synced at the log's head, must hold exactly the repository's rows —
// its refs, its HEAD, its packs — which is the cache's one rule (ADR-0051).
//
// RunFleet runs the cases only a fleet can — a push decided by its
// repository's committer whichever node received it, a committer that dies
// after appending, a push a gate dropped, a committer that stops or is
// evicted, the crash matrix with another node committing — on ONE Cluster its
// caller hands it, since standing a fleet up costs far more than a case. So
// between cases it holds the fleet SETTLED — every member running, level with
// the log and drained, the routers past the half minute they remember a
// refusal, every cache holding exactly the rows — and once a case leaves it
// otherwise, the cases after it are not run on it, said by name rather than
// failed for the first one's fault. It refuses a cluster of fewer than three
// members: two have no quorum without each other, so the first case that
// stops one would leave a code log nobody can write.
//
// # The suite's own checks are held able to fail
//
// OkOnlyFromARow and NothingButSyncWritesACache are the two behaviours the
// record pins, as checks a meta-test runs against a topology that LIES — a
// host that reports `ok` from a decision, a cache written by something other
// than its sync — and fails if the suite passes either.
//
// # Crashes are held past the node's death
//
// A crash case arms a codefault point with Action.Crash, kills the node while
// the hit is held, restarts it over the same files and judges what the
// restart made of them; only the fleet's teardown releases the crashes, and it
// does so before it stops or waits for anything (internal/code/codefault).
// Kill therefore waits for nothing a crash may hold: it closes the listeners
// and the client connections, cancels the node's work and gives up its files;
// what waits for held work — an HTTP server's Close, the committer's Stop —
// waits in the teardown.
package codetest
