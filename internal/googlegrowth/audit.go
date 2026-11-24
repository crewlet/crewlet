package googlegrowth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/growth"
)

// Record is req's act as the access log names it, without what it changes:
// a read's, an answer's, or a step that ended before its target was read. A
// request that could not be built is named by its call's arguments.
func (c *Client) Record(call growth.Call, req Request, class growth.Class) growth.Record {
	r := growth.Record{
		Product: c.Kind, Connection: c.connection(), Account: req.AccountOrTarget(), Target: req.Target(),
		Tool: call.Tool, Class: class, Handle: call.Handle, Turn: call.Turn, Call: call.ID,
		HeldCall: call.HeldCall, Operation: req.Digest(nil),
	}
	r.Signer = c.Signer
	if req.Address == "" {
		r.Account, r.Target, r.Operation = unknownAccount, "", argsDigest(call)
	}
	if r.HeldCall != "" {
		// A RECORD NAMES ITS ACT ONCE: an approved call is its held call's.
		r.Call = ""
	}
	return r
}

// unknownAccount is the account a record names for a call whose request could
// not be built, so it names none.
const unknownAccount = "unknown"

// argsDigest digests a call's arguments, for a step whose request could not
// be built again (the seat's access or the catalog changed since the ask).
func argsDigest(call growth.Call) string {
	data, _ := json.Marshal(map[string]any{"tool": call.Tool, "args": call.Args})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// record is h's act as the access log holds it: c's record of its request,
// its target's before-state's digest and what it changes there.
func (h Held) record(c *Client, call growth.Call, class growth.Class) growth.Record {
	r := c.Record(call, h.Req, class)
	r.Before = digest(h.Before)
	changes := make([]growth.RecordChange, 0, len(h.changes()))
	for _, ch := range h.changes() {
		rc := RecordChange(ch)
		field := ch.Field[strings.LastIndex(ch.Field, ".")+1:]
		rc.Text = rc.Text || (slices.Contains(c.Texts, field) && rc.Before+rc.After != "")
		changes = append(changes, rc)
	}
	r.Changes = accessdomain.RecordableGrowthChanges(changes)
	return r
}

// digest is a before-state's SHA-256, "" where the target held none.
func digest(before []byte) string {
	if len(before) == 0 {
		return ""
	}
	sum := sha256.Sum256(before)
	return hex.EncodeToString(sum[:])
}

// textWords are the words of a field's name that say it holds somebody's
// words: a name, a title, a description, a person, an address or a URI.
var textWords = []string{"name", "names", "title", "titles", "description", "descriptions", "label", "labels",
	"comment", "comments", "note", "notes", "text", "message", "content", "query", "keyword", "keywords",
	"user", "users", "client", "email", "emails", "phone", "address", "uri", "uris", "url", "urls"}

// RecordChange is one change as the log keeps it. A field is kept in the
// clear by its name, never its value's shape, which free words can mimic: one
// whose name says it holds words ([textWords]), one whose value is structured
// (an object holds anything), or a value the log refuses in the clear goes as
// its digest and length (Text).
func RecordChange(ch growth.Change) growth.RecordChange {
	before, after := "", ""
	if ch.Before != nil {
		before = Words(ch.Before)
	}
	if ch.After != nil {
		after = Words(ch.After)
	}
	text := namesText(ch.Field) || structured(ch.Before) || structured(ch.After) ||
		!accessdomain.GrowthValueRecordable(before) || !accessdomain.GrowthValueRecordable(after)
	label := ch.Field
	if len(label) > accessdomain.GrowthLabelMaxBytes {
		label = ""
	}
	return growth.RecordChange{Field: ch.Field, Label: label, Before: before, After: after,
		Text: text && before+after != ""}
}

// namesText reports whether a dotted field's name holds one of [textWords],
// as words of its lowerCamel or snake case.
func namesText(field string) bool {
	var words []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			words = append(words, strings.ToLower(b.String()))
			b.Reset()
		}
	}
	prev := rune(0)
	for _, r := range field {
		switch {
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			b.WriteRune(r)
		case unicode.IsLetter(r):
			b.WriteRune(r)
		default:
			flush()
		}
		prev = r
	}
	flush()
	return slices.ContainsFunc(words, func(w string) bool { return slices.Contains(textWords, w) })
}

