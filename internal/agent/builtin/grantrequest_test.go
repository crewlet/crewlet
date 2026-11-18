package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	_ "github.com/crewlet/crewlet/internal/cloudflare" // registers the vendor whose lane takes grants
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/tools"
)

// fakeGrantDesk is the engine's half of a grant request: one seat's Cloudflare
// setting and offer, and what was filed, withdrawn and declined.
type fakeGrantDesk struct {
	mu        sync.Mutex
	want      seataccess.Desired
	opts      seataccess.Options
	filed     []seataccess.GrantRequest
	withdrawn []string
	declined  []string
	granted   bool

	// raise, saveErr: what a save answers; asked, saved: what it was asked and made.
	raise   bool
	saveErr error
	asked   []string
	saved   []string
}

// Grant saves once admit admits, as the engine's desk does.
func (d *fakeGrantDesk) Grant(_ context.Context, asker, id string, permanent bool,
	admit func(raise bool) error) error {

	d.mu.Lock()
	defer d.mu.Unlock()
	d.asked = append(d.asked, fmt.Sprintf("%s/%s/%v", asker, id, permanent))
	if d.saveErr != nil {
		return d.saveErr
	}
	if err := admit(d.raise); err != nil {
		return err
	}
	d.saved = append(d.saved, id)
	d.granted = true
	return nil
}

func (d *fakeGrantDesk) Offer(context.Context, string, seataccess.Lane) (seataccess.Desired, seataccess.Options, error) {
	return d.want, d.opts, nil
}

func (d *fakeGrantDesk) File(_ context.Context, r seataccess.GrantRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.filed = append(d.filed, r)
	return nil
}

func (d *fakeGrantDesk) Withdraw(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.withdrawn = append(d.withdrawn, id)
	return nil
}

func (d *fakeGrantDesk) Granted(context.Context, string, string, bool) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.granted, nil
}

func (d *fakeGrantDesk) Declined(_ context.Context, id, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.declined = append(d.declined, id)
	return nil
}

// newGrantDesk is a seat on Secure whose catalogue offers Origin Rules read and
// write on example.com, and DNS Write the provisioner does not hold.
func newGrantDesk() *fakeGrantDesk {
	return &fakeGrantDesk{
		want: seataccess.Desired{Enabled: true, Level: "secure"},
		opts: seataccess.Options{Lane: seataccess.LaneCloudflare,
			Levels: []seataccess.Level{{ID: "read"}, {ID: "operate"}, {ID: "configure"}, {ID: "secure"}},
			Grants: []seataccess.GrantEntry{
				{ID: "Origin Read", Name: "Origin Rules: Read", ScopeKind: "zone"},
				{ID: "Origin Write", Name: "Origin Rules: Edit", ScopeKind: "zone", Write: true},
				{ID: "DNS Write", Name: "DNS Write", ScopeKind: "zone", Write: true,
					Unavailable: "The provisioner token does not hold DNS Write", Missing: []string{"DNS Write"}},
			},
			GrantScope: []string{"example.com"}},
	}
}

func grantDeps(trk *fakeTracker, desk *fakeGrantDesk) builtin.WorkDeps {
	deps := accessDeps(trk)
	deps.Grants = desk
	return deps
}

