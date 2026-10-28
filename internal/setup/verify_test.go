package setup_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/setup"
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
			setup.VerifyRefused, "Acme refused it: Bad credentials."},
		{"a 403 with no reason", setup.Failure{App: "Acme", Err: api, Status: 403},
			setup.VerifyRefused, "Acme refused it."},
		{"a refusal carried in a 200", setup.Failure{App: "Acme", Err: api, Refused: true,
			Reason: "invalid_auth"}, setup.VerifyRefused, "Acme refused it: invalid_auth."},
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
			if got.Outcome != c.want {
				t.Errorf("outcome = %s, want %s (%s)", got.Outcome, c.want, got.Detail)
			}
			if !strings.Contains(got.Detail, c.says) {
				t.Errorf("detail %q does not say %q", got.Detail, c.says)
			}
			if c.want == setup.VerifyUnreachable && !strings.HasSuffix(got.Detail, "Nothing was stored.") {
				t.Errorf("an unreachable vendor does not say nothing was stored: %q", got.Detail)
			}
			if strings.Contains(got.Detail, "\u2014") {
				t.Errorf("the sentence carries an em dash: %q", got.Detail)
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
		return setup.Judge(setup.Failure{App: "Acme", Err: errors.New("x"), Status: 401,
			Reason: reason}, "tok-123").Detail
	}
	if got := refused("token tok-123 is not valid"); got != "Acme refused it." {
		t.Errorf("a reason quoting the credential was repeated: %q", got)
	}
	if got := refused(strings.Repeat("word ", 60)); got != "Acme refused it." {
		t.Errorf("a page of prose was repeated as a reason: %q", got)
	}
	if got := refused("  Invalid\n API key. "); got != "Acme refused it: Invalid API key." {
		t.Errorf("a short reason was not repeated on one line: %q", got)
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
