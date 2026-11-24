package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// applyAutoJoinRooms hands the activated chart's whole seat list to every
// auto-join room ([chatlet.Channel.AutoJoin]); the room takes in each seat it
// has not accounted for ([chatlet.Store.AutoJoin]). Turning the setting on
// accounted for every seat then, and a seat put in is accounted for by the
// same record, so a newcomer who leaves is not put back.
//
// No lease, and every node runs it, for [Engine.applyUnitRooms]' reason: each
// write arbitrates on the room, so a second node costs a refused record.
// Chatlet only, and only for an activated company: a vendor-chat company
// writes nothing into this log, and an unactivated chart is not the company's.
func (e *Engine) applyAutoJoinRooms(ctx context.Context, c *Company) {
	rooms := e.ChatStore()
	if rooms == nil || c == nil || c.Org == nil || c.Config == nil ||
		c.Config.ChatBackendFor() != config.ChatChatlet || c.ActivatedAt.IsZero() {
		return
	}
	applyAutoJoin(ctx, rooms, chartSeats(c.Org))
}

// autoJoinWriter is the half of [chatlet.Store] the pass writes through.
type autoJoinWriter interface {
	AutoJoinRooms(ctx context.Context) ([]string, error)
	AutoJoin(ctx context.Context, actor chatlet.Actor, channelID string,
		seats []string) (chatlet.Written, []string, error)
}

// applyAutoJoin puts the chart's newcomers in every auto-join room. BEST
// EFFORT, as for the unit rooms: a room whose write failed takes them in at the
// next activation or restart, because they are still not accounted for.
func applyAutoJoin(ctx context.Context, rooms autoJoinWriter, seats []string) {
	ids, err := rooms.AutoJoinRooms(ctx)
	if err != nil {
		log.WarnContext(ctx, "chat_auto_join_unreadable", "error", err.Error(),
			"detail", "the rooms that take in everybody the company takes on "+
				"could not be listed; the next config apply or restart retries")
		return
	}
	// THE ENGINE NARRATES, as for the unit rooms: attributing the join to a
	// seat or a token would make it a participant in a room it only staffed.
	actor := chatlet.Actor{Handle: chatAutoJoinActor, Kind: chatlet.AuthorSystem}
	for _, id := range ids {
		_, added, err := rooms.AutoJoin(ctx, actor, id, seats)
		switch {
		case err != nil:
			log.WarnContext(ctx, "chat_auto_join_failed", "channel", id,
				"error", err.Error(), "detail", "the newcomers to the company "+
					"are not in this room yet; the next config apply or restart "+
					"retries")
		case len(added) > 0:
			log.InfoContext(ctx, "chat_auto_join_added", "channel", id,
				"members", added)
		}
	}
}

// chatAutoJoinActor is the name the engine writes an auto-join room's
// newcomers under, beside [chatUnitActor]: a name no seat takes.
const chatAutoJoinActor = iam.EngineAutoJoin