// requestOf calls request_access's integration form as the CTO agent.
func requestOf(t *testing.T, trk *fakeTracker, desk *fakeGrantDesk, args map[string]any) tools.Result {
	t.Helper()
	base := map[string]any{"item": "ENG-1", "ask": "founder", "integration": "cloudflare",
		"why": "Cloudflare refused creating the origin rule with 403."}
	for k, v := range args {
		base[k] = v
	}
	entry, ok := workRegistry(t, grantDeps(trk, desk)).Lookup(builtin.RequestAccessTool)
	if !ok {
		t.Fatal("request_access is not registered")
	}
	got, err := entry.Tool.(tools.SeatCallable).CallForTurn(everyGrant(), turnFor(t, "agent-cto"), base)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// AN AGENT ASKS FOR ONE PERMISSION AS THE ENGINE'S OWN DECISION: the request is
// stored, and posted with three options carrying its id, the temporary grant
// recommended, in the catalogue's own words for the permission, asked of the
// person named. Mutation: post the agent's own options and nothing ties the
// answer to the stored request.
func TestAPermissionRequestIsTheEnginesDecision(t *testing.T) {
	t.Parallel()
	trk, desk := newFakeTracker(), newGrantDesk()
	got := requestOf(t, trk, desk, map[string]any{"permission": "Origin Write",
		"scope": []any{"example.com"}, "hours": 2})
	if got.Failed {
		t.Fatalf("the request failed: %s", got.Output)
	}
	if len(desk.filed) != 1 || desk.filed[0].Hours != 2 || desk.filed[0].Permission != "Origin Write" {
		t.Fatalf("filed = %+v, want the one request for two hours", desk.filed)
	}
	id := desk.filed[0].ID
	if len(trk.patched) != 1 || trk.patched[0].Comment == nil || trk.patched[0].Comment.Decision == nil {
		t.Fatalf("patched = %+v, want one decision", trk.patched)
	}
	d := trk.patched[0].Comment.Decision
	if len(d.Options) != 3 || d.Options[0].ID != seataccess.GrantOptionTemporary(id) ||
		d.Options[1].ID != seataccess.GrantOptionPermanent(id) || d.Options[2].ID != "decline" ||
		d.Recommended != seataccess.GrantOptionTemporary(id) || d.Role != decision.RoleApprover {
		t.Fatalf("decision = %+v, want the temporary grant recommended over permanent and not now", d)
	}
	if !strings.Contains(d.Question, "Origin Rules: Edit") || !strings.Contains(d.Question, "example.com") ||
		!strings.Contains(d.Options[0].Label, "expires in 2 hours") ||
		!strings.Contains(d.Options[0].Detail, "sign in again") {
		t.Errorf("decision words = %+v, want the builder's name, the zone, the hours and the step-up", d)
	}
	if err := d.ValidateRecommendedAt(""); err != nil {
		t.Errorf("the decision is not one an agent may ask: %v", err)
	}
}

// A PERMISSION THE PROVISIONER LACKS IS STILL ASKED, saying what the company's
// own credential must hold first: the person who grants it is the one who can
// add it at the vendor.
func TestAPermissionTheProvisionerLacksIsAskedNamingWhatToAdd(t *testing.T) {
	t.Parallel()
	trk, desk := newFakeTracker(), newGrantDesk()
	got := requestOf(t, trk, desk, map[string]any{"permission": "DNS Write"})
	if got.Failed || len(trk.patched) != 1 {
		t.Fatalf("result = %+v, want the request posted", got)
	}
	if d := trk.patched[0].Comment.Decision; !strings.Contains(d.Options[0].Detail, "must first give") {
		t.Errorf("the option does not say what the provisioner needs: %+v", d.Options[0])
	}
}

// A REQUEST NOBODY COULD GRANT IS REFUSED BEFORE ANYTHING IS STORED: a
// permission the catalogue does not list (naming the near ones), a seat with
// no Cloudflare access, one it holds already, an hour count out of bounds, a
// zone outside the company's, and both forms at once.
func TestAPermissionRequestNobodyCouldGrantIsRefused(t *testing.T) {
	t.Parallel()
	held := newGrantDesk()
	held.want.Grants = []seataccess.Grant{{ID: "g0000000001", Permission: "Origin Read",
		ExpiresAt: time.Now().Add(time.Hour)}}
	off := newGrantDesk()
	off.want.Enabled = false
	for name, c := range map[string]struct {
		desk *fakeGrantDesk
		args map[string]any
		says string
	}{
		"unlisted":   {newGrantDesk(), map[string]any{"permission": "Origin Rules Write"}, "`Origin Write`"},
		"off":        {off, map[string]any{"permission": "Origin Write"}, "Settings, Integrations, Cloudflare, Agents"},
		"held":       {held, map[string]any{"permission": "Origin Read"}, "already hold"},
		"too long":   {newGrantDesk(), map[string]any{"permission": "Origin Write", "hours": 500}, "`hours`"},
		"outside":    {newGrantDesk(), map[string]any{"permission": "Origin Write", "scope": []any{"example.org"}}, "example.org"},
		"both forms": {newGrantDesk(), map[string]any{"permission": "Origin Write", "access": "grant-health"}, "not both"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			got := requestOf(t, trk, c.desk, c.args)
			if !got.Failed || len(trk.patched) != 0 || len(c.desk.filed) != 0 {
				t.Fatalf("result = %+v, filed %d, posted %d; want a refusal and nothing stored",
					got, len(c.desk.filed), len(trk.patched))
			}
			if !strings.Contains(got.Output, c.says) {
				t.Errorf("refusal %q does not say %q", got.Output, c.says)
			}
		})
	}
}

