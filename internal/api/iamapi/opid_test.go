package iamapi_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// everything is an administrator holding every grant, a minute past proving
// who they are — so a case about operation ids reaches every route's writes.
func everything() iam.Principal {
	p := administrator()
	p.Grants = iam.AllGrants
	return p
}

// EVERY DIRECTORY WRITE CARRIES AN INSTANT ITS LEDGER CAN VOUCH FOR, AND EVERY
// STEP ITS GESTURE'S.
//
// The publisher vouches for a retry by the instant an operation id carries.
// Every id here was `people:update:<id>:<uuid4>`, `<op>:seat` and the like,
// which carry none and read as minted at the epoch: once this node's ledger had
// swept anything each such write was answered `unknown` without being
// published — and a step appended to such an id inherits the same nothing. So
// each is held to the grammar, and each STEP to its gesture's instant, which a
// step appended with a colon rather than [statelog.StepOpID] does not carry.
//
// Mutation: mint any gesture's id outside the grammar, or join a step with a
// colon, and its row goes red.
func TestEveryDirectoryWriteCarriesAnInstantItsLedgerCanVouchFor(t *testing.T) {
	t.Parallel()
	floor := at.Add(-time.Minute)
	for _, c := range []struct {
		name, method, target string
		body                 any
	}{
		{"a create with a seat", http.MethodPost, "/iam/people",
			map[string]any{"login": "dana.sre", "email": "dana@example.com",
				"seat": "sre"}},
		{"an edit moving a seat, a login and a stage", http.MethodPatch,
			"/iam/people/" + bob.String(), map[string]any{"seat": "sre",
				"login": "bob.ops", "stage": "suspended",
				"grants": []string{"state:read"}}},
		{"a removal", http.MethodDelete, "/iam/people/" + bob.String(), nil},
		{"ending somebody's sessions", http.MethodDelete,
			"/iam/people/" + bob.String() + "/sessions", nil},
		{"a second factor's reset", http.MethodPost,
			"/iam/people/" + bob.String() + "/mfa/reset", nil},
		{"ending every session", http.MethodPost, "/iam/invalidate-all", nil},
		{"an invitation", http.MethodPost, "/iam/invitations",
			map[string]any{"email": "dana@example.com"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			got := r.as(everything(), c.method, c.target, c.body)
			if got.status/100 != 2 {
				t.Fatalf("answered %d: %v", got.status, got.body)
			}
			gesture, _ := got.body["op_id"].(string)
			// AN INVITATION'S ISSUE IS PUBLISHED UNDER ITS GESTURE'S SEED,
			// the uuid its scoped key begins with: the domain derives the
			// invitation's id from it ([iamapi.Service] createKey).
			seed, _, _ := strings.Cut(gesture, ".")
			gestureAt, ok := statelog.OpMintedAt(gesture)
			if !ok || gestureAt.Before(floor) {
				t.Fatalf("the gesture answered op_id %q, which carries no "+
					"instant the ledger can vouch for", gesture)
			}
			if len(r.writer.ops) == 0 {
				t.Fatal("no write was asked for; this case tests nothing")
			}
			for call, ops := range r.writer.ops {
				for _, op := range ops {
					if err := statelog.CheckCallerOpID(op); err != nil {
						t.Errorf("%s published under %q, outside the grammar: %v",
							call, op, err)
						continue
					}
					if !strings.HasPrefix(op, gesture) &&
						(call != "invite" || op != seed) {
						t.Errorf("%s published under %q, which is no step of "+
							"the gesture %q", call, op, gesture)
					}
					if at, _ := statelog.OpMintedAt(op); !at.Equal(gestureAt) {
						t.Errorf("%s published under %q, dated %s — not its "+
							"gesture's %s", call, op, at, gestureAt)
					}
				}
			}
		})
	}
}

