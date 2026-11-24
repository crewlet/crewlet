package metaads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

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
// and makes a spend act inside the spend guard's apply.

// operationDigest is the SHA-256 of m's exact requests.
func operationDigest(m made) string {
	reqs := make([]any, 0, len(m.targets))
	for _, t := range m.targets {
		reqs = append(reqs, map[string]any{"method": t.row.op.Method, "path": t.path, "params": t.params,
			"chain": t.chain})
	}
	data, _ := json.Marshal(map[string]any{"account": m.account, "requests": reqs})
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

// budgetLimit is how many budget changes Meta takes on one ad set in an
// hour, after which it blocks its budget for the hour (613/1487632).
const budgetLimit = 4

// budgetFields are the fields Meta's hourly budget limit counts.
var budgetFields = []string{"daily_budget", "lifetime_budget"}

// prepare reads m's targets and classes every request; the write's class is
// the highest.
func (p *Product) prepare(ctx context.Context, m made) (prepared, error) {
	pr := prepared{m: m, class: growth.ClassRead}
	for _, t := range m.targets {
		b, err := p.beforeOf(ctx, m, t)
		if err != nil {
			return prepared{}, said(err)
		}
		c := p.classOf(ctx, m, t, b)
		if !c.Callable() {
			return prepared{}, growth.Refuse(growth.RefusedForbidden, fmt.Sprintf("%s is not offered. No call was made.",
				t.row.op.ID))
		}
		if err := p.withinBudgetLimit(ctx, m, t); err != nil {
			return prepared{}, err
		}
		pr.befores, pr.classes = append(pr.befores, b), append(pr.classes, c)
		if c.Rank() > pr.class.Rank() {
			pr.class = c
		}
	}
	return pr, nil
}

// withinBudgetLimit refuses an ad set's budget change Meta would block: the
// fifth in an hour, counted from the access log before a person is asked.
func (p *Product) withinBudgetLimit(ctx context.Context, m made, t target) error {
	if t.row.op.ID != opAdSetUpdate || !slices.ContainsFunc(budgetFields, func(f string) bool { _, ok := t.params[f]; return ok }) {
		return nil
	}
	if p.deps.Audit == nil {
		return nil
	}
	n, err := p.deps.Audit.Changed(ctx, Kind, m.account, targetName(t.row.op.Resource, t.node), "",
		p.deps.Now().Add(-time.Hour))
	if err != nil {
		return growth.Refuse(growth.RefusedUnavailable, "the access log could not count this ad set's changes in "+
			"the last hour, against Meta's limit of four budget changes. No call was made.")
	}
	if n >= budgetLimit {
		return growth.Refuse(growth.RefusedConflict, fmt.Sprintf("ad set %s changed %d times in the last hour, and "+
			"Meta blocks a fifth budget change for an hour. Try again later. No call was made.", t.node, n))
	}
	return nil
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

// beforesDigest is one digest of every target's state, for the access log.
func beforesDigest(pr prepared) string {
	digests := make([]string, 0, len(pr.befores))
	for _, b := range pr.befores {
		digests = append(digests, b.digest)
	}
	return hexSum([]byte(strings.Join(digests, ",")))
}

// ready is a write made concrete, read and classed, and for spend checked
// against the caps.
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

// moneyFields are the fields Meta writes in the account currency's offset
// units, shown and recorded as micros.
var moneyFields = []string{"daily_budget", "lifetime_budget", "bid_amount", "spend_cap", "daily_spend_cap",
	"lifetime_spend_cap", "daily_min_spend_target", "lifetime_min_spend_target", "budget_value"}

// labels are the fields a person reads most, in Meta's own words.
var labels = map[string]string{"status": "Status", "name": "Name", "daily_budget": "Daily budget",
	"lifetime_budget": "Lifetime budget", "bid_amount": "Bid amount", "bid_strategy": "Bid strategy",
	"targeting": "Targeting", "end_time": "End", "stop_time": "End", "start_time": "Start", "creative": "Creative",
	"message": "Primary text", "object_story_spec": "Creative content", "spend_cap": "Spend cap"}

func labelOf(field string) string {
	if l, ok := labels[field]; ok {
		return l
	}
	return field
}

// cardChanges are what request i changes, labelled; an amount of the
// account's currency is money, in micros.
func (pr prepared) cardChanges(i int, currency string) []growth.CardChange {
	var out []growth.CardChange
	for _, ch := range changesOf(pr.m.targets[i], pr.befores[i]) {
		c := growth.CardChange{Field: ch.Field, Label: labelOf(ch.Field), Before: valueText(ch.Before),
			After: valueText(ch.After)}
		if slices.Contains(moneyFields, ch.Field) && currency != "" {
			after, errA := microsOf(c.After, currency)
			before, errB := microsOf(c.Before, currency)
			if errA == nil && errB == nil {
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

// shown is a change's value as a person reads it: money as a decimal amount.
func shown(c growth.CardChange, v, currency string) string {
	if !c.Money || v == "" {
		return v
	}
	micros, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return v
	}
	d, err := digitsOf(currency)
	if err != nil {
		return v
	}
	s, _ := growth.FormatAmount(micros, d)
	return s + " " + currency
}

// reasonOf is an act's reason, redacted and bounded.
func reasonOf(args map[string]any) string {
	r, _ := args["reason"].(string)
	return textcut.Within(redact.Secrets(strings.TrimSpace(r)), 500)
}

// currencyOf is m's ad account's currency, "" where it reads none.
func (p *Product) currencyOf(ctx context.Context, m made) string {
	if m.account == "" {
		return ""
	}
	a, err := p.accountOf(ctx, m.conn, m.account)
	if err != nil {
		return ""
	}
	return a.Currency
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
	currency := p.currencyOf(ctx, m)
	ops := make([]string, 0, len(m.targets))
	for _, t := range m.targets {
		if !slices.Contains(ops, t.row.op.ID) {
			ops = append(ops, t.row.op.ID)
		}
	}
	question := fmt.Sprintf("May %s %s on %s?", call.Handle, verbOf(m), m.where())
	var ctxt strings.Builder
	sends := "this request"
	if len(m.targets) > 1 {
		sends = "these " + words.Count(len(m.targets), "request", "requests")
	}
	fmt.Fprintf(&ctxt, "%s asks to make **%s** on %s. Allow once sends exactly %s, once.", call.Handle,
		strings.Join(ops, ", "), m.where(), sends)
	if slices.ContainsFunc(m.targets, func(t target) bool { return t.row.op.Danger }) {
		ctxt.WriteString(" **Meta cannot undo it.**")
	}
	for i, t := range m.targets {
		fmt.Fprintf(&ctxt, "\n\n**%s %s** (%s)", t.row.op.Method, words.Inert(t.path), words.Inert(t.row.purpose))
		for _, c := range pr.cardChanges(i, currency) {
			fmt.Fprintf(&ctxt, "\n- %s: %s to %s", words.Inert(c.Label),
				words.Inert(textcut.Within(shown(c, c.Before, currency), 200)),
				words.Inert(textcut.Within(shown(c, c.After, currency), 200)))
		}
	}
	if pr.money != nil {
		f := func(micros int64) string {
			d, err := digitsOf(pr.money.Currency)
			if err != nil {
				d = 2
			}
			s, _ := growth.FormatAmount(micros, d)
			return s + " " + pr.money.Currency
		}
		fmt.Fprintf(&ctxt, "\n\n**Spend**\n\nDaily capacity %s, after %s (adds %s). Caps: %s per change, %s a day. %s",
			f(pr.money.DailyMicros), f(pr.money.DailyAfterMicros), f(pr.money.DeltaMicros),
			f(pr.money.PerChangeCapMicros), f(pr.money.DailyCapMicros), overdelivery)
	}
	ctxt.WriteString("\n\nThe call's page shows the whole request.\n\n**Reason given**\n\n" + words.Inert(reasonOf(call.Args)))
	ask := recordOf(call, m, pr.class, "")
	ask.Before, ask.Changes, ask.Money = beforesDigest(pr), pr.recordChanges(currency), pr.money
	return growth.Hold{Class: pr.class, Binding: pr.binding(), Question: question,
		Context: textcut.Within(ctxt.String(), sandbox.MaxAskContextBytes-1024),
		Subject: delegated.Subject(pr.class.Act(), strings.Join(ops, ","), m.account, operationDigest(m)), Ask: ask}, nil
}

// verbOf is what a held act does, in a few words.
func verbOf(m made) string {
	first := m.targets[0].row.op
	switch first.Method {
	case "DELETE":
		return "delete " + first.Resource
	case "POST", "PUT":
		if m.targets[0].node == "" || strings.Count(first.Path, "/") > 1 {
			return "create or apply " + first.Resource + " " + lastSegment(first.Path)
		}
	}
	return "change " + words.Count(len(m.targets), first.Resource, first.Resource)
}

func lastSegment(path string) string { return path[strings.LastIndex(path, "/")+1:] }

// recordChanges are every request's changes as the access log accepts them.
func (pr prepared) recordChanges(currency string) []growth.RecordChange {
	var all []growth.CardChange
	for i := range pr.m.targets {
		all = append(all, pr.cardChanges(i, currency)...)
	}
	return recordChanges(all)
}

// publicText are the fields whose words customers read, which a card shows
// whole as published text.
var publicText = []string{"message", "headline", "name", "body", "title", "description", "caption", "link"}

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
	card.Account = growth.Account{Kind: Kind, Connection: connection, ID: m.account, State: growth.AccountUnknown}
	currency := ""
	if m.account != "" {
		if a, err := p.accountOf(ctx, m.conn, m.account); err == nil {
			card.Account.Name, card.Account.Currency, card.Account.TimeZone = a.Name, a.Currency, a.TimeZone
			card.Account.State, currency = a.state(), a.Currency
		}
	}
	for i, t := range m.targets {
		act := growth.CardAct{Target: t.row.op.Resource + " " + t.path, Summary: t.row.purpose,
			Changes: pr.cardChanges(i, currency), Danger: t.row.op.Danger}
		if pr.classes[i] == growth.ClassPublic || m.tool == ToolCreateAd {
			act.Text = publishedText(t.params)
		}
		card.Acts = append(card.Acts, act)
	}
	return card, nil
}

// publishedText is the words a request publishes, whole: each text field it
// sets, wherever in its creative spec.
func publishedText(params map[string]any) string {
	var parts []string
	var walk func(key string, v any)
	walk = func(key string, v any) {
		switch x := plain(v).(type) {
		case string:
			if slices.Contains(publicText, key) && strings.TrimSpace(x) != "" {
				parts = append(parts, key+": "+x)
			}
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(k, x[k])
			}
		case []any:
			for _, item := range x {
				walk(key, item)
			}
		}
	}
	walk("", params)
	return strings.Join(parts, "\n")
}

// leaseOf is the lease a write on m holds: one write per ad account, or one
// for the business's objects.
func leaseOf(m made) string {
	if m.account != "" {
		return "write/" + Kind + "/" + m.account
	}
	return "write/" + Kind + "/business"
}

// Applied is an applied write's answer: each request Meta answered, fenced,
// since Meta echoes names and ad text other people wrote.
type Applied struct {
	Account string   `json:"account"`
	Results []Result `json:"results"`
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
	if held && !toolClass[call.Tool].Held() {
		// AN OPERATE TOOL'S CALL IS NEVER ASKED, so none comes approved.
		return nil, growth.Refuse(growth.RefusedForbidden, call.Tool+" is made at once, never as an approved call. "+
			"No call was made.")
	}
	m, err := p.concrete(ctx, call.Handle, call.Tool, call.Args)
	rec := heldRecord(call, m, err == nil)
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
	currency := p.currencyOf(act, m)
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
	rec.Changes = pr.recordChanges(currency)
	var answer Applied
	send := func(ctx context.Context) error {
		a, via, sendErr := p.send(ctx, pr, held)
		answer, rec.Via = a, via
		return sendErr
	}
	if pr.class == growth.ClassSpend {
		var f moneyFigures
		f, err = p.applySpend(act, spendAct{m: m, befores: pr.befores}, send)
		if f != nil {
			rec.Money = f
		}
	} else {
		err = send(act)
	}
	if err != nil {
		rec.Failure = writeFailure(err)
		p.note(ctx, growth.StepFailed, rec)
		return nil, said(refusedBy(err))
	}
	step := growth.StepMade
	if held {
		step = growth.StepApplied
	}
	p.note(ctx, step, rec)
	return answer, nil
}

// moneyFigures are a spend act's figures as the log keeps them.
type moneyFigures = *growth.CardMoney

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
// account, answering the figures it was held to.
func (p *Product) applySpend(ctx context.Context, act spendAct, send func(context.Context) error) (moneyFigures, error) {
	f, err := p.guard().Apply(ctx, act.m.account, act, func(ctx context.Context) (spendguard.Account, spendguard.Caps, error) {
		return p.guardRead(ctx, act.m.conn, act.m.account)
	}, send)
	if f.Currency == "" {
		return nil, err
	}
	return cardMoney(f), err
}

// send makes each request in order, each through the least privileged token
// its class and operation allow, filling a chained parameter from an earlier
// answer, and stops at the first Meta refuses: what was sent before it stands,
// and the answer and the log say which. via names the tokens it went through.
func (p *Product) send(ctx context.Context, pr prepared, held bool) (Applied, string, error) {
	m := pr.m
	answer := Applied{Account: m.account}
	var ids []string
	var via []string
	for i, t := range m.targets {
		tok, err := p.tokenFor(ctx, m.conn, m.own, pr.classes[i], t.row.op.Permissions)
		if err != nil {
			return answer, strings.Join(via, "_"), err
		}
		if slot, _ := tok.signed(); slot != "" && !slices.Contains(via, slot) {
			via = append(via, slot)
		}
		params := url.Values{}
		for k, v := range t.params {
			params.Set(k, formValue(v))
		}
		for k, from := range t.chain {
			if from >= len(ids) || ids[from] == "" {
				return answer, strings.Join(via, "_"), fmt.Errorf("metaads: request %d answered no ID for %s", from, k)
			}
			if k == "creative" {
				params.Set(k, `{"creative_id":"`+ids[from]+`"}`)
				continue
			}
			params.Set(k, ids[from])
		}
		resp, err := m.conn.client.Do(ctx, tok.valueNow(), Request{Method: t.row.op.Method, Path: t.path, Params: params,
			Object: objectOf(m.account), Read: pr.classes[i] == growth.ClassRead, Reserved: held})
		if err != nil {
			if len(answer.Results) > 0 {
				return answer, strings.Join(via, "_"), fmt.Errorf("%w: %d of %d requests were made before %s %s failed: %w",
					errSent, len(answer.Results), len(m.targets), t.row.op.Method, t.path, err)
			}
			return answer, strings.Join(via, "_"), err
		}
		var created struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(resp.Body, &created)
		ids = append(ids, created.ID)
		answer.Results = append(answer.Results, Result{Operation: t.row.op.ID, Path: t.path, Status: resp.Status,
			Document: scrub(m.conn.client, resp.Body)})
	}
	return answer, strings.Join(via, "_"), nil
}

// errSent is a write that failed after some of its requests were made.
var errSent = errors.New("metaads: part of the write was made")

// hold takes resource's lease, or answers why it could not.
func (p *Product) hold(ctx context.Context, resource string) (func(), error) {
	if p.deps.Leases == nil {
		return nil, growth.Refuse(growth.RefusedUnavailable, "this node cannot hold an act's lease, so nothing was sent.")
	}
	release, err := p.deps.Leases.Hold(ctx, resource)
	switch {
	case errors.Is(err, growth.ErrActHeld):
		return nil, growth.Refuse(growth.RefusedUnavailable, "another act on the same ad account is being made; "+
			"nothing was sent. Call again.")
	case err != nil:
		return nil, fmt.Errorf("metaads: hold the ad account's act lease, so nothing was sent: %w", err)
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
	rec := heldRecord(call, m, err == nil)
	rec.Answerer = answerer
	p.note(ctx, step, rec)
	return nil
}

// engineActor is who the engine's own answer names, as its lapse sweep
// answers (internal/engine).
const engineActor = "engine"

// said is err as the agent is told it: a refusal as itself, Meta's own refusal
// in its words, a held-back read as the wait, anything else as the engine's.
func said(err error) error {
	var r *growth.Refusal
	if errors.As(err, &r) {
		return r
	}
	var e *APIError
	if errors.As(err, &e) {
		kind := growth.RefusedInvalid
		switch {
		case e.Code == 190 || e.Code == 200 || e.Code == 10 || e.Status == 401 || e.Status == 403:
			kind = growth.RefusedForbidden
		case e.Code == 803 || e.Status == 404:
			kind = growth.RefusedNotFound
		case e.Throttled() || e.Status >= 500:
			kind = growth.RefusedUnavailable
		}
		said := "Meta answered: " + e.Error()
		if errors.Is(err, errSent) {
			said = err.Error()
		}
		return growth.Refuse(kind, said)
	}
	switch {
	case errors.Is(err, ErrReserve):
		return growth.Refuse(growth.RefusedUnavailable, "this ad account's calls for the hour are down to the ones "+
			"kept for approved acts; try again later in the hour.")
	case errors.Is(err, ErrQuota):
		return growth.Refuse(growth.RefusedUnavailable, "Meta asked Crewlet to slow down on this ad account: "+err.Error()+
			". Reads resume then.")
	case errors.Is(err, ErrTokenRefused):
		return growth.Refuse(growth.RefusedUnavailable, err.Error()+". A person replaces it on the integration's page.")
	}
	return err
}

// scrub is an answer as an agent reads it: paging links dropped (Meta writes
// the token into them) and every secret the client holds and every
// credential's shape redacted.
func scrub(c *Client, body []byte) string {
	var doc map[string]any
	if decode(body, &doc) == nil {
		stripPaging(doc)
		if data, err := json.Marshal(doc); err == nil {
			body = data
		}
	}
	return c.Mask(string(body))
}

// stripPaging drops every paging link, keeping the cursors.
func stripPaging(v any) {
	switch x := v.(type) {
	case map[string]any:
		if paging, ok := x["paging"].(map[string]any); ok {
			delete(paging, "next")
			delete(paging, "previous")
		}
		for _, child := range x {
			stripPaging(child)
		}
	case []any:
		for _, child := range x {
			stripPaging(child)
		}
	}
}
