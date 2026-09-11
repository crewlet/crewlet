package datadog_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	if res.Webhook == nil || res.Webhook.Blocked == nil {
		t.Errorf("result = %+v, want it to say no definition exists yet", res.Webhook)
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
	if res.Webhook == nil || res.Webhook.Blocked == nil ||
		!strings.Contains(res.Webhook.Blocked.Detail, "public base URL") {
		t.Errorf("result = %+v, want it to name the missing public base", res.Webhook)
	}
}

// AND NOBODY IS TOLD A COMPANY IS COVERED WHEN NOTHING DELIVERS.
//
// The missing base was carried as a bare note and [datadog.Result.Findings]
// read only the failure beside it, so a pass that registered no webhook AT
// ALL reported nothing: the loop classified Datadog Ready over an inbound
// path that did not exist, which is the shape this package's own doc calls
// strictly worse than an integration that is switched off. It is
// FindingIngressBlocked and its subject is the field to set, because that
// subject is what a status row offers somebody to type into.
func TestNoPublicBaseIsReportedRatherThanNoted(t *testing.T) {
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
	findings := res.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the unregistered webhook reported", findings)
	}
	if findings[0].Kind != integration.FindingIngressBlocked {
		t.Errorf("kind = %q, want ingress_blocked", findings[0].Kind)
	}
	if findings[0].Subject != "integrations.public_base_url" {
		t.Errorf("subject = %q, want the field an operator has to set",
			findings[0].Subject)
	}
}

// A WEBHOOK TOKEN THAT DID NOT RESOLVE IS A MISSING CREDENTIAL, and it says
// so in the word the setup form joins on.
//
// [setup.Requirement.Blocks] on the `webhook_token` requirement declares that
// its absence produces credential_missing, and nothing in this package ever
// produced one — so the field that clears the fault was never offered, and
// the fault itself was never reported: a definition carrying no token is
// never registered, and every alert this company raises reaches nobody.
func TestAnUnresolvedWebhookTokenIsReportedAsAMissingCredential(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{}
	serveWebhook(reg, held)

	opts := hookOptions(t, reg, base)
	opts.WebhookToken = "   "
	res, err := datadog.Reconcile(context.Background(), opts)
	if err != nil {
		t.Fatalf("pass: %v", err)
	}
	if held.creates != 0 {
		t.Error("a definition carrying no token was registered")
	}
	findings := res.Findings()
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want the unresolved token reported", findings)
	}
	if findings[0].Kind != integration.FindingCredentialMissing {
		t.Errorf("kind = %q, want credential_missing: that is what the "+
			"webhook_token requirement declares it produces", findings[0].Kind)
	}
	if findings[0].Subject != "integrations.datadog.webhook_token" {
		t.Errorf("subject = %q, want the field that clears it", findings[0].Subject)
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
	if _, err := datadog.Teardown(context.Background(), datadog.TeardownOptions{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
		RemoveSeats: false, WebhookBase: base,
	}); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if held.hook != nil {
		t.Error("the webhook survived a disconnect")
	}
	// AND IT IS SAFE TO REPEAT. A teardown is re-run after a partial
	// failure, and refusing the second attempt would leave a disconnect
	// stuck on work that is already done.
	if _, err := datadog.Teardown(context.Background(), datadog.TeardownOptions{
		Client: reg.client(t), Config: cfgWith(), Creds: pair, WebhookBase: base,
	}); err != nil {
		t.Fatalf("second teardown: %v", err)
	}
}

// A DEFINITION THIS ENGINE DID NOT REGISTER IS NOT DELETED.
//
// The name is Datadog's primary key and it is not ownership: an organization
// may already hold one called "crewlet". The teardown issued its DELETE
// blind, so a disconnect took down an integration this engine never made —
// unrecoverably, where a refused disconnect is not. Every other vendor
// teardown in this tree proves ownership on the delivery URL first.
func TestADisconnectLeavesSomebodyElsesWebhookAlone(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{hook: &datadog.Webhook{
		Name: "crewlet", URL: "https://someone-else.example.com/hooks/theirs",
	}}
	serveWebhook(reg, held)

	_, err := datadog.Teardown(context.Background(), datadog.TeardownOptions{
		Client: reg.client(t), Config: cfgWith(), Creds: pair, WebhookBase: base,
	})
	if err == nil {
		t.Fatal("a webhook pointing somewhere else was removed without a word")
	}
	if held.hook == nil {
		t.Fatal("somebody else's webhook was deleted")
	}
	// AND THE REFUSAL SAYS WHERE IT POINTS, which is the one thing an
	// operator needs to decide what it is.
	if !strings.Contains(err.Error(), "someone-else.example.com") {
		t.Errorf("the refusal does not name the address: %v", err)
	}
}

