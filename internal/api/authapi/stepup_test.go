package authapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// stepUpRig is the sign-in rig with one live session presented: a cookie the
// fixture's signer minted, validated against rows that hold it live.
type stepUpRig struct {
	*signInRig
	lineage  uuid.UUID
	cookie   string
	absolute time.Time
	carried  []iam.Grant
}

func newStepUpRig(t *testing.T, row session.Row) *stepUpRig {
	t.Helper()
	absolute := clock.Add(72 * time.Hour)
	carried := []iam.Grant{iam.GrantWorkWrite}
	identity := session.Identity{
		Applied: ^uint64(0) >> 1, Generation: 2,
		Session: session.SessionRow{Found: true, Epoch: 3,
			ProvedAt: clock.Add(-2 * time.Hour), GroupGrants: carried},
		Person: session.PersonRow{Found: true, Epoch: 3,
			Stage: iam.StageActive, Login: "jane.doe"},
	}
	if row == session.RowEnded {
		identity.Session.Ended = true
	}
	r := newSignInRigWith(t, func(o *authapi.Options) {
		o.Sessions = rows{identity}
	})
	lineage := uuid.Must(uuid.NewV7())
	millis := clock.Add(-2 * time.Hour).UnixMilli()
	for i := range 6 {
		lineage[i] = byte(millis >> (8 * (5 - i)))
	}
	cookie, err := fixtureSigner(t).Mint(session.Mint{
		Lineage: lineage, Person: r.estate.person.ID, StartPosition: 1,
		Epoch: 3, Generation: 2, AbsoluteExpiresAt: absolute,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return &stepUpRig{signInRig: r, lineage: lineage, cookie: cookie,
		absolute: absolute, carried: carried}
}

// stepUp posts one confirmation from the presented session.
func (r *stepUpRig) stepUp(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"password": password, "code": appCode(t, clock),
	})
	req := httptest.NewRequest(http.MethodPost, "/auth/step-up",
		strings.NewReader(string(body)))
	req.RemoteAddr = "203.0.113.9:4711"
	req.AddCookie(&http.Cookie{Name: session.CookieBaseName, Value: r.cookie})
	req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
		ID:    uuid.MustParse(r.estate.person.ID),
		Login: "jane.doe", Kind: iam.KindPerson, Stage: iam.StageActive,
	}))
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	r.svc.Routes(mux)
	mux.ServeHTTP(rec, req)
	return rec
}

// A STEP-UP ENDS THE SESSION IT REPLACES, AND CONFIRMS RATHER THAN REPEATS IT.
//
// The proof opens a fresh session. It used to leave the one it was made from
// running, so every confirmation left a second live session behind and a copy
// of the old cookie went on working for the rest of its week. The replacement
// keeps the replaced session's absolute deadline — or confirming a session
// would keep it alive for ever — and the grants its identity provider's groups
// conferred, or stepping up would cost a person the authority they stepped up
// to use.
func TestAStepUpEndsTheSessionItReplaces(t *testing.T) {
	t.Parallel()
	r := newStepUpRig(t, session.RowValid)
	rec := r.stepUp(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("the step-up answered %d (%s)", rec.Code, rec.Body)
	}
	r.estate.mu.Lock()
	closes, starts := slices.Clone(r.estate.closes), slices.Clone(r.estate.starts)
	r.estate.mu.Unlock()
	if len(closes) != 1 || closes[0].lineage != r.lineage.String() ||
		closes[0].person != r.estate.person.ID {
		t.Errorf("ended %+v, want exactly the replaced session %s", closes, r.lineage)
	}
	if len(starts) != 1 {
		t.Fatalf("opened %d sessions, want the one replacement", len(starts))
	}
	opened := starts[0]
	if opened.Lineage == r.lineage.String() {
		t.Error("the replacement reuses the lineage it replaces")
	}
	if !opened.AbsoluteExpiresAt.Equal(r.absolute) {
		t.Errorf("the replacement ends at %s, want the replaced session's %s",
			opened.AbsoluteExpiresAt, r.absolute)
	}
	if !slices.Equal(opened.GroupGrants, r.carried) {
		t.Errorf("the replacement carries %v, want the replaced session's %v",
			opened.GroupGrants, r.carried)
	}
	if !opened.ProvedAt.Equal(clock) {
		t.Errorf("the replacement was proved at %s, want now", opened.ProvedAt)
	}
	emitted, _ := r.audit.snapshot()
	var announced bool
	for _, e := range emitted {
		if up, ok := e.(types.IAMStepUpCompleted); ok {
			announced = up.Replaces == r.lineage.String() &&
				up.Lineage == opened.Lineage
		}
	}
	if !announced {
		t.Errorf("announced %#v, want a step-up joining the two lineages", emitted)
	}
}

// A STEP-UP THAT CANNOT END WHAT IT REPLACES CHANGES NOTHING.
//
// The replaced session ends FIRST, so a close that does not land fails the
// gesture before anything opens: the other order would leave the second live
// session this exists to prevent. The refusal is the unknown arm, carrying a
// Retry-After, because the person's proof was good and the node could not
// record the consequence. And a presented session that is no longer live is
// refused outright, opening and ending nothing.
func TestAStepUpThatCannotEndWhatItReplacesChangesNothing(t *testing.T) {
	t.Parallel()
	t.Run("the close does not land", func(t *testing.T) {
		t.Parallel()
		r := newStepUpRig(t, session.RowValid)
		r.estate.closeErr = errors.New("the broker is unreachable")
		rec := r.stepUp(t)
		if rec.Code != http.StatusServiceUnavailable ||
			rec.Header().Get("Retry-After") == "" {
			t.Errorf("answered %d with Retry-After %q, want 503 carrying one",
				rec.Code, rec.Header().Get("Retry-After"))
		}
		if len(r.estate.starts) != 0 {
			t.Errorf("opened %+v beside a session it could not end", r.estate.starts)
		}
	})
	t.Run("the presented session has ended", func(t *testing.T) {
		t.Parallel()
		r := newStepUpRig(t, session.RowEnded)
		rec := r.stepUp(t)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("answered %d, want 401", rec.Code)
		}
		if len(r.estate.starts) != 0 || len(r.estate.closes) != 0 {
			t.Errorf("opened %+v and ended %+v on a session that is over",
				r.estate.starts, r.estate.closes)
		}
	})
}
