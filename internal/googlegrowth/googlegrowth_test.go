package googlegrowth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

var patch = growth.Operation{ID: "x.items.patch", Method: "PATCH", Resource: "items", Class: growth.ClassOperate,
	Path: "https://x.googleapis.com/v1/accounts/{accountsId}/items/{itemsId}"}

// export is a field-classed write, as a catalog's rules name its fields.
var export = growth.Operation{ID: "x.links.patch", Method: "PATCH", Resource: "links", Class: growth.ClassOperate,
	Path: "https://x.googleapis.com/v1/accounts/{accountsId}/links/{linksId}",
	Fields: []growth.FieldRule{
		{Field: "displayName", Class: growth.ClassOperate},
		{Field: "dailyExportEnabled", Class: growth.ClassSpend, Pausing: []any{false}},
		{Field: "restrictedMetricType", Class: growth.ClassAdmin},
	}}

// A REQUEST ADDRESSES ITS OPERATION'S RESOURCE AND NOTHING ELSE: a path
// parameter that is no ID, one missing, or one its path does not name is
// refused before any vendor.
//
// Mutation: drop the stray-parameter check, and a parameter the path does not
// name is taken, for a product to read the call's account from.
func TestARequestAddressesOnlyItsOperationsResource(t *testing.T) {
	t.Parallel()
	for _, path := range []map[string]string{
		{"accountsId": "1/../2", "itemsId": "3"}, {"accountsId": "1"}, {"accountsId": "1", "itemsId": "a b"},
		{"accountsId": "1", "itemsId": "3", "propertiesId": "222"},
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

// A CUSTOM VERB ON A COLLECTION READS NO BEFORE-STATE: a collection is a
// listing, which needs what the call does not give (a filter) and binds to
// nothing one read returns.
//
// Mutation: read every verb's resource, and the roll-up's create reads
// /v1alpha/properties, which Google refuses.
func TestACustomVerbOnACollectionReadsNoBeforeState(t *testing.T) {
	t.Parallel()
	for _, op := range []growth.Operation{
		{ID: "x.properties.createRollupProperty", Method: "POST", Path: "https://x.googleapis.com/v1alpha/properties:createRollupProperty"},
		{ID: "x.accounts.provisionAccountTicket", Method: "POST", Path: "https://x.googleapis.com/v1beta/accounts:provisionAccountTicket"},
		{ID: "x.bindings.batchCreate", Method: "POST", Path: "https://x.googleapis.com/v1alpha/accounts/{accountsId}/accessBindings:batchCreate"},
	} {
		var path map[string]string
		if strings.Contains(op.Path, "{accountsId}") {
			path = map[string]string{"accountsId": "1"}
		}
		req, err := googlegrowth.NewRequest(op, path, nil, nil)
		if err != nil || req.Resource() != "" {
			t.Errorf("%s reads its before-state at %q (%v)", op.ID, req.Resource(), err)
		}
	}
}

// A WRITE IS CLASSED AS GOOGLE READS IT: a snake_case mask or body key, the
// `update_mask` spelling and a `*` mask that clears what the body leaves out
// all come to the class of what Google changes, and a field named twice with
// two values is read as neither.
//
// Mutation: read the mask by `updateMask` alone, take names as written, or read
// a `*` mask as the body's fields alone, and a restriction is lifted or an
// export turned on at operate.
func TestAWriteIsClassedAsGoogleReadsIt(t *testing.T) {
	t.Parallel()
	before := []byte(`{"displayName":"Old","dailyExportEnabled":true,"restrictedMetricType":["REVENUE_DATA"]}`)
	class := func(query map[string]string, body map[string]any) growth.Class {
		req, err := googlegrowth.NewRequest(export, map[string]string{"accountsId": "1", "linksId": "2"}, query, body)
		if err != nil {
			t.Fatal(err)
		}
		return googlegrowth.NewHeld(req, before).Class()
	}
	for name, tc := range map[string]struct {
		query map[string]string
		body  map[string]any
		want  growth.Class
	}{
		"a rename":                 {map[string]string{"updateMask": "displayName"}, map[string]any{"displayName": "New"}, growth.ClassOperate},
		"a pause":                  {map[string]string{"updateMask": "dailyExportEnabled"}, map[string]any{"dailyExportEnabled": false}, growth.ClassOperate},
		"a snake_case mask":        {map[string]string{"updateMask": "restricted_metric_type"}, map[string]any{"restrictedMetricType": []any{}}, growth.ClassAdmin},
		"the update_mask spelling": {map[string]string{"update_mask": "restrictedMetricType"}, map[string]any{"restrictedMetricType": []any{}}, growth.ClassAdmin},
		"both spellings":           {map[string]string{"updateMask": "displayName", "update_mask": "daily_export_enabled"}, map[string]any{"displayName": "N", "daily_export_enabled": true}, growth.ClassSpend},
		"a snake_case body":        {nil, map[string]any{"daily_export_enabled": true}, growth.ClassSpend},
		"one field named twice":    {nil, map[string]any{"dailyExportEnabled": false, "daily_export_enabled": true}, growth.ClassSpend},
		"a full replacement":       {map[string]string{"updateMask": "*"}, map[string]any{"displayName": "New"}, growth.ClassAdmin},
		"a field no rule names":    {map[string]string{"updateMask": "project"}, map[string]any{"project": "projects/9"}, growth.ClassAdmin},
	} {
		if got := class(tc.query, tc.body); got != tc.want {
			t.Errorf("%s: %s, want %s", name, got, tc.want)
		}
	}
	if got := googlegrowth.LowerCamel("rollup_property.source_properties"); got != "rollupProperty.sourceProperties" {
		t.Errorf("a snake_case path reads %q", got)
	}
}

// A BATCH'S TARGETS ARE READ THROUGH THE PRODUCT'S READ: where no one resource
// holds what a write changes, the product's before-read is what the binding
// holds; where it has none, the write's own resource is read; and its refusal
// reaches no vendor.
//
// Mutation: ignore the product's before-read, and a batch binds its request
// alone.
func TestABatchsTargetsAreReadThroughTheProductsRead(t *testing.T) {
	t.Parallel()
	var got []string
	w := newWorld(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"items":[1]}`))}, nil
	}))
	batch := growth.Operation{ID: "x.items.batchDelete", Method: "POST", Class: growth.ClassAdmin,
		Path: "https://x.googleapis.com/v1/accounts/{accountsId}/items:batchDelete"}
	get := growth.Operation{ID: "x.items.batchGet", Method: "GET", Class: growth.ClassRead,
		Path: "https://x.googleapis.com/v1/accounts/{accountsId}/items:batchGet"}
	w.client.BeforeRead = func(ctx context.Context, handle string, req googlegrowth.Request) ([]byte, bool, error) {
		if req.Op.ID != batch.ID {
			return nil, false, nil
		}
		if req.Field("refuse") != nil {
			return nil, false, errors.New("refused")
		}
		read, err := googlegrowth.NewRequest(get, req.Params, map[string]string{"names": "accounts/1/items/2"}, nil)
		if err != nil {
			return nil, false, err
		}
		raw, err := w.client.Send(ctx, handle, read)
		return raw, err == nil, err
	}
	req, _ := googlegrowth.NewRequest(batch, map[string]string{"accountsId": "1"}, nil, map[string]any{"names": []any{"accounts/1/items/2"}})
	h, err := w.client.Read(t.Context(), "seat-a", req)
	if err != nil || string(h.Before) != `{"items":[1]}` || len(got) != 1 ||
		got[0] != "GET /v1/accounts/1/items:batchGet?names=accounts%2F1%2Fitems%2F2" {
		t.Fatalf("the batch read %v into %s, %v", got, h.Before, err)
	}
	patchReq, _ := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "2"}, nil, nil)
	if _, err := w.client.Read(t.Context(), "seat-a", patchReq); err != nil || got[1] != "GET /v1/accounts/1/items/2?" {
		t.Errorf("a patch read %v, %v", got, err)
	}
	refused, _ := googlegrowth.NewRequest(batch, map[string]string{"accountsId": "1"}, nil, map[string]any{"refuse": true})
	if _, err := w.client.Read(t.Context(), "seat-a", refused); err == nil || len(got) != 2 {
		t.Errorf("a refused before-read answered %v and reached %v", err, got)
	}
}

// A PERSON IS SHOWN THE REQUEST AS WRITTEN: a field, and the keys inside its
// value (a Measurement Protocol event's own page_title), read on the card,
// the question and the record as Google receives them, though the write is
// classed by lowerCamel names.
//
// Mutation: show a change under its lowerCamel name, or recase the keys
// inside its value, and a person reads clientId and pageTitle where Google
// gets client_id and page_title.
func TestAPersonIsShownTheRequestAsWritten(t *testing.T) {
	t.Parallel()
	op := growth.Operation{ID: "x.collect", Method: "POST", Path: "https://x.example.com/collect", Class: growth.ClassAdmin}
	req, err := googlegrowth.NewRequest(op, nil, nil, map[string]any{"client_id": "1.2",
		"events": []any{map[string]any{"name": "page_view", "params": map[string]any{"page_title": "Home"}}}})
	if err != nil {
		t.Fatal(err)
	}
	c := &googlegrowth.Client{Kind: "x", Name: "X"}
	call := growth.Call{Handle: "seat-a", Tool: "x_admin", HeldCall: "h"}
	h := googlegrowth.NewHeld(req, nil)
	hold := c.Hold(call, h, growth.ClassAdmin)
	card, _ := json.Marshal(c.Card(call, h, h.Binding, growth.ClassAdmin).Acts)
	for _, words := range []string{hold.Context, string(card)} {
		if !strings.Contains(words, "client_id") || !strings.Contains(words, "page_title") ||
			strings.Contains(words, "clientId") || strings.Contains(words, "pageTitle") {
			t.Errorf("a person is shown %s", words)
		}
	}
	if len(hold.Ask.Changes) != 2 || hold.Ask.Changes[0].Field != "client_id" {
		t.Errorf("the ask records %+v", hold.Ask.Changes)
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

// roundTrip is a transport answering every request with what it says.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A FAILURE IS A CLOSED REASON, never Google's words, read off what Google
// answered through the real request path: a read that got no answer is
// unavailable or timed out, and a write that got none may have reached Google,
// so what it did is not known; one whose context ended before its request
// left reached nothing, and timed out.
//
// Mutation: map a write's reset or deadline as a read's, or flatten the
// transport's error, and an applied change is recorded as never made; send a
// request whose context already ended, and it is recorded as unknown.
func TestAFailureIsAClosedReason(t *testing.T) {
	t.Parallel()
	statusOf := func(status int) roundTrip {
		return func(*http.Request) (*http.Response, error) {
			// A WAIT PAST ONE CALL'S, so a read's 429 is handed straight back.
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"},
				"Retry-After": {"3600"}}, Body: http.NoBody}, nil
		}
	}
	reset := roundTrip(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	})
	// THE DEADLINE ENDS DURING THE SEND, as a transport's own does.
	late := roundTrip(func(*http.Request) (*http.Response, error) { return nil, context.DeadlineExceeded })
	for _, tc := range []struct {
		name        string
		rt          roundTrip
		read, write growth.Failure
	}{
		{"401", statusOf(http.StatusUnauthorized), growth.FailureUnauthorized, growth.FailureUnauthorized},
		{"429", statusOf(http.StatusTooManyRequests), growth.FailureRateLimited, growth.FailureRateLimited},
		{"503", statusOf(http.StatusServiceUnavailable), growth.FailureVendorUnavailable, growth.FailureVendorUnavailable},
		{"403", statusOf(http.StatusForbidden), growth.FailureVendorRefused, growth.FailureVendorRefused},
		{"a reset", reset, growth.FailureVendorUnavailable, growth.FailureUnknownOutcome},
		{"a deadline", late, growth.FailureTimedOut, growth.FailureUnknownOutcome},
		{"a deadline before the send", late, growth.FailureTimedOut, growth.FailureTimedOut},
	} {
		w := newWorld(t, tc.rt)
		for _, op := range []growth.Operation{
			{ID: "x.items.get", Method: "GET", Path: "https://x.googleapis.com/v1/items/{itemsId}", Class: growth.ClassRead},
			{ID: "x.items.patch", Method: "PATCH", Path: "https://x.googleapis.com/v1/items/{itemsId}", Class: growth.ClassOperate},
		} {
			req, _ := googlegrowth.NewRequest(op, map[string]string{"itemsId": "1"}, nil, nil)
			deadline := time.Unix(1<<40, 0)
			if tc.name == "a deadline before the send" {
				deadline = time.Unix(0, 0)
			}
			ctx, cancel := context.WithDeadline(t.Context(), deadline)
			_, err := w.client.Send(ctx, "seat-a", req)
			cancel()
			want := tc.read
			if !req.Reads() {
				want = tc.write
			}
			ce, ok := errors.AsType[*googlegrowth.CallError](err)
			if !ok || ce.Failure != want || !want.Valid() {
				t.Errorf("%s on a %s failed as %v, want %q", tc.name, op.Method, err, want)
			}
		}
	}
}

// FREE WORDS GO TO THE ACCESS LOG AS THEIR DIGEST, decided by the field's
// name and never the value's shape: a name, a description, a URI, a user or a
// structured value is Text however it reads; an enum, a number or an ID of a
// field that names no words is kept as it is.
//
// Mutation: decide by the value's shape again, and a display name written as
// one word, or a user's ID, is kept verbatim.
func TestFreeWordsGoToTheLogAsTheirDigest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		field         string
		before, after any
		text          bool
	}{
		{"state", "PAUSED", "ENABLED", false}, {"budgetMicros", nil, float64(1200000), false},
		{"parent", "accounts/1", nil, false}, {"restrictedMetricType", nil, []any{"COST_DATA"}, false},
		{"displayName", "Old name", "New name", true}, {"displayName", "Gross_revenue", "Ignore.previous", true},
		{"defaultUri", nil, "https://example.com/", true}, {"user_id", nil, "8812", true}, {"clientId", nil, "1.2", true},
		{"conditions", nil, []any{map[string]any{"value": "x"}}, true},
		{"notes", nil, "someone@example.com", true}, {"state", nil, "someone@example.com", true},
	} {
		got := googlegrowth.RecordChange(growth.Change{Field: tc.field, Before: tc.before, After: tc.after})
		if got.Text != tc.text {
			t.Errorf("%s %v to %v: Text %v, want %v", tc.field, tc.before, tc.after, got.Text, tc.text)
		}
	}
}

// AN APPROVED CALL IS RECORDED UNDER ITS HELD CALL ALONE, since a record
// names its act once; one whose request could not be built is named by its
// arguments, in an account the log can hold.
func TestAnApprovedCallIsRecordedUnderItsHeldCall(t *testing.T) {
	t.Parallel()
	c := &googlegrowth.Client{Kind: "x"}
	req, _ := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "2"}, nil, nil)
	r := c.Record(growth.Call{ID: "c", HeldCall: "h", Tool: "x_admin"}, req, growth.ClassAdmin)
	if r.Call != "" || r.HeldCall != "h" || r.Before != "" || r.Connection != googlegrowth.Connection {
		t.Errorf("record %+v", r)
	}
	if r := c.Record(growth.Call{ID: "c", Tool: "x_operate"}, req, growth.ClassOperate); r.Call != "c" || r.Before != "" {
		t.Errorf("a call made at once is recorded as %+v", r)
	}
	unbuilt, _ := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1"}, nil, nil)
	r = c.Record(growth.Call{Handle: "seat-a", HeldCall: "h", Tool: "x_admin", Args: map[string]any{"a": "1"}},
		unbuilt, growth.ClassAdmin)
	if err := accessdomain.GrowthOf(growth.StepFailed, withFailure(r)).Validate(); err != nil || len(r.Operation) != 64 {
		t.Errorf("an unbuilt request's record %+v is refused: %v", r, err)
	}
}

func withFailure(r growth.Record) growth.Record {
	r.Failure = growth.FailureEngineError
	return r
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

// A CREDENTIAL A REQUEST CARRIES IS FILLED IN ONLY AS IT IS SENT: Google gets
// it, and neither the digest an approval binds nor any record holds it.
//
// Mutation: fill the credential into the request's query when it is built,
// and the record's digest changes with the secret.
func TestACredentialARequestCarriesIsFilledOnlyAsItIsSent(t *testing.T) {
	t.Parallel()
	const secret = "Zx8kQ2pL4mN7vB1cR5tY9w"
	var sent []string
	w := newWorld(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		sent = append(sent, r.URL.RawQuery)
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
	}))
	op := growth.Operation{ID: "x.collect", Method: "POST", Path: "https://x.example.com/collect", Class: growth.ClassAdmin}
	build := func(value string) googlegrowth.Request {
		req, _ := googlegrowth.NewRequest(op, nil, map[string]string{"secret_name": "s/1"}, map[string]any{"n": 1.0})
		req.Fill = func(_ context.Context, q url.Values) (url.Values, error) {
			q.Del("secret_name")
			q.Set("api_secret", value)
			return q, nil
		}
		return req
	}
	req := build(secret)
	if _, err := w.client.Send(t.Context(), "seat-a", req); err != nil {
		t.Fatal(err)
	}
	r := w.client.Record(growth.Call{Handle: "seat-a", HeldCall: "h", Tool: "x_admin"}, req, growth.ClassAdmin)
	raw, _ := json.Marshal(r)
	if len(sent) != 1 || !strings.Contains(sent[0], "api_secret="+secret) || strings.Contains(sent[0], "secret_name") ||
		strings.Contains(string(raw), secret) || req.Digest(nil) != build("another").Digest(nil) {
		t.Errorf("sent %v, recorded %s", sent, raw)
	}
}

// fakeAudit keeps every step as the access log would: it refuses a record the
// log's own validation refuses, failing the test, and keeps one answer and
// one outcome per act.
type fakeAudit struct {
	t      *testing.T
	mu     sync.Mutex
	steps  []growth.Step
	recs   []growth.Record
	phases map[string]bool
}

func (a *fakeAudit) add(s growth.Step, r growth.Record) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := accessdomain.GrowthOf(s, r).Validate(); err != nil {
		a.t.Errorf("the access log refuses the %s record %+v: %v", s, r, err)
		return false, err
	}
	phase := r.Call + r.HeldCall + "/outcome"
	if s == growth.StepAllowed || s == growth.StepDenied || s == growth.StepLapsed {
		phase = r.Call + r.HeldCall + "/answer"
	}
	if a.phases == nil {
		a.phases = map[string]bool{}
	}
	if a.phases[phase] {
		return false, nil
	}
	a.phases[phase] = true
	a.steps, a.recs = append(a.steps, s), append(a.recs, r)
	return true, nil
}

func (a *fakeAudit) Made(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepMade, r)
}
func (a *fakeAudit) Verdict(_ context.Context, s growth.Step, r growth.Record) (bool, error) {
	return a.add(s, r)
}
func (a *fakeAudit) Applied(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepApplied, r)
}
func (a *fakeAudit) Void(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepVoid, r)
}
func (a *fakeAudit) Failed(_ context.Context, r growth.Record) (bool, error) {
	return a.add(growth.StepFailed, r)
}

func (a *fakeAudit) taken() []growth.Step {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]growth.Step(nil), a.steps...)
}

// leases is one lease per resource, in process; waiting hears each take that
// must wait for another.
type leases struct {
	mu      sync.Mutex
	held    map[string]chan struct{}
	waiting chan string
}

func (l *leases) Hold(ctx context.Context, resource string) (func(), error) {
	for {
		l.mu.Lock()
		if l.held == nil {
			l.held = map[string]chan struct{}{}
		}
		busy, taken := l.held[resource]
		if !taken {
			done := make(chan struct{})
			l.held[resource] = done
			l.mu.Unlock()
			return func() {
				l.mu.Lock()
				delete(l.held, resource)
				l.mu.Unlock()
				close(done)
			}, nil
		}
		l.mu.Unlock()
		if l.waiting != nil {
			l.waiting <- resource
		}
		select {
		case <-busy:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// world is a client reaching Google through rt, as seat-a.
type world struct {
	client *googlegrowth.Client
	audit  *fakeAudit
	leases *leases
}

func newWorld(t *testing.T, rt http.RoundTripper) *world {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	company, err := config.ParseCompanyDocument([]byte(`
name: Acme
integrations:
  google_cloud:
    enabled: true
    provisioning:
      provisioner: crewlet-provisioner@acme-crewlet-id.iam.gserviceaccount.com
      provider: projects/123456789012/locations/global/workloadIdentityPools/crewlet/providers/crewlet-engine
      federation_key: "${GOOGLE_CLOUD_FEDERATION_KEY}"
`))
	if err != nil {
		t.Fatal(err)
	}
	env := config.NewResolver(config.MapSource{
		"GOOGLE_CLOUD_FEDERATION_KEY": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	w := &world{audit: &fakeAudit{t: t}, leases: &leases{}}
	w.client = &googlegrowth.Client{Kind: "x_ads", Name: "X", Company: func() *config.Company { return company },
		Env: func() *config.Resolver { return env }, Audit: w.audit, Leases: w.leases, HTTP: &http.Client{Transport: rt}}
	return w
}

// item is one item at a fake Google whose writes block until release closes,
// and whose reads say when they came.
type item struct {
	mu      sync.Mutex
	name    string
	writes  int
	reads   chan struct{}
	release chan struct{}
	writing chan struct{}
}

func (it *item) serve(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		it.mu.Lock()
		name := it.name
		it.mu.Unlock()
		select {
		case it.reads <- struct{}{}:
		default:
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": name})
	case http.MethodPatch:
		select {
		case it.writing <- struct{}{}:
		default:
		}
		<-it.release
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		it.mu.Lock()
		it.writes++
		it.name, _ = body["name"].(string)
		it.mu.Unlock()
		_ = json.NewEncoder(w).Encode(body)
	}
}

// TWO APPLIES ON ONE TARGET MAKE ONE CALL: the second waits on the account's
// lease, reads the target the first changed, and sends nothing.
//
// Mutation: drop the lease, and both read the old target and both write.
func TestTwoAppliesOnOneTargetMakeOneCall(t *testing.T) {
	t.Parallel()
	it := &item{name: "old", reads: make(chan struct{}, 8), release: make(chan struct{}), writing: make(chan struct{}, 1)}
	srv := httptest.NewServer(http.HandlerFunc(it.serve))
	t.Cleanup(srv.Close)
	w := newWorld(t, httpxtest.Rewrite(t, srv))
	w.leases.waiting = make(chan string, 1)
	req, _ := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "2"},
		map[string]string{"updateMask": "name"}, map[string]any{"name": "new"})
	h, err := w.client.Read(t.Context(), "seat-a", req)
	if err != nil {
		t.Fatal(err)
	}
	<-it.reads
	apply := func(id string, out chan<- error) {
		_, err := w.client.Apply(t.Context(), growth.Call{ID: id, Handle: "seat-a", Tool: "x_ads_operate"},
			googlegrowth.Write{Req: req, Class: growth.ClassOperate}, h.Binding)
		out <- err
	}
	first, second := make(chan error, 1), make(chan error, 1)
	go apply("call-1", first)
	<-it.writing
	<-it.reads // the first apply's own read, which came before its write
	go apply("call-2", second)
	select {
	case <-w.leases.waiting:
	case <-it.reads:
	}
	close(it.release)
	if err := <-first; err != nil {
		t.Fatalf("the first apply answered %v", err)
	}
	if err := <-second; !errors.Is(err, growth.ErrMoved) {
		t.Errorf("the second apply answered %v, want the target moved", err)
	}
	it.mu.Lock()
	writes := it.writes
	it.mu.Unlock()
	if writes != 1 {
		t.Errorf("Google was written %d times", writes)
	}
}

// AN APPLY IS CUT AT growth.ApplyBound: the product's scope check (which may
// read Google), the target's read and the write all run under the lease and
// carry its deadline, so nothing acts once the lease could lapse.
//
// Mutation: drop the bound, or check the scope before the lease, and a read
// carries the caller's context alone or runs unleased.
func TestAnApplyIsCutAtItsBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var deadlines []time.Time
		w := newWorld(t, roundTrip(func(r *http.Request) (*http.Response, error) {
			d, _ := r.Context().Deadline()
			deadlines = append(deadlines, d)
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"name":"old"}`))}, nil
		}))
		req, _ := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "2"},
			map[string]string{"updateMask": "name"}, map[string]any{"name": "new"})
		start := time.Now()
		h := googlegrowth.NewHeld(req, []byte(`{"name":"old"}`))
		leased := false
		scope := func(ctx context.Context) error {
			d, _ := ctx.Deadline()
			deadlines = append(deadlines, d)
			w.leases.mu.Lock()
			_, leased = w.leases.held["write/x_ads/accounts/1/items/2"]
			w.leases.mu.Unlock()
			return nil
		}
		if _, err := w.client.Apply(t.Context(), growth.Call{ID: "c", Handle: "seat-a", Tool: "x_ads_operate"},
			googlegrowth.Write{Req: req, Class: growth.ClassOperate, Scope: scope}, h.Binding); err != nil {
			t.Fatal(err)
		}
		if len(deadlines) != 3 || !leased {
			t.Fatalf("the scope check, the read and the write carried deadlines %v (scope under the lease: %v)",
				deadlines, leased)
		}
		for _, d := range deadlines {
			if !d.Equal(start.Add(growth.ApplyBound)) {
				t.Errorf("deadlines %v, want each %s from the start", deadlines, growth.ApplyBound)
			}
		}
	})
}

