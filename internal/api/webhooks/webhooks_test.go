package webhooks_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/store"
)

// pinned is the clock every test runs on. Pinned rather than time.Now because
// two of the five schemes sign a timestamp and check it against a replay
// window: a suite on the real clock would assert about the window's edges by
// sleeping.
var pinned = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

// recorder is a queue.Publisher that keeps what it was given, and can be made
// to fail.
type recorder struct {
	mu   sync.Mutex
	sent []*events.Event
	err  error
}

func (r *recorder) Publish(_ context.Context, topic string, ev *events.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	if topic != "crewlet.notifications.inbound" {
		return errors.New("published onto " + topic + ", which nothing consumes")
	}
	r.sent = append(r.sent, ev)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sent)
}

func (r *recorder) last() *events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sent) == 0 {
		return nil
	}
	return r.sent[len(r.sent)-1]
}

func (r *recorder) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// sink is an Emitter that keeps every envelope.
type sink struct {
	mu   sync.Mutex
	seen []livestate.Envelope
}

func (s *sink) Ingest(env livestate.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, env)
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// edge is one receiver plus everything the assertions need to look at.
type edge struct {
	db         *store.DB
	mux        *http.ServeMux
	published  *recorder
	stream     *sink
	events     *store.EventLog
	claims     coord.Claims
	secrets    *webhooks.Secrets
	configured *bool
}

// newEdge builds a receiver over a real store, with every secret set.
//
// A REAL store, not a fake: half of what this package promises is about what
// does and does not reach the event log, and a fake log would only prove that
// the calls were made in the order the test expected.
func newEdge(t *testing.T, opts ...func(*webhooks.Options)) *edge {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "w.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	secrets := &webhooks.Secrets{
		GitHub: "gh-secret", GitLab: gitlabSecret,
		Jira: "jira-secret", Confluence: "conf-secret", ForgeAppID: "app-123",
		Datadog: "dd-token",
		Slack:   map[string]string{"ceo": "slack-secret"},
	}
	configured := true
	e := &edge{
		db:         db,
		mux:        http.NewServeMux(),
		published:  &recorder{},
		stream:     &sink{},
		events:     db.Events(),
		claims:     coordmemory.NewFleet(),
		secrets:    secrets,
		configured: &configured,
	}
	options := webhooks.Options{
		Secrets:    func() webhooks.Secrets { return *secrets },
		Publisher:  e.published,
		Events:     e.events,
		Claims:     e.claims,
		Stream:     e.stream,
		Configured: func() bool { return configured },
		Now:        func() time.Time { return pinned },
		Keys:       testKeys(t),
	}
	for _, opt := range opts {
		opt(&options)
	}
	webhooks.New(options).Routes(e.mux)
	return e
}

// post sends a request through the mux and returns the response.
func (e *edge) post(t *testing.T, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res := httptest.NewRecorder()
	e.mux.ServeHTTP(res, req)
	return res
}

