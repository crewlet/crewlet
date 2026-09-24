package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fakePurger records what the route asked it to destroy.
type fakePurger struct {
	operator, opID, id, project, reason string
	calls                               int
	outcome                             statelog.Outcome
}

func (f *fakePurger) PurgeAs(_ context.Context, operator, opID, id, project,
	reason string) (tracker.WriteResult, error) {

	f.calls++
	f.operator, f.opID, f.id, f.project, f.reason = operator, opID, id, project, reason
	outcome := f.outcome
	if outcome == "" {
		outcome = statelog.OutcomeApplied
	}
	return tracker.WriteResult{
		Outcome:  outcome,
		OpID:     opID,
		Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 41},
	}, nil
}

// purgeApp is an app with the purge route mounted and reads guarded.
func purgeApp(t *testing.T, p *fakePurger) *api.App {
	t.Helper()
	b := closedPosture()
	return newApp(t, api.Options{Bootstrap: &b, Purger: p})
}

// post runs one authenticated request against the purge route.
func postPurge(t *testing.T, a *api.App, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.NewDecoder(rec.Result().Body).Decode(&body)
	return rec.Code, body
}

// THE ROUTE IS THE ONLY WAY A COMPANY CAN DESTROY A TASK.
//
// `tracker.Writer.PurgeTask` had no caller anywhere: no verb, no route, no
// tool. So an erasure request had no mechanism, and a credential pasted into a
// task body stayed in the durable rows of every node for ever — `remove` only
// hides a task and `delete` only stops later records about it.
func TestThePurgeRouteDestroysTheTaskAsTheOperatorWhoAsked(t *testing.T) {
	p := &fakePurger{}
	code, body := postPurge(t, purgeApp(t, p),
		"/work/t-1/purge?confirm=ENG-42&project=ENG&reason=an+erasure+request")
	if code != http.StatusOK {
		t.Fatalf("the route answered %d: %v", code, body)
	}
	if p.id != "t-1" || p.project != "ENG" || p.reason != "an erasure request" {
		t.Errorf("the writer was asked to purge %q in %q for %q",
			p.id, p.project, p.reason)
	}
	// THE RECORD CARRIES THE PERSON, not this process. Every other write
	// on this surface is the fleet's; a purge destroys a company's data
	// and the marker's author is who a later reader asks about.
	if p.operator != "founder" {
		t.Errorf("the purge was attributed to %q rather than the token's own "+
			"operator id", p.operator)
	}
	if body["outcome"] != string(statelog.OutcomeApplied) {
		t.Errorf("the three-valued outcome reached the caller as %v", body["outcome"])
	}
}

// EVERY REFUSAL HAPPENS BEFORE THE WRITE, and each names a different missing
// thing: a confirmation that has to be looked up, the container the record
// arbitrates under, and the only account of the task that survives it.
func TestAPurgeMissingItsConfirmationProjectOrReasonNeverWrites(t *testing.T) {
	for name, path := range map[string]string{
		"no confirmation": "/work/t-1/purge?project=ENG&reason=why",
		"no project":      "/work/t-1/purge?confirm=ENG-42&reason=why",
		"no reason":       "/work/t-1/purge?confirm=ENG-42&project=ENG",
	} {
		p := &fakePurger{}
		code, body := postPurge(t, purgeApp(t, p), path)
		if code != http.StatusBadRequest {
			t.Errorf("%s answered %d rather than refusing: %v", name, code, body)
		}
		if p.calls != 0 {
			t.Errorf("%s still reached the writer", name)
		}
	}

	// THE CONTROL: the same route with all three runs, or the assertions
	// above would pass on a route that refuses everything.
	p := &fakePurger{}
	if code, body := postPurge(t, purgeApp(t, p),
		"/work/t-1/purge?confirm=ENG-42&project=ENG&reason=why"); code != http.StatusOK {
		t.Fatalf("a complete request answered %d: %v", code, body)
	}
	if p.calls != 1 {
		t.Errorf("a complete request reached the writer %d time(s)", p.calls)
	}
}

