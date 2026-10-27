package setup

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/atlassianhost"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Checking a credential somebody has typed, before anything stores it.
//
// # Why a check is its own gesture
//
// A connect seals what it is given and the next reconcile pass finds out
// whether it works, minutes later, as a finding on a card. A person pasting
// a key wants that answer now, while the key is still on their clipboard
// and the vendor's page is still open. So a check asks the vendor the one
// READ-ONLY question that proves a credential (who am I, which organization
// is this) and stores NOTHING: not the value, not a pointer, not a finding.
//
// # Not every credential can be checked ahead
//
// A webhook signing secret is proved by the vendor signing something with
// it, and nothing can make the vendor do that from here without registering
// the hook, which is a write. Such a field is not [Requirement.Checkable],
// and its [Requirement.CheckNote] says when it will be proved. The answer
// for it is [VerifyUnchecked], given by the surface without asking any
// vendor, so the sentence lives once, beside the requirement it is about.

// VerifyOutcome is what a check found.
type VerifyOutcome string

const (
	// VerifyWorks is the vendor accepting the credential.
	VerifyWorks VerifyOutcome = "works"

	// VerifyRefused is the vendor answering and refusing it. Only the
	// vendor's own refusal is this: a refusal is a claim, and a timeout or
	// an outage reported as one sends a person to rotate a key that works.
	VerifyRefused VerifyOutcome = "refused"

	// VerifyUnreachable is the vendor not giving an answer about the
	// credential at all: no connection, a timeout, an outage, or an answer
	// that is not the vendor's API.
	VerifyUnreachable VerifyOutcome = "unreachable"

	// VerifyUnchecked is a credential that cannot be checked ahead without
	// acting on it. See [Requirement.CheckNote].
	VerifyUnchecked VerifyOutcome = "unchecked"
)

// VerifyOutcomes is every outcome, in the order a reader meets them.
var VerifyOutcomes = []VerifyOutcome{
	VerifyWorks, VerifyRefused, VerifyUnreachable, VerifyUnchecked,
}

// Valid reports whether o is an outcome this build knows.
func (o VerifyOutcome) Valid() bool {
	for _, known := range VerifyOutcomes {
		if o == known {
			return true
		}
	}
	return false
}

// Verdict is the answer to one check.
type Verdict struct {
	Outcome VerifyOutcome `json:"outcome"`
	// Detail is one sentence for a person. It never carries the value that
	// was checked.
	Detail string `json:"detail"`
}

// VerifyBudget bounds one check, end to end.
//
// TEN SECONDS, the same order as the vendor clients' own per-request
// timeouts (Datadog, GitHub, GitLab, Jira and Slack each take ten), because
// a check is one such request and occasionally two (Slack's auth.test is
// followed by bots.info, GitLab's /user by the token's own description). A
// person is waiting on a button, so a vendor slower than this is reported as
// not answering rather than waited out: nothing is stored either way, and
// pressing the button again costs nothing.
const VerifyBudget = 10 * time.Second

// Check is what a probe is handed.
type Check struct {
	// Field is the requirement being checked, by [Requirement.Field].
	Field string

	// Value answers any field of the same requirement list: what the
	// person typed in the form, else what the company document holds for
	// it (a whole `${VAR}` resolved through this node's chain), else the
	// requirement's [Requirement.Default]. Value(Field) is the credential
	// under check. A field the list does not have answers "".
	Value func(field string) string

	// Declared is the Atlassian deployment the company declared
	// (`integrations.atlassian.deployment`), or [atlassianhost.Unknown].
	//
	// CARRIED FOR TWO PROBES AND READ BY NO OTHER, because it is the one
	// input a check needs that is not a field of its own list: Jira and
	// Confluence decide REST v3 or v2, and a Cloud site at a base domain no
	// host rule names (Isolated Cloud), from it. Every client this engine
	// builds for those products is handed the same declaration.
	Declared atlassianhost.Deployment
}

