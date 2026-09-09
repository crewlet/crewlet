package datadog_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
)

// sink records what a pass mints, and can refuse a read.
type sink struct {
	held      map[string]string
	readErr   error
	recordErr error
	flushed   bool
}

func newSink() *sink { return &sink{held: map[string]string{}} }

func (s *sink) Record(_ context.Context, name, value string) error {
	if s.recordErr != nil {
		return s.recordErr
	}
	s.held[name] = value
	return nil
}

func (s *sink) Value(_ context.Context, name string) (string, bool, error) {
	if s.readErr != nil {
		return "", false, s.readErr
	}
	v, ok := s.held[name]
	return v, ok, nil
}

func (s *sink) Flush(context.Context) error { s.flushed = true; return nil }

func (s *sink) Describe() string { return "a test sink" }
func (s *sink) NextStep() string { return "a test next step" }

func (s *sink) Discard(context.Context) error {
	clear(s.held)
	return nil
}

func cfgWith() *config.Datadog {
	return &config.Datadog{
		Provisioning: &config.DatadogProvisioning{
			Site: "datadoghq.com", EmailDomain: "agents.test.invalid",
		},
	}
}

func planWith(handles ...string) *provision.Plan {
	p := &provision.Plan{}
	for _, h := range handles {
		p.Add(provision.Seat{
			Handle: h, Role: strings.ToUpper(h),
			TokenVar: strings.ToUpper(h) + "_DD_KEY",
			Email:    "crewlet-" + h + "@agents.test.invalid",
		})
	}
	return p
}

// orgOK is the verify call every pass starts with.
func orgOK(reg *region) {
	reg.handle["/api/v1/org"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"orgs":[{"name":"Acme","public_id":"p1"}]}`))
	}
	reg.handle["/api/v2/roles"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"role-1","attributes":{"name":"Datadog Read Only Role"}}]}`))
	}
}

// A CHECK CREATES NOTHING. It is the same pass with no sink, and it exists to
// answer "is this working now" without writing at the vendor.
func TestAReadOnlyPassCreatesNoAccount(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}
	created := false
	reg.handle["/api/v2/service_accounts"] = func(w http.ResponseWriter, _ *http.Request) {
		created = true
		w.WriteHeader(http.StatusInternalServerError)
	}

	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, // no Sink
	})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if created {
		t.Error("a read-only pass created an account at the vendor")
	}
	// And it reports the seat as missing one, which is the fact it exists
	// to surface.
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Subject != "sre" {
		t.Fatalf("findings = %+v, want the seat reported without an account", findings)
	}
}

// A KEY IS MINTED ONCE. Datadog returns a key's value exactly once, so
// minting on every pass leaves a trail of keys nobody holds, and rotating one
// revokes what the agent is currently authenticating with.
func TestAKeyIsNotMintedForASeatThatAlreadyHasOne(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true}}]}`))
	}
	minted := 0
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(w http.ResponseWriter, _ *http.Request) {
		minted++
		_, _ = w.Write([]byte(`{"data":{"id":"k1","attributes":{"key":"v","name":"crewlet"}}}`))
	}

	s := newSink()
	s.held["SRE_DD_KEY"] = "already-held"
	if _, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	}); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if minted != 0 {
		t.Errorf("minted %d keys for a seat that already held one", minted)
	}
}

// UNKNOWN IS NOT "NO KEY". A sink that could not be read is not a seat
// without one, and minting on unknown revokes what the agent is
// authenticating with.
func TestAnUnreadableSinkStopsTheSeatRatherThanMinting(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true}}]}`))
	}
	minted := 0
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(w http.ResponseWriter, _ *http.Request) {
		minted++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":{"id":"k1","attributes":{"key":"v"}}}`))
	}

	s := newSink()
	s.readErr = context.DeadlineExceeded
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if minted != 0 {
		t.Fatal("a key was minted on an unreadable sink, revoking whatever the seat held")
	}
	if len(res.Seats) != 1 || res.Seats[0].Err == nil {
		t.Fatalf("seat = %+v, want the failure reported", res.Seats)
	}
}

