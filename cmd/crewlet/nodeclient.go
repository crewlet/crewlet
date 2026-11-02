package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
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
	// maxAnswer is the most one answer may carry, zero for
	// maxNodeResponseBytes: a client whose routes answer PAGES of a store's
	// rows reads by the bound those pages are held to — the code reads'
	// ([nodeClient.forCodeReads]).
	maxAnswer int
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

// answerCap is the most one answer of c may carry.
func (c *nodeClient) answerCap() int {
	if c.maxAnswer > 0 {
		return c.maxAnswer
	}
	return maxNodeResponseBytes
}

// nodeClientFor is the shared "one config argument, then find the node"
// preamble the node-facing operator commands share. env is the invocation's
// environment ([runIn]), which the credential is read from.
func nodeClientFor(environ config.Source, args []string, name string, stderr io.Writer,
	extra func(*flag.FlagSet)) (*nodeClient, error) {

	bootstrapPath, args := splitSubject(args)

	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultBootstrapPath,
		"node config file: where this node serves its API")
	addr := nodeAPIFlag(fs,
		"the running node's API (default: from the config's api block). "+
			"The token comes from "+apiTokenEnv+", never a flag")
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
	base, bearer := *addr, nodeTokenOrEmpty(environ)
	if base == "" {
		// The config is read for the ADDRESS only, so an operator who
		// named one can act on a node whose config file this machine does
		// not have.
		boot, err := config.LoadBootstrap(bootstrapPath, config.EnvOnly())
		if err != nil {
			return nil, fmt.Errorf("%w\n\nPass -api to reach a node whose config "+
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
// makes a retry of the same gesture the same operation on the node's ledger —
// and, where body is not nil, a JSON body: the act transport's shape, which
// takes a gesture's arguments as `{"args": …}` and refuses any other
// Content-Type.
func (c *nodeClient) postKeyed(ctx context.Context, path, key string, body, into any) error {
	return c.sendKeyed(ctx, http.MethodPost, path, key, body, into)
}

// putKeyed is [nodeClient.postKeyed] for a route that REPLACES a value rather
// than making a change of its own — a room's horizon.
func (c *nodeClient) putKeyed(ctx context.Context, path, key string, body, into any) error {
	return c.sendKeyed(ctx, http.MethodPut, path, key, body, into)
}

// sendKeyed is one write under an operation key, by method.
func (c *nodeClient) sendKeyed(ctx context.Context, method, path, key string,
	body, into any) error {

	header := http.Header{}
	if key = strings.TrimSpace(key); key != "" {
		header.Set(opkey.Header, key)
	}
	var payload []byte
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode the request: %w", err)
		}
		payload = raw
	}
	return c.send(ctx, method, path, header, payload, into)
}

// maxNodeResponseBytes bounds one answer read back from a node.
//
// An error body is a sentence; a proxy's error page is not. The largest
// legitimate answer this client reads is /query/budgets, which states every
// seat's day, week and month — about two hundred bytes a window, so some seven
// hundred a seat. Four megabytes is some six thousand seats, an order of
// magnitude above a large company, because an answer past the bound fails the
// command outright; and it is still small enough that a misdirected -api cannot
// make the CLI buffer a website.
const maxNodeResponseBytes = 4 << 20

func (c *nodeClient) do(ctx context.Context, method, path string, into any) error {
	return c.send(ctx, method, path, nil, nil, into)
}

func (c *nodeClient) send(ctx context.Context, method, path string,
	header http.Header, payload []byte, into any) error {

	_, err := c.exchange(ctx, method, path, header, payload, into)
	return err
}

// exchange is send answering the node's answer's header beside: what a
// command that waits by a time the node states reads that time against —
// the answer's own Date, which the node's clock wrote — rather than this
// machine's clock, which may disagree with the node's by more than the wait.
func (c *nodeClient) exchange(ctx context.Context, method, path string,
	header http.Header, payload []byte, into any) (http.Header, error) {

	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build the request: %w", err)
	}
	for name, values := range header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	if payload != nil {
		// THE ONE BODY THIS CLIENT SENDS IS JSON, and the act transport
		// refuses any other Content-Type rather than guessing.
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, noAnswer{fmt.Errorf("reach the node at %s: %w\n\n"+
			"This command acts on state the RUNNING engine holds. Start the node, "+
			"or pass -api to name another one", c.base, err)}
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
	limit := c.answerCap()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, noAnswer{fmt.Errorf("reading the node's answer to %s: %w", path, err)}
	}
	if len(body) > limit {
		// NOT THE NODE'S ANSWER EITHER: no answer this API writes comes
		// near the cap, so what did is something in front of it — and
		// what the node did with the request is as unknown as if nothing
		// had come back at all.
		return nil, noAnswer{fmt.Errorf(
			"the answer to %s exceeded %d bytes, so it was not read: this build "+
				"caps one answer to bound a proxy's error page, and what the node "+
				"did with the request is unknown. Check that -api names the engine "+
				"rather than something in front of it",
			path, limit)}
	}
	// ANY 2xx IS AN ANSWER, not only a 200. A write surface says `202` for a
	// change that is durable and not yet applied on this node, which is a
	// success to report as such rather than an error — read as one, the CLI
	// told an operator their purge had failed, and the ordinary next move was
	// to run it again. And a route that begins work outliving its request — a
	// code verify — answers 202 Accepted with the operation it began, which
	// read as a failure had no error code and was reported as something in
	// front of the node.
	if resp.StatusCode/100 != 2 {
		if engineCode(resp.Header, body) == "" {
			return nil, notTheNode(path, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
		err := nodeError(resp.StatusCode, body, c.token != "")
		var refusal *nodeRefusal
		if errors.As(err, &refusal) {
			refusal.RetryAfter = retryAfter(resp.Header)
		}
		return nil, err
	}
	if into == nil {
		return resp.Header, nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		// A 200 WHOSE BODY DOES NOT DECODE is an answer cut short — a
		// proxy that closed a buffered response, a connection that ended
		// on a clean boundary — and says nothing about what the node did.
		// Reported as a plain error it read as a failure, and a gesture
		// that very likely ran printed nothing that finishes it.
		return nil, noAnswer{fmt.Errorf("the answer to %s was cut short or is not the "+
			"JSON the node writes (%w), so what the node did is unknown", path, err)}
	}
	return resp.Header, nil
}

// retryAfter is a refusal's Retry-After as a duration, zero where it carries
// none. Whole seconds only: a refusal that reaches it is the node's own (it
// carries the engine's error code — [notTheNode] takes every other), and the
// engine writes no other form. A value it cannot read is no wait at all
// rather than a guess, so a command that retries by it reports the refusal
// instead.
func retryAfter(h http.Header) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// engineCode is the refusal code a non-200 answer carries where the engine
// wrote it, or "" where it did not. Every refusal the engine writes carries
// its mark ([httpjson.Marked]) and is JSON with an `error` code, the guard's
// 401 included, and so are its router's own `no_route` and
// `method_not_allowed` for a route or a method the node does not serve
// ([httpjson.Mux]): a route this node does not serve is a refusal, not a
// gateway. An answer without the mark is a gateway's, whatever its body says.
func engineCode(h http.Header, body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(ownBody(h, body), &payload) != nil {
		return ""
	}
	return payload.Error
}

// missingSurface is the refusal for a 404 at a REST surface the command needs,
// given the code the body carried.
//
// Only a node with the ingress role mounts the REST surface. A node without
// it binds api.port for its probes — and its own boxes' routes, to its own
// host alone ([api.Probes]) — and its router answers every other path with
// the engine's own `no_route`
// ([httpjson.CodeNoRoute]) — so that code is a Crewlet node missing a role,
// and a 404 carrying no code of the engine's is not a node's API at all: a
// proxy or gateway in front of it, or another service at the address. Each
// is told where to point instead, because "no such route" alone sends an
// operator looking for a build that lacks one.
func missingSurface(base, surface, code string) error {
	if code == string(httpjson.CodeNoRoute) {
		return fmt.Errorf("%s serves no %s surface: it is a node without the "+
			"%s role, which serves only its probes. Point -api at a node "+
			"whose node.roles include %s", base, surface,
			placement.RoleIngress, placement.RoleIngress)
	}
	return fmt.Errorf("%s has no %s surface: whatever answered is not a "+
		"Crewlet node's API. Point -api (or api.host and api.port) at a node "+
		"with the %s role rather than at a proxy or another service",
		base, surface, placement.RoleIngress)
}

// notTheNode is a non-200 that is not the node's own answer: no engine error
// code, so something between this command and the node wrote it — most often
// a reverse proxy's read timeout, a 504 with an HTML page, while the node went
// on to finish the request.
//
// # Why it is a noAnswer and not a refusal
//
// Because a refusal the node wrote means nothing was done, and this does not:
// the gate route answers within fifty seconds (engine.GateAnswerBudget), a
// proxy configured below that cuts it off, and the node finishes a gesture
// whatever happens to the connection. Read as a refusal, the eviction printed
// no -op-id, so the only way on was a second gesture over every log the first
// one reached.
func notTheNode(path string, status int, contentType string, body []byte) error {
	said := foreignAnswer(contentType, body)
	return noAnswer{fmt.Errorf("the answer to %s came back %d with no error code "+
		"the engine writes, so it is not the node's own: something in front of it "+
		"answered (a proxy or gateway, most often its read timeout), and the node "+
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
// do if it can read them, and which ones it renders depends on the refusal's
// code; and a write names the operation it was sent under, which `crewlet
// worklet purge` prints as the -op-id a retry must carry.
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
	// through another node (-api), under the same id.
	Unvouched bool

	// Reason is the refusal's `reason`, where a route names one beside its
	// code — what tells two refusals with one code apart (a code verify's
	// `not_held` beside its `not_found`) without reading the sentence.
	Reason string

	// RetryAfter is the node's Retry-After, zero where it sent none: the
	// node's word that waiting clears this refusal, and for how long — what a
	// command that retries a refusal (a code verify a busy node refused)
	// retries by.
	RetryAfter time.Duration

	// words are the refusal's own sentences, which the message carries
	// beneath its first line.
	words refusalWords

	msg string
}

func (e *nodeRefusal) Error() string { return e.msg }

// answered is the refusal as the node's own answer, whatever its status: the
// status and the code, then what the refusal says and the hint.
func (e *nodeRefusal) answered() string {
	return fmt.Sprintf("the node answered %d: %s", e.Status,
		withRefusalDetail(e.Code, e.words))
}

// asJudgement is the refusal framed as the node's JUDGEMENT of a request — a
// gate refusal — rather than as a node that cannot serve it.
//
// A 503 IS NOT ALWAYS "GO ELSEWHERE". [nodeError] frames one as "this node
// cannot serve that", which is what a draining node and one built without a
// route's backend mean. A gesture that could not be judged answers 503 too
// (`readmission_unjudged`, `eviction_unjudged`), and is about something else
// entirely — a lease listing or a positions register nobody could read, which
// the refusal's own hint and actions say how to get past. Introduced as this
// node's inability, the refusal contradicted its own hint; framed as the answer
// it is, as every 409 gate refusal is, its code, hint and actions say what it
// is.
func (e *nodeRefusal) asJudgement() *nodeRefusal {
	out := *e
	out.msg = e.answered()
	return &out
}

// nodeError turns a non-2xx the NODE wrote into something an operator can act
// on. Only an answer carrying an engine error code reaches it: one that
// carries none is [notTheNode]'s.
func nodeError(status int, body []byte, sentToken bool) error {
	var payload struct {
		refusalWords
		Error     string   `json:"error"`
		Reason    string   `json:"reason"`
		Actions   []string `json:"actions"`
		OpID      string   `json:"op_id"`
		Outcome   string   `json:"outcome"`
		Unvouched bool     `json:"unvouched"`
	}
	_ = json.Unmarshal(body, &payload)
	if msg, ok := credentialRefusal(status, body, sentToken); ok {
		return errors.New(msg)
	}
	refusal := &nodeRefusal{Status: status, Code: payload.Error, Reason: payload.Reason,
		Actions: payload.Actions, OpID: payload.OpID, Unvouched: payload.Unvouched,
		Unsettled: payload.Outcome == string(statelog.OutcomeUnknown),
		words:     payload.refusalWords}
	switch {
	case refusal.Unsettled:
		// NOT A REFUSAL: a write the node could not account for, which
		// may well have landed. Worded as one, it read as "nothing
		// happened", and the ordinary next move was the same gesture
		// under a fresh operation id.
		refusal.msg = "the node could not establish whether this landed: " +
			withRefusalDetail(payload.Error, payload.refusalWords)
	case status == http.StatusServiceUnavailable:
		// TWO DIFFERENT FACTS on this surface: a node built without the
		// backend a route needs, and a node DRAINING for a shutdown. The
		// code says which, and the detail and hint beside it are what say
		// where to go instead — "draining" on its own names what happened
		// and not what to do about it. (A gate's 503 is neither, and its
		// command reads it as the judgement it is: [nodeRefusal.asJudgement].)
		refusal.msg = "this node cannot serve that: " + withRefusalDetail(
			payload.Error, payload.refusalWords)
	default:
		refusal.msg = refusal.answered()
	}
	return refusal
}
