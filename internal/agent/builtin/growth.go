package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE GROWTH TOOLS (internal/growth; ADR-0170, ADR-0171): every tool a growth
// product's catalog lists, built from its row, so a product adds its tools by
// listing them. A read or operate call is made at once for a seat whose lane
// level allows its class. A public, spend or admin call is held as ssh_operate
// is (a fixed tool, Allow once or Deny), read live once before it is held
// ([preparedTool]) and answered by an administrator alone at a fresh step-up;
// it lapses unanswered after a day. No credential reaches a box: the product
// makes every call in the engine.

// GrowthDeps are what the growth tools act through. Requesters and Chart decide
// whoever a turn acts for ([companyRequester]); without them, or without a
// product, no growth tool is registered.
type GrowthDeps struct {
	Products   []GrowthProduct
	Requesters Requesters
	Chart      authz.Chart
}

func (d GrowthDeps) serves() bool {
	return len(d.Products) > 0 && d.Requesters != nil && d.Chart != nil
}

// GrowthProduct is one growth integration as its tools reach it: implemented
// by its package and wired by the engine.
type GrowthProduct interface {
	// Catalog is the product's catalog; its tools are the ones registered.
	Catalog() growth.Catalog
	// Level is the seat's level on the product's lane now, "" for a seat the
	// lane is off for.
	Level(handle string) growth.Level
	// Run makes a read or operate call. Its answer encodes as a JSON object
	// and declares every field holding somebody's words (ADR-0040). A
	// [*growth.Refusal] reached no vendor and is said to the agent.
	Run(ctx context.Context, call growth.Call) (any, error)
	// Prepare reads a held call's target live and builds what a person is
	// asked: refused (a [*growth.Refusal]: out of scope, the spend guard, the
	// vendor's own validation), or the hold.
	Prepare(ctx context.Context, call growth.Call) (growth.Hold, error)
	// Card is a held call's decision card as its target reads now, Moved where
	// it no longer matches binding.
	Card(ctx context.Context, call growth.Call, binding string) (growth.Card, error)
	// Apply makes an approved held call once: under the account's apply lease
	// it reads the target again and answers [growth.ErrMoved] without calling
	// the vendor where it no longer matches binding, and re-checks the seat's
	// level and scope and, for spend, the caps.
	Apply(ctx context.Context, call growth.Call, binding string) (any, error)
	// Denied records that a person denied the held call, or that it lapsed
	// (call.Approver is the engine's actor).
	Denied(ctx context.Context, call growth.Call) error
}

// growthRefusals maps a product's refusal onto the surface's.
var growthRefusals = map[growth.RefusalKind]tools.Refusal{
	growth.RefusedForbidden: tools.RefusalForbidden, growth.RefusedInvalid: tools.RefusalInvalid,
	growth.RefusedNotFound: tools.RefusalNotFound, growth.RefusedConflict: tools.RefusalConflict,
	growth.RefusedUnavailable: tools.RefusalUnavailable,
}

// growthTerms are every growth tool's requester terms.
var growthTerms = requesterTerms{action: authz.ActionGrowthRequester,
	done: "the company's growth accounts are reached", nothing: "No vendor was reached."}

// growthCandidates are every product's tools, each by its class.
func growthCandidates(deps GrowthDeps) []candidate {
	var out []candidate
	for _, p := range deps.Products {
		for _, def := range p.Catalog().Tools {
			base := growthTool{deps: deps, product: p, def: def}
			if def.Class.Held() {
				out = append(out, candidate{&growthHeldTool{base}, deps.serves()})
				continue
			}
			out = append(out, candidate{&base, deps.serves()})
		}
	}
	return out
}

// growthAnnotations classifies a growth tool; false for a name no registered
// catalog lists.
func growthAnnotations(name string) (tools.Annotations, bool) {
	kind, ok := growth.KindOfTool(name)
	if !ok {
		return tools.Annotations{}, false
	}
	c, _ := growth.CatalogOf(kind)
	def, _ := c.Tool(name)
	if def.Class == growth.ClassRead {
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes, OpenWorld: mcp.Yes}, true
	}
	return tools.Annotations{ReadOnly: mcp.No, Destructive: mcp.Yes, Idempotent: mcp.No, OpenWorld: mcp.Yes}, true
}

