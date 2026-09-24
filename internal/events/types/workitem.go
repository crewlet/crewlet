package types

// A turn is charged to ONE work item, and this is the vocabulary that names it
// on the wire: the item, the tracker it lives in, and why the engine believes
// the turn was on it. The resolution itself — which rule answers first, and
// when a turn is on nothing — belongs to the engine that dispatches the turn;
// what lives here is the shape every event, row and screen reads it as.

// WorkBackend names the tracker a work item lives in.
//
// NOT the company's configured tracker (config.TrackerBackend), and the
// difference is the reason this is its own type. That setting says where a
// company keeps ITS work; this says where one ITEM lives. A company on the
// native tracker still has seats woken by a GitHub issue or a GitLab one, and
// a turn on that issue is on a GitHub item whatever the tracker setting reads —
// so the set here is every place a turn's item can be, which is wider than the
// set of trackers a company can choose.
//
// A plain string on the wire, so a backend a newer node names decodes and
// round-trips here as a value rather than failing; [WorkBackend.Valid] is how a
// reader asks whether this build knows it.
type WorkBackend string

// The places a work item can live.
const (
	// WorkNative is the engine's own tracker.
	WorkNative WorkBackend = "native"

	// WorkJira is a Jira issue.
	WorkJira WorkBackend = "jira"

	// WorkGitHub is a GitHub issue or pull request.
	WorkGitHub WorkBackend = "github"

	// WorkGitLab is a GitLab issue or merge request.
	WorkGitLab WorkBackend = "gitlab"
)

// Valid reports whether this build knows b.
func (b WorkBackend) Valid() bool {
	switch b {
	case WorkNative, WorkJira, WorkGitHub, WorkGitLab:
		return true
	}
	return false
}

// WorkItem names the one work item a turn is charged to, on the wire as
// `work_item{backend, id, key, project}`.
//
// THE ID IS THE IDENTITY and the key is the label. A key is what a person reads
// ("ENG-412") and what a project rename or a move changes; the id is what every
// join, every dedupe and every "is this the same item" compare uses, because it
// is the one of the four that survives both.
type WorkItem struct {
	// Backend is the tracker the item lives in.
	Backend WorkBackend `json:"backend"`
	// ID is the item's identity inside that tracker: the value it never
	// hands to another item and does not change when the item is renamed
	// or moved — a native task id, a Jira issue id, and on a code host the
	// repository's or project's numeric id joined to the item's number:
	// `<repository id>#<number>` on GitHub, `<project id>!<iid>` for a
	// GitLab merge request and `<project id>#<iid>` for an issue. NOT the
	// code host's own global id, because GitHub gives a pull request two
	// of those — an issue comment carries the issue face's and a review the
	// pull request's — so one pull request would be two items depending on
	// which event woke the turn. Never a bare number either, which is only
	// unique inside one repository and would make two different items one
	// [WorkItem.Ref].
	ID string `json:"id"`
	// Key is the human label the tracker shows — "ENG-412", "acme/api#88" —
	// as it read when the turn named the item.
	Key string `json:"key"`
	// Project is the container the item was in when the turn named it: a
	// native project key, a Jira project, a repository path.
	Project string `json:"project"`
}

// Ref is the item's identity across every backend, `<backend>:<id>`.
//
// BACKEND-QUALIFIED because an id alone is not unique: a native task id and a
// Jira issue id are drawn from different spaces, and nothing stops two of them
// being the same string. Everything that has to decide whether two turns were
// on the same item — a set of what a turn wrote, a per-item rollup — compares
// this, never the bare id and never the key.
func (w WorkItem) Ref() string { return string(w.Backend) + ":" + w.ID }

// WorkItemBasis is WHY a turn is charged to its work item, on the wire as
// `work_item_basis`. Each value is a rule, in the order the engine tries them.
//
// Carried beside the item rather than left implicit because the rules differ
// in how much they prove: a trigger that names an item is the item by
// construction, while a sole write is an inference from what the turn did. A
// reader weighing a per-item figure needs to know which kind of claim it is
// adding up.
type WorkItemBasis string

// The rules that can charge a turn to an item.
const (
	// BasisTrigger: the event that woke the turn names the item — a
	// tracker assignment, a comment, a vendor issue's webhook.
	BasisTrigger WorkItemBasis = "trigger"

	// BasisAskedBy: the turn answers a colleague's ask, and inherits the
	// item the ASKING turn was on. Help given on a task is work on it.
	BasisAskedBy WorkItemBasis = "asked_by"

	// BasisResume: the turn re-enters a parked run, which recorded the item
	// it was on when it parked.
	BasisResume WorkItemBasis = "resume"

	// BasisSoleWrite: nothing at dispatch named an item, and the turn wrote
	// to exactly one. Resolved at completion only, because only then is
	// "exactly one" a fact rather than a guess.
	BasisSoleWrite WorkItemBasis = "sole_write"
)

// Valid reports whether this build knows b.
func (b WorkItemBasis) Valid() bool {
	switch b {
	case BasisTrigger, BasisAskedBy, BasisResume, BasisSoleWrite:
		return true
	}
	return false
}
