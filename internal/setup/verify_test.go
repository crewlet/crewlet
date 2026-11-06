package setup_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/setup/setuptest"
)

// THE OUTCOMES ARE A CLOSED SET, so an answer off a probe a newer build wrote
// is told apart from one this build means.
func TestVerifyOutcomesAreAClosedSet(t *testing.T) {
	t.Parallel()
	for _, o := range setup.VerifyOutcomes {
		if !o.Valid() {
			t.Errorf("%q is listed and not valid", o)
		}
	}
	for _, o := range []setup.VerifyOutcome{"", "ok", "Works", "maybe"} {
		if o.Valid() {
			t.Errorf("%q is valid and not listed", o)
		}
	}
}

// A REFUSAL IS A CLAIM ONLY THE VENDOR CAN MAKE. 401 and 403 are the vendor
// refusing the credential; every other way a call can fail is the vendor not
// having said anything about it, and reporting that as a refusal sends a
// person to rotate a key that works.
func TestJudgeCallsOnlyTheVendorsOwnRefusalARefusal(t *testing.T) {
	t.Parallel()
	api := errors.New("vendor: GET /me")
	dial := &url.Error{Op: "Get", URL: "https://x.example", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}
	dns := &url.Error{Op: "Get", URL: "https://x.example", Err: &net.DNSError{
		Err: "no such host", Name: "x.example", IsNotFound: true}}
	timeout := &url.Error{Op: "Get", URL: "https://x.example", Err: context.DeadlineExceeded}
	for _, c := range []struct {
		name    string
		failure setup.Failure
		want    setup.VerifyOutcome
		says    string
	}{
		{"a 401", setup.Failure{App: "Acme", Err: api, Status: 401, Reason: "Bad credentials"},
			setup.VerifyRefused, "Acme said: Bad credentials"},
		{"a 403 with no reason", setup.Failure{App: "Acme", Err: api, Status: 403},
			setup.VerifyRefused, "Acme refused it."},
		{"a refusal carried in a 200", setup.Failure{App: "Acme", Err: api, Refused: true,
			Reason: "invalid_auth"}, setup.VerifyRefused, "Acme said: invalid_auth"},
		{"a 500", setup.Failure{App: "Acme", Err: api, Status: 500},
			setup.VerifyUnreachable, "500 Internal Server Error"},
		{"a 429", setup.Failure{App: "Acme", Err: api, Status: 429},
			setup.VerifyUnreachable, "429 Too Many Requests"},
		{"a 404", setup.Failure{App: "Acme", Err: api, Status: 404},
			setup.VerifyUnreachable, "the address is wrong"},
		{"a refused connection", setup.Failure{App: "Acme", Err: dial},
			setup.VerifyUnreachable, "the connection was refused or timed out"},
		{"a name that does not resolve", setup.Failure{App: "Acme", Err: dns},
			setup.VerifyUnreachable, "the address does not resolve"},
		{"a timeout", setup.Failure{App: "Acme", Err: timeout},
			setup.VerifyUnreachable, "did not answer within 10s"},
		{"an answer that is not the API", setup.Failure{App: "Acme",
			Err: errors.New("decode: invalid character '<'")},
			setup.VerifyUnreachable, "not its API"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := setup.Judge(c.failure, "s3cret")
			said := spoken(got)
			if got.Outcome != c.want {
				t.Errorf("outcome = %s, want %s (%s)", got.Outcome, c.want, said)
			}
			if !strings.Contains(said, c.says) {
				t.Errorf("verdict %q does not say %q", said, c.says)
			}
			if c.want == setup.VerifyUnreachable && !slices.Contains(got.Facts, setup.NothingStored) {
				t.Errorf("an unreachable vendor does not say nothing was stored: %q", said)
			}
			// THE ANSWER FIRST, in one sentence, and everything else after it.
			if strings.Count(got.Summary, ". ") > 0 || !strings.HasSuffix(got.Summary, ".") {
				t.Errorf("the summary is not one sentence: %q", got.Summary)
			}
			if strings.Contains(said, "\u2014") {
				t.Errorf("the verdict carries an em dash: %q", said)
			}
		})
	}
}

// A REASON IS REPEATED ONLY WHEN IT IS A REASON: short, and never the value
// that was checked. A vendor echoing a submitted token into its error is rare
// and the one way a check would put a credential back on a screen.
func TestJudgeRepeatsOnlyASayableReason(t *testing.T) {
	t.Parallel()
	refused := func(reason string) string {
		return spoken(setup.Judge(setup.Failure{App: "Acme", Err: errors.New("x"), Status: 401,
			Reason: reason}, "tok-123"))
	}
	if got := refused("token tok-123 is not valid"); got != "Acme refused it." {
		t.Errorf("a reason quoting the credential was repeated: %q", got)
	}
	if got := refused(strings.Repeat("word ", 60)); got != "Acme refused it." {
		t.Errorf("a page of prose was repeated as a reason: %q", got)
	}
	if got := refused("  Invalid\n API key. "); got != "Acme refused it. | Acme said: Invalid API key" {
		t.Errorf("a short reason was not repeated on one line: %q", got)
	}
}

// spoken is a verdict as one string, for a case asserting on what is read.
func spoken(v setup.Verdict) string { return setuptest.Spoken(v) }

