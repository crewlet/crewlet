package node

import (
	"context"
	"slices"
)

// ReleaseSeatTurnHoldForTest lifts [HoldSeatTurn] off handle's inbox while the
// turn that took it is still running, so a case can stand in for a mailbox
// delivery fetched before that hold took effect.
func (n *Node) ReleaseSeatTurnHoldForTest(handle string) {
	n.mu.Lock()
	var held []*admission
	if seat := n.running[handle]; seat != nil {
		held = slices.Clone(seat.turns)
	}
	n.mu.Unlock()
	for _, turn := range held {
		n.holdInbox(context.Background(), turn, false)
	}
}

// SeatTurnRunningForTest reports whether a turn holds handle on this node. It
// asks without trying to admit one, since a refusal is itself an event the
// seat's release answers. A case whose turn reports from inside itself waits
// on this for the turn to let the seat go, because the turn sends its report
// before it is released.
func (n *Node) SeatTurnRunningForTest(handle string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.running[handle] != nil
}

// OnBusyRefusalForTest runs hook as a turn refused on a busy seat lets go of
// the node's lock, before the refusal returns.
func (n *Node) OnBusyRefusalForTest(hook func(handle string)) {
	n.refusedBusy = hook
}

// MailboxTurnForTest marks ctx as a mailbox delivery's, which [Node.AdmitTurn]
// admits on the mailbox's terms: waiting for room rather than refused.
func MailboxTurnForTest(ctx context.Context) context.Context { return mailboxTurn(ctx) }
