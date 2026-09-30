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

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE WITH NO COMPANY SIGNS ITS FIRST PERSON IN, TAKES ITS FIRST COMPANY BY
// APPLY, AND SERVES THE HUMAN WRITE SURFACE — WITH NO RESTART.
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
// # What it asserts, in the order a founder meets it
//
//   - Before any company, the write surface is MOUNTED and says why it cannot
//     serve: `503 no_active_revision`, never the 404 of a route that does not
//     exist and never a panic over a nil half.
//   - The deployment's Tier A token invites a person; they redeem the link and
//     sign in with a password — on a node that has no company at all, because
//     the identity estate is the engine's core and runs from boot.
//   - The first company is applied, and the apply names the `native` stage.
//   - The same cookie files work through `/work/items` on the same API, with
//     nothing rebuilt: the surface reads the halves its request finds, and the
//     item is recorded as written by the person who signed in.
//
// Mutations, each run: build the core with the first company instead of at
// boot and the invitation is refused, there being no /iam to post it to; take
// the write surface's halves once when the API is wired and the last phase is
// the first phase's 503 for ever.
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
	n := &node{engine: e, app: app, server: srv, model: model, id: boot.Node.ID}

	jane := newBrowser(t)
	file := map[string]any{"title": "the first thing we do", "project": "ENG"}

	// THE WRITE SURFACE IS THERE AND SAYS WHY IT CANNOT SERVE YET.
	status, body := tierA(t, n, http.MethodPost, "/work/items", file)
	if status != http.StatusServiceUnavailable ||
		body["error"] != string(httpjson.CodeNoActiveRevision) {
		t.Fatalf("POST /work/items on a node with no company answered %d %v, "+
			"want 503 %s — the route is mounted and its halves are not up yet",
			status, body, httpjson.CodeNoActiveRevision)
	}

	// THE FIRST PERSON, invited under the deployment's own token, on a node
	// that has no company at all.
	signInUnbound(t, n, jane)

	// THE FIRST COMPANY, by apply — what the reconcile loop does with the
	// revision a `crewlet config import` activates.
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(companyDoc, model.url)))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	applied, stages, err := e.Apply(t.Context(), cfg)
	if err != nil || applied != configplane.StatusOK {
		t.Fatalf("Apply = (%s, %v, %v), want ok", applied, stages, err)
	}
	if !slices.Contains(stages, "native") {
		t.Errorf("the first company's apply got through %v, which does not "+
			"name the native stage", stages)
	}

	// AND THE SAME API SERVES THE SAME COOKIE'S WRITE, rebuilt by nothing.
	// Retried under ONE key while the node applies the chart's projects,
	// so a retry is the same operation rather than a second item.
	key := statelog.NewOpID(time.Now(), "")
	var filed map[string]any
	var last string
	settle(t, "the write surface to file work after the first apply", func() (bool, string) {
		status, body := jane.sendKeyed(n, http.MethodPost, "/work/items", key, file)
		last = fmt.Sprintf("%d %v", status, body)
		switch status {
		case http.StatusOK, http.StatusAccepted:
			filed = body
			return true, ""
		case http.StatusServiceUnavailable, http.StatusUnprocessableEntity:
			// THE NODE IS STILL APPLYING what the first company brought:
			// the halves' hydration, or the chart's projects. Both clear.
			return false, ""
		}
		return false, "POST /work/items after the first apply answered " + last
	})
	item, _ := filed["key"].(string)
	if !strings.HasPrefix(item, "ENG-") {
		t.Fatalf("the write answered no ENG work item key: %v (last %s)", filed, last)
	}

	var change tracker.HistoryEntry
	settle(t, "this node to apply "+item, func() (bool, string) {
		detail, err := e.Tracker().Task(t.Context(), item,
			tracker.DetailWants{History: true},
			statelog.Freshness{Level: statelog.ReadSession})
		if err != nil || len(detail.History) == 0 {
			return false, ""
		}
		change = detail.History[len(detail.History)-1]
		return true, ""
	})
	if change.Actor != janeLogin {
		t.Errorf("%s is written as %q (%s), want the person who signed in, %q",
			item, change.Actor, change.ActorKind, janeLogin)
	}
}

// signInUnbound invites a person bound to no seat under the deployment's Tier
// A token, redeems the link and signs them in with a password — which leaves
// the browser holding the session cookie.
//
// NO SEAT, because the node it runs on has no company, so it has no seat to
// bind anybody to; that is the whole premise. [signIn] is the same gesture
// for a fleet that has one.
func signInUnbound(t *testing.T, n *node, b *browser) {
	t.Helper()
	status, issued := tierA(t, n, http.MethodPost, "/iam/invitations", map[string]any{
		"email": janeAddress, "grants": janeGrants, "reason": "the first person",
	})
	if status != http.StatusCreated && status != http.StatusAccepted {
		t.Fatalf("POST /iam/invitations on a node with no company answered %d: %v",
			status, issued)
	}
	link, _ := issued["url"].(string)
	rest, found := strings.CutPrefix(link, deploymentURL+"/dashboard#/invite/")
	id, secret, split := strings.Cut(rest, ".")
	if !found || !split || id == "" || secret == "" {
		t.Fatalf("the invitation's link %q is not the dashboard's invitation "+
			"screen on %s", link, deploymentURL)
	}
	settle(t, "this node to apply the invitation", func() (bool, string) {
		row, err := n.engine.IAM().InvitationByID(t.Context(), id)
		return err == nil && row.ID == id, ""
	})
	status, redeemed := b.send(n, http.MethodPost, auth.AuthInvitePrefix+id, map[string]any{
		"secret": secret, "login": janeLogin, "name": "Jane Doe", "password": janePassword,
	})
	if status != http.StatusOK {
		t.Fatalf("redeeming the invitation answered %d: %v", status, redeemed)
	}
	status, opened := b.send(n, http.MethodPost, auth.PathAuthLogin, map[string]any{
		"login": janeLogin, "password": janePassword,
	})
	if status != http.StatusOK || opened["status"] != "signed_in" ||
		opened["login"] != janeLogin {
		t.Fatalf("signing in on a node with no company answered %d: %v", status, opened)
	}
	if b.session() == "" {
		t.Fatalf("the sign-in set no session cookie: %v", b.cookies)
	}
}
