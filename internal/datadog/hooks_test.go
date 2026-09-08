package datadog_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/integration"
)

const (
	hookCollection = "/api/v1/integration/webhooks/configuration/webhooks"
	hookByName     = hookCollection + "/crewlet"
)

// stored is the webhook definition a fake region holds, and what it recorded
// about the writes that produced it.
type stored struct {
	hook    *datadog.Webhook
	creates int
	updates int
	deletes int
}

// serveWebhook makes the region answer the four webhook calls out of one
// definition, so a test asserts what a pass DID rather than which calls it
// happened to make.
func serveWebhook(reg *region, held *stored) {
	write := func(w http.ResponseWriter, r *http.Request) bool {
		var body datadog.Webhook
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return false
		}
		held.hook = &body
		return true
	}
	byName := func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if held.hook == nil {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"errors":["Webhook does not exist"]}`))
				return
			}
			_ = json.NewEncoder(w).Encode(held.hook)
		case http.MethodPut:
			held.updates++
			if write(w, r) {
				_ = json.NewEncoder(w).Encode(held.hook)
			}
		case http.MethodDelete:
			held.deletes++
			if held.hook == nil {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"errors":["Webhook does not exist"]}`))
				return
			}
			held.hook = nil
		}
	}
	reg.handle[hookByName] = byName
	reg.handle[hookCollection] = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		held.creates++
		if write(w, r) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(held.hook)
		}
	}
}

// noSeats makes the identity half of a pass answer empty, so a webhook test
// exercises only the inbound half.
func noSeats(reg *region) {
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	}
}

const (
	base  = "https://engine.test.invalid"
	token = "token-value"
)

func hookOptions(t *testing.T, reg *region, base string) datadog.Options {
	t.Helper()
	return datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
		Sink: newSink(), WebhookBase: base, WebhookToken: token,
	}
}

// THE ENGINE REGISTERS THE WEBHOOK, which is the whole inbound path.
//
// Datadog posts to whatever URL its Webhooks integration holds and to nothing
// else. Left to a person that address is written once and goes stale the
// first time the deployment moves, with the monitors still firing and the
// alerts landing nowhere.
func TestAPassRegistersTheWebhookAtDatadog(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	res, err := datadog.Reconcile(context.Background(), hookOptions(t, reg, base))
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if held.hook == nil {
		t.Fatal("no webhook was registered")
	}
	if got, want := held.hook.URL, base+"/webhooks/datadog"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
	if held.hook.Name != datadog.DefaultWebhookName {
		t.Errorf("name = %q, want %q", held.hook.Name, datadog.DefaultWebhookName)
	}
	// THE TEMPLATE IS THE WIRE FORMAT. Datadog posts an empty body without
	// one, so a definition carrying none delivers nothing the parser can
	// read.
	if held.hook.Payload != datadog.WebhookPayload {
		t.Errorf("payload = %q, want the template the decoder reads", held.hook.Payload)
	}
	if held.hook.EncodeAs != "json" {
		t.Errorf("encode_as = %q, want json: the route parses a JSON body",
			held.hook.EncodeAs)
	}
	// The shared token is the whole check on a delivery, and it travels in
	// the header the route reads.
	if !strings.Contains(held.hook.CustomHeaders, datadog.TokenHeader) ||
		!strings.Contains(held.hook.CustomHeaders, token) {
		t.Errorf("custom_headers = %q, want it to carry %s",
			held.hook.CustomHeaders, datadog.TokenHeader)
	}
	if res.Webhook == nil || !res.Webhook.Created {
		t.Errorf("result = %+v, want it to report the definition created", res.Webhook)
	}
	if len(res.Findings()) != 0 {
		t.Errorf("findings = %+v, want none: the webhook was registered", res.Findings())
	}
}

// A DEFINITION THAT IS ALREADY RIGHT IS NOT REWRITTEN.
//
// The pass runs every fifteen seconds. Writing on every tick would spend a
// rate limit on a change that is not one, and Datadog's own audit log would
// carry an edit per tick for the life of the deployment.
func TestASecondPassWritesNothing(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	for range 3 {
		if _, err := datadog.Reconcile(
			context.Background(), hookOptions(t, reg, base)); err != nil {
			t.Fatalf("pass: %v", err)
		}
	}
	if held.creates != 1 || held.updates != 0 {
		t.Errorf("three passes made %d creates and %d updates, want 1 and 0",
			held.creates, held.updates)
	}
}