// AND WITH NO PUBLIC BASE NOTHING IS WITHDRAWN, because nothing was ever
// registered without one — the pass refuses to, naming the missing base. A
// teardown that deleted here would be deleting on no evidence at all.
func TestADisconnectWithNoPublicBaseWithdrawsNothing(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)
	noSeats(reg)
	held := &stored{hook: &datadog.Webhook{Name: "crewlet", URL: base + "/webhooks/datadog"}}
	serveWebhook(reg, held)

	if _, err := datadog.Teardown(context.Background(), datadog.TeardownOptions{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
	}); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if held.hook == nil {
		t.Error("a webhook was deleted by a node that could not name its own address")
	}
}

// A DISCONNECT AND A RECONNECT COMPLETE A CYCLE.
//
// Disconnecting disables each account, which is the reversible form deletion
// is deliberately not: an account that merely stops working keeps the monitors
// and notebooks it authored, where deleting it makes them lose their author.
// The cost was that nothing could turn one back on — the pass refused, on the
// reasoning that undoing a decommission is somebody's decision — so every
// cycle ended in manual work or left another dead account. About thirty-seven
// accumulated in one deployment.
//
// The teardown records that it is the one disabling the account, in the one
// place that survives the disconnect: the account itself. The surface's status
// row is forgotten the moment the disconnect succeeds.
func TestADisconnectedAccountCanBeConnectedAgain(t *testing.T) {
	t.Parallel()
	reg := newRegion(t)
	orgOK(reg)

	// The instance holds one live account this engine made.
	account := struct {
		disabled bool
		title    string
	}{}
	var patches int
	reg.handle["/api/v2/users"] = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"data":[{"id":"u1","attributes":{
			"email":"crewlet-sre@agents.test.invalid","service_account":true,
			"disabled":%t,"title":%q}}]}`, account.disabled, account.title)
	}
	reg.handle["/api/v2/users/u1"] = func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			account.disabled = true
		case http.MethodPatch:
			patches++
			var body struct {
				Data struct{ Attributes map[string]any } `json:"data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if title, set := body.Data.Attributes["title"].(string); set {
				account.title = title
			}
			if off, set := body.Data.Attributes["disabled"].(bool); set {
				account.disabled = off
			}
		}
		_, _ = w.Write([]byte(`{"data":{"id":"u1"}}`))
	}
	// THE ACCOUNT HOLDS THE KEY THIS ENGINE MINTED FOR IT, which is the
	// value sealed in the seat's variable.
	keys := []string{"k1"}
	reg.handle["/api/v2/service_accounts/u1/application_keys"] = func(w http.ResponseWriter, _ *http.Request) {
		rows := make([]string, 0, len(keys))
		for _, id := range keys {
			rows = append(rows, fmt.Sprintf(`{"id":%q,"attributes":{"name":"crewlet"}}`, id))
		}
		fmt.Fprintf(w, `{"data":[%s]}`, strings.Join(rows, ","))
	}
	reg.handle["/api/v2/service_accounts/u1/application_keys/k1"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			keys = nil
		}
		w.WriteHeader(http.StatusNoContent)
	}

	if _, err := datadog.Teardown(context.Background(), datadog.TeardownOptions{
		Client: reg.client(t), Config: cfgWith(), Creds: pair,
		Plan: planWith("sre"), RemoveSeats: true,
	}); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if !account.disabled {
		t.Fatal("the disconnect left the account enabled")
	}
	// MARKED BEFORE IT WAS DISABLED, so a run interrupted between the two
	// leaves a marked live account rather than a disabled unmarked one —
	// the state nothing can ever undo on its own.
	if account.title != datadog.DisconnectedTitle {
		t.Fatalf("title = %q, so nothing records that this engine disabled it", account.title)
	}
	// AND THE APPLICATION KEY IS GONE. A live key on a disabled account is
	// a credential that works again the moment anybody re-enables it —
	// which this engine now does, so that moment is one button press away
	// rather than hypothetical. mattermost's teardown states the same rule
	// about its own bots.
	if len(keys) != 0 {
		t.Errorf("the account still holds %v after a disconnect: re-enabling it "+
			"restores a working credential to a company that disconnected", keys)
	}

	// AND NOW CONNECTING GETS IT BACK.
	s := newSink()
	s.held["SRE_DD_KEY"] = "already-held"
	res, err := datadog.Reconcile(context.Background(), datadog.Options{
		Client: reg.client(t), Config: cfgWith(), Plan: planWith("sre"),
		Creds: pair, Sink: s,
	})
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if account.disabled {
		t.Error("the reconnect left the account disabled, so the cycle still " +
			"ends in manual work or another dead account")
	}
	if account.title != "" {
		t.Errorf("title = %q after the reconnect: a live account still marked "+
			"disconnected would be re-enabled again on every pass", account.title)
	}
	if len(res.Seats) != 1 || res.Seats[0].Err != nil {
		t.Fatalf("seat = %+v, want it recovered", res.Seats)
	}
}
