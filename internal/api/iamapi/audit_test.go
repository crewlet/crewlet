package iamapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// recordingAudit keeps every event the surface announced.
type recordingAudit struct {
	mu   sync.Mutex
	seen []events.Payload
}

func (a *recordingAudit) Emit(_ context.Context, payload events.Payload) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, payload)
}

func (a *recordingAudit) all() []events.Payload {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]events.Payload(nil), a.seen...)
}

// only is the one event a case expects, failing unless exactly one arrived.
func only[T events.Payload](t *testing.T, a *recordingAudit) T {
	t.Helper()
	seen := a.all()
	if len(seen) != 1 {
		t.Fatalf("announced %d events, want exactly one: %#v", len(seen), seen)
	}
	got, ok := seen[0].(T)
	if !ok {
		t.Fatalf("announced %T, want %T", seen[0], *new(T))
	}
	return got
}

// THE DIRECTORY ANNOUNCES WHAT IT CHANGED, AND ONLY WHAT LANDED.
//
// Each write below is one an investigation reads the live feed for — a token
// somebody minted, one withdrawn, a factor reset, sessions ended by somebody
// other than their holder, a person removed — and a write that did not land
// announces nothing, because the one false row in a trail is the one nobody
// can trust the rest after.
//
// Mutation: announce a revocation before the write returns and the refused
// write case goes red; name the caller's own sign-out everywhere a revocation
// and the self case does.
func TestTheDirectoryAnnouncesWhatItChanged(t *testing.T) {
	t.Parallel()

	t.Run("a minted token names what it carries", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		got := r.as(administrator(), http.MethodPost, "/iam/credentials",
			map[string]any{"label": "ci", "grants": []string{"state:read"}})
		if got.status != http.StatusCreated {
			t.Fatalf("mint answered %d: %v", got.status, got.body)
		}
		row := only[types.IAMCredentialMinted](t, r.audit)
		if row.Kind != types.CredentialToken || row.Owner != alice.String() ||
			len(row.Grants) != 1 || row.Grants[0] != "state:read" ||
			row.By != "founder" || row.Credential == "" || row.ExpiresAt.IsZero() {
			t.Errorf("minted row = %+v", row)
		}
		if row.Credential != got.body["id"] {
			t.Errorf("the row names credential %q and the answer %v", row.Credential, got.body["id"])
		}
	})

	t.Run("a revocation names the kind it withdrew", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.writer.held = []iamdomain.Credential{{ID: "c-1", Method: iamdomain.MethodToken}}
		got := r.as(administrator(), http.MethodDelete,
			"/iam/credentials/c-1?person="+alice.String(), nil)
		if got.status != http.StatusOK {
			t.Fatalf("revoke answered %d: %v", got.status, got.body)
		}
		row := only[types.IAMCredentialRevoked](t, r.audit)
		if row.Credential != "c-1" || row.Kind != types.CredentialToken || row.Owner != alice.String() {
			t.Errorf("revoked row = %+v", row)
		}
	})

	t.Run("revoking nothing announces nothing", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.as(administrator(), http.MethodDelete,
			"/iam/credentials/nope?person="+alice.String(), nil)
		if seen := r.audit.all(); len(seen) != 0 {
			t.Errorf("a revocation that found no credential announced %v", seen)
		}
	})

	// THE LAST ROUND'S VERDICT, not the first's. A decide runs again
	// against a fresh snapshot after losing a race, and a credential
	// another writer revoked in between is one THIS write did not revoke.
	t.Run("a revocation that lost its race announces nothing", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.writer.held = []iamdomain.Credential{{ID: "c-1", Method: iamdomain.MethodToken}}
		r.writer.rerun = []iamdomain.Credential{{ID: "c-1", Method: iamdomain.MethodToken,
			RevokedAt: at}}
		r.as(administrator(), http.MethodDelete,
			"/iam/credentials/c-1?person="+alice.String(), nil)
		if seen := r.audit.all(); len(seen) != 0 {
			t.Errorf("a revocation somebody else landed first was announced "+
				"as this one's: %v", seen)
		}
	})

	t.Run("a reset second factor", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		got := r.as(administrator(), http.MethodPost,
			"/iam/people/"+bob.String()+"/mfa/reset", nil)
		if got.status != http.StatusOK {
			t.Fatalf("reset answered %d: %v", got.status, got.body)
		}
		row := only[types.IAMMFAReset](t, r.audit)
		if row.Person != bob.String() || row.By != "founder" {
			t.Errorf("reset row = %+v", row)
		}
	})

	t.Run("a removal ends every session", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.as(administrator(), http.MethodDelete, "/iam/people/"+bob.String(), nil)
		row := only[types.IAMSessionEnded](t, r.audit)
		if row.Reason != types.EndPersonRemoved || row.Person != bob.String() || row.Lineage != "" {
			t.Errorf("removal row = %+v", row)
		}
	})

	t.Run("somebody else's sessions are revoked", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.as(administrator(), http.MethodDelete, "/iam/people/"+bob.String()+"/sessions", nil)
		if row := only[types.IAMSessionEnded](t, r.audit); row.Reason != types.EndRevoked {
			t.Errorf("an administrator ending bob's sessions read %q, want revoked", row.Reason)
		}
	})

	t.Run("your own are a sign-out everywhere", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.as(ordinary(), http.MethodDelete, "/iam/people/"+bob.String()+"/sessions", nil)
		if row := only[types.IAMSessionEnded](t, r.audit); row.Reason != types.EndLogoutAll {
			t.Errorf("bob ending his own sessions read %q, want logout_all", row.Reason)
		}
	})

	t.Run("a write that did not land announces nothing", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.writer.err = errors.New("the log is unreachable")
		r.as(administrator(), http.MethodDelete, "/iam/people/"+bob.String()+"/sessions", nil)
		r.as(administrator(), http.MethodDelete, "/iam/people/"+bob.String(), nil)
		if seen := r.audit.all(); len(seen) != 0 {
			t.Errorf("writes that failed announced %v", seen)
		}
	})
}

// THE SURFACE IS REFUSED WITHOUT A TRAIL, by name, rather than built to
// announce nothing.
func TestTheDirectoryNeedsATrail(t *testing.T) {
	t.Parallel()
	_, err := iamapi.New(iamapi.Options{
		Directory: &fakeDirectory{},
		Authority: func(iam.Actor, iam.Kind, []iam.Grant) iamapi.Writer {
			return &fakeWriter{}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "audit") {
		t.Fatalf("building without a trail answered %v, want a refusal naming it", err)
	}
}
