package types

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
)

// THE AUDIT NAMES THE CREDENTIAL AS THE ACTOR, whatever the envelope's source
// says: the source is the kind of writer every runtime audit record shares,
// and a record whose actor fell back to it would read "operator did this" for
// every person in the company.
func TestARuntimeAuditRecordIsTheCredentialsAndNamesThePerson(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload events.Payload
		summary string
	}{{
		name: "a bound person's write",
		payload: NewOperatorActed(OperatorActed{OperatorID: "founder", ActorSeat: "jane-founder",
			Tool: "create_work_item", Outcome: AuditApplied}),
		summary: "founder (jane-founder) ran create_work_item: applied",
	}, {
		name: "an unbound credential's refused write",
		payload: NewOperatorActed(OperatorActed{OperatorID: "ci",
			Tool: "update_work_item", Outcome: AuditRefused, Refusal: "not_found"}),
		summary: "ci ran update_work_item: refused (not_found)",
	}, {
		name: "a backup that was taken",
		payload: NewBackupRequested(BackupRequested{OperatorID: "founder", ActorSeat: "jane-founder",
			Dir: "/var/backups/one", Outcome: AuditApplied, Streams: 12}),
		summary: "founder (jane-founder) backed up to /var/backups/one (12 streams)",
	}, {
		name: "a backup that failed",
		payload: NewBackupRequested(BackupRequested{OperatorID: "founder",
			Dir: "/var/backups/two", Outcome: AuditFailed}),
		summary: "founder backup to /var/backups/two failed",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := actorOf(tc.payload, OperatorSource); got != "founder" && got != "ci" {
				t.Errorf("the actor is %q, want the credential", got)
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