// closeStore breaks every database read and write this receiver makes.
//
// The real thing rather than an injected error: what the fail-open paths have
// to survive is a store that has GONE, and a fake that returned an error on
// call N would prove only that the call site handles that one error.
func (e *edge) closeStore(t *testing.T) {
	t.Helper()
	if err := e.db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

// rows returns every event the log holds.
func (e *edge) rows(t *testing.T) []store.EventRecord {
	t.Helper()
	recs, err := e.events.List(t.Context(), store.ListQuery{Limit: 100})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return recs
}

// --- fixtures --------------------------------------------------------------

func hexMAC(secret string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// githubDelivery is a signed GitHub request. Every source has one of these, so
// a test that wants "a valid delivery" never hand-assembles a signature.
func githubDelivery(body []byte, secret string) map[string]string {
	return map[string]string{
		"X-Hub-Signature-256": "sha256=" + hexMAC(secret, body),
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "delivery-1",
	}
}

func jiraDelivery(body []byte, secret string) map[string]string {
	return map[string]string{
		"X-Hub-Signature":                "sha256=" + hexMAC(secret, body),
		"X-Atlassian-Webhook-Identifier": "jira-delivery-1",
	}
}

func atlassianDelivery(body []byte, secret string) map[string]string {
	return map[string]string{"X-Hub-Signature": "sha256=" + hexMAC(secret, body)}
}

func slackDelivery(body []byte, secret string, at time.Time) map[string]string {
	ts := strconv.FormatInt(at.Unix(), 10)
	signed := append([]byte("v0:"+ts+":"), body...)
	return map[string]string{
		"X-Slack-Request-Timestamp": ts,
		"X-Slack-Signature":         "v0=" + hexMAC(secret, signed),
	}
}

var issueBody = []byte(`{"action":"opened","issue":{"number":7,"title":"Broken"},` +
	`"sender":{"login":"octocat"},"repository":{"full_name":"acme/widgets"}}`)

// --- the invariants every route shares -------------------------------------

// NOTHING UNVERIFIED IS DECODED.
//
// The raw body must be buffered before the gate — the signature is over it —
// but decoding it need not be, and doing so handed an unauthenticated caller
// a JSON unmarshal plus a map several times the size of what they sent, on
// every request, for nothing.
//
// The observable is the STATUS on a delivery that is both unsigned and
// unparseable: 401 says the gate ran first, 400 says the parser did. It is
// also the better answer on its own merits — an unauthenticated caller
// learns nothing about how their body was read.
func TestAnUnverifiedDeliveryIsNeverParsed(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	garbage := []byte("{this is not json")

	for _, route := range []struct{ name, path string }{
		{"github", "/webhooks/github"},
		{"gitlab", "/webhooks/gitlab"},
		{"jira", "/webhooks/jira"},
		{"confluence", "/webhooks/confluence"},
	} {
		t.Run(route.name, func(t *testing.T) {
			t.Parallel()
			// A wrong signature over a body that could never parse. Both
			// checks would reject it; only the order decides which does.
			got := e.post(t, route.path, garbage, map[string]string{
				"X-Hub-Signature-256": "sha256=" + hexMAC("wrong", garbage),
				"X-Hub-Signature":     "sha256=" + hexMAC("wrong", garbage),
				"webhook-signature":   "v1,bm90LWEtc2lnbmF0dXJl",
				"webhook-id":          "msg_1",
				"webhook-timestamp":   strconv.FormatInt(pinned.Unix(), 10),
				"X-GitHub-Event":      "issues",
			}).Code
			if got == http.StatusBadRequest {
				t.Fatalf("%s answered 400, so it decoded an unverified body "+
					"before checking the signature", route.name)
			}
			if got != http.StatusUnauthorized {
				t.Fatalf("%s answered %d, want 401", route.name, got)
			}
		})
	}
}

func TestAnUnsignedDeliveryReachesNothing(t *testing.T) {
	t.Parallel()
	// THE property this package exists for. These routes are exempt from
	// the API's bearer token, so an unverified POST that got as far as the
	// event store would pollute the audit log and inject content into every
	// connected dashboard — without ever waking an agent, which is what
	// made this hole invisible when it was open.
	for _, route := range []struct{ path, header string }{
		{"/webhooks/github", "X-Hub-Signature-256"},
		{"/webhooks/gitlab", "webhook-signature"},
		{"/webhooks/jira", "X-Hub-Signature"},
		{"/webhooks/confluence", "X-Hub-Signature"},
		{"/webhooks/slack/ceo", "X-Slack-Signature"},
	} {
		t.Run(route.path, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			body := []byte(`{"action":"opened","type":"event_callback"}`)

			// Unsigned.
			if got := e.post(t, route.path, body, nil).Code; got != http.StatusUnauthorized {
				t.Errorf("an unsigned delivery got %d, want 401", got)
			}
			// Signed with the wrong key, which is the case a shape
			// check alone would let through.
			forged := map[string]string{route.header: "sha256=" + hexMAC("wrong", body)}
			if route.path == "/webhooks/slack/ceo" {
				forged = slackDelivery(body, "wrong", pinned)
			}
			if got := e.post(t, route.path, body, forged).Code; got != http.StatusUnauthorized {
				t.Errorf("a forged delivery got %d, want 401", got)
			}

			if n := e.published.count(); n != 0 {
				t.Errorf("%d events were published for refused deliveries", n)
			}
			if n := e.stream.count(); n != 0 {
				t.Errorf("%d envelopes reached the dashboard for refused deliveries", n)
			}
			if rows := e.rows(t); len(rows) != 0 {
				t.Errorf("%d rows reached the event store for refused deliveries", len(rows))
			}
		})
	}
}

func TestARouteWithNoSecretHoldsTheDelivery(t *testing.T) {
	t.Parallel()
	// 5xx, never 4xx and never 200. A 4xx tells the sender its request was
	// malformed and should be discarded — and the request is fine; what is
	// missing is on this side. The delivery waits at the provider and flows
	// the moment somebody sets the secret.
	for _, route := range []string{
		"/webhooks/github", "/webhooks/gitlab",
		"/webhooks/jira", "/webhooks/confluence", "/webhooks/slack/ceo",
		"/webhooks/forge",
	} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			e := newEdge(t)
			*e.secrets = webhooks.Secrets{}

			res := e.post(t, route, []byte(`{}`), nil)
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("got %d, want 503", res.Code)
			}
			if got := res.Header().Get("Retry-After"); got != "300" {
				t.Errorf("Retry-After = %q, want the 300 s a human edit needs", got)
			}
			var body map[string]string
			if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["reason"] != "no_webhook_secret" {
				t.Errorf("reason = %q, which does not say what is missing", body["reason"])
			}
		})
	}
}

