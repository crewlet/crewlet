package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// chatRegistrations is the company's own chat in the register ([register]):
// the NINTH domain and the SEVENTH strict, identity-claiming one, whose log
// carries two disciplines — a room's state arbitrated on its own subject, its
// messages sequenced beside it and decided by the applier (ADR-0046). Its declaration, applier,
// write seams, barrier, read model, generation record, node gate, wake feed
// and Tier A ceiling (`stream.chat_log_max_bytes`, read through
// [chatMaxBytes]).
func chatRegistrations() []registration {
	return []registration{{
		Domain: chatlet.Domain{},
		NewApplier: func(s *stateLog) (statelog.Applier, error) {
			// THE ENGINE'S HOOK AS ITS LIVE OBSERVER, whatever is
			// attached: what it hears goes to the chat hub the
			// process attached ([Engine.SetChatLive]) and to this
			// node's relay followers (liverelay.go).
			return chatlet.NewApplier(s.nodeID, chatObserver(s.chatApplied)), nil
		},
		NewSeams: func(s *stateLog, spec statelog.StreamSpec, appendTo *jetstream.DomainLog,
			runner *statelog.Runner) (writeSeams, error) {

			rows, err := chatlet.NewRows(s.estate().Reader(), spec)
			if err != nil {
				return writeSeams{}, err
			}
			fence := chatlet.NewFence(s.estate().Reader(), s.nodeID)
			name := chatlet.Domain{}.Name()
			fence.Floor = s.floorOf(name)
			fence.Ends = s.logEndsOf(name, appendTo, runner)
			fence.Committed = runner.Committed
			return writeSeams{Rows: rows, Fence: fence,
				Gates: chatlet.NewGates(s.estate().Reader()), Evicted: fence.Evicted}, nil
		},
		Barrier: chatlet.EncodeBarrier,
		Trimmed: chatlet.EncodeTrimmed,
		// ITS OWN READ MODEL, counted from what a chat turn reads at
		// `linearizable` — see [chatlet.LinearizableReadsPerSeatDay],
		// which is also the barrier term in the log's ceiling
		// ([chatlet.ChatLogMaxBytes]).
		Reads:      &statelog.ReadModel{PerSeatDay: chatlet.LinearizableReadsPerSeatDay},
		Generation: chatlet.GenerationRecord{},
		NewGate: func(s *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			// A WRITER THAT NEEDS NO ROSTER: an eviction is said
			// about a machine rather than in a room, and wakes
			// nobody — see [chatlet.GateWriter].
			w, err := chatlet.NewGateWriter(chatlet.GateOptions{Publisher: publisher})
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
				actor := chatlet.Actor{Handle: a.Name, Kind: chatlet.AuthorKind(a.Kind),
					OperatorID: a.OperatorID}
				if readmit {
					return w.ReadmitNode(ctx, actor, opID, node)
				}
				return w.EvictNode(ctx, actor, opID, node)
			}, nil
		},
		// THE WAKE A COMMITTED MESSAGE SENDS, derived from the log on
		// the domain's own fleet-wide group ([chatlet.FeedGroup]) rather
		// than published by the writer's goroutine — and from what this
		// node's applier decided about it, so a post the applier refused
		// wakes nobody ([chatlet.Decisions]).
		//
		// ALWAYS THIS NODE'S DECISIONS, never a translator that asks
		// nothing: built where no core runs (the register's own check
		// of each entry's declaration) they are the zero estate, which
		// refuses on use and is logged on every record the feed then
		// derives from the record alone — loud, where a nil would be a
		// feed that silently woke about refused posts.
		Feed: func(_ *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener) {
			decisions := chatlet.NodeDecisions{Estate: records.estate}
			if records.applied != nil {
				decisions.Applied = records.applied
			}
			return chatlet.NewTranslator(decisions), chatlet.FeedSource{Log: records}
		},
		// THE FRAMEWORK'S ONE HORIZON: ADR-0029 judges a retry
		// against it, and a seat re-asks a chat operation on the
		// rhythm it re-asks every other.
		OpsRetention: statelog.OpsRetention,
		// AN EXCURSION BUDGET THE DOMAIN DECLARES, capped by the
		// volume — see [chatMaxBytes].
		//
		// AND NO ScalesTo, unlike the assistant's entry: scaling stops at
		// the gibibyte every corpus-sized log takes, so a fresh node
		// reserves the whole of it ([BrokerFloor]). The assistant gives
		// way on a short broker because a company may never turn it on;
		// chat is the company's own unless a vendor's is declared. Nor
		// could giving way have kept a fresh node inside the 9 GiB limit
		// it fitted before chat joined and left a chat that works: beside
		// the other logs at their floors and the nats bucket that limit
		// leaves it 192 MiB, less than the one min_age window of messages
		// a healthily trimming chat log keeps, about 225 MB at the
		// declared census.
		Ceiling: func(stream config.Stream, free int64) domainCeiling {
			bytes, derived := chatMaxBytes(stream, free)
			return domainCeiling{Bytes: bytes,
				Field: "stream.chat_log_max_bytes", Explicit: !derived,
				Floor: config.ChatLogMaxBytesFloor}
		},
	}}
}