// Secret is the credential under check, for a probe that must keep it out
// of what it reports.
func (c Check) Secret() string {
	if c.Value == nil {
		return ""
	}
	return c.Value(c.Field)
}

// Probe checks one credential at its vendor with a read-only call.
//
// It answers a [Verdict] for everything the VENDOR decides, refusals and
// outages included, and an error only for what the engine decides: a check
// the inputs cannot make ([ErrUnusableInput]) or a field the probe was
// never meant to be asked about ([ErrNotCheckable]), which is a wiring
// mistake.
type Probe func(ctx context.Context, check Check) (Verdict, error)

// ErrUnusableInput marks a check the request cannot be made from: a
// companion value is missing (Datadog checks its two keys together), or an
// address is not one the vendor serves. The caller answers it as the
// request's fault, with the error's sentence.
var ErrUnusableInput = errors.New("setup: the check cannot be made from what was given")

// ErrNotCheckable marks a probe asked about a field it does not check. The
// surface asks a probe only about a [Requirement.Checkable] field, so this
// is a requirement list and its probe disagreeing.
var ErrNotCheckable = errors.New("setup: this probe does not check that field")

// Unusable is an [ErrUnusableInput] carrying the sentence a person reads.
func Unusable(sentence string) error {
	return &unusable{sentence: sentence}
}

type unusable struct{ sentence string }

func (u *unusable) Error() string { return u.sentence }
func (u *unusable) Unwrap() error { return ErrUnusableInput }

// NotCheckable is the [ErrNotCheckable] a probe answers for a field outside
// its switch.
func NotCheckable(app, field string) error {
	return fmt.Errorf("%w: the %s probe was asked about %q", ErrNotCheckable, app, field)
}

// Works is the verdict for an accepted credential.
func Works(detail string) Verdict {
	return Verdict{Outcome: VerifyWorks, Detail: detail}
}

// Refused is the verdict for a credential the vendor answered and refused
// for a reason the vendor's status cannot carry: a token that authenticates
// as an account without the role or the scope this integration needs.
func Refused(detail string) Verdict {
	return Verdict{Outcome: VerifyRefused, Detail: detail}
}

// Unchecked is the verdict for a field that cannot be checked ahead.
func Unchecked(detail string) Verdict {
	return Verdict{Outcome: VerifyUnchecked, Detail: detail}
}

// Failure is how a vendor call ended without an answer to act on, as the
// vendor package can describe it.
type Failure struct {
	// App is the vendor's name as a person reads it: "Datadog".
	App string
	// Err is what the client returned.
	Err error
	// Status is the HTTP status of the vendor's refusal, 0 when there was
	// none (a transport failure, or a vendor like Slack that refuses
	// inside a 200).
	Status int
	// Refused is the vendor's own refusal where it carries no status:
	// Slack's `invalid_auth`. A 401 or 403 needs no help here.
	Refused bool
	// Reason is the vendor's own words about the refusal, when it gave any.
	Reason string
}

// maxReasonRunes is how much of a vendor's reason a verdict repeats.
//
// A reason is shown under a button as part of one sentence, so past a
// couple of hundred characters it is a page of somebody else's prose rather
// than a reason, and it is left out whole rather than cut: a cut reason is a
// different message. Vendors that explain a refusal at all do it in a short
// phrase ("Invalid API key", `invalid_auth`, "401 Unauthorized").
const maxReasonRunes = 200

