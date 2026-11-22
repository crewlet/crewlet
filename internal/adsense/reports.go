package adsense

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/period"
)

// account is a publisher account as accounts.get answers it.
type account struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName"`
	State       string   `json:"state"`
	Pending     []string `json:"pendingTasks"`
	Premium     bool     `json:"premium"`
	TimeZone    struct {
		ID string `json:"id"`
	} `json:"timeZone"`
}

// accountCache keeps each account as last read on this node: its zone
// changes only when a person changes it in AdSense, and every report needs
// the zone to cut its days in.
type accountCache struct {
	mu   sync.Mutex
	byID map[string]cachedAccount
}

type cachedAccount struct {
	account
	read time.Time
}

// pacific is Google's zone, the one zone a report may be cut in beside the
// account's (reportingTimeZone GOOGLE_TIME_ZONE).
const pacific = "America/Los_Angeles"

// accountLife is how long a cached account holds: a zone changed in AdSense
// is read again within the hour, and a busy agent's reports cost one read an
// hour.
const accountLife = time.Hour

// reportYears is how far back AdSense keeps report data.
const reportYears = 3

func newAccountCache() *accountCache { return &accountCache{byID: map[string]cachedAccount{}} }

// accountOf is id's account, read where the cache holds none fresh or fresh
// asks for Google's answer now. The read is the engine's own, never its
// call's record: the log keeps one outcome per call, the call's own.
func (p *Product) accountOf(ctx context.Context, handle, id string, fresh bool) (account, error) {
	p.accounts.mu.Lock()
	cached, ok := p.accounts.byID[id]
	p.accounts.mu.Unlock()
	if ok && !fresh && p.now().Sub(cached.read) < accountLife {
		return cached.account, nil
	}
	op, _ := p.Catalog().Operation(accountGetOp)
	req, err := newRequest(op, map[string]string{"accountsId": id}, nil, nil)
	if err != nil {
		return account{}, err
	}
	raw, err := p.client.Send(ctx, handle, req)
	var a account
	if err == nil {
		switch derr := json.Unmarshal(raw, &a); {
		case derr != nil:
			err = &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome,
				Err: fmt.Errorf("adsense: decode account %s: %w", id, derr)}
		case a.Name != "accounts/"+id:
			err = &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome,
				Err: fmt.Errorf("adsense: Google answered for account %s with %q", id, a.Name)}
		}
	}
	if err != nil {
		return account{}, err
	}
	p.accounts.mu.Lock()
	p.accounts.byID[id] = cachedAccount{account: a, read: p.now()}
	p.accounts.mu.Unlock()
	return a, nil
}

// accountRow is one of the seat's accounts, or why it is not reached.
type accountRow struct {
	Account     string   `json:"account"`
	DisplayName string   `json:"display_name,omitempty" untrusted:"name"`
	State       string   `json:"state,omitempty"`
	TimeZone    string   `json:"time_zone,omitempty"`
	Pending     []string `json:"pending_tasks,omitempty"`
	Premium     bool     `json:"premium,omitempty"`
	// Unreached says why the company's sign-in could not read it.
	Unreached string `json:"unreached,omitempty" untrusted:"fence"`
}

// accountsAnswer is adsense_accounts_list's answer.
type accountsAnswer struct {
	Accounts []accountRow `json:"accounts"`
}

// listAccounts reads each of the seat's accounts by its ID, now rather than
// from the cache, so an account the sign-in no longer reaches, or whose state
// changed, is said as it is.
func (p *Product) listAccounts(ctx context.Context, call growth.Call) (accountsAnswer, error) {
	if p.seat(call.Handle) == nil {
		return accountsAnswer{}, refusal(growth.RefusedForbidden, "this agent is off at AdSense. No vendor was "+
			"reached.")
	}
	out := accountsAnswer{Accounts: []accountRow{}}
	var failed error
	reached := 0
	for _, id := range p.accountsOf(call.Handle) {
		a, err := p.accountOf(ctx, call.Handle, id, true)
		if err != nil {
			failed = cmp.Or(failed, err)
			out.Accounts = append(out.Accounts, accountRow{Account: id, Unreached: err.Error()})
			continue
		}
		reached++
		out.Accounts = append(out.Accounts, accountRow{Account: id, DisplayName: a.DisplayName, State: a.State,
			TimeZone: a.TimeZone.ID, Pending: a.Pending, Premium: a.Premium})
	}
	// ONE OUTCOME FOR THE CALL, on the account listing it stands for: made
	// where any account was read, failed with the first refusal where none was.
	if len(out.Accounts) > 0 {
		if reached > 0 {
			failed = nil
		}
		op, _ := p.Catalog().Operation("adsense.accounts.list")
		req, _ := newRequest(op, nil, nil, nil)
		p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, failed)
	}
	return out, nil
}

