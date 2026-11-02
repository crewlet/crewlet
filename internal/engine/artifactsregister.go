package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// artifactsRegistrations is the artifacts domain's entry in the register
// ([register]): its declaration, applier, write seams, barrier, generation
// record, node gate and Tier A ceiling (`stream.artifacts_log_max_bytes`, read
// through config.Stream.ArtifactsMaxBytes).
//
// A STRICT, IDENTITY-CLAIMING DOMAIN like the tracker's, the pages' and the
// identity estate's, so it states a barrier (a `linearizable` read of an
// artifact is a quorum-committed position), its own generation record, and the
// node gate's writer: the trim counts nodes on this log, so one the gate could
// not write would keep an evicted node counted here for ever. It declares no
// wake feed: no record on it wakes a seat in this release.
func artifactsRegistrations() []registration {
	return []registration{{
		Domain: artifactsdomain.Domain{},
		NewApplier: func(s *stateLog) (statelog.Applier, error) {
			// WHAT EACH COMMITTED BATCH MOVED, told to every listener on this
			// node: the serving origin's authorization cache and the socket
			// hub, each of which serves only this node's own requests, so
			// the apply on every data node is what reaches each of them.
			return artifactsdomain.NewApplier(s.nodeID, s.artifactsMoved), nil
		},
		NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
			runner *statelog.Runner) (writeSeams, error) {

			rows, err := artifactsdomain.NewRows(s.estate().Reader(), spec)
			if err != nil {
				return writeSeams{}, err
			}
			fence := artifactsdomain.NewFence(s.estate().Reader(), s.nodeID)
			name := artifactsdomain.Domain{}.Name()
			fence.Floor = s.floorOf(name)
			fence.Ends = s.logEndsOf(name, appendTo, runner)
			fence.Committed = runner.Committed
			return writeSeams{Rows: rows, Fence: fence,
				Gates: artifactsdomain.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
		},
		Barrier: artifactsdomain.EncodeBarrier,
		// THE REFERENCE COMPANY'S WHOLE RATE, for the tracker's reason: a
		// seat's artifact tools read at `linearizable`
		// (statelog.ReadLevelDefaults), and the census does not say how a
		// company's reads split between the logs a seat reads.
		Trimmed:    artifactsdomain.EncodeTrimmed,
		Reads:      &statelog.ReadModel{PerSeatDay: statelog.LinearizableReadsPerSeatDay},
		Generation: artifactsdomain.GenerationRecord{},
		NewGate: func(s *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			w, err := artifactsdomain.NewWriter(artifactsdomain.WriterDeps{
				Publisher: publisher, DB: s.estate().Reader(),
				// THE GATE NEVER TRANSFERS: an eviction names a machine,
				// and the writer's seat resolver is for a transfer's new
				// owner, which this writer is never asked to record.
				SeatKind: func(context.Context, string) (iam.ActorKind, error) {
					return "", errGateTransfers
				},
			})
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, by iam.Principal, opID, node string,
				readmit bool) (statelog.Result, error) {

				// AS THE PRINCIPAL, recorded the way every write on this
				// log names its author.
				actor := iam.ActorFor(by)
				if readmit {
					return w.ReadmitNode(ctx, actor, opID, node)
				}
				return w.EvictNode(ctx, actor, opID, node)
			}, nil
		},
		OpsRetention: statelog.OpsRetention,
		Ceiling: func(stream config.Stream, free int64) domainCeiling {
			bytes, derived := stream.ArtifactsMaxBytes(free)
			return domainCeiling{Bytes: bytes,
				Field: "stream.artifacts_log_max_bytes", Explicit: !derived,
				Floor: config.ArtifactsLogMaxBytesFloor}
		},
	}}
}
