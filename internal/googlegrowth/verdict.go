package googlegrowth

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
)

// Verdicts records a held call's one answer (the engine's GrowthAudit): first
// is false, with no error, where the call already holds one, which stands.
type Verdicts interface {
	Verdict(ctx context.Context, step growth.Step, r growth.Record) (first bool, err error)
}

// engineActor is who the engine's own answer names: its lapse sweep.
const engineActor = "engine"

// Allow records an administrator's allow of a held call before it acts, and
// refuses to act unless that allow landed as the call's one answer.
func (c *Client) Allow(ctx context.Context, r growth.Record, answerer string) error {
	if c.Verdicts == nil {
		return growth.Refuse(growth.RefusedUnavailable, "this node records no answer, so nothing was sent.")
	}
	r.Answerer = answerer
	first, err := c.Verdicts.Verdict(ctx, growth.StepAllowed, r)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "growth_answer_unrecorded", "kind", c.Kind, "tool", r.Tool, "err", err)
		return growth.Refuse(growth.RefusedUnavailable, "the access log did not record the administrator's "+
			"answer, so nothing was sent. Call again.")
	case !first:
		return growth.Refuse(growth.RefusedForbidden, "the call already holds an answer (it lapsed, or another "+
			"answer came first), which stands. Nothing was sent.")
	}
	return nil
}

// Deny records that an administrator denied a held call, or that it lapsed
// (an answer from nobody, or the engine's).
func (c *Client) Deny(ctx context.Context, call growth.Call, r growth.Record) {
	step, answerer := growth.StepDenied, call.Approver
	if call.Approver == "" || call.Approver == engineActor {
		step, answerer = growth.StepLapsed, ""
	}
	r.Answerer = answerer
	if c.Verdicts == nil {
		slog.WarnContext(ctx, "growth_answer_unrecorded", "kind", c.Kind, "tool", r.Tool, "step", string(step))
		return
	}
	if _, err := c.Verdicts.Verdict(ctx, step, r); err != nil {
		slog.WarnContext(ctx, "growth_answer_unrecorded", "kind", c.Kind, "tool", r.Tool, "step", string(step),
			"err", err)
	}
}

// Note writes one step of an act, logging a write that did not land: the
// call it records has already happened at Google.
func (c *Client) Note(ctx context.Context, step growth.Step, r growth.Record) { c.note(ctx, step, r) }

// EarlyFailure is why an allowed call ended before it was sent: Google's
// reason for a read, the seat's access lowered, or the engine's own.
func EarlyFailure(err error) growth.Failure { return applyFailure(err) }

// SendBytes makes req as handle's identity with data as its body, such as a
// file an upload sends, and answers Google's body.
func (c *Client) SendBytes(ctx context.Context, handle string, req Request, contentType string, data []byte) (
	[]byte, error) {
	var raw json.RawMessage
	err := c.send(ctx, handle, req, googleauth.Bytes{ContentType: contentType, Data: data}, &raw)
	return raw, err
}

// SendTo makes req, a read, as handle's identity and streams Google's body
// to w, such as a file a download reads; w bounds what it keeps.
func (c *Client) SendTo(ctx context.Context, handle string, req Request, w io.Writer) error {
	return c.send(ctx, handle, req, nil, w)
}
