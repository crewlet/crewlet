package queries_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// delegatedAt is the fixtures' instant: the read is handed it as its clock, so
// the day it cuts and the week it reaches back are this one's.
var delegatedAt = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

// delegatedLog is an access log answering each of the read's three questions
// with its own verdicts: the day's, the sampled week's and the agents' denials.
// The verdict named unopened does not open under this node's keyring.
type delegatedLog struct {
	day, sampled, denied []accessdomain.VerdictRow
	unopened             string
}

func (l delegatedLog) Verdicts(_ context.Context, q accessdomain.VerdictQuery,
	_ statelog.Freshness) (accessdomain.VerdictPage, error) {

	rows := l.day
	switch {
	case q.Sampled != nil:
		rows = l.sampled
	case q.Verdict == delegated.Deny:
		rows = l.denied
	}
	return accessdomain.VerdictPage{Rows: rows, Complete: true}, nil
}

func (l delegatedLog) OpenVerdict(v accessdomain.VerdictRow) (string, string, []string, error) {
	if v.ID == l.unopened {
		return "", "", nil, errors.New("sealed under a key this node does not hold")
	}
	return "systemctl status nginx", "a read of a service's state", []string{"reads only"}, nil
}

// THE DELEGATED DECISIONS SCREEN READS WHAT THE ANSWER SENDS. Settings and
// Home read `GET /delegated-decisions` as `DelegatedDecisionsAnswer` and the
// shapes composed into it (`contract/delegated.ts`): every one is held both
// ways over an answer whose fixture fills every half, a verdict that opened and
// one that did not, an agent's, a person's and the engine's. The words a row
// is keyed by are held equal to the engine's own lists.
//
// Mutation: rename `spot_checks_more` on either side, or drop `other` from
// DelegatedAct, and this is red.
func TestTheDelegatedDecisionsScreenReadsWhatTheAnswerSends(t *testing.T) {
	t.Parallel()
	doc := "name: Acme\nroles:\n  - {name: Ops Lead, handle: ops-lead, kind: human}\n" +
		"  - {name: Security A, handle: security-a}\n  - {name: Security B, handle: security-b}\n" +
		"delegated_decisions:\n  enabled: true\n  rules:\n" +
		"    - {kinds: [ssh_operate], delegates: [security-a, security-b], escalate_to: [ops-lead], " +
		"sample_one_in: 5}\n"
	company, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	day := delegatedAt.Add(-12 * time.Hour)
	allowed := accessdomain.VerdictRow{Handle: "sre", ID: "v-1", Request: "r-1",
		Kind: delegated.KindSSHOperate, Rule: 1, By: "security-a", ByKind: accessdomain.VerdictByAgent,
		Through: accessdomain.VerdictThroughDelegation, Asked: delegated.Allow, Confidence: delegated.High,
		Verdict: delegated.Allow, Final: true, Sampled: true, Host: "h-1", At: day}
	// THE ENGINE'S OWN: nobody answered in time, so it put the request to
	// people, and its words did not open here.
	overruled := accessdomain.VerdictRow{Handle: "sre", ID: "v-2", Request: "r-2",
		Kind: delegated.KindSSHOperate, Rule: 1, ByKind: accessdomain.VerdictByEngine,
		Through: accessdomain.VerdictThroughDelegation, Verdict: delegated.Escalate, Final: true,
		Overruled: []string{delegated.OverruledDeadline}, Host: "h-1", At: day.Add(time.Minute)}
	approved := accessdomain.VerdictRow{Handle: "sre", ID: "v-3", Request: "r-3",
		Kind: delegated.KindSSHOperate, By: "ops-lead", ByKind: accessdomain.VerdictByPerson,
		Through: accessdomain.VerdictThroughDecision, Verdict: delegated.Allow, Final: true,
		Host: "h-1", Decision: "d-1", Item: "ENG-4", At: day.Add(2 * time.Minute)}
	denied := accessdomain.VerdictRow{Handle: "sre", ID: "v-4", Request: "r-4",
		Kind: delegated.KindSSHOperate, Rule: 1, By: "security-b", ByKind: accessdomain.VerdictByAgent,
		Through: accessdomain.VerdictThroughDelegation, Asked: delegated.Deny, Confidence: delegated.High,
		Verdict: delegated.Deny, Final: true, Host: "h-2", At: day.Add(3 * time.Minute)}

	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{
		Company: func() (*config.Company, *org.Organization) { return company, nil },
		Now:     func() time.Time { return delegatedAt },
		Delegated: delegatedLog{day: []accessdomain.VerdictRow{allowed, overruled, approved},
			sampled: []accessdomain.VerdictRow{allowed}, denied: []accessdomain.VerdictRow{denied},
			unopened: overruled.ID},
	})
	// THE PERSON A RULE ESCALATES TO, which is who the screen is for.
	raw, err := r.Answer(asSeat(t, "ops-lead"), "delegated_decisions", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := asMap(t, raw)
	holdShape(t, "DelegatedDecisionsAnswer", []map[string]any{body}, false)
	holdShape(t, "DelegatedRule", rowsOf(t, body["rules"]), false)
	limits, ok := body["limits"].(map[string]any)
	if !ok {
		t.Fatalf("the answer's limits are %v", body["limits"])
	}
	holdShape(t, "DelegatedLimits", []map[string]any{limits}, false)
	var verdicts []map[string]any
	for _, list := range []string{"decided", "spot_checks", "flagged"} {
		rows := rowsOf(t, body[list])
		if len(rows) == 0 {
			t.Fatalf("the answer's %s carries no verdict, so the fixture proves nothing about it", list)
		}
		verdicts = append(verdicts, rows...)
	}
	holdShape(t, "DelegatedVerdictRow", verdicts, false)

	for name, engine := range map[string][]string{
		"DelegatedAct":        stringsOf(delegated.Kinds()),
		"DelegatedVerdict":    stringsOf(delegated.Verdicts()),
		"DelegatedConfidence": stringsOf(delegated.Confidences()),
		"DelegatedGiver":      stringsOf(accessdomain.VerdictGivers),
		"DelegatedPath":       stringsOf(accessdomain.VerdictPaths),
	} {
		got, err := clientsource.Union(clientsource.Tree(t), name)
		if err != nil {
			t.Fatalf("%v: this gate cannot run without the client's declaration", err)
		}
		slices.Sort(got)
		if want := slices.Sorted(slices.Values(engine)); !slices.Equal(got, want) {
			t.Errorf("the dashboard's %s is %v; the engine sends %v", name, got, want)
		}
	}
}

