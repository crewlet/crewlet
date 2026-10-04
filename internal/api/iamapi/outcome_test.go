package iamapi_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A WRITE NOBODY CAN CONFIRM IS NEITHER ANSWERED NOR ANNOUNCED AS LANDED.
//
// The writer answered a bare position, so an `unknown` outcome came back as a
// zero position beside a nil error and every route read it as success: a 200,
// an invitation link, a token value, and the removal, revocation, reset and
// credential events beside them — each said of a write that may not exist.
// Each case makes the route's write answer unknown and holds it to a 503
// carrying the Retry-After and the op id, with nothing announced and nothing
// handed out.
//
// Mutation: treat unknown as landed on any one route and its case answers 200
// (or 201), announces an event, or hands out a link or a token.
func TestAWriteNobodyCanConfirmIsNeitherAnsweredNorAnnounced(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, call, method, target string
		body                       any
	}{
		{"removing a person", "remove", http.MethodDelete,
			"/iam/people/" + bob.String(), nil},
		{"ending somebody's sessions", "revoke", http.MethodDelete,
			"/iam/people/" + bob.String() + "/sessions", nil},
		{"resetting a second factor", "credentials", http.MethodPost,
			"/iam/people/" + bob.String() + "/mfa/reset", nil},
		{"revoking a credential", "credentials", http.MethodDelete,
			"/iam/credentials/c-1?person=" + bob.String(), nil},
		{"minting a token", "mint", http.MethodPost,
			"/iam/credentials?person=" + alice.String(), map[string]any{"label": "ci"}},
		{"inviting somebody", "invite", http.MethodPost, "/iam/invitations",
			map[string]any{"email": "dana@example.com"}},
		{"creating a person", "enrol", http.MethodPost, "/iam/people",
			map[string]any{"login": "dana.sre", "email": "dana@example.com"}},
		{"invalidating every session", "invalidate", http.MethodPost,
			"/iam/invalidate-all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.outcomes = map[string]statelog.Outcome{
				tc.call: statelog.OutcomeUnknown,
			}
			caller := administrator()
			caller.Grants = append(caller.Grants, iam.GrantFleetOperate)
			got := r.as(caller, tc.method, tc.target, tc.body)
			if got.status != http.StatusServiceUnavailable ||
				got.header.Get("Retry-After") != "2" {
				t.Fatalf("answered %d with Retry-After %q (%v), want 503 carrying 2",
					got.status, got.header.Get("Retry-After"), got.body)
			}
			if op, _ := got.body["op_id"].(string); op == "" {
				t.Errorf("body %v names no operation id — the only safe retry "+
					"is under the same one", got.body)
			}
			for _, handed := range []string{"url", "token"} {
				if _, ok := got.body[handed]; ok {
					t.Errorf("handed out a %s for a write nobody can confirm", handed)
				}
			}
			if seen := r.audit.all(); len(seen) != 0 {
				t.Errorf("announced %v about a write whose outcome is unknown", seen)
			}
		})
	}
}

// A CREATE RETRIED UNDER THE KEY ITS UNKNOWN ANSWER CARRIED NAMES WHAT ITS
// FIRST ATTEMPT CREATED.
//
// The unknown answer says the only safe retry is the SAME one, sent back as the
// Idempotency-Key — and both creates minted a fresh id per request, so the
// retry named a second person or a second invitation, which the address the
// first attempt claimed refused as somebody else's: 409 against its own first
// attempt, and the link to an invitation that may have landed shown to nobody.
// Mutation: mint the person or the invitation per request again and the retry
// names another one.
func TestACreateRetriedUnderItsKeyNamesWhatItsFirstAttemptCreated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, call, target string
		body               map[string]any
		created            func(r *rig, answer answered) string
	}{
		{"a person", "enrol", "/iam/people",
			map[string]any{"login": "dana.sre", "email": "dana@example.com"},
			func(r *rig, _ answered) string { return r.writer.enrolled.PersonID }},
		{"an invitation", "invite", "/iam/invitations",
			map[string]any{"email": "dana@example.com"},
			func(_ *rig, answer answered) string {
				id, _ := answer.body["id"].(string)
				return id
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.outcomes = map[string]statelog.Outcome{
				tc.call: statelog.OutcomeUnknown}
			first := r.as(administrator(), http.MethodPost, tc.target, tc.body)
			key, _ := first.body["op_id"].(string)
			if first.status != http.StatusServiceUnavailable || key == "" {
				t.Fatalf("the first attempt answered %d (%v), want 503 with "+
					"the key to retry under", first.status, first.body)
			}
			firstPerson := r.writer.enrolled.PersonID

			r.writer.outcomes = nil
			retry := r.asWith(administrator(), http.MethodPost, tc.target,
				tc.body, http.Header{opkey.Header: {key}})
			if retry.status/100 != 2 {
				t.Fatalf("the retry answered %d: %v", retry.status, retry.body)
			}
			if tc.call == "enrol" && tc.created(r, retry) != firstPerson {
				t.Errorf("the retry enrolled %s, want its first attempt's %s",
					tc.created(r, retry), firstPerson)
			}
			if tc.call == "invite" {
				// THE SAME LINK: the id is derived from the key, so the
				// retry hands back the invitation the first attempt may
				// have issued.
				again := r.asWith(administrator(), http.MethodPost, tc.target,
					tc.body, http.Header{opkey.Header: {key}})
				if tc.created(r, retry) == "" ||
					tc.created(r, again) != tc.created(r, retry) {
					t.Errorf("two retries under one key answered invitations "+
						"%q and %q", tc.created(r, retry), tc.created(r, again))
				}
			}
			// AN ISSUE IS PUBLISHED UNDER ITS KEY'S SEED, the uuid the
			// scoped key the answer handed back begins with.
			seed, _, _ := strings.Cut(key, ".")
			if r.writer.invited.OpID != "" && r.writer.invited.OpID != seed {
				t.Errorf("the retry was published under %q, want its key's seed %q",
					r.writer.invited.OpID, seed)
			}
		})
	}
}

