package metaads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"regexp"
	"strconv"
	"sync"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/spendguard"
)

func hexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// recordOf is the access log's record of call made as m in class: its account
// the ad account, or the business for a call no ad account owns, and the
// token slot it went through as its via.
func recordOf(call growth.Call, m made, class growth.Class, via string) growth.Record {
	r := growth.Record{Product: Kind, Tool: call.Tool, Class: class, Handle: call.Handle, Turn: call.Turn,
		Call: call.ID, HeldCall: call.HeldCall, Operation: operationDigest(m), Connection: connection,
		Account: m.account, Via: via, Signer: growth.SignerCompany}
	if m.own != nil {
		r.Via, r.Signer = "", growth.SignerOwn
	}
	if r.HeldCall != "" {
		// A HELD CALL IS NAMED BY ITS HELD CALL ALONE, as the log keys its act.
		r.Call = ""
	}
	if r.Account == "" {
		r.Account = "business"
	}
	if len(m.targets) == 1 && m.targets[0].node != "" && !accountShape().MatchString(m.targets[0].node) {
		t := m.targets[0]
		if id := targetName(t.row.op.Resource, t.node); accessdomain.GrowthIDRecordable(id) {
			r.Target = id
		}
	}
	return r
}

// targetName is how a record names an object: its node type in snake case
// and its ID, as the budget limit counts changes to it.
func targetName(resource, id string) string { return snake(resource) + "/" + id }

func snake(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 && (s[i-1] < 'A' || s[i-1] > 'Z') {
				out = append(out, '_')
			}
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// heldRecord is a held call's record: from m where the call was made
// concrete, else from its arguments (the seat's access or the target changed
// since).
func heldRecord(call growth.Call, m made, concrete bool) growth.Record {
	if concrete {
		return recordOf(call, m, toolClass[call.Tool], "")
	}
	return growth.Record{Product: Kind, Tool: call.Tool, Class: toolClass[call.Tool], Handle: call.Handle,
		Turn: call.Turn, HeldCall: call.HeldCall, Operation: argsDigest(call), Account: accountArg(call.Args),
		Connection: connection}
}

// recordChanges are card changes as the access log accepts them: free text and
// values it refuses in the clear go as digests; a field it may not name, or
// one past its bound, is left out and counted.
func recordChanges(ch []growth.CardChange) []growth.RecordChange {
	out := make([]growth.RecordChange, 0, len(ch))
	left := 0
	for _, c := range ch {
		if !accessdomain.GrowthFieldRecordable(c.Field) || len(out) == accessdomain.MaxGrowthChanges-1 {
			left++
			continue
		}
		label := c.Label
		if len(label) > accessdomain.GrowthLabelMaxBytes || !accessdomain.GrowthValueRecordable(label) {
			label = ""
		}
		text := (!inClear(c.Before) || !inClear(c.After)) && c.Before+c.After != ""
		out = append(out, growth.RecordChange{Field: c.Field, Label: label, Before: c.Before, After: c.After,
			Money: c.Money && !text, Text: text})
	}
	if left > 0 {
		out = append(out, growth.RecordChange{Field: unrecordedField, Label: "Fields left out", After: strconv.Itoa(left)})
	}
	return out
}

// unrecordedField is the change that counts the fields a record leaves out.
const unrecordedField = "unrecorded_fields"

// tokenShape is a value kept in the clear: a status, a number, an ID or a
// flag, never words somebody wrote, such as a campaign's name.
var tokenShape = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9_.:+-]{1,128}$`) })

func inClear(v string) bool {
	return v == "" || (tokenShape().MatchString(v) && accessdomain.GrowthValueRecordable(v))
}

// failureOf is a failure's closed word for the access log.
func failureOf(err error) growth.Failure {
	var apiErr *APIError
	var capped *spendguard.Refusal
	var refused *growth.Refusal
	switch {
	case errors.As(err, &capped):
		return growth.FailureCapExceeded
	case errors.Is(err, ErrTokenRefused):
		return growth.FailureUnauthorized
	case errors.Is(err, ErrQuota), errors.Is(err, ErrReserve):
		return growth.FailureRateLimited
	case errors.As(err, &apiErr) && apiErr.Code == 190:
		return growth.FailureUnauthorized
	case errors.As(err, &apiErr) && apiErr.Throttled():
		return growth.FailureRateLimited
	case errors.As(err, &apiErr) && apiErr.Status >= 500:
		return growth.FailureVendorUnavailable
	case errors.As(err, &apiErr):
		return growth.FailureVendorRefused
	case errors.Is(err, context.DeadlineExceeded):
		return growth.FailureTimedOut
	case errors.As(err, &refused) && refused.Kind == growth.RefusedForbidden:
		return growth.FailureAccessLowered
	}
	return growth.FailureEngineError
}

// writeFailure is why a write failed: one never sent fails as its cause did;
// one that got no answer of Meta's own after its request left (its deadline,
// a timeout, a reset, an answer cut off) may have reached Meta, so what it
// did is not known.
func writeFailure(err error) growth.Failure {
	var netErr net.Error
	switch {
	case errors.Is(err, growth.ErrNotSent):
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

// accountArg is the ad account a call names for the log, for a step whose
// call is not made concrete; "unknown" where it names none the log may keep.
func accountArg(args map[string]any) string {
	if a, _ := args["account"].(string); a != "" && accessdomain.GrowthIDRecordable(a) {
		return a
	}
	return "unknown"
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
		log.ErrorContext(ctx, "meta_ads_unrecorded", "tool", r.Tool, "step", string(growth.StepAllowed),
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
		log.ErrorContext(ctx, "meta_ads_unrecorded", "tool", r.Tool, "step", string(step), "error", err.Error())
	}
}
