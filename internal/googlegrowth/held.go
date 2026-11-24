package googlegrowth

import (
	"context"
	"encoding/json"
	"errors"
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

// Read is req held to its target as Google answers for it now, made from
// that state where req is shaped by it ([Request.Shape]).
func (c *Client) Read(ctx context.Context, handle string, req Request) (Held, error) {
	before, err := c.before(ctx, handle, req)
	if err != nil {
		return Held{Req: req}, err
	}
	return heldTo(req, before)
}

// heldTo is req held to before, made from that state where req is shaped by
// it.
func heldTo(req Request, before []byte) (Held, error) {
	if req.Shape == nil {
		return NewHeld(req, before), nil
	}
	sent, err := req.Shape(before)
	if err != nil {
		return Held{Req: req, Before: before}, &unshaped{err}
	}
	sent.Shape = nil
	return NewHeld(sent, before), nil
}

// unshaped is a target's state the change a request says does not apply to:
// at an apply, a state the approval did not hold.
type unshaped struct{ err error }

func (e *unshaped) Error() string { return e.err.Error() }
func (e *unshaped) Unwrap() error { return e.err }

// before is req's target's live state: the product's read of a batch's
// targets ([Client.BeforeRead]), its resource, or none for a write that acts
// on nothing one read returns ([Request.Resource]).
func (c *Client) before(ctx context.Context, handle string, req Request) ([]byte, error) {
	if c.BeforeRead != nil {
		before, ok, err := c.BeforeRead(ctx, handle, req)
		if err != nil || ok {
			return before, err
		}
	}
	address := req.Resource()
	if address == "" {
		return nil, nil
	}
	scopes := req.Op.Permissions
	if c.ReadScopes != nil {
		scopes = c.ReadScopes(req.Op)
	}
	get := growth.Operation{ID: req.Op.ID + ".before", Method: "GET", Path: address, Class: growth.ClassRead,
		Permissions: scopes}
	return c.Send(ctx, handle, Request{Op: get, Address: address})
}

// Class is the class h comes to against its target as read now
// ([growth.Classify]), on the same bytes its binding holds, each field by
// the lowerCamel name a catalog's rules use.
func (h Held) Class() growth.Class {
	changes := h.changes()
	for i := range changes {
		changes[i].Field = LowerCamel(changes[i].Field)
	}
	return growth.Classify(h.Req.Op, changes)
}

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

// Write is one write as a product hands it to [Client.Apply]: its request,
// built afresh from the call, or Err where that failed (the request kept as
// far as it was built, for the record); its tool's class, which every record
// of it names; the product's scope check, which Apply runs under the lease;
// the product's own read of its target where Google serves no get, nil for
// [Client.Read]; its last check there, on the target read again once the
// binding holds (a spend's caps, internal/spendguard), whose refusal ends the
// act failed with its reason; and how its answer is cut. A nil check or cut is
// none.
type Write struct {
	Req   Request
	Err   error
	Class growth.Class
	Scope func(ctx context.Context) error
	Read  func(ctx context.Context) ([]byte, error)
	Check func(ctx context.Context, h Held) error
	Cut   func(any) any
}

// Apply makes w once and records it: an operate call at once (made or
// failed), or an approved held call, whose administrator's answer is recorded
// first and which acts only on the act's one answer (applied, void or failed,
// every way it ends). Under the account's lease and within growth.ApplyBound
// it holds w to the seat's scope, reads its target again, sends nothing where
// binding no longer holds, and checks it last.
func (c *Client) Apply(ctx context.Context, call growth.Call, w Write, binding string) (any, error) {
	held := call.HeldCall != ""
	r := c.Record(call, w.Req, w.Class)
	if held {
		if err := c.allow(ctx, r, call.Approver); err != nil {
			return nil, err
		}
	}
	if w.Err != nil {
		return nil, c.failEarly(ctx, held, r, w.Err)
	}
	release, err := c.hold(ctx, "write/"+c.Kind+"/"+w.Req.AccountOrTarget())
	if err != nil {
		return nil, c.failEarly(ctx, held, r, err)
	}
	defer release()
	// EVERYTHING UNDER THE LEASE IS CUT AT ITS BOUND, so nothing acts once the
	// lease could lapse; the records take the caller's context.
	act, cancel := context.WithTimeout(ctx, growth.ApplyBound)
	defer cancel()
	if w.Scope != nil {
		if err = w.Scope(act); err != nil {
			return nil, c.failEarly(ctx, held, r, err)
		}
	}
	h, err := c.readTarget(act, call.Handle, w)
	if _, moved := errors.AsType[*unshaped](err); moved {
		// THE TARGET MOVED: the state its approval bound took the change.
		if held {
			r.Before = digest(h.Before)
			c.note(ctx, growth.StepVoid, r)
		}
		return nil, growth.ErrMoved
	}
	if err != nil {
		return nil, c.failEarly(ctx, held, r, err)
	}
	r = h.record(c, call, w.Class)
	if !growth.BindingHolds(binding, h.Binding) {
		if held {
			c.note(ctx, growth.StepVoid, r)
		}
		return nil, growth.ErrMoved
	}
	if w.Check != nil {
		if err = w.Check(act, h); err != nil {
			return nil, c.failEarly(ctx, held, r, err)
		}
	}
	out, err := c.SendAndRead(act, call.Handle, h.Req, w.Cut)
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

// readTarget is w's target as read now, w shaped by it: the product's own
// read where it has one, else [Client.Read].
func (c *Client) readTarget(ctx context.Context, handle string, w Write) (Held, error) {
	if w.Read == nil {
		return c.Read(ctx, handle, w.Req)
	}
	raw, err := w.Read(ctx)
	if err != nil {
		return Held{Req: w.Req}, err
	}
	return heldTo(w.Req, raw)
}

// hold takes resource's lease, or answers why it could not.
func (c *Client) hold(ctx context.Context, resource string) (func(), error) {
	if c.Leases == nil {
		return nil, growth.Refuse(growth.RefusedUnavailable, "this node cannot hold an act's lease, so nothing "+
			"was sent.")
	}
	release, err := c.Leases.Hold(ctx, resource)
	switch {
	case errors.Is(err, growth.ErrActHeld):
		return nil, growth.Refuse(growth.RefusedUnavailable, "another act on the same "+c.Name+" account is "+
			"being made; nothing was sent. Call again.")
	case err != nil:
		return nil, fmt.Errorf("googlegrowth: hold the %s account's act lease, so nothing was sent: %w", c.Name, err)
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
