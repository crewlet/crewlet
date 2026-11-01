package stream

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tokens"
)

// The audiences these cases are about, as the guard would compose them.
var (
	adminPrincipal = iam.Principal{Login: "erin.ng", Stage: iam.StageActive,
		Seat: "founder", Grants: []iam.Grant{iam.GrantAdmin}}
	workletPrincipal = iam.Principal{Login: "sam.lee", Stage: iam.StageActive,
		Seat: "analyst", Grants: []iam.Grant{iam.GrantWorklet}}
	overviewPrincipal = iam.Principal{Login: "pat.kim", Stage: iam.StageActive,
		Seat: "intern", Grants: []iam.Grant{}}
)

// EVERY KIND SAYS WHO MAY READ IT, exactly once.
//
// A kind in neither table is received by nobody — the closed end, and a push
// that silently never arrives. A kind in both is two answers to one question.
// And a need this build does not know is one no audience meets, so it would be
// the same silent nothing as an absent row.
//
// Mutation: drop a kind from needs, or declare one with a need outside
// [Needs], and this fails.
func TestEveryPushKindDeclaresWhoMayReadIt(t *testing.T) {
	t.Parallel()
	for _, kind := range slices.Sorted(maps.Keys(routes)) {
		need, gated := needs[kind]
		switch {
		case gated && answers[kind]:
			t.Errorf("%q is both declared for %s and an answer to the socket's "+
				"own exchange; it is one or the other", kind, need)
		case !gated && !answers[kind]:
			t.Errorf("%q declares no reader: add it to needs with the need its "+
				"question takes, or to answers if it says nothing about the "+
				"company", kind)
		case gated && !need.Valid():
			t.Errorf("%q is declared for %q, which no audience meets", kind, need)
		}
	}
	for kind := range needs {
		if _, routed := routes[kind]; !routed {
			t.Errorf("needs names %q, which is not a push kind", kind)
		}
	}
	for kind := range answers {
		if _, routed := routes[kind]; !routed {
			t.Errorf("answers names %q, which is not a push kind", kind)
		}
	}
}

// AN EVENT IS THE `events` QUESTION'S ROW, so it reaches only an ADMIN — the
// company-wide trail is theirs alone — and the roster beside it reaches
// everybody, a principal holding no permission at all included.
//
// Mutation: declare KindEvent everybody's, or KindAgents admin's, and this
// fails.
func TestAnEventReachesOnlyAnAdmin(t *testing.T) {
	t.Parallel()
	h := NewHub()
	defer h.Close()
	admin := NewClient(NewAudience(adminPrincipal, nil))
	workletReader := NewClient(NewAudience(workletPrincipal, nil))
	overview := NewClient(NewAudience(overviewPrincipal, nil))
	nobody := NewClient(Audience{})
	for _, c := range []*Client{admin, workletReader, overview, nobody} {
		h.Register(c)
	}
	at := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	h.Broadcast(Push(KindEvent, map[string]any{"type": "agent_phase_completed"}, at))
	h.Broadcast(Push(KindAgents, []any{}, at))

	for _, tc := range []struct {
		name   string
		client *Client
		want   []Kind
	}{
		{"an admin", admin, []Kind{KindEvent, KindAgents}},
		{"Worklet alone", workletReader, []Kind{KindAgents}},
		{"no permission at all", overview, []Kind{KindAgents}},
		{"the zero audience", nobody, nil},
	} {
		var got []Kind
		for _, f := range drainClient(tc.client) {
			got = append(got, f.kind)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s received %v, want %v", tc.name, got, tc.want)
		}
	}
}