// A HELD CALL'S ANSWER IS RECORDED BEFORE IT ACTS, and it acts only on the
// act's one answer: an allowed call records allowed then its outcome, one
// whose answer is already taken (it lapsed) sends nothing, one that cannot be
// built again records its failure, and a denial or a lapse is its answer.
//
// Mutation: act before the answer lands, or skip the failure of an unbuilt
// request, and the log holds an outcome without its answer, or none at all.
func TestAHeldCallsAnswerIsRecordedBeforeItActs(t *testing.T) {
	t.Parallel()
	it := &item{name: "old", reads: make(chan struct{}, 8), release: make(chan struct{}), writing: make(chan struct{}, 1)}
	close(it.release)
	srv := httptest.NewServer(http.HandlerFunc(it.serve))
	t.Cleanup(srv.Close)
	w := newWorld(t, httpxtest.Rewrite(t, srv))
	req, _ := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "2"},
		map[string]string{"updateMask": "name"}, map[string]any{"name": "new"})
	h, _ := w.client.Read(t.Context(), "seat-a", req)
	call := growth.Call{Handle: "seat-a", Tool: "x_ads_admin", HeldCall: "held-1", Approver: "founder"}
	admin := func(req googlegrowth.Request, err error) googlegrowth.Write {
		return googlegrowth.Write{Req: req, Err: err, Class: growth.ClassAdmin}
	}
	if _, err := w.client.Apply(t.Context(), call, admin(req, nil), h.Binding); err != nil {
		t.Fatal(err)
	}
	lapsed := growth.Call{Handle: "seat-a", Tool: "x_ads_admin", HeldCall: "held-2", Approver: growth.EngineActor}
	w.client.Denied(t.Context(), lapsed, req, growth.ClassAdmin)
	lapsed.Approver = "founder"
	if _, err := w.client.Apply(t.Context(), lapsed, admin(req, nil), h.Binding); err == nil {
		t.Error("an allow after the lapse was applied")
	}
	unbuilt, err := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1"}, nil, nil)
	call3 := growth.Call{Handle: "seat-a", Tool: "x_ads_admin", HeldCall: "held-3", Approver: "founder"}
	if _, aerr := w.client.Apply(t.Context(), call3, admin(unbuilt, err), h.Binding); aerr == nil {
		t.Error("an unbuilt request was applied")
	}
	w.client.Denied(t.Context(), growth.Call{Handle: "seat-a", Tool: "x_ads_admin", HeldCall: "held-4",
		Approver: "founder"}, req, growth.ClassAdmin)
	want := []growth.Step{growth.StepAllowed, growth.StepApplied, growth.StepLapsed, growth.StepAllowed,
		growth.StepFailed, growth.StepDenied}
	if got := w.audit.taken(); strings.Join(steps(got), ",") != strings.Join(steps(want), ",") {
		t.Errorf("recorded %v, want %v", got, want)
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.writes != 1 {
		t.Errorf("Google was written %d times", it.writes)
	}
}

