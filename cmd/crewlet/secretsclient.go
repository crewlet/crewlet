package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
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

// apiTokenEnv is where the CLI reads a bearer token that is not in Tier A.
//
// AN ENVIRONMENT VARIABLE, never a flag: a token on argv is in the shell
// history and in `ps` output for every user on the host, which is the same
// reason `secrets set` reads its value from stdin.
const apiTokenEnv = "CREWLET_API_TOKEN"

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
	base := strings.TrimSpace(override)
	if base == "" {
		if boot.API.Port == 0 {
			return nil, errors.New(
				"this node's api.port is 0, so it serves no HTTP surface and " +
					"there is no way to reach the fleet's secret store; set " +
					"api.port, or pass -api URL for a node that has one")
		}
		host := strings.TrimSpace(boot.API.Host)
		switch host {
		case "", "0.0.0.0", "::", "[::]":
			host = "127.0.0.1"
		}
		base = "http://" + net.JoinHostPort(host, strconv.Itoa(boot.API.Port))
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("%q is not a URL the API can be reached at "+
			"(want something like http://127.0.0.1:8080)", base)
	}
	token := strings.TrimSpace(os.Getenv(apiTokenEnv))
	if token == "" && !boot.API.Auth.Disabled {
		// THE FIRST configured token, and it is not an arbitrary pick:
		// Tier A's token list is what THIS node accepts, so any entry
		// authenticates. The id is stamped as the author of the write,
		// which is why the environment variable exists — an operator who
		// wants their own attribution supplies their own token.
		if len(boot.API.Auth.Tokens) == 0 {
			return nil, fmt.Errorf(
				"this node lists no api.auth.tokens, so nothing can authenticate "+
					"to its /secrets surface; add one, or export %s",
				apiTokenEnv)
		}
		token = boot.API.Auth.Tokens[0].Token
	}
	return &secretsClient{
		base:  strings.TrimRight(parsed.String(), "/"),
		token: token,
		http:  &http.Client{Timeout: apiTimeout},
	}, nil
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
	if err := c.call(ctx, http.MethodGet, "/secrets", nil, &body); err != nil {
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
	return c.call(ctx, http.MethodPut, path, []byte(value), nil)
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
func (c *secretsClient) Rekey(ctx context.Context, activeKeyID, _ string, _ time.Time) ([]string, error) {
	var body struct {
		Moved []string `json:"moved"`
	}
	err := c.call(ctx, http.MethodPost,
		"/secrets/rekey?key_id="+url.QueryEscape(activeKeyID), nil, &body)
	if err != nil {
		return nil, err
	}
	return body.Moved, nil
}

// call performs one request and decodes the answer, or explains the refusal.
func (c *secretsClient) call(ctx context.Context, method, path string, body []byte, out any) error {
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
	// BOUNDED, AND REFUSED PAST THE BOUND. Every answer here is a small JSON
	// object or one credential; the reveal route's value is capped by the
	// same limit the write is, and nothing else this client reads is larger.
	//
	// The refusal is what the bound needs to be worth having: io.LimitReader
	// stops at its cap and reports a clean EOF, so an over-long answer used
	// to arrive CLIPPED — and a clipped credential is one this command prints
	// for an operator to paste somewhere, silently missing its tail.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSecretResponseBytes+1))
	if err != nil {
		return fmt.Errorf("reading the answer from %s: %w", c.base, err)
	}
	if len(raw) > maxSecretResponseBytes {
		return fmt.Errorf(
			"the node's answer to %s exceeded %d bytes, so it was not read: a "+
				"credential this long is not one this build stores, and a clipped "+
				"one would be worse than none", path, maxSecretResponseBytes)
	}
	if resp.StatusCode/100 != 2 {
		return c.refusal(resp.StatusCode, path, raw)
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
func (c *secretsClient) refusal(status int, path string, raw []byte) error {
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
		return fmt.Errorf("%s has no /secrets surface: it is running a build "+
			"from before secrets moved onto the fleet, or it cannot reach the "+
			"coordination store", c.base)
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%s refused the bearer token: set %s to one of its "+
			"api.auth.tokens", c.base, apiTokenEnv)
	}
	msg := body.Error
	if msg == "" {
		msg = strings.TrimSpace(string(raw))
	}
	for _, extra := range []string{body.Detail, body.Hint} {
		if extra != "" {
			msg += "\n  " + extra
		}
	}
	return fmt.Errorf("%s answered %d for %s: %s", c.base, status, path, msg)
}