// AN OPERATION ID THE CALLER BRINGS IS WHAT MAKES A RETRY SAFE. A purge that
// answers `unknown` has no acknowledgement and no position, so retrying is
// correct — and a retry with a FRESH id would append a second purge of a task
// the first one may already have destroyed.
func TestAPurgeRetryReusesTheOperationIdItWasGiven(t *testing.T) {
	p := &fakePurger{}
	a := purgeApp(t, p)
	given := statelog.NewOpID(time.Now().Add(-time.Minute), "purge-t-1")
	if code, _ := postPurge(t, a,
		"/work/t-1/purge?confirm=ENG-42&project=ENG&reason=why&op_id="+
			url.QueryEscape(given)); code != http.StatusOK {
		t.Fatalf("the route answered %d", code)
	}
	if p.opID != given {
		t.Errorf("the retry was written under %q rather than the id it was "+
			"given, %q", p.opID, given)
	}
	// AND WITHOUT ONE IT MINTS ITS OWN, or two operators purging two
	// tasks would share an operation id.
	if code, _ := postPurge(t, a,
		"/work/t-2/purge?confirm=ENG-43&project=ENG&reason=why"); code != http.StatusOK {
		t.Fatalf("the route answered %d", code)
	}
	if p.opID == given || p.opID == "" {
		t.Errorf("a call with no operation id was written under %q", p.opID)
	}
	if _, minted := statelog.OpMintedAt(p.opID); !minted {
		t.Errorf("the route minted %q, which carries no mint instant", p.opID)
	}
}

// AN OPERATION ID THE ENGINE DID NOT MINT IS REFUSED, not passed on — by the
// purge and by the node gate, the two routes that take one.
//
// It carries no instant, so the state log reads it as older than every loss
// its ledger has had — and on any deployment old enough to have swept its
// ledger once, that answers the gesture `unknown` without publishing it, on
// the first attempt as on every retry: a gesture that can never run and never
// says why.
func TestAGestureRefusesAnOperationIdTheEngineDidNotMint(t *testing.T) {
	p := &fakePurger{}
	code, body := postPurge(t, purgeApp(t, p),
		"/work/t-1/purge?confirm=ENG-42&project=ENG&reason=why&op_id=op-abc")
	if code != http.StatusBadRequest || body["error"] != "op_id_invalid" {
		t.Fatalf("an invented purge op_id answered %d %v, want 400 op_id_invalid",
			code, body)
	}
	if p.calls != 0 {
		t.Errorf("the invented op_id still reached the purge")
	}

	b := closedPosture()
	gate := &fakeNodeGate{}
	code, body = postPurge(t, newApp(t, api.Options{Bootstrap: &b, Nodes: gate}),
		"/work/retention/evict/node-4?confirm=node-4&op_id=op-abc")
	if code != http.StatusBadRequest || body["error"] != "op_id_invalid" {
		t.Fatalf("an invented gate op_id answered %d %v, want 400 op_id_invalid",
			code, body)
	}
	if len(gate.asked) != 0 {
		t.Errorf("the invented op_id still reached the gate: %+v", gate.asked)
	}
}

