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

// sink records what a pass mints, and can refuse a read or a write.
type sink struct {
	// forgotten is what a teardown asked this sink to delete.
	forgotten []string

	held    map[string]string
	readErr error
	// recordErr refuses every write; recordErrFor refuses ONE name, so a
	// test can have a pass mint for one seat and fail on a later one.
	recordErr    error
	recordErrFor map[string]error
	flushed      bool
	// flushedHeld is what this sink was holding WHEN Flush was called,
	// which is the only way to tell a pass that flushed what it sealed
	// from one that flushed before it sealed anything.
	flushedHeld map[string]string
}

func newSink() *sink {
	return &sink{held: map[string]string{}, recordErrFor: map[string]error{}}
}

func (s *sink) Record(_ context.Context, name, value string) error {
	if err := s.recordErrFor[name]; err != nil {
		return err
	}
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

func (s *sink) Flush(context.Context) error {
	s.flushed = true
	s.flushedHeld = map[string]string{}
	for name, value := range s.held {
		s.flushedHeld[name] = value
	}
	return nil
}

func (s *sink) Describe() string { return "a test sink" }
func (s *sink) NextStep() string { return "a test next step" }

// Forget implements [provision.TokenSink]: it records what a teardown
// asked to be deleted, so a case can assert the deletion happened.
func (s *sink) Forget(_ context.Context, names ...string) error {
	s.forgotten = append(s.forgotten, names...)
	return nil
}

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
	// to surface. Beside the webhook this pass registered no address for
	// — these Options carry no WebhookBase — because a check that leaves
	// out the half of the integration that carries the alerts is a check
	// somebody would read as an all-clear.
	findings := res.Findings()
	var seat integration.Finding
	for _, f := range findings {
		if f.Subject == "sre" {
			seat = f
		}
	}
	if seat.Kind != integration.FindingIdentityMissing {
		t.Fatalf("findings = %+v, want the seat reported without an account",
			findings)
	}
}

