package metaads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/words"
)

// AccountRow is one of the seat's ad accounts as the accounts tool answers it.
type AccountRow struct {
	ID            string `json:"account"`
	Name          string `json:"name" untrusted:"name"`
	Currency      string `json:"currency"`
	TimeZone      string `json:"time_zone"`
	Status        string `json:"status"`
	DisableReason string `json:"disable_reason"`
	AmountSpent   string `json:"amount_spent"`
	SpendCap      string `json:"spend_cap"`
	MinDaily      string `json:"min_daily_budget"`
	Reachable     bool   `json:"reachable"`
}

// Accounts is the accounts tool's answer.
type Accounts struct {
	Accounts []AccountRow `json:"accounts"`
	// Tier is Meta's Marketing API access tier for the app, as its last
	// answer said: Limited access is heavily rate limited.
	Tier string `json:"access_tier"`
}

// disableWords are Meta's disable_reason values in words.
var disableWords = map[int]string{0: "", 1: "ads integrity policy", 2: "ads IP review", 3: "risk payment",
	4: "gray account shut down", 5: "ads AFC review", 6: "business integrity", 7: "permanent close",
	8: "unused reseller account", 9: "unused account"}

// amount is a Meta amount in offset units as a decimal in currency, "" where
// it does not read.
func amount(v any, currency string) string {
	m, err := microsOf(v, currency)
	if err != nil {
		return ""
	}
	d, _ := digitsOf(currency)
	s, _ := growth.FormatAmount(m, d)
	return s
}

// accounts are the seat's ad accounts as the system user reaches them.
func (p *Product) accounts(ctx context.Context, call growth.Call) (any, error) {
	seat, err := p.seat(call.Handle)
	if err != nil {
		return nil, err
	}
	c, err := p.connNow()
	if err != nil {
		return nil, err
	}
	reach, err := p.accountsOf(ctx, c)
	if err != nil {
		return nil, said(err)
	}
	out := Accounts{Accounts: []AccountRow{}, Tier: c.client.Tier()}
	for _, id := range seatAccounts(seat) {
		row := AccountRow{ID: id}
		if i := slices.IndexFunc(reach, func(a adAccount) bool { return a.ID == id }); i >= 0 {
			a := reach[i]
			row = AccountRow{ID: id, Name: a.Name, Currency: a.Currency, TimeZone: a.TimeZone, Status: a.statusText(),
				DisableReason: disableWords[a.DisableReason], AmountSpent: amount(a.AmountSpent, a.Currency),
				SpendCap: amount(a.SpendCap, a.Currency), MinDaily: amount(a.MinDaily, a.Currency), Reachable: true}
		}
		out.Accounts = append(out.Accounts, row)
	}
	p.note(ctx, growth.StepMade, growth.Record{Product: Kind, Tool: call.Tool, Class: growth.ClassRead,
		Handle: call.Handle, Turn: call.Turn, Call: call.ID, Connection: connection, Account: "business", Via: slotRead,
		Operation: argsDigest(call)})
	return out, nil
}

// ObjectRow is one campaign, ad set or ad as its list tool answers it. Money
// is a decimal in the account's currency.
type ObjectRow struct {
	ID              string `json:"id"`
	Name            string `json:"name" untrusted:"name"`
	Status          string `json:"status"`
	EffectiveStatus string `json:"effective_status"`
	Objective       string `json:"objective,omitempty"`
	Campaign        string `json:"campaign_id,omitempty"`
	AdSet           string `json:"ad_set_id,omitempty"`
	DailyBudget     string `json:"daily_budget,omitempty"`
	LifetimeBudget  string `json:"lifetime_budget,omitempty"`
	BidStrategy     string `json:"bid_strategy,omitempty"`
	BidAmount       string `json:"bid_amount,omitempty"`
	Optimization    string `json:"optimization_goal,omitempty"`
	PromotedApp     string `json:"promoted_app,omitempty"`
	StoreURL        string `json:"store_url,omitempty"`
	Start           string `json:"start,omitempty"`
	End             string `json:"end,omitempty"`
	Countries       string `json:"countries,omitempty"`
	Ages            string `json:"ages,omitempty"`
	Platforms       string `json:"platforms,omitempty"`
	Creative        string `json:"creative_id,omitempty"`
	// Text is the ad's creative text and Meta's review feedback: other
	// people's words.
	Text string `json:"text,omitempty" untrusted:"fence"`
}

