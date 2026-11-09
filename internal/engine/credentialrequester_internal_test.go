package engine

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A CREDENTIAL IS PLACED BY ITS LOGIN'S NAMESPACE, AND ONLY THERE (ADR-0109).
//
// A turn a credential acting as no seat woke is worked for it as THIS node
// resolves it: a `token:` login by this node's own Tier A — a token another
// node declares is nobody here, whatever the identity directory holds under
// the same name — and any other login by the directory alone, as a service
// account bound to no seat that may act. A token the directory binds to a seat
// is the person's, so it never stands here as itself. Every principal placed
// holds apps alone, never admin; a directory this node cannot vouch for is
// unknown, never a stranger.
//
// Mutation: consult the directory for a `token:` login Tier A does not declare
// — the token another node declares is placed with the directory's admin row;
// drop the seat check on a service account — the bound account is placed.
func TestACredentialIsPlacedByItsLoginsNamespaceAlone(t *testing.T) {
	t.Parallel()
	machineRow := func(id, login, seat string, stage iam.Stage, grants ...iam.Grant) iamdomain.Sighting {
		return iamdomain.Sighting{ID: id, Kind: iam.KindMachine, Stage: stage, Login: login, Seat: seat,
			Grants: grants}
	}
	dir := fakeBindings{rows: map[string]iamdomain.Sighting{
		"deploy-bot": machineRow("018f3a9c-0000-7000-8000-0000000000d1", "deploy-bot", "",
			iam.StageActive, iam.GrantDoclet, iam.GrantAdmin),
		"paused-bot": machineRow("018f3a9c-0000-7000-8000-0000000000d2", "paused-bot", "",
			iam.StageSuspended, iam.GrantWorklet),
		"seated-bot": machineRow("018f3a9c-0000-7000-8000-0000000000d3", "seated-bot", "platform-lead",
			iam.StageActive, iam.GrantWorklet),
		"ana": {ID: "018f3a9c-0000-7000-8000-0000000000d4", Kind: iam.KindPerson, Stage: iam.StageActive,
			Login: "ana", Seat: "ana", Grants: []iam.Grant{iam.GrantWorklet}},
		// A ROW UNDER A TOKEN'S LOGIN this node's Tier A does not declare,
		// carrying admin: never a credential here.
		"token:elsewhere": machineRow("018f3a9c-0000-7000-8000-0000000000d5", "token:elsewhere", "",
			iam.StageActive, iam.GrantAdmin),
		// A TIER A TOKEN BOUND TO A SEAT, whose writes are the person's.
		"token:ops": boundMachine(),
	}}
	tokens := map[string][]iam.Grant{
		"ci":  {iam.GrantWorklet},
		"ops": {iam.GrantAdmin},
		"all": {iam.GrantAdmin},
	}
	for _, c := range []struct {
		login  string
		placed bool
		apps   []iam.Grant
	}{
		{"token:ci", true, []iam.Grant{iam.GrantWorklet}},
		{"token:all", true, iam.Apps},
		{"token:ops", false, nil},
		{"token:elsewhere", false, nil},
		{"token:unknown", false, nil},
		{"deploy-bot", true, []iam.Grant{iam.GrantDoclet, iam.GrantWorklet, iam.GrantArtifacts, iam.GrantChatlet,
			iam.GrantGitlet}},
		{"paused-bot", false, nil},
		{"seated-bot", false, nil},
		{"ana", false, nil},
		{"nobody", false, nil},
	} {
		p, placed, err := credentialOf(t.Context(), dir, tokens, c.login)
		if err != nil {
			t.Errorf("%s: %v", c.login, err)
			continue
		}
		if placed != c.placed {
			t.Errorf("%s placed: %v, want %v", c.login, placed, c.placed)
			continue
		}
		if !placed {
			continue
		}
		if p.Kind != iam.KindMachine || p.Login != c.login || !p.Stage.MayAct() ||
			!slices.Equal(p.Grants, c.apps) || slices.Contains(p.Grants, iam.GrantAdmin) {
			t.Errorf("%s placed as %+v, want an active machine using %v and never admin",
				c.login, p, c.apps)
		}
	}

	// A DIRECTORY THIS NODE CANNOT VOUCH FOR is unknown: a lagging copy may
	// not have applied the account's removal yet.
	lagging := dir
	lagging.lag = statelog.StallGrace + time.Second
	if _, _, err := credentialOf(t.Context(), lagging, tokens, "deploy-bot"); err == nil {
		t.Error("a service account read off a lagging directory was placed")
	}
	unread := dir
	unread.err = errors.New("the store blinked")
	if _, placed, err := credentialOf(t.Context(), unread, tokens, "deploy-bot"); err == nil || placed {
		t.Errorf("an unreadable directory answered placed=%v, err=%v; want unknown", placed, err)
	}
}

