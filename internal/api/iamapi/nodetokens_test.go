package iamapi_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THIS NODE'S TIER A TOKENS, JOINED TO THE ROWS THEIR LOGINS NAME.
//
// A label mistyped on one side of the join — in a node's Tier A, or on the
// directory row — leaves a token acting as itself while its operator believes
// it acts as a seat, and nothing else can say so: the labels are a node's
// configuration and no directory read reaches them. Each answer is one the
// dashboard renders differently — no row, a row naming no seat, a row naming
// a seat — so each is asserted.
//
// AND A ROW IS REPORTED AS WHAT IT HOLDS, never judged: a seat the company no
// longer holds is `/iam/check`'s finding about the same row, so the token
// naming one reads exactly like a token naming a seat that is there, with no
// verdict of this route's own beside it. Mutation: join on the bare label
// instead of `token:<id>` and every row reads `none`; add a `binding` back and
// the field check goes red.
func TestNodeTokensJoinEachLabelToItsRow(t *testing.T) {
	t.Parallel()
	ops := uuid.MustParse("018f3a9c-0000-7000-8000-0000000000d4")
	deploy := uuid.MustParse("018f3a9c-0000-7000-8000-0000000000e5")
	stale := uuid.MustParse("018f3a9c-0000-7000-8000-0000000000f6")
	r := newRig(t, func(o *iamapi.Options) {
		// UNSORTED, as a Tier A file lists them: the answer is sorted.
		o.TokenIDs = func() []string { return []string{"stale", "ops", "ci", "deploy"} }
		o.Bindings = func(_ context.Context, row iamdomain.PersonRow) (bool, string, error) {
			if row.Seat == "gone" {
				return true, "the seat gone is not in the org chart", nil
			}
			return false, "", nil
		}
	})
	for id, row := range map[uuid.UUID]iamdomain.PersonRow{
		ops:    {Login: "token:ops", Seat: "cto"},
		deploy: {Login: "token:deploy"},
		stale:  {Login: "token:stale", Seat: "gone"},
	} {
		row.ID, row.Kind, row.Stage = id.String(), iam.KindMachine, iam.StageActive
		r.directory.people[id.String()] = row
	}
	got := r.as(administrator(), http.MethodGet, "/iam/node-tokens", nil)
	if got.status != http.StatusOK {
		t.Fatalf("GET /iam/node-tokens = %d %v", got.status, got.body)
	}
	tokens, _ := got.body["tokens"].([]any)
	type row struct{ id, login, row, person, stage, seat string }
	var have []row
	for _, raw := range tokens {
		m, _ := raw.(map[string]any)
		str := func(k string) string { s, _ := m[k].(string); return s }
		have = append(have, row{str("id"), str("login"), str("row"), str("person"),
			str("stage"), str("seat")})
		for key := range m {
			switch key {
			case "id", "login", "row", "person", "stage", "seat", "company_writer":
			default:
				t.Errorf("token %s carries %q, a field beyond the row it holds — "+
					"whether its seat dangles is /iam/check's to say", str("id"), key)
			}
		}
	}
	want := []row{
		{"ci", "token:ci", "none", "", "", ""},
		{"deploy", "token:deploy", "held", deploy.String(), "active", ""},
		{"ops", "token:ops", "held", ops.String(), "active", "cto"},
		{"stale", "token:stale", "held", stale.String(), "active", "gone"},
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("tokens =\n  %+v\nwant\n  %+v", have, want)
	}

	// THE ONE REPORT NAMES THE STALE TOKEN'S SEAT, under the token's login.
	check := r.as(administrator(), http.MethodGet, "/iam/check", nil)
	finding := findingOf(check.body, string(iamapi.KindDanglingBinding))
	if finding == nil || finding["login"] != "token:stale" || finding["seat"] != "gone" {
		t.Errorf("GET /iam/check named %v, want the stale token's row and its seat",
			finding)
	}
}