// structured reports a JSON object, or a list holding one: what it holds is
// nobody's to name field by field.
func structured(v any) bool {
	switch t := v.(type) {
	case map[string]any, conflict:
		return true
	case []any:
		return slices.ContainsFunc(t, structured)
	}
	return false
}

// note writes one step of an act; a write that fails is logged, since the
// call it records has already happened at Google.
func (c *Client) note(ctx context.Context, step growth.Step, r growth.Record) {
	var err error
	switch step {
	case growth.StepMade:
		_, err = c.Audit.Made(ctx, r)
	case growth.StepApplied:
		_, err = c.Audit.Applied(ctx, r)
	case growth.StepVoid:
		_, err = c.Audit.Void(ctx, r)
	case growth.StepFailed:
		_, err = c.Audit.Failed(ctx, r)
	default:
		_, err = c.Audit.Verdict(ctx, step, r)
	}
	if err != nil {
		slog.WarnContext(ctx, "growth_record_failed", "kind", c.Kind, "step", string(step), "tool", r.Tool,
			"seat", r.Handle, "err", err)
	}
}

// allow records an administrator's allow of a held act before it runs: the
// act runs only once the record landed as the act's one answer, since an
// earlier answer (the engine's lapse, another allow) stands.
func (c *Client) allow(ctx context.Context, r growth.Record, answerer string) error {
	r.Answerer = answerer
	first, err := c.Audit.Verdict(ctx, growth.StepAllowed, r)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "growth_record_failed", "kind", c.Kind, "step", string(growth.StepAllowed),
			"tool", r.Tool, "seat", r.Handle, "err", err)
		return growth.Refuse(growth.RefusedUnavailable, "the access log did not record the administrator's "+
			"answer, so nothing was sent. Call again.")
	case !first:
		return growth.Refuse(growth.RefusedForbidden, "the call already holds an answer (it lapsed, or another "+
			"answer came first), which stands. Nothing was sent.")
	}
	return nil
}

// MadeOrFailed records a call made at once once it reached for Google: made,
// or failed with its reason. A call refused before Google is no act, and a
// read records no change.
func (c *Client) MadeOrFailed(ctx context.Context, call growth.Call, req Request, class growth.Class, err error) {
	r := c.Record(call, req, class)
	if err == nil {
		c.note(ctx, growth.StepMade, r)
		return
	}
	if ce, ok := errors.AsType[*CallError](err); ok {
		r.Failure = ce.Failure
		c.note(ctx, growth.StepFailed, r)
	}
}

// Denied records a held call's one answer when it is not allowed: an
// administrator's deny, or the engine's lapse ([growth.Call.Denial]).
func (c *Client) Denied(ctx context.Context, call growth.Call, req Request, class growth.Class) {
	step, answerer := call.Denial()
	r := c.Record(call, req, class)
	r.Answerer = answerer
	c.note(ctx, step, r)
}

// failureOfStep is why a write ended without its result: Google's reason (its
// read's, or its write's), the seat's access lowered, or the engine's own.
func failureOfStep(err error) growth.Failure {
	if ce, ok := errors.AsType[*CallError](err); ok {
		if ce.Failure == growth.FailureUnknownOutcome && errors.Is(err, growth.ErrNotSent) {
			// A STAGED STEP CUT OFF before its edit's commit left changed
			// nothing: the vendor's answer was lost, not the outcome.
			return growth.FailureVendorUnavailable
		}
		return ce.Failure
	}
	if r, ok := errors.AsType[*growth.Refusal](err); ok && r.Kind == growth.RefusedForbidden {
		return growth.FailureAccessLowered
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return growth.FailureTimedOut
	}
	return growth.FailureEngineError
}