// Judge turns a failed vendor call into a verdict.
//
// THE CLASSIFICATION IS [integration.Reject]'s, the rule every reconcile pass
// already applies: 401 and 403 are the vendor refusing the credential, and
// everything else (a 404 from a wrong address, a 429, a 5xx, a dead
// connection, an answer that is not the vendor's API) is the vendor not
// having said anything about it. Reporting those as refusals is exactly the
// mistake [integration.Refusal] records: a working key sent for rotation.
//
// secret is the value that was checked, and a reason that repeats it is
// dropped: a vendor echoing a submitted token into its error is rare and
// the one place this would put a credential back on a screen.
func Judge(f Failure, secret string) Verdict {
	app := f.App
	switch {
	case errors.Is(f.Err, context.DeadlineExceeded):
		return Verdict{Outcome: VerifyUnreachable, Detail: fmt.Sprintf(
			"%s did not answer within %s. Nothing was stored.",
			app, statelog.Spoken(VerifyBudget))}
	case f.Refused || errors.Is(integration.Reject(f.Err, f.Status), integration.ErrCredentialRejected):
		if reason := sayable(f.Reason, secret); reason != "" {
			return Verdict{Outcome: VerifyRefused, Detail: fmt.Sprintf(
				"%s refused it: %s.", app, strings.TrimRight(reason, "."))}
		}
		return Verdict{Outcome: VerifyRefused, Detail: app + " refused it."}
	case f.Status == http.StatusTooManyRequests || f.Status >= http.StatusInternalServerError:
		return Verdict{Outcome: VerifyUnreachable, Detail: fmt.Sprintf(
			"%s answered %s, so it could not say whether this works. Nothing was stored.",
			app, statusText(f.Status))}
	case f.Status != 0:
		return Verdict{Outcome: VerifyUnreachable, Detail: fmt.Sprintf(
			"%s answered %s at this address, which usually means the address is "+
				"wrong rather than the credential. Nothing was stored.",
			app, statusText(f.Status))}
	}
	var transport *url.Error
	if errors.As(f.Err, &transport) {
		return Verdict{Outcome: VerifyUnreachable, Detail: fmt.Sprintf(
			"%s could not be reached: %s. Nothing was stored.", app, connectionCause(f.Err))}
	}
	return Verdict{Outcome: VerifyUnreachable, Detail: fmt.Sprintf(
		"%s answered with something that is not its API, which usually means "+
			"the address is wrong. Nothing was stored.", app)}
}

// ReasonIn reads the message out of a vendor's refusal text, as
// [httpx.Refusal] left it: a JSON body compacted onto one line, an HTML
// page's title, or plain text.
//
// THE SHAPE IS THE VENDOR'S, so the vendor names the keys its errors carry
// a person's sentence under ("message" for GitHub, "errorMessages" for
// Jira), and a key holding a list answers its first entry. A JSON body with
// none of them answers "", because a verdict repeating somebody else's
// error object is a payload under a button rather than a reason. Text that
// is not JSON is the reason as it stands.
func ReasonIn(detail string, keys ...string) string {
	detail = strings.TrimSpace(detail)
	if !strings.HasPrefix(detail, "{") {
		return detail
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(detail), &body); err != nil {
		return detail
	}
	for _, key := range keys {
		switch v := body[key].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return v
			}
		case []any:
			for _, item := range v {
				if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
					return text
				}
			}
		}
	}
	return ""
}

// sayable is a vendor reason fit to repeat, or "".
func sayable(reason, secret string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	switch {
	case reason == "",
		utf8.RuneCountInString(reason) > maxReasonRunes,
		secret != "" && strings.Contains(reason, strings.TrimSpace(secret)):
		return ""
	}
	return reason
}

func statusText(status int) string {
	if text := http.StatusText(status); text != "" {
		return fmt.Sprintf("%d %s", status, text)
	}
	return fmt.Sprint(status)
}

// connectionCause names why a connection failed in words a person can act
// on, rather than the transport's own text, which carries the whole request
// line around a cause nobody can find.
func connectionCause(err error) string {
	var dns *net.DNSError
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &dns):
		return "the address does not resolve"
	case errors.As(err, &unknownAuthority), errors.As(err, &hostname), errors.As(err, &invalid):
		return "its certificate is not one this engine trusts"
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return "the connection was refused or timed out"
	}
	return "the connection failed"
}
