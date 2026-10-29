package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
)

// The CLI's client for /secrets, and why the command has one at all.
//
// The company's secrets live on the coordination KV so every node reads the
// same rows. On the default topology that KV is inside the engine's own
// process and does not listen on a socket, and while the engine runs a second
// broker on its store directory is refused — so the running node is the only
// way in. This is how `crewlet secrets` reaches it.
//
// # It is not a fallback, it is the primary path
//
// A fleet's engine is running. With this node's engine STOPPED, the command
// reaches the same store itself instead (fleetstore.go), and a node that
// holds no data, or a member of a cluster, still goes through a running one.

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

// newSecretsClient builds a client for the node this Tier A describes, for the
// run described, whose use of the store decides the bearer it asks for
// ([storeRun.token]). boot may be nil where override names the node.
//
// The BIND ADDRESS is what Tier A carries, and a bind address is not always a
// reachable one: 0.0.0.0 and :: mean "every interface", which as a
// destination means nothing at all. They resolve to loopback here, because
// this command runs on the node whose config it just read — and `-api` is
// there for the case where it does not.
func newSecretsClient(environ config.Source, boot *config.Bootstrap, override string,
	run storeRun) (*secretsClient, error) {

	base, err := nodeBaseURL(boot, override, "the fleet's secret store")
	if err != nil {
		return nil, err
	}
	token, err := run.token(environ)
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
//
// A VALUE PAST [secrets.MaxValueBytes] IS REFUSED BEFORE IT IS SENT, in the
// words the fleet's store refuses a direct write in: the node would refuse it
// too, with a 413 that names a body rather than a value, after the whole of it
// had crossed the network.
func (c *secretsClient) Set(ctx context.Context, name, value string, _ secrets.Author,
	_ secrets.Source, _ time.Time) (storedSecret, error) {

	if err := secrets.CheckValue(value); err != nil {
		return storedSecret{}, err
	}
	var body struct {
		UpdatedBy     string              `json:"updated_by"`
		UpdatedByKind string              `json:"updated_by_kind"`
		OperatorID    string              `json:"operator_id"`
		ReadBy        []string            `json:"read_by"`
		Reload        *configapi.Reloaded `json:"reload"`
	}
	if err := c.call(ctx, http.MethodPut, "/secrets/"+url.PathEscape(name),
		[]byte(value), &body); err != nil {
		return storedSecret{}, err
	}
	return storedSecret{
		Author: secrets.Author{Name: body.UpdatedBy, Kind: body.UpdatedByKind,
			OperatorID: body.OperatorID},
		ReadBy: map[string][]string{name: body.ReadBy}, Reload: body.Reload,
	}, nil
}

// SetMany stores several values through `PUT /secrets`, which seals them all
// and re-activates the revision that reads any of them once (ADR-0070). The
// node stamps the author, as on [secretsClient.Set], and its answer for a
// batch names none, since it is the caller's own credential.
func (c *secretsClient) SetMany(ctx context.Context, values map[string]string, _ secrets.Author,
	_ secrets.Source, _ time.Time, deferReload bool) (storedSecret, error) {

	if len(values) > secretsapi.MaxRevealNames {
		return storedSecret{}, fmt.Errorf("%d values, and one write stores at most %d",
			len(values), secretsapi.MaxRevealNames)
	}
	for name, value := range values {
		if err := secrets.CheckValue(value); err != nil {
			return storedSecret{}, fmt.Errorf("%s: %w", name, err)
		}
	}
	body := map[string]any{"values": values}
	if deferReload {
		body["defer_reload"] = true
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return storedSecret{}, err
	}
	var answer struct {
		ReadBy map[string][]string `json:"read_by"`
		Reload *configapi.Reloaded `json:"reload"`
	}
	if err := c.callWithin(ctx, http.MethodPut, "/secrets", "application/json", raw, &answer,
		maxRowAnswerBytes, "no answer about one write is this long"); err != nil {
		return storedSecret{}, err
	}
	return storedSecret{ReadBy: answer.ReadBy, Reload: answer.Reload}, nil
}

// Get reads one value back. Break-glass, and the node logs it by name.
//
// `crewlet secrets get -reveal` is its one caller: a run reading the values a
// command resolves asks for them together ([secretsClient.Values]).
//
// READ TO THE LENGTH THE ROUTE CAN ANSWER FOR THIS NAME
// ([secretsapi.RevealAnswerBytes]), which is not the write's limit: the value
// comes back as a JSON string, which spells each byte it escapes in six, so a
// reader held to the stored length refused a value the store had taken.
func (c *secretsClient) Get(ctx context.Context, name string) (string, error) {
	var body struct {
		Value string `json:"value"`
	}
	err := c.callWithin(ctx, http.MethodGet, "/secrets/"+url.PathEscape(name)+"?reveal=true",
		"", nil, &body, secretsapi.RevealAnswerBytes(name),
		"a credential this long is not one this build stores, and a clipped one "+
			"would be worse than none")
	if err != nil {
		return "", err
	}
	return body.Value, nil
}

// Values reads the values held under the names asked for through the node's
// POST /secrets/reveal, in requests of at most [secretsapi.MaxRevealNames], so
// a set is one request or a few and never one per name. A name nothing holds
// is absent from the answer, and only the names asked come back, whatever the
// node answers beside them.
//
// It is the BREAK-GLASS READ, as [secretsClient.Get] is: the node decides it
// on `secrets.reveal` — admin, a recent proof and a person present, so a
// machine token is refused it — logs every name each request asked for in
// one line (`secrets_revealed`), and refuses the whole of a request one bad
// name is in. A draining node answers 503, for any token.
//
// A REFUSAL OR AN UNREACHABLE NODE FAILS THE WHOLE READ, the values of the
// requests before it included, and the [nodeRefusal] is kept for the caller
// to tell a refused credential from a failing node ([refusedTheToken]): a
// value missing because the node could not be asked is not a value the fleet
// does not hold, and nothing here fills it in.
func (c *secretsClient) Values(ctx context.Context, names []string) (map[string]string, error) {
	out := make(map[string]string, len(names))
	for batch := range slices.Chunk(names, secretsapi.MaxRevealNames) {
		request, err := json.Marshal(map[string][]string{"names": batch})
		if err != nil {
			return nil, err
		}
		var body struct {
			Values map[string]string `json:"values"`
		}
		if err := c.callWithin(ctx, http.MethodPost, "/secrets/reveal", "application/json",
			request, &body, secretsapi.MaxRevealAnswerBytes,
			"no request of this build's size can be answered at that length"); err != nil {
			return nil, err
		}
		for _, name := range batch {
			if value, held := body.Values[name]; held {
				out[name] = value
			}
		}
	}
	return out, nil
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
//
// READ TO THE LISTING'S BOUND ([maxListingBytes]), not one row's: the answer
// names every row the rekey moved, so it grows with the company as the listing
// does. Held to the bound every one-value answer shared (68 KiB), a rotation
// the node had finished was reported as a failure once its names passed it — a
// few thousand of them.
func (c *secretsClient) Rekey(ctx context.Context, activeKeyID string) (rotation, error) {
	var body struct {
		Moved      []string                     `json:"moved"`
		EngineKeys int                          `json:"engine_keys_moved"`
		Identity   *secretsapi.IdentityResealed `json:"identity"`
	}
	err := c.callWithin(ctx, http.MethodPost,
		"/secrets/rekey?key_id="+url.QueryEscape(activeKeyID), "", nil, &body, maxListingBytes,
		"a rekey this long names more secrets than this build reads at once")
	if err != nil {
		return rotation{}, err
	}
	return rotation{
		Rekeyed:  fleetsecrets.Rekeyed{Moved: body.Moved, EngineKeys: body.EngineKeys},
		Identity: body.Identity,
	}, nil
}

// EngineKeys reads the counts the node's listing carries beside the operator's
// names: the engine's own keys, the identity estate's values and the
// assistant's, per keyring key, none of them named.
func (c *secretsClient) EngineKeys(ctx context.Context) (engineHeld, error) {
	var body struct {
		EngineKeys secrets.EngineKeys           `json:"engine_keys"`
		Identity   *iamdomain.SealedCount       `json:"identity_values"`
		Assistant  *assistantdomain.SealedCount `json:"assistant_values"`
	}
	if err := c.listing(ctx, &body); err != nil {
		return engineHeld{}, err
	}
	return engineHeld{Keys: body.EngineKeys, Identity: body.Identity,
		Assistant: body.Assistant}, nil
}

// call performs one request whose body, where it has one, is a credential, and
// whose answer is about one row — the author a write was stamped with, whether
// an unset removed anything — read to [maxRowAnswerBytes], and decodes it, or
// explains the refusal.
//
// OCTET-STREAM, because the body IS the credential: bytes, not a document.
// Labelling it text/plain would invite a proxy to re-encode a value the vendor
// will compare byte for byte.
func (c *secretsClient) call(ctx context.Context, method, path string, body []byte, out any) error {
	return c.callWithin(ctx, method, path, "application/octet-stream", body, out,
		maxRowAnswerBytes,
		"no answer about one row is this long, and a clipped one would be worse "+
			"than none")
}

// listing reads GET /secrets, which names every stored secret and holds no
// value, under its own bound ([maxListingBytes]).
func (c *secretsClient) listing(ctx context.Context, out any) error {
	return c.callWithin(ctx, http.MethodGet, "/secrets", "", nil, out, maxListingBytes,
		"a listing this long names more secrets than this build reads at once")
}

// callWithin performs one request carrying a body of contentType, where it
// has one, refusing an answer longer than limit with why such an answer is not
// one to read, and decodes it, or explains the refusal.
func (c *secretsClient) callWithin(ctx context.Context, method, path, contentType string,
	body []byte, out any, limit int, tooLong string) error {
	return c.callWith(ctx, method, path, contentType, nil, body, out, limit, tooLong)
}

// callWith is [secretsClient.callWithin] with headers of the caller's own.
func (c *secretsClient) callWith(ctx context.Context, method, path, contentType string,
	header http.Header, body []byte, out any, limit int, tooLong string) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	for name, values := range header {
		req.Header[name] = values
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reaching %s: %w\n\nthe node's API is where the "+
			"fleet's secret store is written; check it is running and that "+
			"-api names its address", c.base, err)
	}
	defer resp.Body.Close()
	// BOUNDED, AND REFUSED PAST THE BOUND, by the longest answer the route
	// can give: one row's metadata ([maxRowAnswerBytes]), one credential as
	// JSON spells it ([secretsapi.RevealAnswerBytes]), the credentials of one
	// batch of names ([secretsapi.MaxRevealAnswerBytes]), or every row's name
	// ([maxListingBytes]).
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
		return c.refusal(resp.StatusCode, path, resp.Header, raw)
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

// maxRowAnswerBytes bounds an answer about one row, and the refusal of a route
// that answers one: the author a write was stamped with, whether an unset
// removed anything, and the engine's envelope for a refusal of either — each
// well under a kibibyte. Sixty-four kibibytes is room for the error page a
// proxy in front of the node answers in their place, so that its status still
// reaches the operator.
//
// NOT a credential's bound, which every route once shared: a revealed value
// comes back as JSON spells it, up to six times its stored length, and is read
// to its own ([secretsapi.RevealAnswerBytes]).
const maxRowAnswerBytes = 64 << 10

// maxListingBytes bounds an answer naming every row: GET /secrets, and the
// names POST /secrets/rekey moved.
//
// NOT a credential's bound, which the listing used to share: a row is
// some 224 bytes and a name, so a fleet of about 300 stored values passed it,
// and every command that resolves a company through a running node — they
// all start with the listing — failed before reading one, blaming a
// credential. 64 MiB is some 290,000 rows: ten thousand seats holding a
// credential at each of the eight surfaces stay under a third of it. And it
// stays a bound, so a node answering garbage cannot make this process read
// without end.
const maxListingBytes = 64 << 20

// refusal turns a status code into something an operator can act on.
func (c *secretsClient) refusal(status int, path string, header http.Header, raw []byte) error {
	own := ownBody(header, raw)
	var body struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
		Hint   string `json:"hint"`
	}
	_ = json.Unmarshal(own, &body)
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
	if own != nil {
		if msg, ok := credentialRefusal(status, own, true); ok {
			return &nodeRefusal{Status: status, Code: body.Error, msg: c.base + ": " + msg}
		}
	}
	msg := body.Error
	if msg == "" {
		msg = foreignAnswer(header.Get("Content-Type"), raw)
	}
	// A [nodeRefusal], so a caller can tell a refused credential from a
	// node that is failing ([refusedTheToken]): the remedy for the first is
	// the token, and for the second the node.
	return &nodeRefusal{Status: status, Code: body.Error, detail: body.Detail, hint: body.Hint,
		msg: fmt.Sprintf("%s answered %d for %s: %s",
			c.base, status, path, withRefusalDetail(msg, body.Detail, body.Hint))}
}

// refusedTheToken reports whether err is the node refusing the credential
// this command sent: 401, or 403. A node that is down, draining or failing
// answers the same for any token, and naming the token then sends an operator
// to change one that works.
func refusedTheToken(err error) bool {
	var refusal *nodeRefusal
	return errors.As(err, &refusal) &&
		(refusal.Status == http.StatusUnauthorized || refusal.Status == http.StatusForbidden)
}

// Reload re-activates the company the node runs, unchanged (`POST
// /config/reload`): what a provisioning run that sealed its values with the
// re-activation deferred sends once at its end, so the running company reads
// them. It seals nothing, so a value rotated meanwhile is never overwritten.
func (c *secretsClient) Reload(ctx context.Context, summary string) (*configapi.Reloaded, error) {
	var answer struct {
		RevisionID string `json:"revision_id"`
		Epoch      int64  `json:"epoch"`
	}
	if err := c.callWith(ctx, http.MethodPost, "/config/reload", "",
		http.Header{"X-Summary": {summary}}, nil, &answer, maxRowAnswerBytes,
		"no answer about one re-activation is this long"); err != nil {
		return nil, err
	}
	return &configapi.Reloaded{RevisionID: answer.RevisionID, Epoch: answer.Epoch}, nil
}