// A PRODUCT'S LAST CHECK RUNS UNDER THE LEASE AND SENDS NOTHING IT REFUSES:
// once the target read again holds the binding, a refusal (a spend over the
// caps read now) ends the allowed act failed with the check's reason.
//
// Mutation: skip the check, or run it before the lease, and the write is sent
// or the check reads what another apply is about to change.
func TestAProductsLastCheckRunsUnderTheLease(t *testing.T) {
	t.Parallel()
	it := &item{name: "old", reads: make(chan struct{}, 8), release: make(chan struct{}), writing: make(chan struct{}, 1)}
	close(it.release)
	srv := httptest.NewServer(http.HandlerFunc(it.serve))
	t.Cleanup(srv.Close)
	w := newWorld(t, httpxtest.Rewrite(t, srv))
	req, _ := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "2"},
		map[string]string{"updateMask": "name"}, map[string]any{"name": "new"})
	req.Account = "accounts/1"
	h, _ := w.client.Read(t.Context(), "seat-a", req)
	call := growth.Call{Handle: "seat-a", Tool: "x_ads_spend", HeldCall: "held-1", Approver: "founder"}
	leased := false
	_, err := w.client.Apply(t.Context(), call, googlegrowth.Write{Req: req, Class: growth.ClassSpend,
		Check: func(ctx context.Context, read googlegrowth.Held) error {
			w.leases.mu.Lock()
			_, held := w.leases.held["write/x_ads/accounts/1"]
			w.leases.mu.Unlock()
			_, bounded := ctx.Deadline()
			leased = held && bounded && read.Binding == h.Binding
			return &googlegrowth.CallError{Failure: growth.FailureCapExceeded, Err: errors.New("over the daily cap")}
		}}, h.Binding)
	if err == nil || !leased || !errors.Is(err, growth.ErrNotSent) {
		t.Fatalf("a refused check answered %v (checked under the lease: %v), want it not sent", err, leased)
	}
	got := w.audit.taken()
	w.audit.mu.Lock()
	last := w.audit.recs[len(w.audit.recs)-1]
	w.audit.mu.Unlock()
	if strings.Join(steps(got), ",") != "allowed,failed" || last.Failure != growth.FailureCapExceeded {
		t.Errorf("recorded %v with %q, want allowed then failed over the caps", got, last.Failure)
	}
	it.mu.Lock()
	defer it.mu.Unlock()
	if it.writes != 0 {
		t.Errorf("Google was written %d times", it.writes)
	}
}

