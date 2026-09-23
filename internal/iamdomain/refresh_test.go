package iamdomain_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
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
// probe COLLECTS NOTHING it did not end itself: a session that ended some other
// way is the key duty's to collect ([Refreshes.CollectEnded]), because that duty
// runs whether or not a provider is configured — and it collects without ever
// taking one this node simply has not applied yet.
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
	rig.closeSession(person, signedOut, "logout")

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

	sessions, err := iamdomain.NewProbeSessions(reader, rig.writer.As(principalNamed("node-a",
		iam.KindMachine, []iam.Grant{iam.GrantFleetOperate})), custody, idp.URL, nil)
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
		var pass oidc.Pass
		pass, err = prober.Run(t.Context())
		checked, ended = pass.Checked, pass.Ended
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
	grants, unreadable, err := custody.Held(t.Context())
	if err != nil || len(unreadable) != 0 {
		t.Fatalf("Held: %v %v", err, unreadable)
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
	// DROPPED BY THE PROBE: only the session it ended itself.
	if _, kept := held[deactivated]; kept {
		t.Error("custody still holds the deactivated session's refresh token " +
			"after the probe ended it")
	}
	// KEPT BY THE PROBE: the unknown answer, another provider's token, a
	// session this node has not applied yet, and the signed-out one — which
	// the key duty collects below, whether or not a probe runs at all.
	for name, lineage := range map[string]string{
		"an unknown answer's":         flaky,
		"another provider's":          elsewhere,
		"a not-yet-applied session's": unseen,
		"a signed-out session's":      signedOut,
	} {
		if _, kept := held[lineage]; !kept {
			t.Errorf("the probe collected %s grant", name)
		}
	}
	if idp.asked("previous-provider-token") {
		t.Error("another provider's token was presented to this one")
	}

	// THE KEY DUTY'S COLLECTION: the signed-out session's grant, and not
	// one this node has not applied.
	collected, skipped, err := custody.CollectEnded(t.Context(), reader, time.Now())
	if err != nil || len(skipped) != 0 {
		t.Fatalf("CollectEnded: %v %v", err, skipped)
	}
	if len(collected) != 1 || collected[0] != signedOut {
		t.Errorf("the collection took %v, want exactly the signed-out "+
			"session's grant", collected)
	}
}