// untrustedDimensions are the dimensions whose values somebody else wrote:
// names a person typed, page and domain names, a domain's registrant, a
// buyer network's name and a visitor's search.
var untrustedDimensions = []string{"ACCOUNT_NAME", "AD_UNIT_NAME", "CUSTOM_CHANNEL_NAME", "URL_CHANNEL_NAME",
	"OWNED_SITE_DOMAIN_NAME", "PAGE_URL", "DOMAIN_NAME", "BUYER_NETWORK_NAME", "CUSTOM_SEARCH_STYLE_NAME",
	"DOMAIN_REGISTRANT", "WEBSEARCH_QUERY_STRING"}

// reportAnswer is a report tool's answer.
type reportAnswer struct {
	Account string        `json:"account"`
	Report  growth.Report `json:"report"`
	// Warnings are Google's, in English.
	Warnings []warning `json:"warnings,omitempty"`
}

// warning is one of a report's warnings, in Google's words.
type warning struct {
	Text string `json:"text" untrusted:"fence"`
}

// googleReport is a report as reports.generate answers it.
type googleReport struct {
	Total   string `json:"totalMatchedRows"`
	Headers []struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Currency string `json:"currencyCode"`
	} `json:"headers"`
	Rows []struct {
		Cells []struct {
			Value string `json:"value"`
		} `json:"cells"`
	} `json:"rows"`
	Warnings []string `json:"warnings"`
}

// reportFrame is what a report call names before anything is sent: its
// account, read for its zone, the zone, the days and the row limit.
type reportFrame struct {
	account string
	path    map[string]string
	zone    string
	days    growth.Days
	limit   int
}

// frame holds a report call's account, zone and dates before anything is
// sent but the account read the days are cut by, whose failure is the call's
// one outcome.
func (p *Product) frame(ctx context.Context, call growth.Call, op growth.Operation) (reportFrame, error) {
	id := argString(call.Args, "account")
	path := map[string]string{"accountsId": id}
	if strings.Contains(op.Path, "{reportsId}") {
		path["reportsId"] = argString(call.Args, "report")
	}
	probe, err := newRequest(op, path, nil, nil)
	if err != nil {
		return reportFrame{}, err
	}
	if err = p.inScope(call.Handle, probe); err != nil {
		return reportFrame{}, err
	}
	z := argString(call.Args, "time_zone")
	if z != "" && z != pacific {
		return reportFrame{}, refusal(growth.RefusedInvalid, fmt.Sprintf("a report's days are cut in the "+
			"account's own zone, or in %s, Google's; not %q. No vendor was reached.", pacific, z))
	}
	if _, err = p.days(call.Args, time.UTC); err != nil {
		return reportFrame{}, err
	}
	acct, err := p.accountOf(ctx, call.Handle, id, false)
	if err != nil {
		// THE CALL'S ONE OUTCOME is its failure, where the lookup it needed
		// reached Google and failed.
		p.client.MadeOrFailed(ctx, call, probe, growth.ClassRead, err)
		return reportFrame{}, err
	}
	zoneName := cmp.Or(z, acct.TimeZone.ID, pacific)
	zone, err := period.LoadZone(zoneName)
	if err != nil {
		return reportFrame{}, refusal(growth.RefusedInvalid, fmt.Sprintf("the report's zone %q does not load: "+
			"%v. No vendor was reached.", zoneName, err))
	}
	days, err := p.days(call.Args, zone)
	if err != nil {
		return reportFrame{}, err
	}
	return reportFrame{account: id, path: path, zone: zoneName, days: days,
		limit: growth.RowLimit(argInt(call.Args, "limit"))}, nil
}

// localized is a report's dates, zone, currency and language as Google's
// query parameters.
func localized(args map[string]any, f reportFrame) url.Values {
	q := url.Values{"dateRange": {"CUSTOM"}}
	for prefix, day := range map[string]string{"startDate": f.days.Start, "endDate": f.days.End} {
		t, _ := time.Parse(time.DateOnly, day)
		q.Set(prefix+".year", strconv.Itoa(t.Year()))
		q.Set(prefix+".month", strconv.Itoa(int(t.Month())))
		q.Set(prefix+".day", strconv.Itoa(t.Day()))
	}
	if argString(args, "time_zone") == pacific {
		q.Set("reportingTimeZone", "GOOGLE_TIME_ZONE")
	}
	if c := argString(args, "currency"); c != "" {
		q.Set("currencyCode", c)
	}
	if l := argString(args, "language"); l != "" {
		q.Set("languageCode", l)
	}
	return q
}

