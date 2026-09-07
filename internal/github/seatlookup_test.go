package github_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// gitHubFor stands in for the API a seat's installation token reads: it mints
// tokens, and answers the two lists a participant lookup asks for. It counts
// the mints, which is what the token cache is judged on.
func gitHubFor(t *testing.T, commenters []string, mints *atomic.Int64) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			mints.Add(1)
			// An hour out, which is what GitHub issues and what the
			// cache's freshness check is measured against.
			_, _ = w.Write([]byte(
				`{"token":"ghs-minted","expires_at":"2099-01-01T00:00:00Z"}`))
		case strings.HasSuffix(r.URL.Path, "/comments"),
			strings.HasSuffix(r.URL.Path, "/reviews"):
			people := make([]map[string]any, 0, len(commenters))
			for _, login := range commenters {
				people = append(people, map[string]any{"user": map[string]any{"login": login}})
			}
			_ = json.NewEncoder(w).Encode(people)
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// installedSeat is one agent whose app exists, is installed, and holds a key.
func installedSeat(t *testing.T, handle string, id int64) github.SeatApp {
	t.Helper()
	_, pem := testKey(t)
	return github.SeatApp{
		Handle: handle, Name: handle, Tier: github.TierReadOnly,
		AppID: id, Slug: handle, InstallationID: id, Key: pem,
	}
}

// WHO ELSE IS TAKING PART, READ THROUGH THE AGENTS' OWN APPS.
//
// This took a personal access token an operator pasted in, held at the
// company and used for every repository. An agent that acts as itself already
// holds a credential that answers it: its app is installed on the
// repositories it works in and every tier grants the two reads a participant
// lookup needs. So the reader is there for free and scoped to what that agent
// may see, and there is one fewer credential to paste, rotate and be warned
// about.
func TestParticipantsAreReadThroughAnAgentsOwnApp(t *testing.T) {
	t.Parallel()
	var mints atomic.Int64
	base := gitHubFor(t, []string{"Ana", "writer"}, &mints)

	lookup := &github.SeatLookup{Opts: github.SeatAppOptions{
		APIBase: base, WebBase: base, Org: "acme",
		Seats: []github.SeatApp{installedSeat(t, "sre-lead", 41)},
	}}

	people, err := lookup.Of(t.Context(), "acme", "api", "issue", 16)
	if err != nil {
		t.Fatalf("the agents' own apps could not read the thread: %v", err)
	}
	if len(people) == 0 {
		t.Fatal("the lookup found nobody taking part")
	}

	// ONE MINT, HOWEVER MANY DELIVERIES. This is the inbound hot path: a
	// busy repository produces a delivery per app per comment, and minting
	// a token for each would spend a request per delivery to learn
	// something valid for an hour.
	before := mints.Load()
	for range 5 {
		if _, err := lookup.Of(t.Context(), "acme", "api", "issue", 16); err != nil {
			t.Fatalf("a later lookup failed: %v", err)
		}
	}
	if got := mints.Load(); got != before {
		t.Errorf("five more lookups minted %d extra tokens, want the cached one",
			got-before)
	}
}

// A SEAT WITH NOTHING TO READ WITH IS SKIPPED, and the next one is asked.
//
// A company is routinely mid-rollout, with some agents' apps installed and
// others not, and one seat that cannot answer must not take the whole
// company's fan-out down with it.
func TestALookupSkipsSeatsThatCannotRead(t *testing.T) {
	t.Parallel()
	var mints atomic.Int64
	base := gitHubFor(t, []string{"ana"}, &mints)

	installed := installedSeat(t, "reviewer", 42)
	lookup := &github.SeatLookup{Opts: github.SeatAppOptions{
		APIBase: base, WebBase: base, Org: "acme",
		Seats: []github.SeatApp{
			// No app at all, then created and installed nowhere, then
			// one that works.
			{Handle: "fresh", Tier: github.TierReadOnly},
			{Handle: "half", AppID: 41, Slug: "half", Tier: github.TierReadOnly},
			installed,
		},
	}}

	people, err := lookup.Of(t.Context(), "acme", "api", "issue", 16)
	if err != nil {
		t.Fatalf("a roster mid-rollout answered nothing: %v", err)
	}
	if len(people) == 0 {
		t.Fatal("the seat that could read was never asked")
	}
}

// A COMPANY WHOSE AGENTS HOLD NO APPS SAYS SO, rather than answering an empty
// list. Empty means "nobody else is taking part", which is a different fact
// from "nothing here could look", and the parser degrades to the payload's
// own names on an error while an empty answer it would believe.
func TestALookupWithNoInstalledAppRefusesRatherThanAnsweringEmpty(t *testing.T) {
	t.Parallel()
	lookup := &github.SeatLookup{Opts: github.SeatAppOptions{
		APIBase: "https://api.github.example", Org: "acme",
		Seats: []github.SeatApp{{Handle: "fresh", Tier: github.TierReadOnly}},
	}}

	people, err := lookup.Of(t.Context(), "acme", "api", "issue", 16)
	if err == nil {
		t.Fatalf("a company with no installed app answered %v", people)
	}
	if !strings.Contains(err.Error(), "no agent's app could read this thread") {
		t.Errorf("error = %v, and it does not say why nobody was looked up", err)
	}
}
