package googlegrowth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
)

// Held is a write's request, its target's live state and the binding a
// person's approval is to. A product builds the request, holding it to the
// seat's scope first, and [Client.Read] reads its target.
type Held struct {
	Req     Request
	Before  []byte
	Binding string
}

// NewHeld is req held to before, its target's live state.
func NewHeld(req Request, before []byte) Held {
	return Held{Req: req, Before: before, Binding: req.Digest(before)}
}

// Read is req held to its target as Google answers for it now, or to none for
// a write that acts on nothing one read returns ([Request.Resource]).
func (c *Client) Read(ctx context.Context, handle string, req Request) (Held, error) {
	address := req.Resource()
	if address == "" {
		return NewHeld(req, nil), nil
	}
	scopes := req.Op.Permissions
	if c.ReadScopes != nil {
		scopes = c.ReadScopes(req.Op)
	}
	get := growth.Operation{ID: req.Op.ID + ".before", Method: "GET", Path: address, Class: growth.ClassRead,
		Permissions: scopes}
	raw, err := c.Send(ctx, handle, Request{Op: get, Address: address})
	if err != nil {
		return Held{Req: req}, err
	}
	return NewHeld(req, raw), nil
}

// Class is the class h comes to against its target as read now
// ([growth.Classify]), on the same bytes its binding holds.
func (h Held) Class() growth.Class { return growth.Classify(h.Req.Op, h.changes()) }

// changes is what h changes on its target as it reads now.
func (h Held) changes() []growth.Change {
	var current map[string]any
	_ = json.Unmarshal(h.Before, &current)
	return h.Req.Changes(current)
}

// changeWords lists a held call's changes for the question's context.
func (h Held) changeWords() string {
	var out []string
	for _, ch := range h.changes() {
		out = append(out, fmt.Sprintf("%s: %s to %s", ch.Field, Words(ch.Before), Words(ch.After)))
	}
	if len(out) == 0 {
		return "It changes no field."
	}
	return "Changes " + strings.Join(out, "; ") + "."
}

// Hold is h prepared at class, the class its change comes to against its
// target as read now, which the framework holds to the tool's: what a person
// is asked where it is held, and the ask the access log records then.
func (c *Client) Hold(call growth.Call, h Held, class growth.Class) growth.Hold {
	return growth.Hold{
		Class: class, Binding: h.Binding, Subject: h.Req.Subject(c.Kind),
		Question: fmt.Sprintf("Allow %s to %s?", call.Handle, h.Req.Describe()),
		Context:  c.Name + " " + h.Req.Op.ID + " on " + h.Req.Target() + ". " + h.changeWords(),
		Ask:      h.record(c, call, class),
	}
}

// Card is h's decision card as its target reads now, made at class; Moved
// where it is no longer what binding approved.
func (c *Client) Card(call growth.Call, h Held, binding string, class growth.Class) growth.Card {
	act := growth.CardAct{Target: h.Req.Target(), Summary: h.Req.Describe(), Danger: h.Req.Op.Danger}
	for _, ch := range h.changes() {
		act.Changes = append(act.Changes, growth.CardChange{Field: ch.Field, Label: ch.Field,
			Before: Words(ch.Before), After: Words(ch.After)})
	}
	return growth.Card{
		Act: class.Act(), Product: c.Kind, Tool: call.Tool,
		Account: growth.Account{Kind: c.Kind, ID: h.Req.AccountOrTarget(), State: growth.AccountUnknown},
		Acts:    []growth.CardAct{act},
		Moved:   !growth.BindingHolds(binding, h.Binding),
	}
}

// Apply makes a write once and records it at its tool's class: an operate
// call at once (made or failed), or an approved held call, whose
// administrator's answer is recorded first and which acts only on the act's
// one answer (applied, void or failed, every way it ends). req is the
// product's fresh build of the call, held to the seat's scope; built is why
// that failed. Under the account's lease and within growth.ApplyBound it reads
// the target again and sends nothing where binding no longer holds. cut
// shapes the answer.
func (c *Client) Apply(ctx context.Context, call growth.Call, req Request, built error, binding string,
	class growth.Class, cut func(any) any) (any, error) {
	held := call.HeldCall != ""
	r := c.Record(call, req, class)
	if held {
		if err := c.allow(ctx, r, call.Approver); err != nil {
			return nil, err
		}
	}
	if built != nil {
		return nil, c.failEarly(ctx, held, r, built)
	}
	release, err := c.hold(ctx, "write/"+c.Kind+"/"+req.AccountOrTarget())
	if err != nil {
		return nil, c.failEarly(ctx, held, r, err)
	}
	defer release()
	// EVERYTHING UNDER THE LEASE IS CUT AT ITS BOUND, so nothing acts once the
	// lease could lapse; the records take the caller's context.
	act, cancel := context.WithTimeout(ctx, growth.ApplyBound)
	defer cancel()
	h, err := c.Read(act, call.Handle, req)
	if err != nil {
		return nil, c.failEarly(ctx, held, r, err)
	}
	r = h.record(c, call, class)
	if !growth.BindingHolds(binding, h.Binding) {
		if held {
			c.note(ctx, growth.StepVoid, r)
		}
		return nil, growth.ErrMoved
	}
	out, err := c.SendAndRead(act, call.Handle, h.Req, cut)
	switch {
	case err != nil:
		r.Failure = failureOfStep(err)
		c.note(ctx, growth.StepFailed, r)
		return nil, err
	case held:
		c.note(ctx, growth.StepApplied, r)
	default:
		c.note(ctx, growth.StepMade, r)
	}
	return out, nil
}

// hold takes resource's lease, or answers why it could not.
func (c *Client) hold(ctx context.Context, resource string) (func(), error) {
	if c.Leases == nil {
		return nil, growth.Refuse(growth.RefusedUnavailable, "this node cannot hold an act's lease, so nothing "+
			"was sent.")
	}
	release, err := c.Leases.Hold(ctx, resource)
	if err != nil {
		return nil, growth.Refuse(growth.RefusedUnavailable, "another act on the same "+c.Name+" account is "+
			"being made; nothing was sent. Call again.")
	}
	return release, nil
}

// failEarly records the failed outcome of an allowed held call that ended
// before its write was sent, and answers err; an operate call that sent
// nothing is no act.
func (c *Client) failEarly(ctx context.Context, held bool, r growth.Record, err error) error {
	if held {
		r.Failure = failureOfStep(err)
		c.note(ctx, growth.StepFailed, r)
	}
	return err
}
