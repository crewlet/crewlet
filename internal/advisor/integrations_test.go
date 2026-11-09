package advisor_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/integration"
)

func reconciled(kind integration.Kind, findings ...integration.Finding) integration.State {
	return integration.State{Kind: kind, Findings: findings}
}

// WHO ACTS IS THE KIND'S OWN VERDICT. A refused credential and an app waiting
// for approval are the owner's and block; a delivery path that cannot be set up
// costs; an account the engine is still creating is the engine's, never a to-do.
func TestAnIntegrationFindingIsTheOwnersOnlyWhereItsVerdictSaysSo(t *testing.T) {
	t.Parallel()
	got := advisor.FromIntegrations([]integration.State{
		reconciled(integration.KindDatadog, integration.Finding{Kind: integration.FindingCredentialRejected,
			Detail: "The key pair was refused.", Remedy: "Replace the credential."}),
		reconciled(integration.KindGitHub,
			integration.Finding{Kind: integration.FindingApprovalRequired, Title: "GitHub is waiting for you",
				ActionURL: "https://github.com/settings/installations/1"},
			integration.Finding{Kind: integration.FindingIngressBlocked, Detail: "No public address."},
			integration.Finding{Kind: integration.FindingIdentityMissing}),
	})
	want := map[advisor.Class]struct {
		severity advisor.Severity
		actor    advisor.Actor
		fix      advisor.FixKind
	}{
		"integration.credential_rejected": {advisor.SeverityBlocks, advisor.ActorOwner, advisor.FixExternalStep},
		"integration.approval_required":   {advisor.SeverityBlocks, advisor.ActorOwner, advisor.FixExternalStep},
		"integration.ingress_blocked":     {advisor.SeverityCosts, advisor.ActorOwner, advisor.FixExternalStep},
		"integration.identity_missing":    {advisor.SeverityNote, advisor.ActorEngine, advisor.FixNone},
	}
	if len(got) != len(want) {
		t.Fatalf("findings = %+v, want one per reconcile finding", got)
	}
	for _, f := range got {
		w, ok := want[f.Class]
		if !ok || f.Severity != w.severity || f.Actor != w.actor || f.Fix.Kind != w.fix {
			t.Errorf("%s = %s/%s/%s, want %+v", f.Class, f.Severity, f.Actor, f.Fix.Kind, w)
		}
		if f.Title == "" || f.Fix.Sentence == "" || f.Source != advisor.SourceReconcile {
			t.Errorf("%s carries no title or fix sentence, or the wrong source: %+v", f.Class, f)
		}
		if f.Class == "integration.approval_required" &&
			(f.Fix.URL != "https://github.com/settings/installations/1" || f.Subject.Name != "GitHub") {
			t.Errorf("approval finding = %+v, want GitHub's approval page", f)
		}
	}
}

func TestADisconnectingIntegrationIsNotAdvisedAbout(t *testing.T) {
	t.Parallel()
	state := reconciled(integration.KindExpo, integration.Finding{Kind: integration.FindingCredentialRejected})
	state.Disconnecting = true
	if got := advisor.FromIntegrations([]integration.State{state}); len(got) != 0 {
		t.Errorf("findings = %+v, want none for a surface being removed", got)
	}
}

// AN UNREAD SOURCE IS SAID. Integrations this node could not read are named,
// so an empty list never reads as nothing to change.
func TestAnswerNamesTheIntegrationsItCouldNotRead(t *testing.T) {
	t.Parallel()
	unread := advisor.Read(advisor.Inputs{Company: liveShape(t), Revision: "r1"})
	if !slices.Equal(unread.Unread, []string{advisor.UnreadIntegrations}) {
		t.Errorf("unread = %v, want the integrations named", unread.Unread)
	}
	if got := read(liveShape(t), "r1").Unread; len(got) != 0 {
		t.Errorf("unread = %v with every integration read, want none", got)
	}
}

func TestABlockingIntegrationLeadsTheAnswer(t *testing.T) {
	t.Parallel()
	a := advisor.Read(advisor.Inputs{Company: liveShape(t), Revision: "r1",
		Integrations: []integration.State{reconciled(integration.KindDatadog,
			integration.Finding{Kind: integration.FindingCredentialMissing})}})
	if a.Findings[0].Class != "integration.credential_missing" {
		t.Errorf("first finding = %s, want the blocking integration ahead of every cost", a.Findings[0].Class)
	}
}
