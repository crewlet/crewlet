package appstoreconnect

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"sync"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/growth"
)

// recordOf is the access log's record of call made as m: its account the app,
// or the team for a call no app owns.
func recordOf(call growth.Call, m made) growth.Record {
	r := growth.Record{Product: Kind, Tool: call.Tool, Class: m.class, Handle: call.Handle, Turn: call.Turn,
		Signer: growth.SignerOwn,
		Call:   call.ID, HeldCall: call.HeldCall, Operation: operationDigest(m)}
	if r.HeldCall != "" {
		// A HELD CALL IS NAMED BY ITS HELD CALL ALONE, as the log keys its act.
		r.Call = ""
	}
	if m.team != nil {
		r.Connection = m.team.id
	}
	if m.key.role != "" {
		r.Via = via(m.key.role)
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

// recordChanges are card changes as the access log accepts them: published
// words, free text and values it refuses in the clear go as digests; a field
// it may not name, or one past its bound, is left out and counted.
func recordChanges(ch []growth.CardChange, public bool) []growth.RecordChange {
	out := make([]growth.RecordChange, 0, len(ch))
	for _, c := range ch {
		label := c.Label
		if len(label) > accessdomain.GrowthLabelMaxBytes || !accessdomain.GrowthValueRecordable(label) {
			label = ""
		}
		text := (public || !inClear(c.Before) || !inClear(c.After)) && c.Before+c.After != ""
		out = append(out, growth.RecordChange{Field: c.Field, Label: label, Before: c.Before, After: c.After,
			Money: c.Money && !text, Text: text})
	}
	return accessdomain.RecordableGrowthChanges(out)
}

// tokenShape is a value kept in the clear: a state, a number, an ID or a flag,
// never words somebody wrote.
var tokenShape = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_.:+-]{1,128}$`) })

func inClear(v string) bool {
	return v == "" || (tokenShape().MatchString(v) && accessdomain.GrowthValueRecordable(v))
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
	case errors.Is(err, ErrRole), errors.Is(err, ErrReserve):
		return growth.FailureEngineError
	}
	return growth.FailureUnknownOutcome
}

// writeFailure is why a write failed: one never sent fails as its cause did;
// one that got no answer of Apple's own after its request left (its deadline,
// a timeout, a reset, an answer cut off) may have reached Apple, so what it
// did is not known.
func writeFailure(err error) growth.Failure {
	var netErr net.Error
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errUnsent):
		return failureOf(err)
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return growth.FailureUnknownOutcome
	}
	if f := failureOf(err); f != growth.FailureEngineError {
		return f
	}
	return growth.FailureUnknownOutcome
}

// argsDigest digests a call's arguments, for a step whose request cannot be
// made concrete again (a denial after the seat's access changed).
func argsDigest(call growth.Call) string {
	data, _ := json.Marshal(map[string]any{"tool": call.Tool, "args": call.Args})
	return hexSum(data)
}

// allow records an administrator's allow of a held act before it runs: the
// act runs only once the record landed as the act's one answer, since an
// earlier answer (the engine's lapse) stands.
func (p *Product) allow(ctx context.Context, r growth.Record, answerer string) error {
	if p.deps.Audit == nil {
		return growth.Refuse(growth.RefusedUnavailable, "this node records no act, so nothing was sent.")
	}
	r.Answerer = answerer
	first, err := p.deps.Audit.Verdict(ctx, growth.StepAllowed, r)
	switch {
	case err != nil:
		log.ErrorContext(ctx, "app_store_connect_unrecorded", "tool", r.Tool, "step", string(growth.StepAllowed),
			"error", err.Error())
		return growth.Refuse(growth.RefusedUnavailable, "the access log did not record the administrator's "+
			"answer, so nothing was sent. Call again.")
	case !first:
		return growth.Refuse(growth.RefusedForbidden, "the call already holds an answer (it lapsed, or another "+
			"answer came first), which stands. Nothing was sent.")
	}
	return nil
}

// note records a step after the act, logging a record that did not land as an
// error: the act is made, and its answer says so whatever the log said.
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
		log.ErrorContext(ctx, "app_store_connect_unrecorded", "tool", r.Tool, "step", string(step), "error", err.Error())
	}
}

// accountOf is the account a request names for the log, for a step whose
// call is not made concrete; "unknown" where it names none the log may keep.
func accountOf(args map[string]any) string {
	app, _ := args["app"].(string)
	if path, _ := args["path"].(map[string]any); app == "" && path != nil {
		app, _ = path["id"].(string)
	}
	if !accessdomain.GrowthIDRecordable(app) {
		return "unknown"
	}
	return app
}