// AN OPERATION ID THE BROKER WOULD CARRY AS ANOTHER IS REFUSED, ON BOTH ROUTES
// THAT TAKE ONE, BEFORE ANYTHING IS WRITTEN — minted by the engine or not.
//
// Only an id's first thirty-six bytes are the minted uuid; the rest is free
// text, and the whole id travels as the broker's message-id header, whose
// writer trims its ends and turns a line break into a space. So an engine id
// carrying either was deduplicated at the broker as another id than the one
// every ledger answers for, and the route's own trim answered with an id the
// caller never sent. The one an answer carried goes through byte for byte.
func TestAnOperationIDTheBrokerWouldRewriteIsRefused(t *testing.T) {
	minted := statelog.NewOpID(time.Now().Add(-time.Minute), "purge-t-1")
	for name, opID := range map[string]string{
		"a leading space":       " " + minted,
		"a trailing space":      minted + " ",
		"an inner space":        minted + ".a b",
		"a line break":          minted + "\n",
		"a tab":                 minted + ".\ttab",
		"a byte past ASCII":     minted + ".\u00e9",
		"one byte over the cap": minted + "." + strings.Repeat("a", 128-len(minted)),
	} {
		query := url.Values{"op_id": {opID}}.Encode()
		p := &fakePurger{}
		code, body := postPurge(t, purgeApp(t, p),
			"/work/t-1/purge?confirm=ENG-42&project=ENG&reason=why&"+query)
		if code != http.StatusBadRequest || body["error"] != "op_id_invalid" {
			t.Errorf("a purge under an op_id with %s answered %d %v, want 400 "+
				"op_id_invalid", name, code, body)
		}
		if p.calls != 0 {
			t.Errorf("a purge under an op_id with %s reached the writer", name)
		}

		b := closedPosture()
		gate := &fakeNodeGate{}
		code, body = postPurge(t, newApp(t, api.Options{Bootstrap: &b, Nodes: gate}),
			"/work/retention/evict/node-4?confirm=node-4&"+query)
		if code != http.StatusBadRequest || body["error"] != "op_id_invalid" {
			t.Errorf("an eviction under an op_id with %s answered %d %v, want "+
				"400 op_id_invalid", name, code, body)
		}
		if len(gate.asked) != 0 {
			t.Errorf("an eviction under an op_id with %s reached the gate", name)
		}
	}

	// THE CONTROL: a minted id at the cap, carrying punctuation from across
	// the visible range, is the caller's and goes through verbatim.
	good := minted + "." + strings.Repeat("a", 128-len(minted)-1-8) + "-_.:~!@9"
	if len(good) != 128 {
		t.Fatalf("the control is %d bytes, want exactly the cap", len(good))
	}
	p := &fakePurger{}
	code, body := postPurge(t, purgeApp(t, p),
		"/work/t-1/purge?confirm=ENG-42&project=ENG&reason=why&"+
			url.Values{"op_id": {good}}.Encode())
	if code != http.StatusOK || p.opID != good {
		t.Fatalf("a purge under a valid op_id answered %d %v and ran under %q",
			code, body, p.opID)
	}
}

// AN UNKNOWN PURGE CARRIES NO POSITION, and every other outcome does.
//
// `unknown` is the one outcome with no acknowledgement: the record may never
// have reached the log. The route sent the writer's zero position anyway,
// which reads as a record at the log's origin, and the command printed it as
// "unknown at  0".
func TestAnUnknownPurgeCarriesNoPosition(t *testing.T) {
	for outcome, positioned := range map[statelog.Outcome]bool{
		statelog.OutcomeApplied: true,
		statelog.OutcomePending: true,
		statelog.OutcomeUnknown: false,
	} {
		p := &fakePurger{outcome: outcome}
		code, body := postPurge(t, purgeApp(t, p),
			"/work/t-1/purge?confirm=ENG-42&project=ENG&reason=why")
		if code != http.StatusOK {
			t.Fatalf("an %s purge answered %d %v", outcome, code, body)
		}
		if _, has := body["position"]; has != positioned {
			t.Errorf("an %s purge answered position %v, want present = %v",
				outcome, body["position"], positioned)
		}
		if body["op_id"] == "" || body["op_id"] == nil {
			t.Errorf("an %s purge answered no op_id — it is what a retry names", outcome)
		}
	}
}

// THE ROUTE IS ABSENT ON A BUILD THAT CANNOT SERVE IT, rather than answering
// 503: an operator who cannot purge must not be told they can and then find
// out at the moment they need it.
func TestAProcessWithNoTrackerServesNoPurgeRoute(t *testing.T) {
	b := closedPosture()
	code, _ := postPurge(t, newApp(t, api.Options{Bootstrap: &b}),
		"/work/t-1/purge?confirm=ENG-42&project=ENG&reason=why")
	if code != http.StatusNotFound {
		t.Errorf("a build with no tracker answered %d for the purge route", code)
	}
}
