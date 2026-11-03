package node

import "context"

// ReleaseSeatTurnHoldForTest lifts [HoldSeatTurn] off handle's inbox while the
// turn that took it is still running, so a case can stand in for a mailbox
// delivery fetched before that hold took effect.
func (n *Node) ReleaseSeatTurnHoldForTest(handle string) {
	n.mu.Lock()
	held := n.running[handle]
	n.mu.Unlock()
	if held != nil {
		n.holdInbox(context.Background(), handle, held.hold, false)
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
