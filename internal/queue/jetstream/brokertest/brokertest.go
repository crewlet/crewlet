// Package brokertest is what a test broker declares: its size, so that whether
// a suite passes does not depend on how much disk the machine running it has
// free, and the directory it keeps its store in, so that it does not depend on
// when a stopped broker's last write lands either.
//
// # Why a test broker is not sized from the disk
//
// An embedded broker whose `stream.store_max_bytes` is unset takes three
// quarters of the free space on its store's volume as its limit
// (nats-server's own derivation), and an external server with no
// `max_file_store` does the same. A production node is meant to: the volume is
// its own. A test's volume is the runner's, shared with every package running
// beside it, so a derived limit is a fact about the machine and not about the
// case — and a fresh node reserves several gibibytes of its broker before it
// does anything (internal/engine's BrokerFloorFor has the figure). On a runner
// short of that, every boot was refused at whichever state log's ceiling no
// longer fitted, with `insufficient storage resources available`, whatever the
// change under test.
//
// A declared limit is ACCOUNTING: nats-server holds a reservation to it and
// never checks it against the disk, so declaring more than the runner has free
// costs nothing until a case actually writes that much, and none does.
//
// # Which brokers declare it
//
// EVERY EMBEDDED BROKER A TEST STARTS WITH A STORE DIRECTORY — a node, a
// cluster member, a lone domain log, a queue a case opens — and a test binary
// refuses to start one that does not (jetstream.ErrUndeclaredSize). The check
// is at the start of the server, where every way a test reaches one arrives —
// an engine's boot, a fleet store opened offline, a queue opened directly, a
// Tier A file a case wrote and loaded — so no fixture is missed however it
// gets there. It used to be a rule for the brokers that reserve a fixed
// amount, with a broker that reserves nothing and a case that reserves only
// what the broker reports it has left exempt; but declaring costs nothing,
// and the fixtures that left it unset were not the exempt ones — an engine's
// Gitlet node and the CLI's restart cases both boot several gibibytes of logs,
// and both were refused on a runner short of disk (CI run 41729197183). One
// rule is one a refusal can hold.
//
// The one case that leaves the limit to nats-server is one whose SUBJECT is
// that derivation, and it starts its broker under jetstream.SizedByTheDisk —
// an engine under engine.SizedByTheMachine — saying why. A broker with no
// store directory keeps its streams in memory, where a file-store limit bounds
// nothing, and is not asked. A server a case builds from nats-server's own
// options never reaches the check, so it declares its limit in them —
// JetStreamMaxStore: StoreMaxBytes, as externaltest's server does, or
// max_file_store in a configuration it writes for nats-server to read — and
// TestEveryNATSServerATestBuildsDeclaresItsSize, which reads every Go file of
// the module and judges an Options value by its type, fails one that does not.
//
// A server a NODE dials as its external broker is held one step further,
// because the node never reads the server's cap: it reads the account's limit,
// and where the account states none it stands three quarters of its own
// volume's free space in for one. A server built from options with its cap
// declared and its account unlimited therefore refused what that stand-in
// sized on a roomy enough runner: the engine's object-window case built one,
// and it refused the node's bucket on any runner with more than about 92 GB
// free. So externaltest's server states the account's limit too, and a test
// binary's node refuses to open on an external broker whose account states
// none (engine.ErrUndeclaredBrokerLimit).
//
// # Why one figure
//
// The floor of a fresh node is a sum over the state-log register and the
// object bucket, and it grows when a log joins the register. A figure restated
// per fixture would drift from it one fixture at a time. With one, a register
// that outgrows it refuses every node boot on every machine at once —
// deterministically, rather than only on a small disk — and the engine's
// TestTheTestBrokerSizeHoldsAFreshNode is the failure among them that names the
// cause and this constant.
//
// # And the code cache beside it
//
// A node that hosts code derives its cache's budget from the ROOM on the
// cache's volume (internal/engine's codeCacheBudget). The room is the free
// space, less what a broker storing its streams there may still write under its
// limit, the request spools and the revision facts' patch spools. A declared
// limit is a claim on that room, and the room is the runner's free disk. So a
// fixture with this package's sixteen gibibytes on the temporary volume beside
// its cache has a derived budget only where the runner has more than about
// seventeen and a half gibibytes free. Below that the room is nothing, and the
// node refuses to host code at all. The end-to-end gates' runner had 9.2 GB
// free (CI run 41669250876), and all five of their code-hosting cases failed at
// engine.New with that refusal.
//
// So a node fixture that declares its broker here declares its code cache
// beside it ([GitletCacheIn]). The budget is the smallest a node is ever given
// explicitly. The transfer lane is a fixed one, since an unset lane follows the
// machine's cores and sizes the request spools taken out of the same room. An
// explicit budget is never refused for room. On a runner with less free space
// than the declared tenants, the engine still says, at code_cache_sized, that
// the budget is more than the room. That is the truth about a declaration that
// is accounting, and it changes neither the budget the cache is held to nor the
// case's outcome.
//
// A test binary's node that builds Gitlet with either left unset refuses
// to (engine.ErrUndeclaredCodeCache), at the build, so a fixture that boots
// Gitlet by any route is held to it. The budget is not asked of a case that
// stands in for the cache's volume itself (internal/engine's cacheDisk), whose
// derivation is the case's own figures; and a case whose subject is the
// derivation boots under engine.SizedByTheMachine.
//
// # And the directory the store is kept in
//
// Every directory a test's broker keeps its store in comes from [TempDir],
// never from t.TempDir: the store of a node fixture ([StoreIn]'s directory), of
// a broker a case opens or starts itself, of jetstreamtest's members, of
// externaltest's server and of a server a case builds from nats-server's
// options. A stopped nats-server can still write its store for a moment after
// it reports it has stopped, a consumer's last state flush landing late, and
// t.TempDir's one removal then fails the case on a file the case never wrote.
// TempDir's removal tries again while, and only while, the directory comes
// back not empty, a bounded number of times ([TempDir] has the race and the
// bound, and when the retry goes). A broker running in a
// child process the case waits for ends its writes with the process, and is
// held to the same rule anyway, so a fixture is never asked which kind it
// starts. A store directory set only to be validated, with no broker started
// on it, is no broker's and may stay a t.TempDir.
//
// HELD WHERE THE SIZE IS, and for its reason: a rule kept by hand holds only
// for the cases converted the day it was written, and a store in a t.TempDir
// fails one rare run in many, which a rerun hides. So a test binary refuses to
// start an embedded broker whose store directory lies in no directory TempDir
// made (jetstream.ErrStoreOutsideTempDir), told by the name TempDir gives each
// one (testbinary.StoreDirPrefix), however the case reached it. A server a
// case builds from nats-server's own options never gets there, and this
// package's TestEveryNATSServerATestBuildsKeepsItsStoreInATempDir fails one
// whose store a testing.TB's TempDir made, on the walk the size gate reads.
//
// # Why not jetstreamtest or externaltest
//
// Importing jetstreamtest declares a multi-member suite (see internal/solo),
// and externaltest is the external server alone. The embedded fixtures need the
// figure, and the directory, without either.
package brokertest