// A CREATE'S KEY IS A UUID7, because the id it creates is derived from it and
// every id this estate creates is a uuid7 whose instant is its creation.
func TestACreatesKeyThatIsNoUUID7IsRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	for _, target := range []string{"/iam/people", "/iam/invitations"} {
		got := r.asWith(administrator(), http.MethodPost, target,
			map[string]any{"login": "dana.sre", "email": "dana@example.com"},
			http.Header{opkey.Header: {"retry-1"}})
		if got.status != http.StatusBadRequest {
			t.Errorf("%s under the key retry-1 answered %d, want 400", target,
				got.status)
		}
	}
	if len(r.writer.calls) != 0 {
		t.Errorf("a refused key still published %v", r.writer.calls)
	}
}

// A WRITE DURABLE AND NOT YET APPLIED HERE IS A 202, NOT A 200.
//
// `200` promises the administrator's next read HERE sees what they wrote;
// a pending record will be applied by every node and this one has not yet.
// The package doc promised the 202 and the handler never gave it. Mutation:
// answer pending as applied and the status is 200.
func TestAWriteDurableButNotAppliedHereIsAccepted(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.outcomes = map[string]statelog.Outcome{"revoke": statelog.OutcomePending}
	got := r.as(administrator(), http.MethodDelete,
		"/iam/people/"+bob.String()+"/sessions", nil)
	if got.status != http.StatusAccepted || got.body["position"] == "" ||
		got.body["outcome"] != string(statelog.OutcomePending) {
		t.Fatalf("answered %d (%v), want 202 with the position to read at",
			got.status, got.body)
	}
	// DURABLE IS LANDED: the ending is announced, because every node will
	// apply it.
	if seen := r.audit.all(); len(seen) != 1 {
		t.Errorf("announced %d events for a durable revocation, want 1", len(seen))
	}
}

// A STEP NOBODY CAN CONFIRM ENDS THE SEQUENCE THERE.
//
// An edit is several records, and each is built on the one before: a seat
// binding whose outcome is unknown may or may not be on the log, so the stage
// and the document after it must not be published over a guess. Mutation:
// carry on past an unknown step and the update is asked for.
func TestAnEditStopsAtAStepNobodyCanConfirm(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.outcomes = map[string]statelog.Outcome{"claim:seat": statelog.OutcomeUnknown}
	stage := iam.StageSuspended
	got := r.as(administrator(), http.MethodPatch, "/iam/people/"+bob.String(),
		map[string]any{"seat": "platform", "stage": stage,
			"grants": []iam.Grant{iam.GrantStateRead}})
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("answered %d (%v), want 503", got.status, got.body)
	}
	for _, call := range r.writer.calls {
		if call == "stage" || call == "update" {
			t.Errorf("published %q after a step nobody could confirm (calls %v)",
				call, r.writer.calls)
		}
	}
}

