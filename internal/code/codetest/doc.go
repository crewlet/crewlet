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
// its refs, its HEAD, its packs — which is the cache's one rule (ADR-0054).
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
// than its sync — and fails if the suite passes either. Each holds an ENGINE
// WRITE as it holds a push, and is held able to fail on one: a host that
// answers a write's pending proposal applied, and a build that writes an
// object into the cache its scratch leans on.
//
// # An engine write is a seat's commit, and lands as a push
//
// Every topology judges a seat by THE FIXTURE CHART (FixtureRoster on its git
// nodes, FixtureSeatRules on its committers and change writers, FixtureSigner
// signing its commits) unless a case names its own: the unit ENG — owner of
// every case's repository — led by `lead`, with `alice` full_access, `bob`
// read_only and the human seat `pat`, whose person holds a token like every
// person's. Fleet.Write asks a write THROUGH THE FLEET'S ROUTING, as
// Fleet.GitRead asks a read: the production estate router asking the git
// nodes — each serving its git writes' lane, its git half its query server
// and its host together (GitNode.Git) — the repository's home first, stamped
// so by the walk, and the next git node the same write where one ran nothing
// or went silent (code.git_write); and Cluster.GitWriteThrough asks one
// through a member's own router. The cases (F32–F38, ADR-0062) hold what a
// person's clone then reads of the commit — its author, its committer, its
// dates, its tree — and what the push row says of who built it, that one
// write asked again is one push, that a conflict and a refusal store nothing,
// and that a git node killed at every point of a write's path leaves one push
// or none; F39–F41 (RunFleet) that a write asked through every member is
// built by the repository's home, that a home killed after its write's append
// leaves the write applied as one push — the walk's runner-up answering it
// from the push's row — and that a seat a revision revoked is refused,
// nothing landing, where the repository's committer is confirmed behind that
// revision (Cluster.Revoke), while a person's push lands beside it.
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
