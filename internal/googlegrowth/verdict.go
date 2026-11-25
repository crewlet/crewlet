package googlegrowth

import (
	"context"
	"encoding/json"
	"io"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/growth"
)

// Allow records an administrator's allow of a held call before it acts, and
// refuses to act unless that allow landed as the call's one answer; a node
// with no access log acts on none.
func (c *Client) Allow(ctx context.Context, r growth.Record, answerer string) error {
	if c.Audit == nil {
		return growth.Refuse(growth.RefusedUnavailable, "this node records no answer, so nothing was sent.")
	}
	return c.allow(ctx, r, answerer)
}

// Deny records a held call's one answer when it is not allowed: an
// administrator's deny, or its lapse ([growth.Call.Denial]).
func (c *Client) Deny(ctx context.Context, call growth.Call, r growth.Record) {
	step, answerer := call.Denial()
	r.Answerer = answerer
	c.note(ctx, step, r)
}

// Note writes one step of an act, logging a write that did not land: the
// call it records has already happened at Google.
func (c *Client) Note(ctx context.Context, step growth.Step, r growth.Record) { c.note(ctx, step, r) }

// StepFailure is why one step of an act ended without its result: Google's
// reason (a write that got no answer of Google's own is unknown, unless
// nothing was sent: then timed out or unavailable), the seat's access
// lowered, or the engine's own.
func StepFailure(err error) growth.Failure { return failureOfStep(err) }

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