// A DISABLED SERVICE ACCOUNT IS NOT A PROVISIONED SEAT.
//
// Disabling one is exactly how a disconnect that removes seats decommissions
// it, and a disabled account stays in the listing — so a pass that read
// "present in the listing" as "has an identity" reported nothing at all for a
// seat whose every Datadog call is refused, and no later pass ever created a
// replacement. The surface sat Ready over an agent that could not
// authenticate, for the life of the deployment.
func TestADisabledAccountIsReportedRatherThanReadAsProvisioned(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true,
			"disabled":true}}]}`))
	}
	created := 0
	reg.handle["/api/v2/service_accounts"] = func(w http.ResponseWriter, _ *http.Request) {
		created++
		w.WriteHeader(http.StatusInternalServerError)
	}

	s := newSink()
	s.held["SRE_DD_KEY"] = "already-held"
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	// AND NO SECOND ACCOUNT ON TOP OF IT. The address is Datadog's own
	// key for the identity, so creating one here would be refused every
	// pass for ever.
	if created != 0 {
		t.Errorf("a second account was created over a disabled one %d times", created)
	}
	if len(res.Seats) != 1 || res.Seats[0].Err == nil {
		t.Fatalf("seat = %+v, want the disabled account reported", res.Seats)
	}
	if !strings.Contains(res.Seats[0].Err.Error(), "disabled") {
		t.Errorf("the report does not say the account is disabled: %v", res.Seats[0].Err)
	}
	findings := res.Findings()
	if len(findings) == 0 || findings[len(findings)-1].Kind != integration.FindingIdentityFailed {
		t.Fatalf("findings = %+v, want identity_failed for the seat", findings)
	}
}

// A CANCELLED PASS RAISES RATHER THAN REPORTING A CONVERGED COMPANY.
//
// The two are opposite claims to the reconcile loop: an error is a fault it
// retries, and an empty findings list is a statement that everything is fine.
// A node shutting down would otherwise record Datadog ready on its way out,
// and the next node to hold the duty trusts that for a full settled interval.
// [integrationtest] drives the same clause; this pins what makes it true,
// which that clause cannot see. The pass consults the context ITSELF, before
// anything else it could answer from — rather than relying on the credential
// probe below to fail because a transport happened to honour cancellation.
func TestACancelledPassRaisesRatherThanReportingHealth(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := datadog.Reconcile(ctx, hookOptions(t, reg, base))
	if err == nil && len(res.Findings()) == 0 {
		t.Fatal("a cancelled pass reported a converged integration")
	}
	if err == nil {
		t.Fatalf("a cancelled pass answered with findings rather than a fault: %+v",
			res.Findings())
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation itself", err)
	}
	// AND IT ASKED DATADOG NOTHING. Not an efficiency: a pass that reaches
	// the network before it reads the context is one whose answer depends
	// on the transport it was built with, and the arm ABOVE that probe
	// returns integration.ErrNotConfigured — which the loop reads as
	// "forget this surface's status row", so a node draining against a
	// company mid-edit would delete the fleet's Datadog status on its way
	// out.
	if len(reg.calls) != 0 {
		t.Errorf("a cancelled pass made %v; it observed nothing and must say so "+
			"without asking", reg.calls)
	}
	// AND IT DOES NOT ACCUSE THE OPERATOR'S KEY. A cancellation says
	// nothing about the credential, and reporting one as refused — or even
	// as unverifiable — sends somebody to rotate a pair that works.
	if errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("a cancelled pass was classified as a credential rejection: %v", err)
	}
	if strings.Contains(err.Error(), "integrations.datadog.provisioning") {
		t.Errorf("a cancelled pass sent somebody to look at the organization "+
			"credential pair, which it never asked about: %v", err)
	}
}

// EVERYTHING A PASS SEALED IS FLUSHED, INCLUDING WHEN A LATER SEAT FAILED.
//
// [provision.TokenSink] hands nothing to the fleet until Flush, so a key
// minted at Datadog and sealed but never flushed is exactly the state that
// contract legislates against: it exists, nobody holds it, and the next pass
// finds a seat whose ${VAR} is empty and mints another — one orphaned
// application key per tick, for ever, none of them recoverable because
// Datadog shows a value once.
//
// The shape that produces it is a mid-pass `return res, err` placed after the
// first mint, which is why the plan loop carries a failing seat in its own
// SeatResult instead of returning. Nothing else in this package's tests would
// notice if that changed, or if the Flush call were simply dropped: every
// other assertion reads the sink's held map, which a Record has already
// written.
func TestEverySealedKeyIsFlushedEvenWhenALaterSeatFails(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"u1","attributes":{
				"email":"crewlet-sre@agents.test.invalid","service_account":true}},
			{"id":"u2","attributes":{
				"email":"crewlet-dba@agents.test.invalid","service_account":true}}]}`))
	}
	mint := func(account, key string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"data":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"id":"` + key + `","attributes":{
				"key":"value-for-` + account + `","name":"crewlet"}}}`))
		}
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = mint("u1", "k1")
	reg.handle["/api/v2/service_accounts/u2/application_keys"] = mint("u2", "k2")
	reg.handle["/api/v2/service_accounts/u2/application_keys/k2"] = func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}

	s := newSink()
	s.recordErrFor["DBA_DD_KEY"] = errors.New("the keyring refused the write")
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre", "dba"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	// The plan orders seats by handle, so `dba` is attempted FIRST and
	// fails, and `sre` mints after it — which is the ordering that matters
	// here: what has to survive is the seal made after the failure.
	byHandle := map[string]datadog.SeatResult{}
	for _, seat := range res.Seats {
		byHandle[seat.Handle] = seat
	}
	if len(res.Seats) != 2 || byHandle["dba"].Err == nil || byHandle["sre"].Err != nil {
		t.Fatalf("seats = %+v, want dba failed and sre provisioned", res.Seats)
	}
	if !s.flushed {
		t.Fatal("a pass that minted never flushed: the key is live at Datadog " +
			"and the fleet holds nothing for it")
	}
	if s.flushedHeld["SRE_DD_KEY"] != "value-for-u1" {
		t.Errorf("the flush carried %v, want the key minted after the seat that "+
			"failed — a pass that returned on that failure, or flushed before "+
			"it ran the plan, hands the fleet nothing", s.flushedHeld)
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
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		// THE LIST AND THE MINT ARE THE SAME PATH, AND DIFFERENT ANSWERS.
		// The pass asks what the account holds before it decides, so a
		// fake that answered the mint's body to a GET would report a
		// decode failure — and the run would skip the mint for the wrong
		// reason, which is a passing test proving nothing.
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"data":[{"id":"k1","attributes":{"name":"crewlet"}}]}`))
			return
		}
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

