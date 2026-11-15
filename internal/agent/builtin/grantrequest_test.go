package builtin_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	_ "github.com/crewlet/crewlet/internal/cloudflare" // registers the vendor whose lane takes grants
	"github.com/crewlet/crewlet/internal/decision"
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
