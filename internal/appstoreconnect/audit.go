package appstoreconnect

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"

	"github.com/crewlet/crewlet/internal/growth"
)

// recordOf is the access log's record of call made as m: its account the app,
// or the team for a call no app owns.
func recordOf(call growth.Call, m made) growth.Record {
	r := growth.Record{Product: Kind, Tool: call.Tool, Class: m.class, Handle: call.Handle, Turn: call.Turn,
		Call: call.ID, HeldCall: call.HeldCall, Operation: operationDigest(m)}
	if m.team != nil {
		r.Connection = m.team.id
	}
	r.Account = m.app
	if r.Account == "" && m.team != nil {
		r.Account = "team:" + m.team.id
	}
	if typ, id := typeAndID(m.path); id != "" {
		r.Target = typ + "/" + id
	}
	return r
}

// recordChanges are card changes as the access log keeps them; text marks
// words that are published, which the log keeps as a digest.
func recordChanges(ch []growth.CardChange, text bool) []growth.RecordChange {
	out := make([]growth.RecordChange, 0, len(ch))
	for _, c := range ch {
		out = append(out, growth.RecordChange{Field: c.Field, Label: c.Label, Before: c.Before, After: c.After,
			Money: c.Money, Text: text})
	}
	return out
}

// failureOf is a failure's closed word for the access log.
func failureOf(err error) growth.Failure {
	var apiErr *APIError
	switch {
	case errors.As(err, &apiErr) && (apiErr.Status == 401 || apiErr.Status == 403):
		return growth.FailureUnauthorized
	case errors.As(err, &apiErr) && apiErr.Status == 429:
		return growth.FailureRateLimited
	case errors.As(err, &apiErr) && apiErr.Status >= 500:
		return growth.FailureVendorUnavailable
	case errors.As(err, &apiErr):
		return growth.FailureVendorRefused
	case errors.Is(err, context.DeadlineExceeded):
		return growth.FailureTimedOut
	case errors.Is(err, ErrNoKey), errors.Is(err, ErrReserve):
		return growth.FailureEngineError
	}
	return growth.FailureUnknownOutcome
}

// argsDigest digests a call's arguments, for a step whose request cannot be
// made concrete again (a denial after the seat's access changed).
func argsDigest(call growth.Call) string {
	data, _ := json.Marshal(map[string]any{"tool": call.Tool, "args": call.Args})
	return hexSum(data)
}

// note records a step, logging a record that did not land: the act itself
// never waits on the log's answer.
func (p *Product) note(ctx context.Context, step growth.Step, r growth.Record) {
	if p.deps.Audit == nil {
		return
	}
	var err error
	switch step {
	case growth.StepMade:
		_, err = p.deps.Audit.Made(ctx, r)
	case growth.StepApplied:
		_, err = p.deps.Audit.Applied(ctx, r)
	case growth.StepVoid:
		_, err = p.deps.Audit.Void(ctx, r)
	case growth.StepFailed:
		_, err = p.deps.Audit.Failed(ctx, r)
	default:
		_, err = p.deps.Audit.Verdict(ctx, step, r)
	}
	if err != nil {
		log.WarnContext(ctx, "app_store_connect_unrecorded", "tool", r.Tool, "step", string(step), "error", err.Error())
	}
}

// accountOf is the account a request on path names for the log, for a step
// whose call is not made concrete.
func accountOf(args map[string]any) string {
	if app, _ := args["app"].(string); app != "" {
		return app
	}
	if path, _ := args["path"].(map[string]any); path != nil {
		if id, _ := path["id"].(string); id != "" {
			return url.PathEscape(id)
		}
	}
	return "unknown"
}
