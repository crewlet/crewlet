package collect_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/integration"
)

// AN ASK IS HONOURED ONCE THE FLOOR HAS PASSED, AND NEVER BEFORE: a source
// read a minute ago is not read again because somebody asked, the tick past
// the floor reads it well before its interval, and the ask is dropped.
//
// Mutation: read on any waiting ask, and the tick a minute after the first
// read reads the vendor a second time.
func TestAnAskIsHonouredOnlyOnceTheFloorHasPassed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.source.set(world(t, t0, originA), nil)
	h.tick(0)
	ask := collect.Request{RequestedAt: t0.Add(30 * time.Second), By: "ada"}
	if err := h.store.Ask(t.Context(), integration.KindCloudflare, ask); err != nil {
		t.Fatal(err)
	}
	h.tick(time.Minute)
	if h.source.reads != 1 {
		t.Fatalf("an ask a minute after a read was honoured inside the floor: %d reads", h.source.reads)
	}
	if asked := h.asks(); !asked[integration.KindCloudflare].RequestedAt.Equal(ask.RequestedAt) {
		t.Fatalf("the ask did not wait for the floor: %+v", asked)
	}
	h.tick(collect.RefreshFloor - time.Minute)
	if h.source.reads != 2 {
		t.Fatalf("the tick at the floor read %d times, want the ask honoured", h.source.reads)
	}
	if asked := h.asks(); len(asked) != 0 {
		t.Errorf("the ask outlived the read that answered it: %+v", asked)
	}
	h.tick(time.Minute)
	if h.source.reads != 2 {
		t.Errorf("an answered ask read the source again")
	}
}

