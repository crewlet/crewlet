package googleauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact"
)

// APIError is a call Google refused. It holds what Google said, with anything
// shaped like a token taken out, and never the request's own credential.
type APIError struct {
	Method string
	// Path is the host and path called, without the query.
	Path   string
	Status int

	// Code is Google's status (`INVALID_ARGUMENT`, `PERMISSION_DENIED`), or
	// the OAuth error STS answers with (`invalid_grant`); Message its text,
	// and Reasons the details' reasons.
	Code    string
	Message string
	Reasons []string

	// RetryAfter is how long a 429 asked the call to wait, zero otherwise.
	RetryAfter time.Duration
	// Unwaited is why a read handed a 429 back rather than waiting it out.
	Unwaited string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("googleauth: %s %s: %s", e.Method, e.Path, e.Answer())
	if len(e.Reasons) > 0 {
		s += " [" + strings.Join(e.Reasons, ", ") + "]"
	}
	if e.Unwaited != "" {
		s += fmt.Sprintf(" (asked to wait %s; %s)", e.RetryAfter, e.Unwaited)
	}
	return s
}

// Answer is what Google said, without the call it answered: its status, its
// code and its message.
func (e *APIError) Answer() string {
	s := strconv.Itoa(e.Status)
	if e.Code != "" {
		s += " " + e.Code
	}
	return s + ": " + e.Message
}

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error, so [integration.Reject] can classify it.
func Status(err error) int {
	if api, ok := errors.AsType[*APIError](err); ok {
		return api.Status
	}
	return 0
}

// ConfigError is a connection input that would fail every call with a
// misleading refusal. Sentence names the field to change as the setup form
// labels it, in words a person reads as they are: no config path, no package.
type ConfigError struct {
	Sentence string
	// Err is what the input's fault is an instance of, such as
	// [integration.ErrNotConfigured] for a block that is off or unresolved.
	Err error
}

func (e *ConfigError) Error() string { return "googleauth: " + e.Sentence }
func (e *ConfigError) Unwrap() error { return e.Err }

// Sentence is what a person reads for err: a [ConfigError]'s own sentence, or
// the error's text.
func Sentence(err error) string {
	if c, ok := errors.AsType[*ConfigError](err); ok {
		return c.Sentence
	}
	return err.Error()
}

// Stage is one step of reaching Google as an account the federation
// impersonates, which a failure names so a person knows which half of the
// setup to look at.
type Stage string

// The two stages: STS refusing the engine's signed token, and IAM refusing the
// federated subject acting as the account.
const (
	StageFederation    Stage = "federation"
	StageImpersonation Stage = "impersonation"
)

// StageError is a failure to reach Google as an account the federation
// impersonates (the provisioner, or another account the identity project
// lets the company's subject act as), at one stage.
type StageError struct {
	Stage Stage
	Err   error
}

func (e *StageError) Error() string { return fmt.Sprintf("googleauth: %s: %v", e.Stage, e.Err) }
func (e *StageError) Unwrap() error { return e.Err }

// IsStage reports a failure to reach Google as a federated account at all,
// which is the connection's fault rather than the call's.
func IsStage(err error) bool {
	_, ok := errors.AsType[*StageError](err)
	return ok
}

// Reject is [integration.Reject] for a Google error, and a refused federation
// or impersonation is the connection's credential whatever its status: STS
// refuses a token its provider does not trust with a 400.
func Reject(err error) error {
	if st, ok := errors.AsType[*StageError](err); ok {
		if s := Status(st.Err); s >= http.StatusBadRequest && s < http.StatusInternalServerError &&
			s != http.StatusTooManyRequests {
			return fmt.Errorf("%w: %w", integration.ErrCredentialRejected, err)
		}
	}
	return integration.Reject(err, Status(err))
}

// ErrUnreached is a call that got no answer from Google: a failure a wait
// clears.
var ErrUnreached = errors.New("no answer from Google")

// ErrTooLarge is an answer longer than one call reads (8 MiB): a narrower
// request is the fix.
var ErrTooLarge = errors.New("the answer from Google is larger than one call reads")

// Passes reports a failure a wait clears: Google not reached, its rate limit,
// or a 5xx.
func Passes(err error) bool {
	if api, ok := errors.AsType[*APIError](err); ok {
		return api.Status == http.StatusTooManyRequests || api.Status >= http.StatusInternalServerError
	}
	return errors.Is(err, ErrUnreached)
}

// refusalOf reads Google's refusal: its RPC status shape, or STS's OAuth one.
func refusalOf(method, path string, res *http.Response, payload []byte) *APIError {
	api := &APIError{Method: method, Path: path, Status: res.StatusCode}
	var envelope struct {
		Error       json.RawMessage `json:"error"`
		Description string          `json:"error_description"`
	}
	if json.Unmarshal(payload, &envelope) == nil && len(envelope.Error) > 0 {
		var rpc struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				Reason string `json:"reason"`
			} `json:"details"`
		}
		var oauth string
		switch {
		case json.Unmarshal(envelope.Error, &rpc) == nil && (rpc.Message != "" || rpc.Status != ""):
			api.Code, api.Message = rpc.Status, rpc.Message
			for _, d := range rpc.Details {
				if d.Reason != "" {
					api.Reasons = append(api.Reasons, d.Reason)
				}
			}
		case json.Unmarshal(envelope.Error, &oauth) == nil:
			api.Code, api.Message = oauth, envelope.Description
		}
	}
	if strings.TrimSpace(api.Message) == "" {
		api.Message = httpx.Refusal(res.Header.Get("Content-Type"), payload)
	}
	if strings.TrimSpace(api.Message) == "" {
		api.Message = "no detail"
	}
	// A REFUSAL NEVER CARRIES A TOKEN ON, and Google's can quote one: STS
	// echoes the federation token it would not trust, a JWT, and a call's
	// refusal can name the access token it was made with. Taken out by the
	// one denylist every surface redacts with (internal/redact), which knows
	// both shapes and every other.
	api.Message = redact.Secrets(api.Message)
	return api
}