// oneChangeDesk is newGrantDesk with a permission granted for one change.
func oneChangeDesk() *fakeGrantDesk {
	d := newGrantDesk()
	d.opts.Grants = append(d.opts.Grants, seataccess.GrantEntry{ID: "Bucket Write", Name: "Bucket Settings: Edit",
		ScopeKind: "zone", Write: true, MaxHours: 4, Note: "It also changes lifecycle rules, which can delete objects."})
	return d
}

// A PERMISSION GRANTED FOR ONE CHANGE IS ASKED FOR ITS TARGET ALONE, for an hour
// by default, saying what else it allows and offering no permanent grant.
// Mutation: keep the permanent option and the person can approve it for good.
func TestAOneChangePermissionIsAskedForItsTargetAlone(t *testing.T) {
	t.Parallel()
	trk, desk := newFakeTracker(), oneChangeDesk()
	why := "Make bucket user-uploads private: its listing is public."
	got := requestOf(t, trk, desk, map[string]any{"permission": "Bucket Write", "target": "user-uploads", "why": why})
	if got.Failed {
		t.Fatalf("the request failed: %s", got.Output)
	}
	if len(desk.filed) != 1 || desk.filed[0].Hours != 1 || desk.filed[0].Target != "user-uploads" {
		t.Fatalf("filed = %+v, want one request for one hour naming its target", desk.filed)
	}
	id := desk.filed[0].ID
	d := trk.patched[0].Comment.Decision
	if len(d.Options) != 2 || d.Options[0].ID != seataccess.GrantOptionTemporary(id) || d.Options[1].ID != "decline" {
		t.Fatalf("options = %+v, want the hour's grant and not now, never a permanent one", d.Options)
	}
	if !strings.Contains(d.Question, "for one change to user-uploads") || d.Rationale != why {
		t.Errorf("question %q and rationale %q, want the target asked and the agent's change", d.Question, d.Rationale)
	}
	if !strings.Contains(d.Options[0].Detail, "lifecycle rules, which can delete objects") ||
		!strings.Contains(d.Options[0].Label, "expires in 1 hour)") {
		t.Errorf("the grant option %+v does not say what else it allows for its hour", d.Options[0])
	}
	for name, c := range map[string]struct {
		args map[string]any
		says string
	}{
		"no target":  {map[string]any{"permission": "Bucket Write"}, "`target`"},
		"a sentence": {map[string]any{"permission": "Bucket Write", "target": "the uploads bucket"}, "`target`"},
		"past bound": {map[string]any{"permission": "Bucket Write", "target": "user-uploads", "hours": 5}, "at most 4 hours"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk, desk := newFakeTracker(), oneChangeDesk()
			got := requestOf(t, trk, desk, c.args)
			if !got.Failed || len(desk.filed) != 0 || !strings.Contains(got.Output, c.says) {
				t.Errorf("result %q (filed %d), want a refusal saying %q", got.Output, len(desk.filed), c.says)
			}
		})
	}
}