// Objects is a list tool's answer.
type Objects struct {
	Account  string      `json:"account"`
	Currency string      `json:"currency"`
	Rows     []ObjectRow `json:"rows"`
	After    string      `json:"after,omitempty"`
}

// objectLists are each list tool's operation, edge and fields.
var objectLists = map[string]struct{ op, edge, fields string }{
	ToolCampaigns: {opCampaignsList, "/campaigns", "id,name,status,effective_status,objective,daily_budget," +
		"lifetime_budget,bid_strategy,spend_cap,start_time,stop_time,account_id"},
	ToolAdSets: {opAdSetsList, "/adsets", "id,name,status,effective_status,campaign_id,daily_budget,lifetime_budget," +
		"bid_strategy,bid_amount,optimization_goal,promoted_object,start_time,end_time,targeting,account_id"},
	ToolAds: {opAdsList, "/ads", "id,name,status,effective_status,campaign_id,adset_id,creative,ad_review_feedback," +
		"issues_info,account_id"},
}

// objects lists one list tool's objects of one of the seat's ad accounts.
func (p *Product) objects(ctx context.Context, call growth.Call) (any, error) {
	seat, err := p.seat(call.Handle)
	if err != nil {
		return nil, err
	}
	account, err := accountNamed(seat, call.Args)
	if err != nil {
		return nil, err
	}
	c, err := p.connNow()
	if err != nil {
		return nil, err
	}
	a, err := p.accountOf(ctx, c, account)
	if err != nil {
		return nil, said(err)
	}
	spec := objectLists[call.Tool]
	q := url.Values{"fields": {spec.fields}, "limit": {"50"}}
	if n, lerr := limitOf(call.Args["limit"]); lerr == nil && n >= 1 && n <= maxListLimit {
		q.Set("limit", strconv.Itoa(n))
	}
	if after, _ := call.Args["after"].(string); after != "" {
		q.Set("after", after)
	}
	switch status, _ := call.Args["status"].(string); status {
	case "ACTIVE", "PAUSED":
		q.Set("filtering", `[{"field":"effective_status","operator":"IN","value":["`+status+`"]}]`)
	case "", "all":
	default:
		return nil, invalid("`status` is ACTIVE, PAUSED or all.")
	}
	tok, err := p.tokenFor(ctx, c, growth.ClassRead, rowOf()[spec.op].op.Permissions)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(ctx, tok.valueNow(), Request{Method: http.MethodGet, Path: "/" + account + spec.edge,
		Params: q, Object: objectOf(account), Read: true})
	rec := growth.Record{Product: Kind, Tool: call.Tool, Class: growth.ClassRead, Handle: call.Handle, Turn: call.Turn,
		Call: call.ID, Connection: connection, Account: account, Via: tok.slot, Operation: argsDigest(call)}
	if err != nil {
		rec.Failure = failureOf(err)
		p.note(ctx, growth.StepFailed, rec)
		return nil, said(err)
	}
	p.note(ctx, growth.StepMade, rec)
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	if err := decode(resp.Body, &doc); err != nil {
		return nil, fmt.Errorf("%w: %s's %s: %w", errAnswer, account, spec.edge, err)
	}
	out := Objects{Account: account, Currency: a.Currency, Rows: []ObjectRow{}, After: afterOf(resp.Body)}
	for _, d := range doc.Data {
		if acct, _ := d["account_id"].(string); "act_"+acct != account {
			continue
		}
		out.Rows = append(out.Rows, objectRow(d, a.Currency))
	}
	return out, nil
}

