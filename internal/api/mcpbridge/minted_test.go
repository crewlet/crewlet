package mcpbridge_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
)

// Session ids as minting harnesses spell them: Kimi's are `session_<uuid>`.
const (
	mintedID  = "session_0b9c1d3e-5f70-4a8b-9c1d-2e3f4a5b6c7d"
	mintedID2 = "session_7d6c5b4a-3f2e-4d1c-8b9a-07f5e3d1c9b0"
)

// bindings records what [mcpbridge.Session.Bound] was told and which ids
// [mcpbridge.Session.Foreign] was asked of.
type bindings struct {
	mu            sync.Mutex
	bound, asked  []string
	foreign       map[string]bool
	foreignFailed error
}

// mintedFixture is a fixture whose session's harness mints its id, opened.
func mintedFixture(t *testing.T, continues string) (*fixture, *bindings, string) {
	t.Helper()
	f := newFixture(t)
	heard := &bindings{foreign: map[string]bool{}}
	f.session.Mints, f.session.Continues = true, continues
	f.session.Foreign = func(_ context.Context, id string) (bool, error) {
		heard.mu.Lock()
		defer heard.mu.Unlock()
		heard.asked = append(heard.asked, id)
		return heard.foreign[id], heard.foreignFailed
	}
	f.session.Bound = func(_ context.Context, id string) {
		heard.mu.Lock()
		defer heard.mu.Unlock()
		heard.bound = append(heard.bound, id)
	}
	url := f.open(t)
	if url == "" {
		t.Fatal("the minted session did not open")
	}
	return f, heard, url
}

func (h *bindings) told() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.bound...)
}

// failedClosed fails t unless the run's box can make no further call.
func failedClosed(t *testing.T, url string) {
	t.Helper()
	if res := callTool(t, dial(t, url), "read_page", map[string]any{}); !res.IsError {
		t.Errorf("a call after the refused report ran: %s", text(res))
	}
}

// A MINTED SESSION BINDS ONCE: the first report binds the launch's session to
// its id, and a later report, of the same id or another, is refused and closes
// the run's tools.
//
// Mutation: let a bound session bind again, or refuse without closing the
// tools, and this goes red.
func TestAMintedSessionBindsOnce(t *testing.T) {
	t.Parallel()
	for name, again := range map[string]string{"the same id": mintedID, "another id": mintedID2} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, heard, url := mintedFixture(t, "")
			if err := f.bridge.Bind(t.Context(), f.session, mintedID); err != nil {
				t.Fatalf("the first report was refused: %v", err)
			}
			if res := callTool(t, dial(t, url), "read_page", map[string]any{}); res.IsError {
				t.Fatalf("a call on the bound session failed: %s", text(res))
			}
			if err := f.bridge.Bind(t.Context(), f.session, again); !errors.Is(err, mcpbridge.ErrBindRefused) {
				t.Fatalf("a second report bound: %v", err)
			}
			if got := heard.told(); len(got) != 1 || got[0] != mintedID {
				t.Errorf("the session was told it bound %v, want %s once", got, mintedID)
			}
			failedClosed(t, url)
		})
	}
}

// A MINTED SESSION BINDS ONLY A FRESH ID: one already bound to another run,
// turn or seat is refused, as is one whose freshness cannot be told, and the
// run fails closed.
//
// Mutation: skip the freshness check, or bind where it failed, and this goes
// red.
func TestAMintedSessionRefusesAForeignID(t *testing.T) {
	t.Parallel()
	for name, failed := range map[string]error{"bound elsewhere": nil, "not told": errors.New("store unreachable")} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, heard, url := mintedFixture(t, "")
			heard.foreign[mintedID] = failed == nil
			heard.foreignFailed = failed
			if err := f.bridge.Bind(t.Context(), f.session, mintedID); !errors.Is(err, mcpbridge.ErrBindRefused) {
				t.Fatalf("a foreign id bound: %v", err)
			}
			if got := heard.told(); len(got) != 0 {
				t.Errorf("the session was told it bound %v", got)
			}
			failedClosed(t, url)
		})
	}
}

// A RESUME REPORTS NOTHING NEW: a box that took up the session its launch
// continued binds that id, which no freshness check is asked of since an
// earlier run holds it; a report of any other is refused and fails the run.
//
// Mutation: bind a resume's differing report, or ask the continued id's
// freshness, and this goes red.
func TestAResumeBindsOnlyTheSessionItContinued(t *testing.T) {
	t.Parallel()
	f, _, url := mintedFixture(t, mintedID)
	if err := f.bridge.Bind(t.Context(), f.session, mintedID2); !errors.Is(err, mcpbridge.ErrBindRefused) {
		t.Fatalf("a resume's differing report bound: %v", err)
	}
	failedClosed(t, url)

	g, heard, _ := mintedFixture(t, mintedID)
	heard.foreign[mintedID] = true
	if err := g.bridge.Bind(t.Context(), g.session, mintedID); err != nil {
		t.Fatalf("a resume's report of its own session was refused: %v", err)
	}
	if len(heard.asked) != 0 {
		t.Errorf("the continued session's freshness was asked: %v", heard.asked)
	}
}

// ONCE BOUND, THE RUN'S TOKEN RESOLVES ONLY TO THE BOUND SESSION: the token
// the box holds is kept and served, and the job's other launch-keyed token is
// refused, though the box never dialled before its report.
//
// Mutation: leave a bind ahead of the box's first request without settling
// which session the box started, and the other token is served.
func TestABoundSessionRefusesTheJobsOtherToken(t *testing.T) {
	t.Parallel()
	for name, continues := range map[string]string{"a fresh run": "", "a resume": mintedID} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, _, url := mintedFixture(t, continues)
			heard := &told{}
			f.session.Started = heard.hook(f.ledger)
			other := f.bridge.FreshEndpoint(f.session)
			if err := f.bridge.Bind(t.Context(), f.session, mintedID); err != nil {
				t.Fatalf("Bind: %v", err)
			}
			if status := postPingTo(t, f.server.Client(), other); status != http.StatusUnauthorized {
				t.Errorf("the job's other token answered %d once bound, want 401", status)
			}
			if res := callTool(t, dial(t, url), "read_page", map[string]any{}); res.IsError {
				t.Errorf("the run's own token was refused once bound: %s", text(res))
			}
			if said, _ := heard.words(); len(said) != 1 || said[0] {
				t.Errorf("the session was told the box started %v, want the launch's session once", said)
			}
		})
	}
}
