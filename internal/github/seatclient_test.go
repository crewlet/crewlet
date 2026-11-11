package github_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/github"
)

// seatGitHub mints each installation its own token, `ghs-<installation>`, and
// answers a pull request read with the bearer it was asked with, so a case can
// tell which seat's app acted. It counts the mints.
func seatGitHub(t *testing.T, mints *atomic.Int64) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if installation, ok := strings.CutSuffix(r.URL.Path, "/access_tokens"); ok {
			mints.Add(1)
			id := installation[strings.LastIndex(installation, "/")+1:]
			_, _ = w.Write([]byte(`{"token":"ghs-` + id + `","expires_at":"2099-01-01T00:00:00Z"}`))
			return
		}
		_, _ = w.Write([]byte(`{"number":41,"title":"` +
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") + `"}`))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// AN AGENT'S ACT IS DONE AS THAT AGENT. The participant lookup takes the
// first seat that can read; a merge asked for by one agent must be made by
// that agent's own app, never by whichever installed seat comes first.
func TestClientForActsAsTheNamedSeatAlone(t *testing.T) {
	t.Parallel()
	var mints atomic.Int64
	base := seatGitHub(t, &mints)
	lookup := &github.SeatLookup{Opts: github.SeatAppOptions{
		APIBase: base, WebBase: base, Org: "acme",
		Seats: []github.SeatApp{installedSeat(t, "author", 41), installedSeat(t, "requester", 42)},
	}}
	client, err := lookup.ClientFor(t.Context(), "requester")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	pr, err := client.PullRequest(t.Context(), "acme", "web", 41)
	if err != nil {
		t.Fatalf("read through the seat's client: %v", err)
	}
	if pr.Title != "ghs-42" {
		t.Errorf("the client acted with %q, want the requester's own installation's token", pr.Title)
	}
	if mints.Load() != 1 {
		t.Errorf("%d tokens minted, want the requester's alone", mints.Load())
	}
}

// A SEAT THAT CANNOT ACT IS REFUSED BY NAME, and no other seat is asked: one
// not in the roster, one whose app is not created or installed, one switched
// off. Each refusal is comparable and names the handle and what to do.
func TestClientForRefusesASeatThatCannotAct(t *testing.T) {
	t.Parallel()
	var mints atomic.Int64
	base := seatGitHub(t, &mints)
	off := installedSeat(t, "paused", 43)
	off.Off = true
	lookup := &github.SeatLookup{Opts: github.SeatAppOptions{
		APIBase: base, WebBase: base, Org: "acme",
		Seats: []github.SeatApp{
			installedSeat(t, "other", 41),
			{Handle: "fresh", Permissions: tierPerms("read_only", false)},
			{Handle: "half", AppID: 44, Slug: "half", Permissions: tierPerms("read_only", false)},
			off,
		},
	}}
	for handle, want := range map[string]error{
		"stranger": github.ErrUnknownSeat,
		"fresh":    github.ErrNotInstalled,
		"half":     github.ErrNotInstalled,
		"paused":   github.ErrSeatOff,
	} {
		client, err := lookup.ClientFor(t.Context(), handle)
		if client != nil || !errors.Is(err, want) {
			t.Errorf("%s: ClientFor = %v, %v; want %v", handle, client, err, want)
			continue
		}
		if !strings.Contains(err.Error(), handle) || !strings.Contains(err.Error(), "Agents tab") {
			t.Errorf("%s: %q names neither the seat nor where to act", handle, err)
		}
	}
	if mints.Load() != 0 {
		t.Errorf("%d tokens minted for seats that cannot act: another seat was asked", mints.Load())
	}
}

// A REVIEW BY A SEAT'S OWN APP IS THAT SEAT'S, and only the app's bot account
// is: a person whose login is the slug is somebody else.
func TestSeatOfLoginMapsOnlyAnAppsBotAccount(t *testing.T) {
	t.Parallel()
	opts := github.SeatAppOptions{Seats: []github.SeatApp{
		{Handle: "sre-lead", Slug: "acme-sre-lead"},
		{Handle: "reviewer", Slug: "Acme-Reviewer", Off: true},
		{Handle: "fresh"},
	}}
	for login, want := range map[string]string{
		"acme-sre-lead[bot]": "sre-lead",
		"ACME-SRE-Lead[bot]": "sre-lead",
		"acme-reviewer[bot]": "reviewer",
		"acme-sre-lead":      "",
		"[bot]":              "",
		"":                   "",
		"someone[bot]":       "",
	} {
		handle, ok := opts.SeatOfLogin(login)
		if handle != want || ok != (want != "") {
			t.Errorf("SeatOfLogin(%q) = %q, %v; want %q", login, handle, ok, want)
		}
	}
}
