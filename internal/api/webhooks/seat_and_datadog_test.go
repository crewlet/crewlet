package webhooks_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// GitHub delivers to every app installed on a repository, each delivery
// carrying its own id. A repository five agents work therefore produces five
// deliveries of one comment, and they are not duplicates: they are five
// agents being told, which is the point of each holding its own app. The
// handle in the path is the only thing that can tell them apart.
func TestGitHub_CarriesTheSeatItWasAddressedTo(t *testing.T) {
	t.Parallel()

	e := newEdge(t)
	body := []byte(`{"action":"created"}`)

	res := e.post(t, "/webhooks/github/agent-swe", body, githubDelivery(body, "gh-secret"))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", res.Code, res.Body)
	}

	if e.published.count() != 1 {
		t.Fatalf("published %d events", e.published.count())
	}

	if got := seatOf(t, e.published.last()); got != "agent-swe" {
		t.Fatalf("handle = %q, want agent-swe", got)
	}
}

// A single app serving a whole organisation names no seat, and refusing it
// would drop the one delivery every tenant already gets.
func TestGitHub_AcceptsADeliveryNamingNoSeat(t *testing.T) {
	t.Parallel()

	e := newEdge(t)
	body := []byte(`{"action":"opened"}`)

	res := e.post(t, "/webhooks/github", body, githubDelivery(body, "gh-secret"))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", res.Code, res.Body)
	}

	if got := seatOf(t, e.published.last()); got != "" {
		t.Fatalf("handle = %q, want empty", got)
	}
}

// The seat form verifies exactly as the bare one does. A path that carried a
// handle past the signature check would be a route anyone could post to.
func TestGitHub_SeatFormStillVerifies(t *testing.T) {
	t.Parallel()

	e := newEdge(t)
	body := []byte(`{"action":"created"}`)

	res := e.post(t, "/webhooks/github/agent-swe", body, githubDelivery(body, "wrong-secret"))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", res.Code)
	}

	if e.published.count() != 0 {
		t.Fatal("an unverified delivery was published")
	}
}

// AN AGENT'S APP IS VERIFIED AGAINST ITS OWN SECRET.
//
// GitHub generates a signing secret per app and returns it once, so a
// delivery from an agent's app is signed with that app's secret rather than
// the organization's. Checked against the organization's, every delivery from
// every agent was refused with a 503 while GitHub's own hook page showed the
// app healthy and the config showed a secret set.
func TestGitHub_AnAgentsAppVerifiesAgainstItsOwnSecret(t *testing.T) {
	t.Parallel()

	e := newEdge(t)
	e.secrets.GitHubSeat = map[string]string{"agent-swe": "seat-secret"}
	body := []byte(`{"action":"created"}`)

	res := e.post(t, "/webhooks/github/agent-swe", body, githubDelivery(body, "seat-secret"))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", res.Code, res.Body)
	}
	if got := seatOf(t, e.published.last()); got != "agent-swe" {
		t.Fatalf("handle = %q, want agent-swe", got)
	}

	// AND NOT AGAINST THE ORGANIZATION'S. A seat holding its own app has
	// one credential its deliveries can carry; accepting the other would
	// mean a leak of the organization secret forges any agent.
	res = e.post(t, "/webhooks/github/agent-swe", body, githubDelivery(body, "gh-secret"))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", res.Code)
	}
}

// A SEAT WITH NO APP OF ITS OWN FALLS BACK TO THE ORGANIZATION'S.
//
// The same path serves a second deployment: one organization app whose
// deliveries are pointed at a seat. That app has only the organization's
// secret, and refusing it would break every company on that shape.
func TestGitHub_ASeatWithNoAppOfItsOwnUsesTheOrganizationSecret(t *testing.T) {
	t.Parallel()

	e := newEdge(t)
	e.secrets.GitHubSeat = map[string]string{"agent-swe": "seat-secret"}
	body := []byte(`{"action":"created"}`)

	res := e.post(t, "/webhooks/github/agent-pm", body, githubDelivery(body, "gh-secret"))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", res.Code, res.Body)
	}
	if got := seatOf(t, e.published.last()); got != "agent-pm" {
		t.Fatalf("handle = %q, want agent-pm", got)
	}
}

// A COMPANY WITH ONLY PER-AGENT APPS STILL SERVES ITS ROUTE.
//
// One app per agent is the only way an agent acts as itself, so a company on
// that shape never configures an organization-wide app at all. Reading the
// organization's secret alone left it with nothing to verify with, and the
// route answered 503 to every delivery from every agent.
func TestGitHub_ServesACompanyThatHasOnlyPerAgentApps(t *testing.T) {
	t.Parallel()

	e := newEdge(t)
	e.secrets.GitHub = ""
	e.secrets.GitHubSeat = map[string]string{"agent-swe": "seat-secret"}
	body := []byte(`{"action":"created"}`)

	res := e.post(t, "/webhooks/github/agent-swe", body, githubDelivery(body, "seat-secret"))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", res.Code, res.Body)
	}

	// AND REFUSES THE ONE IT CANNOT CHECK. A seat with no app of its own
	// and no organization secret to fall back to has nothing to verify
	// with, which is 503 and never an accepted delivery.
	res = e.post(t, "/webhooks/github/agent-pm", body, githubDelivery(body, "seat-secret"))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", res.Code)
	}
}

func datadogDelivery(token string) map[string]string {
	return map[string]string{"X-Crewlet-Token": token}
}

func TestDatadog_AcceptsTheConfiguredToken(t *testing.T) {
	t.Parallel()

	e := newEdge(t)
	body := []byte(`{"id":"n-1","title":"CPU high","alert_transition":"Triggered"}`)

	res := e.post(t, "/webhooks/datadog", body, datadogDelivery("EXAMPLEDATADOGTOKEN0000000"))
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", res.Code, res.Body)
	}

	if e.published.count() != 1 {
		t.Fatal("a verified alert did not reach the queue")
	}
}

