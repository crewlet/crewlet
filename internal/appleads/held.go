package appleads

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/spendguard"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/words"
)

// Every write is prepared, then applied. Prepare reads each target, classes
// the write by what it changes there, binds the exact requests and the
// targets' states, and for spend checks the account's caps; a held act also
// builds what an administrator reads (ADR-0171). Apply, under the ad
// account's lease, reads the targets again and sends nothing if any moved,
// and makes a spend act inside the spend guard's apply, which checks the caps
// against live capacity once more.

func hexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// operationDigest is the SHA-256 of m's exact requests.
func operationDigest(m made) string {
	reqs := make([]any, 0, len(m.targets))
	for _, t := range m.targets {
		r := map[string]any{"method": t.op.Method, "path": t.path, "query": t.query.Encode(), "body": t.body}
		if t.upload != nil {
			r["upload"] = map[string]any{"name": t.upload.Name, "sha256": hexSum(t.upload.Data), "fields": t.upload.Fields}
		}
		reqs = append(reqs, r)
	}
	data, _ := json.Marshal(map[string]any{"account": m.account, "connection": m.conn.id, "requests": reqs})
	return hexSum(data)
}

// prepared is a write read and classed: its targets as they read now, and the
// class each request comes to.
type prepared struct {
	m       made
	befores []before
	classes []growth.Class
	class   growth.Class
	money   *growth.CardMoney
}

// prepare reads m's targets and classes every request; the write's class is
// the highest.
func (p *Product) prepare(ctx context.Context, m made) (prepared, error) {
	pr := prepared{m: m, class: growth.ClassRead}
	for _, t := range m.targets {
		if err := p.checkRefs(ctx, m, t); err != nil {
			return prepared{}, said(err)
		}
		b, err := p.beforeOf(ctx, m, t)
		if err != nil {
			return prepared{}, said(err)
		}
		if body, ok := t.body.(map[string]any); ok && budgetOrder(t.op) {
			if err := m.owns(body); err != nil {
				return prepared{}, said(err)
			}
		}
		c := p.classOf(ctx, m, t, b)
		pr.befores, pr.classes = append(pr.befores, b), append(pr.classes, c)
		if c.Rank() > pr.class.Rank() {
			pr.class = c
		}
	}
	return pr, nil
}

// binding is what an approval binds: the requests, every target's state and
// the class the call was asked as.
func (pr prepared) binding() string {
	digests := make([]string, 0, len(pr.befores))
	for _, b := range pr.befores {
		digests = append(digests, b.digest)
	}
	return hexSum([]byte(operationDigest(pr.m) + "|" + strings.Join(digests, ",") + "|" + string(pr.class)))
}

// fill completes what a curated tool's body leaves to the account: the
// currency of every amount it sets.
func (p *Product) fill(ctx context.Context, m made) error {
	if m.tool != ToolSetBudget && m.tool != ToolSetBids {
		return nil
	}
	info, err := p.accountOf(ctx, m.conn, m.account)
	if err != nil {
		return said(err)
	}
	for _, t := range m.targets {
		fillCurrency(t.body, info.Currency)
	}
	return nil
}

// ready is a write made concrete, its amounts' currency filled, read and
// classed, and for spend checked against the caps.
func (p *Product) ready(ctx context.Context, call growth.Call) (prepared, error) {
	m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
	if err != nil {
		return prepared{}, err
	}
	pr, err := p.prepare(ctx, m)
	if err != nil {
		return prepared{}, err
	}
	if pr.class == growth.ClassSpend {
		if pr.money, err = p.checkSpend(ctx, spendAct{m: m, befores: pr.befores}); err != nil {
			return prepared{}, err
		}
	}
	return pr, nil
}

// cardChanges are what each request changes, labelled; an amount both before
// and after is money, written as micros of the account's currency.
func (pr prepared) cardChanges(i int) []growth.CardChange {
	var out []growth.CardChange
	for _, ch := range changesOf(pr.m.targets[i], pr.befores[i]) {
		c := growth.CardChange{Field: ch.Field, Label: labelOf(ch.Field), Before: valueText(ch.Before),
			After: valueText(ch.After)}
		if strings.HasSuffix(ch.Field, ".amount") {
			before, errB := growth.ParseAmount(c.Before)
			after, errA := growth.ParseAmount(c.After)
			if errA == nil && (errB == nil || c.Before == "") {
				c.Money, c.After = true, strconv.FormatInt(after, 10)
				if c.Before != "" {
					c.Before = strconv.FormatInt(before, 10)
				}
			}
		}
		out = append(out, c)
	}
	return out
}

