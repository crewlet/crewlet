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
// With a store directory and no [Config.StoreMaxBytes], the limit comes from
// the volume's free space, a fact about the runner rather than the case
// (brokertest's package doc, "Why a test broker is not sized from the disk"),
// so IN A TEST BINARY ([testbinary.Running]) the broker REFUSES TO START. Only
// a case about that derivation opts out ([SizedByTheDisk]); an in-memory
// broker is not asked. Checked in [startEmbedded], not the fixtures, so every
// way a test reaches a broker, tomorrow's fixture included, is held to it.
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
// A STOPPED NATS-SERVER CAN STILL WRITE ITS STORE ([shutdownAndClean]), failing
// a removal that tries once, as t.TempDir's does; brokertest.TempDir's
// outlasts it. So IN A TEST BINARY a store in no brokertest.TempDir REFUSES TO
// START; an in-memory broker is not asked. TOLD BY THE NAME
// ([testbinary.InStoreDir]), not a record: a child test binary may run on its
// parent's directory. Checked in [startEmbedded] as [ErrUndeclaredSize] is;
// brokertest's TestEveryNATSServerATestBuildsKeepsItsStoreInATempDir holds
// servers a case builds itself. The rule goes once nats-server joins that
// flush in its stop.
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
