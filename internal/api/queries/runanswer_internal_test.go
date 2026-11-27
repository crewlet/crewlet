package queries

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runanswer"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// standingCompany is a founder who leads the unit the product manager and the
// release manager sit in, a colleague the merge rule escalates to, and a
// person who neither leads nor is named.
const standingCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - {name: Ana, handle: ana, kind: human}
  - {name: Bo, handle: bo, kind: human}
units:
  - name: Product
    id: product
    lead: founder
    roles:
      - {name: Founder, handle: founder, kind: human}
      - {name: Product Manager, handle: pm, llm: zulu}
      - {name: Release Manager, handle: release-manager, llm: zulu}
delegated_decisions:
  enabled: true
  rules:
    - {kinds: [pr_merge], delegates: [release-manager], escalate_to: [ana]}
`

// standingSources is standingCompany's sources at now, with the real chart
// the answer routes decide on.
func standingSources(t *testing.T, now time.Time) Sources {
	t.Helper()
	company, err := config.ParseCompany([]byte(standingCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	o, err := company.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}
	return Sources{Company: func() (*config.Company, *org.Organization) { return company, o },
		Chart: orgchart.Of(o), Now: func() time.Time { return now }}
}

// asPerson is a signed-in person on seat (none for ""), with grants.
func asPerson(t *testing.T, login, seat string, kind iam.Kind, grants ...iam.Grant) context.Context {
	t.Helper()
	return iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.NewSHA1(uuid.Nil, []byte(login)), Login: login,
		Kind: kind, Seat: seat, Stage: iam.StageActive, Grants: grants})
}

// A PARKED RUN SAYS, FOR EACH READER, WHOM IT IS PUT TO AND WHETHER THEY MAY
// ANSWER IT, by the answer route's own authority ([runanswer.May], what
// `POST /mcp-approvals/{id}` decides on), never a copy of it:
//
//   - PUT TO ME: an escalated merge, read by the person its rule escalates to.
//   - I MAY ANSWER, PUT TO SOMEBODY ELSE: the merge while it waits on its
//     delegate, read by the asking seat's lead; once escalated or overdue it
//     waits on them too.
//   - I MAY NOT: neither leads the seat nor is named; told who it waits on.
//
// Mutation: decide may_answer by the audience list, or drop the
// escalated/overdue arm of NeedsYou, and a case goes red.
func TestAParkedRunSaysWhomItIsPutToAndWhoMayAnswerIt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	s := standingSources(t, now)
	// HELD BY THIS PROCESS, so its deadline is read on its own clock.
	s.Incarnation = "node-a:reader"
	held := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-0000000000a1", LaunchID: "launch-a", Server: "crewlet",
		Tool: "ask_to_merge", Raw: "ask_to_merge", Fixed: true, Approvers: sandbox.HeldApproversRelease,
		Act: string(delegated.KindPRMerge), HeldAt: now.Add(-5 * time.Minute), HeldClock: s.Incarnation,
		Delegation: &sandbox.HeldDelegation{Rule: 1, Delegates: []string{"release-manager"}}}
	run := sandbox.PendingRun{TurnID: "t-merge", LaunchID: "launch-a", AgentHandle: "pm",
		AgentID: uuid.NewSHA1(uuid.Nil, []byte("pm")).String(), Status: sandbox.StatusAwaiting,
		HeldCall: &held, AudienceHandles: []string{"ana", "founder"}}
	escalated := held
	delegation := *held.Delegation
	delegation.Escalated = &sandbox.HeldEscalation{Overruled: []string{delegated.OverruledDeadline}, At: now}
	escalated.Delegation = &delegation
	up := run
	up.HeldCall = &escalated
	late := run
	lateHeld := held
	lateHeld.HeldAt = now.Add(-delegated.DefaultDeadline - time.Minute)
	late.HeldCall = &lateHeld

	founder := asPerson(t, "founder@example.com", "founder", iam.KindPerson)
	ana := asPerson(t, "ana@example.com", "ana", iam.KindPerson)
	bo := asPerson(t, "bo@example.com", "bo", iam.KindPerson)
	for _, c := range []struct {
		name   string
		ctx    context.Context
		run    sandbox.PendingRun
		record string
		want   runanswer.Waiting
		needs  bool
	}{
		{"put to me: escalated to the person its rule names", ana, up, "ana",
			runanswer.Waiting{PutTo: []string{"ana", "founder"}, PutToYou: true, MayAnswer: true, Escalated: true}, true},
		{"I may answer, put to its delegate: the asking seat's lead", founder, run, "founder",
			runanswer.Waiting{PutTo: []string{"release-manager"}, MayAnswer: true}, false},
		{"I may answer, and its delegates let the deadline pass", founder, late, "founder",
			runanswer.Waiting{PutTo: []string{"release-manager"}, MayAnswer: true, Overdue: true}, true},
		{"I may not answer: neither its lead nor named", bo, up, "bo",
			runanswer.Waiting{PutTo: []string{"ana", "founder"}, Escalated: true}, false},
		{"I may not answer while it waits on its delegate", bo, run, "bo",
			runanswer.Waiting{PutTo: []string{"release-manager"}}, false},
	} {
		got, err := s.waiting(c.ctx, c.run, c.record)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.PutToYou != c.want.PutToYou || got.MayAnswer != c.want.MayAnswer ||
			got.Escalated != c.want.Escalated || got.Overdue != c.want.Overdue ||
			!equalStrings(got.PutTo, c.want.PutTo) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
		if got.NeedsYou() != c.needs {
			t.Errorf("%s: needs you %v, want %v", c.name, got.NeedsYou(), c.needs)
		}
		row := serialiseRun(c.run, sandbox.BridgeLog{}, s.reach(c.ctx, c.run.AgentHandle), runOpened{}, got)
		if row["may_answer"] != c.want.MayAnswer || row["needs_you"] != c.needs || row["put_to_you"] != c.want.PutToYou {
			t.Errorf("%s: the row says may_answer=%v needs_you=%v put_to_you=%v", c.name, row["may_answer"],
				row["needs_you"], row["put_to_you"])
		}
	}
}

// A QUESTION IS ANSWERED BY ITS SEAT'S LEAD AND BY NOBODY ELSE WHO IS NOT ITS
// AUDIENCE, by `answer_executor`'s own object ([runanswer.Question]): the
// lead may answer a question put to somebody else, and a person who does not
// lead the seat may not.
//
// Mutation: answer the question by the held-call verb, or by the audience
// list, and the lead is refused or the stranger admitted.
func TestAQuestionIsTheSeatsLeadsToAnswer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	s := standingSources(t, now)
	run := sandbox.PendingRun{TurnID: "t-ask", LaunchID: "launch-q", AgentHandle: "pm",
		AgentID: uuid.NewSHA1(uuid.Nil, []byte("pm")).String(), Status: sandbox.StatusAwaiting,
		Question: "Ship it?", AudienceHandles: []string{"ana"}}
	for _, c := range []struct {
		who            string
		ctx            context.Context
		put, mayAnswer bool
	}{
		{"its audience", asPerson(t, "ana@example.com", "ana", iam.KindPerson), true, false},
		{"its seat's lead", asPerson(t, "founder@example.com", "founder", iam.KindPerson), false, true},
		{"a stranger", asPerson(t, "bo@example.com", "bo", iam.KindPerson), false, false},
		{"an administrator", asPerson(t, "admin@example.com", "", iam.KindPerson, iam.GrantAdmin), false, true},
	} {
		record := iam.RecordOwner(must(iam.From(c.ctx)))
		got, err := s.waiting(c.ctx, run, record)
		if err != nil {
			t.Fatalf("%s: %v", c.who, err)
		}
		if got.PutToYou != c.put || got.MayAnswer != c.mayAnswer {
			t.Errorf("%s: put to them %v, may answer %v; want %v, %v", c.who, got.PutToYou, got.MayAnswer,
				c.put, c.mayAnswer)
		}
	}
}

// A RUN THAT WAITS ON NOBODY IS NOBODY'S: running, answered or ending, it is
// put to nobody and answered by nobody, whoever reads it.
//
// Mutation: drop the awaiting check from Sources.waiting, and an answered run
// offers its answer again.
func TestARunWaitingOnNobodyIsAnsweredByNobody(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	s := standingSources(t, now)
	admin := asPerson(t, "admin@example.com", "", iam.KindPerson, iam.GrantAdmin)
	for _, status := range []string{sandbox.StatusRunning, sandbox.StatusAnswered} {
		run := sandbox.PendingRun{TurnID: "t-" + status, AgentHandle: "pm",
			AgentID: uuid.NewSHA1(uuid.Nil, []byte("pm")).String(), Status: status, AudienceHandles: []string{"ana"}}
		got, err := s.waiting(admin, run, "admin@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if got.MayAnswer || got.PutToYou || got.NeedsYou() || len(got.PutTo) > 0 {
			t.Errorf("a %s run stands as %+v for an administrator", status, got)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func must[T any](v T, _ iam.Resolution) T { return v }
