package mattermost_test

import (
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// peopleOrg is a company with one agent on Mattermost and two people: the
// founder, whose profile reads "Jane" (a username Mattermost stores as
// "jane"), and an advisor.
func peopleOrg(t *testing.T, founder, advisor string) *org.Organization {
	t.Helper()
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			{Name: "SWE", DeclaredHandle: "swe"},
			{Name: "Jane Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
				Contact: &org.HumanContact{MattermostUserID: founder}},
			{Name: "Ola Advisor", DeclaredHandle: "advisor", Kind: org.KindHuman,
				Contact: &org.HumanContact{MattermostUserID: advisor}},
		},
	}
	o.Normalize()
	return o
}

// peopleRegistry is the registry an apply builds for an org: indexed, with the
// human contacts reconciled in, exactly as the engine's refreshParties does.
func peopleRegistry(o *org.Organization) *notify.Registry {
	reg := notify.NewRegistry(o, nil)
	reg.ReconcileHumanContacts(o, func(string) (string, bool) { return "", false }, notify.Standing{})
	return reg
}

// peopleTransport is a started transport whose people are the org's.
func peopleTransport(t *testing.T, inst *instance, o *org.Organization, reg *notify.Registry,
	mutate func(*mattermost.TransportOptions),
) *mattermost.Transport {
	t.Helper()
	tr := transport(t, inst, func(opts *mattermost.TransportOptions) {
		opts.Config.People = mattermost.PeopleFrom(o, nil)
		opts.Registry = func() *notify.Registry { return reg }
		if mutate != nil {
			mutate(opts)
		}
	})
	if err := tr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return tr
}

func (i *instance) holds(name string, u mattermost.User) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.people == nil {
		i.people = map[string]mattermost.User{}
	}
	i.people[name] = u
}

func (i *instance) forgets(name string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.people, name)
}

func (i *instance) peopleAnswer(status int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.peopleStatus = status
}

func (i *instance) peopleAsked() []askedPeople {
	i.mu.Lock()
	defer i.mu.Unlock()
	return slices.Clone(i.asked)
}

// eventually waits for a condition the refresh loop brings about.
func eventually(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// THE BUG THIS EXISTS FOR. A person is declared by USERNAME and posts by USER
// ID, and the registry held the username — so every post a person wrote
// reached the agents as a stranger's, whatever the chart said about them. The
// transport asks the server which account holds the name and registers that
// account's id, before any socket is attached; and what reaches a seat then
// names the person as the colleague they are.
func TestAPersonsPostReachesTheAgentsFromTheirSeat(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe", IsBot: true},
	})
	inst.holds("jane", mattermost.User{ID: "u-jane", Username: "jane"})
	inst.holds("ola", mattermost.User{ID: "u-ola", Username: "ola"})
	o := peopleOrg(t, "Jane", "@ola")
	reg := peopleRegistry(o)
	tr := peopleTransport(t, inst, o, reg, nil)

	for id, handle := range map[string]string{"u-jane": "founder", "u-ola": "advisor"} {
		p, ok := reg.ByExternalID(mattermost.Backend, id)
		if !ok || p.Handle != handle || !p.Human {
			t.Fatalf("a post by %s names %+v, want the human seat %s", id, p, handle)
		}
	}
	// ONE request for every person, asking in the case the server stores —
	// it matches names exactly, so "Jane" would have found nobody.
	asked := inst.peopleAsked()
	if len(asked) != 1 {
		t.Fatalf("the people were asked about %d times at connect, want once: %+v",
			len(asked), asked)
	}
	if got := asked[0].names; !slices.Equal(got, []string{"jane", "ola"}) {
		t.Fatalf("the server was asked about %q", got)
	}

	// AND THROUGH THE WHOLE INBOUND PATH: the post the seat's socket hears
	// is parsed and rendered with the registry the engine hands the prompt.
	routed, err := tr.Parser().Parse(t.Context(), post(func(_, p map[string]any) {
		p["user_id"] = "u-jane"
	}), reg)
	if err != nil || len(routed) != 1 {
		t.Fatalf("Parse = %+v, %v", routed, err)
	}
	prompt := tr.Prompt().Build(routed[0].Inbound, reg)
	if !strings.Contains(prompt, "Jane Founder (founder, human colleague)") {
		t.Fatalf("the founder's post reads as a stranger's:\n%s", prompt)
	}
	if got := tr.PeopleFindings(); len(got) != 0 {
		t.Fatalf("people the server holds were reported: %+v", got)
	}
}

