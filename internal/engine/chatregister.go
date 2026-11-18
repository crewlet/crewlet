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
// the NINTH domain and the SEVENTH strict, identity-claiming one. Its log
// carries a room's state arbitrated on its own subject and its messages
// sequenced beside it, decided by the applier (ADR-0046). Its Tier A ceiling is
// `stream.chat_log_max_bytes`, read through [chatMaxBytes].
func chatRegistrations() []registration {
	return []registration{{
		Domain: chatlet.Domain{},
		NewApplier: func(s *stateLog) (statelog.Applier, error) {
			// The engine's hook is the live observer, whatever is
			// attached: it feeds the chat hub ([Engine.SetChatLive]) and
			// this node's relay followers (liverelay.go).
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
		// Its own read model, counted from a chat turn's `linearizable`
		// reads ([chatlet.LinearizableReadsPerSeatDay], also the barrier
		// term of [chatlet.ChatLogMaxBytes]).
		Reads:      &statelog.ReadModel{PerSeatDay: chatlet.LinearizableReadsPerSeatDay},
		Generation: chatlet.GenerationRecord{},
		NewGate: func(s *stateLog, _ *runningLog,
			publisher *statelog.Publisher) (gateWrite, error) {

			// No roster: an eviction is about a machine, not in a room,
			// and wakes nobody ([chatlet.GateWriter]).
			w, err := chatlet.NewGateWriter(chatlet.GateOptions{Publisher: publisher})
			if err != nil {
				return nil, err
			}
			return func(ctx context.Context, by iam.Principal, opID, node string,
				readmit bool) (statelog.Result, error) {

				// The principal as every log records it ([iam.ActorFor]):
				// chat's author kinds are the identity directory's, so
				// every log names one party for one eviction, a
				// credential acting as itself included.
				a := iam.ActorFor(by)
				actor := chatlet.Actor{Handle: a.Name, Kind: chatlet.AuthorKind(a.Kind),
					OperatorID: a.OperatorID}
				if readmit {
					return w.ReadmitNode(ctx, actor, opID, node)
				}
				return w.EvictNode(ctx, actor, opID, node)
			}, nil
		},
		// The wake a committed message sends is derived from the log on
		// the domain's fleet-wide group ([chatlet.FeedGroup]) and from
		// this node's applier's decisions, so a refused post wakes nobody
		// ([chatlet.Decisions]). Always this node's decisions: where no
		// core runs they are the zero estate, which refuses on use and is
		// logged, where a nil would silently wake about refused posts.
		Feed: func(_ *Engine, records domainFeed) (changefeed.Translator, changefeed.Opener) {
			decisions := chatlet.NodeDecisions{Estate: records.estate}
			if records.applied != nil {
				decisions.Applied = records.applied
			}
			return chatlet.NewTranslator(decisions), chatlet.FeedSource{Log: records}
		},
		// The framework's one horizon (ADR-0029): a seat re-asks a chat
		// operation on the rhythm it re-asks every other.
		OpsRetention: statelog.OpsRetention,
		// An excursion budget the domain declares, capped by the volume
		// ([chatMaxBytes]). No ScalesTo, unlike the assistant (which a
		// company may never turn on): chat is the company's own unless a
		// vendor's is declared, so a fresh node reserves the whole
		// corpus-sized gibibyte ([BrokerFloor]). Giving way could not have
		// kept a fresh node in its old 9 GiB either: that leaves chat
		// 192 MiB, under the ~225 MB min_age window a healthy log keeps.
		Ceiling: func(stream config.Stream, free int64) domainCeiling {
			bytes, derived := chatMaxBytes(stream, free)
			return domainCeiling{Bytes: bytes,
				Field: "stream.chat_log_max_bytes", Explicit: !derived,
				Floor: config.ChatLogMaxBytesFloor}
		},
	}}
}