// AN ACT IS NOT SENT UNTIL ITS REQUEST LEAVES, and sent once it has: a check
// that failed before it, or a context that ended before the request was made,
// answers the act not sent; a write Google took and whose answer was cut off
// is not, since what it did is unknown.
//
// Mutation: drop failEarly's mark or the unsent mark on a context ended before
// Do, or mark every failure, and this fails.
func TestAnActIsNotSentUntilItsRequestLeaves(t *testing.T) {
	t.Parallel()
	it := &item{name: "old", reads: make(chan struct{}, 8), release: make(chan struct{}), writing: make(chan struct{}, 1)}
	close(it.release)
	srv := httptest.NewServer(http.HandlerFunc(it.serve))
	t.Cleanup(srv.Close)
	w := newWorld(t, httpxtest.Rewrite(t, srv))
	req, _ := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "2"},
		map[string]string{"updateMask": "name"}, map[string]any{"name": "new"})
	req.Account = "accounts/1"
	h, _ := w.client.Read(t.Context(), "seat-a", req)
	call := growth.Call{Handle: "seat-a", Tool: "x_ads_spend", HeldCall: "held-1", Approver: "founder"}
	unread := &googlegrowth.CallError{Failure: growth.FailureTimedOut, Err: fmt.Errorf("re-read: %w",
		context.DeadlineExceeded)}
	_, err := w.client.Apply(t.Context(), call, googlegrowth.Write{Req: req, Class: growth.ClassSpend,
		Check: func(context.Context, googlegrowth.Held) error { return unread }}, h.Binding)
	if !errors.Is(err, growth.ErrNotSent) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a check that timed out before the write = %v, want not sent", err)
	}
	ended, end := context.WithCancel(t.Context())
	call.HeldCall = "held-2"
	_, err = w.client.Apply(ended, call, googlegrowth.Write{Req: req, Class: growth.ClassSpend,
		Check: func(context.Context, googlegrowth.Held) error { end(); return nil }}, h.Binding)
	if !errors.Is(err, growth.ErrNotSent) || !errors.Is(err, context.Canceled) {
		t.Errorf("a context ended before the write's request = %v, want not sent", err)
	}
	it.mu.Lock()
	writes := it.writes
	it.mu.Unlock()
	if writes != 0 {
		t.Fatalf("Google was written %d times before any write was sent", writes)
	}
	cut := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			it.serve(rw, r)
			return
		}
		conn, _, _ := http.NewResponseController(rw).Hijack()
		_ = conn.Close()
	}))
	t.Cleanup(cut.Close)
	w = newWorld(t, httpxtest.Rewrite(t, cut))
	call.HeldCall = "held-3"
	_, err = w.client.Apply(t.Context(), call, googlegrowth.Write{Req: req, Class: growth.ClassSpend}, h.Binding)
	if err == nil || errors.Is(err, growth.ErrNotSent) {
		t.Errorf("a write whose answer was cut off = %v, want a failure never told as not sent", err)
	}
}

