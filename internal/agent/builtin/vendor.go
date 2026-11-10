package builtin

import (
	"slices"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// VendorNeed is the vendor permission a built-in needs to do its work (E21):
// the grant the engine minted for the seat, which explains a vendor's refusal
// from the engine's own knowledge, never from the vendor's words. Every filed
// built-in declares one, the engine's own estates' tools as reaching none.
type VendorNeed struct {
	// Vendor names the vendor, empty for a tool that reaches none.
	Vendor string
	// Backend is the backend the tool reaches the vendor on: the company's
	// setting that routes it there.
	Backend string
	// Lane is the access lane whose grant holds the permission: a seat on it
	// holds the permission, one off it does not.
	Lane string
	// Permission and Level say what the seat's credential must hold.
	Permission, Level string
}

// Reaches reports whether the tool reaches a vendor at all.
func (n VendorNeed) Reaches() bool { return n.Vendor != "" }

// HeldBy reports whether seat's minted grant holds the permission, as the
// engine itself granted it: the seat is on the need's access lane.
func (n VendorNeed) HeldBy(seat *org.Role) bool {
	return seat != nil && slices.Contains(seat.AccessLanes, n.Lane)
}

// Refusal is what a vendor's refusal of this tool says, from the declaration.
func (n VendorNeed) Refusal(tool string) string {
	return tool + ": " + n.Vendor + " refused this agent's credential. It needs " + n.Level +
		" access to " + n.Permission + ", which the access the engine holds for it does not include. " +
		"An administrator grants it in the agent's " + n.Vendor + " access; asking again changes nothing until then."
}

var (
	// reachesNoVendor is a built-in the engine's own estates serve.
	reachesNoVendor = VendorNeed{}
	// searchesConfluence is search_knowledge on a Confluence knowledge base,
	// which searches as the seat's own Atlassian credential.
	searchesConfluence = VendorNeed{Vendor: "Confluence", Backend: "confluence",
		Lane: string(seataccess.LaneConfluence), Permission: "the spaces it searches", Level: "read"}
)
