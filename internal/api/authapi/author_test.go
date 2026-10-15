package authapi_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/iam"
)

// authors records whom every write the surface made on somebody's behalf was
// authored as, in order.
type authors struct {
	mu sync.Mutex
	by []iam.Principal
}

func (a *authors) record(o *authapi.Options) {
	writer := o.Writer
	o.Behalf = func(p iam.Principal) authapi.Writer {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.by = append(a.by, p)
		return writer
	}
}

// take answers what was recorded since the last take.
func (a *authors) take() []iam.Principal {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.by
	a.by = nil
	return out
}

// A GESTURE A PERSON MAKES HERE IS WRITTEN AS THEM.
//
// Every record this surface writes went out under the node's own writer, so
// the identity trail named the node as WHO for a sign-in, a second factor's
// spend, a sign-out and a reset link alike — it could say somebody had signed
// in and never who. Each is written on the person's behalf now: the person the
// credential they proved resolved to, and the session it opened or closed as
// the credential it came through. Mutation: write any one of them through the
// node's own writer again and its step records nobody.
func TestAGesturePersonMakesHereIsWrittenAsThem(t *testing.T) {
	t.Parallel()
	written := &authors{}
	r := newSignInRigWith(t, func(o *authapi.Options) {
		written.record(o)
	})
	expect := func(step string, got []iam.Principal, via ...string) {
		t.Helper()
		if len(got) != len(via) {
			t.Fatalf("%s wrote %d records on somebody's behalf, want %d: %+v",
				step, len(got), len(via), got)
		}
		for i, p := range got {
			if p.Login != "jane.doe" || p.Kind != iam.KindPerson ||
				p.ID.String() != r.estate.person.ID || p.Via != via[i] {
				t.Errorf("%s wrote as %+v, want jane.doe through %q", step, p, via[i])
			}
		}
	}

	if got := r.login(t, "jane.doe", password, appCode(t, clock)); got != http.StatusOK {
		t.Fatalf("the sign-in answered %d", got)
	}
	lineage := r.estate.starts[0].Lineage
	expect("a sign-in with an app code", written.take(), "",
		iam.SessionName(lineage))

	_, h := r, guarded(t, r)
	id, secret := withResetLink(t, r.estate, nil)
	if spent, _ := spendReset(t, h, id, secret, newPassword); spent.Code != http.StatusOK {
		t.Fatalf("the reset answered %d: %s", spent.Code, spent.Body)
	}
	expect("a reset link's spend", written.take(), "")
}

// A SIGN-OUT IS WRITTEN AS WHOEVER SIGNS OUT, THROUGH THE SESSION IT ENDS.
//
// The route is unguarded, so it names the person the guard resolved through
// that very session. Mutation: close it through the node's own writer and it
// records nobody.
func TestASignOutIsWrittenAsWhoeverSignsOut(t *testing.T) {
	t.Parallel()
	written := &authors{}
	r := newSignInRigWith(t, func(o *authapi.Options) {
		o.Sessions = o.Writer.(*estate)
		written.record(o)
	})
	passwordOnly(r.estate)
	h := guarded(t, r)
	cookie := signedIn(t, h)
	lineage := r.estate.starts[0].Lineage
	written.take()
	if rec, _ := send(t, h, http.MethodPost, "/auth/logout", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("the sign-out answered %d: %s", rec.Code, rec.Body)
	}
	got := written.take()
	if len(got) != 1 || got[0].Login != "jane.doe" ||
		got[0].Via != iam.SessionName(lineage) {
		t.Errorf("the sign-out wrote as %+v, want jane.doe through the session "+
			"it ended", got)
	}
}

// A REDEMPTION IS WRITTEN AS THE PERSON IT CREATES.
//
// The enrolment is the node's to authorise — the invitation is its authority,
// and nobody holds a grant before it lands — and the person's to have made:
// the trail named the node as WHO for every redemption. Then the session it
// opens. Mutation: enrol through the node's own writer and the first step
// records nobody.
func TestARedemptionIsWrittenAsThePersonItCreates(t *testing.T) {
	t.Parallel()
	written := &authors{}
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
		o.Directory = liveInvitation{}
		o.Writer = &recordingWriter{}
		written.record(o)
	}).Routes(mux)
	if got := redeem(t, mux, "dana.ops"); got != http.StatusOK {
		t.Fatalf("the redemption answered %d", got)
	}
	got := written.take()
	if len(got) != 2 || got[0].Login != "dana.ops" || got[0].Via != "" ||
		got[1].Login != "dana.ops" || got[0].ID != got[1].ID ||
		got[0].Kind != iam.KindPerson {
		t.Errorf("the redemption wrote as %+v, want dana.ops enrolled and then "+
			"signed in", got)
	}
}