// A MANAGED COMPANY DOCUMENT'S WRITERS ARE ANSWERED BESIDE THE LABELS, as this
// node's Tier A names them, and each of this node's tokens among them is
// marked — the one place the dashboard learns which credential alone may change
// the document (ADR-0030), since it is a node's configuration no directory
// read reaches. An unmanaged document answers an empty list, never a missing
// field a client must guess the meaning of. Mutation: drop the list and the
// first case reads none; mark by login instead of id and no token is marked.
func TestNodeTokensNameTheManagedDocumentsWriters(t *testing.T) {
	t.Parallel()
	managed := newRig(t, func(o *iamapi.Options) {
		o.TokenIDs = func() []string { return []string{"ops", "gitops"} }
		// UNSORTED, and naming one id this node holds no token for: the
		// list is the Tier A's, and the mark is this node's.
		o.CompanyWriters = []string{"gitops", "ci"}
	})
	got := managed.as(administrator(), http.MethodGet, "/iam/node-tokens", nil)
	if got.status != http.StatusOK {
		t.Fatalf("GET /iam/node-tokens = %d %v", got.status, got.body)
	}
	if writers, _ := got.body["company_writers"].([]any); !reflect.DeepEqual(writers,
		[]any{"ci", "gitops"}) {
		t.Errorf("company_writers = %v, want [ci gitops], sorted", got.body["company_writers"])
	}
	// THE MARK IS ABSENT, NEVER FALSE, on a token the list does not name:
	// the documented answer carries the key on a writer alone, so a client
	// that tests for the key rather than its value reads the same thing.
	marked := map[string]any{}
	tokens, _ := got.body["tokens"].([]any)
	for _, raw := range tokens {
		m, _ := raw.(map[string]any)
		id, _ := m["id"].(string)
		if mark, present := m["company_writer"]; present {
			marked[id] = mark
		}
	}
	if !reflect.DeepEqual(marked, map[string]any{"gitops": true}) {
		t.Errorf("company_writer marks = %v, want gitops alone and the key "+
			"absent on ops", marked)
	}

	unmanaged := newRig(t, func(o *iamapi.Options) {
		o.TokenIDs = func() []string { return []string{"ops"} }
	})
	got = unmanaged.as(administrator(), http.MethodGet, "/iam/node-tokens", nil)
	if writers, ok := got.body["company_writers"].([]any); !ok || len(writers) != 0 {
		t.Errorf("an unmanaged document answered company_writers %v, want an empty list",
			got.body["company_writers"])
	}
}

// WHO CAN REACH THIS COMPANY, AND AS WHOM, is the directory's read: an
// administrator is answered, and a reader holding every app and not admin is
// refused — never the labels, which are a map of what to try.
func TestNodeTokensAreTheDirectorysRead(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *iamapi.Options) {
		o.TokenIDs = func() []string { return []string{"ops"} }
	})
	for name, tc := range map[string]struct {
		p    iam.Principal
		want int
	}{
		"admin":                {administrator(), http.StatusOK},
		"every app, not admin": {ordinary(), http.StatusForbidden},
	} {
		if got := r.as(tc.p, http.MethodGet, "/iam/node-tokens", nil); got.status != tc.want {
			t.Errorf("%s: GET /iam/node-tokens = %d %v, want %d", name, got.status, got.body, tc.want)
		}
	}
}

// A DIRECTORY THIS NODE CANNOT READ IS A 503, never a list of tokens that all
// read `none` — which is exactly the answer this route exists to make
// alarming.
func TestNodeTokensOverAnUnreadableDirectoryAreUnavailable(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *iamapi.Options) {
		o.TokenIDs = func() []string { return []string{"ops"} }
	})
	r.directory.err = errors.New("the replicated store is not open")
	if got := r.as(administrator(), http.MethodGet, "/iam/node-tokens", nil); got.status != http.StatusServiceUnavailable {
		t.Errorf("GET /iam/node-tokens over a failing read = %d %v, want 503", got.status, got.body)
	}
}

// THE SURFACE IS REFUSED WITHOUT THE NODE'S LABELS, by name: every node that
// serves it holds a Tier A token, and an empty list would read as one that
// has nothing to bind.
func TestTheDirectoryNeedsTheNodesTokenLabels(t *testing.T) {
	t.Parallel()
	_, err := iamapi.New(iamapi.Options{
		Directory: &fakeDirectory{},
		Authority: func(iam.Principal) iamapi.Writer { return &fakeWriter{} },
		Opener:    fakeOpener{},
		Audit:     &recordingAudit{},
	})
	if err == nil || !strings.Contains(err.Error(), "token labels") {
		t.Fatalf("building without the labels answered %v, want a refusal naming them", err)
	}
}
