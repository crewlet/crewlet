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
	"github.com/crewlet/crewlet/internal/api/httpjson"
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
}

func newStepUpRig(t *testing.T, row session.Row) *stepUpRig {
	t.Helper()
	return newStepUpRigWith(t, row, nil, false)
}

// newStepUpRigWith is [newStepUpRig] with further fakes replaced, and — where
// restricted — a presented session that may only enrol a second factor,
// marked as a sign-in marks one: on its bearer and on its row.
func newStepUpRigWith(t *testing.T, row session.Row,
	replace func(*authapi.Options), restricted bool) *stepUpRig {

	t.Helper()
	absolute := clock.Add(72 * time.Hour)
	identity := session.Identity{
		Applied: ^uint64(0) >> 1, Generation: 2,
		Session: session.LineageRow{Found: true, Epoch: 3,
			ProvedAt: clock.Add(-2 * time.Hour), EnrolmentOnly: restricted},
		Person: session.PersonRow{Found: true, Epoch: 3,
			Stage: iam.StageActive, Login: "jane.doe"},
	}
	if row == session.RowEnded {
		identity.Session.Ended = true
	}
	r := newSignInRigWith(t, func(o *authapi.Options) {
		o.Sessions = rows{identity}
		if replace != nil {
			replace(o)
		}
	})
	lineage := uuid.Must(uuid.NewV7())
	millis := clock.Add(-2 * time.Hour).UnixMilli()
	for i := range 6 {
		lineage[i] = byte(millis >> (8 * (5 - i)))
	}
	cookie, err := fixtureSigner(t).Mint(session.Mint{
		Lineage: lineage, Person: r.estate.person.ID, StartPosition: 1,
		Epoch: 3, Generation: 2, AbsoluteExpiresAt: absolute,
		EnrolmentOnly: restricted,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return &stepUpRig{signInRig: r, lineage: lineage, cookie: cookie,
		absolute: absolute}
}

// stepUp posts one confirmation from the presented session.
func (r *stepUpRig) stepUp(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return r.stepUpWith(t, appCode(t, clock))
}

// stepUpWith is [stepUpRig.stepUp] presenting code, or none where it is empty.
func (r *stepUpRig) stepUpWith(t *testing.T, code string) *httptest.ResponseRecorder {
	t.Helper()
	fields := map[string]string{"password": password}
	if code != "" {
		fields["code"] = code
	}
	body, _ := json.Marshal(fields)
	req := httptest.NewRequest(http.MethodPost, "/auth/step-up",
		strings.NewReader(string(body)))
	req.RemoteAddr = "203.0.113.9:4711"
	req.AddCookie(&http.Cookie{Name: session.HostCookieName, Value: r.cookie})
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
// keeps the replaced session's absolute deadline, or confirming a session
// would keep it alive for ever.
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

// A STEP-UP WITHOUT THE CODE ITS PERSON HOLDS ASKS FOR IT.
//
// The sign-in answers a right password with no code `second_factor_required`,
// and a step-up is the same proof: it checked the empty code as a wrong one,
// so a person whose password was right was told their sign-in details were
// not accepted. Asked for, it is neither a failure nor a session. The CONTROL
// is a wrong code, which is refused and counted. Mutation: drop the empty-code
// arm and the first case answers `sign_in_refused`.
func TestAStepUpWithoutTheCodeItsPersonHoldsAsksForIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		code    string
		want    httpjson.Code
		counted bool
	}{
		{"no code", "", httpjson.CodeSecondFactorRequired, false},
		{"a wrong code (the control)", "000000", httpjson.CodeSignInRefused, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newStepUpRig(t, session.RowValid)
			rec := r.stepUpWith(t, tc.code)
			if rec.Code != http.StatusUnauthorized || codeOf(t, rec) != string(tc.want) {
				t.Fatalf("the step-up answered %d %s, want 401 %s", rec.Code,
					rec.Body, tc.want)
			}
			r.estate.mu.Lock()
			opened := len(r.estate.starts)
			r.estate.mu.Unlock()
			if opened != 0 {
				t.Errorf("opened %d sessions, want none", opened)
			}
			if _, failures := r.audit.snapshot(); (len(failures) != 0) != tc.counted {
				t.Errorf("counted %v, want counted %v", failures, tc.counted)
			}
		})
	}
}

// A SESSION THAT REPLACES ANOTHER IS OPENED ON THIS NODE BEFORE IT IS ANSWERED.
//
// A sign-in opens its session without waiting for this node to apply it,
// since nothing reads the row before the bearer does. A step-up, a password
// change and an enrolment are made from a page that lists the person's
// sessions and reads them again at once — and read before the start applied,
// the list held the session the gesture ended and not the one it opened, so
// the page said this browser was signed in nowhere. The CONTROL is the
// sign-in, which still does not wait. Mutation: open every session without
// waiting and the step-up's does too.
func TestASessionThatReplacesAnotherWaitsForThisNode(t *testing.T) {
	t.Parallel()
	signIn := newSignInRig(t)
	if rec := signIn.signIn(t, "jane.doe", password, appCode(t, clock)); rec.Code != http.StatusOK {
		t.Fatalf("the sign-in answered %d (%s)", rec.Code, rec.Body)
	}
	stepUp := newStepUpRig(t, session.RowValid)
	if rec := stepUp.stepUp(t); rec.Code != http.StatusOK {
		t.Fatalf("the step-up answered %d (%s)", rec.Code, rec.Body)
	}
	for _, tc := range []struct {
		name   string
		rig    *signInRig
		noWait bool
	}{
		{"a sign-in (the control)", signIn, true},
		{"a step-up", stepUp.signInRig, false},
	} {
		tc.rig.estate.mu.Lock()
		starts := slices.Clone(tc.rig.estate.starts)
		tc.rig.estate.mu.Unlock()
		if len(starts) != 1 || starts[0].NoWait != tc.noWait {
			t.Errorf("%s opened %+v, want one session with NoWait %v", tc.name,
				starts, tc.noWait)
		}
	}
}
