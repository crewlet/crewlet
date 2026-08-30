package atlassian

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The HTTP shape both products and the admin plane share.
//
// This was three near-identical round trips — Jira's, Confluence's, and the
// one a provisioner would have added — differing only in the prefix on their
// error messages. One of them had already drifted: the cloud-host list Jira
// derives its deployment from carried `.atlassian.com` and Confluence's did
// not, so the same Cloud gateway address was Cloud to one product and Data
// Center to the other.

// ClientTimeout bounds one request.
//
// Ten seconds, and the number is the tracker's rather than a fresh guess: the
// watcher lookup runs INSIDE the inbound consumer, before a delivery is
// acked, so a slow instance stalls the fleet's whole notification path rather
// than one turn. Generous for a single-page read and short enough that a hung
// instance costs one round of deliveries rather than a redelivery storm.
const ClientTimeout = 10 * time.Second

// CloudHosts are the domains only Atlassian Cloud answers on.
//
// ONE list, because it is the whole of how an address names its own
// deployment and two copies had already disagreed. `.atlassian.com` covers
// the api.atlassian.com gateway a cloud id resolves to, which is Cloud by
// definition — the gateway does not exist for Data Center.
var CloudHosts = []string{".atlassian.net", ".atlassian.com", ".jira.com"}

// IsCloud reports an address Atlassian itself hosts.
//
// FALSE for an address that cannot be parsed, and that is the safe direction:
// Data Center is the conservative answer everywhere it is consulted — it
// selects the older REST version, which fails loudly with the version in the
// path, and it refuses provisioning rather than calling an admin API that is
// not there.
func IsCloud(base string) bool {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, suffix := range CloudHosts {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// AuthHeader is the one place the two schemes are chosen between.
//
// Cloud rejects a bare bearer API token and Data Center rejects Basic with an
// empty user, so the presence of an EMAIL is the whole discriminator — and it
// is the field an operator already has to set correctly for their deployment.
// Deriving it from the address instead would break the real case of a Data
// Center instance fronted by Atlassian-style auth.
func AuthHeader(email, token string) string {
	if email == "" {
		return "Bearer " + token
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token))
}

// APIError is a refusal from Atlassian, typed.
//
// A caller deciding what a refusal MEANS — 404 is "no such project", 403 is
// "this credential cannot see it", 401 is "the credential is wrong" — would
// otherwise substring-match a message whose wording differs by product, by
// version and by locale.
//
// Surface names the caller: "jira", "confluence", or the admin service. It is
// carried rather than hardcoded because one error type now serves three
// planes, and an operator reading a bare status has no way to tell which of
// them refused them.
type APIError struct {
	Surface string
	Method  string
	Path    string
	Status  int
	Detail  string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s: %s %s: %d", e.Surface, e.Method, e.Path, e.Status)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// StatusOf reads the HTTP status off an error, or 0 where there is none.
//
// The alternative at every call site is an errors.As plus a nil check, and
// the version that forgets the nil check compiles.
func StatusOf(err error) int {
	var api *APIError
	if !errors.As(err, &api) {
		return 0
	}
	return api.Status
}

// Transport is one authenticated conversation with an Atlassian surface.
//
// It carries no notion of a product or of an API version: those are the
// caller's, because the three planes disagree about them and a transport that
// tried to know would need a switch for every one.
type Transport struct {
	// Surface names this transport in an error and nowhere else.
	Surface string
	// Base is the address every path is appended to, already trimmed.
	Base string
	// Auth is the whole Authorization header value.
	Auth string
	// HTTP is the client. Never nil after [NewTransport].
	HTTP *http.Client
}

// NewTransport builds a transport, refusing the two inputs that produce a
// client which fails only at its first request.
func NewTransport(surface, base, auth string, client *http.Client) (*Transport, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil, fmt.Errorf("%s: no instance url", surface)
	}
	if strings.TrimSpace(auth) == "" {
		return nil, fmt.Errorf("%s: no credential", surface)
	}
	if client == nil {
		client = &http.Client{Timeout: ClientTimeout}
	}
	return &Transport{Surface: surface, Base: base, Auth: auth, HTTP: client}, nil
}

// Do runs one request against a path already carrying its own prefix.
//
// The error body is read to 2048 bytes and no further: it is going into a log
// line and an operator's terminal, and Atlassian answers some refusals with a
// whole HTML page. An unwanted success body is drained to 1 MiB so the
// connection can be reused, and no further, so a runaway response cannot hold
// this goroutine.
func (t *Transport) Do(ctx context.Context, method, path string, params url.Values, in, out any) error {
	target := t.Base + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	var payload io.Reader
	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%s: encode %s: %w", t.Surface, path, err)
		}
		payload = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", t.Surface, path, err)
	}
	req.Header.Set("Authorization", t.Auth)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %s: %w", t.Surface, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return &APIError{
			Surface: t.Surface, Method: method, Path: path,
			Status: resp.StatusCode, Detail: strings.TrimSpace(string(detail)),
		}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s: decode %s: %w", t.Surface, path, err)
	}
	return nil
}