// A NAME NOBODY HOLDS IS SAID, in the log and in the integration's status,
// once — and a person who joins under it afterwards is recognised without a
// restart or an apply, on the next refresh.
//
// Not parallel: it reads the process-wide log.
func TestANameNobodyHoldsIsReportedUntilSomebodyJoinsUnderIt(t *testing.T) {
	logs := captureLogs(t)
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe", IsBot: true},
	})
	inst.holds("jane", mattermost.User{ID: "u-jane", Username: "jane"})
	o := peopleOrg(t, "jane", "ola")
	reg := peopleRegistry(o)
	var told atomic.Int32
	tr := peopleTransport(t, inst, o, reg, func(opts *mattermost.TransportOptions) {
		opts.PeopleRefresh = 20 * time.Millisecond
		opts.OnPeople = func() { told.Add(1) }
	})

	findings := tr.PeopleFindings()
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the advisor alone", findings)
	}
	f := findings[0]
	if f.Kind != integration.FindingContactUnknown || f.Subject != "advisor" {
		t.Fatalf("the finding reads %+v", f)
	}
	if !strings.Contains(f.Detail, `"ola"`) || f.Remedy == "" {
		t.Fatalf("the finding does not name the username and the fix: %+v", f)
	}
	if phase, actor := f.Kind.Verdict(); phase != integration.PhaseDegraded ||
		actor != integration.ActorOperator {
		t.Fatalf("an unknown person reads as %s, owed by %s", phase, actor)
	}
	if told.Load() != 1 {
		t.Fatalf("the status was told %d times about one change", told.Load())
	}
	// Asked again on every refresh, and said ONCE.
	eventually(t, "the server asked twice more", func() bool { return len(inst.peopleAsked()) >= 3 })
	if n := strings.Count(logs.String(), "mattermost_person_unknown"); n != 1 {
		t.Fatalf("the missing person was logged %d times:\n%s", n, logs)
	}

	// THE PERSON JOINS.
	inst.holds("ola", mattermost.User{ID: "u-ola", Username: "ola"})
	eventually(t, "the advisor recognised", func() bool {
		p, ok := reg.ByExternalID(mattermost.Backend, "u-ola")
		return ok && p.Handle == "advisor"
	})
	eventually(t, "the finding cleared", func() bool { return len(tr.PeopleFindings()) == 0 })
	eventually(t, "the status told again", func() bool { return told.Load() == 2 })
	if !strings.Contains(logs.String(), "mattermost_person_found") {
		t.Fatalf("the recovery was not said:\n%s", logs)
	}
}

// A RENAME TAKES THE NAME BACK. The chart says who the person is by name; once
// no account holds that name, the account it used to name is no longer this
// seat's — and the stale name is reported, which is the only way an operator
// learns the chart needs the new one.
func TestARenamedPersonsOldNameIsWithdrawnAndReported(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe", IsBot: true},
	})
	inst.holds("jane", mattermost.User{ID: "u-jane", Username: "jane"})
	inst.holds("ola", mattermost.User{ID: "u-ola", Username: "ola"})
	o := peopleOrg(t, "jane", "ola")
	reg := peopleRegistry(o)
	tr := peopleTransport(t, inst, o, reg, func(opts *mattermost.TransportOptions) {
		opts.PeopleRefresh = 20 * time.Millisecond
	})
	if _, ok := reg.ByExternalID(mattermost.Backend, "u-jane"); !ok {
		t.Fatal("the founder was not recognised to begin with")
	}

	inst.forgets("jane")
	inst.holds("jane.doe", mattermost.User{ID: "u-jane", Username: "jane.doe"})
	eventually(t, "the old name withdrawn", func() bool {
		_, ok := reg.ByExternalID(mattermost.Backend, "u-jane")
		return !ok
	})
	if got := reg.ExternalID(mattermost.Backend, "founder"); got != "" {
		t.Fatalf("the founder's seat still holds %q", got)
	}
	got := tr.PeopleFindings()
	if len(got) != 1 || got[0].Subject != "founder" {
		t.Fatalf("findings = %+v, want the founder's stale name", got)
	}
	// The advisor, whose name still holds, is untouched.
	if p, ok := reg.ByExternalID(mattermost.Backend, "u-ola"); !ok || p.Handle != "advisor" {
		t.Fatalf("a rename of one person moved another: %+v", p)
	}
}