// A KEY OUTSIDE THE GRAMMAR IS REFUSED BEFORE ANYTHING IS WRITTEN.
//
// A caller's key is the operation a write is published under, and one that
// carries no instant is one the ledger can never vouch for: every retry of it
// — the only thing a key is for — would be answered `unknown` without being
// published, for ever. So it is held to [statelog.CheckCallerOpID], the rule
// every surface holds a caller's id to, and refused naming the header.
//
// Mutation: take the header as sent and every row publishes.
func TestAKeyOutsideTheGrammarIsRefusedBeforeAnyWrite(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, method, target, key string
		body                      any
	}{
		{"an edit", http.MethodPatch, "/iam/people/" + bob.String(), "retry-7",
			map[string]any{"grants": []string{"state:read"}}},
		{"a removal", http.MethodDelete, "/iam/people/" + bob.String(),
			"0192f00d-0000-4000-8000-000000000001", nil},
		{"ending somebody's sessions", http.MethodDelete,
			"/iam/people/" + bob.String() + "/sessions", "retry-7", nil},
		{"a create", http.MethodPost, "/iam/people", "retry-1",
			map[string]any{"login": "dana.sre", "email": "dana@example.com"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			got := r.asWith(everything(), c.method, c.target, c.body,
				http.Header{opkey.Header: {c.key}})
			if got.status != http.StatusBadRequest ||
				got.body["error"] != string(httpjson.CodeOpIDInvalid) ||
				got.body["message"] != httpjson.CodeOpIDInvalid.Message() ||
				got.body["field"] != opkey.Header {
				t.Fatalf("answered %d %v, want 400 op_id_invalid naming %s",
					got.status, got.body, opkey.Header)
			}
			if len(r.writer.calls) != 0 {
				t.Errorf("a refused key still published %v", r.writer.calls)
			}
		})
	}
}

// AN UNKNOWN THIS NODE'S LEDGER CANNOT VOUCH FOR SENDS THE ADMINISTRATOR
// ELSEWHERE — a whole gesture's included.
//
// Asked here again it answers the same way until the change reaches this
// node, so it carries no Retry-After and says `unvouched`. A gesture of
// several records answers its WEAKEST step, and an unvouched step makes the
// gesture one this node cannot vouch for: retried here, it meets that step's
// silence again. The control is a lost acknowledgement, which says when.
//
// Every one says `outcome: "unknown"` beside the operation, which is what
// tells it from a refusal that wrote nothing without reading the sentence.
//
// Mutation: drop the unvouched arm, or drop it from the fold of a sequence,
// and a row carries a Retry-After; write the unknown through UnavailableWith
// and every row lacks its outcome.
func TestAnUnvouchedUnknownSendsTheAdministratorElsewhere(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, call string
		body       map[string]any
		unvouched  bool
	}{
		{"a create", "enrol",
			map[string]any{"login": "dana.sre", "email": "dana@example.com"}, true},
		{"a create naming a seat", "enrol",
			map[string]any{"login": "dana.sre", "email": "dana@example.com",
				"seat": "sre"}, true},
		{"a lost acknowledgement (the control)", "enrol",
			map[string]any{"login": "dana.sre", "email": "dana@example.com"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.outcomes = map[string]statelog.Outcome{c.call: statelog.OutcomeUnknown}
			r.writer.unvouched = map[string]bool{c.call: c.unvouched}
			got := r.as(everything(), http.MethodPost, "/iam/people", c.body)
			retry := got.header.Get("Retry-After")
			switch {
			case got.status != http.StatusServiceUnavailable || got.body["op_id"] == nil ||
				got.body["outcome"] != httpjson.OutcomeUnknown:
				t.Fatalf("answered %d %v, want 503 naming the operation and "+
					"saying its outcome is unknown", got.status, got.body)
			case c.unvouched && (retry != "" || got.body["unvouched"] != true):
				t.Errorf("an unvouched unknown answered Retry-After %q, "+
					"unvouched %v", retry, got.body["unvouched"])
			case !c.unvouched && (retry != "2" || got.body["unvouched"] != nil):
				t.Errorf("a lost acknowledgement answered Retry-After %q, "+
					"unvouched %v", retry, got.body["unvouched"])
			}
		})
	}
}

