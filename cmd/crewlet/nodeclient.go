package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/config"
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

// APITokenEnv is where a node client reads its bearer token from.
//
// An ENV VAR rather than a flag default, because a token on a command line is
// in the shell history, in `ps`, and in any CI log that echoes the command.
// The flag exists for the case where that is genuinely what an operator
// wants, and it is theirs to choose.
const APITokenEnv = "CREWLET_API_TOKEN"

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
	patient.http = &http.Client{Timeout: limit}
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
		"bearer token; empty takes "+APITokenEnv+", then the config's first token")
	if extra != nil {
		extra(fs)
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if tail := fs.Args(); bootstrapPath == "" && len(tail) == 1 {
		bootstrapPath = tail[0]
	} else if len(tail) > 0 {
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
			base, err = nodeBaseURL(boot)
			if err != nil {
				return nil, err
			}
		}
		if bearer == "" {
			bearer = nodeToken(boot)
		}
	}
	return &nodeClient{
		base:  base,
		token: bearer,
		http:  &http.Client{Timeout: nodeRequestTimeout},
	}, nil
}

// nodeBaseURL is where the config says this node's API listens.
func nodeBaseURL(boot *config.Bootstrap) (string, error) {
	if boot.API.Port == 0 {
		return "", errors.New("this config serves no HTTP surface (api.port is 0), " +
			"so there is no node to ask: set api.port, or pass -url for another node")
	}
	host := boot.API.Host
	// A bind address of 0.0.0.0 (or ::) says "every interface", which is
	// not an address anything can dial — the loopback one is, and it is
	// the interface a command running beside the config is on.
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(boot.API.Port)), nil
}

// nodeToken is the bearer token to send, or "".
//
// The environment first, then the config's first token. The order matters: a
// checked-in config carries ${VAR} references that resolve to the same place
// the environment does, and an operator who exported one deliberately means
// that one.
func nodeToken(boot *config.Bootstrap) string {
	if fromEnv := os.Getenv(APITokenEnv); fromEnv != "" {
		return fromEnv
	}
	if len(boot.API.Auth.Tokens) > 0 {
		return boot.API.Auth.Tokens[0].Token
	}
	return ""
}

func (c *nodeClient) get(ctx context.Context, path string, into any) error {
	return c.do(ctx, http.MethodGet, path, into)
}

func (c *nodeClient) post(ctx context.Context, path string, into any) error {
	return c.do(ctx, http.MethodPost, path, into)
}

func (c *nodeClient) do(ctx context.Context, method, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
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
	// either a proxy's HTML or something that is not this API at all.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
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
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		if !sentToken {
			return errors.New("the node refused the request and no token was sent: " +
				"export " + APITokenEnv + ", or pass -token")
		}
		return errors.New("the node refused the token: check it against the " +
			"api.auth.tokens entry you meant to use")
	case http.StatusServiceUnavailable:
		return fmt.Errorf("this node cannot serve that: %s", firstNonEmpty(payload.Error,
			"it is running without the backend the route needs"))
	default:
		return fmt.Errorf("the node answered %d: %s", status,
			firstNonEmpty(payload.Error, string(body)))
	}
}
