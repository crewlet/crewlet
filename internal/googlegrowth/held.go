package googlegrowth

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/growth"
)

// Held is a held call's request, its target's live state and the binding a
// person's approval is to. A product builds it, holding the request to the
// seat's scope and its tool's class first.
type Held struct {
	Req     Request
	Before  []byte
	Binding string
}

// NewHeld is req held to before, its target's live state.
func NewHeld(req Request, before []byte) Held {
	return Held{Req: req, Before: before, Binding: req.Digest(before)}
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
		Ask:      c.Record(call, h.Req, class, h.Before),
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

// Apply makes a prepared write once and records it: an operate call made at
// once (made or failed), or an approved held call (applied, void or failed).
// h and prepared are the product's fresh read of the call, its target read
// again and held to the seat's scope; nothing is sent where that failed or
// where the target moved since binding was taken. cut shapes the answer.
func (c *Client) Apply(ctx context.Context, call growth.Call, h Held, prepared error, binding string,
	class growth.Class, cut func(any) any) (any, error) {
	held := call.HeldCall != ""
	if prepared != nil {
		if held && h.Req.Op.ID != "" {
			r := c.Record(call, h.Req, class, nil)
			r.Failure = applyFailure(prepared)
			c.note(ctx, growth.StepFailed, r)
		}
		return nil, prepared
	}
	r := c.Record(call, h.Req, class, h.Before)
	if !growth.BindingHolds(binding, h.Binding) {
		if held {
			c.note(ctx, growth.StepVoid, r)
		}
		return nil, growth.ErrMoved
	}
	out, err := c.SendAndRead(ctx, call.Handle, h.Req, cut)
	switch {
	case err != nil:
		r.Failure = applyFailure(err)
		c.note(ctx, growth.StepFailed, r)
		return nil, err
	case held:
		c.note(ctx, growth.StepApplied, r)
	default:
		c.note(ctx, growth.StepMade, r)
	}
	return out, nil
}
