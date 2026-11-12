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
	// A RULE OF EACH SHAPE: a panel on commands, and one delegate on merges
	// and deploys with paths of its own that always go to people and checks
	// it requires.
	doc := "name: Acme\nroles:\n  - {name: Ops Lead, handle: ops-lead, kind: human}\n" +
		"  - {name: Security A, handle: security-a}\n  - {name: Security B, handle: security-b}\n" +
		"  - {name: Release Reviewer, handle: release-reviewer}\n" +
		"delegated_decisions:\n  enabled: true\n  rules:\n" +
		"    - {kinds: [ssh_operate], delegates: [security-a, security-b], escalate_to: [ops-lead], " +
		"sample_one_in: 5}\n" +
		"    - {kinds: [pr_merge, deploy], delegates: [release-reviewer], escalate_to: [ops-lead], " +
		"always_escalate_paths: [billing/**], required_checks: [{repo: acme/api, branch: main, " +
		"checks: [test, lint]}]}\n"
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
	rules := rowsOf(t, body["rules"])
	holdShape(t, "DelegatedRule", rules, false)
	if len(rules) != 2 || len(rowsOf(t, rules[1]["required_checks"])) != 1 {
		t.Fatalf("the merge rule's required checks read %v", rules)
	}
	holdShape(t, "DelegatedRequiredChecks", rowsOf(t, rules[1]["required_checks"]), false)
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

// windowLog is the access log's verdicts as the store answers a window: each
// row whose instant is in [Since, Until), narrowed by Sampled and Verdict.
type windowLog struct{ rows []accessdomain.VerdictRow }

func (l windowLog) Verdicts(_ context.Context, q accessdomain.VerdictQuery,
	_ statelog.Freshness) (accessdomain.VerdictPage, error) {

	var out []accessdomain.VerdictRow
	for _, row := range l.rows {
		switch {
		//fixedwait:stated a row's instant the case states off delegatedAt, against the window the query derives from its frozen Now
		case row.At.Before(q.Since), !q.Until.IsZero() && !row.At.Before(q.Until),
			q.Sampled != nil && row.Sampled != *q.Sampled, q.Verdict != "" && row.Verdict != q.Verdict:
			continue
		}
		out = append(out, row)
	}
	return accessdomain.VerdictPage{Rows: out, Complete: true}, nil
}

func (windowLog) OpenVerdict(accessdomain.VerdictRow) (string, string, []string, error) {
	return "systemctl status nginx", "a read of a service's state", nil, nil
}

// THE RUNNING DAY IS ANSWERED SO FAR. `day=today`, or today's label, answers
// the company day still running with every verdict given on it until now and
// `complete: false`; the default stays the last whole day, which Home's digest
// reads; a day that has not begun is refused.
//
// Mutation: answer `today` as the last whole day, or end its window at the
// last whole day, and a verdict given this morning is missing until tomorrow.
func TestTheRunningDayAnswersWhatWasDecidedSoFar(t *testing.T) {
	t.Parallel()
	doc := "name: Acme\nroles:\n  - {name: Ops Lead, handle: ops-lead, kind: human}\n" +
		"  - {name: Security A, handle: security-a}\n" +
		"delegated_decisions:\n  enabled: true\n  rules:\n" +
		"    - {kinds: [ssh_operate], delegates: [security-a], escalate_to: [ops-lead]}\n"
	company, err := config.ParseCompanyDocument([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	row := func(id string, at time.Time) accessdomain.VerdictRow {
		return accessdomain.VerdictRow{Handle: "sre", ID: id, Request: "r-" + id, Kind: delegated.KindSSHOperate,
			Rule: 1, By: "security-a", ByKind: accessdomain.VerdictByAgent,
			Through: accessdomain.VerdictThroughDelegation, Asked: delegated.Allow, Confidence: delegated.High,
			Verdict: delegated.Allow, Final: true, Host: "h-1", At: at}
	}
	thisMorning := row("today", delegatedAt.Add(-30*time.Minute))
	yesterday := row("yesterday", delegatedAt.Add(-20*time.Hour))
	r := queries.NewRegistry()
	queries.Register(r, queries.Sources{
		Company:   func() (*config.Company, *org.Organization) { return company, nil },
		Now:       func() time.Time { return delegatedAt },
		Delegated: windowLog{rows: []accessdomain.VerdictRow{yesterday, thisMorning}},
	})
	answer := func(params map[string]any) map[string]any {
		t.Helper()
		raw, err := r.Answer(asSeat(t, "ops-lead"), "delegated_decisions", params)
		if err != nil {
			t.Fatalf("day %v: %v", params, err)
		}
		return asMap(t, raw)
	}
	requests := func(body map[string]any) []string {
		var out []string
		for _, row := range rowsOf(t, body["decided"]) {
			out = append(out, row["request"].(string))
		}
		return out
	}
	for _, c := range []struct {
		name     string
		params   map[string]any
		day      string
		complete bool
		want     []string
	}{
		{"today", map[string]any{"day": "today"}, "2026-11-10", false, []string{"r-today"}},
		{"today's label", map[string]any{"day": "2026-11-10"}, "2026-11-10", false, []string{"r-today"}},
		{"the default, the last whole day", nil, "2026-11-09", true, []string{"r-yesterday"}},
		{"an earlier day", map[string]any{"day": "2026-11-08"}, "2026-11-08", true, nil},
	} {
		body := answer(c.params)
		if body["day"] != c.day || body["complete"] != c.complete || !slices.Equal(requests(body), c.want) {
			t.Errorf("%s: day %v, complete %v, decided %v; want %s, %v, %v", c.name, body["day"],
				body["complete"], requests(body), c.day, c.complete, c.want)
		}
	}
	if _, err := r.Answer(asSeat(t, "ops-lead"), "delegated_decisions",
		map[string]any{"day": "2026-11-11"}); !errors.Is(err, queries.ErrBadParams) {
		t.Errorf("a day that has not begun answered %v, want a bad parameter", err)
	}
}