// A NODE WITH NO KEYRING CREATES NO ACCOUNT AT DATADOG.
//
// [provision.CanMint] exists for exactly this pass: it makes an ACCOUNT and
// then mints a key on it, and the rollback can only revoke the key — so
// reaching the first Record on a sink that cannot record leaves a service
// account at Datadog that nobody asked for and that nothing can ever
// authenticate as. The loop hands provision.ReadOnly() to a node with no
// keyring, and this pass took it for an ordinary sink: it created every
// seat's account, then reported each one as "could not read whether <VAR>
// already holds a key" on every tick, and not one of those sentences named
// the bootstrap field that fixes it.
func TestANodeWithNoKeyringCreatesNoAccount(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}
	created := 0
	reg.handle["/api/v2/service_accounts"] = func(w http.ResponseWriter, _ *http.Request) {
		created++
		_, _ = w.Write([]byte(`{"data":{"id":"u1","attributes":{"email":"a@b","name":"SRE"}}}`))
	}

	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre", "oncall"),
		Creds: pair, Sink: provision.ReadOnly(),
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if created != 0 {
		t.Errorf("a node that cannot seal a credential created %d service "+
			"account(s) nothing will ever authenticate as", created)
	}
	// ONE FINDING NAMING THE BOOTSTRAP FIELD, and NOT one per seat naming
	// a variable none of them can do anything about. (The webhook is
	// reported too — these Options carry no public base — which is a
	// different fact about a different half.)
	var keyring, perSeat int
	for _, f := range res.Findings() {
		switch f.Subject {
		case "secrets.keys":
			keyring++
			if f.Kind != integration.FindingCredentialMissing {
				t.Errorf("kind = %q, want credential_missing", f.Kind)
			}
		case "sre", "oncall":
			perSeat++
		}
	}
	if keyring != 1 {
		t.Errorf("findings = %+v, want one naming secrets.keys", res.Findings())
	}
	if perSeat != 0 {
		t.Errorf("%d seat(s) were reported individually for one bootstrap "+
			"field nothing about a seat can fix", perSeat)
	}
}

// AND THE INBOUND HALF STILL RUNS ON THAT NODE.
//
// This is the difference from every other pass that checks CanMint. The
// webhook definition is written with the organization credentials the company
// document already carries, so it needs no keyring at all — and it is the
// half that carries the alerts. A check placed before it would take a
// company's whole monitoring integration down over a bootstrap field that has
// nothing to do with it.
func TestANodeWithNoKeyringStillRegistersTheWebhook(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	opts := hookOptions(t, reg, base)
	opts.Plan = planWith("sre")
	opts.Sink = provision.ReadOnly()
	if _, err := datadog.Reconcile(context.Background(), opts); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if held.hook == nil {
		t.Fatal("a node with no keyring left this company's alerts unrouted")
	}
	if got, want := held.hook.URL, base+"/webhooks/datadog"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
}

