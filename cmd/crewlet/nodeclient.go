package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
)

// Talking to a running node.
//
// Most of this binary's operator commands open a file. These ones cannot: the
// state they act on is the FLEET'S, kept in the coordination store, and on the
// default topology that store is inside the engine's own process. A command
// that opened it from outside would either find nothing (the engine is down,
// and an embedded broker exists only while it runs) or corrupt it (the engine
// is up, and a second JetStream server on one store directory is accepted
// rather than refused).
//
// So they ask the node. Which node is a flag with a sensible default: the
// address this node's own Tier A config says it binds, so the common case is
// running the command beside the config file and naming nothing.

// nodeClient is one running node's HTTP surface.
type nodeClient struct {
	base  string
	token string
	http  *http.Client
}

// nodeRequestTimeout bounds one operator call.
//
// Ten seconds, against routes that are a coordination-store read and a
// handful of writes. Long enough for a broker under load and a fleet-wide
// listing; short enough that an operator who pointed at the wrong address
// learns so rather than watching a cursor. Not configurable BY AN OPERATOR: a
// longer wait never turns a wrong address into a right one, so the one route
// that genuinely needs longer takes it in code. See [nodeClient.patiently].
const nodeRequestTimeout = 10 * time.Second

// patiently returns a client that waits longer for one call.
//
// THE EXCEPTION to the reasoning above, and a narrow one. Every other route
// here answers from memory or a coordination read, so how long it takes is a
// property of the NETWORK and ten seconds is a diagnosis. A backup's duration
// is a property of the DATA — it copies the whole store and every stream — so
// the same ceiling would abandon a working backup on a large company and
// report a failure for work the engine goes on to finish, leaving a complete
// backup on disk that the operator has been told did not happen.
func (c *nodeClient) patiently(limit time.Duration) *nodeClient {
	patient := *c
	patient.http = httpx.Client(limit)
	return &patient
}

// nodeClientFor is the shared "one config argument, then find the node"
// preamble the node-facing operator commands share.
func nodeClientFor(args []string, name string, stderr io.Writer, extra func(*flag.FlagSet)) (*nodeClient, error) {
	bootstrapPath, args := splitSubject(args)

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultBootstrapPath,
		"Tier A config: where this node binds its API")
	addr := fs.String("url", "",
		"the running node's base URL; empty takes it from the config's api block")
	token := fs.String("token", "",
		"bearer token; empty takes "+apiTokenEnv+" — never the config's own "+
			"api.auth.tokens, which is what the node accepts")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	bootstrapPath, given := onePositional(fs, bootstrapPath)
	if given > 1 {
		fmt.Fprintf(stderr, "usage: crewlet %s [<config.yaml>]\n", name)
		return nil, errors.New("name at most one config document")
	}
	if bootstrapPath == "" {
		bootstrapPath = *configPath
	}

	base, bearer := *addr, *token
	if base == "" || bearer == "" {
		// The config is read for the DEFAULTS only, so an operator who
		// supplied both flags can act on a node whose config file this
		// machine does not have.
		boot, err := config.LoadBootstrap(bootstrapPath, config.EnvOnly())
		if err != nil {
			return nil, fmt.Errorf("%w\n\nPass -url and -token to reach a node "+
				"whose config this machine does not hold", err)
		}
		if base == "" {
			base, err = nodeBaseURL(boot, "", "this node")
			if err != nil {
				return nil, err
			}
		}
		if bearer == "" {
			// THE LENIENT FORM, and what it is lenient about has
			// changed: `api.auth.disabled` is gone, so an absent token
			// is no longer a servable posture — it is a call that will
			// be refused at the far end. It is still not refused HERE,
			// because a development principal resolves an
			// unauthenticated request on a loopback bind of an
			// unreleased binary, and that is a legitimate way to reach
			// these routes. The 401 from a node that does not is the
			// honest answer and it names the variable.
			bearer = nodeTokenOrEmpty()
		}
	}
	return &nodeClient{
		base:  base,
		token: bearer,
		http:  httpx.Client(nodeRequestTimeout),
	}, nil
}

func (c *nodeClient) get(ctx context.Context, path string, into any) error {
	return c.do(ctx, http.MethodGet, path, into)
}

func (c *nodeClient) post(ctx context.Context, path string, into any) error {
	return c.do(ctx, http.MethodPost, path, into)
}

// postKeyed is [nodeClient.post] carrying an operation key, which is what
// makes a retry of the same gesture the same operation on the node's ledger.
func (c *nodeClient) postKeyed(ctx context.Context, path, key string, into any) error {
	header := http.Header{}
	if key = strings.TrimSpace(key); key != "" {
		header.Set(idempotencyHeader, key)
	}
	return c.send(ctx, http.MethodPost, path, header, into)
}

