package googleads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/spendguard"
)

// targets reads every resource req's rules compare against, as handle's
// identity (googlegrowth's Client.BeforeRead): the state an approval binds
// and the rules read, so a call is classed and bound on one reading. A batch
// job's run holds the job as Google counts its operations and the class its
// staging came to, so what was staged after an approval moves its binding.
func (p *Product) targets(ctx context.Context, handle string, req googlegrowth.Request) ([]byte, bool, error) {
	ops, _ := parse(req.Op, req.Body)
	names := append(toRead(ops), recommendationsToRead(req.Body)...)
	job := runOf(req)
	if job != "" {
		names = append(names, job)
	}
	masks := map[string][]string{}
	for _, o := range ops {
		if o.kind == opUpdate {
			masks[o.name] = o.mask
		}
	}
	l := p.readLive(ctx, handle, names, masks)
	if job != "" && p.staged != nil {
		c, ok, err := p.staged.StagedClass(ctx, string(Kind), req.Account, req.Params["batchJobsId"])
		if err != nil {
			return nil, false, fmt.Errorf("googleads: read what batch job %s staged: %w", job, err)
		}
		if ok {
			s := l[job]
			if s == nil {
				s = state{}
			}
			s[stagedClass] = string(c)
			l[job] = s
		}
	}
	before, err := json.Marshal(l)
	return before, err == nil, err
}

// stagedClass is the field of a batch job's state holding the class its
// staging came to, from the access log: no GAQL field holds it.
const stagedClass = "crewlet_staged_class"

// runOf is the batch job req runs, "" for any other request.
func runOf(req googlegrowth.Request) string {
	if ruleOf[req.Op.ID] != "FR-BATCH" || !strings.HasSuffix(req.Op.ID, ".run") {
		return ""
	}
	return req.Account + "/batchJobs/" + req.Params["batchJobsId"]
}

// ruledOf is what h comes to under its rules, read on the state its binding
// holds.
func ruledOf(h googlegrowth.Held) (ruled, error) {
	var l live
	_ = json.Unmarshal(h.Before, &l)
	return classify(h.Req.Op, h.Req.Body, l, func() (growth.Class, bool) {
		c := growth.Class(str(l[runOf(h.Req)][stagedClass]))
		return c, c != ""
	})
}

// prepared is call's request held to the seat's scope and to its targets as
// read now, once, and what its rules make of that reading. Its request is
// kept on a later failure, for the record of it.
func (p *Product) prepared(ctx context.Context, call growth.Call) (googlegrowth.Held, ruled, error) {
	req, err := p.requestOf(call)
	if err != nil {
		return googlegrowth.Held{}, ruled{}, err
	}
	if err = p.inScope(call.Handle, req); err != nil {
		return googlegrowth.Held{Req: req}, ruled{}, err
	}
	h, err := p.client.Read(ctx, call.Handle, req)
	if err != nil {
		return h, ruled{}, err
	}
	r, err := ruledOf(h)
	return h, r, err
}

// Prepare classes a write against its targets as read now, holds a spend to
// the account's caps before a person is asked, and builds what is asked.
func (p *Product) Prepare(ctx context.Context, call growth.Call) (growth.Hold, error) {
	h, r, err := p.prepared(ctx, call)
	if err != nil {
		return growth.Hold{}, err
	}
	hold := p.client.Hold(call, h, r.class)
	if r.class == growth.ClassSpend {
		f, err := p.checkSpend(ctx, call.Handle, h.Req, r)
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
	h, r, err := p.prepared(ctx, call)
	if err != nil {
		return growth.Card{}, err
	}
	card := p.client.Card(call, h, binding, r.class)
	for i := range card.Acts {
		card.Acts[i].Danger = card.Acts[i].Danger || r.danger
	}
	if r.class == growth.ClassSpend {
		f, err := p.checkSpend(ctx, call.Handle, h.Req, r)
		if err != nil {
			return growth.Card{}, err
		}
		card.Money = money(f)
	}
	return card, nil
}

// Apply makes a prepared write once, building it again and, under the
// account's one write lease (googlegrowth's Client.Apply), holding it to the
// seat's scope, reading its targets again, sending nothing where they moved
// since binding was taken, and holding a spend to the caps as they read then.
func (p *Product) Apply(ctx context.Context, call growth.Call, binding string) (any, error) {
	req, err := p.requestOf(call)
	return p.client.Apply(ctx, call, googlegrowth.Write{Req: req, Err: err, Class: p.toolClass(call.Tool),
		Scope: func(context.Context) error { return p.inScope(call.Handle, req) },
		Check: func(ctx context.Context, h googlegrowth.Held) error { return p.recheckSpend(ctx, call.Handle, h) },
	}, binding)
}

// recheckSpend holds an allowed spend to its account's caps again, inside the
// lease the client holds on the account (spendguard's Recheck), so no act
// holds two leases; a refusal fails the act over its caps, and nothing is sent.
func (p *Product) recheckSpend(ctx context.Context, handle string, h googlegrowth.Held) error {
	r, err := ruledOf(h)
	if err != nil || r.class != growth.ClassSpend {
		return err
	}
	customer := customerOf(h.Req)
	_, err = p.guard(handle).Recheck(ctx, r.ops, func(ctx context.Context) (spendguard.Account, spendguard.Caps,
		error) {
		return p.spendAccount(ctx, handle, customer)
	})
	if refused, ok := errors.AsType[*spendguard.Refusal](err); ok {
		return &googlegrowth.CallError{Failure: growth.FailureCapExceeded, Err: refusal(growth.RefusedForbidden,
			"Google Ads was not changed: "+refused.Error()+".")}
	}
	return err
}

// guard is the spend guard reading capacity as handle's identity. It holds no
// lease of its own: an apply rechecks inside the account's write lease.
func (p *Product) guard(handle string) *spendguard.Guard[[]operation] {
	return spendguard.New(string(Kind), capacity{now: p.now, search: func(ctx context.Context, customer, gaql string) (
		[]map[string]any, error) {
		return p.search(ctx, handle, customer, gaql)
	}}, nil)
}

// checkSpend holds a prepared spend to its account's caps now.
func (p *Product) checkSpend(ctx context.Context, handle string, req googlegrowth.Request, r ruled) (
	spendguard.Figures, error) {
	customer := customerOf(req)
	acct, caps, err := p.spendAccount(ctx, handle, customer)
	if err != nil {
		return spendguard.Figures{}, err
	}
	f, err := p.guard(handle).Check(ctx, acct, caps, r.ops)
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