// A REQUEST'S GRANT IS ANSWERED ONLY ONCE ITS AGENT HOLDS IT, whoever sends the
// answer, and "Not now" settles the request it answered. Mutation: skip the
// check for a request's option and the agent is told yes with nothing saved.
func TestAPermissionIsAnsweredOnlyOnceItsAgentHoldsIt(t *testing.T) {
	t.Parallel()
	const id = "abcdefghij"
	for name, granted := range map[string]bool{"not saved yet": false, "saved": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk, desk := newFakeTracker(), newGrantDesk()
			desk.granted = granted
			trk.thread.AnswersDecision = &decision.Decision{Options: []decision.Option{
				{ID: seataccess.GrantOptionTemporary(id), Label: "Grant for this change"},
				{ID: seataccess.GrantOptionPermanent(id), Label: "Add permanently"},
				{ID: "decline", Label: "Not now"}}}
			trk.thread.AnsweredAuthor = "agent-cto"
			reg := tools.NewRegistry()
			if _, err := builtin.Register(reg, gated(builtin.Deps{Work: grantDeps(trk, desk)})); err != nil {
				t.Fatal(err)
			}
			got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"asks": false,
				"item": "ENG-1", "answers": "c-ask", "choice": seataccess.GrantOptionTemporary(id)})
			if posted := !got.Failed && len(trk.patched) == 1; posted != granted {
				t.Errorf("answer = %+v with %d writes, want posted %v", got, len(trk.patched), granted)
			}
		})
	}
	trk, desk := newFakeTracker(), newGrantDesk()
	trk.thread.AnswersDecision = &decision.Decision{Options: []decision.Option{
		{ID: seataccess.GrantOptionTemporary(id), Label: "Grant"}, {ID: "decline", Label: "Not now"}}}
	trk.thread.AnsweredAuthor = "agent-cto"
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Work: grantDeps(trk, desk)})); err != nil {
		t.Fatal(err)
	}
	if got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"asks": false,
		"item": "ENG-1", "answers": "c-ask", "choice": "decline"}); got.Failed {
		t.Fatalf("declining failed: %s", got.Output)
	}
	if len(desk.declined) != 1 || desk.declined[0] != id {
		t.Errorf("declined = %v, want the request settled", desk.declined)
	}
}

// grantAdmin is an administrator, within the step-up window where fresh, through
// a machine token where via names one.
func grantAdmin(fresh bool, via string) context.Context {
	reauth := time.Now().Add(-time.Minute)
	if fresh {
		reauth = time.Now().Add(time.Hour)
	}
	if via == "" {
		via = "session:browser"
	}
	return iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee3"),
		Kind: iam.KindPerson, Login: "founder", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAdmin}, ReauthAt: reauth, Via: via,
	})
}

// answerGrant answers the CTO agent's grant request on ENG-1 with choice, as caller.
func answerGrant(caller context.Context, t *testing.T, desk *fakeGrantDesk, choice string) (
	tools.Result, *fakeTracker) {

	t.Helper()
	const id = "abcdefghij"
	trk := newFakeTracker()
	trk.thread.AnswersDecision = &decision.Decision{Options: []decision.Option{
		{ID: seataccess.GrantOptionTemporary(id), Label: "Grant for this change"},
		{ID: seataccess.GrantOptionPermanent(id), Label: "Add permanently"},
		{ID: "decline", Label: "Not now"}}}
	trk.thread.AnsweredAuthor = "agent-cto"
	deps := grantDeps(trk, desk)
	deps.Actor = operatorActor
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(builtin.Deps{Work: deps})); err != nil {
		t.Fatal(err)
	}
	entry, ok := reg.Lookup(builtin.CommentOnWorkTool)
	if !ok {
		t.Fatal("comment_on_work_item is not registered")
	}
	got, err := entry.Tool.(tools.SeatCallable).CallForTurn(caller, nil, map[string]any{
		"item": "ENG-1", "answers": "c-ask", "choice": choice})
	if err != nil {
		t.Fatal(err)
	}
	return got, trk
}

