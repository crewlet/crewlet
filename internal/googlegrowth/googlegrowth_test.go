package googlegrowth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
)

var patch = growth.Operation{ID: "x.items.patch", Method: "PATCH", Resource: "items", Class: growth.ClassOperate,
	Path: "https://x.googleapis.com/v1/accounts/{accountsId}/items/{itemsId}"}

// A REQUEST ADDRESSES ITS OPERATION'S RESOURCE AND NOTHING ELSE: a path
// parameter that is no ID, or one missing, is refused before any vendor.
func TestARequestAddressesOnlyItsOperationsResource(t *testing.T) {
	t.Parallel()
	for _, path := range []map[string]string{
		{"accountsId": "1/../2", "itemsId": "3"}, {"accountsId": "1"}, {"accountsId": "1", "itemsId": "a b"},
	} {
		if _, err := googlegrowth.NewRequest(patch, path, nil, nil); err == nil {
			t.Errorf("%v was accepted as path parameters", path)
		}
	}
	req, err := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "a:b"},
		map[string]string{"updateMask": "name"}, map[string]any{"name": "n"})
	if err != nil || req.Target() != "accounts/1/items/a:b" || req.AccountOrTarget() != "accounts/1/items/a:b" ||
		req.URL() != "https://x.googleapis.com/v1/accounts/1/items/a:b?updateMask=name" {
		t.Fatalf("request %+v (%s), %v", req, req.Target(), err)
	}
	if req.Resource() != req.Address {
		t.Errorf("a patch's before-state is read at %q", req.Resource())
	}
	archive := growth.Operation{ID: "x.items.archive", Method: "POST", Path: patch.Path + ":archive"}
	create := growth.Operation{ID: "x.items.create", Method: "POST", Path: "https://x.googleapis.com/v1/items"}
	if a, _ := googlegrowth.NewRequest(archive, map[string]string{"accountsId": "1", "itemsId": "a:b"}, nil, nil); a.Resource() != req.Address {
		t.Errorf("an archive's before-state is read at %q", a.Resource())
	}
	if c, _ := googlegrowth.NewRequest(create, nil, nil, nil); c.Resource() != "" {
		t.Errorf("a create reads a before-state at %q", c.Resource())
	}
}

// AN APPROVAL BINDS TO THE EXACT REQUEST AND ITS TARGET: the digest moves
// with the body, the query and the before-state, and with nothing else.
func TestABindingMovesWithTheRequestOrItsTarget(t *testing.T) {
	t.Parallel()
	build := func(mask, name string) googlegrowth.Request {
		req, err := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "2"},
			map[string]string{"updateMask": mask}, map[string]any{"name": name})
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	base := build("name", "a").Digest([]byte(`{"name":"z"}`))
	if again := build("name", "a").Digest([]byte(`{"name":"z"}`)); again != base || len(base) != 64 {
		t.Fatalf("the same request bound to %s and %s", base, again)
	}
	for what, d := range map[string]string{
		"body":   build("name", "b").Digest([]byte(`{"name":"z"}`)),
		"query":  build("name,title", "a").Digest([]byte(`{"name":"z"}`)),
		"target": build("name", "a").Digest([]byte(`{"name":"y"}`)),
	} {
		if d == base {
			t.Errorf("a changed %s left the binding as it was", what)
		}
	}
}

// A FAILURE IS A CLOSED REASON, never Google's words, which may quote what
// was sent.
func TestAFailureIsAClosedReason(t *testing.T) {
	t.Parallel()
	for err, want := range map[error]growth.Failure{
		&googleauth.APIError{Status: http.StatusUnauthorized}:       growth.FailureUnauthorized,
		&googleauth.APIError{Status: http.StatusTooManyRequests}:    growth.FailureRateLimited,
		&googleauth.APIError{Status: http.StatusServiceUnavailable}: growth.FailureVendorUnavailable,
		&googleauth.APIError{Status: http.StatusForbidden}:          growth.FailureVendorRefused,
		fmt.Errorf("call: %w", context.DeadlineExceeded):            growth.FailureTimedOut,
		errors.New("connection reset"):                              growth.FailureVendorUnavailable,
	} {
		if got := googlegrowth.FailureOf(err); got != want || !got.Valid() {
			t.Errorf("%v failed as %q, want %q", err, got, want)
		}
	}
}

