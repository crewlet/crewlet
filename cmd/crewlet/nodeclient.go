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

	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
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
// longer wait never turns a wrong address into a right one, so the routes that
// genuinely need longer take it in code. See [nodeClient.patiently].
const nodeRequestTimeout = 10 * time.Second

// patiently returns a client that waits longer for one call.
//
// THE EXCEPTION to the reasoning above, and a narrow one. Every other route
// here answers from memory or a coordination read, so how long it takes is a
// property of the NETWORK and ten seconds is a diagnosis. A backup's duration
// is a property of the DATA — it copies the whole store and every stream — so
// the same ceiling would abandon a working backup on a large company and
// report a failure for work the engine goes on to finish, leaving a complete
// backup on disk that the operator has been told did not happen. A reanchor's
// is a property of the BROKER: it rebuilds this node's consumer on the log,
// which is a delete and a create of a replicated object (see
// [reanchorRequestTimeout]).
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
		"the running node's base URL; empty takes it from the config's api block. "+
			"The credential is "+apiTokenEnv+", and never a flag")
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

	// THE CREDENTIAL IS THE ENVIRONMENT'S, AND ONLY THE ENVIRONMENT'S.
	//
	// There was a `-token` flag here, beside a comment on
	// [nodeTokenOrEmpty] saying the token was never a flag: a token typed
	// as an argument is in the shell history, in `ps` and in any CI log
	// that echoes the command, so the flag was the one way to use this CLI
	// that leaked the credential it carried — and it made the 401 below
	// name two sources when an operator has one to check.
	//
	// THE LENIENT FORM: an absent token is not refused HERE, because a
	// development principal resolves an unauthenticated request on a
	// loopback bind of an unreleased binary, and that is a legitimate way
	// to reach these routes. The 401 from a node that does not is the
	// honest answer, and it names the variable.
	base, bearer := *addr, nodeTokenOrEmpty()
	if base == "" {
		// The config is read for the ADDRESS only, so an operator who
		// named one can act on a node whose config file this machine does
		// not have.
		boot, err := config.LoadBootstrap(bootstrapPath, config.EnvOnly())
		if err != nil {
			return nil, fmt.Errorf("%w\n\nPass -url to reach a node whose config "+
				"this machine does not hold", err)
		}
		base, err = nodeBaseURL(boot, "", "this node")
		if err != nil {
			return nil, err
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
		header.Set(opkey.Header, key)
	}
	return c.send(ctx, http.MethodPost, path, header, into)
}

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
		return noAnswer{fmt.Errorf("reach the node at %s: %w\n\n"+
			"This command acts on state the RUNNING engine holds. Start the node, "+
			"or pass -url to name another one", c.base, err)}
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
		return noAnswer{fmt.Errorf("reading the node's answer to %s: %w", path, err)}
	}
	if len(body) > maxNodeResponseBytes {
		// NOT THE NODE'S ANSWER EITHER: no answer this API writes comes
		// near the cap, so what did is something in front of it — and
		// what the node did with the request is as unknown as if nothing
		// had come back at all.
		return noAnswer{fmt.Errorf(
			"the answer to %s exceeded %d bytes, so it was not read: this build "+
				"caps one answer to bound a proxy's error page, and what the node "+
				"did with the request is unknown. Check that -url names the engine "+
				"rather than something in front of it",
			path, maxNodeResponseBytes)}
	}
	// ANY 2xx IS AN ANSWER. A write surface says `202` for a change that
	// is durable and not yet applied on this node, which is a success to
	// report as such rather than an error — read as one, the CLI told an
	// operator their purge had failed, and the ordinary next move was to
	// run it again.
	if resp.StatusCode/100 != 2 {
		if engineCode(body) == "" {
			return notTheNode(path, resp.StatusCode, body)
		}
		return nodeError(resp.StatusCode, body, c.token != "")
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		// A 200 WHOSE BODY DOES NOT DECODE is an answer cut short — a
		// proxy that closed a buffered response, a connection that ended
		// on a clean boundary — and says nothing about what the node did.
		// Reported as a plain error it read as a failure, and a gesture
		// that very likely ran printed nothing that finishes it.
		return noAnswer{fmt.Errorf("the answer to %s was cut short or is not the "+
			"JSON the node writes (%w), so what the node did is unknown", path, err)}
	}
	return nil
}

// engineCode is the refusal code a non-200 body carries, or "" where it carries
// none — which no refusal the engine writes does: every one is JSON with an
// `error` code, the guard's 401 included, and so are its router's own
// `no_route` and `method_not_allowed` for a route or a method the node does
// not serve ([httpjson.Mux]) — a route an older node lacks is a refusal, not a
// gateway.
func engineCode(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	return payload.Error
}

