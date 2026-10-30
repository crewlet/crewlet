package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

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

// appAt stands in for GitHub, answering the calls keeping a seat's record in
// step makes. `present` is whether GitHub still knows the app: false makes every
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

// adoptOne keeps one seat's record in step against base, recording what the
// pass writes.
func adoptOne(t *testing.T, base string, seat github.SeatApp,
	record func(context.Context, string, int64) error) (forgotten []string, err error) {
	t.Helper()
	err = github.AdoptSeatApps(context.Background(), github.SeatAppOptions{
		APIBase: base, WebBase: "https://github.com", Org: "acme",
		Seats:  []github.SeatApp{seat},
		Record: record,
		Forget: func(_ context.Context, handle string) error {
			forgotten = append(forgotten, handle)
			return nil
		},
	})
	return forgotten, err
}

// noWrite is a Record that fails the test: the case under it must not write.
func noWrite(t *testing.T) func(context.Context, string, int64) error {
	return func(_ context.Context, handle string, id int64) error {
		t.Errorf("recorded installation %d for %s", id, handle)
		return nil
	}
}

// A DELETED APP IS NOT AN UNINSTALLED ONE, and the two call for opposite acts.
//
// Both answer 404 from the installation endpoints. Read as "installed
// nowhere", a deleted app would keep its record and the seat would wait on an
// install page GitHub itself 404s. So the app's own identity is asked for, and
// only an app GitHub answers 404 for is forgotten.
func TestAnAppGitHubNoLongerHasIsForgotten(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	seat := github.SeatApp{Handle: "sre-lead", AppID: 7, Slug: "acme-sre-lead", Key: pem}
	forgotten, err := adoptOne(t, appAt(t, false, "[]"), seat, noWrite(t))
	if err != nil || len(forgotten) != 1 || forgotten[0] != "sre-lead" {
		t.Errorf("a deleted app: forgot %v, %v", forgotten, err)
	}

	// AN APP THAT EXISTS AND IS INSTALLED NOWHERE KEEPS ITS RECORD: creating
	// an app and installing it are two acts a day apart, and the state
	// between them is the ordinary one.
	forgotten, err = adoptOne(t, appAt(t, true, "[]"), seat, noWrite(t))
	if err != nil || len(forgotten) != 0 {
		t.Errorf("an app installed nowhere: forgot %v, %v", forgotten, err)
	}
}

// A WRITE THIS ENGINE COULD NOT MAKE IS NOT A CLICK NOBODY MADE.
//
// The installation is found at GitHub and recording WHICH one fails. That is
// this engine's fault, so it is returned naming the write, and it never says
// nobody installed the app.
func TestAnInstallationThatCannotBeRecordedSaysSo(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	boom := errors.New("the config surface refused the write")
	refuse := func(context.Context, string, int64) error { return boom }

	_, err := adoptOne(t, appAt(t, true, `[{"id":42,"account":{"login":"acme"}}]`),
		github.SeatApp{Handle: "sre-lead", AppID: 7, Slug: "acme-sre-lead", Key: pem}, refuse)
	if err == nil || !strings.Contains(err.Error(), boom.Error()) ||
		strings.Contains(err.Error(), "nothing has installed it") {
		t.Errorf("an unrecordable discovery answered %v", err)
	}

	// AND THE OTHER WRITE: a stale id that could not be cleared leaves the
	// seat minting against an installation GitHub has forgotten.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/app" {
			_, _ = w.Write([]byte(`{"id":7,"slug":"acme-sre-lead"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(server.Close)
	_, err = adoptOne(t, server.URL, github.SeatApp{
		Handle: "sre-lead", AppID: 7, Slug: "acme-sre-lead", InstallationID: 99, Key: pem,
	}, refuse)
	if err == nil || !strings.Contains(err.Error(), boom.Error()) {
		t.Errorf("an uncleared stale installation answered %v", err)
	}
}