// growthTool is a read or operate tool of a product's catalog.
type growthTool struct {
	deps    GrowthDeps
	product GrowthProduct
	def     growth.Tool
}

var _ tools.SeatCallable = (*growthTool)(nil)

func (t *growthTool) Name() string { return t.def.Name }

func (t *growthTool) Description() string { return t.def.Description }

func (t *growthTool) Parameters() map[string]any { return t.def.Parameters }

func (t *growthTool) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// admitted is the calling seat, or the refusal of a call no seat whose level
// allows the tool's class makes for its company.
func (t *growthTool) admitted(ctx context.Context, turn *turnctx.Turn) (string, *tools.Result) {
	handle, refusal := seatTurn(t.Name(), turn)
	if refusal != nil {
		return "", refusal
	}
	if refusal = companyRequester(ctx, turn, t.deps.Requesters, t.deps.Chart, t.Name(), growthTerms); refusal != nil {
		return "", refusal
	}
	level := t.product.Level(handle)
	if !level.Allows(t.def.Class) {
		at := "off"
		if level != "" {
			at = "at " + string(level)
		}
		r := refused(tools.RefusalForbidden, fmt.Sprintf("%s didn't run: this agent's %s access is %s, and %s "+
			"needs it at %s. A person raises it on the integration's Agents tab. No vendor was reached.",
			t.Name(), t.product.Catalog().API, at, t.Name(), t.def.Class.Level()))
		return "", &r
	}
	return handle, nil
}

func (t *growthTool) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	handle, refusal := t.admitted(ctx, turn)
	if refusal != nil {
		return *refusal, nil
	}
	answer, err := t.product.Run(ctx, growth.Call{Handle: handle, Turn: turn.RunID, Tool: t.Name(), Args: args})
	if err != nil {
		return growthFailure(ctx, t.Name(), err), nil
	}
	return growthAnswer(ctx, turn, handle, t.Name(), answer)
}

// growthFailure is the answer of a call that did not reach its result.
func growthFailure(ctx context.Context, tool string, err error) tools.Result {
	var r *growth.Refusal
	if errors.As(err, &r) {
		class, ok := growthRefusals[r.Kind]
		if !ok {
			class = tools.RefusalInvalid
		}
		return refusedBy(class, err, r.Said)
	}
	return faulted(ctx, tool, err, tool+" didn't finish: the vendor's answer could not be read, so what it did "+
		"is unknown.")
}

// growthAnswer renders a product's answer, every field it declares
// somebody's words fenced or named as a vendor's record.
func growthAnswer(ctx context.Context, turn *turnctx.Turn, handle, tool string, answer any) (tools.Result, error) {
	read := newForeignRead(turn, handle)
	declared, err := declaredIn(answer)
	if err != nil {
		return labelFault(ctx, tool, err), nil
	}
	label := provenance.VendorAnswer()
	if len(declared) > 0 {
		read.adopt(label)
	}
	for _, f := range declared {
		if f.kind == provenance.FieldFence && f.value.Kind() == reflect.String {
			read.fenceLabelled(f.path, label, f.value.String())
		}
	}
	return read.answer(ctx, tool, answer, "Ask for less: narrow the range, add a filter or lower the limit.")
}

// growthHeldTool is a public, spend or admin tool: held for an administrator.
type growthHeldTool struct{ growthTool }

var (
	_ tools.SeatCallable = (*growthHeldTool)(nil)
	_ fixedTool          = (*growthHeldTool)(nil)
	_ preparedTool       = (*growthHeldTool)(nil)
)

