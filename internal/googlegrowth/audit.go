package googlegrowth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"sync"

	"github.com/crewlet/crewlet/internal/growth"
)

// Record is req's act as the access log holds it; before is the target's live
// state a held call read, nil for a call made at once.
func (c *Client) Record(call growth.Call, req Request, class growth.Class, before []byte) growth.Record {
	r := growth.Record{
		Product: c.Kind, Connection: c.connection(), Account: req.AccountOrTarget(), Target: req.Target(),
		Tool: call.Tool, Class: class, Handle: call.Handle, Turn: call.Turn, Call: call.ID,
		HeldCall: call.HeldCall, Operation: req.Digest(nil),
	}
	if r.HeldCall != "" {
		// A RECORD NAMES ITS ACT ONCE: an approved call is its held call's.
		r.Call = ""
	}
	var current map[string]any
	if len(before) > 0 {
		sum := sha256.Sum256(before)
		r.Before = hex.EncodeToString(sum[:])
		_ = json.Unmarshal(before, &current)
	}
	for _, ch := range req.Changes(current) {
		r.Changes = append(r.Changes, RecordChange(ch))
	}
	return r
}

// identifier is a value the log may keep as it is: an ID, an enum, a number.
var identifier = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_.:/-]{0,128}$`) })

// RecordChange is one change as the log keeps it: free words as their digest
// and length (Text), since a name or a description may hold anything.
func RecordChange(ch growth.Change) growth.RecordChange {
	before, after := "", ""
	if ch.Before != nil {
		before = Words(ch.Before)
	}
	if ch.After != nil {
		after = Words(ch.After)
	}
	return growth.RecordChange{Field: ch.Field, Label: ch.Field, Before: before, After: after,
		Text: !identifier().MatchString(before) || !identifier().MatchString(after)}
}

// note writes one step of an act; a write that fails is logged, since the
// call it records has already happened at Google.
func (c *Client) note(ctx context.Context, step growth.Step, r growth.Record) {
	write := map[growth.Step]func(context.Context, growth.Record) (bool, error){
		growth.StepMade: c.Audit.Made, growth.StepApplied: c.Audit.Applied,
		growth.StepVoid: c.Audit.Void, growth.StepFailed: c.Audit.Failed,
	}[step]
	if _, err := write(ctx, r); err != nil {
		slog.WarnContext(ctx, "growth_record_failed", "kind", c.Kind, "step", string(step), "tool", r.Tool,
			"seat", r.Handle, "err", err)
	}
}

// MadeOrFailed records a call made at once once it reached for Google: made,
// or failed with its reason. A call refused before Google is no act.
func (c *Client) MadeOrFailed(ctx context.Context, call growth.Call, req Request, class growth.Class, err error) {
	r := c.Record(call, req, class, nil)
	if err == nil {
		c.note(ctx, growth.StepMade, r)
		return
	}
	if ce, ok := errors.AsType[*CallError](err); ok {
		r.Failure = ce.Failure
		c.note(ctx, growth.StepFailed, r)
	}
}

// applyFailure is why an allowed call failed: Google's reason, or the seat's
// scope or level no longer allowing it, where nothing was sent.
func applyFailure(err error) growth.Failure {
	if ce, ok := errors.AsType[*CallError](err); ok {
		return ce.Failure
	}
	if _, ok := errors.AsType[*growth.Refusal](err); ok {
		return growth.FailureAccessLowered
	}
	return growth.FailureEngineError
}