func TestDatadog_RefusesAWrongOrMissingToken(t *testing.T) {
	t.Parallel()

	cases := map[string]map[string]string{
		"wrong token": datadogDelivery("not-it"),
		"no token":    {},
	}

	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			e := newEdge(t)

			res := e.post(t, "/webhooks/datadog", []byte(`{"id":"n-1"}`), headers)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401", res.Code)
			}

			// The check runs before anything is written, so a forged
			// alert cannot pollute the feed on its way to being refused.
			if e.published.count() != 0 {
				t.Fatal("an unauthenticated alert was published")
			}
		})
	}
}

// Nothing to check against is not the same as a valid delivery: the route
// holds it for retry rather than accepting or silently dropping it.
func TestDatadog_HoldsADeliveryWithNoTokenConfigured(t *testing.T) {
	t.Parallel()

	e := newEdge(t)
	e.secrets.Datadog = ""

	res := e.post(t, "/webhooks/datadog", []byte(`{"id":"n-1"}`), datadogDelivery("anything"))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", res.Code)
	}

	if e.published.count() != 0 {
		t.Fatal("an alert was published with nothing to verify it")
	}
}

// Datadog's notification id survives its own retries, which is the only
// thing making dedupe possible on a route with no signature.
func TestDatadog_ClaimsOnTheNotificationID(t *testing.T) {
	t.Parallel()

	e := newEdge(t)
	body := []byte(`{"id":"n-1","title":"CPU high"}`)

	first := e.post(t, "/webhooks/datadog", body, datadogDelivery("EXAMPLEDATADOGTOKEN0000000"))
	second := e.post(t, "/webhooks/datadog", body, datadogDelivery("EXAMPLEDATADOGTOKEN0000000"))

	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("got %d then %d, want 200 twice", first.Code, second.Code)
	}

	if e.published.count() != 1 {
		t.Fatalf("a retried alert woke the seat %d times", e.published.count())
	}
}

// seatOf reads the handle off a published webhook event.
//
// Through the typed payload rather than the envelope's free-form bag: the
// handle is a field of RawWebhook, and reading it any other way would pass
// while the thing consumers actually receive stayed empty.
func seatOf(t *testing.T, ev *events.Event) string {
	t.Helper()

	if ev == nil {
		t.Fatal("nothing was published")
	}

	hook, ok := ev.Data.(*types.RawWebhook)
	if !ok {
		t.Fatalf("published payload is %T, want *types.RawWebhook", ev.Data)
	}

	return hook.Handle
}

// THE ENGINE'S OWN SHARED TOKEN DOES NOT REACH THE EVENT STORE.
//
// Datadog and Confluence Cloud have no signature to send, so both routes
// authenticate on a token carried in a header and compared for EQUALITY:
// every accepted delivery therefore arrives carrying the secret itself, not a
// value derived from it. Copied into the published delivery, it is written to
// the broker, kept in the dead-letter stream for the retention window, and
// rendered on the dashboard beside the payload. It is the same shape as the
// GitLab key the denylist was created for, in this engine's own header.
func TestTheSharedWebhookTokenIsRedactedBeforeADeliveryIsStored(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	body := []byte(`{"id":"n-1","title":"CPU high","alert_transition":"Triggered"}`)

	if got := e.post(t, "/webhooks/datadog", body, datadogDelivery("EXAMPLEDATADOGTOKEN0000000")).Code; got != http.StatusOK {
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
	if strings.Contains(string(blob), "EXAMPLEDATADOGTOKEN0000000") {
		t.Error("the published delivery carries the shared token verbatim, so it " +
			"lands in the event store, in the dead-letter stream, and on every " +
			"dashboard socket")
	}
	// AND THE HEADER IS STILL THERE, marked. A delivery whose headers lost a
	// name entirely reads as one that never carried it, which is a different
	// fact from one whose value was withheld.
	if !strings.Contains(string(blob), "REDACTED") {
		t.Errorf("the header was dropped rather than redacted:\n%s", blob)
	}
}

// A TOKEN TOO WEAK TO BE THE AUTHENTICATION IS ONE THIS ROUTE DOES NOT HAVE.
//
// Datadog signs nothing — its integration attaches headers with fixed values
// only — so the shared token is the entire check and a guessable one is a way
// to wake a seat with content somebody else chose. Config refuses a short
// literal; this is the other half, on the RESOLVED value, so a ${VAR} pointing
// at a word is refused exactly where the word itself would have been. Written
// in one place and checked in one of the two, the reference is the hole.
func TestDatadog_RefusesAShortSharedToken(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.secrets.Datadog = "letmein"

	res := e.post(t, "/webhooks/datadog",
		[]byte(`{"id":"n-1","title":"CPU high","alert_transition":"Triggered"}`),
		datadogDelivery("letmein"))
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503: %s", res.Code, res.Body)
	}
	// AND NOTHING WAS PUBLISHED. A 503 that still woke a seat would be
	// the failure with extra steps.
	if e.published.count() != 0 {
		t.Errorf("published %d events on a route that cannot authenticate",
			e.published.count())
	}
}

// The same on the other side of the same rule: Confluence Cloud's token rides
// in the query and is equally the whole check.
func TestConfluenceCloud_RefusesAShortSharedToken(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	e.secrets.ConfluenceToken = "letmein"

	res := e.post(t, "/webhooks/confluence/page_updated?token=letmein", []byte(cloudPage), nil)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503: %s", res.Code, res.Body)
	}
	if e.published.count() != 0 {
		t.Errorf("published %d events on a route that cannot authenticate",
			e.published.count())
	}
}
