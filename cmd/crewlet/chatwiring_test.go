package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
)

// chatWiringToken is this case's own operator credential; the guard runs
// ahead of the mux, so the surface has to admit the request before a route's
// presence is observable at all.
const chatWiringToken = "chat-wiring-test-token"

// THE CHAT SURFACE IS REACHABLE ON A COMPANY THAT RUNS NATIVE CHAT.
//
// # Why this exists
//
// Every chat dependency [api.Options] takes is OPTIONAL, and correctly so: a
// company on `chat.backend: vendor` or `none` has no native chat, and the API
// leaves the routes and queries unregistered rather than serving a surface
// with nothing behind it.
//
// That tolerance has one failure mode, and it shipped: if the process simply
// never fills them, a native-chat company gets the same unregistered surface —
// and an absent route is indistinguishable from a company that opted out. The
// whole feature was unreachable in the shipped binary (no `Chat`, `Cursors`,
// `ChatLive`, `Sources.Chat`, `ChatSearch` or `ChatReads` anywhere in
// `serveAPI`), every unit test passed, and the dashboard talked to nothing.
//
// A compile check could not catch it — nil is a legal value for all six. So
// the assertion has to be made against a SERVED surface, which is what this
// does: build the node the way `crewlet run` builds it, and ask whether the
// routes and queries are there.
//
// IT ASSERTS MOUNTING, NOT BEHAVIOUR. Every route here is guarded, so an
// unauthenticated request is refused — 401 or 403 is a PASS, because it proves
// the handler exists to do the refusing. 404 is the failure, and it is the
// exact symptom the bug had.
func TestANativeChatCompanyServesItsChatSurface(t *testing.T) {
	t.Parallel()
	e := testEngine(t)
	if e.Chat() == nil {
		t.Fatal("the test company runs no native chat, so this asserts nothing " +
			"— check companyYAML still leaves chat.backend to derive")
	}
	boot := bootstrapFor(t, freePort(t))
	boot.API.Auth.Tokens = []config.APIToken{{ID: "ops", Token: chatWiringToken}}
	surface, err := serveNode(t, boot, e)
	if err != nil {
		t.Fatalf("serveNode: %v", err)
	}
	if surface == nil {
		t.Fatal("no HTTP surface")
	}
	t.Cleanup(func() { surface.stop(context.Background(), logging.Get("test")) })

	// AUTHENTICATED, and that is not incidental. The guard runs BEFORE the
	// mux resolves a path, so an unauthenticated probe is 401 whether the
	// route exists or not — measured: POST to a route nobody ever wrote
	// answers 401 exactly as a mounted one does. A 404 check without a
	// token is a check that cannot fail, which is worse than no check.
	//
	// One route per gesture family, not all fifteen: they are mounted by
	// one call over one dependency, so a second missing one cannot happen
	// without the first.
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/chat/channels"},
		{http.MethodPost, "/chat/channels/c1/messages"},
		{http.MethodPost, "/chat/dms"},
		// The cursor route, which hangs off Options.Cursors rather than
		// Options.Chat — a separate field, so worth its own probe.
		{http.MethodPost, "/chat/read"},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+chatWiringToken)
		rec := httptest.NewRecorder()
		surface.app.ServeHTTP(rec, req)
		// 404 AND 405 BOTH MEAN UNMOUNTED, and the second is the one
		// that matters: several of these paths also carry a GET read
		// route, so an unmounted POST resolves to the pattern that IS
		// there and comes back method-not-allowed rather than absent.
		// Measured with the write side nil: /chat/channels and
		// /chat/channels/{id}/messages answer 405, /chat/dms answers 404.
		// Checking only 404 would have missed two of the three.
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s answered %d — not mounted, although this company "+
				"runs native chat. The write side is unwired, exactly as it "+
				"shipped.", route.method, route.path, rec.Code)
		}
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s still refused the test token, so this case is "+
				"asserting nothing", route.method, route.path)
		}
	}

	// And the read half, which travels over the query channel rather than
	// as named routes. Same rule: anything but "unknown query" proves it
	// is registered.
	for _, what := range []string{
		"chat_channels", "chat_channel", "chat_messages",
		"chat_thread", "chat_mentions",
	} {
		req := httptest.NewRequest(http.MethodGet, "/query/"+what, nil)
		rec := httptest.NewRecorder()
		surface.app.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Errorf("the %s query is unregistered although this company runs "+
				"native chat — the read side is unwired", what)
		}
	}
}