import "github.com/crewlet/crewlet/internal/config"

// StoreMaxBytes is the storage limit a test broker declares: an embedded one
// through `stream.store_max_bytes`, an external one (externaltest's server) as
// its account's JetStream file-store limit.
//
// Sixteen gibibytes, held from below by two consumers: the floor of a fresh
// node of the fixtures' Tier A (TestTheTestBrokerSizeHoldsAFreshNode prints
// both figures when it fails), and internal/engine's brokerWithHeadroom, whose
// largest scenario leaves twelve gibibytes of headroom and spends a gibibyte
// around it (it fails naming this constant when the cap cannot hold that).
// Nothing a case reads bounds it from above, since the figure is accounting; it
// is the power of two that clears both with room for the register to grow
// before anyone has to raise it.
const StoreMaxBytes int64 = 16 << 30

// StoreIn gives s an embedded broker whose store is dir, sized at
// [StoreMaxBytes]. dir is in, or is, a [TempDir], whose removal at the end of
// the test outlasts the broker's last late write (see the package doc); a
// test binary refuses to start the broker in any other
// (jetstream.ErrStoreOutsideTempDir).
//
// THE DIRECTORY AND THE SIZE TOGETHER: a store directory is what turns an
// embedded broker's limit from a memory figure into a derivation from that
// directory's volume, and a node reserves several gibibytes of it at boot, so
// a node fixture that sets the one sets the other. A case whose subject IS the
// derived limit sets store_dir alone and boots under engine.SizedByTheMachine,
// and one about a refusal below the floor sets a store_max_bytes of its own.
func StoreIn(s *config.Stream, dir string) {
	s.StoreDir = dir
	s.StoreMaxBytes = StoreMaxBytes
}

// CodeCacheMaxBytes is the code cache budget a node fixture declares beside a
// broker [StoreIn] sized: `gitlet.cache_max_bytes`'s floor, the smallest budget a
// node is ever given explicitly. A case's repositories hold kilobytes, so the
// floor holds every one of them with three orders of magnitude to spare.
const CodeCacheMaxBytes = config.GitletCacheMaxBytesFloor

// GitTransfers is the transfer lane a node fixture declares: four transfers,
// 256 MiB of request spool. Unset, the lane follows the machine's cores, four a
// core as memory allows. A sixteen-core runner's 4 GiB of spools then came out
// of the room of every cache beside them, so whether a node hosted code was a
// property of the machine running the suite, never of the case.
const GitTransfers = 4

// GitletCacheIn declares g's code cache budget and transfer lane, for a node
// fixture whose broker [StoreIn] sized on the same volume — see the package
// doc. A fixture that never hosts code keeps both unread, and one whose subject
// is the derived budget, or the lane, boots under engine.SizedByTheMachine.
func GitletCacheIn(g *config.Gitlet) {
	g.CacheMaxBytes = CodeCacheMaxBytes
	g.MaxTransfers = GitTransfers
}