// AN ESTATE THAT COULD NOT DECIDE IS A 503 THAT SAYS WHEN TO COME BACK, AND
// WHICH OPERATION TO COME BACK WITH.
//
// A refusal the framework raises — this node is behind, below the trim floor,
// holding a record it cannot decode — answered a bare 503 with no Retry-After
// and no op id, which a client cannot tell from a node gone for good, and
// which leaves the retry the docs promise with nothing to retry under.
//
// AND WHEN IS THE REFUSAL'S OWN ANSWER. A write the log refused for good — a
// record too large for the broker, a full log, a refusal the broker named, an
// evicted node — carried the identity hint too, telling a client to come back
// in two seconds for a write that could never land here, where the same
// refusal on /work carries none. A node that is behind is the
// control, and keeps the hint.
func TestARefusedWriteSaysWhenAndWhatToRetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason statelog.Reason
		retry  string
	}{
		{statelog.ReasonBehind, "2"},
		{statelog.ReasonRecordTooLarge, ""},
		{statelog.ReasonLogFull, ""},
		{statelog.ReasonBrokerRefused, ""},
		{statelog.ReasonEvicted, ""},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.err = fmt.Errorf("iamdomain: publish: %w", &statelog.Unavailable{
				Reason: tc.reason, Detail: "what the refusal is about"})
			req := "/iam/people/" + bob.String() + "/sessions"
			got := r.as(administrator(), http.MethodDelete, req, nil)
			if got.status != http.StatusServiceUnavailable ||
				got.header.Get("Retry-After") != tc.retry {
				t.Fatalf("a %s refusal answered %d with Retry-After %q, want 503 "+
					"with %q", tc.reason, got.status, got.header.Get("Retry-After"),
					tc.retry)
			}
			// THE KEY THE WRITE WAS PUBLISHED UNDER A STEP OF, which is
			// what a retry sends back ([iamapi.Service]'s opIDFor).
			if op, _ := got.body["op_id"].(string); len(r.writer.ops["revoke"]) != 1 ||
				op == "" || !strings.HasPrefix(r.writer.ops["revoke"][0], op+".") {
				t.Errorf("op id %q, want the key this route published under a "+
					"step of %v", op, r.writer.ops["revoke"])
			}
		})
	}
}

// A WRITE THAT FAULTED KEEPS ITS OWN WORDS IN THE LOG.
//
// `internal_error` says the reason is in this node's log, and a fault's reason
// is a store's or a driver's words — a database path here — which the directory
// sent as the answer's `detail`. The control is a refusal by the domain, whose
// sentence is written for the caller and still travels.
func TestADirectoryWriteThatFaultedKeepsItsWordsInTheLog(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.err = errors.New("open /var/lib/crewlet/replicated.db: disk I/O error")
	got := r.as(administrator(), http.MethodDelete, "/iam/people/"+bob.String()+"/sessions", nil)
	if got.status != http.StatusInternalServerError {
		t.Fatalf("a write that faulted answered %d (%v), want 500", got.status, got.body)
	}
	if detail, _ := got.body["detail"].(string); strings.Contains(detail, "/var/lib") {
		t.Errorf("the fault's own words reached the caller: %v", got.body)
	}
}

// AN INVITATION WITH NO ADDRESS TO POINT AT IS A FAULT, NOT AN OUTAGE.
//
// `api.external_url` is this node's own configuration, and waiting never
// supplies it; the 503 it answered told every client to retry in two seconds
// for ever.
func TestAnInvitationWithNoExternalURLIsAFault(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *iamapi.Options) { o.ExternalBase = "" })
	got := r.as(administrator(), http.MethodPost, "/iam/invitations",
		map[string]any{"email": "dana@example.com"})
	if got.status != http.StatusInternalServerError || got.body["error"] != "no_external_url" {
		t.Errorf("answered %d (%v), want 500 no_external_url", got.status, got.body)
	}
}

// EVERY 503 THIS SURFACE ANSWERS SAYS WHETHER TO COME BACK, BECAUSE NOTHING
// HERE SPELLS ONE ITSELF OR PICKS ITS HINT WITHOUT ITS CAUSE.
//
// The same gate internal/api/authapi holds itself to: three sites here spelled
// a bare 503 through FailWith, and a behavioural case per site says nothing
// about the next one. No file in this package names the status, so
// httpjson.Unavailable and httpjson.UnavailableWith — which pair it with the
// header — are the only ways to answer one. And no file names the bare hint:
// every site asks auth.RetryIdentity with the 503's cause, so a refusal no
// wait clears carries no Retry-After here as it carries none on /work.
func TestEveryUnavailableAnswerHereSaysWhetherToComeBack(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "http" &&
					n.Sel.Name == "StatusServiceUnavailable" {
					t.Errorf("%s spells a 503 itself; answer it with "+
						"httpjson.Unavailable or UnavailableWith, which carry "+
						"the Retry-After", fset.Position(n.Pos()))
				}
				if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "auth" &&
					n.Sel.Name == "RetryIdentitySeconds" {
					t.Errorf("%s takes the identity hint whatever caused the "+
						"503; ask auth.RetryIdentity with the cause, nil where "+
						"there is none", fset.Position(n.Pos()))
				}
			case *ast.BasicLit:
				if n.Kind == token.INT && n.Value == "503" {
					t.Errorf("%s spells a 503 as a literal", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	// THE CONTROL: a glob that matched nothing would pass every rule.
	if checked < 5 {
		t.Fatalf("checked %d source files, which is not this package", checked)
	}
}
