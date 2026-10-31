package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
)

// THE AUTO-JOIN ROOMS: a public room an administrator set to take in
// everybody the company takes on ([chatlet.Channel.AutoJoin]).
//
// # When a seat joins the company
//
// A seat joins the company when an activation's org chart holds it, so this
// runs where the unit rooms are staffed, at every published company, with the
// chart's whole seat list. The room decides who of them is new: every seat it
// has not accounted for, since turning the setting on accounted for every seat
// the company had then ([chatlet.Store.AutoJoin]). A seat put in is accounted
// for by the same record, so the next activation decides nothing for it, and a
// newcomer who leaves the room is not put back.
//
// # Why no lease, and why every node
//
// For [Engine.applyUnitRooms]' reason: each room's write arbitrates on the
// room, so two nodes running this race at the broker and the loser finds the
// newcomers accounted for and decides nothing. The worst a second node costs
// is a refused record.
//
// # Behind the same two gates
//
// Chatlet's alone, and only for a company an activation named: a company that
// moved to a vendor surface writes nothing into this log, and a chart nobody
// activated is not the company's yet.
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

// applyAutoJoin puts the chart's newcomers in every auto-join room, logging
// each room it took somebody into and each it could not bring into line.
//
// BEST EFFORT, retried by the next published company, as the unit rooms are:
// a room whose write failed takes its newcomers in at the next activation or
// restart, because they are still not accounted for.
func applyAutoJoin(ctx context.Context, rooms autoJoinWriter, seats []string) {
	ids, err := rooms.AutoJoinRooms(ctx)
	if err != nil {
		log.WarnContext(ctx, "chat_auto_join_unreadable", "error", err.Error(),
			"detail", "the rooms that take in everybody the company takes on "+
				"could not be listed; the next config apply or restart retries")
		return
	}
	// THE ENGINE NARRATING, as for the unit rooms: the line says the
	// newcomers joined, and attributing it to a seat or a token would make
	// the engine a participant in a room it only staffed.
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
// newcomers under, beside [chatUnitActor]: a name, not a seat.
const chatAutoJoinActor = "auto-join"