// report runs adsense_report: every argument held to its closed list before
// anything is sent, one row past the limit asked for, so a cut is seen.
func (p *Product) report(ctx context.Context, call growth.Call) (reportAnswer, error) {
	op, _ := p.Catalog().Operation(reportOp)
	dims, metrics := names(call.Args["dimensions"]), names(call.Args["metrics"])
	if len(metrics) == 0 {
		return reportAnswer{}, refusal(growth.RefusedInvalid, "name at least one metric. No vendor was reached.")
	}
	if err := within("dimensions", dims, reportDimensions); err != nil {
		return reportAnswer{}, err
	}
	if err := within("metrics", metrics, reportMetrics); err != nil {
		return reportAnswer{}, err
	}
	filters, orders := names(call.Args["filters"]), names(call.Args["order_by"])
	for _, f := range filters {
		if err := filterHolds(f); err != nil {
			return reportAnswer{}, err
		}
	}
	for _, o := range orders {
		name := strings.TrimLeft(o, "+-")
		if len(o)-len(name) > 1 || !slices.Contains(dims, name) && !slices.Contains(metrics, name) {
			return reportAnswer{}, refusal(growth.RefusedInvalid, fmt.Sprintf("order_by %q names no dimension "+
				"or metric the report returns, after an optional + or -. No vendor was reached.", o))
		}
	}
	f, err := p.frame(ctx, call, op)
	if err != nil {
		return reportAnswer{}, err
	}
	q := localized(call.Args, f)
	q["dimensions"], q["metrics"] = dims, metrics
	if len(filters) > 0 {
		q["filters"] = filters
	}
	if len(orders) > 0 {
		q["orderBy"] = orders
	}
	q.Set("limit", strconv.Itoa(f.limit+1))
	req, err := newRequest(op, f.path, q, nil)
	if err != nil {
		return reportAnswer{}, err
	}
	return p.run(ctx, call, req, f)
}

// savedReport runs a saved report over the call's dates: its dimensions and
// metrics are the saved ones, and Google takes no row limit for it, so the
// answer is cut here.
func (p *Product) savedReport(ctx context.Context, call growth.Call) (reportAnswer, error) {
	op, _ := p.Catalog().Operation(savedReportOp)
	f, err := p.frame(ctx, call, op)
	if err != nil {
		return reportAnswer{}, err
	}
	req, err := newRequest(op, f.path, localized(call.Args, f), nil)
	if err != nil {
		return reportAnswer{}, err
	}
	return p.run(ctx, call, req, f)
}

// run sends a report's request, records its one outcome, and shapes it.
func (p *Product) run(ctx context.Context, call growth.Call, req googlegrowth.Request,
	f reportFrame) (reportAnswer, error) {
	raw, err := p.client.Send(ctx, call.Handle, req)
	var g googleReport
	if err == nil {
		if derr := json.Unmarshal(raw, &g); derr != nil {
			err = &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome,
				Err: fmt.Errorf("adsense: decode %s: %w", req.Op.ID, derr)}
		}
	}
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return reportAnswer{}, err
	}
	out := shape(g, f.limit)
	out.Account, out.Report.TimeZone = f.account, f.zone
	zone, _ := period.LoadZone(f.zone)
	yesterday := period.At(period.Day, p.now(), zone).Shift(-1).Start.Format(time.DateOnly)
	out.Report.DataThrough = min(f.days.End, yesterday)
	return out, nil
}

