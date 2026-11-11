package seataccess

import (
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
)

// LaneView is one agent's lane at one integration as a person reads its
// Agents tab row, every value worded from the access answer by the vendor's own
// names. It is what `GET /setup/integrations/{kind}/access/seats/{handle}/{lane}`
// answers, so a reader that shows one agent's access words it the engine's way.
//
// THE GRANT AND WHAT THE VENDOR HOLDS, SIDE BY SIDE. A credential is narrowed
// to the grant in Crewlet, so "In sync" compares the grant's own permissions
// only; what GitHub's app holds past the grant is Unused, never drift.
type LaneView struct {
	Kind        integration.Kind `json:"kind"`
	Lane        Lane             `json:"lane"`
	Integration string           `json:"integration"`
	Handle      string           `json:"handle"`
	Name        string           `json:"name"`

	// Status is the row's word ([StatusOf]), and StatusLabel it in the tab's
	// words.
	Status      LaneStatus `json:"status"`
	StatusLabel string     `json:"status_label"`

	// Grant is the seat's grant in Crewlet; Holds what its credential holds
	// at the vendor now (for GitHub, what its tokens carry). AppHolds is the
	// permissions GitHub's app is registered with, and Unused those of them
	// the grant leaves out or holds at a lower level.
	Grant    string `json:"grant"`
	Holds    string `json:"holds,omitempty"`
	AppHolds string `json:"app_holds,omitempty"`
	Unused   string `json:"unused,omitempty"`

	Scope   string    `json:"scope,omitempty"`
	Account string    `json:"account,omitempty"`
	Tool    ToolState `json:"tool"`

	// CheckedAt is when a pass last read the vendor for this lane, a dry run
	// included; AppliedAt when a sync last wrote it, Outcome how that ended
	// and Refusal the vendor's reason with its remedy where it failed.
	CheckedAt time.Time `json:"checked_at,omitzero"`
	AppliedAt time.Time `json:"applied_at,omitzero"`
	Outcome   string    `json:"outcome,omitempty"`
	Refusal   string    `json:"refusal,omitempty"`

	// Plan is what the next sync does for this lane as the document stands.
	Plan []Step `json:"plan"`
}

// ViewOf is the lane of the agent seat with handle in answer, worded, or false
// where the answer lists no such seat or the seat has left the document.
func ViewOf(answer Answer, handle string, lane Lane) (LaneView, bool) {
	var seat *SeatAnswer
	for i := range answer.Seats {
		if s := &answer.Seats[i]; s.Handle == handle && !s.Departed {
			seat = s
		}
	}
	if seat == nil || !lane.Valid() {
		return LaneView{}, false
	}
	var offer Options
	for _, o := range answer.Options {
		if o.Lane == lane {
			offer = o
		}
	}
	la := seat.Lanes[lane]
	title := LaneName(lane)
	want, have := la.Desired, la.Observed
	out := LaneView{Kind: answer.Kind, Lane: lane, Integration: title, Handle: seat.Handle,
		Name:   SeatRef{Handle: seat.Handle, Name: seat.Name}.Label(),
		Status: StatusOf(la, answer.Syncing, false), Account: have.Account, Tool: la.Tool,
		CheckedAt: la.ObservedAt, Plan: append([]Step{}, la.Plan...)}
	out.StatusLabel = out.Status.Label(title)
	switch {
	case !want.Enabled:
		out.Grant = "Off"
	case len(offer.Permissions) > 0:
		out.Grant = permissionWords(offer, want.Permissions)
	case want.Level != "":
		out.Grant = levelName(offer, want.Level)
	default:
		out.Grant = "On"
	}
	switch {
	case !have.Exists:
		out.Holds = "No account yet"
	case len(offer.Permissions) > 0:
		out.Holds = permissionWords(offer, have.Permissions)
		if len(have.Ceiling) > 0 {
			out.AppHolds = permissionWords(offer, have.Ceiling)
			out.Unused = unusedWords(offer, want.Permissions, have.Ceiling)
		}
	case have.Level != "":
		out.Holds = levelName(offer, have.Level)
	}
	if want.Enabled && (offer.Scoped || len(want.Scope) > 0) {
		out.Scope = scopeWords(offer, want.Scope)
	}
	if s := la.Sync; s != nil {
		out.AppliedAt, out.Outcome = s.At, outcomeWords(s.Outcome)
		if s.Error != nil {
			out.Refusal = s.Error.Said
			if s.Error.Remedy != "" {
				out.Refusal += " (" + s.Error.Remedy + ")"
			}
		}
	}
	return out, true
}

// permissionWords is GitHub permissions in the catalogue's order and names
// ("Contents Read and write, Issues Read-only").
func permissionWords(offer Options, held Permissions) string {
	var parts []string
	for _, p := range offer.Permissions {
		if access := held[p.Key]; access != "" {
			parts = append(parts, p.Name+" "+access.Label())
		}
	}
	if len(parts) == 0 {
		return "No permissions"
	}
	return strings.Join(parts, ", ")
}

// unusedWords is each permission the app holds at a higher level than the
// grant does, at the app's level.
func unusedWords(offer Options, grant, ceiling Permissions) string {
	var parts []string
	for _, p := range offer.Permissions {
		if ceiling[p.Key].Rank() > grant[p.Key].Rank() {
			parts = append(parts, p.Name+" "+ceiling[p.Key].Label())
		}
	}
	return strings.Join(parts, ", ")
}

// levelName is a stored level by the vendor's names for it: one level, or
// each of a set; the id where the offer names none.
func levelName(offer Options, id string) string {
	if levels, ok := offer.LevelsOf(id); ok {
		names := make([]string, 0, len(levels))
		for _, l := range levels {
			names = append(names, l.Name)
		}
		return strings.Join(names, ", ")
	}
	return id
}

// scopeWords is a scope by the vendor's names, or the vendor's default reach.
func scopeWords(offer Options, scope []string) string {
	if len(scope) == 0 {
		return "Default reach"
	}
	named := make([]string, 0, len(scope))
	for _, entry := range scope {
		if name := strings.TrimSpace(offer.ScopeNames[entry]); name != "" {
			entry = name
		}
		named = append(named, entry)
	}
	return strings.Join(named, ", ")
}

// outcomeWords is how the last writing sync ended.
func outcomeWords(o Outcome) string {
	switch o {
	case OutcomeOK:
		return "Applied"
	case OutcomeFailed:
		return "Refused"
	case OutcomeWaiting:
		return "Waiting on a person"
	}
	return string(o)
}