func TestAnEmptySlackMapIsCannotVerifyNotNothingToVerify(t *testing.T) {
	t.Parallel()
	// The exact hole this closes: with no Slack secret configured, a route
	// that skips verification entirely answers 200 to an unsigned POST. Anyone who could reach the port could publish a
	// raw_webhook addressed at any seat, and the engine treats that as a
	// message from Slack — it wakes the agent and drives a turn.
	e := newEdge(t)
	e.secrets.Slack = nil

	res := e.post(t, "/webhooks/slack/ceo", []byte(`{"type":"event_callback"}`), nil)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 — an unverifiable Slack delivery must not be accepted", res.Code)
	}
	if e.published.count() != 0 {
		t.Error("an unsigned Slack delivery reached the queue")
	}
}

func TestAnUnknownSlackSeatIsRefusedWithoutHoldingTheDelivery(t *testing.T) {
	t.Parallel()
	// 401, not 503: the map is populated, so this is a delivery addressed
	// to a seat with no Slack app rather than a node with nothing to check
	// against. A 503 would tell the sender to keep retrying something that
	// can never succeed.
	e := newEdge(t)
	res := e.post(t, "/webhooks/slack/nobody", []byte(`{"type":"event_callback"}`), nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", res.Code)
	}
}

func TestAnUnconfiguredNodeHoldsTheDeliveryBriefly(t *testing.T) {
	t.Parallel()
	// A node that missed a config activation must not answer 200 — its
	// peers are handling these, and a sender told "accepted" never retries.
	// The Retry-After is the SHORT one: this resolves on the next reconcile
	// poll, not on a human editing config.
	e := newEdge(t)
	*e.configured = false

	res := e.post(t, "/webhooks/github", issueBody, githubDelivery(issueBody, "gh-secret"))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", res.Code)
	}
	if got := res.Header().Get("Retry-After"); got != "15" {
		t.Errorf("Retry-After = %q, want the 15 s a reconcile poll needs — "+
			"the no-secret 300 would be a five-minute wait on something "+
			"that fixes itself", got)
	}
}

