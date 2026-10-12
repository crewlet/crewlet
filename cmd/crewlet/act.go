package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Calling one operator tool through the act transport, as the person the
// token is bound to.
//
// `POST /operator/act/{tool}` is the route the dashboard writes through
// (ADR-0024), and every command here that makes a person's gesture — a seat
// paused, a message said — makes it over the same route, so a gesture from a
// terminal and one from a screen are one gesture with one record and one
// `operator_acted` audit row.
//
// # A retry is the same request, and the command is what remembers its id
//
// The transport derives the call's operation from the caller's `request_id`,
// which is a UUIDv7 whose first 48 bits are the instant the gesture began (see
// builtin.RequestOperation). A command mints one per invocation and a
// `-request-id` flag sends it again, so a retry lands on the first attempt's
// operation rather than making the change twice. That only works if the
// operator is TOLD the id whenever the outcome is not known — and there are
// two ways not to know, which arrive looking nothing alike:
//
//   - the node ANSWERED that it could not vouch for the write: 503
//     `unavailable` carrying `outcome: unknown`, which is how the transport
//     answers a tool's unknown write and a call cut short alike. Its class
//     alone is a refusal's, which is why the outcome key is read here before
//     the status is — a command that reported it as "this node cannot serve
//     that" told a person nothing had happened about a write that may have;
//   - the node did NOT answer: a dropped connection, a timeout, an answer cut
//     off ([noAnswer]).
//
// [nodeClient.act] folds the first into an answer whose outcome is `unknown`,
// beside a 200 that says so itself, and leaves the second a [noAnswer]; the
// command prints the id in both cases ([actUnknown], [actUnanswered]).

// actAnswer is one act call's answer: the outcome and position every write
// states, and the tool's own receipt.
type actAnswer struct {
	Outcome  string          `json:"outcome"`
	Position *string         `json:"position"`
	Receipt  json.RawMessage `json:"receipt"`

	// detail is the node's sentence for an outcome it answered `unknown`
	// with a 503 — the tool's own word on what to read before sending again.
	// Empty on every other answer.
	detail string
}

// unknown reports whether this answer is the one outcome to retry.
func (a actAnswer) unknown() bool {
	return a.Outcome == string(statelog.OutcomeUnknown)
}

// act calls one tool as the token's person, under requestID.
//
// The node's `unknown` comes back as an answer, whichever status carried it; a
// request it never answered is a [noAnswer]; every other refusal is the error
// [nodeError] makes of it.
func (c *nodeClient) act(ctx context.Context, tool, requestID string,
	args map[string]any) (actAnswer, error) {

	var answer actAnswer
	err := c.postJSON(ctx, "/operator/act/"+url.PathEscape(tool),
		map[string]any{"request_id": requestID, "args": args}, &answer)
	var refusal *nodeRefusal
	if errors.As(err, &refusal) && refusal.Outcome == string(statelog.OutcomeUnknown) {
		return actAnswer{Outcome: refusal.Outcome, detail: refusal.detail}, nil
	}
	return answer, err
}

// newRequestID mints the act transport's request id for one gesture: a UUIDv7,
// which the transport requires — an id carrying no instant is refused rather
// than accepted into a write every node whose operation ledger ever lost a row
// would answer `unknown` without making.
func newRequestID() string { return uuid.Must(uuid.NewV7()).String() }

// actUnknown prints an outcome the node could not vouch for, and the request id
// that finishes it.
func actUnknown(stdout io.Writer, what, requestID string, answer actAnswer) {
	fmt.Fprintf(stdout, "%s: unknown — the node could not confirm the write.\n", what)
	if answer.detail != "" {
		fmt.Fprintf(stdout, "  %s\n", answer.detail)
	}
	fmt.Fprintf(stdout, "  Retry with -request-id %s: the retry is the same request, "+
		"so it cannot make the change twice.\n", requestID)
}

// actUnanswered says, for a request the node never answered, that its outcome
// is unknown and which id finishes it — or nothing, for any other failure.
func actUnanswered(stderr io.Writer, err error, what, requestID string) {
	var lost noAnswer
	if !errors.As(err, &lost) {
		return
	}
	fmt.Fprintf(stderr, "The node did not answer, so whether the %s landed is "+
		"unknown. Run the same command again with -request-id %s — the retry is "+
		"the same request, so it cannot make the change twice.\n", what, requestID)
}
