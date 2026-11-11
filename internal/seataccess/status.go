package seataccess

import "slices"

// LaneStatus is one seat's lane as one word: what the Agents tab draws on the
// row and what Ask Crewlet reads, computed here once so the two cannot word
// the same lane two ways. A draft's "Not saved" is the dashboard's alone.
type LaneStatus string

const (
	// StatusSyncing is a lane with steps to take while a sync of its
	// integration is running.
	StatusSyncing LaneStatus = "syncing"
	// StatusRefused is a lane whose last sync failed.
	StatusRefused LaneStatus = "refused"
	// StatusChangedAtVendor is a lane changed at the vendor by somebody
	// else, which the next sync restores.
	StatusChangedAtVendor LaneStatus = "changed_at_vendor"
	// StatusNeedsYou is a lane waiting on a person: a step only a person
	// takes, or a sync that ended waiting on one.
	StatusNeedsYou LaneStatus = "needs_you"
	// StatusNextSync is a lane with steps the next sync takes.
	StatusNextSync LaneStatus = "next_sync"
	// StatusInSync is a lane that is on and has nothing left to take.
	StatusInSync LaneStatus = "in_sync"
	// StatusOffAccountKept is a lane that is off whose account is kept.
	StatusOffAccountKept LaneStatus = "off_account_kept"
	// StatusOff is a lane that is off with no account.
	StatusOff LaneStatus = "off"
	// StatusDeparted is a seat that left the company document whose account
	// the ledger still records.
	StatusDeparted LaneStatus = "departed"
)

// LaneStatuses is every status, in the order a person needs to act on them.
func LaneStatuses() []LaneStatus {
	return []LaneStatus{
		StatusDeparted, StatusSyncing, StatusRefused, StatusChangedAtVendor, StatusNeedsYou,
		StatusNextSync, StatusInSync, StatusOffAccountKept, StatusOff,
	}
}

// Valid reports whether s is a status this build words.
func (s LaneStatus) Valid() bool { return slices.Contains(LaneStatuses(), s) }

// Label is the status in the Agents tab's words, vendor naming who changed a
// lane at the vendor ("Changed at GitHub").
func (s LaneStatus) Label(vendor string) string {
	switch s {
	case StatusSyncing:
		return "Syncing"
	case StatusRefused:
		return "Refused"
	case StatusChangedAtVendor:
		return "Changed at " + vendor
	case StatusNeedsYou:
		return "Needs you"
	case StatusNextSync:
		return "Next sync"
	case StatusInSync:
		return "In sync"
	case StatusOffAccountKept:
		return "Off, account kept"
	case StatusOff:
		return "Off"
	case StatusDeparted:
		return "Agent left"
	}
	return string(s)
}

// StatusOf is one saved lane's status: syncing is a sync of its integration
// running now, and departed a seat that left the document.
func StatusOf(la LaneAnswer, syncing, departed bool) LaneStatus {
	pending := len(la.Plan) > 0
	switch {
	case departed:
		return StatusDeparted
	case syncing && pending:
		return StatusSyncing
	case la.Sync != nil && la.Sync.Outcome == OutcomeFailed:
		return StatusRefused
	case !la.Observed.ChangedByHandAt.IsZero() && pending:
		return StatusChangedAtVendor
	case la.Sync != nil && la.Sync.Outcome == OutcomeWaiting,
		slices.ContainsFunc(la.Plan, func(s Step) bool { return s.By == ByYou }):
		return StatusNeedsYou
	case pending:
		return StatusNextSync
	case la.Desired.Enabled:
		return StatusInSync
	case la.Observed.Exists:
		return StatusOffAccountKept
	}
	return StatusOff
}

// SetStatuses sets every seat's lane's Status and StatusLabel from the answer
// as it stands.
func (a *Answer) SetStatuses() {
	for i, seat := range a.Seats {
		for lane, la := range seat.Lanes {
			la.Status = StatusOf(la, a.Syncing, seat.Departed)
			la.StatusLabel = la.Status.Label(LaneName(lane))
			a.Seats[i].Lanes[lane] = la
		}
	}
}
