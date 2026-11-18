package e2e

import (
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A NODE WITH NO COMPANY SIGNS ITS TIER A TOKEN IN, TAKES ITS FIRST COMPANY BY
// APPLY, SERVES THE HUMAN WRITE SURFACE — WITH NO RESTART — AND ONLY THEN TAKES
// ITS FIRST PERSON, ONTO THE HUMAN SEAT THAT COMPANY DECLARES.
//
// A person holds a human seat (ADR-0032) and a node with no company has none,
// so the first session is the Tier A token exchanged for a cookie (the identity
// estate runs from boot). Before the company the write surface is mounted and
// says `503 no_active_revision`, never a 404 or a nil panic; an invitation onto
// no seat is `400 seat_required`, onto the seat the company will declare `409
// no_active_revision`. After apply the same cookie files work through
// `/worklet/items` with nothing rebuilt, recorded as written by the token; then
// the first person is invited onto the founder's seat and files work AS it.
func TestANodeWithNoCompanySignsInAndServesWorkAfterItsFirstApply(t *testing.T) {
	t.Parallel()
	model := newScriptedModel(t)
	boot := config.DefaultBootstrap()
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&boot.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	brokertest.GitletCacheIn(&boot.Gitlet)
	withServingTierA(t, &boot)
	withSignIn(&boot)

	// THE BRIDGE A SEATS NODE SERVES, bound before the company it will run
	// exists: the company arrives by apply, and its seats' executors call
	// their tools back over this node's bridge from the first turn.
	ln := bridgeListener(t, "")
	e, chatLive := bootEngine(t, engine.Options{
		Bootstrap: &boot, Environment: nodeEnvironment(nil), SandboxPollInterval: runPollInterval,
		BoxHostURL: listenerURL(ln),
	})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	if e.Company() != nil {
		t.Fatal("the premise: this node has no company")
	}
	app, srv := serveAPI(t, e, &boot, chatLive.Hub(), ln, nil)
	n := &node{engine: e, app: app, server: srv, model: model, id: e.Node().ID(),
		chat: chatLive}

	operator := newBrowser(t)
	file := map[string]any{"title": "the first thing we do", "project": "ENG"}

	// THE WRITE SURFACE IS THERE AND SAYS WHY IT CANNOT SERVE YET.
	status, body := tierA(t, n, http.MethodPost, "/worklet/items", file)
	if status != http.StatusServiceUnavailable ||
		body["error"] != string(httpjson.CodeNoActiveRevision) {
		t.Fatalf("POST /worklet/items on a node with no company answered %d %v, "+
			"want 503 %s — the route is mounted and its halves are not up yet",
			status, body, httpjson.CodeNoActiveRevision)
	}

	// THE SESSION THE COMPANY IS CREATED WITH: the deployment's own token,
	// exchanged for a cookie on a node that has no company at all.
	exchangeTierA(t, n, operator)

	// NOBODY CAN BE INVITED OR CREATED YET, and each refusal says why.
	refuseFirstPersonBeforeTheSeat(t, n)

	// THE FIRST COMPANY, by apply — what the reconcile loop does with the
	// revision a `crewlet config import` activates.
	cfg, err := config.ParseCompany([]byte(goldenCompany(model)))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	applied, stages, err := e.Apply(t.Context(), cfg, time.Now())
	if err != nil || applied != configplane.StatusOK {
		t.Fatalf("Apply = (%s, %v, %v), want ok", applied, stages, err)
	}
	if !slices.Contains(stages, "native") {
		t.Errorf("the first company's apply got through %v, which does not "+
			"name the native stage", stages)
	}

	// AND THE SAME API SERVES THE SAME COOKIE'S WRITE, rebuilt by nothing —
	// written by the token, which no directory row binds to a seat, under
	// its own whole login.
	item := fileWork(t, n, operator, file)
	assertAuthor(t, n, item, iam.TokenLogin(e2eTokenID), worklet.AuthorOperator)

	// THE FIRST PERSON, now that the company declares the seat they will
	// hold: invited onto it under the same token, signed in, and writing AS
	// it.
	jane := newBrowser(t)
	signIn(t, n, jane)
	theirs := fileWork(t, n, jane, map[string]any{
		"title": "the second thing we do", "project": "ENG",
	})
	assertAuthor(t, n, theirs, janeSeat, worklet.AuthorHuman)
}

// e2eTokenID is the id of the fixture's Tier A entry ([withCredential]), and so
// the login its session writes under: `token:<id>`.
const e2eTokenID = "e2e"

// exchangeTierA trades the deployment's Tier A bearer for a session cookie
// this browser then holds — what the dashboard's sign-in page does with a
// token typed into it, and the one way into a node nobody has been invited to.
func exchangeTierA(t *testing.T, n *node, b *browser) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		n.server.URL+"/auth/token", nil)
	if err != nil {
		t.Fatalf("build POST /auth/token: %v", err)
	}
	req.Header.Set("Origin", deploymentURL)
	res, err := n.server.Client().Do(present(req))
	if err != nil {
		t.Fatalf("POST /auth/token on member %s: %v", n.id, err)
	}
	defer res.Body.Close()
	b.keep(res.Cookies())
	if res.StatusCode != http.StatusOK || b.session() == "" {
		t.Fatalf("exchanging the Tier A token on a node with no company "+
			"answered %d and set %v, want 200 and a session cookie",
			res.StatusCode, b.cookies)
	}
}

