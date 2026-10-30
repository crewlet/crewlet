package seataccess

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
)

// The access routes' wire shapes, declared beside the types they carry so the
// routes and the reconcile cannot describe one lane two ways:
//
//	GET  /setup/integrations/{kind}/access          every seat: desired, observed, last sync, plan, tool
//	POST /setup/integrations/{kind}/access/plan     what a draft would do, writing nothing
//	PUT  /setup/integrations/{kind}/access          a draft saved as one revision, then synced
//	POST /setup/integrations/{kind}/access/seats/{handle}/{lane}/{action}
//
// REVISIONS ARE THE DOCUMENT'S ENTITY TAGS, as every other config write takes
// them: an answer carries the tag it was read at, and a PUT sends it back as
// If-Match, so a save over somebody else's change is refused rather than lost.

// LaneAnswer is one seat's lane on the access route.
type LaneAnswer struct {
	Desired  Desired     `json:"desired"`
	Observed Observed    `json:"observed"`
	Sync     *SyncResult `json:"sync,omitempty"`

	// Plan is what the next sync does for this lane as the document stands.
	Plan []Step `json:"plan"`

	// Tool is whether the seat can use its credential through a tool now.
	Tool ToolState `json:"tool"`

	// Administers is whether the lane's setting is an administration grant
	// (D10), for the review's warning.
	Administers bool `json:"administers,omitempty"`
}

// SeatAnswer is one seat on the access route.
type SeatAnswer struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`

	// Departed is a seat the document no longer has whose account the ledger
	// still records, listed so a person can remove it.
	Departed bool `json:"departed,omitempty"`

	Lanes map[Lane]LaneAnswer `json:"lanes"`
}

// Notice is a company-level notice on one integration's Agents tab, such as
// the migration's danger notice that a company credential may hold a seat's
// value (D12).
type Notice struct {
	Tone  string `json:"tone"`
	Title string `json:"title"`
	Body  string `json:"body"`

	// ReplaceKey opens the integration's own key replacement.
	ReplaceKey bool `json:"replace_key,omitempty"`
}

// Answer is the access route's GET.
type Answer struct {
	Kind     integration.Kind `json:"kind"`
	Revision string           `json:"revision"`

	// Syncing is a sync in flight for this kind, started at StartedAt.
	Syncing   bool      `json:"syncing"`
	StartedAt time.Time `json:"started_at,omitzero"`

	// LastSyncAt is when the last sync for this kind finished.
	LastSyncAt time.Time `json:"last_sync_at,omitzero"`

	Notice *Notice `json:"notice,omitempty"`

	// Options are what each lane offers now, for the pickers.
	Options []Options `json:"options"`

	Seats []SeatAnswer `json:"seats"`
}

// SeatPlan is one seat's lane in a plan answer.
type SeatPlan struct {
	Handle string `json:"handle"`
	Lane   Lane   `json:"lane"`
	Steps  []Step `json:"steps"`

	// StepUp is whether saving this seat's draft needs a fresh step-up
	// (D10).
	StepUp bool `json:"step_up,omitempty"`
}

// PlanAnswer is the plan route's answer: every seat's lane the draft changes.
type PlanAnswer struct {
	Kind     integration.Kind `json:"kind"`
	Revision string           `json:"revision"`
	Plans    []SeatPlan       `json:"plans"`

	// StepUp is whether saving the whole draft needs a fresh step-up.
	StepUp bool `json:"step_up,omitempty"`
}

// Draft is the body of the plan and save routes: each seat's lanes as the
// person left them. A seat or lane it leaves out keeps its setting.
type Draft struct {
	Seats map[string]map[Lane]Desired `json:"seats"`
}

// SeatAction is one action on one seat's lane. The values are the wire's
// (the last path segment).
type SeatAction string

const (
	// ActionRotate mints a new credential and revokes the old.
	ActionRotate SeatAction = "rotate"

	// ActionRemove removes the account; refused while the seat is on.
	ActionRemove SeatAction = "remove"

	// ActionKeep writes the vendor's state into the document as a new
	// revision: "Keep the vendor's setting" after drift, "Keep off" after a
	// disable by hand.
	ActionKeep SeatAction = "keep"

	// ActionRestore turns back on an account disabled by hand ("Turn back
	// on"), or restores drift now rather than on the next pass.
	ActionRestore SeatAction = "restore"

	// ActionRetry runs the sync again for the lane.
	ActionRetry SeatAction = "retry"

	// ActionDone re-reads the vendor after a person took a step there.
	ActionDone SeatAction = "done"
)

// Valid reports whether a is an action this build takes.
func (a SeatAction) Valid() bool {
	return slices.Contains([]SeatAction{
		ActionRotate, ActionRemove, ActionKeep, ActionRestore, ActionRetry, ActionDone,
	}, a)
}