// A FAILED ASK IS NOT AN ANSWER. A server that cannot say who anybody is says
// nothing about who anybody is: every person stays what the last answer made
// them, and nobody is reported.
func TestAnAskThatFailsChangesNothing(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe", IsBot: true},
	})
	inst.holds("jane", mattermost.User{ID: "u-jane", Username: "jane"})
	inst.holds("ola", mattermost.User{ID: "u-ola", Username: "ola"})
	o := peopleOrg(t, "jane", "ola")
	reg := peopleRegistry(o)
	tr := peopleTransport(t, inst, o, reg, func(opts *mattermost.TransportOptions) {
		opts.PeopleRefresh = 20 * time.Millisecond
	})

	// A status the client does not retry, so each ask fails at once.
	inst.peopleAnswer(http.StatusInternalServerError)
	after := len(inst.peopleAsked())
	eventually(t, "two failed asks", func() bool { return len(inst.peopleAsked()) >= after+2 })

	for id, handle := range map[string]string{"u-jane": "founder", "u-ola": "advisor"} {
		if p, ok := reg.ByExternalID(mattermost.Backend, id); !ok || p.Handle != handle {
			t.Fatalf("a failed ask made %s a stranger: %+v", handle, p)
		}
	}
	if got := tr.PeopleFindings(); len(got) != 0 {
		t.Fatalf("a failed ask was reported as people nobody holds: %+v", got)
	}
}

// BEFORE ANY ANSWER THERE IS NOTHING TO REPORT. A server that could not be
// asked at connect has not said that nobody holds a name, and a status
// claiming it would send an operator to correct a chart that is right.
func TestNothingIsReportedBeforeTheServerHasAnswered(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe", IsBot: true},
	})
	inst.peopleAnswer(http.StatusInternalServerError)
	o := peopleOrg(t, "jane", "ola")
	tr := peopleTransport(t, inst, o, peopleRegistry(o), nil)
	if got := tr.PeopleFindings(); len(got) != 0 {
		t.Fatalf("an unanswered ask was reported: %+v", got)
	}
}

// AN APPLY FILLS ITS NEW REGISTRY FROM WHAT IS KNOWN, AT ONCE, and then asks.
// The registry is rebuilt on every apply, so a transport that waited for the
// server before registering anyone made every person a stranger for as long
// as the question took — and a person the apply ADDS is asked about without
// waiting for the next refresh.
func TestAnApplyRegistersThePeopleBeforeItAsksAgain(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe", IsBot: true},
	})
	inst.holds("jane", mattermost.User{ID: "u-jane", Username: "jane"})
	o := peopleOrg(t, "jane", "ola")
	tr := peopleTransport(t, inst, o, peopleRegistry(o), func(opts *mattermost.TransportOptions) {
		// Never on its own, so only the apply can make it ask.
		opts.PeopleRefresh = time.Hour
	})

	// THE SERVER STOPS ANSWERING, and the apply arrives.
	inst.peopleAnswer(http.StatusInternalServerError)
	next := peopleOrg(t, "jane", "ola.new")
	reg := peopleRegistry(next)
	asked := len(inst.peopleAsked())
	tr.Reregister(reg, mattermost.PeopleFrom(next, nil))
	if p, ok := reg.ByExternalID(mattermost.Backend, "u-jane"); !ok || p.Handle != "founder" {
		t.Fatalf("the new registry did not get the founder from what was known: %+v", p)
	}

	// And the apply's new name is asked about now, not in an hour.
	inst.peopleAnswer(0)
	inst.holds("ola.new", mattermost.User{ID: "u-ola", Username: "ola.new"})
	eventually(t, "the apply asked", func() bool { return len(inst.peopleAsked()) > asked })
	eventually(t, "the new name registered", func() bool {
		p, ok := reg.ByExternalID(mattermost.Backend, "u-ola")
		return ok && p.Handle == "advisor"
	})
	if got := inst.peopleAsked(); !slices.Contains(got[len(got)-1].names, "ola.new") {
		t.Fatalf("the last ask was about %q", got[len(got)-1].names)
	}
}