// AN INBOX MOVEMENT IS THE TRACKER'S, as the `work_inbox` question it tells a
// screen to ask again is: a reader without Worklet receives none however
// they watch.
//
// Mutation: declare KindInboxChanged everybody's, and the overview reader
// receives the frame.
func TestAnInboxMovementReachesOnlyWorklet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		p    iam.Principal
		want bool
	}{
		{"an admin", adminPrincipal, true},
		{"Worklet", workletPrincipal, true},
		{"no permission at all", overviewPrincipal, false},
	} {
		if got := NewAudience(tc.p, nil).Receives(KindInboxChanged); got != tc.want {
			t.Errorf("%s receives inbox_changed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// THE SNAPSHOT IS WHAT THE PUSHES WOULD BE, key by key.
//
// Every key the service builds names the push it stands in for, and a reader
// receives in the handshake exactly what it would receive afterwards: an
// admin every key, everybody else every key but the audit feed, and the zero
// audience nothing.
func TestTheSnapshotCarriesOnlyWhatItsAudienceMayRead(t *testing.T) {
	t.Parallel()
	svc := newServiceOver(t, authz.NoChart{}, func(string) (SeatState, bool) {
		return SeatState{}, false
	})

	every := svc.Snapshot(NewAudience(adminPrincipal, nil))
	for key := range every {
		if _, ok := snapshotKinds[key]; !ok {
			t.Errorf("the snapshot builds %q, which names no push kind — no "+
				"reader can ever receive it", key)
		}
	}
	if len(every) != len(snapshotKinds) {
		t.Errorf("an admin's audience received %d of the %d snapshot keys",
			len(every), len(snapshotKinds))
	}

	overview := svc.Snapshot(NewAudience(overviewPrincipal, nil))
	if _, ok := overview["events"]; ok {
		t.Error("an overview snapshot carries the event feed, which the " +
			"events question keeps to an admin")
	}
	for _, key := range []string{"agents", "org", "tools", "schedules", "executors",
		"tokens", "budget", "health"} {
		if _, ok := overview[key]; !ok {
			t.Errorf("an overview snapshot is missing %q, which is everybody's", key)
		}
	}
	if got := svc.Snapshot(Audience{}); len(got) != 0 {
		t.Errorf("the zero audience received %v; it is the closed end",
			slices.Sorted(maps.Keys(got)))
	}
}

// nodeHealth stands in for `api.Health`: the one field a socket's posture is
// decided from, and nothing else this package reads.
type nodeHealth struct {
	Status string `json:"status"`
}

func (h nodeHealth) NodeStatus() string { return h.Status }

// drainClient reads everything currently queued for a client.
func drainClient(c *Client) []*Frame {
	var out []*Frame
	for {
		select {
		case frame, ok := <-c.Out():
			if !ok {
				return out
			}
			out = append(out, frame)
		default:
			return out
		}
	}
}

// A REPLY NEVER CARRIES A FACT ITS CLIENT MAY NOT READ, even from a path that
// forgot to build it for the client's audience: the direct path is gated by
// the same table as the broadcast, while the socket's own answers pass.
func TestAReplyNeverCarriesAFactItsClientMayNotRead(t *testing.T) {
	t.Parallel()
	c := NewClient(NewAudience(workletPrincipal, nil))
	c.Reply(Push(KindEvent, map[string]any{"type": "x"}, time.Now()))
	c.Reply(Envelope{Kind: KindResult, ID: 1, What: "anything"})
	var got []Kind
	for _, f := range drainClient(c) {
		got = append(got, f.kind)
	}
	if !slices.Equal(got, []Kind{KindResult}) {
		t.Fatalf("a client who is not an admin was replied %v, want only its "+
			"own answer", got)
	}
}

// shapedOrg answers an admin audience a field nobody else may read, and every
// other audience the narrow shape.
type shapedOrg struct{}

func (shapedOrg) For(a Audience) (string, any) {
	if a.Admin() {
		return "admin", map[string]any{"runs_on": "a provider key"}
	}
	return "overview", map[string]any{}
}

// A SHAPED PAYLOAD REACHES EACH AUDIENCE IN THE SHAPE IT DESCRIBES — on the
// broadcast and on the direct path alike, and encoded once per shape rather
// than once per client.
//
// The need table decides whether a reader receives a kind at all; one kind can
// still carry a field its readers may not all see — a seat's resolved model
// chain on the org tree everybody reads is derived from the seat's runtime
// settings, which only an admin reads. Handed the payload whole, every reader
// of the tree would read it.
//
// Mutation: encode the broadcast once per posture, ignoring the variant, and
// the overview reader is handed the admin's field.
func TestAShapedPayloadReachesEachAudienceInItsOwnShape(t *testing.T) {
	t.Parallel()
	h := NewHub()
	reader := NewClient(NewAudience(workletPrincipal, nil))
	admin := NewClient(NewAudience(adminPrincipal, nil))
	// The admin registers FIRST, so a cache keyed on the posture alone
	// would hand the reader the admin's encoding.
	h.Register(admin)
	h.Register(reader)
	at := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	h.Broadcast(Push(KindOrg, shapedOrg{}, at))
	reader.Reply(Push(KindOrg, shapedOrg{}, at))
	admin.Reply(Push(KindOrg, shapedOrg{}, at))

	for name, tc := range map[string]struct {
		client *Client
		holds  bool
	}{
		"Worklet alone": {reader, false},
		"an admin":      {admin, true},
	} {
		frames := drainClient(tc.client)
		if len(frames) != 2 {
			t.Fatalf("%s received %d frames, want the broadcast and the reply", name, len(frames))
		}
		for _, f := range frames {
			if got := strings.Contains(string(f.Raw()), "a provider key"); got != tc.holds {
				t.Errorf("%s received %s: carries the admin's field = %v, want %v",
					name, f.Raw(), got, tc.holds)
			}
		}
	}
}

// leadsAgent is a chart in which the seat `lead` leads every seat in seats and
// nobody else leads anybody.
type leadsAgent struct {
	authz.NoChart
	seats map[string]bool
}

func (c leadsAgent) Leads(_ context.Context, actor, subject string) (bool, error) {
	return actor == "lead" && c.seats[subject], nil
}

// WHOSE TRAIL A READER READS IS THE AUTHORITY TABLE'S `audit.seat.read`, seat
// by seat: an admin every seat's, a lead their own seats', everybody else none
// — and an undecidable chart none at all, because a transcript is withheld
// rather than guessed at.
//
// Mutation: put every agent seat in the body set, or read an unknown decision
// as body, and this fails.
func TestAnAudienceReadsTheTrailOfTheSeatsTheTableAdmits(t *testing.T) {
	t.Parallel()
	seats := tokens.Seats{
		"agent-eng":           {Handle: "eng-bot"},
		"agent-ops":           {Handle: "ops-bot"},
		tokens.PersonKey("x"): {Handle: "x"},
	}
	build := func(chart authz.Chart) *Service {
		svc, err := NewService(livestate.New(), Options{
			Health:    func() Health { return nodeHealth{Status: "ok"} },
			Posture:   func(Health) FramePosture { return FrameLive },
			Seats:     func() tokens.Seats { return seats },
			Roster:    func() []map[string]any { return nil },
			Org:       func() any { return map[string]any{} },
			Tools:     func() []map[string]any { return nil },
			Schedules: func() any { return []any{} },
			Placement: func() (map[string]bool, error) { return map[string]bool{}, nil },
			Chart:     chart,
			Holders:   blindHolders{},
			SeatOf:    func(string) (SeatState, bool) { return SeatState{}, false },
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(svc.Stop)
		return svc
	}
	chart := leadsAgent{seats: map[string]bool{"eng-bot": true}}
	svc := build(chart)
	lead := iam.Principal{Login: "lee.lead", Stage: iam.StageActive, Seat: "lead",
		Grants: []iam.Grant{iam.GrantWorklet}}

	for _, tc := range []struct {
		name string
		a    Audience
		want map[string]bool
	}{
		{"an admin", svc.AudienceFor(t.Context(), adminPrincipal),
			map[string]bool{"agent-eng": true, "agent-ops": true}},
		{"the eng seat's lead", svc.AudienceFor(t.Context(), lead),
			map[string]bool{"agent-eng": true, "agent-ops": false}},
		{"anybody else", svc.AudienceFor(t.Context(), workletPrincipal),
			map[string]bool{"agent-eng": false, "agent-ops": false}},
		{"a lead on a node whose chart cannot answer",
			build(authz.NoChart{}).AudienceFor(t.Context(), lead),
			map[string]bool{"agent-eng": false, "agent-ops": false}},
	} {
		for agent, want := range tc.want {
			if got := tc.a.Body(agent); got != want {
				t.Errorf("%s reads %s's body = %v, want %v", tc.name, agent, got, want)
			}
		}
	}
}

// TWO AUDIENCES DIFFERING IN WHAT THEY MAY READ NEVER SHARE A VARIANT, so the
// hub's one encode per variant never hands one reader another's frame — and
// two that read the same thing share one, so it stays one encode.
//
// Mutation: drop the body set, or the apps, from the variant's name, and a
// lead's frame reaches somebody outside their team.
func TestAudiencesThatReadDifferentlyNeverShareAVariant(t *testing.T) {
	t.Parallel()
	lead := NewAudience(workletPrincipal, []string{"agent-eng"})
	otherLead := NewAudience(workletPrincipal, []string{"agent-ops"})
	overview := NewAudience(overviewPrincipal, nil)
	trackerOnly := NewAudience(workletPrincipal, nil)
	for _, pair := range [][2]Audience{{lead, otherLead}, {lead, trackerOnly},
		{trackerOnly, overview}} {
		if pair[0].variant(false) == pair[1].variant(false) {
			t.Errorf("two audiences that read differently share the variant %q",
				pair[0].variant(false))
		}
	}
	twin := NewAudience(iam.Principal{Login: "sam.other", Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantWorklet}}, []string{"agent-eng"})
	if lead.variant(false) != twin.variant(false) {
		t.Errorf("two readers of exactly one seat's body were given two variants, "+
			"%q and %q, which costs an encode each", lead.variant(false),
			twin.variant(false))
	}
}

// EVERY APP IS A NEED AN AUDIENCE MEETS, and each one apart from the others.
//
// The app terms are derived from [iam.Apps] rather than written out per app,
// so the next app needs no edit here — which holds only if every one of them
// is a need this build declares, a holder of exactly that app has it and no
// other, the refusal of its kinds names it, and two readers differing only in
// it are two audiences with two variants: compared equal they would keep a
// socket whose grants moved, and sharing a variant one encode would hand one
// reader the other's frame.
//
// Mutation: drop an app from [NewAudience]'s loop, from [Audience.Equal] or
// from the variant's name, or answer [Audience.Has] from the first app, and
// this fails.
func TestEveryAppIsANeedAnAudienceMeets(t *testing.T) {
	t.Parallel()
	nobody := NewAudience(overviewPrincipal, nil)
	for _, app := range iam.Apps {
		need := NeedFor(app)
		if !need.Valid() {
			t.Errorf("the %s app is not a need this build declares", app)
		}
		if got := need.Grants(); !slices.Equal(got, []iam.Grant{app}) {
			t.Errorf("a refusal of a %s kind names %v, want %s", app, got, app)
		}
		holder := NewAudience(iam.Principal{Login: "holder", Stage: iam.StageActive,
			Seat: "holder", Grants: []iam.Grant{app}}, nil)
		for _, other := range iam.Apps {
			if got := holder.Has(other); got != (other == app) {
				t.Errorf("a holder of %s alone has %s = %v", app, other, got)
			}
		}
		if holder.Equal(nobody) {
			t.Errorf("a holder of %s is the same audience as a holder of nothing", app)
		}
		if holder.variant(false) == nobody.variant(false) {
			t.Errorf("a holder of %s shares the variant %q with a holder of nothing",
				app, holder.variant(false))
		}
	}
	admin := NewAudience(adminPrincipal, nil)
	for _, app := range iam.Apps {
		if !admin.Has(app) {
			t.Errorf("an administrator does not have %s", app)
		}
	}
	if admin.Has(iam.Grant("timesheets")) || nobody.Has(iam.GrantAdmin) {
		t.Error("an audience has something that is not an app it holds")
	}
}

// A CODING RUN'S QUESTION NAMES ITS READER IN THE VARIANT, AND NOTHING ELSE
// DOES.
//
// A run parked on a question shows it to the seats it is put to whatever their
// reach to the seat that asked, so two readers with the same grants and the
// same body set receive two different payloads when one of them is asked and
// the other is not — and the hub encodes once per variant, so the two must not
// share one. With no run parked on a question the reader's record changes
// nothing, and the two share one encode.
//
// Mutation: drop the record from the variant's name, or name it on every
// sandbox push, and this fails.
func TestACodingRunsQuestionNamesItsReaderInTheVariant(t *testing.T) {
	t.Parallel()
	person := func(login, seat string) Audience {
		return NewAudience(iam.Principal{Login: login, Kind: iam.KindPerson,
			Seat: seat, Stage: iam.StageActive,
			Grants: []iam.Grant{iam.GrantWorklet}}, nil)
	}
	asked, bystander := person("sam.lee", "analyst"), person("kim.ray", "auditor")

	parked := ExecutorRows{{TurnID: "turn-1", AgentID: "agent-eng", Task: "fix it",
		Question: "which branch?", AudienceHandles: []string{"analyst"}}}
	askedName, askedRows := parked.For(asked)
	byName, byRows := parked.For(bystander)
	if askedName == byName {
		t.Errorf("the reader a question is put to and one it is not share the "+
			"variant %q, so one encode hands both the same payload", askedName)
	}
	if got := askedRows.([]livestate.ExecutorEntry)[0].Question; got != "which branch?" {
		t.Errorf("the reader the question is put to was shown %q", got)
	}
	if got := byRows.([]livestate.ExecutorEntry)[0].Question; got != "" {
		t.Errorf("a reader the question is not put to was shown %q", got)
	}

	running := ExecutorRows{{TurnID: "turn-2", AgentID: "agent-eng", Task: "fix it",
		AudienceHandles: []string{"analyst"}}}
	if a, b := runningName(running, asked), runningName(running, bystander); a != b {
		t.Errorf("with no run parked on a question two readers of the same reach "+
			"were given two variants, %q and %q, which costs an encode each", a, b)
	}
}

// runningName is the variant rows are pushed to audience under.
func runningName(rows ExecutorRows, audience Audience) string {
	name, _ := rows.For(audience)
	return name
}

// THE SANDBOXES PUSH NAMES THE ENTRY A RUN WAS LAUNCHED ON TO AN ADMIN ALONE.
// Which `providers.llm` entry a coding run spends, the chain's head or the
// fallback a launch took, is the company's configuration: everybody else, the
// seat's lead included, is sent the run and the model it runs under without
// it. The push and the snapshot both shape the rows through here.
//
// Mutation: drop the configuration rule from the coding run's mask, or reach
// it with no Config for an admin, and a reader is handed the entry or an
// admin is not.
func TestTheSandboxesPushNamesARunsEntryToAnAdminAlone(t *testing.T) {
	t.Parallel()
	rows := ExecutorRows{{TurnID: "turn-1", AgentID: "agent-eng", Task: "fix it",
		ProviderKey: "plan-zulu", Model: "opus"}}
	lead := iam.Principal{Login: "ana.ruiz", Stage: iam.StageActive, Seat: "eng-lead",
		Grants: []iam.Grant{iam.GrantWorklet}}
	for name, tc := range map[string]struct {
		audience Audience
		entry    bool
	}{
		"an admin":        {NewAudience(adminPrincipal, nil), true},
		"the seat's lead": {NewAudience(lead, []string{"agent-eng"}), false},
		"a tracker user":  {NewAudience(workletPrincipal, nil), false},
		"the overview":    {NewAudience(overviewPrincipal, nil), false},
	} {
		_, data := rows.For(tc.audience)
		got := data.([]livestate.ExecutorEntry)
		if len(got) != 1 || (got[0].ProviderKey == "plan-zulu") != tc.entry || got[0].Model != "opus" {
			t.Errorf("%s is sent %+v, want the entry shown %v and the model always", name, got, tc.entry)
		}
	}
	if rows[0].ProviderKey != "plan-zulu" {
		t.Error("shaping the push wrote through to the rows the broadcast shares")
	}
}

// THE AUDIENCE HOLDS EXACTLY THE APPS THE PRINCIPAL CAN.
//
// It held one field per app and a switch over them, so an app added to the
// vocabulary reached no socket reader until somebody remembered a second edit
// here. Walked over EVERY combination of the vocabulary, a name this build
// does not know included: the audience uses exactly the apps
// [iam.Principal.Can] says (an administrator every one), uses nothing that is
// not an app, and receives the tracker's kinds exactly when it uses the
// tracker. And two readers whose apps differ never share a variant nor
// compare equal, so a broadcast encoded once per variant never hands one the
// other's frame and a reader who lost an app is closed for the reconnect.
//
// Mutation: drop an app from NewAudience's loop, or answer Has from the admin
// flag alone for an unknown name, and this fails.
func TestTheAudienceHoldsExactlyTheAppsThePrincipalCan(t *testing.T) {
	t.Parallel()
	vocab := append(slices.Clone(iam.AllGrants), iam.Grant("future"))
	seen := map[string]Audience{}
	for mask := range 1 << len(vocab) {
		var grants []iam.Grant
		for i, g := range vocab {
			if mask&(1<<i) != 0 {
				grants = append(grants, g)
			}
		}
		p := iam.Principal{Login: "sam.lee", Kind: iam.KindPerson, Stage: iam.StageActive,
			Seat: "analyst", Grants: grants}
		a := NewAudience(p, nil)
		for _, g := range vocab {
			if got, want := a.Has(g), g.IsApp() && p.Can(g); got != want {
				t.Errorf("%v: Has(%q) = %v, want %v", grants, g, got, want)
			}
		}
		if a.Admin() != p.Can(iam.GrantAdmin) {
			t.Errorf("%v: Admin() = %v", grants, a.Admin())
		}
		if got := a.Receives(KindInboxChanged); got != p.Can(iam.GrantWorklet) {
			t.Errorf("%v: receives Worklet's inbox movement = %v", grants, got)
		}
		var held []string
		for _, app := range iam.Apps {
			if p.Can(app) {
				held = append(held, string(app))
			}
		}
		key := strings.Join(held, ",")
		if p.Can(iam.GrantAdmin) {
			key = "admin"
		}
		for other, b := range seen {
			if other == key {
				continue
			}
			if a.variant(false) == b.variant(false) {
				t.Errorf("readers using %q and %q share the variant %q", key, other,
					a.variant(false))
			}
			if a.Equal(b) {
				t.Errorf("readers using %q and %q compare equal", key, other)
			}
		}
		seen[key] = a
	}
	// EVERY APP ON ITS OWN reaches the audience, which is the half that
	// proves the set is the vocabulary's and not a list kept here.
	for _, app := range iam.Apps {
		a := NewAudience(iam.Principal{Login: "sam.lee", Stage: iam.StageActive,
			Grants: []iam.Grant{app}}, nil)
		for _, other := range iam.Apps {
			if got := a.Has(other); got != (other == app) {
				t.Errorf("holding only %q: Has(%q) = %v", app, other, got)
			}
		}
	}
	if (Audience{}).Has(iam.GrantWorklet) {
		t.Error("the zero audience uses an app")
	}
}

// THE AUDIENCE KEEPS WHO IT WAS DESCRIBED FOR, AND NEVER SHARES IT.
//
// An artifact room is decided for the principal a socket was opened by, so
// the audience carries it and compares it: a reader re-bound to another seat
// is a different reader even where every app is the same, and a step-up that
// only moved how recently they proved who they are is the same reader. The
// grants handed out are a copy, so a caller appending to them reaches no
// other client. Mutation: drop the principal from Equal, or return the
// principal without cloning its grants, and this fails.
func TestTheAudienceKeepsItsPrincipalAndComparesIt(t *testing.T) {
	t.Parallel()
	p := iam.Principal{Login: "sam.lee", Kind: iam.KindPerson, Stage: iam.StageActive,
		Seat: "analyst", Grants: []iam.Grant{iam.GrantWorklet}}
	a := NewAudience(p, nil)
	if got := a.Principal(); got.Login != p.Login || got.Seat != p.Seat ||
		!slices.Equal(got.Grants, p.Grants) {
		t.Fatalf("the audience describes %+v, want %+v", got, p)
	}
	held := a.Principal()
	held.Grants = append(held.Grants[:0], iam.GrantAdmin)
	if a.Principal().Grants[0] != iam.GrantWorklet {
		t.Error("editing the grants a caller was handed reached the audience")
	}
	p.Grants[0] = iam.GrantAdmin
	if a.Principal().Grants[0] != iam.GrantWorklet {
		t.Error("editing the caller's own principal reached the audience it built")
	}
	p.Grants[0] = iam.GrantWorklet

	stepped := p
	stepped.ReauthAt = time.Date(2026, 6, 14, 13, 0, 0, 0, time.UTC)
	if !NewAudience(stepped, nil).Equal(a) {
		t.Error("a step-up changed the audience, so a socket would close on every " +
			"proof of identity")
	}
	moved := p
	moved.Seat = "auditor"
	if NewAudience(moved, nil).Equal(a) {
		t.Error("a reader re-bound to another seat compares equal, so the rooms " +
			"decided for the old seat would stay open")
	}
	// THE SAME SEAT, SOMEBODY ELSE: the record and the apps are one, so only
	// the principal tells the two apart.
	other := p
	other.Login = "kim.ray"
	if NewAudience(other, nil).Equal(a) {
		t.Error("another person on the same seat compares equal")
	}
	if (Audience{}).Principal().Kind != "" {
		t.Error("the zero audience describes somebody")
	}
}
