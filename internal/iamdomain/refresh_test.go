package iamdomain_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DEACTIVATION PROBE, END TO END OVER THIS ESTATE.
//
// internal/iam/oidc certifies the probe's three verdicts against a fake. What
// only this estate can certify is what each one DOES here: an `invalid_grant`
// is a close record on the session's subject, reason `idp_revoked`, written by
// the node and applied like any other; every other answer ends nothing; a
// rotated token is recorded in custody so the next pass presents it. And the
// pass is also what collects custody for sessions that ended some other way —
// without ever collecting one this node simply has not applied yet.
func TestTheProbeEndsRevokesRotatesAndCollectsThroughTheEstate(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	custody, err := iamdomain.NewRefreshes(rig.keys, "node-a")
	if err != nil {
		t.Fatalf("NewRefreshes: %v", err)
	}
	idp := newRefreshIssuer(t)

	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "Sarah Chen", Email: "sarah.chen@example.com",
		Login: "sarah.chen", OpID: "enrol-sarah", Reason: "a joiner",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	later := time.Now().Add(24 * time.Hour)
	hold := func(token, issuer string) string {
		t.Helper()
		lineage, at := rig.openSessionAt(person, later)
		if err := custody.Hold(t.Context(), iamdomain.RefreshGrant{
			Lineage: lineage, Person: person, Issuer: issuer, Token: token,
			Start: uint64(at.Packed()),
		}, time.Now()); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		return lineage
	}
	deactivated := hold("gone", idp.URL)
	rotating := hold("fine", idp.URL)
	flaky := hold("flaky", idp.URL)
	elsewhere := hold("previous-provider-token", "https://previous-provider.example.com")
	signedOut := hold("gone", idp.URL)
	rig.closeSession(signedOut, "logout")

	// A GRANT WHOSE SESSION THIS NODE HAS NOT APPLIED: its start is past
	// everything applied, which is what a login landing on a peer an
	// instant ago looks like from here.
	unseen := uuid.Must(uuid.NewV7()).String()
	if err := custody.Hold(t.Context(), iamdomain.RefreshGrant{
		Lineage: unseen, Person: person, Issuer: idp.URL, Token: "gone",
		Start: 1 << 50,
	}, time.Now()); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	sessions, err := iamdomain.NewProbeSessions(reader, rig.writer.As("node-a",
		iam.KindMachine, []iam.Grant{iam.GrantFleetOperate}), custody, idp.URL, nil)
	if err != nil {
		t.Fatalf("NewProbeSessions: %v", err)
	}
	prober := oidc.NewProber(oidc.NewProvider(oidc.Config{
		Issuer: idp.URL, ClientID: "crewlet", ClientSecret: "not-a-real-secret",
		RedirectURI: "https://crewlet.example.com/auth/oidc/callback",
	}, idp.Client(), nil), sessions, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var checked, ended int
	if err := rig.during(func() error {
		var err error
		checked, ended, err = prober.Run(t.Context())
		return err
	}); err != nil {
		t.Fatalf("the pass failed: %v", err)
	}
	if checked != 3 || ended != 1 {
		t.Fatalf("the pass checked %d and ended %d, want the three live sessions "+
			"of this provider checked and the deactivated one ended", checked, ended)
	}

	// INVALID_GRANT: a close record, reason idp_revoked, and no custody.
	if why := rig.column(`SELECT ended_reason FROM iam_sessions
		WHERE lineage = ? AND ended_at > 0`, deactivated); len(why) != 1 ||
		why[0] != oidc.ReasonIdPRevoked {
		t.Errorf("the deactivated session reads %v, want ended as %s", why,
			oidc.ReasonIdPRevoked)
	}
	// EVERY OTHER ANSWER ENDS NOTHING.
	for _, lineage := range []string{rotating, flaky} {
		if ended := rig.column(`SELECT lineage FROM iam_sessions
			WHERE lineage = ? AND ended_at > 0`, lineage); len(ended) != 0 {
			t.Errorf("session %s ended on an answer that was not invalid_grant", lineage)
		}
	}

	held := map[string]string{}
	grants, err := custody.Held(t.Context())
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	for _, g := range grants {
		held[g.Lineage] = g.Token
	}
	// A ROTATION IS RECORDED.
	if held[rotating] != "fine-2" {
		t.Errorf("the rotated session's grant reads %q, want the provider's "+
			"replacement — the next pass would present a retired token and end "+
			"the session of somebody perfectly employed", held[rotating])
	}
	// COLLECTED: the deactivated one (ended here) and the signed-out one
	// (ended elsewhere, found over by the pass).
	for _, lineage := range []string{deactivated, signedOut} {
		if _, kept := held[lineage]; kept {
			t.Errorf("custody still holds %s's refresh token after its session "+
				"ended", lineage)
		}
	}
	// KEPT: the unknown answer, another provider's token, and a session
	// this node has not applied yet.
	for name, lineage := range map[string]string{
		"an unknown answer's":         flaky,
		"another provider's":          elsewhere,
		"a not-yet-applied session's": unseen,
	} {
		if _, kept := held[lineage]; !kept {
			t.Errorf("%s grant was collected", name)
		}
	}
	if idp.asked("previous-provider-token") {
		t.Error("another provider's token was presented to this one")
	}
}

// A SESSION IS OVER THE MOMENT NOTHING CAN PRESENT IT, whichever lever ended
// it — and only one of those levers writes to the session's own row.
//
// A sign-out closes the row. A revocation moves the person's epoch, a
// company-wide invalidation moves the generation, and an absolute deadline
// simply passes; none of those touches the session, so a liveness that read
// only `ended_at` would keep asking a provider about sessions nobody can use
// and never collect their tokens.
func TestASessionIsOverWhicheverLeverEndedIt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	enrol := func(login string) string {
		t.Helper()
		id := uuid.Must(uuid.NewV7()).String()
		if err := rig.enrol(iamdomain.Enrolment{
			PersonID: id, Kind: iam.KindPerson, Stage: iam.StageActive,
			Name: login, Email: login + "@example.com", Login: login,
			OpID: "enrol-" + login, Reason: "a joiner",
		}); err != nil {
			t.Fatalf("enrol %s: %v", login, err)
		}
		return id
	}
	revoked, kept := enrol("sam.revoked"), enrol("kim.kept")
	later := time.Now().Add(24 * time.Hour)
	grant := func(person string, expires time.Time) iamdomain.RefreshGrant {
		lineage, at := rig.openSessionAt(person, expires)
		return iamdomain.RefreshGrant{Lineage: lineage, Person: person,
			Start: uint64(at.Packed())}
	}
	live := grant(kept, later)
	lapsed := grant(kept, time.Now().Add(time.Minute))
	byEpoch := grant(revoked, later)
	if err := rig.during(func() error {
		_, err := rig.writer.Revoke(t.Context(), revoked, "revoke-sam", "left")
		return err
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	states, err := reader.SessionStates(t.Context(),
		[]iamdomain.RefreshGrant{live, lapsed, byEpoch}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SessionStates: %v", err)
	}
	for _, tc := range []struct {
		name  string
		grant iamdomain.RefreshGrant
		want  iamdomain.SessionState
	}{
		{"an open session", live, iamdomain.SessionLive},
		{"a session past its absolute deadline", lapsed, iamdomain.SessionOver},
		{"a revoked person's session", byEpoch, iamdomain.SessionOver},
	} {
		if got := states[tc.grant.Lineage]; got != tc.want {
			t.Errorf("%s is %q, want %q", tc.name, got, tc.want)
		}
	}

	// AND THE COMPANY-WIDE LEVER: every session opened before it is over,
	// and one opened after it is not.
	if err := rig.during(func() error {
		_, err := rig.writer.InvalidateAll(t.Context(), "invalidate-all",
			"a restore")
		return err
	}); err != nil {
		t.Fatalf("InvalidateAll: %v", err)
	}
	after := grant(kept, later)
	states, err = reader.SessionStates(t.Context(),
		[]iamdomain.RefreshGrant{live, after}, time.Now())
	if err != nil {
		t.Fatalf("SessionStates: %v", err)
	}
	if states[live.Lineage] != iamdomain.SessionOver {
		t.Errorf("a session opened before the invalidation is %q, want over",
			states[live.Lineage])
	}
	if states[after.Lineage] != iamdomain.SessionLive {
		t.Errorf("a session opened after the invalidation is %q, want live",
			states[after.Lineage])
	}
}

// --- a provider whose answer depends on the token presented --------------- //

type refreshIssuer struct {
	*httptest.Server
	mu        sync.Mutex
	presented map[string]bool
}

// newRefreshIssuer answers a refresh by the token presented: "gone" is
// invalid_grant, "flaky" is a provider having a bad minute, "fine" rotates.
func newRefreshIssuer(t *testing.T) *refreshIssuer {
	t.Helper()
	i := &refreshIssuer{presented: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc(oidc.MetadataPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 i.URL,
			"authorization_endpoint": i.URL + "/authorize",
			"token_endpoint":         i.URL + "/token",
			"jwks_uri":               i.URL + "/jwks",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		token := r.Form.Get("refresh_token")
		i.mu.Lock()
		i.presented[token] = true
		i.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch token {
		case "gone":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		case "flaky":
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "temporarily_unavailable"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"refresh_token": token + "-2", "token_type": "Bearer",
			})
		}
	})
	i.Server = httptest.NewTLSServer(mux)
	t.Cleanup(i.Close)
	return i
}

// asked reports whether any request presented this token.
func (i *refreshIssuer) asked(token string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.presented[token]
}

// openSessionAt opens one session and returns where its record landed.
func (r *writeRig) openSessionAt(person string, expires time.Time) (string,
	statelog.Position) {

	r.t.Helper()
	lineage := uuid.Must(uuid.NewV7()).String()
	var at statelog.Position
	if err := r.during(func() error {
		var err error
		at, err = r.writer.OpenSession(r.t.Context(), iamdomain.SessionStart{
			Lineage: lineage, Person: person, AbsoluteExpiresAt: expires,
			OpID: "session:" + lineage,
		})
		return err
	}); err != nil {
		r.t.Fatalf("open a session: %v", err)
	}
	return lineage, at
}