func steps(s []growth.Step) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = string(x)
	}
	return out
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

// TWO ASKS ARE ONE ONLY WHERE THEY DECIDE THE SAME THING: one operation on one
// address in two accounts (a site its body names) are two subjects, so the
// second is never joined to the first's waiting ask; the same request twice is
// one.
//
// Mutation: leave the account out of the subject, and verifying a second site
// reads as a repeat of the first.
func TestTwoAsksAreOneOnlyWhereTheyDecideTheSameThing(t *testing.T) {
	t.Parallel()
	insert := growth.Operation{ID: "x.resources.insert", Method: "POST", Resource: "resources",
		Class: growth.ClassAdmin, Path: "https://x.googleapis.com/v1/resources"}
	a, err := googlegrowth.NewRequest(insert, nil, nil, map[string]any{"site": "a"})
	if err != nil {
		t.Fatal(err)
	}
	b := a
	a.Account, b.Account = "sc-domain:example.com", "sc-domain:example.org"
	if a.Subject("x") == b.Subject("x") {
		t.Error("one operation in two accounts is one subject")
	}
	if again := a; again.Subject("x") != a.Subject("x") {
		t.Error("the same request is two subjects")
	}
}

// A REQUEST SHAPED BY ITS TARGET IS MADE FROM THE STATE ITS APPROVAL BINDS:
// what it sends is built from the target as read under the lease, and a state
// the change no longer applies to is a target that moved, void and unsent.
//
// Mutation: send the request as built, or fail a change the target no longer
// takes, and the body overwrites what was read or the log says the engine
// erred.
func TestARequestShapedByItsTargetIsMadeFromTheStateItBinds(t *testing.T) {
	t.Parallel()
	it := &item{name: "old", reads: make(chan struct{}, 8), release: make(chan struct{}), writing: make(chan struct{}, 1)}
	close(it.release)
	srv := httptest.NewServer(http.HandlerFunc(it.serve))
	t.Cleanup(srv.Close)
	w := newWorld(t, httpxtest.Rewrite(t, srv))
	build := func() googlegrowth.Request {
		req, err := googlegrowth.NewRequest(patch, map[string]string{"accountsId": "1", "itemsId": "2"},
			map[string]string{"updateMask": "name"}, map[string]any{"suffix": "!"})
		if err != nil {
			t.Fatal(err)
		}
		req.Shape = func(before []byte) (googlegrowth.Request, error) {
			var current map[string]any
			_ = json.Unmarshal(before, &current)
			name, _ := current["name"].(string)
			if strings.HasSuffix(name, "!") {
				return googlegrowth.Request{}, growth.Refuse(growth.RefusedInvalid, "the name is marked already")
			}
			shaped := req
			shaped.Body = map[string]any{"name": name + "!"}
			return shaped, nil
		}
		return req
	}
	h, err := w.client.Read(t.Context(), "seat-a", build())
	if err != nil || googlegrowth.Words(h.Req.Body["name"]) != "old!" {
		t.Fatalf("the shaped request is %v, %v", h.Req.Body, err)
	}
	call := growth.Call{Handle: "seat-a", Tool: "x_ads_admin", HeldCall: "held-1", Approver: "founder"}
	if _, err := w.client.Apply(t.Context(), call, googlegrowth.Write{Req: build(), Class: growth.ClassAdmin},
		h.Binding); err != nil {
		t.Fatal(err)
	}
	it.mu.Lock()
	name := it.name
	it.mu.Unlock()
	if name != "old!" {
		t.Fatalf("Google holds %q, want the name made from the state read", name)
	}
	again, err := w.client.Read(t.Context(), "seat-a", build())
	if err == nil || again.Req.Body["suffix"] == nil {
		t.Fatalf("a state the change does not apply to was shaped into %v", again.Req.Body)
	}
	call.HeldCall = "held-2"
	if _, err := w.client.Apply(t.Context(), call, googlegrowth.Write{Req: build(), Class: growth.ClassAdmin},
		h.Binding); !errors.Is(err, growth.ErrMoved) {
		t.Errorf("an approval on a state the change does not apply to answered %v", err)
	}
	got := strings.Join(steps(w.audit.taken()), ",")
	it.mu.Lock()
	writes := it.writes
	it.mu.Unlock()
	if got != "allowed,applied,allowed,void" || writes != 1 {
		t.Errorf("recorded %s after %d writes, want the second approval void and unsent", got, writes)
	}
}