// A WRITE NAMING A PERSON THE DIRECTORY REMOVED IS NOT FOUND.
//
// The domain refuses a record about a removed person ([statelog.ReasonDeleted])
// — a removal that landed between the read this route decided on and the
// record — and nothing will ever write them again. It was the generic 503,
// which sent an administrator looking for a node that would take a write about
// somebody who no longer exists.
//
// Mutation: drop the removal arm and every row answers 503.
func TestAWriteNamingARemovedPersonIsNotFound(t *testing.T) {
	t.Parallel()
	gone := &statelog.Unavailable{Reason: statelog.ReasonDeleted,
		Detail: "that person was removed"}
	for _, c := range []struct {
		name, call, method, target string
		body                       any
	}{
		{"an edit", "update", http.MethodPatch, "/iam/people/" + bob.String(),
			map[string]any{"grants": []string{"state:read"}}},
		{"ending their sessions", "revoke", http.MethodDelete,
			"/iam/people/" + bob.String() + "/sessions", nil},
		{"a seat binding", "identity", http.MethodPatch,
			"/iam/people/" + bob.String(), map[string]any{"seat": "sre"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.refusals = map[string]error{c.call: gone}
			got := r.as(everything(), c.method, c.target, c.body)
			if got.status != http.StatusNotFound ||
				got.body["error"] != string(httpjson.CodeNotFound) {
				t.Errorf("answered %d %v, want 404", got.status, got.body)
			}
			if !slices.Contains(r.writer.calls, c.call) {
				t.Errorf("the write was never asked for (%v); this case tests "+
					"nothing", r.writer.calls)
			}
		})
	}
}

// A RETRY ANSWERED FROM THE LEDGER IS ANSWERED, AND NEVER ANNOUNCED OR BUILT
// ON.
//
// A write retried under the key an earlier answer handed back is answered from
// the ledger before this call's decide runs ([statelog.Result.Collapsed]) — so
// what the decide would have reached is nobody's verdict, and the write was
// announced by the call that made it, where that call saw its own outcome.
// Announced again, one reset was two rows in the trail; read from an empty
// verdict, a revocation that landed answered "nothing changed".
//
// Mutation: announce on [landed] again and a row's event reappears; drop the
// collapsed arm of the credential's revocation and it says nothing changed.
func TestARetryAnsweredFromTheLedgerIsNeverAnnounced(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, call, method, target string
	}{
		{"a second factor's reset", "credentials", http.MethodPost,
			"/iam/people/" + bob.String() + "/mfa/reset"},
		{"ending somebody's sessions", "revoke", http.MethodDelete,
			"/iam/people/" + bob.String() + "/sessions"},
		{"a removal", "remove", http.MethodDelete, "/iam/people/" + bob.String()},
		{"a credential's revocation", "credentials", http.MethodDelete,
			"/iam/credentials/0192f00d-0000-7000-8000-00000000c0de?person=" +
				bob.String()},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.collapsed = map[string]bool{c.call: true}
			got := r.as(everything(), c.method, c.target, nil)
			if got.status != http.StatusOK {
				t.Fatalf("answered %d: %v", got.status, got.body)
			}
			if seen := r.audit.all(); len(seen) != 0 {
				t.Errorf("announced %#v for a write this call did not make", seen)
			}
			if detail, _ := got.body["detail"].(string); strings.Contains(detail,
				"nothing changed") {
				t.Errorf("a revocation that landed answered %q", detail)
			}
		})
	}
}

