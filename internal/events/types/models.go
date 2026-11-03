package types

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
)

func init() {
	events.Register[ModelActed]()
}

// ModelAction is which gesture on a model a [ModelActed] records: the verbs of
// the model routes (internal/api/llmapi), each its own route.
type ModelAction string

// The model gestures.
const (
	// ModelCheck is the doctor over the API: a model's checks run on the
	// answering node, a few tokens of a real call.
	ModelCheck ModelAction = "check"
	// ModelRetry probes a sitting-out model once and, if it answers,
	// clears its sit-out on every node; ModelClear clears it with no call.
	ModelRetry ModelAction = "retry"
	ModelClear ModelAction = "clear"
	// ModelCredential tests candidate credential values and, unless it
	// was a dry run, seals them and re-activates what reads them.
	ModelCredential ModelAction = "credential"
	// ModelDeviceSignIn is a Codex device sign-in started.
	ModelDeviceSignIn ModelAction = "device_sign_in"
	// ModelDeviceSignInCancel is a running device sign-in cancelled.
	ModelDeviceSignInCancel ModelAction = "device_sign_in_cancel"
	// ModelDeviceSignInEnded is how a device sign-in ended (approved,
	// failed, expired or cancelled), published by the node that ran it
	// and attributed to whoever started it: the moment a login is stored
	// is the one an audit is opened to find, and no route call marks it.
	ModelDeviceSignInEnded ModelAction = "device_sign_in_ended"
	// ModelHostLoginCheck reads a host harness login's status on the
	// answering node.
	ModelHostLoginCheck ModelAction = "host_login_check"
)

// ModelActions is every action, for the gate holding the dashboard's copy.
var ModelActions = []ModelAction{ModelCheck, ModelRetry, ModelClear, ModelCredential,
	ModelDeviceSignIn, ModelDeviceSignInCancel, ModelDeviceSignInEnded, ModelHostLoginCheck}

// Valid reports whether a is an action this build records.
func (a ModelAction) Valid() bool { return slices.Contains(ModelActions, a) }

// ModelActed is one call to a model route (`/llm/providers/{key}/...` and
// `/llm/device-sign-ins/{id}`): who made it, which gesture on which entry, and
// what became of it, refused ones included.
//
// ITS OWN EVENT, not an [OperatorActed]: that one's `tool` names an act
// catalogue tool, and these are REST verbs with a record of their own, the
// precedent [BackupRequested] set. And beside the records the write already
// makes: a credential write's secret rows name who stored them and its
// re-activation is a revision, but a check, a retry or a sign-in leaves
// nothing anywhere else, and a refused credential leaves nothing at all.
//
// NAMES, NEVER A VALUE: a credential call names the variables it tested or
// sealed, which is what an audit is opened to find, and the value would be the
// leak.
//
// THE NODE IS THE ENVELOPE'S, as a backup's is: the route publishes this from
// the node that answered, which is the node the queue stamps as the origin,
// and a payload field under that key is dropped on the way out.
type ModelActed struct {
	// ActorName, ActorKind and OperatorID are who acted, in the three
	// halves of [OperatorActed.ActorName].
	ActorName  string `json:"actor"`
	ActorKind  string `json:"actor_kind"`
	OperatorID string `json:"operator_id"`

	Action      ModelAction `json:"action"`
	ProviderKey string      `json:"provider_key"`
	// Names are the secret NAMES a credential call named.
	Names  []string `json:"names,omitempty"`
	DryRun bool     `json:"dry_run,omitempty"`
	// Outcome is `applied` for a gesture that did what it was asked,
	// `refused` for one the route turned down for a stated reason (its
	// code in Refusal), and `failed` for one that broke without one.
	Outcome AuditOutcome `json:"outcome"`
	Refusal string       `json:"refusal,omitempty"`
	// RevisionID is the revision a credential write's re-activation
	// activated, empty when it made none.
	RevisionID string `json:"revision_id,omitempty"`
	// Status is how a device sign-in ended, on [ModelDeviceSignInEnded]
	// alone: approved, failed, expired or cancelled.
	Status string `json:"status,omitempty"`
	Failed bool   `json:"failed,omitempty"`
}

// EventType is the "model_acted" wire type.
func (ModelActed) EventType() string { return "model_acted" }

// Actor is who acted on the model.
func (e ModelActed) Actor() string { return e.ActorName }

// SummaryFor says what was done to which model and how it ended.
func (e ModelActed) SummaryFor(actor string) string {
	what := string(e.Action)
	switch e.Action {
	case ModelCheck:
		what = "checked"
	case ModelRetry:
		what = "retried"
	case ModelClear:
		what = "cleared the sit-out of"
	case ModelCredential:
		what = "set the credential of"
		if e.DryRun {
			what = "tested a credential for"
		}
	case ModelDeviceSignIn:
		what = "started a device sign-in for"
	case ModelDeviceSignInCancel:
		what = "cancelled the device sign-in for"
	case ModelDeviceSignInEnded:
		// THE SESSION ENDED, not the person's gesture: said as what
		// became of the sign-in they started.
		text := "device sign-in for model " + e.ProviderKey + " " + deviceEndWords(e.Status)
		if e.Outcome == AuditFailed && e.Status == "approved" {
			text += ", and its login was not stored everywhere"
		}
		return lead(actorPossessive(actor), text)
	case ModelHostLoginCheck:
		what = "checked the host login of"
	}
	text := what + " model " + e.ProviderKey
	if len(e.Names) > 0 {
		text += " (" + strings.Join(e.Names, ", ") + ")"
	}
	if e.Outcome != AuditApplied {
		text += ": " + outcomeWords(e.Outcome, e.Refusal)
	}
	return lead(actor, text)
}

// NewModelActed builds the audit record for one model gesture, with [Failed]
// derived from the outcome.
func NewModelActed(e ModelActed) ModelActed {
	e.Failed = e.Outcome.failed()
	return e
}

// deviceEndWords is how a device sign-in ended, as a sentence ends.
func deviceEndWords(status string) string {
	switch status {
	case "approved":
		return "was approved"
	case "expired":
		return "expired before it was approved"
	case "cancelled":
		return "was cancelled"
	case "":
		return "ended"
	}
	return status
}

// actorPossessive is actor's, for a phrase about something they started;
// empty stays empty, so [lead] capitalises the phrase.
func actorPossessive(actor string) string {
	if actor == "" {
		return ""
	}
	return actor + "'s"
}
