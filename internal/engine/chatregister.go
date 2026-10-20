package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// chatRegistrations is the company's own chat in the register ([register]):
// the EIGHTH domain and the SIXTH strict, identity-claiming one, whose log
// carries two disciplines — a room's state arbitrated on its own subject, its
// messages sequenced beside it and decided by the applier (ADR-0045). Its declaration, applier,
// write seams, barrier, read model, generation record, node gate, wake feed
// and Tier A ceiling (`stream.chat_log_max_bytes`, read through
// [chatMaxBytes]).
func chatRegistrations() []registration {
	return []registration{{
		Domain: chat.Domain{},
		NewApplier: func(s *stateLog) (statelog.Applier, error) {
			// THE ENGINE'S HOOK AS ITS LIVE OBSERVER, whatever is
			// attached: what it hears goes to the chat hub the
			// process attached ([Engine.SetChatLive]) and to this
			// node's relay followers (liverelay.go).
			return chat.NewApplier(s.nodeID, chatObserver(s.chatApplied)), nil
		},
		NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
			runner *statelog.Runner) (writeSeams, error) {

			rows, err := chat.NewRows(s.estate().Reader(), spec)
			if err != nil {
				return writeSeams{}, err
			}
			fence := chat.NewFence(s.estate().Reader(), s.nodeID)
			name := chat.Domain{}.Name()
			fence.Floor = s.floorOf(name)
			fence.Ends = s.logEndsOf(name, appendTo, runner)
			fence.Committed = runner.Committed
			return writeSeams{Rows: rows, Fence: fence,
				Gates: chat.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
		},
		Barrier: chat.EncodeBarrier,
		// ITS OWN READ MODEL, counted from what a chat turn reads at
		// `linearizable` — see [chat.LinearizableReadsPerSeatDay],
		// which is also the barrier term in the log's ceiling
		// ([chat.ChatLogMaxBytes]).
		Reads:      &statelog.ReadModel{PerSeatDay: chat.LinearizableReadsPerSeatDay},
		Generation: chat.GenerationRecord{},
		NewGate: func(s *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			// A WRITER THAT NEEDS NO ROSTER: an eviction is said
			// about a machine rather than in a room, and wakes
			// nobody — see [chat.GateWriter].
			w, err := chat.NewGateWriter(chat.GateOptions{Publisher: publisher})
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, by iam.Principal, opID, node string,
				readmit bool) (statelog.Result, error) {

				// THE PRINCIPAL AS EVERY LOG RECORDS IT ([iam.ActorFor]):
				// chat's four author kinds are the identity
				// directory's own words, so the tracker's, the wiki's
				// and this log's records of one eviction name one
				// party — a credential acting as itself included,
				// since an eviction needs no seat.
				a := iam.ActorFor(by)
				actor := chat.Actor{Handle: a.Name, Kind: chat.AuthorKind(a.Kind),
					OperatorID: a.OperatorID}
				if readmit {
					return w.ReadmitNode(ctx, actor, opID, node)
				}
				return w.EvictNode(ctx, actor, opID, node)
			}, nil
		},
		// THE WAKE A COMMITTED MESSAGE SENDS, derived from the log on
		// the domain's own fleet-wide group ([chat.FeedGroup]) rather
		// than published by the writer's goroutine.
		Feed: func(_ *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener) {
			return chat.NewTranslator(), chat.FeedSource{Log: records}
		},
		// THE FRAMEWORK'S ONE HORIZON: ADR-0029 judges a retry
		// against it, and a seat re-asks a chat operation on the
		// rhythm it re-asks every other.
		OpsRetention: statelog.OpsRetention,
		// AN EXCURSION BUDGET THE DOMAIN DECLARES, capped by the
		// volume — see [chatMaxBytes].
		Ceiling: func(stream config.Stream, free int64) domainCeiling {
			bytes, derived := chatMaxBytes(stream, free)
			return domainCeiling{Bytes: bytes,
				Field: "stream.chat_log_max_bytes", Explicit: !derived,
				Floor: config.ChatLogMaxBytesFloor}
		},
	}}
}
