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

// OnBusyRefusalForTest runs hook as a turn refused on a busy seat lets go of
// the node's lock, before the refusal returns.
func (n *Node) OnBusyRefusalForTest(hook func(handle string)) {
	n.refusedBusy = hook
}