// shown is a change's value as a person reads it: money as a decimal amount.
func shown(c growth.CardChange, v string) string {
	if !c.Money || v == "" {
		return v
	}
	micros, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return v
	}
	s, _ := growth.FormatAmount(micros, 2)
	return s
}

// labels are the fields a person reads most, in Apple Ads' own words.
var labels = map[string]string{fieldDailyBudget: "Daily budget", "status": "Status", "name": "Name",
	"bid.amount": "Bid", "bidStrategy.bid.amount": "Default bid", "cpaCap.value.amount": "Target CPA",
	"value.amount": "Budget order amount"}

func labelOf(field string) string {
	if l, ok := labels[field]; ok {
		return l
	}
	return field
}

func valueText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	}
	data, _ := json.Marshal(v)
	return string(data)
}

// reasonOf is an act's reason, redacted and bounded.
func reasonOf(args map[string]any) string {
	r, _ := args["reason"].(string)
	return textcut.Within(redact.Secrets(strings.TrimSpace(r)), 500)
}

// Prepare reads a write's targets, classes it and binds it; for spend it
// checks the caps, and for a held call builds what an administrator reads.
func (p *Product) Prepare(ctx context.Context, call growth.Call) (growth.Hold, error) {
	pr, err := p.ready(ctx, call)
	if err != nil {
		return growth.Hold{}, err
	}
	if !pr.class.Held() {
		return growth.Hold{Class: pr.class, Binding: pr.binding()}, nil
	}
	if reasonOf(call.Args) == "" {
		return growth.Hold{}, invalid("`reason` is one line saying why, which the administrator reads.")
	}
	m := pr.m
	ops := make([]string, 0, len(m.targets))
	for _, t := range m.targets {
		if !slices.Contains(ops, t.op.ID) {
			ops = append(ops, t.op.ID)
		}
	}
	question := fmt.Sprintf("May %s %s on %s?", call.Handle, verbOf(pr), m.where())
	var ctxt strings.Builder
	sends := "this request"
	if len(m.targets) > 1 {
		sends = "these " + words.Count(len(m.targets), "request", "requests")
	}
	fmt.Fprintf(&ctxt, "%s asks to make **%s** on %s, as connection %s. Allow once sends exactly %s, once.",
		call.Handle, strings.Join(ops, ", "), m.where(), m.conn.id, sends)
	if slices.ContainsFunc(m.targets, func(t target) bool { return t.op.Danger }) {
		ctxt.WriteString(" **Apple cannot undo it.**")
	}
	for i, t := range m.targets {
		if pr.classes[i] == growth.ClassRead {
			continue
		}
		fmt.Fprintf(&ctxt, "\n\n**%s %s**", t.op.Method, words.Inert(t.path))
		for _, c := range pr.cardChanges(i) {
			fmt.Fprintf(&ctxt, "\n- %s: %s to %s", words.Inert(c.Label), words.Inert(textcut.Within(shown(c, c.Before), 200)),
				words.Inert(textcut.Within(shown(c, c.After), 200)))
		}
	}
	if pr.money != nil {
		f := func(micros int64) string {
			s, _ := growth.FormatAmount(micros, 2)
			return s + " " + pr.money.Currency
		}
		fmt.Fprintf(&ctxt, "\n\n**Spend**\n\nDaily capacity %s, after %s (adds %s). Caps: %s per change, %s a day.",
			f(pr.money.DailyMicros), f(pr.money.DailyAfterMicros), f(pr.money.DeltaMicros),
			f(pr.money.PerChangeCapMicros), f(pr.money.DailyCapMicros))
	}
	ctxt.WriteString("\n\nThe call's page shows the whole request.\n\n**Reason given**\n\n" + words.Inert(reasonOf(call.Args)))
	ask := recordOf(call, m, pr.class)
	ask.Before, ask.Changes, ask.Money = beforesDigest(pr), pr.recordChanges(), pr.money
	return growth.Hold{Class: pr.class, Binding: pr.binding(), Question: question,
		Context: textcut.Within(ctxt.String(), sandbox.MaxAskContextBytes-1024),
		Subject: delegated.Subject(pr.class.Act(), strings.Join(ops, ","), m.account, operationDigest(m)), Ask: ask}, nil
}

