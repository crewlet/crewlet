package engine

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A VERDICT ACTS ONLY ONCE ITS RECORD LANDED AS ITS GIVER'S FIRST: an applied
// or pending record is first; a giver's earlier verdict on the request answers
// not first and no error, so the second does not act and nothing is retried;
// and a refusal or a landing nobody can confirm is an error, which does not act
// either.
//
// Mutation: answer an unknown landing as first and a verdict whose record may
// never have landed acts.
func TestAVerdictActsOnlyOnceItsRecordLandedAsItsGiversFirst(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		res     statelog.Result
		err     error
		first   bool
		failure bool
	}{
		"applied":            {res: statelog.Result{Outcome: statelog.OutcomeApplied}, first: true},
		"pending":            {res: statelog.Result{Outcome: statelog.OutcomePending}, first: true},
		"recorded already":   {err: accessdomain.ErrRecorded},
		"landing unknown":    {res: statelog.Result{Outcome: statelog.OutcomeUnknown}, failure: true},
		"refused as invalid": {err: accessdomain.ErrInvalid, failure: true},
	} {
		first, err := verdictLanded(tc.res, tc.err, "call-1")
		if first != tc.first || (err != nil) != tc.failure {
			t.Errorf("%s answers first %t and %v, want first %t and failure %t",
				name, first, err, tc.first, tc.failure)
		}
	}
}

// THE ENGINE RECORDS A GIVER'S VERDICT ONCE AND THE AUDIT OPENS IT: the SSH
// auditor's write is its giver's first on the request, a retry of the same
// decision lands on that record, a second decision by the same giver is not
// first and records nothing, a panel's other delegate records its own, and
// the setup surface's read pages the verdicts and opens what each decided. A
// node with no access log records nothing and says so.
//
// Mutation: derive the operation from the call's own clock rather than the
// decision's start, and the retry is answered as a second verdict.
func TestTheEngineRecordsAGiversVerdictOnceAndTheAuditOpensIt(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	audit, minted := sshAudit{engine: e}, time.Now().UTC()
	request := uuid.NewString()
	v := accessdomain.NewVerdict{
		Request: request, Kind: delegated.KindSSHOperate, Rule: 1, Handle: "ceo",
		Turn: uuid.NewString(), By: "reviewer", ByKind: accessdomain.VerdictByAgent,
		Through: accessdomain.VerdictThroughDelegation, Asked: delegated.Allow,
		Confidence: delegated.High, Verdict: delegated.Allow,
		Digest: strings.Repeat("ab", 32), Host: "api-1", User: "crewlet-ops",
		Subject: "systemctl restart api", Reason: "the item names this service",
		Findings: []string{"no sudo beyond the unit"},
	}
	for _, attempt := range []string{"the decision", "its retry"} {
		if first, err := audit.RecordVerdict(t.Context(), minted, v); err != nil || !first {
			t.Fatalf("%s answers first %t, %v; want the giver's first", attempt, first, err)
		}
	}
	again := v
	again.Asked, again.Verdict = delegated.Deny, delegated.Deny
	if first, err := audit.RecordVerdict(t.Context(), minted.Add(time.Minute), again); err != nil || first {
		t.Errorf("the giver's second decision answers first %t, %v; want not first and no error", first, err)
	}
	panel := v
	panel.By, panel.Final = "second-reviewer", true
	if first, err := audit.RecordVerdict(t.Context(), minted, panel); err != nil || !first {
		t.Errorf("the panel's other delegate answers first %t, %v", first, err)
	}
	broken := v
	broken.By, broken.Overruled = "third-reviewer", []string{"Not A Closed Word"}
	if first, err := audit.RecordVerdict(t.Context(), minted, broken); !errors.Is(err, accessdomain.ErrInvalid) || first {
		t.Errorf("a verdict the log refuses answers first %t, %v", first, err)
	}
	service := e.SSHAccess()
	page, err := service.Verdicts(t.Context(), accessdomain.VerdictQuery{Request: request},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || len(page.Rows) != 2 {
		t.Fatalf("the audit pages %+v, %v; want the two givers' verdicts", page.Rows, err)
	}
	var givers []string
	for _, row := range page.Rows {
		subject, reason, findings, err := service.OpenVerdict(row)
		if err != nil || subject != v.Subject || reason != v.Reason || !slices.Equal(findings, v.Findings) {
			t.Errorf("%s's verdict opens to %q, %q, %q, %v", row.By, subject, reason, findings, err)
		}
		givers = append(givers, row.By+":"+string(row.Verdict))
	}
	slices.Sort(givers)
	if want := []string{"reviewer:allow", "second-reviewer:allow"}; !slices.Equal(givers, want) {
		t.Errorf("the request's verdicts are %v, want %v", givers, want)
	}
	if first, err := (sshAudit{engine: &Engine{}}).RecordVerdict(t.Context(), minted, v); !errors.Is(err, errNoAccessLog) || first {
		t.Errorf("a node with no access log answers first %t, %v", first, err)
	}
	if _, err := (SSHAccessService{engine: &Engine{}}).Verdicts(t.Context(), accessdomain.VerdictQuery{},
		statelog.Freshness{Level: statelog.ReadStale}); !errors.Is(err, errNoAccessLog) {
		t.Errorf("a node with no access log pages verdicts: %v", err)
	}
}