// objectRow is one listed object as an agent reads it.
func objectRow(d map[string]any, currency string) ObjectRow {
	s := func(k string) string {
		v, _ := d[k].(string)
		return v
	}
	r := ObjectRow{ID: s("id"), Name: s("name"), Status: s("status"), EffectiveStatus: s("effective_status"),
		Objective: s("objective"), Campaign: s("campaign_id"), AdSet: s("adset_id"), BidStrategy: s("bid_strategy"),
		Optimization: s("optimization_goal"), Start: s("start_time"), End: s("stop_time") + s("end_time")}
	if d["daily_budget"] != nil {
		r.DailyBudget = amount(d["daily_budget"], currency)
	}
	if d["lifetime_budget"] != nil {
		r.LifetimeBudget = amount(d["lifetime_budget"], currency)
	}
	if d["bid_amount"] != nil {
		r.BidAmount = amount(d["bid_amount"], currency)
	}
	if po, ok := d["promoted_object"].(map[string]any); ok {
		r.PromotedApp, _ = po["application_id"].(string)
		r.StoreURL, _ = po["object_store_url"].(string)
	}
	if tg, ok := d["targeting"].(map[string]any); ok {
		if geo, ok := tg["geo_locations"].(map[string]any); ok {
			r.Countries = joinAny(geo["countries"])
		}
		if tg["age_min"] != nil || tg["age_max"] != nil {
			r.Ages = fmt.Sprintf("%v-%v", tg["age_min"], tg["age_max"])
		}
		r.Platforms = joinAny(tg["publisher_platforms"])
	}
	if cr, ok := d["creative"].(map[string]any); ok {
		r.Creative, _ = cr["id"].(string)
	}
	var text []string
	if fb, ok := d["ad_review_feedback"]; ok {
		data, _ := json.Marshal(fb)
		text = append(text, "review feedback: "+string(data))
	}
	if issues, ok := d["issues_info"]; ok {
		data, _ := json.Marshal(issues)
		text = append(text, "issues: "+string(data))
	}
	r.Text = strings.Join(text, "\n")
	return r
}

func joinAny(v any) string {
	list, _ := v.([]any)
	parts := make([]string, 0, len(list))
	for _, x := range list {
		parts = append(parts, fmt.Sprint(x))
	}
	return strings.Join(parts, ",")
}

// insightBreakdowns are the single breakdowns Meta accepts at every level.
var insightBreakdowns = []string{"age", "gender", "country", "publisher_platform", "platform_position",
	"device_platform", "impression_device"}

// bidStrategies are Meta's bid strategies an ad set may name.
var bidStrategies = []string{"LOWEST_COST_WITHOUT_CAP", "LOWEST_COST_WITH_BID_CAP", "COST_CAP", "LOWEST_COST_WITH_MIN_ROAS"}

func rangeNames() []string {
	out := make([]string, 0, len(growth.Ranges()))
	for _, r := range growth.Ranges() {
		out = append(out, string(r))
	}
	return out
}

// InsightRow is one row of insights. Spend is a decimal in the account's
// currency, as Meta answers it.
type InsightRow struct {
	Start       string `json:"date_start"`
	End         string `json:"date_stop"`
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty" untrusted:"name"`
	Breakdown   string `json:"breakdown,omitempty"`
	Spend       string `json:"spend"`
	Impressions int64  `json:"impressions"`
	Clicks      int64  `json:"clicks"`
	Reach       int64  `json:"reach"`
	Installs    int64  `json:"app_installs"`
}

// Insights is the insights tool's answer, honest about truncation, its zone
// and why it is empty.
type Insights struct {
	Account     string             `json:"account"`
	Currency    string             `json:"currency"`
	TimeZone    string             `json:"time_zone"`
	Level       string             `json:"level"`
	Start       string             `json:"start"`
	End         string             `json:"end"`
	Rows        []InsightRow       `json:"rows"`
	Truncated   bool               `json:"truncated"`
	Estimated   bool               `json:"estimated"`
	EmptyReason growth.EmptyReason `json:"empty_reason,omitempty"`
}

// asyncAfterDays is the longest range read synchronously: Meta answers longer
// ones, an ad level or a breakdown more reliably as an async report run.
const asyncAfterDays = 31

// restated is how far back Meta still restates figures: attribution settles
// within 28 days.
const restated = 28 * 24 * time.Hour

