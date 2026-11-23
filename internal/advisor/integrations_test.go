package advisor_test

import (
	"slices"
	"strings"
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

// UNREAD FINDINGS LEAVE THE INTEGRATIONS UNREAD; A CUT RECORD DOES NOT. A
// surface whose findings could not be read beside its status says nothing of
// any integration this round. One whose record dropped findings was read: it
// is a partial reading of that surface alone, said to a person by name and
// count, and never the word that stops every surface concluding.
//
// Mutation: read a cut record as unread, or say no silence for it, and an arm
// below fails.
func TestIntegrationFindingsNotInHandLeaveTheIntegrationsUnread(t *testing.T) {
	t.Parallel()
	unread := reconciled(integration.KindDatadog)
	unread.FindingsUnread = true
	a := advisor.Read(advisor.Inputs{Company: liveShape(t), Revision: "r1", Integrations: []integration.State{unread}})
	if !slices.Equal(a.Unread, []string{advisor.UnreadIntegrations}) {
		t.Errorf("unread findings: unread = %v, want the integrations named", a.Unread)
	}

	cut := reconciled(integration.KindExpo, integration.Finding{Kind: integration.FindingGrantShort, Subject: "sre"})
	cut.FindingsRecord.Omitted = 3
	a = advisor.Read(advisor.Inputs{Company: liveShape(t), Revision: "r1", Integrations: []integration.State{cut}})
	if len(a.Unread) != 0 || !slices.Equal(a.Partial, []string{advisor.IntegrationPath(integration.KindExpo)}) {
		t.Errorf("a cut record: unread %v, partial %v; want none unread and Expo's path partial", a.Unread, a.Partial)
	}
	said := false
	for _, s := range a.Silent {
		said = said || s.Class == advisor.ClassIntegrationRecordCut &&
			strings.Contains(s.Reason, advisor.SurfaceName(integration.KindExpo)) && strings.Contains(s.Reason, "3 more")
	}
	if !said {
		t.Errorf("a cut record is said to nobody: silent %+v", a.Silent)
	}
}

// ONE CUT SURFACE LEAVES EVERY OTHER CONCLUDING. Slack's record dropped
// findings: the finding it held open and no longer carries stays open, and
// the one it carries opens; meanwhile GitHub's new finding opens and
// Datadog's cleared one resolves, as on any run.
//
// Mutation: hold no path partial, or every path, and a step below differs.
func TestACutSurfaceHoldsOnlyItsOwnFindingsOpen(t *testing.T) {
	t.Parallel()
	only := func(state integration.State) advisor.Finding {
		return advisor.FromIntegrations([]integration.State{state})[0]
	}
	held := only(reconciled(integration.KindSlack,
		integration.Finding{Kind: integration.FindingApprovalRequired, Subject: "sre"}))
	cleared := only(reconciled(integration.KindDatadog,
		integration.Finding{Kind: integration.FindingCredentialRejected}))
	cut := reconciled(integration.KindSlack, integration.Finding{Kind: integration.FindingGrantShort, Subject: "cto"})
	cut.FindingsRecord.Omitted = 3
	a := advisor.Read(advisor.Inputs{Company: liveShape(t), Revision: "r1", Integrations: []integration.State{
		cut, reconciled(integration.KindGitHub, integration.Finding{Kind: integration.FindingCredentialRejected}),
		reconciled(integration.KindDatadog),
	}})
	var obs []advisor.Observation
	for _, f := range a.Findings {
		if f.Area == advisor.AreaIntegrations {
			obs = append(obs, observed(f))
		}
	}
	stored := []advisor.Stored{storedAs(t, observed(held), advisor.StateOpen),
		storedAs(t, observed(cleared), advisor.StateOpen)}
	r := advisor.Reading{At: at, Groups: []advisor.Group{advisor.GroupSetup}, Unread: a.Unread,
		PartialPaths: a.Partial}
	opened, resolved := map[string]bool{}, map[string]bool{}
	for _, tr := range advisor.Reconcile(stored, obs, r) {
		switch {
		case tr.Step == advisor.StepOpen:
			opened[tr.Observed.Finding.Path] = true
		case tr.Step == advisor.StepClose:
			resolved[tr.Stored.Observation.Finding.Path] = true
		}
	}
	slack, github, datadog := advisor.IntegrationPath(integration.KindSlack),
		advisor.IntegrationPath(integration.KindGitHub), advisor.IntegrationPath(integration.KindDatadog)
	if !opened[slack] || !opened[github] || !resolved[datadog] || resolved[slack] {
		t.Errorf("opened %v and resolved %v; want Slack's carried and GitHub's opened, Datadog's resolved, "+
			"and Slack's held one kept open", opened, resolved)
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

// TWO FINDINGS OF ONE KIND ON ONE SURFACE SAY WHICH. A reconcile finding with
// no title of its own is titled with its label, which names its subject, so
// two agents' refusals at one vendor are two findings a person can tell apart,
// worded as Ask Crewlet's list of the integration's findings words them.
func TestTwoFindingsOfOneKindOnASurfaceSayWhich(t *testing.T) {
	t.Parallel()
	findings := []integration.Finding{
		{Kind: integration.FindingIdentityFailed, Subject: "pm", Detail: "Refused."},
		{Kind: integration.FindingIdentityFailed, Subject: "sre", Detail: "Refused."},
	}
	got := advisor.FromIntegrations([]integration.State{reconciled(integration.KindDatadog, findings...)})
	if len(got) != 2 || got[0].Title == got[1].Title {
		t.Fatalf("findings = %+v, want two with titles that differ", got)
	}
	for i, who := range []string{"pm", "sre"} {
		if want := "Datadog: " + findings[i].Label() + "."; got[i].Title != want ||
			!strings.HasSuffix(findings[i].Label(), "for "+who) {
			t.Errorf("title %q, want %q naming %s", got[i].Title, want, who)
		}
	}
}
