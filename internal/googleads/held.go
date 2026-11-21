package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/spendguard"
)

// prepared is a write as the engine will make it: its request held to the
// seat's scope, the live state its rules read (as the held binding's
// before-state) and the class it comes to.
type prepared struct {
	held  googlegrowth.Held
	ruled ruled
}

// prepare builds call's request, holds it to the seat's scope, reads what its
// rules compare and classes it. Its request is kept on a later failure, for
// the record of it.
func (p *Product) prepare(ctx context.Context, call growth.Call) (prepared, error) {
	req, err := p.requestOf(call)
	if err != nil {
		return prepared{}, err
	}
	ops, _ := parse(req.Op, req.Body)
	if err = p.inScope(call.Handle, req, ops); err != nil {
		return prepared{held: googlegrowth.Held{Req: req}}, err
	}
	names := append(toRead(ops), recommendationsToRead(req.Body)...)
	masks := map[string][]string{}
	for _, o := range ops {
		if o.kind == opUpdate {
			masks[o.name] = o.mask
		}
	}
	l := p.readLive(ctx, call.Handle, names, masks)
	r, err := classify(req.Op, req.Body, l, func() (growth.Class, bool) {
		if p.staged == nil {
			return "", false
		}
		c, ok, readErr := p.staged.StagedClass(ctx, string(Kind), req.Account, req.Params["batchJobsId"])
		return c, ok && readErr == nil
	})
	if err != nil {
		return prepared{held: googlegrowth.Held{Req: req}}, err
	}
	// THE LIVE STATE IS THE BINDING'S BEFORE-STATE: a target that moves under
	// an approval moves the digest, and nothing is sent.
	before, _ := json.Marshal(l)
	return prepared{held: googlegrowth.NewHeld(req, before), ruled: r}, nil
}

// Prepare classes a write against its targets as read now, holds a spend to
// the account's caps before a person is asked, and builds what is asked.
func (p *Product) Prepare(ctx context.Context, call growth.Call) (growth.Hold, error) {
	pr, err := p.prepare(ctx, call)
	if err != nil {
		return growth.Hold{}, err
	}
	hold := p.client.Hold(call, pr.held, pr.ruled.class)
	if pr.ruled.class == growth.ClassSpend {
		f, err := p.checkSpend(ctx, call.Handle, pr)
		if err != nil {
			return growth.Hold{}, err
		}
		hold.Ask.Money = money(f)
	}
	return hold, nil
}

// Card is a held call's decision card as its targets read now, with a
// spend's figures against the caps.
func (p *Product) Card(ctx context.Context, call growth.Call, binding string) (growth.Card, error) {
	pr, err := p.prepare(ctx, call)
	if err != nil {
		return growth.Card{}, err
	}
	card := p.client.Card(call, pr.held, binding, pr.ruled.class)
	for i := range card.Acts {
		card.Acts[i].Danger = card.Acts[i].Danger || pr.ruled.danger
	}
	if pr.ruled.class == growth.ClassSpend {
		f, err := p.checkSpend(ctx, call.Handle, pr)
		if err != nil {
			return growth.Card{}, err
		}
		card.Money = money(f)
	}
	return card, nil
}

// Apply makes a prepared write once, reading its targets again first and
// sending nothing where they moved since binding was taken. A spend is held to
// the caps again under the account's lease, and made inside it.
func (p *Product) Apply(ctx context.Context, call growth.Call, binding string) (any, error) {
	pr, err := p.prepare(ctx, call)
	class := pr.ruled.class
	if class == "" {
		tool, _ := p.Catalog().Tool(call.Tool)
		class = tool.Class
	}
	if err != nil || class != growth.ClassSpend {
		return p.client.Apply(ctx, call, pr.held, err, binding, class, nil)
	}
	customer := customerOf(pr.held.Req)
	var out any
	_, guardErr := p.guard(call.Handle).Apply(ctx, customer, pr.ruled.ops,
		func(ctx context.Context) (spendguard.Account, spendguard.Caps, error) {
			return p.spendAccount(ctx, call.Handle, customer)
		},
		func(ctx context.Context) error {
			var applyErr error
			out, applyErr = p.client.Apply(ctx, call, pr.held, nil, binding, class, nil)
			return applyErr
		})
	if refused, ok := errors.AsType[*spendguard.Refusal](guardErr); ok {
		r := p.client.Record(call, pr.held.Req, class, pr.held.Before)
		r.Failure = growth.FailureCapExceeded
		p.client.Fail(ctx, r)
		return nil, refusal(growth.RefusedForbidden, "Google Ads was not changed: "+refused.Error()+".")
	}
	return out, guardErr
}

// guard is the spend guard reading capacity as handle's identity.
func (p *Product) guard(handle string) *spendguard.Guard[[]operation] {
	return spendguard.New(string(Kind), capacity{now: p.now, search: func(ctx context.Context, customer, gaql string) (
		[]map[string]any, error) {
		return p.search(ctx, handle, customer, gaql)
	}}, p.leases)
}

// checkSpend holds a prepared spend to its account's caps now.
func (p *Product) checkSpend(ctx context.Context, handle string, pr prepared) (spendguard.Figures, error) {
	customer := customerOf(pr.held.Req)
	acct, caps, err := p.spendAccount(ctx, handle, customer)
	if err != nil {
		return spendguard.Figures{}, err
	}
	f, err := p.guard(handle).Check(ctx, acct, caps, pr.ruled.ops)
	if refused, ok := errors.AsType[*spendguard.Refusal](err); ok {
		return f, refusal(growth.RefusedForbidden, "nobody was asked: "+refused.Error()+".")
	}
	return f, err
}

// spendAccount is customer as Google answers it now and its caps as the
// running document sets them.
func (p *Product) spendAccount(ctx context.Context, handle, customer string) (spendguard.Account, spendguard.Caps,
	error) {
	rows, err := p.search(ctx, handle, customer, "SELECT customer.currency_code, customer.status FROM customer")
	if err != nil {
		return spendguard.Account{}, spendguard.Caps{}, err
	}
	if len(rows) == 0 {
		return spendguard.Account{}, spendguard.Caps{}, fmt.Errorf("googleads: account %s answered no customer", customer)
	}
	m, _ := snaked(rows[0]["customer"]).(map[string]any)
	status := str(m["status"])
	caps, _ := p.company().Integrations.GrowthCaps(string(Kind), customer)
	return spendguard.Account{ID: customer, Currency: str(m["currency_code"]), Serving: status == "ENABLED",
		State: status}, caps, nil
}

// money is a spend's figures as a card and the access log carry them.
func money(f spendguard.Figures) *growth.CardMoney {
	return &growth.CardMoney{Currency: f.Currency, DeltaMicros: f.Delta, DailyMicros: f.Daily,
		DailyAfterMicros: f.DailyAfter, PerChangeCapMicros: f.PerChangeCap, DailyCapMicros: f.DailyCap,
		Overdelivery: "Google Ads may spend up to twice a campaign's daily budget on one day, and no more than " +
			"30.4 times it over a month."}
}