// insights reads one of the seat's ad accounts' insights.
func (p *Product) insights(ctx context.Context, call growth.Call) (any, error) {
	seat, err := p.seat(call.Handle)
	if err != nil {
		return nil, err
	}
	account, err := accountNamed(seat, call.Args)
	if err != nil {
		return nil, err
	}
	c, err := p.connNow()
	if err != nil {
		return nil, err
	}
	a, err := p.accountOf(ctx, c, account)
	if err != nil {
		return nil, said(err)
	}
	zone, err := time.LoadLocation(a.TimeZone)
	if err != nil {
		zone = time.UTC
	}
	level, _ := call.Args["level"].(string)
	if level == "" {
		level = "account"
	}
	if !slices.Contains([]string{"account", "campaign", "adset", "ad"}, level) {
		return nil, invalid("`level` is account, campaign, adset or ad.")
	}
	days, err := daysOf(call.Args, p.deps.Now(), zone)
	if err != nil {
		return nil, err
	}
	breakdown, _ := call.Args["breakdown"].(string)
	if breakdown != "" && !slices.Contains(insightBreakdowns, breakdown) {
		return nil, invalid("`breakdown` is one of %s.", words.Or(insightBreakdowns))
	}
	limit := growth.ReportDefaultRows
	if n, lerr := limitOf(call.Args["limit"]); lerr == nil {
		limit = growth.RowLimit(n)
	}
	fields := "spend,impressions,clicks,reach,actions,date_start,date_stop"
	if level != "account" {
		fields += "," + level + "_id," + level + "_name"
	}
	q := url.Values{"level": {level}, "fields": {fields}, "use_unified_attribution_setting": {"true"},
		"time_range": {`{"since":"` + days.Start + `","until":"` + days.End + `"}`}, "limit": {"500"}}
	if daily, _ := call.Args["daily"].(bool); daily {
		q.Set("time_increment", "1")
	}
	if breakdown != "" {
		q.Set("breakdowns", breakdown)
	}
	tok, err := p.tokenFor(ctx, c, growth.ClassRead, []string{"ads_read"})
	if err != nil {
		return nil, err
	}
	rec := growth.Record{Product: Kind, Tool: call.Tool, Class: growth.ClassRead, Handle: call.Handle, Turn: call.Turn,
		Call: call.ID, Connection: connection, Account: account, Via: tok.slot, Operation: argsDigest(call)}
	start, _ := time.ParseInLocation(time.DateOnly, days.Start, zone)
	end, _ := time.ParseInLocation(time.DateOnly, days.End, zone)
	async := level == "ad" || breakdown != "" || end.Sub(start) > asyncAfterDays*24*time.Hour
	out := Insights{Account: account, Currency: a.Currency, TimeZone: a.TimeZone, Level: level, Start: days.Start,
		End: days.End, Rows: []InsightRow{}, Estimated: p.deps.Now().Sub(end) < restated}
	collect := func(raw json.RawMessage) error {
		if len(out.Rows) >= limit {
			out.Truncated = true
			return nil
		}
		row, rerr := insightRow(raw, level, breakdown)
		if rerr != nil {
			return rerr
		}
		out.Rows = append(out.Rows, row)
		return nil
	}
	path := "/" + account + "/insights"
	if !async {
		err = p.pages(ctx, c, tok, path, q, objectOf(account), false, growth.ReportMaxRows/500+1, collect)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == 100 && apiErr.Subcode == 1487534 {
			async, err = true, nil
		}
	}
	if async {
		var run string
		run, err = p.reportRun(ctx, c, tok, path, q, objectOf(account))
		if err == nil {
			err = p.pages(ctx, c, tok, "/"+run+"/insights", url.Values{"limit": {"500"}}, objectOf(account), false,
				growth.ReportMaxRows/500+1, collect)
		}
	}
	if errors.Is(err, errTooLong) {
		out.Truncated, err = true, nil
	}
	if err != nil {
		rec.Failure = failureOf(err)
		p.note(ctx, growth.StepFailed, rec)
		return nil, said(err)
	}
	p.note(ctx, growth.StepMade, rec)
	if len(out.Rows) == 0 {
		out.EmptyReason = growth.EmptyNoData
	}
	return out, nil
}