// idempotencyHeader is the header every write surface on a node reads an
// operation key from.
const idempotencyHeader = "Idempotency-Key"

// maxNodeResponseBytes bounds one answer read back from a node.
//
// An error body is a sentence; a proxy's error page is not. The largest
// legitimate answer this client reads is /query/budgets, which is one row of
// roughly two hundred bytes per seat — so a megabyte is three orders of
// magnitude above a large company's answer and still small enough that a
// misdirected -url cannot make the CLI buffer a website.
const maxNodeResponseBytes = 1 << 20

func (c *nodeClient) do(ctx context.Context, method, path string, into any) error {
	return c.send(ctx, method, path, nil, into)
}

func (c *nodeClient) send(ctx context.Context, method, path string,
	header http.Header, into any) error {

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	for name, values := range header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach the node at %s: %w\n\n"+
			"This command acts on state the RUNNING engine holds. Start the node, "+
			"or pass -url to name another one", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Capped, because an error body is a sentence and anything larger is
	// either a proxy's HTML or something that is not this API at all — and
	// REFUSED at the cap rather than read up to it. io.LimitReader reports a
	// clean EOF when it stops, so a clipped answer would reach json.Unmarshal
	// as malformed JSON and be reported as "not the expected JSON", which
	// sends the reader looking for a protocol fault that is not there.
	//
	// The read error is no longer discarded either: a connection that died
	// mid-body is a different fact from a short answer.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNodeResponseBytes+1))
	if err != nil {
		return fmt.Errorf("reading the node's answer to %s: %w", path, err)
	}
	if len(body) > maxNodeResponseBytes {
		return fmt.Errorf(
			"the node's answer to %s exceeded %d bytes, so it was not read: this "+
				"build caps one answer to bound a proxy's error page. Check that "+
				"-url names the engine rather than something in front of it",
			path, maxNodeResponseBytes)
	}
	// ANY 2xx IS AN ANSWER. A write surface says `202` for a change that
	// is durable and not yet applied on this node, which is a success to
	// report as such rather than an error — read as one, the CLI told an
	// operator their purge had failed, and the ordinary next move was to
	// run it again.
	if resp.StatusCode/100 != 2 {
		return nodeError(resp.StatusCode, body, c.token != "")
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("the node's answer was not the expected JSON: %w", err)
	}
	return nil
}

// nodeError turns a non-200 into something an operator can act on.
func nodeError(status int, body []byte, sentToken bool) error {
	var payload struct {
		Error  string `json:"error"`
		Detail string `json:"detail"`
		Hint   string `json:"hint"`
		OpID   string `json:"op_id"`
	}
	_ = json.Unmarshal(body, &payload)
	// A WRITE THE NODE COULD NOT ACCOUNT FOR carries the operation key a
	// retry must reuse, and that is the one fact the caller cannot recover
	// any other way — so it survives whatever status it came with.
	if payload.OpID != "" && status == http.StatusServiceUnavailable {
		return &unsettledWrite{opID: payload.OpID, err: fmt.Errorf(
			"the node could not establish whether this landed: %s",
			withRefusalDetail(firstNonEmpty(payload.Error, "unknown outcome"),
				payload.Detail, payload.Hint))}
	}
	if msg, ok := credentialRefusal(status, body, sentToken); ok {
		return errors.New(msg)
	}
	switch status {
	case http.StatusServiceUnavailable:
		// TWO DIFFERENT FACTS on this surface, and the guess below is
		// only one of them: a node built without the backend a route
		// needs, and a node DRAINING for a shutdown. The body says
		// which, so the guess is a fallback rather than the answer, and
		// the detail and hint beside it are what say where to go
		// instead — "draining" on its own names what happened and not
		// what to do about it.
		return fmt.Errorf("this node cannot serve that: %s", withRefusalDetail(
			firstNonEmpty(payload.Error,
				"it is running without the backend the route needs"),
			payload.Detail, payload.Hint))
	default:
		return fmt.Errorf("the node answered %d: %s", status,
			withRefusalDetail(firstNonEmpty(payload.Error, string(body)),
				payload.Detail, payload.Hint))
	}
}

// unsettledWrite is a write whose outcome the node could not establish, and
// the operation key a retry must carry to be the same operation.
type unsettledWrite struct {
	opID string
	err  error
}

func (u *unsettledWrite) Error() string { return u.err.Error() }
func (u *unsettledWrite) Unwrap() error { return u.err }
