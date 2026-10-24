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

	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iamdomain"
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
func newSecretsClient(environ config.Source, boot *config.Bootstrap, override string) (*secretsClient, error) {
	base, err := nodeBaseURL(boot, override, "the fleet's secret store")
	if err != nil {
		return nil, err
	}
	token, err := nodeAPIToken(environ, "/secrets")
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
			Name          string         `json:"name"`
			KeyID         string         `json:"key_id"`
			UpdatedAt     string         `json:"updated_at"`
			UpdatedBy     string         `json:"updated_by"`
			UpdatedByKind string         `json:"updated_by_kind"`
			OperatorID    string         `json:"operator_id"`
			Source        secrets.Source `json:"source"`
		} `json:"secrets"`
	}
	if err := c.listing(ctx, &body); err != nil {
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
			UpdatedBy: row.UpdatedBy, UpdatedByKind: row.UpdatedByKind,
			OperatorID: row.OperatorID, Source: row.Source,
		})
	}
	return out, nil
}

// Set stores or rotates one value, and reports the author the node recorded.
//
// NEITHER `by` NOR `source` IS SENT. The node stamps the party its own guard
// authenticated, which is the only attribution that means anything on this
// path — a client-supplied author would be a field the caller chooses — and
// its route's own source, `api`, and it refuses a write that names either:
// the route takes no query at all. It answers with what it stamped, which is
// what the command prints.
func (c *secretsClient) Set(ctx context.Context, name, value string, _ secrets.Author,
	_ secrets.Source, _ time.Time) (secrets.Author, error) {

	var body struct {
		UpdatedBy     string `json:"updated_by"`
		UpdatedByKind string `json:"updated_by_kind"`
		OperatorID    string `json:"operator_id"`
	}
	if err := c.call(ctx, http.MethodPut, "/secrets/"+url.PathEscape(name),
		[]byte(value), &body); err != nil {
		return secrets.Author{}, err
	}
	return secrets.Author{Name: body.UpdatedBy, Kind: body.UpdatedByKind,
		OperatorID: body.OperatorID}, nil
}

// Get reads one value back. Break-glass, and the node logs it by name.
func (c *secretsClient) Get(ctx context.Context, name string) (string, error) {
	var body struct {
		Value string `json:"value"`
	}
	err := c.call(ctx, http.MethodGet,
		"/secrets/"+url.PathEscape(name)+"?reveal=true", nil, &body)
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
	err := c.call(ctx, http.MethodDelete, "/secrets/"+url.PathEscape(name), nil, &body)
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
func (c *secretsClient) Rekey(ctx context.Context, activeKeyID string) (rotation, error) {
	var body struct {
		Moved      []string                     `json:"moved"`
		EngineKeys int                          `json:"engine_keys_moved"`
		Identity   *secretsapi.IdentityResealed `json:"identity"`
	}
	err := c.call(ctx, http.MethodPost,
		"/secrets/rekey?key_id="+url.QueryEscape(activeKeyID), nil, &body)
	if err != nil {
		return rotation{}, err
	}
	return rotation{
		Rekeyed:  fleetsecrets.Rekeyed{Moved: body.Moved, EngineKeys: body.EngineKeys},
		Identity: body.Identity,
	}, nil
}

// EngineKeys reads the counts the node's listing carries beside the operator's
// names: the engine's own keys and the identity estate's values, per keyring
// key, none of them named.
func (c *secretsClient) EngineKeys(ctx context.Context) (engineHeld, error) {
	var body struct {
		EngineKeys secrets.EngineKeys     `json:"engine_keys"`
		Identity   *iamdomain.SealedCount `json:"identity_values"`
	}
	if err := c.listing(ctx, &body); err != nil {
		return engineHeld{}, err
	}
	return engineHeld{Keys: body.EngineKeys, Identity: body.Identity}, nil
}

// call performs one request whose answer is at most one credential, and
// decodes it, or explains the refusal.
func (c *secretsClient) call(ctx context.Context, method, path string, body []byte, out any) error {
	return c.callWithin(ctx, method, path, body, out, maxSecretResponseBytes,
		"a credential this long is not one this build stores, and a clipped "+
			"one would be worse than none")
}

// listing reads GET /secrets, which names every stored secret and holds no
// value, under its own bound ([maxListingBytes]).
func (c *secretsClient) listing(ctx context.Context, out any) error {
	return c.callWithin(ctx, http.MethodGet, "/secrets", nil, out, maxListingBytes,
		"a listing this long names more secrets than this build reads at once")
}

// callWithin performs one request, refusing an answer longer than limit with
// why such an answer is not one to read, and decodes it, or explains the
// refusal.
func (c *secretsClient) callWithin(ctx context.Context, method, path string, body []byte,
	out any, limit int, tooLong string) error {
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
	// BOUNDED, AND REFUSED PAST THE BOUND, by what the route returns: a
	// small JSON object or one credential, whose value is capped by the same
	// limit the write is, or the listing ([maxListingBytes]).
	//
	// The refusal is what the bound needs to be worth having: io.LimitReader
	// stops at its cap and reports a clean EOF, so an over-long answer used
	// to arrive CLIPPED — and a clipped credential is one this command prints
	// for an operator to paste somewhere, silently missing its tail.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return fmt.Errorf("reading the answer from %s: %w", c.base, err)
	}
	if len(raw) > limit {
		return fmt.Errorf("the node's answer to %s exceeded %d bytes, so it was not "+
			"read: %s", path, limit, tooLong)
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

// maxListingBytes bounds the answer to GET /secrets.
//
// NOT the credential bound above, which the listing used to share: a row is
// some 224 bytes and a name, so a fleet of about 300 stored values passed it,
// and every command that resolves a company through a running node — they
// all start with the listing — failed before reading one, blaming a
// credential. 64 MiB is some 290,000 rows: ten thousand seats holding a
// credential at each of the eight surfaces stay under a third of it. And it
// stays a bound, so a node answering garbage cannot make this process read
// without end.
const maxListingBytes = 64 << 20


// refusal turns a status code into something an operator can act on.
func (c *secretsClient) refusal(status int, path, contentType string, raw []byte) error {
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
		Hint   string `json:"hint"`
	}
	_ = json.Unmarshal(raw, &body)
	switch {
	case status == http.StatusNotFound && body.Error == "not_found":
		// THE SENTINEL, so a caller can tell "no such secret" from
		// "the node refused". `secrets get` prints one and reports the
		// other, and the provisioning sink treats absence as a value to
		// mint rather than as a failure to abort on.
		return fmt.Errorf("%w: %s", secrets.ErrNotFound,
			strings.TrimPrefix(strings.SplitN(path, "?", 2)[0], "/secrets/"))
	case status == http.StatusNotFound:
		return missingSurface(c.base, "/secrets", body.Error)
	case status == http.StatusForbidden && body.Error == "reserved_name":
		// THE SENTINEL again, for the same reason: the name is the
		// engine's own, which no grant changes — a caller that read this
		// as a missing grant would go and ask for one.
		return fmt.Errorf("%w: %s", secrets.ErrReservedName,
			withRefusalDetail(strings.TrimPrefix(
				strings.SplitN(path, "?", 2)[0], "/secrets/"), "", body.Hint))
	}
	if msg, ok := credentialRefusal(status, raw, true); ok {
		return fmt.Errorf("%s: %s", c.base, msg)
	}
	msg := body.Error
	if msg == "" {
		msg = foreignAnswer(contentType, raw)
	}
	return fmt.Errorf("%s answered %d for %s: %s", c.base, status, path,
		withRefusalDetail(msg, body.Detail, body.Hint))
}