// A MOVED DEPLOYMENT MOVES THE WEBHOOK.
//
// This is the failure the whole registration exists to close: the address at
// Datadog is the only thing that decides where an alert goes, and a
// deployment that moves without it keeps every monitor firing into an address
// that no longer answers.
func TestAMovedBaseMovesTheWebhook(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	if _, err := datadog.Reconcile(
		context.Background(), hookOptions(t, reg, base)); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	const moved = "https://moved.test.invalid"
	res, err := datadog.Reconcile(context.Background(), hookOptions(t, reg, moved))
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if held.updates != 1 {
		t.Fatalf("the definition was updated %d times, want 1", held.updates)
	}
	if got, want := held.hook.URL, moved+"/webhooks/datadog"; got != want {
		t.Errorf("url = %q, want %q", got, want)
	}
	if res.Webhook == nil || !res.Webhook.Updated {
		t.Errorf("result = %+v, want it to report the definition moved", res.Webhook)
	}
}

// A ROTATED TOKEN REACHES DATADOG.
//
// The token is rotated by writing a new value into the company document. A
// comparison that skipped the header would leave Datadog carrying the old one
// with nothing anywhere saying so: the route would refuse every delivery
// while the integration reported itself connected.
func TestARotatedTokenRewritesTheHeader(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	if _, err := datadog.Reconcile(
		context.Background(), hookOptions(t, reg, base)); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	opts := hookOptions(t, reg, base)
	opts.WebhookToken = "rotated-value"
	if _, err := datadog.Reconcile(context.Background(), opts); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if held.updates != 1 {
		t.Fatalf("the definition was updated %d times, want 1", held.updates)
	}
	if !strings.Contains(held.hook.CustomHeaders, "rotated-value") {
		t.Errorf("custom_headers = %q, want the rotated token", held.hook.CustomHeaders)
	}
}

// A CHECK REGISTERS NOTHING, on the same terms as an account: the missing
// definition is the fact a read-only pass exists to report.
func TestAReadOnlyPassRegistersNoWebhook(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	opts := hookOptions(t, reg, base)
	opts.Sink = nil
	res, err := datadog.Reconcile(context.Background(), opts)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if held.creates != 0 {
		t.Error("a read-only pass registered a webhook at the vendor")
	}
	if res.Webhook == nil || res.Webhook.Note == "" {
		t.Errorf("result = %+v, want a note saying no definition exists yet", res.Webhook)
	}
}

// NO PUBLIC ADDRESS, NO REGISTRATION. Pointing Datadog at an address that
// cannot be reached is worse than pointing it nowhere: Datadog reports a
// healthy webhook over deliveries that go nowhere.
func TestNoPublicBaseRegistersNothing(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	res, err := datadog.Reconcile(context.Background(), hookOptions(t, reg, ""))
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if held.creates != 0 {
		t.Error("a pass with no public base registered a webhook")
	}
	if res.Webhook == nil || !strings.Contains(res.Webhook.Note, "public base URL") {
		t.Errorf("result = %+v, want a note naming the missing public base", res.Webhook)
	}
}

// A REFUSED REGISTRATION IS AN INGRESS FINDING, not an identity one: it is
// every alert this company has going nowhere, and it is owned by whoever
// holds the Datadog keys.
func TestARefusedRegistrationIsReportedAsBlockedIngress(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	reg.handle[hookByName] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"errors":["Webhook does not exist"]}`))
	}
	reg.handle[hookCollection] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":["Forbidden"]}`))
	}

	res, err := datadog.Reconcile(context.Background(), hookOptions(t, reg, base))
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingIngressBlocked {
		t.Fatalf("findings = %+v, want one blocked-ingress finding", findings)
	}
}

// THE WEBHOOK GOES ON A DISCONNECT WHETHER OR NOT THE ACCOUNTS DO.
//
// This engine registered it, nothing else uses it, and one left behind posts
// every alert to a company that no longer has a block to route it.
func TestADisconnectWithdrawsTheWebhookEvenWhenAccountsStay(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	if _, err := datadog.Reconcile(
		context.Background(), hookOptions(t, reg, base)); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if err := datadog.Teardown(context.Background(), datadog.TeardownOptions{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
		RemoveSeats: false,
	}); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if held.hook != nil {
		t.Error("the webhook survived a disconnect")
	}
	// AND IT IS SAFE TO REPEAT. A teardown is re-run after a partial
	// failure, and refusing the second attempt would leave a disconnect
	// stuck on work that is already done.
	if err := datadog.Teardown(context.Background(), datadog.TeardownOptions{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
	}); err != nil {
		t.Fatalf("second teardown: %v", err)
	}
}
