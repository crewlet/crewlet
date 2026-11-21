package googlegrowth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
)

// Tokens mints a seat's growth token carrying scopes, good for at least life
// (the engine's GoogleGrowthToken).
type Tokens interface {
	GoogleGrowthToken(ctx context.Context, handle string, scopes []string, life time.Duration,
		hc *http.Client) (googleauth.SeatToken, error)
}

// Audit is the access log's record of each step of a growth act (the engine's
// GrowthAudit, ADR-0171): what a call made at once did, and a held call's
// outcome. The held call's ask is recorded by the engine from [growth.Hold.Ask].
type Audit interface {
	Made(ctx context.Context, r growth.Record) (bool, error)
	Applied(ctx context.Context, r growth.Record) (bool, error)
	Void(ctx context.Context, r growth.Record) (bool, error)
	Failed(ctx context.Context, r growth.Record) (bool, error)
}

// TokenLife is how long a minted token must stay good: one call's bound with
// room, since the issuer renews ahead of it.
const TokenLife = 2 * time.Minute

// Connection is the one connection a Google growth product has: the company's
// Google identity, which reaches every account its identities are added to.
const Connection = "google-cloud"

// Client is one Google growth product's way to Google.
type Client struct {
	// Kind is the product's integration kind and Name its name for a person.
	Kind, Name string
	Company    func() *config.Company
	Env        func() *config.Resolver
	Tokens     Tokens
	Audit      Audit
	// HTTP is the transport Google is reached through; nil in production.
	HTTP *http.Client
	// Secrets are the fields of an answer that hold a credential.
	Secrets []string
	// Refused says Google's refusal of a request in the product's words; nil
	// passes Google's error on.
	Refused func(req Request, api *googleauth.APIError) error
}

// CallError is a call that reached for Google and did not get its result,
// with the closed reason the access log keeps for it.
type CallError struct {
	Failure growth.Failure
	Err     error
}

func (e *CallError) Error() string { return e.Err.Error() }
func (e *CallError) Unwrap() error { return e.Err }

// FailureOf is why a call sent to Google failed, never Google's own words.
func FailureOf(err error) growth.Failure {
	if api, ok := errors.AsType[*googleauth.APIError](err); ok {
		switch {
		case api.Status == http.StatusUnauthorized:
			return growth.FailureUnauthorized
		case api.Status == http.StatusTooManyRequests:
			return growth.FailureRateLimited
		case api.Status >= 500:
			return growth.FailureVendorUnavailable
		}
		return growth.FailureVendorRefused
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return growth.FailureTimedOut
	}
	return growth.FailureVendorUnavailable
}

// Send makes req as handle's identity and answers Google's body. Its every
// failure is a [*CallError], with the reason the access log keeps.
func (c *Client) Send(ctx context.Context, handle string, req Request) ([]byte, error) {
	conn, err := googleauth.For(c.Company(), c.Env(), c.HTTP)
	if err != nil {
		return nil, &CallError{growth.FailureEngineError, growth.Refuse(growth.RefusedUnavailable,
			c.Name+" is not reachable: "+googleauth.Sentence(err)+". A person fixes it on the Google Cloud "+
				"connection. No vendor was reached.")}
	}
	bearer := ""
	if len(req.Op.Permissions) > 0 {
		token, mintErr := c.Tokens.GoogleGrowthToken(ctx, handle, req.Op.Permissions, TokenLife, c.HTTP)
		if mintErr != nil {
			return nil, &CallError{growth.FailureEngineError,
				growth.Refuse(growth.RefusedUnavailable, c.Name+" was not called: "+mintErr.Error())}
		}
		bearer = token.Token
	}
	var body any
	if req.Body != nil {
		body = req.Body
	}
	var raw json.RawMessage
	err = conn.Send(ctx, googleauth.Call{Method: req.Op.Method, Address: req.URL(), Bearer: bearer, Header: req.Header,
		Body: body, Reads: req.Reads()}, &raw)
	if err != nil {
		return nil, &CallError{FailureOf(err), c.refused(req, err)}
	}
	return raw, nil
}

// refused is Google's refusal in the product's words, where it has any.
func (c *Client) refused(req Request, err error) error {
	if api, ok := errors.AsType[*googleauth.APIError](err); ok && c.Refused != nil {
		if said := c.Refused(req, api); said != nil {
			return said
		}
	}
	return err
}

// Answer is a call's answer: Google's JSON, every credential in it withheld,
// which carries words other people wrote (names, page titles, search terms).
type Answer struct {
	Operation string `json:"operation"`
	Answer    string `json:"answer" untrusted:"fence"`
}

// AnswerOf is Google's body for req, its credentials withheld and, where cut
// is set, cut to what the seat may see.
func (c *Client) AnswerOf(req Request, raw []byte, cut func(any) any) (Answer, error) {
	var v any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &v); err != nil {
			return Answer{}, fmt.Errorf("googlegrowth: decode %s answer to %s: %w", c.Kind, req.Op.ID, err)
		}
	}
	v = Scrub(v, c.Secrets)
	if cut != nil {
		v = cut(v)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return Answer{}, fmt.Errorf("googlegrowth: encode %s answer to %s: %w", c.Kind, req.Op.ID, err)
	}
	return Answer{Operation: req.Op.ID, Answer: Compact(out)}, nil
}

// SendAndRead sends req and reads Google's answer: an answer that cannot be
// read is a call whose outcome is not known.
func (c *Client) SendAndRead(ctx context.Context, handle string, req Request, cut func(any) any) (Answer, error) {
	raw, err := c.Send(ctx, handle, req)
	if err != nil {
		return Answer{}, err
	}
	out, err := c.AnswerOf(req, raw, cut)
	if err != nil {
		return Answer{}, &CallError{growth.FailureUnknownOutcome, err}
	}
	return out, nil
}
