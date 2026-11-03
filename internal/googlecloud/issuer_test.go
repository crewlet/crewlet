package googlecloud_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// delegated is a new account the provisioner may mint for, by unique ID.
func (w *clientWorld) delegated(t *testing.T) string {
	t.Helper()
	sa, err := w.c.CreateServiceAccount(t.Context(), "crewlet-sre-1a2b", "Crewlet Acme sre", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.c.EditAccountPolicy(t.Context(), sa.Email, func(p *googlecloud.Policy) bool {
		return googlecloud.Grant(p, "serviceAccount:"+provisioner, []string{"roles/iam.serviceAccountTokenCreator"})
	}); err != nil {
		t.Fatal(err)
	}
	return sa.UniqueID
}

// ONE MINT AN HOUR SERVES EVERY READER: a token good for the life asked is
// handed out again, and one that is not is minted afresh.
func TestOneSeatMintServesEveryReaderWhileItIsGood(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	account := w.delegated(t)
	issuer := &googlecloud.SeatIssuer{Now: w.clock.now}
	first, err := issuer.Token(t.Context(), w.c, account, 50*time.Minute)
	if err != nil || !first.ExpiresAt.Equal(w.clock.now().Add(googlecloud.MaxTokenLifetime)) {
		t.Fatalf("first mint: %v, expires %s; want an hour's token", err, first.ExpiresAt)
	}
	w.clock.advance(30 * time.Minute)
	again, _ := issuer.Token(t.Context(), w.c, account, 10*time.Minute)
	if again.Token != first.Token {
		t.Error("a reader the held token still serves was handed a new one")
	}
	renewed, _ := issuer.Token(t.Context(), w.c, account, 50*time.Minute)
	if renewed.Token == first.Token || renewed.Token == "" {
		t.Error("a reader the held token cannot serve was handed it")
	}
}

// A REFUSAL IS HELD FOR ITS WAIT, so every reader within it is handed Google's
// answer without a request, and the first after it asks again.
func TestASeatMintGoogleRefusedIsHeldForItsWait(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	account := w.delegated(t)
	w.f.edit(func(f *fakeGoogle) { f.mintRefusals = 1 })
	issuer := &googlecloud.SeatIssuer{Now: w.clock.now, RetryWait: func(int) time.Duration { return time.Minute }}
	_, refused := issuer.Token(t.Context(), w.c, account, time.Minute)
	if googlecloud.Status(refused) != http.StatusForbidden {
		t.Fatalf("the refused mint answered %v", refused)
	}
	if _, err := issuer.Token(t.Context(), w.c, account, time.Minute); !errors.Is(err, refused) {
		t.Fatalf("a reader within the wait was answered %v, want the held refusal", err)
	}
	w.clock.advance(time.Minute)
	if _, err := issuer.Token(t.Context(), w.c, account, time.Minute); err != nil {
		t.Fatalf("the first reader after the wait: %v, want Google asked again", err)
	}
}

// A REFUSAL SAYS WHAT TO FIX AND WHETHER A WAIT CLEARS IT, and Google refusing
// a fresh grant is told apart from the connection's own refusal.
func TestASeatTokenRefusalSaysWhatToFix(t *testing.T) {
	t.Parallel()
	denied := &googlecloud.APIError{Status: http.StatusForbidden, Code: "PERMISSION_DENIED", Message: "denied"}
	for _, tc := range []struct {
		name     string
		err      error
		granting bool
		says     string
		passes   bool
	}{
		{"a grant not applied", denied, true, "Agents tab", false},
		{"an account disabled", &googlecloud.APIError{Status: http.StatusBadRequest,
			Message: "The service account is disabled."}, false, "turn the agent back on", false},
		{"a rate limit", &googlecloud.APIError{Status: http.StatusTooManyRequests, Message: "slow down"}, false,
			"slow down", true},
		{"Google failing", &googlecloud.APIError{Status: http.StatusServiceUnavailable, Message: "later"}, false,
			"later", true},
	} {
		said, passes := googlecloud.TokenRefusal(tc.err)
		if !strings.Contains(said, tc.says) || passes != tc.passes || googlecloud.StillGranting(tc.err) != tc.granting {
			t.Errorf("%s: told %q (passes %v, granting %v)", tc.name, said, passes, googlecloud.StillGranting(tc.err))
		}
	}

	// THE PROVISIONER REFUSED IS THE CONNECTION'S, whatever its status.
	w := newClientWorld(t)
	w.f.edit(func(f *fakeGoogle) { f.refuse["POST /v1/token"] = http.StatusForbidden })
	_, _, err := w.c.SeatToken(t.Context(), "100", time.Hour)
	said, passes := googlecloud.TokenRefusal(err)
	if googlecloud.StillGranting(err) || passes || !strings.Contains(said, "Cloud Shell block") {
		t.Errorf("the provisioner refused is told %q (passes %v), want the connection's fix", said, passes)
	}
}

// GOOGLE HAS APPLIED THE GRANT once GrantSettle has passed since the account
// was made and since the lane's last recorded grant.
func TestAGrantSettlesAfterTheLastOne(t *testing.T) {
	t.Parallel()
	made := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	rec := seataccess.Record{Account: &seataccess.Account{ID: "100", CreatedAt: made}}
	if got := googlecloud.GrantSettled(rec); !got.Equal(made.Add(googlecloud.GrantSettle)) {
		t.Errorf("a new account's grant settles at %s", got)
	}
	later := made.Add(time.Hour).Add(googlecloud.GrantSettle)
	rec.Lanes = map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneGoogleCloud: {
		Settling: &seataccess.Settling{Reason: googlecloud.ReasonGranting, Until: later}}}
	if got := googlecloud.GrantSettled(rec); !got.Equal(later) {
		t.Errorf("a later grant settles at %s, want %s", got, later)
	}
}