// AN ASK A READ ALREADY ANSWERED, OR FOR A SOURCE THE DUTY DOES NOT READ, is
// dropped rather than left to trigger a read some later day.
func TestAnAskNothingWaitsOnIsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.source.set(world(t, t0, originA), nil)
	for _, kind := range []integration.Kind{integration.KindCloudflare, integration.KindDatadog} {
		if err := h.store.Ask(t.Context(), kind, collect.Request{RequestedAt: t0.Add(-time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	h.tick(0)
	h.tick(time.Minute)
	if asked := h.asks(); len(asked) != 0 {
		t.Errorf("asks nothing waits on were kept: %+v", asked)
	}
	if h.source.reads != 1 {
		t.Errorf("a stale ask read the source %d times", h.source.reads)
	}
}

// EACH SCOPE KEEPS ITS OWN LAST WHOLE READ: a server read whole is read at
// this read, one refused a part keeps the instant it was last read whole and
// counts the parts, and one the read no longer visits has no standing.
func TestAScopeKeepsItsLastWholeReadThroughARefusedOne(t *testing.T) {
	t.Parallel()
	first := collect.Decide(nil, collect.Read{Scopes: []string{"web-2", "web-1", "old"}},
		collect.Status{Kind: integration.KindSSH}, t0)
	later := t0.Add(time.Hour)
	read := collect.Read{Scopes: []string{"web-1", "web-2"}, Unread: []collect.Unread{
		{Part: "web-2 ports", Scope: "web-2", Detail: "The engine could not reach the server."},
		{Part: "web-2 containers", Scope: "web-2", Detail: "The engine could not reach the server."},
	}}
	got := collect.Decide(nil, read, first.Status, later).Status
	want := []collect.Scope{{ID: "web-1", LastReadAt: later}, {ID: "web-2", LastReadAt: t0, Unread: 2}}
	if !slices.Equal(got.Scopes, want) {
		t.Errorf("scopes = %+v, want %+v", got.Scopes, want)
	}
	if len(got.Unread) != 2 || got.Unread[0].Scope != "web-2" {
		t.Errorf("the findings lost their scope: %+v", got.Unread)
	}
}

// FINDINGS PAST THE BOUND ARE COUNTED, never dropped in silence, and a
// scope's count comes from the read rather than from the bounded findings.
func TestFindingsPastTheBoundAreCounted(t *testing.T) {
	t.Parallel()
	var read collect.Read
	for i := range collect.MaxUnread + 3 {
		id := "srv-" + string(rune('a'+i))
		read.Scopes = append(read.Scopes, id)
		read.Unread = append(read.Unread, collect.Unread{Part: id + " ports", Scope: id})
	}
	st := collect.Decide(nil, read, collect.Status{Kind: integration.KindSSH}, t0).Status
	if len(st.Unread) != collect.MaxUnread || st.UnreadOverflow != 3 {
		t.Errorf("kept %d findings and counted %d more, want %d and 3", len(st.Unread), st.UnreadOverflow,
			collect.MaxUnread)
	}
	for _, s := range st.Scopes {
		if s.Unread != 1 || !s.LastReadAt.IsZero() {
			t.Errorf("scope %s = %+v, want one refused part and no whole read", s.ID, s)
		}
	}
}

// THE REPORT SAYS EACH SOURCE IN ONE WORD, WHEN IT IS READ NEXT AND WHEN AN
// ASK IS HONOURED, and never carries the read's digest or the ids of what it
// stopped seeing.
func TestTheReportDescribesEverySourceTheCompanyConnects(t *testing.T) {
	t.Parallel()
	now := t0.Add(2 * time.Minute)
	standings := map[integration.Kind]collect.Status{
		integration.KindCloudflare: {LastAttemptAt: t0, Completed: true, LastCompleteAt: t0, Entries: 7, Calls: 4,
			Withheld: 1, Digest: "ab", Missing: []collect.Missing{{Since: t0, Entries: []string{"a", "b"}}},
			MissingOverflow: 1},
		integration.KindDatadog: {LastAttemptAt: t0, Completed: true, LastCompleteAt: t0,
			Unread: []collect.Finding{{Part: "monitors", Detail: "Grant monitors_read."}}},
		integration.KindGitHub: {LastAttemptAt: t0, LastCompleteAt: t0.Add(-time.Hour), LastError: "boom"},
	}
	asked := map[integration.Kind]collect.Request{
		integration.KindCloudflare: {RequestedAt: t0.Add(time.Minute), By: "ada"},
		integration.KindDatadog:    {RequestedAt: t0.Add(-time.Minute), By: "ada"},
	}
	kinds := []integration.Kind{integration.KindSSH, integration.KindGitHub, integration.KindDatadog,
		integration.KindCloudflare}
	got := collect.Describe(kinds, standings, asked, time.Hour, now)
	if got.RefreshIntervalSeconds != 3600 || got.RefreshFloorSeconds != 300 {
		t.Errorf("cadence = %d and %d, want 3600 and 300", got.RefreshIntervalSeconds, got.RefreshFloorSeconds)
	}
	states := map[integration.Kind]collect.State{}
	for _, s := range got.Sources {
		states[s.Kind] = s.State
		if !s.State.Valid() {
			t.Errorf("%s has the state %q", s.Kind, s.State)
		}
	}
	want := map[integration.Kind]collect.State{
		integration.KindCloudflare: collect.StateRead, integration.KindDatadog: collect.StatePartial,
		integration.KindGitHub: collect.StateFailed, integration.KindSSH: collect.StateNever,
	}
	for kind, state := range want {
		if states[kind] != state {
			t.Errorf("%s is %q, want %q", kind, states[kind], state)
		}
	}
	cf := got.Sources[0]
	if cf.Kind != integration.KindCloudflare || cf.Missing != 3 || cf.Withheld != 1 || cf.Calls != 4 {
		t.Errorf("cloudflare = %+v", cf)
	}
	if !cf.RefreshRequestedAt.Equal(t0.Add(time.Minute)) || cf.RefreshRequestedBy != "ada" ||
		!cf.RefreshableAt.Equal(t0.Add(collect.RefreshFloor)) || !cf.NextReadAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("cloudflare's ask and next read = %+v", cf)
	}
	if dd := got.Sources[1]; !dd.RefreshRequestedAt.IsZero() {
		t.Errorf("an ask made before the last read still waits: %+v", dd)
	}
	if gh := got.Sources[2]; !gh.NextReadAt.Equal(t0.Add(collect.RetryAfter)) || gh.LastError != "boom" {
		t.Errorf("a failed source is retried at %v with %q", gh.NextReadAt, gh.LastError)
	}
	if ssh := got.Sources[3]; !ssh.NextReadAt.IsZero() || !ssh.RefreshableAt.IsZero() || ssh.Unread == nil ||
		ssh.Scopes == nil {
		t.Errorf("a source never read = %+v, want it due now with empty lists", ssh)
	}
}