// A MINT THAT LANDED AS A COPY HANDS OUT NO VALUE.
//
// The domain answers a collapsed mint with [iamdomain.ErrCollapsed]: what the
// token was granted is the decide's, and the copy that landed may not be this
// call's — so a value built on it could carry grants no record gave it. The
// answer is the 503 every identity write this node could not finish is, with
// the operation, and nothing is shown or announced.
//
// Mutation: build the token on a collapsed mint and the value is shown.
func TestACollapsedMintHandsOutNoValue(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.collapsed = map[string]bool{"mint": true}
	got := r.as(administrator(), http.MethodPost,
		"/iam/credentials?person="+alice.String(), map[string]any{})
	if got.status != http.StatusServiceUnavailable || got.body["op_id"] == nil {
		t.Fatalf("answered %d %v, want 503 naming the operation", got.status, got.body)
	}
	if _, shown := got.body["token"]; shown {
		t.Error("a token value was handed out on a mint this call cannot prove")
	}
	if seen := r.audit.all(); len(seen) != 0 {
		t.Errorf("announced %#v", seen)
	}
}

// A STEP'S REFUSAL IS ANSWERED UNDER ITS GESTURE.
//
// Every step of a sequence publishes under an id derived from the gesture's
// ([statelog.StepOpID]), and the framework's refusal names the STEP's. The
// answer handed that back as `op_id` — the value a retry sends as the
// Idempotency-Key — and a step's id sent back is a new gesture: every one of
// its steps derives an id the first attempt never used, so a step that had
// landed was made again as an operation no ledger had seen. The answer names
// the gesture's, whatever step refused.
//
// Mutation: answer the refusal's own op id again and the step's comes back.
func TestAStepsRefusalIsAnsweredUnderItsGesture(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	key := statelog.NewOpID(time.Now(), "")
	r.writer.refusals = map[string]error{"identity": &statelog.Unavailable{
		Reason: statelog.ReasonEvicted, OpID: statelog.StepOpID(key, "identity"),
		Detail: "this node has been removed from the fleet"}}
	got := r.asWith(everything(), http.MethodPatch, "/iam/people/"+bob.String(),
		map[string]any{"seat": "sre"}, http.Header{opkey.Header: {key}})
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("answered %d %v, want the refusal's 503", got.status, got.body)
	}
	if !slices.Contains(r.writer.calls, "identity") {
		t.Fatalf("the bind was never asked for (%v); this case tests nothing",
			r.writer.calls)
	}
	// THE GESTURE'S KEY, SCOPED BY THE CALLER: what every step was
	// published as a step of, and what a retry sends back — never the
	// refused step's own id.
	answered, _ := got.body["op_id"].(string)
	claimed := r.writer.ops["identity"]
	if answered == "" || len(claimed) == 0 || !strings.HasPrefix(claimed[0], answered+".") {
		t.Errorf("answered op_id %q, want the gesture every step (%v) is a step of "+
			"— the one a retry sends back", answered, claimed)
	}
	if answered == statelog.StepOpID(key, "identity") {
		t.Errorf("answered the refused step's own id %q", answered)
	}
}

// TWO KEYS THAT SHARE A UUID ARE TWO CREATES. A create's id is derived from
// the uuid its scoped key begins with, and the scope derives that from the
// WHOLE key the caller sent — so a key with a name after the uuid is another
// operation naming another person, never a second key naming the same one,
// which a seed read off the caller's own uuid would be. Mutation: seed the
// create from the uuid the caller sent rather than the scoped key's, and the
// two creates name one person.
func TestTwoKeysSharingAUUIDCreateTwoPeople(t *testing.T) {
	t.Parallel()
	bare := statelog.NewOpID(time.Now(), "")
	named := statelog.StepOpID(bare, "people-create")
	people := map[string]string{}
	for _, key := range []string{bare, named} {
		r := newRig(t)
		got := r.asWith(administrator(), http.MethodPost, "/iam/people",
			map[string]any{"login": "dana.sre", "email": "dana@example.com"},
			http.Header{opkey.Header: {key}})
		if got.status/100 != 2 {
			t.Fatalf("a create under %q answered %d: %v", key, got.status, got.body)
		}
		people[key] = r.writer.enrolled.PersonID
	}
	if people[bare] == "" || people[bare] == people[named] {
		t.Errorf("the keys %q and %q created %q and %q, want two people",
			bare, named, people[bare], people[named])
	}
}