// filterPart is one alternative of a report filter: a dimension, an operator
// and a value.
var filterPart = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^([A-Z_]+)(==|!=|=@|!@)(.*)$`)
})

// filterHolds refuses a filter naming a dimension the report does not know,
// in any of its comma-joined alternatives (a value's own comma is \,).
func filterHolds(filter string) error {
	for _, part := range alternatives(filter) {
		m := filterPart().FindStringSubmatch(strings.TrimSpace(part))
		if m == nil || !slices.Contains(reportDimensions, m[1]) {
			return refusal(growth.RefusedInvalid, fmt.Sprintf("filter %q is not DIMENSION==value (or !=, =@, "+
				"!@) over the report's dimensions. No vendor was reached.", filter))
		}
	}
	return nil
}

// alternatives are a filter's parts between its commas, an escaped comma (\,)
// kept in its value.
func alternatives(filter string) []string {
	var out []string
	start := 0
	for i := 0; i < len(filter); i++ {
		switch filter[i] {
		case '\\':
			i++
		case ',':
			out = append(out, filter[start:i])
			start = i + 1
		}
	}
	return append(out, filter[start:])
}

// days are the report's first and last day in zone: its named range, its
// dates, or the last 28 complete days, within the three years AdSense keeps.
func (p *Product) days(args map[string]any, zone *time.Location) (growth.Days, error) {
	r := growth.Range(argString(args, "range"))
	start, end := argString(args, "start_date"), argString(args, "end_date")
	var days growth.Days
	switch {
	case r != "" && (start != "" || end != ""):
		return days, refusal(growth.RefusedInvalid, "give a range or dates, not both. No vendor was reached.")
	case r != "":
		if !r.Valid() {
			return days, refusal(growth.RefusedInvalid, fmt.Sprintf("%q is not a named range. No vendor was "+
				"reached.", r))
		}
		d, err := r.Resolve(p.now(), zone)
		if err != nil {
			return days, err
		}
		days = d
	default:
		d, _ := growth.RangeLast28Days.Resolve(p.now(), zone)
		days = d
		if start != "" {
			days.Start = start
		}
		if end != "" {
			days.End = end
		}
	}
	first, errA := time.Parse(time.DateOnly, days.Start)
	last, errB := time.Parse(time.DateOnly, days.End)
	oldest := period.At(period.Day, p.now(), zone).Start.AddDate(-reportYears, 0, 0).Format(time.DateOnly)
	switch {
	case errA != nil || errB != nil:
		return days, refusal(growth.RefusedInvalid, "dates are YYYY-MM-DD. No vendor was reached.")
	case last.Before(first):
		return days, refusal(growth.RefusedInvalid, "the last day is before the first. No vendor was reached.")
	case days.Start < oldest:
		return days, refusal(growth.RefusedInvalid, fmt.Sprintf("AdSense keeps %d years of report data: start "+
			"on or after %s. No vendor was reached.", reportYears, oldest))
	}
	return days, nil
}

// headerName is a report column's name as Google's enums write them.
var headerName = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`) })

// shape is Google's report as the answer gives it: a column per header,
// money a decimal string of its currency, counts and rates numbers, at most
// limit rows, cut where Google had more.
func shape(g googleReport, limit int) reportAnswer {
	var out reportAnswer
	for _, w := range g.Warnings {
		out.Warnings = append(out.Warnings, warning{Text: w})
	}
	r := &out.Report
	for i, h := range g.Headers {
		col := growth.Column{Name: h.Name, Type: growth.ColumnNumber}
		if !headerName().MatchString(h.Name) {
			// A NAME OUTSIDE GOOGLE'S ENUMS is no column name to pass on.
			col.Name = "COLUMN_" + strconv.Itoa(i+1)
		}
		switch h.Type {
		case "DIMENSION":
			col.Type = growth.ColumnText
			if h.Name == "DATE" {
				col.Type = growth.ColumnDate
			}
			col.Untrusted = slices.Contains(untrustedDimensions, h.Name) || col.Name != h.Name
		case "METRIC_CURRENCY":
			col.Type, col.Currency = growth.ColumnMoney, h.Currency
			r.Estimated = true
		}
		r.Columns = append(r.Columns, col)
	}
	rows := g.Rows
	if len(rows) > limit {
		rows, r.Truncated = rows[:limit], true
	}
	if n, err := strconv.ParseInt(g.Total, 10, 64); err == nil {
		r.TotalRows = &n
		r.Truncated = r.Truncated || n > int64(len(rows))
	}
	r.Rows = make([][]any, 0, len(rows))
	for _, row := range rows {
		cells := make([]any, len(r.Columns))
		for i := range r.Columns {
			if i < len(row.Cells) {
				cells[i] = cellOf(r.Columns[i].Type, row.Cells[i].Value)
			}
		}
		r.Rows = append(r.Rows, cells)
	}
	if len(r.Rows) == 0 {
		r.EmptyReason = growth.EmptyNoData
	}
	return out
}

// cellOf is one cell as its column holds it: text and money as Google's
// strings, a number parsed, nil where Google's value does not parse.
func cellOf(typ growth.ColumnType, value string) any {
	if typ != growth.ColumnNumber {
		return value
	}
	if n, err := strconv.ParseInt(value, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(value, 64); err == nil {
		return f
	}
	return nil
}
