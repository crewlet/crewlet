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
