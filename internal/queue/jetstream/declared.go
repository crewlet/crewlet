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
