package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/secrets"
)

// The CLI's client for /secrets, and why the command has one at all.
//
// The company's secrets live on the coordination KV so every node reads the
// same rows. On the default topology that KV is inside the
// engine's own process and does not listen on a socket, so a second process
// cannot write it — the running node is the only thing that can. This is how
// `crewlet secrets` reaches it.
//
// # It is not a fallback, it is the primary path
//
// A fleet's engine is running. The local table is what a STOPPED node can
// write, and the engine migrates those rows onto the fleet at its next start
// — see [github.com/crewlet/crewlet/internal/fleetsecrets.Migrate].

// apiTimeout bounds one call to the node's API.
//
// Every route this client calls is a single KV round trip against a broker
// the node is already connected to, so a healthy answer is milliseconds. 30 s
// is long enough that a JetStream re-election (a few seconds) does not fail
// an operator's rotation, and short enough that a node which is up but wedged
// reports that rather than hanging a terminal indefinitely.
const apiTimeout = 30 * time.Second

// secretsClient talks to a running node's /secrets surface.
type secretsClient struct {
	base  string
	token string
	http  *http.Client
}

// newSecretsClient builds a client for the node this Tier A describes.
//
// The BIND ADDRESS is what Tier A carries, and a bind address is not always a
// reachable one: 0.0.0.0 and :: mean "every interface", which as a
// destination means nothing at all. They resolve to loopback here, because
// this command runs on the node whose config it just read — and `-api` is
// there for the case where it does not.
func newSecretsClient(boot *config.Bootstrap, override string) (*secretsClient, error) {
	base, err := nodeBaseURL(boot, override, "the fleet's secret store")
	if err != nil {
		return nil, err
	}
	token, err := nodeAPIToken(boot, "/secrets")
	if err != nil {
		return nil, err
	}
	return &secretsClient{base: base, token: token, http: httpx.Client(apiTimeout)}, nil
}

// Describe names where a write lands, for the line the command prints.
func (c *secretsClient) Describe() string {
	return "the fleet's secret store, through " + c.base + " (every node reads it)"
}

// List implements the listing half of the backend.
func (c *secretsClient) List(ctx context.Context) ([]secrets.Record, error) {
	var body struct {
		Secrets []struct {
			Name      string `json:"name"`
			KeyID     string `json:"key_id"`
			UpdatedAt string `json:"updated_at"`
			UpdatedBy string `json:"updated_by"`
			Source    string `json:"source"`
		} `json:"secrets"`
	}
	if err := c.call(ctx, http.MethodGet, "/secrets", nil, &body, oneAnswer); err != nil {
		return nil, err
	}
	out := make([]secrets.Record, 0, len(body.Secrets))
	for _, row := range body.Secrets {
		// AN UNPARSEABLE TIMESTAMP IS A ZERO TIME, not a refusal: the
		// row exists and its name is the answer the operator wanted,
		// and refusing the whole listing over one formatting oddity
		// would hide every other row.
		at, _ := time.Parse(time.RFC3339Nano, row.UpdatedAt)
		out = append(out, secrets.Record{
			Name: row.Name, KeyID: row.KeyID, UpdatedAt: at,
			UpdatedBy: row.UpdatedBy, Source: row.Source,
		})
	}
	return out, nil
}

// Set stores or rotates one value.
//
// `by` is DELIBERATELY not sent. The node stamps the operator id its own
// guard authenticated, which is the only attribution that means anything on
// this path — a client-supplied author would be a field the caller chooses.
func (c *secretsClient) Set(ctx context.Context, name, value, _, source string, _ time.Time) error {
	path := "/secrets/" + url.PathEscape(name)
	if source != "" {
		path += "?source=" + url.QueryEscape(source)
	}
	return c.call(ctx, http.MethodPut, path, []byte(value), nil, oneAnswer)
}

// Get reads one value back. Break-glass, and the node logs it by name.
func (c *secretsClient) Get(ctx context.Context, name string) (string, error) {
	var body struct {
		Value string `json:"value"`
	}
	err := c.call(ctx, http.MethodGet,
		"/secrets/"+url.PathEscape(name)+"?reveal=true", nil, &body, oneAnswer)
	if err != nil {
		return "", err
	}
	return body.Value, nil
}

// Unset removes one value, reporting whether it was there.
func (c *secretsClient) Unset(ctx context.Context, name string) (bool, error) {
	var body struct {
		Removed bool `json:"removed"`
	}
	err := c.call(ctx, http.MethodDelete, "/secrets/"+url.PathEscape(name), nil, &body, oneAnswer)
	if err != nil {
		return false, err
	}
	return body.Removed, nil
}

