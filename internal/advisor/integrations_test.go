package advisor_test

import (
	"errors"
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
// is a partial reading of what that record may have dropped alone, said to a
// person by name and count, singular or plural, of one surface or of two, and
// never the word that stops every surface concluding; the run's class outcomes
// list it as silent.
//
// Mutation: read a cut record as unread, say no silence for it, say "1 more
// findings" or two surfaces' "its record", or list no silence of an unread
// class in Outcomes, and an arm below fails.
func TestIntegrationFindingsNotInHandLeaveTheIntegrationsUnread(t *testing.T) {
	t.Parallel()
	unread := reconciled(integration.KindDatadog)
	unread.FindingsUnread = true
	a := advisor.Read(advisor.Inputs{Company: liveShape(t), Revision: "r1", Integrations: []integration.State{unread}})
	if !slices.Equal(a.Unread, []string{advisor.UnreadIntegrations}) {
		t.Errorf("unread findings: unread = %v, want the integrations named", a.Unread)
	}

	for _, c := range []struct {
		omitted, owed int
		said          string
		want          advisor.Cut
	}{
		{3, 1, "3 more findings than", advisor.Cut{Owed: true, Unowed: true}},
		{1, 0, "1 more finding than", advisor.Cut{Unowed: true}},
		{1, 1, "1 more finding than", advisor.Cut{Owed: true}},
	} {
		cut := reconciled(integration.KindExpo, integration.Finding{Kind: integration.FindingGrantShort, Subject: "sre"})
		cut.FindingsRecord.Omitted, cut.FindingsRecord.OwedOmitted = c.omitted, c.owed
		a = advisor.Read(advisor.Inputs{Company: liveShape(t), Revision: "r1", Integrations: []integration.State{cut}})
		c.want.Path = advisor.IntegrationPath(integration.KindExpo)
		if len(a.Unread) != 0 || !slices.Equal(a.Partial, []advisor.Cut{c.want}) {
			t.Errorf("%d cut, %d owed: unread %v, partial %+v; want none unread and %+v", c.omitted, c.owed,
				a.Unread, a.Partial, c.want)
		}
		said := ""
		for _, o := range advisor.Outcomes(a.Findings, a.Unread, a.Silent) {
			if o.Class == advisor.ClassIntegrationRecordCut && o.Outcome == advisor.OutcomeSilent {
				said = o.Reason
			}
		}
		if !strings.Contains(said, advisor.SurfaceName(integration.KindExpo)+"'s last pass found "+c.said) ||
			!strings.HasSuffix(said, ", so its open findings the record may have left out stay open until a "+
				"pass keeps them.") {
			t.Errorf("%d cut: the run's outcomes say %q, want a silence saying %q of its record", c.omitted, said,
				c.said)
		}
	}

	expo := reconciled(integration.KindExpo)
	expo.FindingsRecord.Omitted = 3
	slack := reconciled(integration.KindSlack)
	slack.FindingsRecord.Omitted = 1
	a = advisor.Read(advisor.Inputs{Company: liveShape(t), Revision: "r1",
		Integrations: []integration.State{expo, slack}})
	want := "Expo's last pass found 3 more findings than its record keeps; Slack's last pass found 1 more " +
		"finding than its record keeps, so the open findings their records may have left out stay open until a " +
		"pass keeps them."
	if len(a.Silent) != 1 || a.Silent[0].Reason != want {
		t.Errorf("two cut surfaces are said as %+v, want %q", a.Silent, want)
	}
}

// cutReading is what Reconcile does of stored on a run that read GitHub's cut
// record, whose drop counts are omitted and owed, beside Datadog's empty one:
// the keys it resolved, and the paths it opened.
func cutReading(t *testing.T, stored []advisor.Stored, omitted, owed int) (resolved map[string]bool,
	opened map[string]bool) {

	t.Helper()
	cut := reconciled(integration.KindGitHub, integration.Finding{Kind: integration.FindingGrantShort, Subject: "cto"})
	cut.FindingsRecord.Omitted, cut.FindingsRecord.OwedOmitted = omitted, owed
	a := advisor.Read(advisor.Inputs{Company: liveShape(t), Revision: "r1", Integrations: []integration.State{
		cut, reconciled(integration.KindDatadog)}})
	var obs []advisor.Observation
	for _, f := range a.Findings {
		if f.Area == advisor.AreaIntegrations {
			obs = append(obs, observed(f))
		}
	}
	r := advisor.Reading{At: at, Groups: []advisor.Group{advisor.GroupSetup, advisor.GroupEvidence},
		Unread: a.Unread, Cuts: a.Partial}
	resolved, opened = map[string]bool{}, map[string]bool{}
	for _, tr := range advisor.Reconcile(stored, obs, r) {
		switch tr.Step {
		case advisor.StepOpen:
			opened[tr.Observed.Finding.Path] = true
		case advisor.StepClose:
			resolved[tr.Stored.Observation.Key()] = true
		}
	}
	return resolved, opened
}

// A CUT RECORD HOLDS OPEN ONLY WHAT IT MAY HAVE DROPPED. GitHub's record
// dropped findings: what it carries opens; of its stored findings it no longer
// carries, one a person owes stays open only where the record dropped one a
// person owes, and one nobody owes only where it dropped one nobody owes;
// an inbound silence and a stale atlas source on the same path, which no
// record carries, resolve when cleared, as does Datadog's.
//
// Mutation: hold every finding on a cut path whatever its source, hold one a
// person owes where none was dropped, or hold none, and an arm below differs.
func TestACutSurfaceHoldsOnlyItsOwnFindingsOpen(t *testing.T) {
	t.Parallel()
	only := func(state integration.State) advisor.Finding {
		return advisor.FromIntegrations([]integration.State{state})[0]
	}
	path := advisor.IntegrationPath(integration.KindGitHub)
	owed := only(reconciled(integration.KindGitHub,
		integration.Finding{Kind: integration.FindingApprovalRequired, Subject: "sre"}))
	unowed := only(reconciled(integration.KindGitHub,
		integration.Finding{Kind: integration.FindingGrantPending, Subject: "pm"}))
	silent := advisor.Finding{Class: advisor.ClassInboundSilent, Severity: advisor.SeverityCosts,
		Actor: advisor.ActorOwner, Source: advisor.SourceEvidence, Area: advisor.AreaIntegrations,
		Subject: advisor.Subject{Kind: advisor.SubjectIntegration, Name: "GitHub"}, Path: path,
		Evidence: []advisor.Fact{}, Fix: advisor.Fix{Kind: advisor.FixExternalStep, Options: []advisor.Option{}}}
	stale := advisor.Finding{Class: advisor.ClassAtlasSourceStale, Severity: advisor.SeverityCosts,
		Actor: advisor.ActorOwner, Source: advisor.SourceAtlas, Area: advisor.AreaAtlas,
		Subject: advisor.Subject{Kind: advisor.SubjectIntegration, Name: "GitHub"}, Path: path,
		Evidence: []advisor.Fact{}, Fix: advisor.Fix{Kind: advisor.FixNone, Options: []advisor.Option{}}}
	cleared := only(reconciled(integration.KindDatadog,
		integration.Finding{Kind: integration.FindingCredentialRejected}))
	all := []advisor.Finding{owed, unowed, silent, stale, cleared}
	for _, c := range []struct {
		omitted, owed int
		// held are the findings of all that stay open.
		held []advisor.Finding
	}{
		{4, 0, []advisor.Finding{unowed}},
		{4, 1, []advisor.Finding{owed, unowed}},
		{1, 1, []advisor.Finding{owed}},
	} {
		var stored []advisor.Stored
		for _, f := range all {
			stored = append(stored, storedAs(t, observed(f), advisor.StateOpen))
		}
		resolved, opened := cutReading(t, stored, c.omitted, c.owed)
		if !opened[path] {
			t.Errorf("%d cut, %d owed: GitHub's carried finding did not open (opened %v)", c.omitted, c.owed, opened)
		}
		for _, f := range all {
			if want := !slices.ContainsFunc(c.held, func(h advisor.Finding) bool {
				return h.Class == f.Class &&
					h.Focus == f.Focus
			}); resolved[advisor.Key(f)] != want {
				t.Errorf("%d cut, %d owed: %s (%s) resolved %t, want %t", c.omitted, c.owed, f.Class, f.Focus,
					!want, want)
			}
		}
	}
}

// A FINDING KEPT WITHOUT ITS WORDS KEEPS THE ONES IT HAD. GitHub's record cut
// the words of an open finding a person owes, read as a run reads it (its
// headline drawn from its title): the run records no revise and no refresh,
// and the finding keeps its headline, title, why, remedy and link; once the
// record carries new words they are read as on any run.
//
// Mutation: let a wordless observation replace any stored word, the headline
// included, and the finding is refreshed or revised to the kind's words.
func TestAFindingKeptWithoutItsWordsKeepsTheOnesItHad(t *testing.T) {
	t.Parallel()
	read := func(f integration.Finding, wordsOmitted int) advisor.Observation {
		state := reconciled(integration.KindGitHub, f)
		state.FindingsRecord.WordsOmitted = wordsOmitted
		for _, found := range advisor.Read(advisor.Inputs{Company: liveShape(t), Revision: "r1",
			Integrations: []integration.State{state}}).Findings {
			if found.Area == advisor.AreaIntegrations {
				return observed(found)
			}
		}
		t.Fatalf("the read listed no finding of %s", f.Kind)
		return advisor.Observation{}
	}
	whole := integration.Finding{Kind: integration.FindingGrantShort, Subject: "sre", Title: "sre is short",
		Detail: "sre lacks write.", Remedy: "Grant it write.", ActionURL: "https://example.com/grant"}
	kept := read(whole, 0)
	if kept.Finding.Headline != whole.Title {
		t.Fatalf("premise: the read's headline is %q, want the title", kept.Finding.Headline)
	}
	stored := []advisor.Stored{storedAs(t, kept, advisor.StateOpen)}
	r := advisor.Reading{At: at, Groups: []advisor.Group{advisor.GroupSetup}}
	cut := read(integration.Finding{Kind: integration.FindingGrantShort, Subject: "sre"}, 1)
	if steps := advisor.Reconcile(stored, []advisor.Observation{cut}, r); len(steps) != 0 {
		t.Errorf("a cut of words took %d steps, the first %s with headline %q and fix %+v; want none", len(steps),
			steps[0].Step, steps[0].Observed.Finding.Headline, steps[0].Observed.Finding.Fix)
	}
	moved := whole
	moved.ActionURL = "https://example.com/grant/new"
	steps := advisor.Reconcile(stored, []advisor.Observation{read(moved, 0)}, r)
	if len(steps) != 1 || steps[0].Step != advisor.StepRevise || steps[0].Observed.Finding.Fix.URL != moved.ActionURL {
		t.Errorf("a new link took steps %+v, want it revised", steps)
	}
}

// A RE-CHECK OF A FINDING A CUT RECORD MAY HAVE DROPPED SAYS NOTHING. GitHub's
// record dropped a finding nobody owes: a re-check of the open one the record
// no longer carries can neither send nor resolve it, while one it carries
// holds.
//
// Mutation: re-check it as holding, and it is sent unread.
func TestARecheckOfAFindingACutRecordMayHaveDroppedConcludesNothing(t *testing.T) {
	t.Parallel()
	only := func(state integration.State) advisor.Finding {
		return advisor.FromIntegrations([]integration.State{state})[0]
	}
	dropped := storedAs(t, observed(only(reconciled(integration.KindGitHub,
		integration.Finding{Kind: integration.FindingGrantPending, Subject: "pm"}))), advisor.StateOpen)
	carried := only(reconciled(integration.KindGitHub,
		integration.Finding{Kind: integration.FindingGrantPending, Subject: "cto"}))
	r := advisor.Reading{At: at, Groups: []advisor.Group{advisor.GroupSetup},
		Cuts: []advisor.Cut{{Path: advisor.IntegrationPath(integration.KindGitHub), Unowed: true}}}
	if _, _, err := advisor.Recheck(dropped, []advisor.Observation{observed(carried)}, r); !errors.Is(err,
		advisor.ErrUnconcluded) {
		t.Errorf("a re-check of a finding the record may have dropped: %v, want %v", err, advisor.ErrUnconcluded)
	}
	outcome, _, err := advisor.Recheck(storedAs(t, observed(carried), advisor.StateOpen),
		[]advisor.Observation{observed(carried)}, r)
	if err != nil || outcome != advisor.RecheckHolds {
		t.Errorf("a re-check of a carried finding: %s, %v; want it holding", outcome, err)
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