// verbOf is what a held act does, in a few words.
func verbOf(pr prepared) string {
	switch pr.m.targets[0].op.Method {
	case "POST":
		return "create or apply " + pr.m.targets[0].op.Resource
	case "DELETE":
		return "delete " + pr.m.targets[0].op.Resource
	}
	return "change " + words.Count(len(pr.m.targets), pr.m.targets[0].op.Resource, pr.m.targets[0].op.Resource)
}

// beforesDigest is one digest of every target's state, for the access log.
func beforesDigest(pr prepared) string {
	digests := make([]string, 0, len(pr.befores))
	for _, b := range pr.befores {
		digests = append(digests, b.digest)
	}
	return hexSum([]byte(strings.Join(digests, ",")))
}

// recordChanges are every request's changes as the access log accepts them.
func (pr prepared) recordChanges() []growth.RecordChange {
	var all []growth.CardChange
	for i := range pr.m.targets {
		all = append(all, pr.cardChanges(i)...)
	}
	return recordChanges(all)
}

// Card is a held call's decision card as its targets read now.
func (p *Product) Card(ctx context.Context, call growth.Call, approved string) (growth.Card, error) {
	pr, err := p.ready(ctx, call)
	var refusal *growth.Refusal
	switch {
	case errors.As(err, &refusal):
		// A CARD STILL SHOWS AN ACT THE GUARD OR THE SCOPE NOW REFUSES, as
		// moved: Allow once would send nothing, and the agent is told to ask again.
		return growth.Card{Act: toolClass[call.Tool].Act(), Product: Kind, Tool: call.Tool,
			Reason: reasonOf(call.Args), Moved: true, Findings: []string{refusal.Said}}, nil
	case err != nil:
		return growth.Card{}, err
	}
	m := pr.m
	card := growth.Card{Act: pr.class.Act(), Product: Kind, Tool: call.Tool, Money: pr.money,
		Reason: reasonOf(call.Args), Moved: !growth.BindingHolds(approved, pr.binding())}
	card.Account = growth.Account{Kind: Kind, Connection: m.conn.id, ID: m.account, State: growth.AccountUnknown}
	if m.account != "" {
		if info, err := p.accountOf(ctx, m.conn, m.account); err == nil {
			card.Account.Name, card.Account.Currency, card.Account.TimeZone = info.Name, info.Currency, info.TimeZone
			card.Account.State, card.Account.Roles = info.state(), m.roles
		}
	}
	for i, t := range m.targets {
		if pr.classes[i] == growth.ClassRead {
			continue
		}
		card.Acts = append(card.Acts, growth.CardAct{Target: t.op.Resource + " " + t.path, Summary: t.op.Method + " " +
			t.op.ID, Changes: pr.cardChanges(i), Danger: t.op.Danger})
	}
	return card, nil
}

// leaseOf is the lease a write on m holds: one write per ad account, or per
// organization.
func leaseOf(m made) string {
	if m.account != "" {
		return "write/" + Kind + "/" + m.account
	}
	return "write/" + Kind + "/org/" + m.conn.id
}

// Applied is an applied write's answer: each request Apple answered, fenced,
// since Apple echoes names and keywords other people wrote.
type Applied struct {
	Connection string   `json:"connection"`
	Account    string   `json:"account,omitempty"`
	Results    []Result `json:"results"`
	// Unchanged counts the requests not sent, whose targets already held
	// what they set.
	Unchanged int `json:"unchanged"`
}

// Result is one request's answer.
type Result struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Status    int    `json:"status"`
	Document  string `json:"document" untrusted:"fence"`
}

// Apply makes a prepared write once: an operate call at once, a held call
// once approved, which names its held call. An allowed act's answer is
// recorded before it runs, and every way it ends after that is its outcome.
func (p *Product) Apply(ctx context.Context, call growth.Call, approved string) (any, error) {
	held := call.HeldCall != ""
	m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
	rec := p.heldRecord(call, m, err == nil)
	if held {
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
	pr, err := p.prepare(act, m)
	if err != nil {
		return nil, p.failEarly(ctx, held, rec, err)
	}
	rec.Class, rec.Before = pr.class, beforesDigest(pr)
	if !growth.BindingHolds(approved, pr.binding()) {
		if held {
			p.note(ctx, growth.StepVoid, rec)
		}
		return nil, growth.ErrMoved
	}
	if pr.class.Held() != held {
		// UNREACHABLE THROUGH THE TOOLS, whose binding holds the class asked:
		// a held class is made only once allowed, and an allowed call as asked.
		return nil, p.failEarly(ctx, held, rec, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("as its "+
			"targets read now this is %s, which is made only once an administrator allows it. No call was made.",
			pr.class)))
	}
	rec.Changes = pr.recordChanges()
	var answer Applied
	send := func(ctx context.Context) error {
		a, sendErr := p.send(ctx, pr, held)
		answer = a
		return sendErr
	}
	if pr.class == growth.ClassSpend {
		err = p.applySpend(act, spendAct{m: m, befores: pr.befores}, send, &rec)
	} else {
		err = send(act)
	}
	if err != nil {
		rec.Failure = writeFailure(err)
		p.note(ctx, growth.StepFailed, rec)
		return nil, said(refusedBy(err))
	}
	if len(answer.Results) > 0 || held {
		step := growth.StepMade
		if held {
			step = growth.StepApplied
		}
		p.note(ctx, step, rec)
	}
	return answer, nil
}

