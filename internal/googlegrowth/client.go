package googlegrowth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
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
// answer and outcome. The held call's ask is recorded by the engine from
// [growth.Hold.Ask]. first is false where the act already holds the step.
type Audit interface {
	Made(ctx context.Context, r growth.Record) (first bool, err error)
	Verdict(ctx context.Context, step growth.Step, r growth.Record) (first bool, err error)
	Applied(ctx context.Context, r growth.Record) (first bool, err error)
	Void(ctx context.Context, r growth.Record) (first bool, err error)
	Failed(ctx context.Context, r growth.Record) (first bool, err error)
}

// Leaser holds one lease on a resource at a time, fleet-wide (the engine's
// growth act lease).
type Leaser interface {
	Hold(ctx context.Context, resource string) (release func(), err error)
}

// Grant is a company's consent grant, for a product Google refuses service
// accounts for: the token a person's sign-in renews inside the engine, which
// every seat of the product sends as, in place of its own growth identity.
type Grant interface {
	// Token is the grant's access token, renewed ahead of its expiry.
	Token(ctx context.Context) (string, error)
	// Renewed is a fresh token after Google refused refused as unauthenticated:
	// one renewal, after which the sign-in is the person's to make again.
	Renewed(ctx context.Context, refused string) (string, error)
}

// TokenLife is how long a minted token must stay good: one call's bound with
// room, since the issuer renews ahead of it.
const TokenLife = 2 * time.Minute

// Connection is the one connection a Google growth product has: the company's
// Google identity, which reaches every account its identities are added to;
// GrantConnection a consent product's, the company's consent grant.
const (
	Connection      = "google-cloud"
	GrantConnection = "consent"
)

// connection is what the access log names c's calls as made through.
func (c *Client) connection() string {
	if c.Grant != nil {
		return GrantConnection
	}
	return Connection
}

// Client is one Google growth product's way to Google.
type Client struct {
	// Kind is the product's integration kind and Name its name for a person.
	Kind, Name string
	Company    func() *config.Company
	Env        func() *config.Resolver
	Tokens     Tokens
	Audit      Audit
	// Leases hold one applied write per account at a time; nil sends no write.
	Leases Leaser
	// HTTP is the transport Google is reached through; nil in production.
	HTTP *http.Client
	// Secrets are the fields of an answer that hold a credential.
	Secrets []string
	// Texts are the fields a change sets to somebody's words whatever they
	// look like (a one-word name reads as an identifier), by their last
	// segment: the access log keeps them as their digest.
	Texts []string
	// Refused says Google's refusal of a request in the product's words; nil
	// passes Google's error on.
	Refused func(req Request, api *googleauth.APIError) error
	// ReadScopes are the scopes a write's target is read back with; nil reads
	// with the write's own.
	ReadScopes func(op growth.Operation) []string
	// BeforeRead is the product's read of a write's targets, as handle,
	// where no one resource holds them (a batch naming them in its body, a
	// mutate's every resource): their state as the binding holds it, false
	// where the write's own resource is read ([Request.Resource]); nil is never.
	BeforeRead func(ctx context.Context, handle string, req Request) (before []byte, ok bool, err error)
	// Grant, where set, is what every call is sent as: the company's consent
	// grant, never a seat's identity, so the seat's level and scope are the
	// engine's alone to hold.
	Grant Grant
	// Signer is whose identity signs the client's calls, as the access log
	// names it: the seat's own for a product's client, the company's for a
	// reader client; "" for a consent product, which offers no seat identity.
	Signer growth.Signer
}

// CallError is a call that reached for Google and did not get its result,
// with the closed reason the access log keeps for it.
type CallError struct {
	Failure growth.Failure
	Err     error
}

func (e *CallError) Error() string { return e.Err.Error() }
func (e *CallError) Unwrap() error { return e.Err }

// VendorUnavailable reports Google failing to serve the call.
func (e *CallError) VendorUnavailable() bool { return e.Failure == growth.FailureVendorUnavailable }

// FailureOf is why a call sent to Google failed, never Google's own words. A
// write that got no answer of Google's own (no reply, a reset, its deadline)
// may have reached Google, so what it did is not known; one that failed
// before its request left ([Unsent]) did nothing.
func FailureOf(err error, write bool) growth.Failure {
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
	var netErr net.Error
	switch {
	case Unsent(err) && errors.Is(err, context.DeadlineExceeded):
		return growth.FailureTimedOut
	case Unsent(err):
		return growth.FailureEngineError
	case write:
		return growth.FailureUnknownOutcome
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return growth.FailureTimedOut
	case errors.Is(err, googleauth.ErrTooLarge):
		return growth.FailureUnknownOutcome
	}
	return growth.FailureVendorUnavailable
}

// Send makes req as handle's identity and answers Google's body. Its every
// failure is a [*CallError], with the reason the access log keeps.
func (c *Client) Send(ctx context.Context, handle string, req Request) ([]byte, error) {
	if req.Raw {
		var kept googleauth.Raw
		if err := c.send(ctx, handle, req, nil, &kept); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"contentType": kept.ContentType, "data": string(kept.Data)})
	}
	var raw json.RawMessage
	err := c.send(ctx, handle, req, nil, &raw)
	return raw, err
}