// A SUSPENDED PERSON'S ACCOUNT ROUTES NOWHERE. The identity directory, not the
// chart, says whether a seat's holder may still be reached, and a registry
// built from a reading that withholds the founder leaves out every contact
// identity of theirs — the account their name resolves to included, or a
// suspended founder's posts would still reach the agents with the founder's
// authority. Nor is a withheld seat reported: a name holding nobody owes the
// operator nothing while its owner is not to be reached at all. The
// reinstatement is a new registry, and it gets the founder from the last
// answer at once.
func TestAPersonTheDirectoryWithholdsIsNotRegistered(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe", IsBot: true},
	})
	inst.holds("jane", mattermost.User{ID: "u-jane", Username: "jane"})
	inst.holds("ola", mattermost.User{ID: "u-ola", Username: "ola"})
	o := peopleOrg(t, "jane", "ola")
	withheld := func(holder notify.Holder) *notify.Registry {
		reg := notify.NewRegistry(o, nil)
		reg.ReconcileHumanContacts(o, func(string) (string, bool) { return "", false },
			notify.StandingOf([]notify.Holder{holder}))
		return reg
	}
	reg := withheld(notify.Holder{Seat: "founder", Stage: iam.StageSuspended})
	tr := peopleTransport(t, inst, o, reg, func(opts *mattermost.TransportOptions) {
		// Never on its own, so what the reinstatement registers is what
		// was already known.
		opts.PeopleRefresh = time.Hour
	})

	if p, ok := reg.ByExternalID(mattermost.Backend, "u-jane"); ok {
		t.Fatalf("a suspended founder's post names %+v, want a stranger", p)
	}
	if p, ok := reg.ByExternalID(mattermost.Backend, "u-ola"); !ok || p.Handle != "advisor" {
		t.Fatalf("withholding the founder withheld %+v too", p)
	}
	// ASKED ALL THE SAME, so the reinstatement has an answer to register.
	if got := inst.peopleAsked(); len(got) != 1 || !slices.Contains(got[0].names, "jane") {
		t.Fatalf("the asks were %+v, want the founder's name among one", got)
	}

	// THE REINSTATEMENT is a new registry, filled from the last answer
	// before anything is asked again.
	reinstated := peopleRegistry(o)
	tr.Reregister(reinstated, mattermost.PeopleFrom(o, nil))
	if p, ok := reinstated.ByExternalID(mattermost.Backend, "u-jane"); !ok || p.Handle != "founder" {
		t.Fatalf("the reinstated founder's post names %+v, want the founder", p)
	}

	// A NAME NOBODY HOLDS IS REPORTED WHILE A REGISTRY ROUTES ITS SEAT —
	// and not for a seat nobody may reach.
	inst.forgets("jane")
	tr.Reregister(peopleRegistry(o), mattermost.PeopleFrom(o, nil))
	eventually(t, "the founder reported", func() bool {
		got := tr.PeopleFindings()
		return len(got) == 1 && got[0].Subject == "founder"
	})
	tr.Reregister(withheld(notify.Holder{Seat: "founder", Removed: true}), mattermost.PeopleFrom(o, nil))
	if got := tr.PeopleFindings(); len(got) != 0 {
		t.Fatalf("findings = %+v, want none about a removed founder", got)
	}
}