// notTheNode is a non-200 that is not the node's own answer: no engine error
// code, so something between this command and the node wrote it — most often
// a reverse proxy's read timeout, a 504 with an HTML page, while the node went
// on to finish the request.
//
// # Why it is a noAnswer and not a refusal
//
// Because a refusal the node wrote means nothing was done, and this does not:
// the gate route takes up to a minute past its judgement, a proxy's default
// read timeout is a minute, and the node finishes a gesture whatever happens to
// the connection. Read as a refusal, the eviction printed no -op-id, so the
// only way on was a second gesture over every log the first one reached.
func notTheNode(path string, status int, body []byte) error {
	said := textcut.Ellipsis(strings.TrimSpace(string(body)), maxRefusalTextBytes)
	if said == "" {
		said = "(an empty body)"
	}
	return noAnswer{fmt.Errorf("the answer to %s came back %d with no error code "+
		"the engine writes, so it is not the node's own: something in front of it "+
		"answered — a proxy or gateway, most often its read timeout — and the node "+
		"may still have done what was asked. It said: %s", path, status, said)}
}

// noAnswer is a request the node never answered: it could not be reached, the
// wait ran out, or its answer was cut off mid-body.
//
// A TYPE OF ITS OWN because it is the one failure that leaves what the node DID
// unknown. A refusal the node sent is an answer — a 409 wrote nothing — while a
// POST with no answer may have run to completion after the connection went, so
// a command that writes has to say how to find out, which it can only do if it
// can tell the two apart.
type noAnswer struct{ error }

func (e noAnswer) Unwrap() error { return e.error }

// nodeRefusal is a non-2xx the node answered: the status, and the refusal's
// own fields beside the message built from them.
//
// A TYPE rather than a bare error for the commands that have to act on a field
// the message does not carry: a gate refusal names what to do as `actions` — a
// closed set each surface renders in its own words — and `crewlet retention
// evict` renders them as the flags it has (-force, -op-id), which it can only
// do if it can read them; and a write names the operation it was sent under,
// which `crewlet work purge` prints as the -op-id a retry must carry.
type nodeRefusal struct {
	Status  int
	Code    string
	Actions []string

	// OpID is the operation the write was sent under, where the answer
	// names one — a write's 503 does, and so does a gate refusal carrying
	// actions. A retry carries it and never a fresh one: if any of the
	// write landed, the same id is answered from the node's ledger, where a
	// fresh one is a second write.
	OpID string

	// Unsettled is a write whose outcome the node could not establish
	// (`outcome: unknown`) — which may well have landed, and so is not a
	// refusal at all however its status reads.
	Unsettled bool

	// Unvouched is an unknown this node cannot settle: its operation ledger
	// may have lost the row the operation needs, so the same request here
	// answers the same way until the write reaches it — the retry goes
	// through another node (-url), under the same id.
	Unvouched bool

	msg string
}

func (e *nodeRefusal) Error() string { return e.msg }

// nodeError turns a non-2xx the NODE wrote into something an operator can act
// on. Only an answer carrying an engine error code reaches it: one that
// carries none is [notTheNode]'s.
func nodeError(status int, body []byte, sentToken bool) error {
	var payload struct {
		Error     string   `json:"error"`
		Detail    string   `json:"detail"`
		Hint      string   `json:"hint"`
		Actions   []string `json:"actions"`
		OpID      string   `json:"op_id"`
		Outcome   string   `json:"outcome"`
		Unvouched bool     `json:"unvouched"`
	}
	_ = json.Unmarshal(body, &payload)
	if msg, ok := credentialRefusal(status, body, sentToken); ok {
		return errors.New(msg)
	}
	refusal := &nodeRefusal{Status: status, Code: payload.Error, Actions: payload.Actions,
		OpID: payload.OpID, Unvouched: payload.Unvouched,
		Unsettled: payload.Outcome == string(statelog.OutcomeUnknown)}
	switch {
	case refusal.Unsettled:
		// NOT A REFUSAL: a write the node could not account for, which
		// may well have landed. Worded as one, it read as "nothing
		// happened", and the ordinary next move was the same gesture
		// under a fresh operation id.
		refusal.msg = "the node could not establish whether this landed: " +
			withRefusalDetail(payload.Error, payload.Detail, payload.Hint)
	case status == http.StatusServiceUnavailable:
		// TWO DIFFERENT FACTS on this surface: a node built without the
		// backend a route needs, and a node DRAINING for a shutdown. The
		// code says which, and the detail and hint beside it are what say
		// where to go instead — "draining" on its own names what happened
		// and not what to do about it.
		refusal.msg = "this node cannot serve that: " + withRefusalDetail(
			payload.Error, payload.Detail, payload.Hint)
	default:
		refusal.msg = fmt.Sprintf("the node answered %d: %s", status,
			withRefusalDetail(payload.Error, payload.Detail, payload.Hint))
	}
	return refusal
}
