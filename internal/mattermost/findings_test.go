package mattermost_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
)

// theOnlySeat is the company every case here runs.
func theOnlySeat() []*org.Role {
	return []*org.Role{chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership")}
}

// THE PREFLIGHT REACHES THE LOOP AND NOT ONLY THE TERMINAL.
//
// `crewlet mattermost provision` prints [mattermost.Result.Notes] to whoever
// ran it; the reconcile loop reads [mattermost.Result.Findings] and nothing
// else — engine.mattermostPass returns res.Findings() and drops the notes. So
// a preflight that spoke only in notes told the person at the terminal that
// this credential cannot write and told the loop that Mattermost was READY.
//
// A CONVERGED COMPANY IS WHERE THIS BITES. Nothing is written on a pass that
// needs nothing, so the refusal never surfaces as an error: the company
// reports ready for ever, and the first anybody hears of it is the day a
// tenth seat is added and every pass from then on fails.
func TestAnUnderPrivilegedAdminTokenIsReportedToTheLoop(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.adminRoles = "system_user"

	res, err := reconcileChat(t, srv, newChatSink(), theOnlySeat())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	findings := res.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the refused administrator token", findings)
	}
	got := findings[0]
	// REJECTED, NOT MISSING. The ${VAR} resolved; what it names is an
	// account the instance will not let write. The two kinds were split on
	// exactly that, and they send a person to different places.
	if got.Kind != integration.FindingCredentialRejected {
		t.Errorf("kind = %q, want %q", got.Kind, integration.FindingCredentialRejected)
	}
	if got.Subject != "integrations.mattermost.provisioning.admin_token" {
		t.Errorf("subject = %q, want the field a person changes", got.Subject)
	}
	if _, actor := got.Kind.Verdict(); !actor.WaitsOnAPerson() {
		t.Errorf("owed by %s, so nobody is asked to act", actor)
	}
	if !strings.Contains(got.Detail, "system administrator") {
		t.Errorf("detail does not say what is wrong: %q", got.Detail)
	}
	// AND THE TERMINAL STILL HEARS IT. The two readers are different
	// people, not two spellings of one channel.
	if !strings.Contains(strings.Join(res.Notes, "\n"), "system administrator") {
		t.Errorf("the printed notes lost it: %v", res.Notes)
	}
}

// AN INSTANCE SWITCH NO CREDENTIAL GETS PAST is the Mattermost
// administrator's, and it is reported as theirs.
func TestADisabledInstanceSettingIsReportedToTheLoop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ key, says string }{
		{"EnableBotAccountCreation", "no bot account can be created"},
		{"EnableUserAccessTokens", "none of them can connect"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()
			srv := newChatServer()
			srv.settings[tc.key] = "false"

			res, err := reconcileChat(t, srv, newChatSink(), theOnlySeat())
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			findings := res.Findings()
			if len(findings) != 1 {
				t.Fatalf("findings = %+v, want the disabled setting", findings)
			}
			got := findings[0]
			if got.Kind != integration.FindingApprovalRequired {
				t.Errorf("kind = %q, want %q", got.Kind,
					integration.FindingApprovalRequired)
			}
			if got.Subject != "ServiceSettings."+tc.key {
				t.Errorf("subject = %q, want the key somebody searches the "+
					"System Console for", got.Subject)
			}
			if _, actor := got.Kind.Verdict(); actor != integration.ActorAdmin {
				t.Errorf("owed by %s; the switch is in somebody's System "+
					"Console, not in this deployment's config", actor)
			}
			if !strings.Contains(got.Detail, tc.says) {
				t.Errorf("detail does not say what it stops: %q", got.Detail)
			}
		})
	}
}

// CANNOT TELL IS NOT CONFIRMED WRONG.
//
// An instance that will not answer /users/me says nothing about whether the
// provisioning account is an administrator. Reporting it would put every
// company behind a blinking server into a blocked phase somebody is sent to
// go and fix — and the run is about to make real calls that report the same
// reachability problem with far more context, which is why it stays a note.
func TestAnUnreadableProvisioningAccountIsNotReportedAsRefused(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.refuseIdentity = true

	res, err := reconcileChat(t, srv, newChatSink(), theOnlySeat())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if findings := res.Findings(); len(findings) != 0 {
		t.Fatalf("findings = %+v; an account this run could not ask about was "+
			"reported as one it asked and was refused", findings)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "could not confirm") {
		t.Errorf("and it was not even mentioned: %v", res.Notes)
	}
}

// A HEALTHY CONVERGED COMPANY REPORTS NOTHING, which is what converged means
// — and what every clause of the conformance suite that walks a findings list
// relies on being true here rather than accidentally.
func TestAConvergedCompanyReportsNoFinding(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	if _, err := reconcileChat(t, srv, newChatSink(), theOnlySeat()); err != nil {
		t.Fatalf("first run: %v", err)
	}
	sink := newChatSink()
	first, err := reconcileChat(t, srv, sink, theOnlySeat())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if findings := first.Findings(); len(findings) != 0 {
		t.Fatalf("a converged company reported %+v", findings)
	}
}

// A SUCCESSFUL DECOMMISSION IS NOT OUTSTANDING WORK.
//
// [mattermost.Result.Decommissioned] is exactly the seats the company
// document NO LONGER HAS: somebody passed -decommission and the run disabled
// their bots, deliberately and successfully. Reported as identity_missing —
// which is PhaseProvisioning / ActorEngine, rendering as "the engine is
// creating agent identities" — a destructive run that did precisely what was
// asked classified as work in flight, for handles that will never be created,
// on a row nothing ever clears.
func TestASuccessfulDecommissionIsNotReportedAsOutstandingWork(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	if _, err := reconcileChat(t, srv, newChatSink(), []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
		chatSeat("CTO", "${MM_TOKEN_CTO}", "eng"),
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := reconcileChatWith(t, srv, newChatSink(), theOnlySeat(),
		func(o *mattermost.Options) { o.Decommission = true })
	if err != nil {
		t.Fatalf("decommission: %v", err)
	}
	if len(res.Decommissioned) != 1 {
		t.Fatalf("Decommissioned = %v, want the departed seat", res.Decommissioned)
	}
	if findings := res.Findings(); len(findings) != 0 {
		t.Fatalf("the sweep that did what was asked reported %+v", findings)
	}
	// AND THE REPORT AN OPERATOR READS SAYS SO: ready, not provisioning.
	if report := integration.Classify(res.Findings()); report.Phase != integration.PhaseReady {
		t.Errorf("phase = %q, want ready", report.Phase)
	}
}

// A NODE WITH NO KEYRING STILL SHORT-CIRCUITS EVERYTHING ELSE. It created no
// account and read no instance, so it has nothing else it could honestly say.
func TestANodeWithNoKeyringReportsOnlyThat(t *testing.T) {
	t.Parallel()
	res := &mattermost.Result{
		NoKeyring: true,
		NotAdmin:  "this should never be reached",
		Disabled:  []mattermost.DisabledSetting{{Key: "EnableBotAccountCreation"}},
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingCredentialMissing {
		t.Fatalf("findings = %+v, want only the missing keyring", findings)
	}
}
