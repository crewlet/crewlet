// Package brokertest is the size a test broker declares, so that whether a
// suite passes does not depend on how much disk the machine running it has
// free.
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
// Every test broker that reserves a FIXED amount — a node, a cluster member, a
// lone domain log or object bucket at a ceiling the case chose. Three kinds are
// left to derive their limit, because no disk can make them fail: a broker that
// reserves nothing (key-value buckets, a core-NATS transfer, streams with no
// byte limit), a case that reserves only what the broker reports it has left,
// and a case whose subject IS the derived limit.
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
// # Why not jetstreamtest or externaltest
//
// Importing jetstreamtest declares a multi-member suite (see internal/solo),
// and externaltest is the external server alone. The embedded fixtures need the
// figure without either.
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
// [StoreMaxBytes].
//
// THE DIRECTORY AND THE SIZE TOGETHER: a store directory is what turns an
// embedded broker's limit from a memory figure into a derivation from that
// directory's volume, and a node reserves several gibibytes of it at boot, so
// a node fixture that sets the one sets the other. A case whose subject IS the
// derived limit, or a refusal below the floor, sets store_dir and
// store_max_bytes itself.
func StoreIn(s *config.Stream, dir string) {
	s.StoreDir = dir
	s.StoreMaxBytes = StoreMaxBytes
}
