package appstoreconnect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// Every write is prepared, then applied. Prepare classes it (the class its
// tool calls with, which concrete refuses where the target's state does not
// allow it) and binds the exact request and the target's state before it; a
// public or admin act also builds what an administrator reads (ADR-0171).
// Apply, under the app's lease, reads the target again and sends nothing if
// either moved: an operate call at once, a held one once approved.

// operationDigest is the SHA-256 of m's exact request.
func operationDigest(m made) string {
	keys := make([]string, 0, len(m.query))
	for k := range m.query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	q := make([][2]string, 0, len(keys))
	for _, k := range keys {
		q = append(q, [2]string{k, m.query.Get(k)})
	}
	data, _ := json.Marshal(map[string]any{"method": m.op.Method, "path": m.path, "query": q, "body": m.body})
	return hexSum(data)
}

func hexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// before is a held act's target as it reads now: its attributes, and their
// digest, which the approval binds. unread is a target the agent's key may
// not read: the approval binds the request alone.
type before struct {
	attrs  map[string]any
	digest string
	unread bool
}

// beforeOf reads m's target: the resource a PATCH or DELETE names; a POST's
// target is new, so only what its body relates to is bound.
func (p *Product) beforeOf(ctx context.Context, m made) (before, error) {
	typ, id := typeAndID(m.path)
	if id == "" || m.op.Method == "POST" {
		ref, _ := bodyTarget(m.body)
		data, _ := json.Marshal(ref)
		return before{attrs: map[string]any{}, digest: hexSum(data)}, nil
	}
	path := "/v1/" + typ + "/" + url.PathEscape(id)
	if err := m.reader(path); err != nil {
		return before{attrs: map[string]any{}, digest: hexSum([]byte("unread")), unread: true}, nil //nolint:nilerr // An unreadable target binds the request alone.
	}
	resp, err := m.do(ctx, Request{Method: "GET", Path: path, Read: true, Reserved: true})
	if err != nil {
		return before{}, err
	}
	var doc struct {
		Data struct {
			Attributes map[string]any `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		return before{}, fmt.Errorf("appstoreconnect: read %s %s: %w", typ, id, err)
	}
	data, _ := json.Marshal(doc.Data.Attributes)
	return before{attrs: doc.Data.Attributes, digest: hexSum(data)}, nil
}

// binding is what an approval binds: the request, the before-state and the
// class the call was asked as.
func binding(m made, b before) string {
	return hexSum([]byte(operationDigest(m) + "|" + b.digest + "|" + string(m.class)))
}

// changesOf are the attributes m's body sets, before and after.
func changesOf(m made, b before) []growth.CardChange {
	data, _ := m.body["data"].(map[string]any)
	attrs, _ := data["attributes"].(map[string]any)
	names := make([]string, 0, len(attrs))
	for n := range attrs {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]growth.CardChange, 0, len(names))
	for _, n := range names {
		out = append(out, growth.CardChange{Field: n, Label: n, Before: valueText(b.attrs[n]), After: valueText(attrs[n])})
	}
	return out
}

func valueText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	data, _ := json.Marshal(v)
	return string(data)
}

// reasonOf is an act's reason, redacted and bounded.
func reasonOf(args map[string]any) string {
	r, _ := args["reason"].(string)
	return textcut.Within(redact.Secrets(strings.TrimSpace(r)), 500)
}

// Prepare reads a write's target, classes it and binds it, and for a held
// call builds what an administrator reads.
func (p *Product) Prepare(ctx context.Context, call growth.Call) (growth.Hold, error) {
	if call.Tool == ToolReply {
		return p.prepareReplies(ctx, call)
	}
	m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
	if err != nil {
		return growth.Hold{}, err
	}
	if m.class.Held() && reasonOf(call.Args) == "" {
		return growth.Hold{}, invalid("`reason` is one line saying why, which the administrator reads.")
	}
	b, err := p.beforeOf(ctx, m)
	if err != nil {
		return growth.Hold{}, said(err)
	}
	if !m.class.Held() {
		return growth.Hold{Class: m.class, Binding: binding(m, b)}, nil
	}
	where := "the team " + m.team.id
	if m.app != "" {
		where = "app " + m.app
	}
	question := fmt.Sprintf("May %s %s %s on %s?", call.Handle, verbOf(m.op.Method), m.op.Resource, where)
	var ctxt strings.Builder
	fmt.Fprintf(&ctxt, "%s asks to make **%s** (%s %s) on %s, through its own %s key. Allow once sends exactly "+
		"this request, once.", call.Handle, m.op.ID, m.op.Method, words.Inert(m.path), where, roleName(m.key.role))
	if m.op.Danger {
		ctxt.WriteString(" **It cannot be undone.**")
	}
	if ch := changesOf(m, b); len(ch) > 0 {
		ctxt.WriteString("\n\n**Changes**\n")
		for _, c := range ch {
			fmt.Fprintf(&ctxt, "\n- %s: %s to %s", words.Inert(c.Field), words.Inert(textcut.Within(c.Before, 300)),
				words.Inert(textcut.Within(c.After, 300)))
		}
		ctxt.WriteString("\n\nThe call's page shows the whole request.")
	}
	ctxt.WriteString("\n\n**Reason given**\n\n" + words.Inert(reasonOf(call.Args)))
	ask := recordOf(call, m)
	ask.Before, ask.Changes = b.digest, recordChanges(changesOf(m, b), m.class == growth.ClassPublic)
	hold := growth.Hold{Class: m.class, Binding: binding(m, b), Question: question,
		Context: textcut.Within(ctxt.String(), sandbox.MaxAskContextBytes-1024),
		Subject: delegated.Subject(m.class.Act(), m.op.ID, m.path, operationDigest(m)), Ask: ask}
	return hold, nil
}

func verbOf(method string) string {
	switch method {
	case "POST":
		return "create"
	case "PATCH":
		return "change"
	case "DELETE":
		return "delete"
	}
	return "call"
}

// Card is a held call's decision card as its target reads now.
func (p *Product) Card(ctx context.Context, call growth.Call, approved string) (growth.Card, error) {
	if call.Tool == ToolReply {
		return p.replyCard(ctx, call, approved)
	}
	m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
	if err != nil {
		return growth.Card{}, err
	}
	b, err := p.beforeOf(ctx, m)
	if err != nil {
		return growth.Card{}, said(err)
	}
	var findings []string
	if b.unread {
		findings = append(findings, "The agent's own "+roleName(m.key.role)+" key may not read the target: "+
			"Allow once sends exactly this request without reading the target first.")
	}
	return cardOf(call, m, b, !growth.BindingHolds(approved, binding(m, b)), findings), nil
}

// cardOf is m's card over its target's state b.
func cardOf(call growth.Call, m made, b before, moved bool, findings []string) growth.Card {
	target := m.op.Resource
	if _, id := typeAndID(m.path); id != "" {
		target += " " + id
	}
	return growth.Card{Act: m.class.Act(), Product: Kind, Tool: call.Tool,
		Account: growth.Account{Kind: Kind, Connection: m.team.id, ID: m.app, State: growth.AccountServing},
		Acts: []growth.CardAct{{Target: target, Summary: verbOf(m.op.Method) + " " + m.op.Resource,
			Changes: changesOf(m, b), Danger: m.op.Danger}},
		Reason: reasonOf(call.Args), Moved: moved, Findings: findings}
}

// leaseOf is the lease a write on m holds: one write per app, or per team.
func leaseOf(m made) string {
	if m.app != "" {
		return "write/" + Kind + "/" + m.app
	}
	return "write/" + Kind + "/team/" + m.team.id
}

// Apply makes a prepared write once: an operate call at once, a held call
// once approved, which names its held call.
func (p *Product) Apply(ctx context.Context, call growth.Call, approved string) (any, error) {
	if call.Tool == ToolReply {
		if call.HeldCall == "" {
			return nil, growth.Refuse(growth.RefusedForbidden, "replies are made only once an administrator "+
				"allows them. No reply was posted.")
		}
		return p.applyReplies(ctx, call, approved)
	}
	held := call.HeldCall != ""
	if class := classOfTool(call.Tool); class.Held() != held {
		return nil, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s calls are %s, which is made only "+
			"once an administrator allows it. No call was made.", call.Tool, class))
	}
	m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
	rec := p.heldRecord(call, m, err == nil)
	if held {
		// THE ANSWER FIRST, then one outcome for every way the act ends.
		if aerr := p.allow(ctx, rec, call.Approver); aerr != nil {
			return nil, aerr
		}
	}
	if err != nil {
		return nil, p.failEarly(ctx, held, rec, err)
	}
	release, err := p.hold(ctx, leaseOf(m))
	if err != nil {
		return nil, p.failEarly(ctx, held, rec, err)
	}
	defer release()
	// EVERYTHING UNDER THE LEASE IS CUT AT ITS BOUND, so nothing acts once the
	// lease could lapse; the records take the caller's context.
	act, cancel := context.WithTimeout(ctx, p.applyBound)
	defer cancel()
	b, err := p.beforeOf(act, m)
	if err != nil {
		rec.Failure = failureOf(err)
		p.note(ctx, growth.StepFailed, rec)
		return nil, said(err)
	}
	rec.Before = b.digest
	if !growth.BindingHolds(approved, binding(m, b)) {
		if held {
			p.note(ctx, growth.StepVoid, rec)
		}
		return nil, growth.ErrMoved
	}
	resp, err := m.do(act, Request{Method: m.op.Method, Path: m.path, Query: m.query,
		Body: m.body, Reserved: held})
	if err != nil {
		rec.Failure = writeFailure(err)
		p.note(ctx, growth.StepFailed, rec)
		return nil, said(err)
	}
	rec.Changes = recordChanges(changesOf(m, b), m.class == growth.ClassPublic)
	step := growth.StepMade
	if held {
		step = growth.StepApplied
	}
	p.note(ctx, step, rec)
	if rootedAt(m, resp.Body) {
		p.owners.remember(m.app, resp.Body)
	}
	return Answer{Operation: m.op.ID, Status: resp.Status, Connection: m.team.id, Role: m.key.role,
		Document: string(resp.Body)}, nil
}

// hold takes resource's lease, or answers why it could not.
func (p *Product) hold(ctx context.Context, resource string) (func(), error) {
	if p.deps.Leases == nil {
		return nil, growth.Refuse(growth.RefusedUnavailable, "this node cannot hold an act's lease, so nothing was sent.")
	}
	release, err := p.deps.Leases.Hold(ctx, resource)
	switch {
	case errors.Is(err, growth.ErrActHeld):
		return nil, growth.Refuse(growth.RefusedUnavailable, "another act on the same app is being made; nothing was sent. Call again.")
	case err != nil:
		return nil, fmt.Errorf("%w: appstoreconnect: hold the app's act lease: %w", growth.ErrNotSent, err)
	}
	return release, nil
}

// Denied records that an administrator denied the call, or that it lapsed.
func (p *Product) Denied(ctx context.Context, call growth.Call) error {
	step, answerer := call.Denial()
	var rec growth.Record
	if call.Tool == ToolReply {
		m, rs, err := p.replies(ctx, call)
		rec = p.heldRecord(call, m, err == nil)
		if err == nil {
			rec.Operation = replyBinding(m.app, rs)
		}
	} else {
		m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
		rec = p.heldRecord(call, m, err == nil)
	}
	rec.Answerer = answerer
	p.note(ctx, step, rec)
	return nil
}

// heldRecord is a held call's record: from m where the call was made concrete,
// else from its arguments (the seat's access or the target changed since).
func (p *Product) heldRecord(call growth.Call, m made, concrete bool) growth.Record {
	if concrete {
		return recordOf(call, m)
	}
	return growth.Record{Product: Kind, Tool: call.Tool, Class: classOfTool(call.Tool), Handle: call.Handle,
		Turn: call.Turn, HeldCall: call.HeldCall, Operation: argsDigest(call), Account: accountOf(call.Args),
		Connection: p.connectionOf(call.Args)}
}

// connectionOf is the connection a call names for the log, the only one where
// it names none, else "unknown".
func (p *Product) connectionOf(args map[string]any) string {
	name, _ := args["connection"].(string)
	teams, err := p.teamsNow()
	for _, t := range teams {
		if err == nil && (t.id == name || (name == "" && len(teams) == 1)) {
			return t.id
		}
	}
	return "unknown"
}

// failEarly records the failed outcome of an allowed held call that ended
// before its request was sent, and answers err as the agent is told it.
func (p *Product) failEarly(ctx context.Context, held bool, rec growth.Record, err error) error {
	if held {
		rec.Failure = earlyFailure(err)
		p.note(ctx, growth.StepFailed, rec)
	}
	return said(err)
}

// earlyFailure is why an allowed call ended before its request was sent: the
// seat's access lowered, Apple's refusal of a read, or the engine's own.
func earlyFailure(err error) growth.Failure {
	var refused *growth.Refusal
	var apiErr *APIError
	switch {
	case errors.As(err, &refused) && refused.Kind == growth.RefusedForbidden:
		return growth.FailureAccessLowered
	case errors.As(err, &apiErr), errors.Is(err, context.DeadlineExceeded):
		return failureOf(err)
	}
	return growth.FailureEngineError
}

func classOfTool(tool string) growth.Class {
	if c, ok := toolClass[tool]; ok {
		return c
	}
	return growth.ClassPublic
}
