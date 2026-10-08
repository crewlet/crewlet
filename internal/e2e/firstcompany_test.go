package e2e

import (
	"context"
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
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE WITH NO COMPANY SIGNS ITS TIER A TOKEN IN, TAKES ITS FIRST COMPANY BY
// APPLY, SERVES THE HUMAN WRITE SURFACE — WITH NO RESTART — AND ONLY THEN TAKES
// ITS FIRST PERSON, ONTO THE HUMAN SEAT THAT COMPANY DECLARES.
//
// # Why this is the case the core runtime exists for
//
// The quickstart's own route is to start a node with nothing configured and
// build the company from there, and the dashboard's "create the company" path
// needs a session to do it with. A node's whole durable state used to come up
// with its FIRST COMPANY, so a node started with none had no identity estate —
// no /auth, no /iam, nobody to sign in — and the surfaces its API took from
// the engine at startup (the write surface, the board, the operator's
// assistant) were absent for the life of the process: the company arrived by
// apply, and nobody could file work in it until somebody restarted the node.
//
// # Why the first session is the token's and not a person's
//
// Every person holds a human seat for as long as they are here (ADR-0026), and
// a node with no company has no seat to give anybody — so the session that
// creates the company is the deployment's own Tier A token, exchanged for a
// cookie, and the first PERSON arrives once the company declares the seat they
// will hold. This case used to invite its founder onto no seat at all, on this
// node, before any company existed; that is exactly what is refused now.
//
// # What it asserts, in the order a founder meets it
//
//   - Before any company, the write surface is MOUNTED and says why it cannot
//     serve: `503 no_active_revision`, never the 404 of a route that does not
//     exist and never a panic over a nil half.
//   - The deployment's Tier A token is exchanged for a session on a node that
//     has no company at all, because the identity estate is the engine's core
//     and runs from boot.
//   - Nobody can be invited or created yet: a person named onto no seat is
//     `400 seat_required`, and one named onto the seat the company is about to
//     declare is `409 no_active_revision` — a refusal naming its remedy, never
//     the 503 no wait would clear.
//   - The first company is applied, and the apply names the `native` stage.
//   - The same cookie files work through `/work/items` on the same API, with
//     nothing rebuilt: the surface reads the halves its request finds, and the
//     item is recorded as written by the token, under its own login.
//   - And now the first person is invited onto the founder's seat, signs in,
//     and files work AS that seat.
//
// Mutations, each run: build the core with the first company instead of at
// boot and the exchange is refused, there being no session table to open one
// in; take the write surface's halves once when the API is wired and the
// token's write is the first phase's 503 for ever; answer a seat lookup on a
// node with no company as unavailable and the founder's invitation is a 503.
func TestANodeWithNoCompanySignsInAndServesWorkAfterItsFirstApply(t *testing.T) {
	t.Parallel()
	model := newScriptedModel(t)
	boot := config.DefaultBootstrap()
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	boot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	withServingTierA(t, &boot)
	withSignIn(&boot)

	e, err := engine.New(t.Context(), engine.Options{Bootstrap: &boot})
	if err != nil {
		t.Fatalf("engine.New with no company: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}
	if e.Company() != nil {
		t.Fatal("the premise: this node has no company")
	}
	app, srv := serveAPI(t, e, &boot, nil)
	n := &node{engine: e, app: app, server: srv, model: model, id: e.Node().ID()}

	operator := newBrowser(t)
	file := map[string]any{"title": "the first thing we do", "project": "ENG"}

	// THE WRITE SURFACE IS THERE AND SAYS WHY IT CANNOT SERVE YET.
	status, body := tierA(t, n, http.MethodPost, "/work/items", file)
	if status != http.StatusServiceUnavailable ||
		body["error"] != string(httpjson.CodeNoActiveRevision) {
		t.Fatalf("POST /work/items on a node with no company answered %d %v, "+
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
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(companyDoc, model.url)))
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
	assertAuthor(t, n, item, iam.TokenLogin(e2eTokenID), tracker.AuthorOperator)

	// THE FIRST PERSON, now that the company declares the seat they will
	// hold: invited onto it under the same token, signed in, and writing AS
	// it.
	jane := newBrowser(t)
	signIn(t, n, jane)
	theirs := fileWork(t, n, jane, map[string]any{
		"title": "the second thing we do", "project": "ENG",
	})
	assertAuthor(t, n, theirs, janeSeat, tracker.AuthorHuman)
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
		status, body := b.sendKeyed(n, http.MethodPost, "/work/items", key, file)
		last = fmt.Sprintf("%d %v", status, body)
		switch status {
		case http.StatusOK, http.StatusAccepted:
			filed = body
			return true, ""
		case http.StatusServiceUnavailable, http.StatusUnprocessableEntity:
			return false, ""
		}
		return false, "POST /work/items answered " + last
	})
	item, _ := filed["key"].(string)
	if !strings.HasPrefix(item, "ENG-") {
		t.Fatalf("the write answered no ENG work item key: %v (last %s)", filed, last)
	}
	return item
}

// assertAuthor waits for this node to apply item and asserts who its last
// change is recorded as written by.
func assertAuthor(t *testing.T, n *node, item, actor string, kind tracker.AuthorKind) {
	t.Helper()
	var change tracker.HistoryEntry
	settle(t, "this node to apply "+item, func() (bool, string) {
		detail, err := n.engine.Tracker().Task(t.Context(), item,
			tracker.DetailWants{History: true},
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