// A BOT'S NAME IS NOT A PERSON. Registered, it would hand that bot's every
// post to the person — an agent's words presented to its colleagues as the
// founder's, with the founder's authority — so it is refused and reported.
func TestANameABotHoldsIsNotRegisteredForAPerson(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-swe": {ID: "bot-swe", Username: "agent-swe", IsBot: true},
	})
	inst.holds("agent-swe", mattermost.User{ID: "bot-swe", Username: "agent-swe", IsBot: true})
	inst.holds("ola", mattermost.User{ID: "u-ola", Username: "ola"})
	o := peopleOrg(t, "agent-swe", "ola")
	reg := peopleRegistry(o)
	tr := peopleTransport(t, inst, o, reg, nil)

	if p, _ := reg.ByExternalID(mattermost.Backend, "bot-swe"); p.Handle != "swe" {
		t.Fatalf("the bot's posts are attributed to %+v, want the agent", p)
	}
	if got := reg.ExternalID(mattermost.Backend, "founder"); got != "" {
		t.Fatalf("the founder was registered as the bot %q", got)
	}
	got := tr.PeopleFindings()
	if len(got) != 1 || got[0].Subject != "founder" || !strings.Contains(got[0].Detail, "bot") {
		t.Fatalf("findings = %+v, want the founder named by a bot's account", got)
	}
}

// THE NEXT SEAT ASKS WHEN THE SERVER REFUSES ONE, as the instance read does: a
// refused token is a fact about that bot, and another bot can still say who
// the people are.
func TestTheNextSeatAsksWhenOnesTokenIsRefused(t *testing.T) {
	inst := newInstance(t, map[string]mattermost.User{
		"tok-a": {ID: "bot-a", Username: "agent-a", IsBot: true},
		"tok-b": {ID: "bot-b", Username: "agent-b", IsBot: true},
	})
	inst.refusesPeople = map[string]bool{"tok-a": true}
	inst.holds("jane", mattermost.User{ID: "u-jane", Username: "jane"})
	o := peopleOrg(t, "jane", "ola")
	reg := peopleRegistry(o)
	peopleTransport(t, inst, o, reg, func(opts *mattermost.TransportOptions) {
		opts.Config.Seats = []mattermost.SeatConfig{
			{Handle: "a", Token: "tok-a"}, {Handle: "b", Token: "tok-b"},
		}
	})
	if p, ok := reg.ByExternalID(mattermost.Backend, "u-jane"); !ok || p.Handle != "founder" {
		t.Fatalf("one refused token left the founder a stranger: %+v", p)
	}
	tokens := make([]string, 0, 2)
	for _, a := range inst.peopleAsked() {
		tokens = append(tokens, a.token)
	}
	if !slices.Equal(tokens, []string{"tok-a", "tok-b"}) {
		t.Fatalf("the people were asked on %v, want the refused token then the next", tokens)
	}
}

// The people are read from the HUMAN seats, through the same resolution every
// consumer of a contact uses, normalised as the server stores a name — and a
// ${VAR} that does not resolve is left out rather than asked about as its
// literal text.
func TestPeopleAreReadFromTheHumanSeats(t *testing.T) {
	o := peopleOrg(t, " @Jane ", "${ADVISOR_MM}")
	got := mattermost.PeopleFrom(o, func(string) (string, bool) { return "", false })
	if want := []mattermost.Person{{Handle: "founder", Username: "jane"}}; !slices.Equal(got, want) {
		t.Fatalf("PeopleFrom = %+v, want %+v", got, want)
	}
	got = mattermost.PeopleFrom(o, func(name string) (string, bool) {
		return "Ola", name == "ADVISOR_MM"
	})
	if len(got) != 2 || got[1] != (mattermost.Person{Handle: "advisor", Username: "ola"}) {
		t.Fatalf("PeopleFrom with the variable set = %+v", got)
	}
	if mattermost.PeopleFrom(nil, nil) != nil {
		t.Fatal("PeopleFrom(nil) found people")
	}
}
