package seataccess

// DisabledByOf is who turned off an account a vendor observes disabled (D3),
// from the ledger's record of it and the vendor's own marker on the account.
//
// THE LEDGER WHERE IT HAS A RECORD of the account, and the marker only where
// it has none, which is the marker's purpose: surviving a lost ledger, or
// naming an account this engine turned off before the ledger existed. A
// marker can be stale (an off that failed part way, or an older engine's), and
// trusting it over a ledger that says the account is active would make
// somebody's own disable read as this engine's, and be undone. Anything the
// ledger does not record as this engine's off is somebody else's.
func DisabledByOf(marked bool, rec Record, recorded bool) DisabledBy {
	if recorded && rec.Account != nil {
		if rec.Account.State == AccountDisabled {
			return DisabledByCrewlet
		}
		return DisabledByHand
	}
	if marked {
		return DisabledByCrewlet
	}
	return DisabledByHand
}

// LeavesOn reports an op that leaves the account on, on which a vendor's
// marker saying this engine turned the account off is stale and is taken off.
func (o Op) LeavesOn() bool {
	switch o {
	case OpDisable, OpRevoke, OpRemove:
		return false
	default:
		return true
	}
}