// failEarly records the failed outcome of an allowed held call that ended
// before its request was sent, and answers err as the agent is told it.
func (p *Product) failEarly(ctx context.Context, held bool, rec growth.Record, err error) error {
	if held {
		rec.Failure = failureOf(err)
		p.note(ctx, growth.StepFailed, rec)
	}
	return said(err)
}

// applySpend makes send inside the spend guard's apply, for the act's ad
// account; a budget order, which adds no daily capacity, is checked again
// for each ad account it names.
func (p *Product) applySpend(ctx context.Context, act spendAct, send func(context.Context) error,
	rec *growth.Record) error {
	if act.m.account == "" {
		money, err := p.checkSpend(ctx, act)
		if err != nil {
			return err
		}
		rec.Money = money
		return send(ctx)
	}
	f, err := p.guard().Apply(ctx, act.m.account, act, func(ctx context.Context) (spendguard.Account, spendguard.Caps, error) {
		return p.guardRead(ctx, act.m.account)
	}, send)
	if f.Currency != "" {
		rec.Money = cardMoney(f)
	}
	return err
}

// send makes each request that changes its target, in order, stopping at the
// first Apple refuses: what was sent before it stands, and the answer and the
// log say which.
func (p *Product) send(ctx context.Context, pr prepared, held bool) (Applied, error) {
	m := pr.m
	answer := Applied{Connection: m.conn.id, Account: m.account}
	for i, t := range m.targets {
		if pr.classes[i] == growth.ClassRead {
			answer.Unchanged++
			continue
		}
		resp, err := m.conn.client.Do(ctx, Request{Method: t.op.Method, Path: t.path, Query: t.query, Body: t.body,
			Upload: t.upload, Account: m.account, Reserved: held})
		if err != nil {
			if len(answer.Results) > 0 {
				return answer, fmt.Errorf("%w: %d of %d requests were made before %s %s failed: %w", errSent,
					len(answer.Results), len(m.targets), t.op.Method, t.path, err)
			}
			return answer, err
		}
		answer.Results = append(answer.Results, Result{Operation: t.op.ID, Path: t.path, Status: resp.Status,
			Document: string(resp.Body)})
	}
	return answer, nil
}

// errSent is a write that failed after some of its requests were made.
var errSent = errors.New("appleads: part of the write was made")

// hold takes resource's lease, or answers why it could not.
func (p *Product) hold(ctx context.Context, resource string) (func(), error) {
	if p.deps.Leases == nil {
		return nil, growth.Refuse(growth.RefusedUnavailable, "this node cannot hold an act's lease, so nothing was sent.")
	}
	release, err := p.deps.Leases.Hold(ctx, resource)
	switch {
	case errors.Is(err, growth.ErrActHeld):
		return nil, growth.Refuse(growth.RefusedUnavailable, "another act on the same ad account is being made; nothing "+
			"was sent. Call again.")
	case err != nil:
		return nil, fmt.Errorf("appleads: hold the ad account's act lease, so nothing was sent: %w", err)
	}
	return release, nil
}

// Denied records that an administrator denied the call, or that it lapsed.
func (p *Product) Denied(ctx context.Context, call growth.Call) error {
	step, answerer := growth.StepDenied, call.Approver
	if call.Approver == "" || call.Approver == engineActor {
		step, answerer = growth.StepLapsed, ""
	}
	m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
	rec := p.heldRecord(call, m, err == nil)
	rec.Answerer = answerer
	p.note(ctx, step, rec)
	return nil
}

// engineActor is who the engine's own answer names, as its lapse sweep
// answers (internal/engine).
const engineActor = "engine"
