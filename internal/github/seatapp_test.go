package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
)

func kinds(findings []integration.Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, string(f.Kind))
	}
	return out
}

// A SEAT WITH NO APP IS A STEP OUTSTANDING, NOT A FAULT.
//
// Nobody has done anything wrong: there is a click left. The finding carries
// no action URL, because an app is created by POSTing a manifest from a page
// and there is no address to send anybody to.
//
// IT WAITS ON A PERSON, never on the engine. This was identity_missing for
// one commit, which reads as the engine provisioning an account: true on
// Slack, false here, because no engine can create a GitHub App for anybody.
// The card said "Setting up agents" over a seat where nothing was happening
// and nothing would until somebody clicked.
func TestASeatWithNoAppReportsTheClickThatIsLeft(t *testing.T) {
	t.Parallel()
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		Seats: []github.SeatApp{{Handle: "sre-lead", Permissions: tierPerms("review", false)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res.Findings); len(got) != 1 || got[0] != string(integration.FindingApprovalRequired) {
		t.Fatalf("findings = %v", got)
	}
	if phase, actor := res.Findings[0].Kind.Verdict(); actor != integration.ActorAdmin {
		t.Errorf("a seat needing an app waits on %v in phase %v, and only a "+
			"person at GitHub can create one", actor, phase)
	}
	if url := res.Findings[0].ActionURL; url != "" {
		t.Errorf("creating an app was given an address to follow: %q", url)
	}
	if len(res.Ready) != 0 {
		t.Errorf("a seat with no app reported ready: %v", res.Ready)
	}
}

// ONE AGENT WITH NO APP IS NAMED ONCE, however the document leaves it so.
//
// An agent can have no app two ways: its seat carries no `integrations.github`
// block at all, which the roster cannot hold and the engine names in
// [github.SeatAppOptions.Unrecorded], or its block records no app id. Each
// way used to be reported by its own reader, so an agent named by both was
// told twice to do the same thing, and a company with one of each read two
// sentences where the act is one. Mutation: report the roster's empty blocks
// apart from the unrecorded agents and this finds two findings.
func TestEveryAgentWithNoAppIsNamedOnceInOneFinding(t *testing.T) {
	t.Parallel()
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		Seats:      []github.SeatApp{{Handle: "sre-lead", Permissions: tierPerms("review", false)}},
		Unrecorded: []string{"swe", "sre-lead"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res.Findings); len(got) != 1 {
		t.Fatalf("findings = %v, want the one approval every agent with no app shares", got)
	}
	if got := res.Findings[0].Subjects; strings.Join(got, ",") != "sre-lead,swe" {
		t.Errorf("subjects = %v, want each agent once, in order", got)
	}
}

// AND AN AGENT THE ROSTER CANNOT HOLD IS STILL REPORTED when no seat carries
// a block at all, which is how a company that has just connected starts.
func TestAnAgentWithNoBlockIsReportedWithNoRoster(t *testing.T) {
	t.Parallel()
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		Unrecorded: []string{"swe"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 || !strings.Contains(res.Findings[0].Detail, "swe") {
		t.Fatalf("findings = %+v, want the agent named", res.Findings)
	}
}

// THE KEY IS ISSUED ONCE AND NEVER REISSUED, so an unresolved one is not a
// credential to re-enter: the app has to be created again, and the sentence
// has to say so or an operator hunts for a value that does not exist.
func TestAnUnresolvedKeySaysTheAppMustBeCreatedAgain(t *testing.T) {
	t.Parallel()
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		Seats: []github.SeatApp{{Handle: "sre-lead", AppID: 7, Permissions: tierPerms("review", false)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res.Findings); len(got) != 1 || got[0] != string(integration.FindingCredentialMissing) {
		t.Fatalf("findings = %v", got)
	}
	if detail := res.Findings[0].Detail; !strings.Contains(detail, "create it again") {
		t.Errorf("the finding does not say the app must be recreated: %q", detail)
	}
}

// A TIER IS A WORD IN A DOCUMENT UNTIL THE INSTALLATION IS READ BACK.
//
// A manifest can be edited before it is submitted and an installation widened
// afterwards, so what the app HOLDS is compared against what the tier asks
// for, in both directions.
func TestTheInstallationIsComparedAgainstTheTierInBothDirections(t *testing.T) {
	t.Parallel()
	held := map[string]string{
		"metadata": "read", "contents": "read", "issues": "read",
		"administration": "write",
	}
	if excess := github.Excess(held); len(excess) != 1 || excess[0] != "administration" {
		t.Fatalf("Excess = %v, want the denied grant", excess)
	}
	// A review seat cannot write what it was set up to write, and GitHub
	// would only say so by refusing its every token.
	short := tierPerms("review", false).Short(held)
	if len(short) == 0 {
		t.Fatal("a review seat on a read-only installation reports no shortfall")
	}
	for _, want := range []string{"issues", "pull_requests"} {
		if !strings.Contains(strings.Join(short, ","), want) {
			t.Errorf("the shortfall omits %q: %v", want, short)
		}
	}
}

// MINTING NEEDS AN INSTALLED APP, and saying so beats a request GitHub
// refuses: an app nobody installed sees no repository, so the token endpoint
// has nothing to scope to.
func TestMintingRefusesASeatWithNoInstallation(t *testing.T) {
	t.Parallel()
	_, err := github.TokenFor(context.Background(), github.SeatAppOptions{}, github.SeatApp{
		Handle: "sre-lead", AppID: 7,
	})
	if err == nil {
		t.Fatal("a seat with no installation minted a token")
	}
	if !strings.Contains(err.Error(), "no installed app") {
		t.Errorf("the refusal does not name the cause: %v", err)
	}
}

// A TEARDOWN OF SOMETHING ALREADY GONE IS THE OUTCOME ASKED FOR. Treating it
// as a failure would leave the record behind for ever, retried on every pass.
func TestUninstallingWhatIsAlreadyGoneSucceeds(t *testing.T) {
	t.Parallel()
	err := github.UninstallSeat(context.Background(), github.SeatAppOptions{}, github.SeatApp{
		Handle: "sre-lead",
	})
	if err != nil {
		t.Errorf("uninstalling a seat with no app failed: %v", err)
	}
}

// SEATS ARE REPORTED IN A STABLE ORDER, so two passes name the same seat
// first and a status line does not change every tick.
func TestSeatsAreOrderedSoAStatusLineIsStable(t *testing.T) {
	t.Parallel()
	got := github.SeatsFrom([]github.SeatApp{{Handle: "zeta"}, {Handle: "alpha"}, {Handle: "mid"}})
	if got[0].Handle != "alpha" || got[2].Handle != "zeta" {
		t.Errorf("order = %v", []string{got[0].Handle, got[1].Handle, got[2].Handle})
	}
}

// appAt stands in for GitHub, answering the four calls a seat reconcile
// makes. `present` is whether GitHub still knows the app: false makes every
// endpoint answer the 404 a deleted app answers with.
func appAt(t *testing.T, present bool, installations string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !present {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Integration not found"}`))
			return
		}
		switch r.URL.Path {
		case "/app":
			_, _ = w.Write([]byte(`{"id":7,"slug":"acme-sre-lead"}`))
		default:
			_, _ = w.Write([]byte(installations))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// A DELETED APP IS NOT AN UNINSTALLED ONE, and the two call for opposite acts.
//
// Both answer 404 from the installation endpoints. Read as "installed
// nowhere", a deleted app sent an operator to an install page GitHub itself
// 404s, on a card that said the app existed, for ever: the step could not be
// taken and the state could not change.
func TestAnAppGitHubNoLongerHasIsReportedAsGone(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	base := appAt(t, false, "[]")

	forgotten := ""
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		APIBase: base, WebBase: "https://github.com", Org: "acme",
		Seats: []github.SeatApp{{
			Handle: "sre-lead", AppID: 7, Slug: "acme-sre-lead",
			Key: pem, Permissions: tierPerms("review", false),
		}},
		Forget: func(_ context.Context, handle string) error {
			forgotten = handle
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res.Findings); len(got) != 1 || got[0] != string(integration.FindingApprovalRequired) {
		t.Fatalf("findings = %v, want the app to be reported missing", got)
	}
	// NO LINK, because there is nothing to follow: an app is created by a
	// form POST from a page carrying the operator's own session, and the
	// install link for an app that does not exist is a 404.
	if url := res.Findings[0].ActionURL; url != "" {
		t.Errorf("a deleted app was given an address to follow: %q", url)
	}
	if detail := res.Findings[0].Detail; !strings.Contains(detail, "no longer exists") {
		t.Errorf("the finding does not say the app is gone: %q", detail)
	}
	// AND THE DOCUMENT IS CORRECTED, which is what makes every other
	// surface agree: the roster's step, the card's tag and the finding all
	// read the same seat.
	if forgotten != "sre-lead" {
		t.Errorf("the stale app record was left in place (forgot %q)", forgotten)
	}
	if len(res.Forgotten) != 1 || res.Forgotten[0] != "sre-lead" {
		t.Errorf("Forgotten = %v", res.Forgotten)
	}
}

// A CHECK READS AND REPORTS. Without a sink the pass writes nothing, so the
// finding stands on its own and the record is corrected by the loop instead.
func TestADryRunReportsAGoneAppWithoutClearingIt(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		APIBase: appAt(t, false, "[]"), WebBase: "https://github.com", Org: "acme",
		Seats: []github.SeatApp{{
			Handle: "sre-lead", AppID: 7, Slug: "acme-sre-lead",
			Key: pem, Permissions: tierPerms("review", false),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res.Findings); len(got) != 1 || got[0] != string(integration.FindingApprovalRequired) {
		t.Fatalf("findings = %v", got)
	}
	if len(res.Forgotten) != 0 {
		t.Errorf("a check cleared a record: %v", res.Forgotten)
	}
}

// AN APP THAT EXISTS AND IS INSTALLED NOWHERE STILL SAYS "INSTALL IT".
//
// The probe above must not turn every empty installation list into a deleted
// app: creating an app and installing it are two acts a day apart, and the
// state between them is the ordinary one.
func TestAnAppInstalledNowhereStillAsksForTheInstall(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		APIBase: appAt(t, true, "[]"), WebBase: "https://github.com", Org: "acme",
		Seats: []github.SeatApp{{
			Handle: "sre-lead", AppID: 7, Slug: "acme-sre-lead",
			Key: pem, Permissions: tierPerms("review", false),
		}},
		Forget: func(context.Context, string) error {
			t.Error("an app that exists had its record cleared")
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res.Findings); len(got) != 1 || got[0] != string(integration.FindingApprovalRequired) {
		t.Fatalf("findings = %v, want the install to be asked for", got)
	}
	want := "https://github.com/organizations/acme/settings/apps/acme-sre-lead/installations"
	if url := res.Findings[0].ActionURL; url != want {
		t.Errorf("install link = %q, want %q", url, want)
	}
}

// A WRITE THIS ENGINE COULD NOT MAKE IS NOT A CLICK NOBODY MADE.
//
// The installation is found at GitHub and recording WHICH one fails. That
// arrived at the caller as a plain error, indistinguishable from a failed
// read, and for a seat with nothing recorded yet it was reported as "'s app
// exists and nothing has installed it, so it sees no repository: install it
// on acme" — a sentence that contradicts the one fact the pass established,
// pointing an operator at a page where they would find the app already
// installed. The real failure was logged nowhere.
func TestAnInstallationThatCannotBeRecordedSaysSo(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	base := appAt(t, true, `[{"id":42,"account":{"login":"acme"}}]`)

	boom := errors.New("the config surface refused the write")
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		APIBase: base, WebBase: "https://github.com", Org: "acme",
		Seats: []github.SeatApp{{
			Handle: "sre-lead", AppID: 7, Slug: "acme-sre-lead",
			Key: pem, Permissions: tierPerms("review", false),
		}},
		Record: func(context.Context, string, int64) error { return boom },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one", res.Findings)
	}
	got := res.Findings[0]
	if got.Kind != integration.FindingIdentityFailed {
		t.Errorf("kind = %q, want %q — the click was made, so this is not "+
			"something an operator does at GitHub",
			got.Kind, integration.FindingIdentityFailed)
	}
	if strings.Contains(got.Detail, "nothing has installed it") {
		t.Errorf("the finding claims nobody installed the app: %q", got.Detail)
	}
	if !strings.Contains(got.Detail, boom.Error()) {
		t.Errorf("the finding does not name the write that failed: %q", got.Detail)
	}
	// AND NO INSTALL LINK, because following it changes nothing.
	if got.ActionURL != "" {
		t.Errorf("an operator was sent to %q for a failure at this engine",
			got.ActionURL)
	}
}

// And the same for the other write: a stale id that could not be cleared
// leaves the seat minting against an installation GitHub has forgotten, which
// is a different sentence from either "installed" or "not installed".
func TestAStaleInstallationThatCannotBeClearedSaysSo(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/app" {
			_, _ = w.Write([]byte(`{"id":7,"slug":"acme-sre-lead"}`))
			return
		}
		// The recorded installation is gone; the app itself is not.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(server.Close)

	boom := errors.New("the config surface refused the write")
	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		APIBase: server.URL, WebBase: "https://github.com", Org: "acme",
		Seats: []github.SeatApp{{
			Handle: "sre-lead", AppID: 7, Slug: "acme-sre-lead",
			InstallationID: 99, Key: pem, Permissions: tierPerms("review", false),
		}},
		Record: func(context.Context, string, int64) error { return boom },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly one", res.Findings)
	}
	if detail := res.Findings[0].Detail; !strings.Contains(detail, boom.Error()) {
		t.Errorf("the finding does not name the write that failed: %q", detail)
	}
}

// AN APP CREATED BEFORE ITS SEAT OPTED IN TO WORKFLOWS SAYS WHERE TO WIDEN IT.
//
// A manifest is read once, at creation, so the opt-in written afterwards asks
// for a grant the app never requested. GitHub then refuses every token the
// seat asks for, and the card has to say that the widening starts on the app's
// permissions page and ends with the installation's approval, rather than send
// a person to an installation that has nothing to approve yet.
func TestAWorkflowsOptInTheAppLacksSendsItsOwnerToThePermissionsPage(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	// THE RECORDED INSTALLATION, read back by id, which is the call a seat
	// that already names one makes; a list answers the discovery call.
	installedWith := func(perms map[string]string) string {
		one := map[string]any{
			"id": 42, "account": map[string]any{"login": "acme", "type": "Organization"},
			"repository_selection": "all", "permissions": perms,
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			var body any = []any{one}
			switch r.URL.Path {
			case "/app":
				body = map[string]any{"id": 7, "slug": "acme-sre-lead"}
			case "/app/installations/42":
				body = one
			}
			if err := json.NewEncoder(w).Encode(body); err != nil {
				t.Error(err)
			}
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	seat := github.SeatApp{
		Handle: "sre-lead", AppID: 7, Slug: "acme-sre-lead", InstallationID: 42,
		Key: pem, Permissions: tierPerms("full_access", true),
	}
	reconcile := func(held map[string]string) *github.SeatAppResult {
		t.Helper()
		res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
			APIBase: installedWith(held), WebBase: "https://github.com", Org: "acme",
			Seats: []github.SeatApp{seat},
		})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := reconcile(tierPerms("full_access", false).Token())
	if got := kinds(res.Findings); len(got) != 1 || got[0] != string(integration.FindingGrantShort) {
		t.Fatalf("findings = %v, want the missing workflows grant", got)
	}
	want := "https://github.com/organizations/acme/settings/apps/acme-sre-lead/permissions"
	if url := res.Findings[0].ActionURL; url != want {
		t.Errorf("action link = %q, want the app's permissions page %q", url, want)
	}
	for _, step := range []string{"Workflows", "permissions page", "approves"} {
		if !strings.Contains(res.Findings[0].Detail, step) {
			t.Errorf("the finding does not name %q: %q", step, res.Findings[0].Detail)
		}
	}
	if len(res.Ready) != 0 {
		t.Errorf("a seat GitHub refuses every token for was reported ready: %v", res.Ready)
	}

	granted := tierPerms("full_access", true).Token()
	if res := reconcile(granted); len(res.Findings) != 0 || len(res.Ready) != 1 {
		t.Errorf("an installation holding the opt-in: findings %v, ready %v", kinds(res.Findings), res.Ready)
	}
}