func TestABodyThatIsNotAnObjectIsRefused(t *testing.T) {
	t.Parallel()
	// A correctly signed list body must be a 400, not a panic on the way to
	// a 500: every reader downstream immediately asks the body for a field.
	e := newEdge(t)
	for _, body := range []string{`[1,2,3]`, `"a string"`, `null`, `not json`} {
		raw := []byte(body)
		res := e.post(t, "/webhooks/github", raw, githubDelivery(raw, "gh-secret"))
		if res.Code != http.StatusBadRequest {
			t.Errorf("body %q got %d, want 400", body, res.Code)
		}
	}
	if e.published.count() != 0 {
		t.Error("a non-object body was published")
	}
}

func TestABodyOverTheCapIsRefusedBeforeItIsBuffered(t *testing.T) {
	t.Parallel()
	// The body must be read before the signature can be checked — the
	// signature is over the body — so without a bound an unauthenticated
	// caller picks this process's allocation size.
	e := newEdge(t)
	huge := make([]byte, webhooks.MaxBodyBytes+1)
	for i := range huge {
		huge[i] = 'x'
	}
	res := e.post(t, "/webhooks/github", huge, githubDelivery(huge, "gh-secret"))
	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413", res.Code)
	}
}

func TestAVerifiedDeliveryReachesAllThreeSurfaces(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	res := e.post(t, "/webhooks/github", issueBody, githubDelivery(issueBody, "gh-secret"))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", res.Code, res.Body)
	}

	ev := e.published.last()
	if ev == nil {
		t.Fatal("nothing was published, so no agent will ever hear about it")
	}
	if ev.Type != "raw_webhook" {
		t.Errorf("published %q, want raw_webhook", ev.Type)
	}
	if ev.Source != "github" {
		t.Errorf("source = %q, want github", ev.Source)
	}

	rows := e.rows(t)
	if len(rows) != 1 {
		t.Fatalf("%d rows in the event log, want 1", len(rows))
	}
	if rows[0].Type != "webhook:issues" {
		t.Errorf("row type = %q, want webhook:issues", rows[0].Type)
	}
	if rows[0].Category != "webhook" {
		t.Errorf("row category = %q, want webhook", rows[0].Category)
	}
	if !strings.Contains(rows[0].Summary, "octocat") {
		t.Errorf("summary %q does not name who did it", rows[0].Summary)
	}

	if e.stream.count() != 1 {
		t.Errorf("%d envelopes reached the dashboard, want 1", e.stream.count())
	}
	// The row and the live envelope must share an id, or the dashboard
	// draws the delivery twice: once live, once again on reload.
	e.stream.mu.Lock()
	live := e.stream.seen[0]
	e.stream.mu.Unlock()
	if live.ID != rows[0].ID {
		t.Errorf("the live envelope (%s) and the stored row (%s) are two different events",
			live.ID, rows[0].ID)
	}
}