// send makes req as handle's identity, or as the company's consent grant
// where the product has one, with body (req's own where nil), reading
// Google's answer into out. Its every failure is a [*CallError].
func (c *Client) send(ctx context.Context, handle string, req Request, body, out any) error {
	if c.Grant != nil {
		return c.sendAsGrant(ctx, req, body, out)
	}
	conn, err := googleauth.For(c.Company(), c.Env(), c.HTTP)
	if err != nil {
		return &CallError{growth.FailureEngineError, growth.Refuse(growth.RefusedUnavailable,
			c.Name+" is not reachable: "+googleauth.Sentence(err)+". A person fixes it on the Google Cloud "+
				"connection. No vendor was reached.")}
	}
	bearer := ""
	if len(req.Op.Permissions) > 0 {
		token, mintErr := c.Tokens.GoogleGrowthToken(ctx, handle, req.Op.Permissions, TokenLife, c.HTTP)
		if mintErr != nil {
			return &CallError{growth.FailureEngineError,
				growth.Refuse(growth.RefusedUnavailable, c.Name+" was not called: "+mintErr.Error())}
		}
		bearer = token.Token
	}
	address, err := c.address(ctx, req)
	if err != nil {
		return unsent(err)
	}
	if err = sendOn(ctx, &conn.Transport, req, address, bearer, body, out); err != nil {
		return &CallError{FailureOf(err, !req.Reads()), c.refused(req, err)}
	}
	return nil
}

// sendAsGrant makes req with the company's consent grant: once, and once more
// with a renewed token where Google refused the first as unauthenticated.
func (c *Client) sendAsGrant(ctx context.Context, req Request, body, out any) error {
	notCalled := func(err error) error {
		return &CallError{growth.FailureEngineError,
			growth.Refuse(growth.RefusedUnavailable, c.Name+" was not called: "+err.Error())}
	}
	token, err := c.Grant.Token(ctx)
	if err != nil {
		return notCalled(err)
	}
	address, err := c.address(ctx, req)
	if err != nil {
		return unsent(err)
	}
	transport := googleauth.NewTransport(c.HTTP)
	err = sendOn(ctx, transport, req, address, token, body, out)
	if googleauth.Status(err) == http.StatusUnauthorized {
		if token, err = c.Grant.Renewed(ctx, token); err != nil {
			return notCalled(err)
		}
		err = sendOn(ctx, transport, req, address, token, body, out)
	}
	if err != nil {
		return &CallError{FailureOf(err, !req.Reads()), c.refused(req, err)}
	}
	return nil
}

// sendOn makes req at address on t with bearer and its API's headers, with
// body (req's own where nil), reading the answer into out; a read's 429 is
// waited out.
func sendOn(ctx context.Context, t *googleauth.Transport, req Request, address, bearer string, body, out any) error {
	call := googleauth.Call{Method: req.Op.Method, Address: address, Bearer: bearer, Header: req.Header,
		Body: body, Reads: req.Reads()}
	if body == nil && req.Body != nil {
		call.Body = req.Body
	}
	return t.Send(ctx, call, out)
}

// address is where req is sent: its query as [Request.Fill] makes it. A fill
// that failed sent nothing, so it fails as the read it is.
func (c *Client) address(ctx context.Context, req Request) (string, error) {
	if req.Fill == nil {
		return req.URL(), nil
	}
	q := url.Values{}
	for k, v := range req.Query {
		q[k] = append([]string(nil), v...)
	}
	q, err := req.Fill(ctx, q)
	if err != nil {
		if _, ok := errors.AsType[*CallError](err); ok {
			return "", err
		}
		return "", &CallError{FailureOf(err, false), err}
	}
	return req.Address + "?" + q.Encode(), nil
}

// unsent is err, from before req's request was made (its query's fill), as
// the [*CallError] it is, marked [Unsent].
func unsent(err error) error {
	ce, ok := errors.AsType[*CallError](err)
	if !ok {
		ce = &CallError{FailureOf(err, false), err}
	}
	return &CallError{ce.Failure, fmt.Errorf("%w: %w", googleauth.ErrUnsent, ce.Err)}
}

// Unsent reports whether err, a call's failure, came before its request left
// for Google ([googleauth.ErrUnsent]): nothing reached Google.
func Unsent(err error) bool { return errors.Is(err, googleauth.ErrUnsent) }

// refused is Google's refusal in the product's words, where it has any, and
// an answer too large to read as what to ask for instead.
func (c *Client) refused(req Request, err error) error {
	if errors.Is(err, googleauth.ErrTooLarge) {
		return growth.Refuse(growth.RefusedInvalid, c.Name+" answered "+req.Op.ID+" with more than one call "+
			"reads; narrow it: fewer days, dimensions or rows.")
	}
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