// Digest is the tool's definition's digest, which a resume holds the call to.
func (t *growthHeldTool) Digest() string {
	data, err := json.Marshal(map[string]any{"name": t.def.Name, "class": t.def.Class,
		"description": t.def.Description, "parameters": t.def.Parameters})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Approvers is an administrator alone, at a fresh step-up.
func (t *growthHeldTool) Approvers() string { return sandbox.HeldApproversAdmin }

// Act is the decision the tool's class is asked as.
func (t *growthHeldTool) Act() delegated.Kind { return t.def.Class.Act() }

// BeforeHold refuses a call no seat whose level allows it makes for its
// company; the product's own refusals come with [growthHeldTool.PrepareHold].
func (t *growthHeldTool) BeforeHold(ctx context.Context, turn *turnctx.Turn, _ map[string]any) *tools.Result {
	_, refusal := t.admitted(ctx, turn)
	return refusal
}

// PrepareHold reads the call's target live and builds its hold.
func (t *growthHeldTool) PrepareHold(ctx context.Context, turn *turnctx.Turn, args map[string]any) (HeldPrep, *tools.Result) {
	hold, err := t.product.Prepare(ctx, growth.Call{Handle: turn.Handle(), Turn: turn.RunID, Tool: t.Name(), Args: args})
	if err != nil {
		r := growthFailure(ctx, t.Name(), err)
		return HeldPrep{}, &r
	}
	if len(hold.Binding) != sha256.Size*2 || (hold.Subject != "" && len(hold.Subject) != sha256.Size*2) {
		r := faulted(ctx, t.Name(), fmt.Errorf("builtin: %s prepared a hold with no digest binding it",
			t.Name()), t.Name()+" was not called, and nothing was asked: the engine could not bind the "+
			"question to what it would send")
		return HeldPrep{}, &r
	}
	return HeldPrep(hold), nil
}

// Binding, Subject and HeldWords are read in [growthHeldTool.PrepareHold].
func (t *growthHeldTool) Binding(*turnctx.Turn, map[string]any) string { return "" }
func (t *growthHeldTool) Subject(*turnctx.Turn, map[string]any) string { return "" }
func (t *growthHeldTool) HeldWords(*turnctx.Turn, map[string]any) (string, string) {
	return "", ""
}

// CallForTurn makes an approved call: never one nobody approved, and never
// one whose target moved since.
func (t *growthHeldTool) CallForTurn(ctx context.Context, turn *turnctx.Turn, args map[string]any) (tools.Result, error) {
	handle, refusal := seatTurn(t.Name(), turn)
	if refusal != nil {
		return *refusal, nil
	}
	approval, approved := tools.ApprovalOf(ctx)
	if !approved || approval.HeldID == "" || approval.Binding == "" {
		return refused(tools.RefusalForbidden, t.Name()+" didn't run: it runs only once an administrator "+
			"approves the call, and this call carries no approval. No vendor was reached."), nil
	}
	// THE LEVEL IS DECIDED AGAIN AT THE RUN: a seat lowered while the call
	// waited asks for nothing.
	if !t.product.Level(handle).Allows(t.def.Class) {
		return refused(tools.RefusalForbidden, t.Name()+" didn't run: this agent's access was lowered since "+
			"the call was approved. Nothing was sent."), nil
	}
	call := growth.Call{Handle: handle, Turn: turn.RunID, Tool: t.Name(), Args: args, HeldCall: approval.HeldID,
		Approver: approval.By}
	answer, err := t.product.Apply(ctx, call, approval.Binding)
	switch {
	case errors.Is(err, growth.ErrMoved):
		return refused(tools.RefusalConflict, t.Name()+" didn't run: what it changes moved since the call was "+
			"approved, so it is not what the administrator allowed. Nothing was sent. Read it again, and call "+
			t.Name()+" again to ask about it as it is now."), nil
	case err != nil:
		return growthFailure(ctx, t.Name(), err), nil
	}
	return growthAnswer(ctx, turn, handle, t.Name(), answer)
}

// RecordDenied records that an administrator denied the call, or that it
// lapsed unanswered.
func (t *growthHeldTool) RecordDenied(ctx context.Context, turn *turnctx.Turn, args map[string]any) error {
	approval, ok := tools.ApprovalOf(ctx)
	if !ok || approval.HeldID == "" {
		return errors.New("builtin: a denied growth call names no approver or held call")
	}
	return t.product.Denied(ctx, growth.Call{Handle: turn.Handle(), Turn: turn.RunID, Tool: t.Name(), Args: args,
		HeldCall: approval.HeldID, Approver: approval.By})
}
