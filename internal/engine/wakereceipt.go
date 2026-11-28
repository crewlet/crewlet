package engine

import (
	"context"
	"errors"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
)

// A person's Chatlet message keeps a receipt for each seat it reached, which
// its sender reads on the message. When a busy seat's running turn takes the
// message (joined, steered or absorbed) instead of a turn of its own, the
// engine records that on the receipt, so the message never goes quiet unseen.

// chatReceipt is the Chatlet message a wake was written as: its room and id.
type chatReceipt struct{ room, message string }

// receiptOf is the Chatlet message ev was woken by, whose receipt for the seat
// can say what a running turn did with it; false for any other wake (a
// vendor's chat keeps no receipt, a tracker's notice is no message).
func receiptOf(ev *events.Event) (chatReceipt, bool) {
	n, ok := events.DataAs[*types.ExternalNotification](ev)
	if !ok || n == nil || n.NotificationSource != chatlet.Source {
		return chatReceipt{}, false
	}
	r := chatReceipt{room: n.Metadata[notify.ChannelField], message: n.Metadata[notify.MessageIDField]}
	return r, r.room != "" && r.message != ""
}

// String is r as a wake carries it ([steer.Wake.Message]).
func (r chatReceipt) String() string { return r.room + "/" + r.message }

// parseReceipt is the receipt a wake carries, false for none.
func parseReceipt(s string) (chatReceipt, bool) {
	room, message, ok := strings.Cut(s, "/")
	return chatReceipt{room: room, message: message}, ok && room != "" && message != ""
}

// tookWakes is wakes turn's running work took in place of turns of their own,
// and how.
type tookWakes struct {
	turn  string
	as    chatlet.Taken
	wakes []steer.Wake
}

// tookOver is [runSteers.handedOver]: each wake's queued line comes down, and
// a person's joined message's receipt says the running turn took it, written
// off the path that read it, until the routers stop (an absorbed one's said so
// before it was absorbed, [Engine.absorbWake]).
func (e *Engine) tookOver(ctx context.Context, handle string, took tookWakes) {
	var triggers []string
	var people []steer.Wake
	for _, w := range took.wakes {
		triggers = append(triggers, w.Triggers...)
		if w.ByKind == iam.ActorHuman && w.Message != "" {
			people = append(people, w)
		}
	}
	if len(triggers) > 0 {
		e.settleWaiting(ctx, handle, triggers)
	}
	if took.as != chatlet.TakenJoined || len(people) == 0 {
		return
	}
	routers := &e.wakeRouters
	routers.mu.Lock()
	defer routers.mu.Unlock()
	if routers.stopped {
		return
	}
	routers.marks.Add(1)
	go func() {
		defer routers.marks.Done()
		for _, w := range people {
			if err := e.markTaken(ctx, handle, took.turn, took.as, w); err != nil {
				log.WarnContext(ctx, "seat_wake_receipt_unmarked", "seat", handle, "turn_id", took.turn,
					"wake_id", w.ID, "error", err.Error(),
					"detail", "the running turn took the message, and its receipt does not say so")
			}
		}
	}()
}

// markTaken records on handle's receipt for w's message that turnID's running
// turn took it as as, as the engine ([chatlet.Store.TakeReceipt]); nil for a
// wake whose message keeps no receipt.
func (e *Engine) markTaken(ctx context.Context, handle, turnID string, as chatlet.Taken, w steer.Wake) error {
	r, ok := parseReceipt(w.Message)
	if !ok {
		return nil
	}
	rooms := e.receipts
	if rooms == nil {
		chat, ok := e.chatHalves()
		if !ok {
			return errors.New("engine: this node hands out no chat writer")
		}
		rooms = chat
	}
	actor := chatlet.Actor{Handle: receiptActor, Kind: chatlet.AuthorSystem, TurnID: turnID}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), steerPublishBudget)
	defer cancel()
	_, err := rooms.TakeReceipt(bounded, actor, r.room, r.message, handle, as)
	return err
}

// receiptTaker is the chat write a take is recorded through.
type receiptTaker interface {
	TakeReceipt(ctx context.Context, actor chatlet.Actor, channelID, messageID, handle string,
		as chatlet.Taken) (chatlet.Written, error)
}

// receiptActor is the name the engine records a running turn's take under, a
// turn's session being what took it.
const receiptActor = iam.EngineSessions