// A CREDENTIAL'S TURN USES ONLY WHAT EVERY ONE OF ITS WRITES CARRIED.
//
// A burst of one token's writes merged into one trigger can span a narrowing:
// the first write carried what the token held then, a later one what it holds
// now. A merge must not launder a constraint, so the turn is worked for the
// token with the apps EVERY write carried — and the requester is read as
// requesterOf reads it, past the seat's own voice and never past a person who
// spoke first.
//
// Mutation: take the first write's apps — the burst uses Doclet, which its
// later write no longer carried.
func TestACredentialsTurnUsesOnlyWhatEveryOneOfItsWritesCarried(t *testing.T) {
	t.Parallel()
	token := func(apps ...iam.Grant) *types.NotificationWriter {
		return notify.FirstPartyWriter(iam.ActorOperator, "token:ci", apps)
	}
	burst := []*events.Event{events.New(types.ExternalNotification{
		NotificationSource: worklet.Source, Sender: "token:ci", Writer: token(iam.GrantWorklet),
		Messages: []types.CoalescedMessage{
			{Sender: "token:ci", Writer: token(iam.GrantWorklet, iam.GrantDoclet)},
			{Sender: "token:ci", Writer: token(iam.GrantWorklet)},
		},
	}, events.TraceContext{})}
	credential := types.InboundInteraction{Sender: types.CanonicalIdentity{
		ExternalID: "token:ci", Platform: worklet.Source, Login: "token:ci"}}
	itself := types.InboundInteraction{Sender: types.CanonicalIdentity{Handle: "swe"}}
	person := types.InboundInteraction{Sender: types.CanonicalIdentity{Handle: "ada"}}

	login, apps := credentialRequesterOf(burst, burst, []types.InboundInteraction{credential, credential}, "swe")
	if login != "token:ci" || !slices.Equal(apps, []iam.Grant{iam.GrantWorklet}) {
		t.Errorf("the burst is worked for %q using %v, want token:ci using Worklet alone", login, apps)
	}
	login, _ = credentialRequesterOf(burst, burst, []types.InboundInteraction{itself, credential}, "swe")
	if login != "token:ci" {
		t.Errorf("past the seat's own voice the turn is worked for %q, want token:ci", login)
	}
	if login, apps := credentialRequesterOf(burst, burst,
		[]types.InboundInteraction{person, credential}, "swe"); login != "" || apps != nil {
		t.Errorf("a turn a person spoke first in is worked for the credential %q (%v); requesterOf "+
			"names the person", login, apps)
	}
	ask := []*events.Event{events.New(types.A2ARequest{
		ChannelID: "c-1", Requester: "cto", Content: "can you look",
	}, events.TraceContext{})}
	if login, _ := credentialRequesterOf(ask, ask, []types.InboundInteraction{credential}, "swe"); login != "" {
		t.Errorf("a colleague's ask is worked for the credential %q; an ask names its asking seat", login)
	}
}

// A CREDENTIAL'S TURN IS STILL ITS TURN WHEN IT RESUMES (ADR-0109). The
// credential and the apps its write carried reach the turn's context at
// dispatch, the launch that records the run's row, and the context a resumed
// segment rebuilds from that row — so every call the turn makes after its
// executor parks, and every call its box makes through the bridge after a
// takeover, is bounded by the token, never by nobody.
//
// Mutation: leave RequesterLogin off describeResume — the resumed turn is
// worked for nobody; off sandboxTurnRef — the launch records no credential.
func TestACredentialsTurnIsStillItsTurnWhenItResumes(t *testing.T) {
	t.Parallel()
	e, _ := starting(t, refusingModels(t))
	company := e.Company()
	body := "the nightly build failed on main"
	wake := events.New(types.ExternalNotification{
		NotificationSource: worklet.Source, SourceEventType: "comment", Sender: "token:ci",
		Body: body, SalientBody: &body,
		Writer:   notify.FirstPartyWriter(iam.ActorOperator, "token:ci", []iam.Grant{iam.GrantWorklet}),
		Metadata: map[string]string{notify.ActorField: "token:ci"},
	}, events.TraceContext{})
	tel, err := e.describeTurn(t.Context(), company, Request{
		RunID: "run-1", Handle: "swe", Events: []*events.Event{wake},
	})
	if err != nil {
		t.Fatalf("describeTurn: %v", err)
	}
	worklets := []iam.Grant{iam.GrantWorklet}
	dispatched := tel.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context
	if dispatched.Requester != "" || dispatched.RequesterLogin != "token:ci" ||
		!slices.Equal(dispatched.RequesterApps, worklets) {
		t.Fatalf("the dispatched turn is worked for seat %q, credential %q using %v; want token:ci "+
			"using Worklet", dispatched.Requester, dispatched.RequesterLogin, dispatched.RequesterApps)
	}
	ref := sandboxTurnRef(t.Context(), dispatched, "SWE", nil)
	if ref.RequesterLogin != "token:ci" || !slices.Equal(ref.RequesterApps, worklets) {
		t.Errorf("the launch records credential %q using %v, want token:ci using Worklet",
			ref.RequesterLogin, ref.RequesterApps)
	}
	// THE RESUMED TURN AS THE COORDINATOR BUILDS IT ([resumedTurn]), off the
	// row the launch wrote.
	run := sandbox.PendingRun{TurnID: "run-1", AgentHandle: "swe",
		RequesterLogin: ref.RequesterLogin, RequesterApps: ref.RequesterApps}
	resumed, err := e.describeResume(t.Context(), company, resumeInput{
		Run:  run,
		Turn: resumedTurn(run, company.Org.AgentSeatByHandle("swe"), company.Org, turnctx.NewHolds(run.CodeHolds)),
	})
	if err != nil {
		t.Fatalf("describeResume: %v", err)
	}
	again := resumed.runnerTurn(company, 0, nil, "task", turn.Reply{}, nil).Context
	if again.RequesterLogin != "token:ci" || !slices.Equal(again.RequesterApps, worklets) {
		t.Errorf("the resumed turn is worked for %q using %v, want the credential its row recorded",
			again.RequesterLogin, again.RequesterApps)
	}
}
