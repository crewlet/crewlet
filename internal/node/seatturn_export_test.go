package node

import "context"

// ReleaseSeatTurnHoldForTest lifts [HoldSeatTurn] off handle's inbox while the
// turn that took it is still running, so a case can stand in for a mailbox
// delivery fetched before that hold took effect.
func (n *Node) ReleaseSeatTurnHoldForTest(handle string) {
	n.holdInbox(context.Background(), handle, false)
}