// A HELD CALL'S CARD READS WHAT ITS DELEGATION SENDS. A parked run's
// `held_call.delegation` on the executors board is read as `HeldDelegation`
// and the answer and escalation shapes composed into it
// (`contract/delegated.ts`), which the Inbox, Home and the board draw before
// the call itself. Held both ways over two calls, one still with its panel and
// one the engine put to people, so every key either sends is exercised.
//
// Mutation: rename `escalated` on either side, or stop sending a delegate's
// `findings`, and this is red.
func TestAHeldCallsCardReadsWhatItsDelegationSends(t *testing.T) {
	t.Parallel()
	held := func(id, turn string, d sandbox.HeldDelegation) sandbox.PendingRun {
		call := sandbox.HeldCall{ID: id, LaunchID: "launch-" + turn, Server: "crewlet", Tool: "ssh_operate",
			Raw: "ssh_operate", Fixed: true, Approvers: sandbox.HeldApproversSSH,
			Act: string(delegated.KindSSHOperate), HeldAt: delegatedAt, Delegation: &d}
		return sandbox.PendingRun{TurnID: turn, LaunchID: "launch-" + turn, AgentHandle: "sre",
			Status: sandbox.StatusAwaiting, HeldCall: &call, AudienceHandles: []string{"ops-lead"}}
	}
	panel := []string{"security-a", "security-b"}
	allow := sandbox.HeldVerdict{By: "security-a", Verdict: string(delegated.Allow),
		Confidence: string(delegated.High), Reason: "a read of a service's state", At: delegatedAt}
	escalate := sandbox.HeldVerdict{By: "security-b", Verdict: string(delegated.Escalate),
		Confidence: string(delegated.Low), Reason: "restarts a service in use",
		Findings: []string{"restarts nginx"}, At: delegatedAt.Add(time.Minute)}
	runs := fixedRuns{
		held("0190f3a8-0000-7000-8000-0000000000d1", "t-waiting", sandbox.HeldDelegation{Rule: 1,
			Delegates: panel, Digest: "sha256:d1", Given: []sandbox.HeldVerdict{allow}}),
		held("0190f3a8-0000-7000-8000-0000000000d2", "t-escalated", sandbox.HeldDelegation{Rule: 1,
			Delegates: panel, Digest: "sha256:d2", Given: []sandbox.HeldVerdict{allow, escalate},
			Escalated: &sandbox.HeldEscalation{Overruled: []string{delegated.OverruledDisagreement},
				Engine: []string{"The panel disagreed."}, At: delegatedAt.Add(2 * time.Minute)}}),
	}
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{Sandbox: runs})
	raw, err := r.Answer(everyGrant(t), "executors", nil)
	if err != nil {
		t.Fatal(err)
	}
	var delegations, given, escalated []map[string]any
	for _, row := range rowsOf(t, asMap(t, raw)["runs"]) {
		call, ok := row["held_call"].(map[string]any)
		if !ok {
			t.Fatalf("a run parked on a held call carries held_call %v", row["held_call"])
		}
		d, ok := call["delegation"].(map[string]any)
		if !ok {
			t.Fatalf("a delegated call carries delegation %v", call["delegation"])
		}
		delegations = append(delegations, d)
		given = append(given, rowsOf(t, d["given"])...)
		if e, ok := d["escalated"].(map[string]any); ok {
			escalated = append(escalated, e)
		}
	}
	holdShape(t, "HeldDelegation", delegations, false)
	holdShape(t, "HeldDelegateAnswer", given, false)
	holdShape(t, "HeldEscalation", escalated, false)
}