// daysOf are a call's days: its named range, or its start and end, in zone.
func daysOf(args map[string]any, now time.Time, zone *time.Location) (growth.Days, error) {
	start, _ := args["start"].(string)
	end, _ := args["end"].(string)
	if start != "" || end != "" {
		s, err1 := time.ParseInLocation(time.DateOnly, start, zone)
		e, err2 := time.ParseInLocation(time.DateOnly, end, zone)
		if err1 != nil || err2 != nil || e.Before(s) {
			return growth.Days{}, invalid("`start` and `end` are days, YYYY-MM-DD, the end not before the start.")
		}
		return growth.Days{Start: start, End: end, Zone: zone}, nil
	}
	r, _ := args["range"].(string)
	if r == "" {
		r = string(growth.RangeLast7Days)
	}
	days, err := growth.Range(r).Resolve(now, zone)
	if err != nil {
		return growth.Days{}, invalid("`range` is one of %s.", words.Or(rangeNames()))
	}
	return days, nil
}

// insightRow reads one row of insights.
func insightRow(raw json.RawMessage, level, breakdown string) (InsightRow, error) {
	var d map[string]any
	if err := decode(raw, &d); err != nil {
		return InsightRow{}, fmt.Errorf("%w: an insights row: %w", errAnswer, err)
	}
	s := func(k string) string {
		v, _ := d[k].(string)
		return v
	}
	n := func(k string) int64 {
		i, _ := strconv.ParseInt(fmt.Sprint(d[k]), 10, 64)
		return i
	}
	row := InsightRow{Start: s("date_start"), End: s("date_stop"), Spend: s("spend"), Impressions: n("impressions"),
		Clicks: n("clicks"), Reach: n("reach")}
	if row.Spend == "" {
		row.Spend = "0"
	}
	if level != "account" {
		row.ID, row.Name = s(level+"_id"), s(level+"_name")
	}
	if breakdown != "" {
		row.Breakdown = s(breakdown)
	}
	actions, _ := d["actions"].([]any)
	for _, raw := range actions {
		a, _ := raw.(map[string]any)
		if a["action_type"] == "mobile_app_install" {
			row.Installs, _ = strconv.ParseInt(fmt.Sprint(a["value"]), 10, 64)
		}
	}
	return row, nil
}

// reportPolls bounds an async report's wait: polled every two seconds, up to
// a minute and a half, past which the agent is told to ask again.
const (
	reportPoll  = 2 * time.Second
	reportPolls = 45
)

// reportRun starts an async insights report and waits for it, answering its id.
func (p *Product) reportRun(ctx context.Context, c *conn, tok *token, path string, q url.Values, object string) (string, error) {
	resp, err := c.client.Do(ctx, tok.valueNow(), Request{Method: http.MethodPost, Path: path, Params: q,
		Object: object, Read: true})
	if err != nil {
		return "", err
	}
	var started struct {
		ID string `json:"report_run_id"`
	}
	if json.Unmarshal(resp.Body, &started) != nil || !graphID().MatchString(started.ID) {
		return "", fmt.Errorf("%w: the report run's ID", errAnswer)
	}
	for range reportPolls {
		resp, err := c.client.Do(ctx, tok.valueNow(), Request{Method: http.MethodGet, Path: "/" + started.ID,
			Params: url.Values{"fields": {"async_status,async_percent_completion"}}, Object: object, Read: true})
		if err != nil {
			return "", err
		}
		var st struct {
			Status string `json:"async_status"`
		}
		_ = json.Unmarshal(resp.Body, &st)
		switch st.Status {
		case "Job Completed":
			return started.ID, nil
		case "Job Failed", "Job Skipped":
			return "", growth.Refuse(growth.RefusedUnavailable, "Meta's report run failed; ask again with a shorter range.")
		}
		if err := httpx.Sleep(ctx, reportPoll); err != nil {
			return "", err
		}
	}
	return "", growth.Refuse(growth.RefusedUnavailable, "Meta's report run is still working; ask again in a minute.")
}