// CHOOSING A REQUEST'S GRANT OPTION SAVES IT BEFORE THE ANSWER, once, and a
// failed save posts nothing. Mutation: post before the save and this fails.
func TestChoosingARequestsGrantSavesItBeforeTheAnswer(t *testing.T) {
	t.Parallel()
	const id = "abcdefghij"
	for _, c := range []struct {
		name   string
		choice string
		asked  string
	}{
		{"for its hours", seataccess.GrantOptionTemporary(id), "agent-cto/" + id + "/false"},
		{"for good", seataccess.GrantOptionPermanent(id), "agent-cto/" + id + "/true"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			desk := newGrantDesk()
			got, trk := answerGrant(grantAdmin(true, ""), t, desk, c.choice)
			if got.Failed || len(trk.patched) != 1 || trk.patched[0].Comment.Choice != c.choice {
				t.Fatalf("answer = %+v with %d writes, want the answer posted", got, len(trk.patched))
			}
			if !slices.Equal(desk.asked, []string{c.asked}) || len(desk.saved) != 1 {
				t.Errorf("saves asked %v, made %v; want the one grant saved before the answer",
					desk.asked, desk.saved)
			}
		})
	}

	held := newGrantDesk()
	held.granted = true
	if got, trk := answerGrant(grantAdmin(true, ""), t, held, seataccess.GrantOptionTemporary(id)); got.Failed ||
		len(trk.patched) != 1 || len(held.asked) != 0 {
		t.Errorf("an answer whose grant is held = %+v, %d writes, saves asked %v; want it posted "+
			"and nothing saved again", got, len(trk.patched), held.asked)
	}

	for name, c := range map[string]struct {
		err  error
		says string
	}{
		"lapsed":  {seataccess.ErrNoRequest, "has lapsed or was already settled"},
		"refused": {&seataccess.GrantRefused{Reason: "Origin Rules: Edit: The provisioner token does not hold it"}, "The provisioner token does not hold it"},
	} {
		desk := newGrantDesk()
		desk.saveErr = c.err
		got, trk := answerGrant(grantAdmin(true, ""), t, desk, seataccess.GrantOptionTemporary(id))
		if !got.Failed || len(trk.patched) != 0 || !strings.Contains(got.Output, c.says) {
			t.Errorf("%s: answer = %+v with %d writes, want it refused saying %q", name, got,
				len(trk.patched), c.says)
		}
	}
}

// A REQUEST'S GRANT IS CHOSEN ONLY AT THE ACCESS WRITE'S PROOF, NEVER BY AN
// AGENT. Mutation: skip the raise check and the machine-token case fails.
func TestARequestsGrantIsChosenOnlyAtTheAccessWritesProof(t *testing.T) {
	t.Parallel()
	choice := seataccess.GrantOptionTemporary("abcdefghij")
	for _, c := range []struct {
		name   string
		caller context.Context
		raise  bool
		saved  bool
		says   string
		token  bool
	}{
		{"an administrator within the window", grantAdmin(true, ""), false, true, "", false},
		{"an administrator raising, in person", grantAdmin(true, ""), true, true, "", false},
		{"an administrator past the window", grantAdmin(false, ""), false, false, "setup.access.write", false},
		{"a machine token raising", grantAdmin(true, "pat:0123"), true, false, "setup.access.raise", true},
		{"a machine token, read-only", grantAdmin(true, "pat:0123"), false, true, "", false},
		{"an agent holding every grant", everyGrant(), false, false, "setup.access.write", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			desk := newGrantDesk()
			desk.raise = c.raise
			got, trk := answerGrant(c.caller, t, desk, choice)
			if c.says == string(authz.ActionSetupAccessWrite) && len(desk.asked) != 0 {
				t.Errorf("a caller who may not save a grant had the save asked for: %v", desk.asked)
			}
			if saved := len(desk.saved) == 1; saved != c.saved || (len(trk.patched) == 1) != c.saved {
				t.Fatalf("saved %v with %d writes (%s), want saved and posted %v", saved,
					len(trk.patched), got.Output, c.saved)
			}
			if !c.saved && (!got.Failed || !strings.Contains(got.Output, c.says)) {
				t.Errorf("refusal %q does not name %s", got.Output, c.says)
			}
			var refused *builtin.DecisionRefused
			if c.token && (!errors.As(got.Cause, &refused) || refused.Decision.Reason != authz.ReasonTokenRefused ||
				!strings.Contains(got.Output, "needs the person here")) {
				t.Errorf("a token at a fresh proof was refused %v (%q), want token_refused", got.Cause, got.Output)
			}
		})
	}
}
