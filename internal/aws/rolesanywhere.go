package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	awssdk "github.com/aws/aws-sdk-go-v2/aws"

	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/sigv4"
)

// Throttling and server errors are retried with a doubling, jittered wait, as
// AWS's own SDK retryer does; a throttled request was not processed, so a
// retry is safe. Eight attempts span about a minute at the ceiling, the bound
// the live probe needed when IAM throttled a burst.
const (
	maxAttempts  = 8
	retryBase    = 500 * time.Millisecond
	retryCeiling = 20 * time.Second
)

// APIError is a refusal from an AWS endpoint the SDK does not reach.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("aws: %d %s: %s", e.Status, e.Code, e.Message) }

// transient reports an answer worth retrying: a throttle under each name a
// service gives one (SQS answers its own with a 403), or a server's fault.
func (e *APIError) transient() bool {
	switch e.Code {
	case "Throttling", "ThrottlingException", "TooManyRequestsException", "ServiceUnavailable", "InternalFailure",
		"RequestLimitExceeded", "RequestThrottled", "SlowDown", "PriorRequestNotComplete":
		return true
	}
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// session is a role session Roles Anywhere vended.
type session struct {
	creds          awssdk.Credentials
	sourceIdentity string
}

// rolesAnywhere signs CreateSession with a leaf of the company's CA.
type rolesAnywhere struct {
	http   *http.Client
	region string
	// endpoint and wait are seams for tests.
	endpoint func(host string) string
	wait     func(attempt int) time.Duration
	now      func() time.Time
}

func newRolesAnywhere(hc *http.Client, region string) rolesAnywhere {
	return rolesAnywhere{
		http: hc, region: region,
		endpoint: func(host string) string { return "https://" + host },
		wait: func(attempt int) time.Duration {
			return backoff.Jitter(backoff.Doubling(attempt, retryBase, retryCeiling), 0.5)
		},
		now: time.Now,
	}
}

// createSession mints an ECDSA leaf for cn and exchanges it, through
// profile, for an hour of role's credentials. The session's source identity is "CN=" + cn.
func (r rolesAnywhere) createSession(ctx context.Context, ca *CA, cn string, profile, role, anchor string) (session, error) {
	body, err := json.Marshal(map[string]any{
		"durationSeconds": sessionSeconds, "profileArn": profile, "roleArn": role, "trustAnchorArn": anchor,
	})
	if err != nil {
		return session{}, err
	}
	for attempt := 1; ; attempt++ {
		s, err := r.once(ctx, ca, cn, body)
		var api *APIError
		if err == nil || !errors.As(err, &api) || !api.transient() || attempt == maxAttempts {
			return s, err
		}
		select {
		case <-ctx.Done():
			return session{}, ctx.Err()
		case <-time.After(r.wait(attempt)):
		}
	}
}

// once signs and sends one attempt; each attempt gets a fresh leaf and date.
func (r rolesAnywhere) once(ctx context.Context, ca *CA, cn string, body []byte) (session, error) {
	now := r.now().UTC()
	leaf, key, err := ca.Leaf(cn, now, LeafECDSA)
	if err != nil {
		return session{}, err
	}
	host := "rolesanywhere." + r.region + ".amazonaws.com"
	hdr := http.Header{
		"Content-Type":   {"application/json"},
		"X-Amz-Date":     {now.Format(sigv4.DateFormat)},
		sigv4.HeaderX509: {sigv4.X509Header(leaf)},
	}
	auth, err := sigv4.SignX509(sigv4.Request{Method: http.MethodPost, Host: host, Path: "/sessions", Header: hdr,
		PayloadHash: sigv4.PayloadHash(body)}, leaf, key, r.region, now)
	if err != nil {
		return session{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint(host)+"/sessions", bytes.NewReader(body))
	if err != nil {
		return session{}, err
	}
	req.Header = hdr
	req.Host = host
	req.Header.Set("Authorization", auth)
	resp, err := r.http.Do(req)
	if err != nil {
		return session{}, fmt.Errorf("aws: CreateSession: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return session{}, err
	}
	if resp.StatusCode >= 300 {
		return session{}, apiError(resp, data)
	}
	var out struct {
		CredentialSet []struct {
			Credentials struct {
				AccessKeyID     string    `json:"accessKeyId"`
				SecretAccessKey string    `json:"secretAccessKey"`
				SessionToken    string    `json:"sessionToken"`
				Expiration      time.Time `json:"expiration"`
			} `json:"credentials"`
			SourceIdentity string `json:"sourceIdentity"`
		} `json:"credentialSet"`
	}
	if err := json.Unmarshal(data, &out); err != nil || len(out.CredentialSet) == 0 {
		return session{}, errors.New("aws: CreateSession answered no credential set")
	}
	cs := out.CredentialSet[0]
	return session{
		creds: awssdk.Credentials{AccessKeyID: cs.Credentials.AccessKeyID, SecretAccessKey: cs.Credentials.SecretAccessKey,
			SessionToken: cs.Credentials.SessionToken, CanExpire: true, Expires: cs.Credentials.Expiration, Source: "RolesAnywhere"},
		sourceIdentity: cs.SourceIdentity,
	}, nil
}

// apiError reads Roles Anywhere's JSON refusal and its error-type header.
func apiError(resp *http.Response, data []byte) *APIError {
	e := &APIError{Status: resp.StatusCode, Code: resp.Header.Get("X-Amzn-Errortype")}
	var body struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &body) == nil {
		e.Message = body.Message
		if e.Code == "" {
			e.Code = body.Type
		}
	}
	if i := strings.LastIndex(e.Code, "#"); i >= 0 {
		e.Code = e.Code[i+1:]
	}
	if i := strings.Index(e.Code, ":"); i >= 0 {
		e.Code = e.Code[:i]
	}
	if e.Code == "" {
		e.Code = strings.ReplaceAll(http.StatusText(resp.StatusCode), " ", "")
	}
	return e
}
