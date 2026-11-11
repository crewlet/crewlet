package seataccess

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
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

	// ObservedAt is when a pass last read the vendor for this lane, a dry
	// run included ([LaneRecord.ObservedAt]); Sync.At is the last writing sync.
	ObservedAt time.Time `json:"observed_at,omitzero"`

	// Status is the lane as one word ([StatusOf]), set once the answer is
	// whole ([Answer.SetStatuses]).
	Status LaneStatus `json:"status"`

	// Plan is what the next sync does for this lane as the document stands.
	Plan []Step `json:"plan"`

	// Tool is whether the seat can use its credential through a tool now.
	Tool ToolState `json:"tool"`

	// ToolReason is why Tool is waiting, in the engine's words, where it
	// waits on the vendor applying a grant ([Settling]); empty otherwise.
	ToolReason string `json:"tool_reason,omitempty"`

	// Administers is whether the lane's setting is an administration grant
	// (D10), for the review's warning.
	Administers bool `json:"administers,omitempty"`

	// Box is what the seat's executor box holds for a CLI there to use the
	// lane ([BoxOf]); nil where the lane's tool is a server.
	Box *Box `json:"box,omitempty"`

	// HandIn is what a person makes at the vendor for this lane, where a
	// person hands its credential in ([HandsIn]); nil elsewhere.
	HandIn *HandIn `json:"hand_in,omitempty"`
}

// HandIn is what a person makes at the vendor for a seat.
type HandIn struct {
	// Name is the name to give it at the vendor, at the seat's desired
	// level. Computed by the engine, never read back from the vendor.
	Name string `json:"name"`
}

// HandInNamer is a [Vendor] whose seats' credentials a person makes and names
// at the vendor: the name to give one seat's, at its desired level. Pure.
type HandInNamer interface {
	HandInName(c *config.Company, seat SeatRef, lane Lane, want Desired) string
}

// HandInOf is the lane's [HandIn] for the seat, or nil where vendor names none.
func HandInOf(v Vendor, c *config.Company, seat SeatRef, lane Lane, want Desired) *HandIn {
	if name := handInName(v, c, seat, lane, want); name != "" {
		return &HandIn{Name: name}
	}
	return nil
}

// handInName is [HandInNamer]'s name for the seat, "" where v names none or
// no document was handed.
func handInName(v Vendor, c *config.Company, seat SeatRef, lane Lane, want Desired) string {
	namer, ok := v.(HandInNamer)
	if !ok || c == nil {
		return ""
	}
	return namer.HandInName(c, seat, lane, want)
}

// Box is a seat's executor box as one lane needs it.
type Box struct {
	// Env is each variable the engine sets there: the credentials the lane's
	// tool reads, then every other, by name.
	Env []BoxLine `json:"env"`

	// Files are the files the engine writes there, by path; omitted where it
	// writes none.
	Files []BoxFileLine `json:"files,omitempty"`
}

// BoxFileLine is one file in a seat's box: at Path, relative to the box's
// home, holding Value, the `${VAR}` reference as configured and never what it
// resolves to; Rewritten where the completion poll keeps it current.
type BoxFileLine struct {
	Path      string `json:"path"`
	Value     string `json:"value"`
	Rewritten bool   `json:"rewritten"`
}

// BoxLine is one variable in a seat's box: Name set to Value, as configured
// and never what it resolves to. A credential holds the seat's own sealed or
// issued name as a whole reference (`${EXPO_TOKEN_QA}`); a setting holds a
// literal or a reference, or one of the box's files' paths from its home
// (`~/.config/crewlet/token`). Tool marks a credential the lane's tool, a CLI
// in the box, reads ([LaneSpec.BoxEnv]).
type BoxLine struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Tool  bool   `json:"tool,omitempty"`
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

	// ManagedBy is [ManagedByOperator] where the company document is the
	// operator's to write: the tab is read-only, and a change is made on
	// the Seat object. Empty where the engine's own surfaces write it.
	ManagedBy string `json:"managed_by,omitempty"`

	// Options are what each lane offers now, for the pickers.
	Options []Options `json:"options"`

	Seats []SeatAnswer `json:"seats"`
}

// ManagedByOperator is [Answer.ManagedBy] for a document the operator
// renders from its custom resources.
const ManagedByOperator = "operator"

// InstallLink is the Slack install seat action's answer: a fresh, single-use
// link a person follows to approve one agent's app. Minted on request and
// never stored or answered on a read.
type InstallLink struct {
	URL string `json:"url"`
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

	// Reviews are the agents the draft changes the access of that have open
	// work to review once it is saved (ADR-0111); absent where not counted.
	Reviews []SeatReview `json:"reviews,omitempty"`
}

// SeatReview is one agent the draft asks to review its open work.
type SeatReview struct {
	Handle string `json:"handle"`
	// Open is how many distinct open items it has: questions still open, runs
	// waiting on a person, and work items in progress or blocked. AtLeast is
	// that the tracker stopped counting at its ceiling, so Open is a floor.
	Open    int  `json:"open"`
	AtLeast bool `json:"at_least,omitempty"`
	// Changes are the change lines it will be told, at most twenty.
	Changes []string `json:"changes"`
}

// CredentialAnswer is the answer to a credential a person pasted for a seat:
// the sentence they read and the tab as it now stands.
type CredentialAnswer struct {
	Verdict setup.Verdict `json:"verdict"`
	Access  Answer        `json:"access"`
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

	// ActionRelease keeps at the vendor an account a step waits for a person
	// to delete ("Keep the robot at Expo"): the step closes and Crewlet stops
	// tracking the account.
	ActionRelease SeatAction = "release"
)

// Valid reports whether a is an action this build takes.
func (a SeatAction) Valid() bool {
	return slices.Contains([]SeatAction{
		ActionRotate, ActionRemove, ActionKeep, ActionRestore, ActionRetry, ActionDone,
		ActionRelease,
	}, a)
}

// RefusalField is the part of a seat lane's setting an access refusal is
// about, so the Agents tab can place the refusal beside that control.
type RefusalField string

// The parts of a seat lane's setting a refusal names.
const (
	FieldLevel       RefusalField = "level"
	FieldPermissions RefusalField = "permissions"
	FieldScope       RefusalField = "scope"
)

// Valid reports whether f is a part this build names.
func (f RefusalField) Valid() bool {
	switch f {
	case FieldLevel, FieldPermissions, FieldScope:
		return true
	}
	return false
}

// OfferRefusal is a seat lane's setting the vendor does not offer now: the
// fields of an `access_not_offered` refusal, beside the envelope's own.
type OfferRefusal struct {
	Seat   string       `json:"seat"`
	Lane   Lane         `json:"lane"`
	Field  RefusalField `json:"field"`
	Detail string       `json:"detail"`

	// Offered is what the vendor offers instead, where a list says it;
	// Required is the level a set must hold ([Options.RequiredLevel]).
	Offered  []string `json:"offered,omitempty"`
	Required string   `json:"required,omitempty"`
}
