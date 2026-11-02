package types

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
)

// THE AUDIT NAMES THE AUTHOR AS THE ACTOR — the name iam.ActorFor gives the
// caller, which is the name every record the call wrote carries — whatever the
// envelope's source says: the source is the kind of writer every runtime audit
// record shares, and a record whose actor fell back to it would read "operator
// did this" for every person in the company. The credential is beside it in
// operator_id and never in its place, so a person bound to a seat reads as
// that seat rather than as the session or token they happened to act through.
func TestARuntimeAuditRecordIsItsAuthors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload events.Payload
		actor   string
		summary string
	}{{
		name: "a bound person's write",
		payload: NewOperatorActed(OperatorActed{ActorName: "jane-founder", ActorKind: "human",
			OperatorID: "session:0f3c", Tool: "create_work_item", Outcome: AuditApplied}),
		actor:   "jane-founder",
		summary: "jane-founder ran create_work_item: applied",
	}, {
		name: "an unbound credential's refused write",
		payload: NewOperatorActed(OperatorActed{ActorName: "token:ci", ActorKind: "operator",
			OperatorID: "token:ci", Tool: "update_work_item", Outcome: AuditRefused, Refusal: "not_found"}),
		actor:   "token:ci",
		summary: "token:ci ran update_work_item: refused (not_found)",
	}, {
		name: "a backup that was taken",
		payload: NewBackupRequested(BackupRequested{ActorName: "jane-founder", ActorKind: "human",
			OperatorID: "pat:4b1e", Dir: "/var/backups/one", Outcome: AuditApplied, Streams: 12}),
		actor:   "jane-founder",
		summary: "jane-founder backed up to /var/backups/one (12 streams)",
	}, {
		name: "a backup that failed",
		payload: NewBackupRequested(BackupRequested{ActorName: "token:ops", ActorKind: "operator",
			OperatorID: "token:ops", Dir: "/var/backups/two", Outcome: AuditFailed}),
		actor:   "token:ops",
		summary: "token:ops backup to /var/backups/two failed",
	}, {
		name: "a credential set on a model, named and never valued",
		payload: NewModelActed(ModelActed{ActorName: "jane-founder", ActorKind: "human",
			OperatorID: "session:0f3c", Action: ModelCredential, ProviderKey: "sonnet",
			Names: []string{"ANTHROPIC_API_KEY_SONNET"}, Outcome: AuditApplied}),
		actor:   "jane-founder",
		summary: "jane-founder set the credential of model sonnet (ANTHROPIC_API_KEY_SONNET)",
	}, {
		name: "a credential the vendor refused, on a dry run",
		payload: NewModelActed(ModelActed{ActorName: "jane-founder", ActorKind: "human",
			Action: ModelCredential, ProviderKey: "sonnet", DryRun: true,
			Names: []string{"K"}, Outcome: AuditRefused, Refusal: "credential_refused"}),
		actor:   "jane-founder",
		summary: "jane-founder tested a credential for model sonnet (K): refused (credential_refused)",
	}, {
		name: "a retry",
		payload: NewModelActed(ModelActed{ActorName: "token:ops", ActorKind: "operator",
			Action: ModelRetry, ProviderKey: "default", Outcome: AuditApplied}),
		actor:   "token:ops",
		summary: "token:ops retried model default",
	}, {
		name: "an MCP server restarted on every node",
		payload: NewMCPServerActed(MCPServerActed{ActorName: "jane-founder", ActorKind: "human",
			OperatorID: "session:0f3c", Action: MCPServerRestart, Server: "tracker",
			Outcome: AuditApplied}),
		actor:   "jane-founder",
		summary: "jane-founder restarted MCP server tracker",
	}, {
		name: "one seat's instance of an MCP server, refused",
		payload: NewMCPServerActed(MCPServerActed{ActorName: "token:ops", ActorKind: "operator",
			Action: MCPServerRestart, Server: "tracker", Seat: "cto", Outcome: AuditRefused,
			Refusal: "conflict"}),
		actor:   "token:ops",
		summary: "token:ops restarted MCP server tracker for seat cto: refused (conflict)",
	}, {
		name: "a device sign-in cancelled, said apart from its start",
		payload: NewModelActed(ModelActed{ActorName: "jane-founder", ActorKind: "human",
			Action: ModelDeviceSignInCancel, ProviderKey: "codex", Outcome: AuditApplied}),
		actor:   "jane-founder",
		summary: "jane-founder cancelled the device sign-in for model codex",
	}, {
		name: "a device sign-in that expired, as what became of the person's",
		payload: NewModelActed(ModelActed{ActorName: "jane-founder", ActorKind: "human",
			Action: ModelDeviceSignInEnded, ProviderKey: "codex", Status: "expired",
			Outcome: AuditApplied}),
		actor:   "jane-founder",
		summary: "jane-founder's device sign-in for model codex expired before it was approved",
	}, {
		name: "a device sign-in approved whose login was not stored",
		payload: NewModelActed(ModelActed{ActorName: "jane-founder", ActorKind: "human",
			Action: ModelDeviceSignInEnded, ProviderKey: "codex", Status: "approved",
			Outcome: AuditFailed}),
		actor: "jane-founder",
		summary: "jane-founder's device sign-in for model codex was approved, and its login " +
			"was not stored everywhere",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := actorOf(tc.payload, OperatorSource); got != tc.actor {
				t.Errorf("the actor is %q, want the author %q", got, tc.actor)
			}
			if got := summaryOf(tc.payload, OperatorSource); got != tc.summary {
				t.Errorf("summary = %q, want %q", got, tc.summary)
			}
		})
	}
}

// A REFUSED OR FAILED CALL IS MARKED FAILED, and nothing else is: the flag is
// what the event store tags a row by, and the log's failure filter is how an
// operator finds the call that did not happen. It is derived from the outcome
// rather than set beside it, so the two cannot disagree.
func TestOnlyARefusedOrFailedCallIsMarkedFailed(t *testing.T) {
	t.Parallel()
	all := []AuditOutcome{AuditApplied, AuditPending, AuditUnknown, AuditRefused, AuditFailed}
	for _, o := range all {
		if !o.Valid() {
			t.Errorf("%q is not a valid outcome", o)
		}
		want := o == AuditRefused || o == AuditFailed
		if got := NewOperatorActed(OperatorActed{Outcome: o}).Failed; got != want {
			t.Errorf("an act that was %s is failed=%v, want %v", o, got, want)
		}
		if got := NewBackupRequested(BackupRequested{Outcome: o}).Failed; got != want {
			t.Errorf("a backup that was %s is failed=%v, want %v", o, got, want)
		}
	}
	if AuditOutcome("maybe").Valid() || slices.Contains(all, "") {
		t.Error("an outcome outside the five is valid")
	}
}
