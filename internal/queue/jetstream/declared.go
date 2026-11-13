package jetstream

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/testbinary"
)

// ErrUndeclaredSize is the refusal of a TEST's embedded broker that would size
// its store from the machine's free disk.
//
// AN EMBEDDED BROKER GIVEN A STORE DIRECTORY AND NO [Config.StoreMaxBytes]
// takes its limit from the free space on that directory's volume when its
// JetStream comes up. A production node is meant to: the volume is its own. A
// test's volume is the runner's, shared with every package running beside it,
// so a derived limit is a fact about the machine and not about the case — and
// every reservation the case makes is held to it. A node's boot reserves
// several gibibytes before it does anything, so on a runner short of disk the
// same case refused to boot (`the broker refused to reserve the tracker log's
// ceiling: CREWLET_TRACKER_LOG needed 1073741824 bytes and the broker had
// 867252224 bytes left`, CI run 41729197183) whatever the change under test.
// internal/queue/jetstream/brokertest's package doc has the history, and the
// figure every test declares.
//
// So IN A TEST BINARY ([testbinary.Running]) an embedded broker with a store
// directory and no limit REFUSES TO START, naming the declaration it lacks:
// brokertest.StoreIn for a node's stream, StoreMaxBytes:
// brokertest.StoreMaxBytes for a broker a test opens directly. Declaring costs
// a test nothing — the limit is accounting, which nats-server never checks
// against the disk — so the one case that may leave it unset is one whose
// SUBJECT is the derivation, and it says so where it starts the broker
// ([SizedByTheDisk]). A broker with no store directory keeps its streams in
// memory, where the file-store limit bounds nothing, and is not asked.
// Outside a test binary none of this is read.
//
// AT THE START OF EVERY EMBEDDED SERVER ([startEmbedded]) rather than in the
// fixtures: every way a test reaches one — an engine's boot, a fleet store
// opened offline, a queue opened directly, a Tier A file a case wrote and
// loaded — starts it there, so the rule needs no list of the ways, and a
// fixture written tomorrow is held to it the first time it runs. A test
// binary is told apart without linking the testing package into the crewlet
// binary ([testbinary]).
var ErrUndeclaredSize = errors.New("jetstream: a test's broker left its store limit to the machine's free disk")

// SizedByTheDisk is ctx carrying why a test leaves an embedded broker it
// starts under ctx to size its store from its directory's volume: the one
// kind of case whose subject is that derivation ([ErrUndeclaredSize]). why is
// the case's own reason; an empty one opts nothing out.
func SizedByTheDisk(ctx context.Context, why string) context.Context {
	return context.WithValue(ctx, sizedByTheDiskKey{}, why)
}

type sizedByTheDiskKey struct{}

// undeclaredSize is [ErrUndeclaredSize] for cfg started under ctx, or nil:
// outside a test binary, for a broker with no store directory, for a declared
// limit, and under [SizedByTheDisk].
func undeclaredSize(ctx context.Context, cfg Config) error {
	if !testbinary.Running() || cfg.StoreDir == "" || cfg.StoreMaxBytes != 0 {
		return nil
	}
	if why, _ := ctx.Value(sizedByTheDiskKey{}).(string); why != "" {
		return nil
	}
	return fmt.Errorf("%w: it stores in %s with no StoreMaxBytes, so nats-server would take three "+
		"quarters of that volume's free space as its limit, a fact about the machine running the "+
		"suite rather than about the case. Declare it — brokertest.StoreIn for a node's "+
		"stream.store_dir, StoreMaxBytes: brokertest.StoreMaxBytes for a broker a test opens "+
		"directly — or, where the case is about that derivation, start the broker under "+
		"jetstream.SizedByTheDisk", ErrUndeclaredSize, cfg.StoreDir)
}

// ErrStoreOutsideTempDir is the refusal of a TEST's embedded broker whose
// store directory is not one brokertest.TempDir made, nor under one.
//
// A STOPPED NATS-SERVER CAN STILL WRITE ITS STORE: a consumer's last state
// flush can land after Shutdown and WaitForShutdown have both returned
// ([shutdownAndClean] measures it), and a removal that tries once — a
// t.TempDir's — then fails the case on a file the case never wrote
// (crewlet/crewlet#1079). brokertest.TempDir's removal outlasts that write. So
// IN A TEST BINARY ([testbinary.Running]) an embedded broker whose store
// directory lies in no directory brokertest.TempDir made REFUSES TO START,
// naming the directory and brokertest.TempDir, before anything is written
// there. A broker with no store directory keeps its streams in memory, its
// metadata in a scratch directory this package makes and removes itself
// ([removeScratch]), and is not asked; nor is a store directory a case only
// validates, on which nothing starts.
//
// TOLD BY THE NAME brokertest.TempDir gives its directories
// ([testbinary.InStoreDir]), never by a record of the directories it made:
// a case can run its broker in a child test binary on a directory the parent
// made and handed it (internal/engine's broker-death case), and the name
// travels with the path.
//
// AT THE START OF EVERY EMBEDDED SERVER ([startEmbedded]), as
// [ErrUndeclaredSize] is held and for its reason: every way a test reaches one
// starts it there, so a fixture written tomorrow is held to the rule the first
// time it runs. A server a case builds from nats-server's own options never
// gets here, and brokertest's
// TestEveryNATSServerATestBuildsKeepsItsStoreInATempDir holds those.
//
// IT GOES WITH brokertest.TempDir's RETRY, once a pinned nats-server joins a
// consumer's flush loop in its stop rather than polling for it
// ([shutdownAndClean]): a test's store can then live anywhere a file can.
var ErrStoreOutsideTempDir = errors.New("jetstream: a test's broker keeps its store outside a brokertest.TempDir")

// storeOutsideTempDir is [ErrStoreOutsideTempDir] for cfg, or nil: outside a
// test binary, for a broker with no store directory, and for one whose store
// directory is, or lies under, a directory brokertest.TempDir made.
func storeOutsideTempDir(cfg Config) error {
	if !testbinary.Running() || cfg.StoreDir == "" || testbinary.InStoreDir(cfg.StoreDir) {
		return nil
	}
	return fmt.Errorf("%w: it stores in %s, where a stopped nats-server's last write, a "+
		"consumer's state flush, can land after it reports it has stopped and fail the case at a "+
		"removal that tries once (t.TempDir's among them) on a file the case never wrote "+
		"(crewlet/crewlet#1079). Keep a test broker's store in brokertest.TempDir(t), or in a "+
		"directory under one, whose removal outlasts that write", ErrStoreOutsideTempDir, cfg.StoreDir)
}