// CONNECTING RE-ENABLES AN ACCOUNT THIS ENGINE'S OWN DISCONNECT DISABLED.
//
// Re-enabling used to be refused outright, on the reasoning that undoing a
// decommission is somebody's decision and a pass that reversed it would fight
// that gesture on every tick. That holds for an account a PERSON disabled and
// not for one this engine turned off — and with nothing recording which was
// which, both were the same ambiguous bit and both were refused.
//
// So a disconnect-then-reconnect cycle could not complete: the pass found the
// account it had itself disabled, said "re-enable it at Datadog: this pass will
// not", and the operator either did it by hand or left another dead account
// behind. About thirty-seven accumulated in one deployment.
func TestConnectingReEnablesAnAccountThisEngineDisabled(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	enabled := 0
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true,
			"disabled":true,"title":"crewlet:disconnected"}}]}`))
	}
	reg.handle["/api/v2/users/u1"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		enabled++
		_, _ = w.Write([]byte(`{"data":{"id":"u1"}}`))
	}

	s := newSink()
	s.held["SRE_DD_KEY"] = "already-held"
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if enabled != 1 {
		t.Errorf("the account was re-enabled %d times, want once", enabled)
	}
	if len(res.Seats) != 1 || res.Seats[0].Err != nil {
		t.Fatalf("seat = %+v, want it reported as recovered rather than failed", res.Seats)
	}
	if !res.Seats[0].Enabled {
		t.Error("the pass did not report turning the account back on")
	}
}

// AND AN ACCOUNT SOMEBODY ELSE DISABLED IS STILL LEFT ALONE.
//
// That was a deliberate act at Datadog. A pass that reversed it would fight
// the operator's own gesture on every tick, which is why the refusal exists
// and why it survives — what changed is that the two cases are now different
// facts rather than one bit.
func TestAnAccountDisabledByHandIsStillReportedRatherThanReEnabled(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		// NO MARKER: this engine did not disable it.
		_, _ = w.Write([]byte(`{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true,
			"disabled":true}}]}`))
	}
	reg.handle["/api/v2/users/u1"] = func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an account this engine did not disable was re-enabled")
		w.WriteHeader(http.StatusInternalServerError)
	}

	s := newSink()
	s.held["SRE_DD_KEY"] = "already-held"
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if len(res.Seats) != 1 || res.Seats[0].Err == nil {
		t.Fatalf("seat = %+v, want the disabled account reported", res.Seats)
	}
	if !strings.Contains(res.Seats[0].Err.Error(), "not disabled by this engine") {
		t.Errorf("the report does not say who disabled it: %v", res.Seats[0].Err)
	}
}

// A STORED VALUE IS NOT A WORKING CREDENTIAL, AND THE ACCOUNT IS WHAT SAYS SO.
//
// "A value is held" used to end the seat's work, and it is a different fact
// from "the agent can authenticate". A teardown that KEEPS the account still
// revokes the key this engine minted, and the sealed value survives that by
// design — so a reconnect found a value, minted nothing, and reported the seat
// ready over a credential Datadog answers 403 for. Measured over five cycles
// against a real organization: Connected in five seconds, seat satisfied, zero
// findings, nought application keys on the account. An administrator deleting
// the key by hand left the same state, for ever.
func TestASeatWhoseAccountHoldsNoKeyIsMintedOver(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true}}]}`))
	}
	minted := 0
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method == http.MethodGet {
			// NOUGHT KEYS, which is the state a revoked or hand-deleted
			// key leaves and the one thing that proves the sealed value
			// cannot be one of the account's.
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		minted++
		_, _ = w.Write([]byte(`{"data":{"id":"k9","attributes":{"key":"fresh","name":"crewlet"}}}`))
	}

	s := newSink()
	s.held["SRE_DD_KEY"] = "dead-value-from-an-earlier-cycle"
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if minted != 1 {
		t.Fatalf("minted %d keys, want exactly one over the dead value", minted)
	}
	if got := s.held["SRE_DD_KEY"]; got != "fresh" {
		t.Errorf("sealed %q, want the replacement: a mint nobody recorded is a "+
			"credential nobody can use", got)
	}
	if len(res.Seats) != 1 || res.Seats[0].Err != nil {
		t.Fatalf("seat = %+v, want it reported healthy once repaired", res.Seats)
	}
	if !res.Seats[0].KeyMinted {
		t.Error("the replacement was not reported as minted")
	}
}

// AND "CANNOT TELL" CHANGES NOTHING. A Datadog blip that read as "no keys"
// would rotate every agent's credential on the loop's timer, which is an
// outage this engine caused — the same asymmetry atlassian.orphaned draws.
func TestAnUnreadableKeyListingLeavesAHeldCredentialAlone(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true}}]}`))
	}
	minted := 0
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(
		w http.ResponseWriter, r *http.Request,
	) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"errors":["unwell"]}`))
			return
		}
		minted++
		_, _ = w.Write([]byte(`{"data":{"id":"k9","attributes":{"key":"fresh"}}}`))
	}

	s := newSink()
	s.held["SRE_DD_KEY"] = "the-live-one"
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if minted != 0 {
		t.Error("a key was minted on an answer Datadog never gave, revoking " +
			"what the agent is authenticating with")
	}
	if got := s.held["SRE_DD_KEY"]; got != "the-live-one" {
		t.Errorf("the held value became %q", got)
	}
	if len(res.Seats) != 1 || res.Seats[0].Err != nil {
		t.Errorf("seat = %+v, want no failure over a blip", res.Seats)
	}
}
