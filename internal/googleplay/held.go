package googleplay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// Prepare classes a write against its targets as read now and builds what a
// person is asked where it is held.
func (p *Product) Prepare(ctx context.Context, call growth.Call) (growth.Hold, error) {
	if call.Tool == toolReply {
		return p.prepareReplies(ctx, call)
	}
	a, err := p.actOf(ctx, call)
	if err != nil {
		return growth.Hold{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Google Play, app %s.", a.app)
	if a.edit {
		b.WriteString(" Staged in one edit and committed together, never cancelling changes in review unless " +
			"it says so.")
	}
	for i, s := range a.steps {
		fmt.Fprintf(&b, "\n\n**Change %d:** %s. %s", i+1, s.req.Describe(), changeWords(s))
	}
	if a.reason != "" {
		b.WriteString("\n\n**Reason given**\n\n" + words.Inert(a.reason))
	}
	ids := []string{a.app}
	for _, s := range a.steps {
		ids = append(ids, s.req.Op.ID, s.req.Address)
	}
	return growth.Hold{
		Class: a.class, Binding: binding(a),
		Subject:  delegated.Subject(a.class.Act(), ids...),
		Question: fmt.Sprintf("Allow %s to %s on Google Play app %s?", call.Handle, a.what(), a.app),
		Context:  textcut.Within(b.String(), sandbox.MaxAskContextBytes-1024),
		Ask:      p.record(call, a),
	}, nil
}

// what says what a does, in a few words.
func (a act) what() string {
	if a.summary != "" {
		return a.summary
	}
	if len(a.steps) == 1 {
		return a.steps[0].req.Describe()
	}
	return fmt.Sprintf("commit %d changes in one edit", len(a.steps))
}

// changeWords lists a step's changes for the question's context.
func changeWords(s step) string {
	var out []string
	for _, ch := range changes(s) {
		out = append(out, fmt.Sprintf("%s: %s to %s", ch.Field, words.Inert(textcut.Within(googlegrowth.Words(
			ch.Before), 300)), words.Inert(textcut.Within(googlegrowth.Words(ch.After), 300))))
	}
	if s.media != nil {
		out = append(out, fmt.Sprintf("uploads %s (%d bytes, SHA-256 %s)", s.media.Ref, len(s.media.Data),
			s.media.SHA256))
	}
	if len(out) == 0 {
		return "It changes no field."
	}
	return "Changes " + strings.Join(out, "; ") + "."
}

// changes is what s changes on its target as it reads now.
func changes(s step) []growth.Change {
	var current map[string]any
	_ = json.Unmarshal(s.before, &current)
	return s.req.Changes(current)
}

// record is a's ask as the access log keeps it: the act's binding as its
// operation, every step's changes, free words as their digest and length.
func (p *Product) record(call growth.Call, a act) growth.Record {
	r := p.client.Record(call, a.steps[0].req, a.class)
	r.Account, r.Operation = a.app, binding(a)
	if a.edit {
		r.Target = "applications/" + a.app + "/edits"
	}
	r.Changes = nil
	var before []byte
	for _, s := range a.steps {
		before = append(before, s.before...)
		for _, ch := range changes(s) {
			r.Changes = append(r.Changes, googlegrowth.RecordChange(ch))
		}
	}
	if len(before) > 0 {
		r.Before = hexSum(before)
	}
	return r
}

// Card is a held call's decision card as its targets read now.
func (p *Product) Card(ctx context.Context, call growth.Call, approved string) (growth.Card, error) {
	if call.Tool == toolReply {
		return p.replyCard(ctx, call, approved)
	}
	a, err := p.actOf(ctx, call)
	if err != nil {
		return growth.Card{}, err
	}
	card := growth.Card{Act: a.class.Act(), Product: string(Kind), Tool: call.Tool, Reason: a.reason,
		Account: growth.Account{Kind: string(Kind), Connection: googlegrowth.Connection, ID: a.app,
			State: growth.AccountUnknown},
		Moved: !growth.BindingHolds(approved, binding(a))}
	for _, s := range a.steps {
		ca := growth.CardAct{Target: s.req.Target(), Summary: s.req.Describe(), Danger: s.req.Op.Danger}
		for _, ch := range changes(s) {
			ca.Changes = append(ca.Changes, growth.CardChange{Field: ch.Field, Label: ch.Field,
				Before: googlegrowth.Words(ch.Before), After: googlegrowth.Words(ch.After)})
		}
		if s.media != nil {
			ca.Changes = append(ca.Changes, growth.CardChange{Field: "media", Label: "File uploaded",
				After: fmt.Sprintf("%s (%d bytes, SHA-256 %s)", s.media.Ref, len(s.media.Data), s.media.SHA256)})
		}
		if micros, currency, ok := orderTotal(s); ok && s.class == growth.ClassSpend {
			// A REFUND'S MONEY IS THE ORDER'S, read now: what goes back to the buyer.
			card.Account.Currency = currency
			ca.Changes = append(ca.Changes, growth.CardChange{Field: "total", Label: "Paid back to the buyer",
				After: strconv.FormatInt(micros, 10), Money: true})
		}
		card.Acts = append(card.Acts, ca)
	}
	if a.edit && commitQuery(a).Get("changesInReviewBehavior") != errorIfInReview {
		card.Findings = append(card.Findings, "This commit cancels the changes already in review at Google Play "+
			"and sends these in their place.")
	}
	return card, nil
}

// reread is an act's record from its targets as read under the lease, under
// the operation the administrator approved, so every outcome names what was
// asked and what it met.
func (p *Product) reread(call growth.Call, a act, class growth.Class, approved string) growth.Record {
	r := p.record(call, a)
	r.Class, r.Operation = class, approved
	return r
}

// orderTotal is what the order a refund step names was paid, in micros of its
// currency, from the order as read now; false for any other step.
func orderTotal(s step) (int64, string, bool) {
	if s.req.Op.ID != "androidpublisher.orders.refund" && s.req.Op.ID != "androidpublisher.orders.reviewrefund" {
		return 0, "", false
	}
	var order struct {
		Total struct {
			Units    string `json:"units"`
			Nanos    int64  `json:"nanos"`
			Currency string `json:"currencyCode"`
		} `json:"total"`
	}
	if json.Unmarshal(s.before, &order) != nil || order.Total.Currency == "" {
		return 0, "", false
	}
	units, err := strconv.ParseInt(order.Total.Units, 10, 64)
	if err != nil {
		return 0, "", false
	}
	return units*1_000_000 + order.Total.Nanos/1_000, order.Total.Currency, true
}

// Apply makes a prepared write once: an operate call at once, a held call
// once allowed. Under the app's write lease it reads every target again
// (inside the edit it commits) and sends nothing where any moved.
func (p *Product) Apply(ctx context.Context, call growth.Call, approved string) (any, error) {
	if call.Tool == toolReply {
		return p.applyReplies(ctx, call, approved)
	}
	held := call.HeldCall != ""
	tool, _ := p.Catalog().Tool(call.Tool)
	if tool.Class.Held() != held {
		return nil, refusal(growth.RefusedForbidden, fmt.Sprintf("%s calls are %s, made only once an "+
			"administrator allows one. Nothing was sent.", call.Tool, tool.Class))
	}
	a, err := p.build(ctx, call)
	rec := p.client.Record(call, googlegrowth.Request{Op: growth.Operation{ID: call.Tool}}, tool.Class)
	if err == nil {
		rec = p.record(call, a)
		// THE ANSWER AND EVERY OUTCOME NAME THE OPERATION THE ASK BOUND.
		rec.Class, rec.Operation = tool.Class, approved
		for _, s := range a.steps {
			if err = p.inScope(call.Handle, s.req); err != nil {
				break
			}
		}
	}
	if held {
		// THE ANSWER FIRST, then one outcome for every way the act ends.
		if aerr := p.client.Allow(ctx, rec, call.Approver); aerr != nil {
			return nil, aerr
		}
	}
	if err != nil {
		return nil, p.failEarly(ctx, held, rec, err)
	}
	release, err := p.hold(ctx, "write/"+string(Kind)+"/"+a.app)
	if err != nil {
		return nil, p.failEarly(ctx, held, rec, err)
	}
	defer release()
	bound, cancel := context.WithTimeout(ctx, growth.ApplyBound)
	defer cancel()
	if a.edit {
		return p.commitEdit(bound, ctx, call, a, approved, rec)
	}
	if err = p.readBefores(bound, call.Handle, &a, ""); err != nil {
		return nil, p.failEarly(ctx, held, rec, err)
	}
	a.class = p.classOfAct(a)
	rec = p.reread(call, a, rec.Class, approved)
	if !growth.BindingHolds(approved, binding(a)) {
		if held {
			p.client.Note(ctx, growth.StepVoid, rec)
		}
		return nil, growth.ErrMoved
	}
	s := a.steps[0]
	out, err := p.send(bound, call.Handle, s)
	return p.outcome(ctx, held, rec, out, err)
}

// send makes one step: its file as the upload's body where it has one.
func (p *Product) send(ctx context.Context, handle string, s step) (googlegrowth.Answer, error) {
	if s.media != nil {
		q := url.Values{}
		for k, v := range s.req.Query {
			q[k] = v
		}
		q.Set("uploadType", "media")
		s.req.Query, s.req.Body = q, nil
		raw, err := p.client.SendBytes(ctx, handle, s.req, s.media.ContentType, s.media.Data)
		if err != nil {
			return googlegrowth.Answer{}, err
		}
		return p.client.AnswerOf(s.req, raw, nil)
	}
	return p.client.SendAndRead(ctx, handle, s.req, nil)
}

// commitEdit makes an edit act: opens the edit, reads every target in it,
// stages each change, validates and commits, discarding the edit on any
// failure. records takes the caller's context, the vendor calls bound's.
func (p *Product) commitEdit(bound, records context.Context, call growth.Call, a act, approved string,
	rec growth.Record) (any, error) {
	held := call.HeldCall != ""
	id, err := p.openEdit(bound, call.Handle, a.app)
	if err != nil {
		return nil, p.failEarly(records, held, rec, err)
	}
	committed := false
	defer func() {
		if !committed {
			p.discardEdit(records, call.Handle, a.app, id)
		}
	}()
	if err = p.readBefores(bound, call.Handle, &a, id); err != nil {
		return nil, p.failEarly(records, held, rec, err)
	}
	if err = shapeHalt(&a); err != nil {
		return nil, p.failEarly(records, held, rec, err)
	}
	a.class = p.classOfAct(a)
	rec = p.reread(call, a, rec.Class, approved)
	if !growth.BindingHolds(approved, binding(a)) {
		if held {
			p.client.Note(records, growth.StepVoid, rec)
		}
		return nil, growth.ErrMoved
	}
	for _, s := range a.steps {
		s.req = inEdit(s.req, id)
		if _, err = p.send(bound, call.Handle, s); err != nil {
			return p.outcome(records, held, rec, googlegrowth.Answer{}, err)
		}
	}
	validate, err := p.sessionRequest(opEditValidate, a.app, id, nil)
	if err == nil {
		_, err = p.client.Send(bound, call.Handle, validate)
	}
	if err != nil {
		return p.outcome(records, held, rec, googlegrowth.Answer{}, err)
	}
	commit, err := p.sessionRequest(opEditCommit, a.app, id, commitQuery(a))
	if err != nil {
		return p.outcome(records, held, rec, googlegrowth.Answer{}, err)
	}
	out, err := p.client.SendAndRead(bound, call.Handle, commit, nil)
	committed = err == nil
	return p.outcome(records, held, rec, out, inReview(err))
}

// inReview says a commit refused because changes are already in review in
// the words the agent acts on; any other error passes.
func inReview(err error) error {
	if err != nil && strings.Contains(err.Error(), "in review") {
		return refusal(growth.RefusedConflict, "Google Play holds changes in review for this app, which Crewlet "+
			"never cancels unless the approved call says so. Nothing was committed: try after the review, or ask "+
			"again with query.changesInReviewBehavior CANCEL_IN_REVIEW_AND_SUBMIT. ("+err.Error()+")")
	}
	return err
}

// outcome records how a sent act ended and answers it.
func (p *Product) outcome(ctx context.Context, held bool, rec growth.Record, out googlegrowth.Answer, err error) (
	any, error) {
	switch {
	case err != nil:
		rec.Failure = googlegrowth.StepFailure(err)
		p.client.Note(ctx, growth.StepFailed, rec)
		return nil, err
	case held:
		p.client.Note(ctx, growth.StepApplied, rec)
	default:
		p.client.Note(ctx, growth.StepMade, rec)
	}
	return out, nil
}

// failEarly records the failed outcome of an allowed call that ended before
// anything was sent, and answers err.
func (p *Product) failEarly(ctx context.Context, held bool, rec growth.Record, err error) error {
	if held {
		rec.Failure = googlegrowth.StepFailure(err)
		p.client.Note(ctx, growth.StepFailed, rec)
	}
	return err
}

// hold takes resource's lease, or answers why it could not.
func (p *Product) hold(ctx context.Context, resource string) (func(), error) {
	if p.deps.Leases == nil {
		return nil, refusal(growth.RefusedUnavailable, "this node cannot hold an app's write lease, so nothing "+
			"was sent.")
	}
	release, err := p.deps.Leases.Hold(ctx, resource)
	switch {
	case errors.Is(err, growth.ErrActHeld):
		return nil, refusal(growth.RefusedUnavailable, "another change to the same app is being made; nothing "+
			"was sent. Call again.")
	case err != nil:
		return nil, fmt.Errorf("%w: googleplay: hold the app's write lease: %w", growth.ErrNotSent, err)
	}
	return release, nil
}

// Denied records that an administrator denied the held call, or that it
// lapsed unanswered.
func (p *Product) Denied(ctx context.Context, call growth.Call) error {
	tool, _ := p.Catalog().Tool(call.Tool)
	rec := p.client.Record(call, googlegrowth.Request{Op: growth.Operation{ID: call.Tool}}, tool.Class)
	if call.Tool == toolReply {
		if m, err := p.replies(ctx, call); err == nil {
			rec = p.replyRecord(call, m)
		}
	} else if a, err := p.build(ctx, call); err == nil {
		rec = p.record(call, a)
		rec.Class = tool.Class
	}
	p.client.Deny(ctx, call, rec)
	return nil
}
