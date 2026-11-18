package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// artifactsRegistrations is the artifacts domain's entry in the register
// ([register]), with its Tier A ceiling `stream.artifacts_log_max_bytes`
// (config.Stream.ArtifactsMaxBytes).
//
// A STRICT, IDENTITY-CLAIMING DOMAIN like the tracker's, so it states a barrier
// (a `linearizable` read is a quorum-committed position), its own generation
// record, and the node gate's writer: the trim counts nodes on this log, and a
// log the gate could not write would count an evicted node for ever. Its wake
// feed derives each comment's wake from the committed record (ADR-0012,
// ADR-0154), and its inbox tells the people a comment concerns.
func artifactsRegistrations() []registration {
	return []registration{{
		Domain: artifactsdomain.Domain{},
		Inbox:  true,
		NewApplier: func(s *stateLog) (statelog.Applier, error) {
			// WHAT EACH COMMITTED BATCH MOVED, told to this node's listeners (the
			// origin's authorization cache, the socket hub), which serve only
			// this node's requests, so every data node's apply reaches its own.
			return artifactsdomain.NewApplier(s.nodeID, s.artifactsMoved,
				s.inboxFor(artifactsdomain.Domain{})), nil
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
		Trimmed: artifactsdomain.EncodeTrimmed,
		// THE REFERENCE COMPANY'S WHOLE RATE, as the tracker's: artifact tools
		// read at `linearizable` (statelog.ReadLevelDefaults), and the census
		// does not say how reads split between the logs a seat reads.
		Reads:      &statelog.ReadModel{PerSeatDay: statelog.LinearizableReadsPerSeatDay},
		Generation: artifactsdomain.GenerationRecord{},
		NewGate: func(s *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			w, err := artifactsdomain.NewWriter(artifactsdomain.WriterDeps{
				Publisher: publisher, DB: s.estate().Reader(),
				// THE GATE NEVER TRANSFERS: an eviction names a machine, and the
				// seat resolver serves only a transfer's new owner.
				SeatKind: func(context.Context, string) (iam.ActorKind, error) {
					return "", errGateTransfers
				},
			})
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, by iam.Principal, opID, node string,
				readmit bool) (statelog.Result, error) {

				// AS THE PRINCIPAL, as every write on this log names its author.
				actor := iam.ActorFor(by)
				if readmit {
					return w.ReadmitNode(ctx, actor, opID, node)
				}
				return w.EvictNode(ctx, actor, opID, node)
			}, nil
		},
		OpsRetention: statelog.OpsRetention,
		// THE WAKE FEED, over the log's fleet-wide group: a comment's wake is
		// derived from the committed record, never published by the writer
		// (domainfeed.go).
		Feed: func(_ *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener) {
			return artifactsdomain.NewTranslator(), artifactsdomain.FeedSource{Log: records}
		},
		Ceiling: func(stream config.Stream, free int64) domainCeiling {
			bytes, derived := stream.ArtifactsMaxBytes(free)
			return domainCeiling{Bytes: bytes,
				Field: "stream.artifacts_log_max_bytes", Explicit: !derived,
				Floor: config.ArtifactsLogMaxBytesFloor}
		},
	}}
}
