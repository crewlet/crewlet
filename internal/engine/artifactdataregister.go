package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// artifactDataRegistrations is the artifact datastore's entry in the register
// ([register]), after the artifacts domain: its declaration, applier, write
// seams, barrier, generation record, node gate and Tier A ceiling
// (`stream.artifact_data_log_max_bytes`, read through
// config.Stream.ArtifactDataMaxBytes).
//
// A STRICT, IDENTITY-CLAIMING DOMAIN, so it states a barrier (a
// `linearizable` read of a store is a quorum-committed position), its own
// generation record and the node gate's writer: the trim counts nodes on this
// log too. It declares no wake feed: no record on it wakes a seat in this
// release (datastore watches arrive with a later build).
func artifactDataRegistrations() []registration {
	return []registration{{
		Domain: artifactdata.Domain{},
		NewApplier: func(s *stateLog) (statelog.Applier, error) {
			// WHAT EACH COMMITTED BATCH MOVED, told to this node's socket
			// hub: every data node applies every record, so each node's
			// apply is what reaches its own sockets.
			return artifactdata.NewApplier(s.nodeID, s.artifactDataMoved), nil
		},
		NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
			runner *statelog.Runner) (writeSeams, error) {

			rows, err := artifactdata.NewRows(s.estate().Reader(), spec)
			if err != nil {
				return writeSeams{}, err
			}
			fence := artifactdata.NewFence(s.estate().Reader(), s.nodeID)
			name := artifactdata.Domain{}.Name()
			fence.Floor = s.floorOf(name)
			fence.Ends = s.logEndsOf(name, appendTo, runner)
			fence.Committed = runner.Committed
			return writeSeams{Rows: rows, Fence: fence,
				Gates: artifactdata.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
		},
		Barrier: artifactdata.EncodeBarrier,
		// THE REFERENCE COMPANY'S WHOLE RATE, for the tracker's reason: a
		// seat's datastore tools read at `linearizable`
		// (statelog.ReadLevelDefaults), and the census does not say how a
		// company's reads split between the logs a seat reads.
		Reads:      &statelog.ReadModel{PerSeatDay: statelog.LinearizableReadsPerSeatDay},
		Generation: artifactdata.GenerationRecord{},
		NewGate: func(s *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			w, err := artifactdata.NewWriter(artifactdata.WriterDeps{
				Publisher: publisher, DB: s.estate().Reader(),
				// THE GATE WRITES NO DOCUMENT: an eviction names a machine,
				// and the writer's seat resolver is for a person column's
				// value, which this writer is never asked to hold.
				SeatExists: func(context.Context, string) (bool, error) {
					return false, errGateWritesNoDocument
				},
			})
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, by iam.Principal, opID, node string,
				readmit bool) (statelog.Result, error) {

				actor := iam.ActorFor(by)
				if readmit {
					return w.ReadmitNode(ctx, actor, opID, node)
				}
				return w.EvictNode(ctx, actor, opID, node)
			}, nil
		},
		OpsRetention: statelog.OpsRetention,
		Ceiling: func(stream config.Stream, free int64) domainCeiling {
			bytes, derived := stream.ArtifactDataMaxBytes(free)
			return domainCeiling{Bytes: bytes,
				Field: "stream.artifact_data_log_max_bytes", Explicit: !derived,
				Floor: config.ArtifactDataLogMaxBytesFloor}
		},
	}}
}