// Rekey re-seals every stale row on the node.
//
// The key id travels so the NODE can refuse a mismatch. A CLI whose Tier A
// names a different active key than the node's is an operator rekeying onto a
// key the fleet will not be sealing with — silent success there would report
// a completed rotation over rows sealed under something else.
func (c *secretsClient) Rekey(ctx context.Context, activeKeyID, _ string, _ time.Time) ([]string, error) {
	var body struct {
		Moved []string `json:"moved"`
	}
	err := c.call(ctx, http.MethodPost,
		"/secrets/rekey?key_id="+url.QueryEscape(activeKeyID), nil, &body, oneAnswer)
	if err != nil {
		return nil, err
	}
	return body.Moved, nil
}

// Values reads every value the fleet holds, for a command resolving the
// company document: see [companyResolver] for why it takes them all.
//
// The node logs the read, naming every value's name and the operator its
// guard authenticated.
func (c *secretsClient) Values(ctx context.Context) (map[string]string, error) {
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := c.call(ctx, http.MethodGet, "/secrets?reveal=true", nil, &body, wholeStore); err != nil {
		return nil, err
	}
	if body.Values == nil {
		// A NODE THAT ANSWERED WITHOUT THE FIELD is not a fleet with no
		// secrets: it is a node that did not serve this read, and resolving
		// on as if the store were empty would read every stored credential
		// as unset.
		return nil, fmt.Errorf("%s answered GET /secrets?reveal=true without "+
			"the values it holds; a node on this build answers it", c.base)
	}
	return body.Values, nil
}

// answerBound is how much of one answer this client reads, and why.
type answerBound struct {
	bytes int
	why   string
}

var (
	// oneAnswer bounds every route but the bulk read: a small JSON object,
	// or one credential.
	oneAnswer = answerBound{maxSecretResponseBytes, "a credential this long " +
		"is not one this build stores, and a clipped one would be worse than none"}

	// wholeStore bounds the bulk read at the tree's ceiling for a body that
	// is decoded ([httpx.MaxResponseBody]): it holds every value the fleet
	// does, each under the write limit, so it has no bound of its own short
	// of the store's size.
	wholeStore = answerBound{httpx.MaxResponseBody, "a store this large is " +
		"not read whole, and a clipped one would resolve every value past " +
		"the cut as unset"}
)

// call performs one request and decodes the answer, or explains the refusal.
func (c *secretsClient) call(ctx context.Context, method, path string, body []byte, out any, bound answerBound) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		// OCTET-STREAM, because the body IS the credential: bytes, not a
		// document. Labelling it text/plain would invite a proxy to
		// re-encode a value the vendor will compare byte for byte.
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching %s: %w\n\nthe node's API is where the "+
			"fleet's secret store is written; check it is running and that "+
			"-api names its address", c.base, err)
	}
	defer resp.Body.Close()
	// BOUNDED PER ROUTE, AND REFUSED PAST THE BOUND ([answerBound]). The
	// refusal is what a bound needs to be worth having: io.LimitReader stops
	// at its cap and reports a clean EOF, so a bound that only capped would
	// hand on a CLIPPED answer — a credential this command prints for an
	// operator to paste somewhere, silently missing its tail.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(bound.bytes)+1))
	if err != nil {
		return fmt.Errorf("reading the answer from %s: %w", c.base, err)
	}
	if len(raw) > bound.bytes {
		return fmt.Errorf("the node's answer to %s exceeded %d bytes, so it "+
			"was not read: %s", path, bound.bytes, bound.why)
	}
	if resp.StatusCode/100 != 2 {
		return c.refusal(resp.StatusCode, path, resp.Header.Get("Content-Type"), raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("the node answered %s with something this build "+
			"cannot read: %w", path, err)
	}
	return nil
}

// maxSecretResponseBytes bounds one answer.
//
// The write limit plus room for the JSON around it: the largest thing this
// client ever reads back is a revealed credential, and one the node refused
// to store cannot come back out of it.
const maxSecretResponseBytes = (64 << 10) + (4 << 10)

// refusal turns a status code into something an operator can act on.
func (c *secretsClient) refusal(status int, path, contentType string, raw []byte) error {
	var body refusalBody
	decodeRefusal(raw, &body)
	if !body.fromNode() {
		// SHOWN, NEVER INTERPRETED — see [refusalBody.fromNode]. An
		// answer here can be as large as the route's [answerBound], and
		// [unrecognisedRefusal] bounds what of it is shown.
		return fmt.Errorf("%s answered %d for %s: %s", c.base, status, path,
			unrecognisedRefusal(contentType, raw))
	}
	switch {
	case status == http.StatusNotFound && body.Error == "not_found":
		// THE SENTINEL, so a caller can tell "no such secret" from
		// "the node refused". `secrets get` prints one and reports the
		// other, and the provisioning sink treats absence as a value to
		// mint rather than as a failure to abort on.
		return fmt.Errorf("%w: %s", secrets.ErrNotFound,
			strings.TrimPrefix(strings.SplitN(path, "?", 2)[0], "/secrets/"))
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%s refused the bearer token for %s: set %s to one "+
			"of its api.auth.tokens", c.base, path, apiTokenEnv)
	}
	return fmt.Errorf("%s answered %d for %s: %s", c.base, status, path, body.said())
}