// A ROLE THE ORGANIZATION DOES NOT HAVE IS REFUSED, not defaulted. Creating
// accounts under whatever role happened to match would grant an agent access
// nobody asked for.
func TestAnUnknownRoleIsRefusedRatherThanGuessed(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/roles"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"other","attributes":{"name":"Datadog Admin Role"}}]}`))
	}
	cfg := cfgWith()
	cfg.Provisioning.Role = "Crewlet Agents"

	_, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfg, Plan: planWith("sre"),
		Creds: pair, Sink: newSink(),
	})
	if err == nil || !strings.Contains(err.Error(), "Crewlet Agents") {
		t.Fatalf("err = %v, want a refusal naming the role", err)
	}
}

// A REFUSED ORGANIZATION CREDENTIAL IS A REJECTION, so the surface reads
// Failed and points at the operator rather than sitting in a retry.
func TestRefusedOrgCredentialsAreReportedAsARejection(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v1/org"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["Forbidden"]}`))
	}
	_, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: newSink(),
	})
	if err == nil {
		t.Fatal("a refused credential was not reported")
	}
	if !strings.Contains(err.Error(), "was refused") {
		t.Errorf("err = %v", err)
	}
	if !errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a 403 was not classified as a rejection: %v", err)
	}
}

// AND ANYTHING ELSE IS NOT A REFUSAL. A 404, a 500 or a timeout says nothing
// about the key, and reporting one as refused sends an operator to rotate a
// credential that works. This is not hypothetical: the verify call named a
// route Datadog does not serve, and every pass reported the keys rejected.
func TestANonRefusalDoesNotAccuseTheCredential(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	reg.handle["/api/v1/org"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"errors":["upstream"]}`))
	}
	_, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: newSink(),
	})
	if err == nil {
		t.Fatal("a 500 was not reported at all")
	}
	if !strings.Contains(err.Error(), "could not be verified") {
		t.Errorf("err = %v, want it to stop short of claiming a refusal", err)
	}
	if errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a 500 was classified as a credential rejection: %v", err)
	}
}

// A KEY THAT COULD NOT BE RECORDED IS REVOKED.
//
// Datadog shows a key's value exactly once, so one this engine minted and
// then could not persist is a credential that exists, is held by nobody, and
// which nothing will ever remember to remove — the state
// provision.TokenSink's contract legislates against. It also wedged the seat
// for good: the next pass found a key it could not read a value for and
// reported the seat permanently stuck, a state this engine created and could
// not leave without somebody logging into Datadog.
func TestAKeyThatCannotBeRecordedIsRevoked(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true}}]}`))
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"k1","attributes":{"key":"v","name":"crewlet"}}}`))
	}
	revoked := 0
	reg.handle["/api/v2/service_accounts/u1/application_keys/k1"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method == http.MethodDelete {
			revoked++
		}
		w.WriteHeader(http.StatusNoContent)
	}

	s := newSink()
	s.recordErr = errors.New("the keyring refused the write")
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if revoked != 1 {
		t.Fatalf("the key was revoked %d times; it is live at Datadog and held "+
			"by nobody", revoked)
	}
	if len(res.Seats) != 1 || res.Seats[0].Err == nil {
		t.Fatalf("seat = %+v, want the failure reported", res.Seats)
	}
	// AND THE ORIGINAL FAILURE SURVIVES. The reason the run stopped is
	// what an operator has to fix; a cleanup message replacing it would
	// hide the cause behind its consequence.
	if !strings.Contains(res.Seats[0].Err.Error(), "the keyring refused the write") {
		t.Errorf("the revocation replaced the cause: %v", res.Seats[0].Err)
	}
}

// AND WHEN THE REVOCATION ALSO FAILS, somebody is told to do it by hand —
// which is the only case where that instruction is honest.
func TestAKeyThatCannotBeRevokedIsReportedForARealPerson(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true}}]}`))
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"k1","attributes":{"key":"v","name":"crewlet"}}}`))
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys/k1"] = func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusInternalServerError)
	}

	s := newSink()
	s.recordErr = errors.New("the keyring refused the write")
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(res.Seats) != 1 || res.Seats[0].Err == nil {
		t.Fatalf("seat = %+v", res.Seats)
	}
	if !strings.Contains(res.Seats[0].Err.Error(), "delete the key named") {
		t.Errorf("nobody was told the key is live: %v", res.Seats[0].Err)
	}
}