// refuseFirstPersonBeforeTheSeat asserts that nobody can be invited or created
// on a node running no company: a person onto no seat is the request's own
// fault, and onto a seat the node has no company to hold is a refusal naming
// what to do — never a 503 a client would retry for ever.
func refuseFirstPersonBeforeTheSeat(t *testing.T, n *node) {
	t.Helper()
	for _, tc := range []struct {
		name   string
		path   string
		body   map[string]any
		status int
		code   httpjson.Code
	}{
		{"an invitation onto no seat", "/iam/invitations",
			map[string]any{"email": janeAddress, "grants": janeGrants},
			http.StatusBadRequest, httpjson.CodeSeatRequired},
		{"an invitation onto the founder's seat", "/iam/invitations",
			map[string]any{"email": janeAddress, "grants": janeGrants, "seat": janeSeat},
			http.StatusConflict, httpjson.CodeNoActiveRevision},
		{"a create onto the founder's seat", "/iam/people",
			map[string]any{"email": janeAddress, "login": janeLogin,
				"grants": janeGrants, "seat": janeSeat},
			http.StatusConflict, httpjson.CodeNoActiveRevision},
	} {
		status, body := tierA(t, n, http.MethodPost, tc.path, tc.body)
		if status != tc.status || body["error"] != string(tc.code) {
			t.Errorf("%s on a node with no company answered %d %v, want %d %s",
				tc.name, status, body, tc.status, tc.code)
		}
	}
}

// fileWork files one item through the human write surface as whoever this
// browser is signed in as, retried under ONE key while the node applies what
// the company brought — the halves' hydration, or the chart's projects — so a
// retry is the same operation rather than a second item. It answers the key.
func fileWork(t *testing.T, n *node, b *browser, file map[string]any) string {
	t.Helper()
	key := statelog.NewOpID(time.Now(), "")
	var (
		filed map[string]any
		last  string
	)
	settle(t, "the write surface to file work", func() (bool, string) {
		status, body := b.sendKeyed(n, http.MethodPost, "/worklet/items", key, file)
		last = fmt.Sprintf("%d %v", status, body)
		switch status {
		case http.StatusOK, http.StatusAccepted:
			filed = body
			return true, ""
		case http.StatusServiceUnavailable, http.StatusUnprocessableEntity:
			return false, ""
		}
		return false, "POST /worklet/items answered " + last
	})
	item, _ := filed["key"].(string)
	if !strings.HasPrefix(item, "ENG-") {
		t.Fatalf("the write answered no ENG work item key: %v (last %s)", filed, last)
	}
	return item
}

// assertAuthor waits for this node to apply item and asserts who its last
// change is recorded as written by.
func assertAuthor(t *testing.T, n *node, item, actor string, kind worklet.AuthorKind) {
	t.Helper()
	var change worklet.HistoryEntry
	settle(t, "this node to apply "+item, func() (bool, string) {
		detail, err := n.engine.Tracker().Task(t.Context(), item,
			worklet.DetailWants{History: true},
			statelog.Freshness{Level: statelog.ReadSession})
		if err != nil || len(detail.History) == 0 {
			return false, ""
		}
		change = detail.History[len(detail.History)-1]
		return true, ""
	})
	if change.Actor != actor || change.ActorKind != kind {
		t.Errorf("%s is written as %q (%s), want %q (%s)",
			item, change.Actor, change.ActorKind, actor, kind)
	}
}
