package webhooks_test

import (
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/events/types"
)

// cloudPage is a Confluence Cloud delivery as it actually arrives: no event
// name, no signature, an actor as a bare account id at the top level, and the
// page under "page". Captured from a real site rather than written.
const cloudPage = `{"page":{"idAsString":"10000001","creatorAccountId":"712020:aaaa","spaceKey":"ENG","spaceId":20000001,"modificationDate":1788716539150,"lastModifierAccountId":"712020:aaaa","self":"https://example.atlassian.net/wiki/spaces/ENG/pages/10000001/x","id":"10000001","title":"a page","creationDate":1788716535483,"contentType":"page","version":2},"userAccountId":"712020:aaaa","timestamp":1788716539192,"accountType":"customer","updateTrigger":"edit_page","suppressNotifications":false}`

// The token rides in the query, because it is the only place Confluence
// Cloud will carry one: it signs nothing, drops userinfo, and honours no
// registration field, and the query string is delivered verbatim.
func TestConfluenceCloud_AcceptsTheConfiguredToken(t *testing.T) {
	t.Parallel()
	e := newEdge(t)

	res := e.post(t, "/webhooks/confluence/page_updated?token=conf-token", []byte(cloudPage), nil)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", res.Code, res.Body)
	}
	if e.published.count() != 1 {
		t.Fatalf("published %d events, want 1", e.published.count())
	}
}

// A wrong token is refused, and so is none. There is no signature to fall
// back on, so the token is the whole authentication.
func TestConfluenceCloud_RefusesAWrongOrMissingToken(t *testing.T) {
	t.Parallel()
	e := newEdge(t)

	for name, path := range map[string]string{
		"wrong":   "/webhooks/confluence/page_updated?token=not-it",
		"missing": "/webhooks/confluence/page_updated",
	} {
		t.Run(name, func(t *testing.T) {
			res := e.post(t, path, []byte(cloudPage), nil)
			if res.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401", res.Code)
			}
		})
	}
	if e.published.count() != 0 {
		t.Fatalf("published %d events from refused deliveries", e.published.count())
	}
}

// A route with no token configured has nothing to check against and answers
// 503 rather than accepting, exactly as its neighbours do. The delivery
// waits at Confluence and flows once the token is set.
func TestConfluenceCloud_HoldsDeliveriesWhenNoTokenIsConfigured(t *testing.T) {
	t.Parallel()
	e := newEdge(t, func(o *webhooks.Options) {
		s := o.Secrets()
		s.ConfluenceToken = ""
		o.Secrets = func() webhooks.Secrets { return s }
	})

	res := e.post(t, "/webhooks/confluence/page_updated?token=anything", []byte(cloudPage), nil)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", res.Code)
	}
}

// THE EVENT IS STAMPED FROM THE PATH. A Cloud payload names no event, so the
// registered path is the only thing that knows which one fired, and the
// parser reads the key the route writes.
func TestConfluenceCloud_StampsTheEventFromThePath(t *testing.T) {
	t.Parallel()
	e := newEdge(t)

	res := e.post(t, "/webhooks/confluence/comment_created?token=conf-token", []byte(cloudPage), nil)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	ev := e.published.last()
	if ev == nil {
		t.Fatal("nothing was published")
	}
	raw, ok := ev.Data.(*types.RawWebhook)
	if !ok {
		t.Fatalf("published payload is %T, want *types.RawWebhook", ev.Data)
	}
	if got, _ := raw.Body["event"].(string); got != "comment_created" {
		t.Fatalf("event stamped as %q, want comment_created", got)
	}
}

// The Data Center route is unchanged: a Cloud-shaped delivery posted to it
// carries no signature and is refused. The two routes have two credentials
// on purpose, so a Data Center operator can never mistake a token check for
// the signature their hook was registered with.
func TestConfluenceCloud_DataCenterRouteStillDemandsASignature(t *testing.T) {
	t.Parallel()
	e := newEdge(t)

	res := e.post(t, "/webhooks/confluence?token=conf-token", []byte(cloudPage), nil)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 from the signed route", res.Code)
	}
}

// A retry is a duplicate. Cloud sends no per-delivery identifier, so the
// claim is on the body, which is what stays identical across a retry.
func TestConfluenceCloud_ARetryIsADuplicate(t *testing.T) {
	t.Parallel()
	e := newEdge(t)

	first := e.post(t, "/webhooks/confluence/page_updated?token=conf-token", []byte(cloudPage), nil)
	second := e.post(t, "/webhooks/confluence/page_updated?token=conf-token", []byte(cloudPage), nil)
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("got %d then %d", first.Code, second.Code)
	}
	if e.published.count() != 1 {
		t.Fatalf("a retried delivery was published %d times", e.published.count())
	}
}

// A Confluence Automation "Send web request" rule, the one DOCUMENTED Cloud
// route, can set a header where the admin hook can only write a URL. Both
// land on one route with one credential.
func TestConfluenceCloud_AcceptsTheTokenInAHeaderToo(t *testing.T) {
	t.Parallel()
	e := newEdge(t)

	res := e.post(t, "/webhooks/confluence/page_updated", []byte(cloudPage),
		map[string]string{"X-Crewlet-Token": "conf-token"})
	if res.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", res.Code, res.Body)
	}
}

// TWO SAVES OF ONE PAGE ARE TWO EVENTS, and on this route the body is the
// only thing that can say so: Cloud sends no per-delivery identifier, so the
// claim is on the body hash. A Confluence Automation body that named only the
// page id and title was byte-identical across two saves inside the claim
// window, so the second was answered as a duplicate and woke nobody. The
// recipe in docs/integrations/confluence.md carries the page version for
// exactly this reason, and this is what holds it there.
func TestConfluenceCloud_TwoSavesOfOnePageAreTwoEvents(t *testing.T) {
	t.Parallel()
	e := newEdge(t)

	body := func(version string) []byte {
		return []byte(`{"page":{"id":"10000001","title":"Deploy runbook","version":{"number":"` +
			version + `"}},"space":{"key":"ENG"},"userAccountId":"712020:actor"}`)
	}
	for _, version := range []string{"3", "4"} {
		res := e.post(t, "/webhooks/confluence/page_updated?token=conf-token", body(version), nil)
		if res.Code != http.StatusOK {
			t.Fatalf("version %s got %d: %s", version, res.Code, res.Body)
		}
	}
	if e.published.count() != 2 {
		t.Fatalf("published %d events for two saves; a body with no per-event "+
			"field collapses them into one", e.published.count())
	}
}