// A VERDICT SORTS WHAT IT SAYS INTO FACTS AND WARNINGS, keeps only what has
// something to say, and an unreachable one always ends on what a check never
// does: store anything.
func TestAVerdictSortsItsDetailsAndKeepsOnlyWhatItHas(t *testing.T) {
	t.Parallel()
	fact := setup.Fact{Label: "Account", Value: "acme"}
	warning := setup.Warning{Title: "It is broad.", Action: "Narrow it."}
	got := setup.Works(" Works. ", warning, setup.Fact{Label: "Empty", Value: "  "}, fact, setup.Warning{})
	if got.Summary != "Works." || !slices.Equal(got.Facts, []setup.Fact{fact}) ||
		!slices.Equal(got.Warnings, []setup.Warning{warning}) {
		t.Errorf("Works = %#v", got)
	}
	if got := setup.Refused("Refused.", warning); got.Outcome != setup.VerifyRefused || len(got.Warnings) != 1 {
		t.Errorf("Refused = %#v", got)
	}
	got = setup.Unreachable("Acme did not answer.", fact)
	if want := []setup.Fact{fact, setup.NothingStored}; !slices.Equal(got.Facts, want) {
		t.Errorf("Unreachable facts = %v, want %v", got.Facts, want)
	}
	if got := setup.Unchecked("Proved later."); got.Outcome != setup.VerifyUnchecked || got.Facts != nil || got.Warnings != nil {
		t.Errorf("Unchecked = %#v", got)
	}
}

// A WARNING WITH NO ACTION IS KEPT, so the shared rules fail the probe that
// wrote it rather than the verdict quietly saying less.
func TestAWarningWithNoActionReachesTheRules(t *testing.T) {
	t.Parallel()
	got := setup.Works("Works.", setup.Warning{Title: "It is broad."})
	if len(got.Warnings) != 1 {
		t.Fatalf("the warning was dropped: %#v", got)
	}
	if problem := setuptest.VerdictProblem(got); problem == "" {
		t.Error("a warning with no action passed the rules")
	}
}

// AN EXPIRY IS SAID ONE WAY FOR EVERY VENDOR: the date or Never, with a warning
// only inside the window every reconcile pass warns in, or past the date.
func TestAnExpiryIsAFactWithAWarningWhenItIsNear(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name       string
		at         time.Time
		value      string
		warned     string
		actionWith string
	}{
		{"none", time.Time{}, "Never", "", ""},
		{"far off", now.AddDate(1, 0, 0), "2027-11-06", "", ""},
		{"inside the window", now.Add(integration.ExpiryWarning - time.Hour), "2026-11-20", "It expires soon.", "before 2026-11-20"},
		{"past", now.Add(-time.Hour), "2026-11-06", "Its expiry date has passed.", "now"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := setup.Works("Works.", setup.Expiry(c.at, now)...)
			if want := []setup.Fact{{Label: "Expires", Value: c.value}}; !slices.Equal(got.Facts, want) {
				t.Errorf("facts = %v, want %v", got.Facts, want)
			}
			if c.warned == "" {
				if got.Warnings != nil {
					t.Errorf("warned on an expiry that is not near: %v", got.Warnings)
				}
				return
			}
			if len(got.Warnings) != 1 || got.Warnings[0].Title != c.warned ||
				!strings.Contains(got.Warnings[0].Action, c.actionWith) {
				t.Errorf("warnings = %v, want %q with %q", got.Warnings, c.warned, c.actionWith)
			}
			if problem := setuptest.VerdictProblem(got); problem != "" {
				t.Error(problem)
			}
		})
	}
}

// A PROBE CHECKING SEVERAL CREDENTIALS holds a reason against every one of
// them, not only the field under check.
func TestSayableHoldsAReasonAgainstEveryCredential(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ reason, want string }{
		{"key fake-a is unknown", ""},
		{"key fake-b is unknown", ""},
		{strings.Repeat("word ", 60), ""},
		{"  Forbidden\n ", "Forbidden"},
	} {
		if got := setup.Sayable(c.reason, "fake-a", " fake-b ", ""); got != c.want {
			t.Errorf("Sayable(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}

// THE VENDOR'S OWN SHAPE NAMES ITS MESSAGE, and a JSON body naming none of the
// keys is left out rather than shown as an object under a button.
func TestReasonInReadsTheVendorsMessage(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		detail string
		keys   []string
		want   string
	}{
		{`{"message":"Bad credentials","documentation_url":"https://x"}`, []string{"message"}, "Bad credentials"},
		{`{"errorMessages":["You are not authenticated."],"errors":{}}`, []string{"errorMessages"}, "You are not authenticated."},
		{`{"error":"invalid_token","error_description":"Token was revoked."}`,
			[]string{"message", "error_description"}, "Token was revoked."},
		{`{"code":401}`, []string{"message"}, ""},
		{"Unauthorized", []string{"message"}, "Unauthorized"},
	} {
		if got := setup.ReasonIn(c.detail, c.keys...); got != c.want {
			t.Errorf("ReasonIn(%s) = %q, want %q", c.detail, got, c.want)
		}
	}
}

// AN UNUSABLE INPUT IS AN ErrUnusableInput carrying the sentence alone, which
// is what the surface answers as the request's fault.
func TestUnusableCarriesItsSentenceAndItsClass(t *testing.T) {
	t.Parallel()
	err := setup.Unusable("Enter both keys.")
	if !errors.Is(err, setup.ErrUnusableInput) || err.Error() != "Enter both keys." {
		t.Errorf("Unusable = %v (is %v)", err, errors.Is(err, setup.ErrUnusableInput))
	}
	wrapped := fmt.Errorf("probe: %w", err)
	if !errors.Is(wrapped, setup.ErrUnusableInput) {
		t.Error("a wrapped unusable input lost its class")
	}
	if !errors.Is(setup.NotCheckable("Acme", "secret"), setup.ErrNotCheckable) {
		t.Error("NotCheckable is not an ErrNotCheckable")
	}
	if errors.Is(setup.NotCheckable("Acme", "secret"), setup.ErrUnusableInput) {
		t.Error("a wiring fault reads as the request's fault")
	}
}