// FREE WORDS GO TO THE ACCESS LOG AS THEIR DIGEST: a name, a description or
// an email is Text; an ID, an enum or a number is kept as it is.
func TestFreeWordsGoToTheLogAsTheirDigest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		before, after any
		text          bool
	}{
		{"PAUSED", "ENABLED", false}, {nil, float64(1200000), false}, {"properties/1", nil, false},
		{"Old name", "New name", true}, {nil, "someone@example.com", true}, {nil, []any{"A", "B"}, true},
	} {
		got := googlegrowth.RecordChange(growth.Change{Field: "f", Before: tc.before, After: tc.after})
		if got.Text != tc.text {
			t.Errorf("%v to %v: Text %v, want %v", tc.before, tc.after, got.Text, tc.text)
		}
	}
}

// AN APPROVED CALL IS RECORDED UNDER ITS HELD CALL ALONE, since a record
// names its act once.
func TestAnApprovedCallIsRecordedUnderItsHeldCall(t *testing.T) {
	t.Parallel()
	c := &googlegrowth.Client{Kind: "x"}
	req, _ := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "2"}, nil, nil)
	r := c.Record(growth.Call{ID: "c", HeldCall: "h", Tool: "x_admin"}, req, growth.ClassAdmin, []byte(`{}`))
	if r.Call != "" || r.HeldCall != "h" || len(r.Before) != 64 || r.Connection != googlegrowth.Connection {
		t.Errorf("record %+v", r)
	}
	if r := c.Record(growth.Call{ID: "c", Tool: "x_operate"}, req, growth.ClassOperate, nil); r.Call != "c" || r.Before != "" {
		t.Errorf("a call made at once is recorded as %+v", r)
	}
}

// A CREDENTIAL GOOGLE RETURNS IS WITHHELD at any depth of an answer.
func TestACredentialInAnAnswerIsWithheld(t *testing.T) {
	t.Parallel()
	c := &googlegrowth.Client{Kind: "x", Secrets: []string{"secretValue"}}
	out, err := c.AnswerOf(googlegrowth.Request{Op: patch}, []byte(`{"items":[{"secretValue":"s3cr3t","name":"a"}]}`), nil)
	if err != nil || strings.Contains(out.Answer, "s3cr3t") || !strings.Contains(out.Answer, `"name":"a"`) {
		t.Errorf("answer %+v, %v", out, err)
	}
}

// A PRODUCT'S OWN RULE NEVER ADMITS A DOT SEGMENT: a URL-shaped value is
// escaped into one path segment, and a value made only of dots, which would
// address the resource above, is refused whatever the rule says.
func TestAProductsParameterRuleNeverAdmitsADotSegment(t *testing.T) {
	t.Parallel()
	site := growth.Operation{ID: "x.sites.get", Method: "GET", Path: "https://x.googleapis.com/webmasters/v3/sites/{siteUrl}"}
	anyValue := func(string, string) bool { return true }
	req, err := googlegrowth.NewRequestWith(site, map[string]string{"siteUrl": "https://www.example.com/"}, nil, nil, anyValue)
	if err != nil || !strings.HasSuffix(req.Address, "/sites/https:%2F%2Fwww.example.com%2F") ||
		req.Target() != "sites/https://www.example.com/" {
		t.Fatalf("a site's request is %q, target %q, %v", req.Address, req.Target(), err)
	}
	for _, v := range []string{".", "..", "..."} {
		if _, err := googlegrowth.NewRequestWith(site, map[string]string{"siteUrl": v}, nil, nil, anyValue); err == nil {
			t.Errorf("%q was accepted as a path parameter", v)
		}
	}
}
