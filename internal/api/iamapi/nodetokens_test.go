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
// dashboard renders differently — no row, a reservation an enrolment stopped
// at, a row naming no seat, a seat the chart still holds, one it does not — so
// each is asserted. Mutation: join on the bare label instead of `token:<id>`
// and every row reads `none`; read a reservation as no row and `half` reads
// `none`; drop the bindings seam and the dangling row reads `bound`.
func TestNodeTokensJoinEachLabelToItsRow(t *testing.T) {
	t.Parallel()
	ops := uuid.MustParse("018f3a9c-0000-7000-8000-0000000000d4")
	deploy := uuid.MustParse("018f3a9c-0000-7000-8000-0000000000e5")
	stale := uuid.MustParse("018f3a9c-0000-7000-8000-0000000000f6")
	r := newRig(t, func(o *iamapi.Options) {
		// UNSORTED, as a Tier A file lists them: the answer is sorted.
		o.TokenIDs = func() []string { return []string{"stale", "ops", "half", "ci", "deploy"} }
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
	// AN ENROLMENT THAT STOPPED AFTER CLAIMING THE LOGIN: a reservation,
	// held by nobody yet, which is neither "no row" nor a row to bind.
	r.directory.reserved = map[string]bool{"token:half": true}

	got := r.as(administrator(), http.MethodGet, "/iam/node-tokens", nil)
	if got.status != http.StatusOK {
		t.Fatalf("GET /iam/node-tokens = %d %v", got.status, got.body)
	}
	tokens, _ := got.body["tokens"].([]any)
	type row struct{ id, login, row, person, seat, binding string }
	var have []row
	for _, raw := range tokens {
		m, _ := raw.(map[string]any)
		str := func(k string) string { s, _ := m[k].(string); return s }
		have = append(have, row{str("id"), str("login"), str("row"), str("person"),
			str("seat"), str("binding")})
	}
	want := []row{
		{"ci", "token:ci", "none", "", "", "unbound"},
		{"deploy", "token:deploy", "held", deploy.String(), "", "unbound"},
		{"half", "token:half", "reserved", "", "", "unbound"},
		{"ops", "token:ops", "held", ops.String(), "cto", "bound"},
		{"stale", "token:stale", "held", stale.String(), "gone", "dangling"},
	}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("tokens =\n  %+v\nwant\n  %+v", have, want)
	}
	last, _ := tokens[len(tokens)-1].(map[string]any)
	if detail, _ := last["detail"].(string); !strings.Contains(detail, "not in the org chart") {
		t.Errorf("the dangling binding carries detail %q, want the seam's own words", detail)
	}
}

// WHO CAN REACH THIS COMPANY, AND AS WHOM, is the directory's read: an
// administrator and an auditor are answered, a reader holding neither grant is
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
		"people:manage": {administrator(), http.StatusOK},
		"audit:read":    {auditor(), http.StatusOK},
		"neither":       {ordinary(), http.StatusForbidden},
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