// A GRANT OUTLIVES NO SESSION, WITH OR WITHOUT A PROVIDER.
//
// Collecting the refresh token of a session that is over used to be the
// deactivation probe's — which is armed only while an `oidc` block is
// configured, so a deployment that dropped its provider kept every session's
// token, a live credential at that provider, for ever. The collection is the
// key duty's now, and this case builds no provider and no probe at all: a
// sign-out and a revocation are collected, and an open session and one this
// node has not applied are kept.
func TestAnEndedSessionsGrantIsCollectedWithNoProviderConfigured(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	custody, err := iamdomain.NewRefreshes(rig.keys, "node-a")
	if err != nil {
		t.Fatalf("NewRefreshes: %v", err)
	}
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
	kept, revoked := enrol("kim.kept"), enrol("sam.revoked")
	later := time.Now().Add(24 * time.Hour)
	hold := func(person string) string {
		t.Helper()
		lineage, at := rig.openSessionAt(person, later)
		if err := custody.Hold(t.Context(), iamdomain.RefreshGrant{
			Lineage: lineage, Person: person,
			Issuer: "https://idp.example.com", Token: "refresh-" + lineage,
			Start: uint64(at.Packed()),
		}, time.Now()); err != nil {
			t.Fatalf("Hold: %v", err)
		}
		return lineage
	}
	live := hold(kept)
	signedOut := hold(kept)
	rig.closeSession(kept, signedOut, "logout")
	byEpoch := hold(revoked)
	if err := rig.during(func() error {
		_, err := rig.writer.Revoke(t.Context(), revoked, "revoke-sam", "left")
		return err
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	unseen := uuid.Must(uuid.NewV7()).String()
	if err := custody.Hold(t.Context(), iamdomain.RefreshGrant{
		Lineage: unseen, Person: kept, Issuer: "https://idp.example.com",
		Token: "refresh-unseen", Start: 1 << 50,
	}, time.Now()); err != nil {
		t.Fatalf("Hold: %v", err)
	}

	collected, skipped, err := custody.CollectEnded(t.Context(), reader, time.Now())
	if err != nil || len(skipped) != 0 {
		t.Fatalf("CollectEnded: %v %v", err, skipped)
	}
	slices.Sort(collected)
	want := []string{signedOut, byEpoch}
	slices.Sort(want)
	if !slices.Equal(collected, want) {
		t.Errorf("the collection took %v, want the signed-out and the revoked "+
			"sessions' grants %v", collected, want)
	}
	grants, _, err := custody.Held(t.Context())
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	left := map[string]bool{}
	for _, g := range grants {
		left[g.Lineage] = true
	}
	if !left[live] || !left[unseen] || len(left) != 2 {
		t.Errorf("custody holds %v after the collection, want exactly the open "+
			"session's and the unapplied one's", left)
	}
}

// ONE GRANT THIS BUILD CANNOT READ DOES NOT STOP THE PROBE.
//
// Custody is one row per session, so a grant a newer peer wrote during a
// rolling upgrade — or one whose name and content disagree — is one session
// the probe cannot ask about, and says nothing about the rest. The listing used
// to join that failure into its error and the probe discarded the whole list,
// so a deactivated person beside it kept their session until its absolute
// deadline, pass after pass, with a warning as the only sign.
func TestAnUnreadableGrantDoesNotStopTheProbe(t *testing.T) {
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
	lineage, at := rig.openSessionAt(person, time.Now().Add(24*time.Hour))
	if err := custody.Hold(t.Context(), iamdomain.RefreshGrant{
		Lineage: lineage, Person: person, Issuer: idp.URL, Token: "gone",
		Start: uint64(at.Packed()),
	}, time.Now()); err != nil {
		t.Fatalf("Hold: %v", err)
	}
	// A GRANT FROM A NEWER BUILD, filed under a session of its own.
	rig.keys.put(iamdomain.SessionRefreshName("other"),
		`{"v":2,"lineage":"other","person":"somebody"}`)

	sessions, err := iamdomain.NewProbeSessions(reader, rig.writer, custody,
		idp.URL, nil)
	if err != nil {
		t.Fatalf("NewProbeSessions: %v", err)
	}
	prober := oidc.NewProber(oidc.NewProvider(oidc.Config{
		Issuer: idp.URL, ClientID: "crewlet", ClientSecret: "not-a-real-secret",
		RedirectURI: "https://crewlet.example.com/auth/oidc/callback",
	}, idp.Client(), nil), sessions, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var pass oidc.Pass
	if err := rig.during(func() error {
		var err error
		pass, err = prober.Run(t.Context())
		return err
	}); err != nil {
		t.Fatalf("one unreadable grant failed the whole pass: %v", err)
	}
	if pass.Checked != 1 || pass.Ended != 1 || pass.Skipped != 1 {
		t.Fatalf("the pass read %+v, want the readable session checked and "+
			"ended and the unreadable grant counted as skipped", pass)
	}
	if why := rig.column(`SELECT ended_reason FROM iam_sessions
		WHERE lineage = ? AND ended_at > 0`, lineage); len(why) != 1 ||
		why[0] != oidc.ReasonIdPRevoked {
		t.Errorf("the deactivated session reads %v, want ended as %s", why,
			oidc.ReasonIdPRevoked)
	}
	// AND THE UNREADABLE GRANT IS LEFT ALONE: it may be a newer peer's, and
	// a build that cannot read it has no business deleting it.
	if rig.keys.value(iamdomain.SessionRefreshName("other")) == "" {
		t.Error("the probe deleted a grant it could not read")
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
		opened, err := r.writer.OpenSession(r.t.Context(), iamdomain.SessionStart{
			Lineage: lineage, Person: person, AbsoluteExpiresAt: expires,
			OpID: "session:" + lineage,
		})
		at = opened.Result.Position
		return err
	}); err != nil {
		r.t.Fatalf("open a session: %v", err)
	}
	return lineage, at
}

// A NAMED SESSION'S STANDING IS ITS OWNER AND WHETHER IT IS STILL LIVE, and
// the second half reads every lever the probe reads.
//
// Ending a named session asked only whose it was, so a session a record had
// already ended was closed and announced again. Its owner stays readable after
// it is over — the owner check is what refuses somebody else's lineage, and it
// must answer the same for a session that ended — and only an absent row names
// nobody.
func TestASessionsStandingIsItsOwnerAndWhetherItIsLive(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	reader := rig.reader(t)
	person := uuid.Must(uuid.NewV7()).String()
	if err := rig.enrol(iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: "kim.kept", Email: "kim.kept@example.com", Login: "kim.kept",
		OpID: "enrol-kim", Reason: "a joiner",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	later := time.Now().Add(24 * time.Hour)
	open, _ := rig.openSessionAt(person, later)
	closed, _ := rig.openSessionAt(person, later)
	lapsed, _ := rig.openSessionAt(person, time.Now().Add(time.Minute))
	if err := rig.during(func() error {
		_, err := rig.writer.CloseSession(t.Context(), closed, person,
			"signed out", "close-"+closed)
		return err
	}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	for _, tc := range []struct {
		name, lineage, owner string
		live                 bool
	}{
		{"an open session", open, person, true},
		{"a session a record ended", closed, person, false},
		{"a session past its absolute deadline", lapsed, person, false},
		{"a lineage nobody holds", uuid.Must(uuid.NewV7()).String(), "", false},
	} {
		owner, live, err := reader.SessionStanding(t.Context(), tc.lineage,
			time.Now().Add(time.Hour))
		if err != nil {
			t.Fatalf("%s: SessionStanding: %v", tc.name, err)
		}
		if owner != tc.owner || live != tc.live {
			t.Errorf("%s stands as (%q, live %v), want (%q, live %v)",
				tc.name, owner, live, tc.owner, tc.live)
		}
	}

	// AND THE PERSON'S OWN LEVER, which touches no session row.
	if err := rig.during(func() error {
		_, err := rig.writer.Revoke(t.Context(), person, "revoke-kim", "left")
		return err
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if owner, live, err := reader.SessionStanding(t.Context(), open,
		time.Now()); err != nil || owner != person || live {
		t.Errorf("a revoked person's session stands as (%q, live %v, %v), "+
			"want its owner and over", owner, live, err)
	}
}