func TestTheStoredPayloadIsWhatTheProviderSent(t *testing.T) {
	t.Parallel()
	// Re-serializing the parsed body would reorder keys and reformat
	// numbers, so the delivery an operator opens in the dashboard would not
	// be the one that was signed.
	e := newEdge(t)
	body := []byte(`{"zeta":1,"alpha":2,"action":"opened"}`)
	if got := e.post(t, "/webhooks/github", body, githubDelivery(body, "gh-secret")).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	rows := e.rows(t)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	rec, err := e.events.ByID(t.Context(), rows[0].ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if string(rec.Payload) != string(body) {
		t.Errorf("stored payload = %s, want the bytes the provider sent (%s)",
			rec.Payload, body)
	}
}

func TestTheRawBytesTravelWithTheDelivery(t *testing.T) {
	t.Parallel()
	// A transport re-verifies the signature as defence in depth, and it
	// cannot do that from the parsed body: key order, whitespace and number
	// formatting are all free in JSON and all inside the provider's HMAC.
	e := newEdge(t)
	body := []byte("{\n  \"action\" : \"opened\"\n}")
	if got := e.post(t, "/webhooks/github", body, githubDelivery(body, "gh-secret")).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	raw, err := json.Marshal(e.published.last())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var envelope struct {
		BodyRaw []byte            `json:"body_raw"`
		Headers map[string]string `json:"headers"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(envelope.BodyRaw) != string(body) {
		t.Errorf("body_raw = %q, want the exact bytes %q", envelope.BodyRaw, body)
	}
	// The signature header travels too, or the second check has nothing to
	// check against.
	if envelope.Headers["x-hub-signature-256"] == "" {
		t.Error("the signature header did not travel, so a transport cannot re-verify")
	}
}

func TestCredentialHeadersAreRedacted(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	headers := githubDelivery(issueBody, "gh-secret")
	headers["Authorization"] = "Bearer super-secret"
	headers["Cookie"] = "session=abc"
	if got := e.post(t, "/webhooks/github", issueBody, headers).Code; got != http.StatusOK {
		t.Fatalf("got %d", got)
	}
	raw, _ := json.Marshal(e.published.last())
	if strings.Contains(string(raw), "super-secret") || strings.Contains(string(raw), "session=abc") {
		t.Errorf("a credential header travelled with the delivery: %s", raw)
	}
}

func TestARedeliveryDoesNotWakeTheSeatTwice(t *testing.T) {
	t.Parallel()
	// Every provider retry and every replay an operator triggers from the
	// provider UI arrives with the same delivery id. Without the claim each
	// one is a fresh wake, and the agent answers again.
	e := newEdge(t)
	headers := githubDelivery(issueBody, "gh-secret")

	first := e.post(t, "/webhooks/github", issueBody, headers)
	if first.Code != http.StatusOK {
		t.Fatalf("first delivery got %d", first.Code)
	}
	second := e.post(t, "/webhooks/github", issueBody, headers)
	if second.Code != http.StatusOK {
		t.Fatalf("the retry got %d, want a 200 so the provider stops retrying", second.Code)
	}
	if !strings.Contains(second.Body.String(), "duplicate") {
		t.Errorf("the retry's answer does not say it was a duplicate: %s", second.Body)
	}
	if n := e.published.count(); n != 1 {
		t.Errorf("%d events published for one delivery and its retry, want 1", n)
	}
}

// THE TRACKER'S REDELIVERIES ARE CLAIMED TOO.
//
// Jira states a per-delivery identifier on both deployments and repeats it on
// its own retries, so a delivery it re-sends after a slow response — or one an
// operator replays from the admin page — must not wake the seat a second
// time. The route once read no key at all, which made every retry a fresh
// wake and the seat answer again.
func TestATrackerRedeliveryDoesNotWakeTheSeatTwice(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	body := []byte(`{"webhookEvent":"jira:issue_created",` +
		`"issue":{"key":"ENG-42","fields":{"summary":"Fix the login redirect"}}}`)
	headers := jiraDelivery(body, "jira-secret")

	if first := e.post(t, "/webhooks/jira", body, headers); first.Code != http.StatusOK {
		t.Fatalf("first delivery got %d", first.Code)
	}
	second := e.post(t, "/webhooks/jira", body, headers)
	if second.Code != http.StatusOK {
		t.Fatalf("the retry got %d, want a 200 so Jira stops retrying", second.Code)
	}
	if !strings.Contains(second.Body.String(), "duplicate") {
		t.Errorf("the retry's answer does not say it was a duplicate: %s", second.Body)
	}
	if n := e.published.count(); n != 1 {
		t.Errorf("%d events published for one delivery and its retry, want 1", n)
	}
}

func TestADeliveryThatCouldNotBeQueuedIsNotClaimed(t *testing.T) {
	t.Parallel()
	// The claim is taken BEFORE the republish, because two concurrent
	// retries must not both wake the seat. If that republish then fails,
	// the provider's retry is the only other copy of the delivery — and it
	// would be refused by a row nothing clears inside the TTL.
	e := newEdge(t)
	e.published.fail(errors.New("broker down"))
	headers := githubDelivery(issueBody, "gh-secret")

	res := e.post(t, "/webhooks/github", issueBody, headers)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 so the provider retries", res.Code)
	}
	if rows := e.rows(t); len(rows) != 0 {
		t.Errorf("%d rows were written for a delivery nothing will handle — "+
			"the feed would show a webhook that reached no agent", len(rows))
	}

	e.published.fail(nil)
	retry := e.post(t, "/webhooks/github", issueBody, headers)
	if retry.Code != http.StatusOK {
		t.Fatalf("the retry got %d", retry.Code)
	}
	if e.published.count() != 1 {
		t.Error("the retry was refused as a duplicate, so the delivery is lost for the whole TTL")
	}
}

func TestASlackChallengeIsAnsweredWithoutASecret(t *testing.T) {
	t.Parallel()
	// Slack sends this the moment an app's request URL is set, which during
	// provisioning is before the operator has the signing secret to put in
	// config — so a verified handshake could never be completed and the app
	// could never be installed. Safe because the answer is a pure echo.
	e := newEdge(t)
	e.secrets.Slack = nil

	body := []byte(`{"type":"url_verification","challenge":"abc123"}`)
	res := e.post(t, "/webhooks/slack/ceo", body, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — an unanswerable handshake blocks provisioning", res.Code)
	}
	var answer map[string]string
	if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if answer["challenge"] != "abc123" {
		t.Errorf("challenge = %q, want the one that was sent", answer["challenge"])
	}
	// It is an ECHO and nothing more.
	if e.published.count() != 0 || e.stream.count() != 0 || len(e.rows(t)) != 0 {
		t.Error("the handshake persisted or published something")
	}
}

func TestAnUnconfiguredNodeDoesNotCompleteTheSlackHandshake(t *testing.T) {
	t.Parallel()
	// Answering it would verify a URL that then discards every event.
	e := newEdge(t)
	*e.configured = false
	res := e.post(t, "/webhooks/slack/ceo",
		[]byte(`{"type":"url_verification","challenge":"abc"}`), nil)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", res.Code)
	}
}

func TestSlackCarriesTheSeatItWasAddressedTo(t *testing.T) {
	t.Parallel()
	// One app per agent, and the seat is in the URL: nothing in the body
	// says which agent a message was for.
	e := newEdge(t)
	body := []byte(`{"type":"event_callback","event_id":"Ev1",` +
		`"event":{"type":"message","user":"U1","text":"hello","channel":"C1"}}`)
	res := e.post(t, "/webhooks/slack/ceo", body, slackDelivery(body, "slack-secret", pinned))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	raw, _ := json.Marshal(e.published.last())
	var envelope struct {
		Handle string `json:"handle"`
	}
	_ = json.Unmarshal(raw, &envelope)
	if envelope.Handle != "ceo" {
		t.Errorf("handle = %q, want ceo — without it the delivery names no seat", envelope.Handle)
	}
	if body := e.published.last(); body == nil {
		t.Fatal("nothing published")
	}
	// Slack reads the body of a 200.
	if !strings.Contains(res.Body.String(), `"ok":true`) {
		t.Errorf("answer = %s, want Slack's own success shape", res.Body)
	}
}

func TestSlackRetriesAreDedupedOnTheEventID(t *testing.T) {
	t.Parallel()
	// Slack repeats event_id on a retry and sets X-Slack-Retry-Num. The
	// obvious edge dedupes nothing here and leaves the question to a
	// per-process ring in the transport, which answers correctly for one
	// node and wrongly for two.
	e := newEdge(t)
	body := []byte(`{"type":"event_callback","event_id":"Ev9",` +
		`"event":{"type":"app_mention","user":"U1"}}`)
	headers := slackDelivery(body, "slack-secret", pinned)

	if got := e.post(t, "/webhooks/slack/ceo", body, headers).Code; got != http.StatusOK {
		t.Fatalf("first: %d", got)
	}
	headers["X-Slack-Retry-Num"] = "1"
	headers = slackDelivery(body, "slack-secret", pinned)
	if got := e.post(t, "/webhooks/slack/ceo", body, headers).Code; got != http.StatusOK {
		t.Fatalf("retry: %d", got)
	}
	if n := e.published.count(); n != 1 {
		t.Errorf("%d events for one Slack message and its retry, want 1", n)
	}
}

func TestAStoreOutageDoesNotSwallowTheWake(t *testing.T) {
	t.Parallel()
	// The event log is observability. A delivery that reached the queue
	// will be worked whatever happens to the audit row, and a receiver that
	// failed the request over a store error would drop real work to keep a
	// feed tidy.
	e := newEdge(t, func(o *webhooks.Options) { o.Events = nil; o.Claims = nil })
	res := e.post(t, "/webhooks/github", issueBody, githubDelivery(issueBody, "gh-secret"))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", res.Code)
	}
	if e.published.count() != 1 {
		t.Error("the delivery did not reach the queue")
	}
}

func TestGETIsNotAWebhook(t *testing.T) {
	t.Parallel()
	// The routes are POST-only. A GET that fell through to a handler would
	// read an empty body, fail verification and answer 401 — which reads
	// like a credential problem to whoever is holding the browser.
	e := newEdge(t)
	req := httptest.NewRequest(http.MethodGet, "/webhooks/github", nil)
	res := httptest.NewRecorder()
	e.mux.ServeHTTP(res, req)
	if res.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /webhooks/github got %d, want 405", res.Code)
	}
}

func TestAClientThatHangsUpStillLeavesARecord(t *testing.T) {
	t.Parallel()
	// The wake is already queued by the time the row is written, so the row
	// is owed whatever the client does next. A store write on the request's
	// own context would be cancelled by a sender that drops the connection
	// the instant it is answered — leaving work happening with no record of
	// why, which is the shape of an outage nobody can explain afterwards.
	e := newEdge(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/webhooks/github",
		bytes.NewReader(issueBody)).WithContext(ctx)
	for k, v := range githubDelivery(issueBody, "gh-secret") {
		req.Header.Set(k, v)
	}
	res := httptest.NewRecorder()
	e.mux.ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 — the delivery was verified and queued", res.Code)
	}
	if rows := e.rows(t); len(rows) != 1 {
		t.Errorf("%d rows, want 1: the audit row followed the client out", len(rows))
	}
}

// CREDENTIAL HEADERS DO NOT REACH THE EVENT STORE.
//
// A delivery's headers are persisted and rendered on the dashboard, so any
// secret among them is a secret at rest in the audit log — readable by
// everyone who can read an event, and impossible to un-write.
//
// `x-gitlab-token` is here because of what put it there. The provisioner
// registered the minted signing key in GitLab's plaintext `token` attribute
// rather than `signing_token`, so GitLab echoed a 32-byte HMAC key back on
// every single delivery, and it was copied verbatim into the stored headers.
// The provisioning bug is fixed and this engine no longer sets that field,
// but a hook created by an older version still carries the old value and
// still sends it.
func TestCredentialHeadersAreRedactedBeforeADeliveryIsStored(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.secrets.GitLab = gitlabSecret

	body := []byte(`{"object_kind":"issue"}`)
	id, ts := "msg_redact", strconv.FormatInt(pinned.Unix(), 10)
	headers := map[string]string{
		"webhook-id":        id,
		"webhook-timestamp": ts,
		"webhook-signature": gitlabSignature(t, gitlabSecret, id, ts, body),
		"X-Gitlab-Event":    "Issue Hook",
		// The three shapes a credential arrives in.
		"X-Gitlab-Token": "whsec_the-key-an-old-hook-still-echoes",
		"Authorization":  "Bearer a-real-looking-token",
		"Cookie":         "session=abc123",
	}
	if got := e.post(t, "/webhooks/gitlab", body, headers).Code; got != http.StatusOK {
		t.Fatalf("the delivery was refused with %d", got)
	}

	ev := e.published.last()
	if ev == nil {
		t.Fatal("nothing was published")
	}
	blob, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{
		"whsec_the-key-an-old-hook-still-echoes",
		"a-real-looking-token",
		"session=abc123",
	} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("the published delivery carries %q verbatim, so it lands "+
				"in the event store and on every dashboard socket", secret)
		}
	}
	// AND THE SIGNATURE SURVIVES. It is an HMAC output rather than a key, and
	// it is the evidence that tells "the provider did not sign this" from
	// "the provider signed it with the wrong key".
	if !strings.Contains(string(blob), "webhook-signature") {
		t.Error("the signature header was stripped, so an operator cannot " +
			"tell an unsigned delivery from a wrongly-signed one")
	}
}

// --- delivery deduplication, for the vendors that send no delivery id ------ //

// CONFLUENCE DATA CENTER SENDS THE SAME HEADER ITS JIRA TWIN DOES, and the
// route ignored it: the claim short-circuits on an empty key, so every
// Confluence redelivery was handled again.
func TestConfluenceRetriesAreDedupedOnTheDeliveryHeader(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	body := []byte(`{"event":"page_updated","page":{"id":"7","title":"Runbook"}}`)
	for attempt := range 2 {
		headers := atlassianDelivery(body, "conf-secret")
		headers["X-Atlassian-Webhook-Identifier"] = "conf-delivery-1"
		if got := e.post(t, "/webhooks/confluence", body, headers).Code; got != http.StatusOK {
			t.Fatalf("attempt %d: %d", attempt, got)
		}
	}
	if n := e.published.count(); n != 1 {
		t.Errorf("%d events for one Confluence delivery and its retry, want 1", n)
	}
}

// AN INSTANCE THAT SENDS NO HEADER STILL DEDUPES. Which Atlassian builds
// carry the identifier has moved between versions, and a fallback that
// answered "no key" would leave those deployments exactly as they were.
func TestAConfluenceInstanceWithNoDeliveryHeaderStillDedupes(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	body := []byte(`{"event":"page_updated","page":{"id":"7","title":"Runbook"}}`)
	for range 2 {
		if got := e.post(t, "/webhooks/confluence", body,
			atlassianDelivery(body, "conf-secret")).Code; got != http.StatusOK {
			t.Fatalf("got %d", got)
		}
	}
	if n := e.published.count(); n != 1 {
		t.Errorf("%d events for one headerless delivery and its retry, want 1", n)
	}
}

// TWO CONFLUENCE EVENTS THAT SHARE NO IDENTIFIER ARE BOTH DELIVERED.
func TestTwoConfluenceEventsAreBothDelivered(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	for _, title := range []string{"Runbook", "Handbook"} {
		body := []byte(`{"event":"page_updated","page":{"id":"7","title":"` + title + `"}}`)
		if got := e.post(t, "/webhooks/confluence", body,
			atlassianDelivery(body, "conf-secret")).Code; got != http.StatusOK {
			t.Fatalf("%s: %d", title, got)
		}
	}
	if n := e.published.count(); n != 2 {
		t.Errorf("%d events for two distinct Confluence events, want 2", n)
	}
}

// AN EMPTY BODY IS NOT A KEY. It is the same for every delivery, so keying on
// it would claim the first and refuse every other delivery from that vendor
// for the whole TTL.
func TestAnEmptyBodyIsNotADeliveryKey(t *testing.T) {
	t.Parallel()
	if got := webhooks.BodyKeyForTest(nil); got != "" {
		t.Errorf("an empty body keyed as %q", got)
	}
	if webhooks.BodyKeyForTest([]byte("{}")) == "" {
		t.Error("a real body produced no key")
	}
}
