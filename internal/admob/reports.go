package admob

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	PublisherID string `json:"publisherId"`
	Currency    string `json:"currencyCode"`
	TimeZone    string `json:"reportingTimeZone"`
}

// accountCache keeps each account's currency and zone on this node: they
// change only when a person changes them in AdMob, and every report needs the
// zone to cut its days in.
type accountCache struct {
	mu   sync.Mutex
	byID map[string]cachedAccount
}

type cachedAccount struct {
	account
	read time.Time
}

// pacific is the one zone Google accepts for a report beside the account's.
const pacific = "America/Los_Angeles"

// accountLife is how long a cached account holds: a zone changed in AdMob is
// read again within the hour, and a busy agent's reports cost one read an hour.
const accountLife = time.Hour

func newAccountCache() *accountCache { return &accountCache{byID: map[string]cachedAccount{}} }

// accountOf is id's account, read where the cache holds none fresh. The read
// is the engine's own, never its call's record: the log keeps one outcome per
// call, the call's own.
func (p *Product) accountOf(ctx context.Context, call growth.Call, id string) (account, error) {
	p.accounts.mu.Lock()
	cached, ok := p.accounts.byID[id]
	p.accounts.mu.Unlock()
	if ok && p.now().Sub(cached.read) < accountLife {
		return cached.account, nil
	}
	op, _ := p.Catalog().Operation("admob.accounts.get")
	req, err := newRequest(op, map[string]string{"accountsId": id}, nil, nil)
	if err != nil {
		return account{}, err
	}
	raw, err := p.client.Send(ctx, call.Handle, req)
	var a account
	if err == nil {
		if derr := json.Unmarshal(raw, &a); derr != nil {
			err = &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome,
				Err: fmt.Errorf("admob: decode account %s: %w", id, derr)}
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
	Account  string `json:"account"`
	Currency string `json:"currency,omitempty"`
	TimeZone string `json:"time_zone,omitempty"`
	// Unreached says why the company's sign-in could not read it.
	Unreached string `json:"unreached,omitempty" untrusted:"fence"`
}

// accountsAnswer is admob_accounts_list's answer.
type accountsAnswer struct {
	Accounts []accountRow `json:"accounts"`
}

// listAccounts reads each of the seat's accounts: accounts.list answers only
// the account last opened in AdMob, so each is read by its ID.
func (p *Product) listAccounts(ctx context.Context, call growth.Call) (accountsAnswer, error) {
	if p.seat(call.Handle) == nil {
		return accountsAnswer{}, refusal(growth.RefusedForbidden, "this agent is off at AdMob. No vendor was reached.")
	}
	out := accountsAnswer{Accounts: []accountRow{}}
	var failed error
	reached := 0
	for _, id := range p.accountsOf(call.Handle) {
		a, err := p.accountOf(ctx, call, id)
		if err != nil {
			failed = cmp.Or(failed, err)
			out.Accounts = append(out.Accounts, accountRow{Account: id, Unreached: err.Error()})
			continue
		}
		reached++
		out.Accounts = append(out.Accounts, accountRow{Account: id, Currency: a.Currency, TimeZone: a.TimeZone})
	}
	// ONE OUTCOME FOR THE CALL, on the account listing it stands for: made
	// where any account was read, failed with the first refusal where none was.
	if len(out.Accounts) > 0 {
		if reached > 0 {
			failed = nil
		}
		op, _ := p.Catalog().Operation("admob.accounts.list")
		req, _ := newRequest(op, nil, nil, nil)
		p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, failed)
	}
	return out, nil
}

// labelled are the dimensions whose values carry a name somebody typed (an
// app's, an ad unit's, a network's, a group's), shown beside the ID.
var labelled = []string{"APP", "AD_UNIT", "AD_SOURCE", "AD_SOURCE_INSTANCE", "MEDIATION_GROUP"}

// untrustedValues are the dimensions whose value itself is somebody's words.
var untrustedValues = []string{"APP_VERSION_NAME", "CAMPAIGN_NAME", "AD_NAME", "PLACEMENT_NAME"}

// moneyMetrics are the metrics Google gives in micros of the report's currency.
var moneyMetrics = []string{"ESTIMATED_EARNINGS", "IMPRESSION_RPM", "OBSERVED_ECPM", "ESTIMATED_COST", "AVERAGE_CPI"}

// warning is a report footer's warning, in Google's words.
type warning struct {
	Type        string `json:"type"`
	Description string `json:"description" untrusted:"fence"`
}

// reportAnswer is a report tool's answer.
type reportAnswer struct {
	Account  string        `json:"account"`
	Currency string        `json:"currency,omitempty"`
	Report   growth.Report `json:"report"`
	// Warnings are Google's: data delayed, a zone or currency changed.
	Warnings []warning `json:"warnings,omitempty"`
}

// googleReport is one message of a report: its header, a row, or its footer.
type googleReport struct {
	Header *struct {
		TimeZone     string `json:"reportingTimeZone"`
		Localization struct {
			Currency string `json:"currencyCode"`
		} `json:"localizationSettings"`
	} `json:"header"`
	Row    *googleRow `json:"row"`
	Footer *struct {
		Warnings []warning `json:"warnings"`
		Matching string    `json:"matchingRowCount"`
	} `json:"footer"`
}

// googleRow is one report row.
type googleRow struct {
	Dimensions map[string]struct {
		Value string `json:"value"`
		Label string `json:"displayLabel"`
	} `json:"dimensionValues"`
	Metrics map[string]struct {
		Integer string   `json:"integerValue"`
		Double  *float64 `json:"doubleValue"`
		Micros  string   `json:"microsValue"`
	} `json:"metricValues"`
}

// report runs a network, mediation or campaign report and shapes its answer.
func (p *Product) report(ctx context.Context, call growth.Call) (reportAnswer, error) {
	id := map[string]string{toolNetworkReport: networkReportOp, toolMediationReport: mediationReportOp,
		toolCampaignReport: campaignReportOp}[call.Tool]
	op, _ := p.Catalog().Operation(id)
	accountID := argString(call.Args, "account")
	probe, err := newRequest(op, map[string]string{"accountsId": accountID}, nil, nil)
	if err != nil {
		return reportAnswer{}, err
	}
	if err = p.inScope(call.Handle, probe); err != nil {
		return reportAnswer{}, err
	}
	acct, err := p.accountOf(ctx, call, accountID)
	if err != nil {
		// THE CALL'S ONE OUTCOME is its failure, where the lookup it needed
		// reached Google and failed.
		p.client.MadeOrFailed(ctx, call, probe, growth.ClassRead, err)
		return reportAnswer{}, err
	}
	zoneName := acct.TimeZone
	switch z := argString(call.Args, "time_zone"); {
	case z != "" && (z != pacific || id == campaignReportOp):
		return reportAnswer{}, refusal(growth.RefusedInvalid, fmt.Sprintf("a report's days are cut in the "+
			"account's zone, %s, or for a network or mediation report in %s, the one other zone Google "+
			"accepts; not %q. No vendor was reached.", acct.TimeZone, pacific, z))
	case z != "":
		zoneName = z
	}
	zone, err := period.LoadZone(zoneName)
	if err != nil {
		return reportAnswer{}, refusal(growth.RefusedInvalid, fmt.Sprintf("the report's zone %q does not "+
			"load: %v. No vendor was reached.", zoneName, err))
	}
	days, err := p.days(call.Args, zone, id == campaignReportOp)
	if err != nil {
		return reportAnswer{}, err
	}
	limit := growth.RowLimit(argInt(call.Args, "limit"))
	dims, metrics := names(call.Args["dimensions"]), names(call.Args["metrics"])
	if len(metrics) == 0 {
		return reportAnswer{}, refusal(growth.RefusedInvalid, "name at least one metric. No vendor was reached.")
	}
	closed := reportLists[id]
	if err = within("dimensions", stringsOf(dims), closed[0]); err != nil {
		return reportAnswer{}, err
	}
	if err = within("metrics", stringsOf(metrics), closed[1]); err != nil {
		return reportAnswer{}, err
	}
	spec := map[string]any{"dateRange": map[string]any{"startDate": date(days.Start), "endDate": date(days.End)},
		"dimensions": dims, "metrics": metrics}
	if id == campaignReportOp {
		if lang := argString(call.Args, "language"); lang != "" {
			spec["languageCode"] = lang
		}
	} else {
		// ONE ROW MORE THAN THE LIMIT tells a full answer from a cut one.
		spec["maxReportRows"] = limit + 1
		if err = streamedSpec(call.Args, spec, zoneName != acct.TimeZone, closed); err != nil {
			return reportAnswer{}, err
		}
	}
	req, err := newRequest(op, map[string]string{"accountsId": accountID}, nil, map[string]any{"reportSpec": spec})
	if err != nil {
		return reportAnswer{}, err
	}
	raw, err := p.client.Send(ctx, call.Handle, req)
	var syntax *json.SyntaxError
	if id == campaignReportOp && errors.As(err, &syntax) {
		// THE ONE REPORT GOOGLE CAPS AT NO ROW COUNT: an answer past the most
		// one call reads is cut, and reads as broken JSON.
		err = &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome, Err: refusal(growth.RefusedInvalid,
			"the campaign report is larger than one answer holds; narrow its dates or dimensions.")}
	}
	var messages []googleReport
	if err == nil {
		err = decodeReport(id, raw, &messages)
	}
	p.client.MadeOrFailed(ctx, call, req, growth.ClassRead, err)
	if err != nil {
		return reportAnswer{}, err
	}
	out := shape(messages, stringsOf(dims), stringsOf(metrics), limit)
	out.Account = accountID
	out.Report.TimeZone = zoneName
	if out.Currency == "" {
		out.Currency = acct.Currency
	}
	yesterday := period.At(period.Day, p.now(), zone).Shift(-1).Start.Format(time.DateOnly)
	out.Report.DataThrough = min(days.End, yesterday)
	out.Report.Estimated = slices.ContainsFunc(stringsOf(metrics), func(m string) bool {
		return slices.Contains(moneyMetrics, m)
	})
	for i, c := range out.Report.Columns {
		if c.Type == growth.ColumnMoney {
			out.Report.Columns[i].Currency = out.Currency
		}
	}
	return out, nil
}

// days are the report's first and last day in zone: its named range, its
// dates, or the last 28 complete days. A campaign report spans at most 30.
func (p *Product) days(args map[string]any, zone *time.Location, campaign bool) (growth.Days, error) {
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
	// CALENDAR DAYS, NOT HOURS: a range across a clock change is no shorter.
	first, errA := time.Parse(time.DateOnly, days.Start)
	last, errB := time.Parse(time.DateOnly, days.End)
	switch {
	case errA != nil || errB != nil:
		return days, refusal(growth.RefusedInvalid, "dates are YYYY-MM-DD. No vendor was reached.")
	case last.Before(first):
		return days, refusal(growth.RefusedInvalid, "the last day is before the first. No vendor was reached.")
	case campaign && last.Sub(first) >= maxCampaignDays*24*time.Hour:
		return days, refusal(growth.RefusedInvalid, fmt.Sprintf("a campaign report spans at most %d days. "+
			"No vendor was reached.", maxCampaignDays))
	}
	return days, nil
}

// reportLists are each report's closed dimensions and metrics.
var reportLists = map[string][2][]string{
	networkReportOp:   {networkDimensions, networkMetrics},
	mediationReportOp: {mediationDimensions, mediationMetrics},
	campaignReportOp:  {campaignDimensions, campaignMetrics},
}

// within refuses values not in allowed, naming them all.
func within(arg string, values, allowed []string) error {
	for _, v := range values {
		if !slices.Contains(allowed, v) {
			return refusal(growth.RefusedInvalid, fmt.Sprintf("%s takes %s, not %q. No vendor was reached.", arg,
				strings.Join(allowed, ", "), v))
		}
	}
	return nil
}

// date is a YYYY-MM-DD day as Google's Date.
func date(day string) map[string]any {
	t, _ := time.Parse(time.DateOnly, day)
	return map[string]any{"year": t.Year(), "month": int(t.Month()), "day": t.Day()}
}

// streamedSpec adds a network or mediation report's filters, sorting,
// localization and zone, held to the report's closed lists; foreign is a zone
// other than the account's.
func streamedSpec(args, spec map[string]any, foreign bool, closed [2][]string) error {
	var filters []any
	for _, f := range listOf(args["dimension_filters"]) {
		if err := within("dimension_filters.dimension", []string{argString(f, "dimension")}, closed[0]); err != nil {
			return err
		}
		filters = append(filters, map[string]any{"dimension": f["dimension"],
			"matchesAny": map[string]any{"values": f["matches_any"]}})
	}
	if len(filters) > 0 {
		spec["dimensionFilters"] = filters
	}
	var sorts []any
	for _, s := range listOf(args["sort"]) {
		cond := map[string]any{"order": s["order"]}
		if err := within("sort.order", []string{argString(s, "order")}, []string{"ASCENDING", "DESCENDING"}); err != nil {
			return err
		}
		d, byDim := s["dimension"].(string)
		m, byMetric := s["metric"].(string)
		switch {
		case byDim == byMetric:
			return refusal(growth.RefusedInvalid, "each sort names a dimension or a metric. No vendor was reached.")
		case byDim:
			if err := within("sort.dimension", []string{d}, closed[0]); err != nil {
				return err
			}
			cond["dimension"] = d
		default:
			if err := within("sort.metric", []string{m}, closed[1]); err != nil {
				return err
			}
			cond["metric"] = m
		}
		sorts = append(sorts, cond)
	}
	if len(sorts) > 0 {
		spec["sortConditions"] = sorts
	}
	loc := map[string]any{}
	if c := argString(args, "currency"); c != "" {
		loc["currencyCode"] = c
	}
	if l := argString(args, "language"); l != "" {
		loc["languageCode"] = l
	}
	if len(loc) > 0 {
		spec["localizationSettings"] = loc
	}
	if foreign {
		spec["timeZone"] = argString(args, "time_zone")
	}
	return nil
}

// listOf is a list argument's objects.
func listOf(v any) []map[string]any {
	list, _ := v.([]any)
	var out []map[string]any
	for _, e := range list {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// stringsOf is a names list as strings.
func stringsOf(list []any) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		out = append(out, v.(string))
	}
	return out
}

// decodeReport reads a report's messages: a streamed report's array, or a
// campaign report's rows.
func decodeReport(id string, raw []byte, into *[]googleReport) error {
	unknown := func(err error) error {
		return &googlegrowth.CallError{Failure: growth.FailureUnknownOutcome,
			Err: fmt.Errorf("admob: decode %s: %w", id, err)}
	}
	if id != campaignReportOp {
		if err := json.Unmarshal(raw, into); err != nil {
			return unknown(err)
		}
		return nil
	}
	var campaign struct {
		Rows []googleRow `json:"rows"`
	}
	if err := json.Unmarshal(raw, &campaign); err != nil {
		return unknown(err)
	}
	for i := range campaign.Rows {
		*into = append(*into, googleReport{Row: &campaign.Rows[i]})
	}
	return nil
}

// shape is a report's messages as the answer gives them: a column per
// dimension (and its label where somebody named it) and per metric, at most
// limit rows, cut where Google had more.
func shape(messages []googleReport, dims, metrics []string, limit int) reportAnswer {
	var out reportAnswer
	var rows []*googleRow
	for _, m := range messages {
		switch {
		case m.Header != nil:
			out.Currency = m.Header.Localization.Currency
		case m.Row != nil:
			rows = append(rows, m.Row)
		case m.Footer != nil:
			out.Warnings = m.Footer.Warnings
			if n, err := strconv.ParseInt(m.Footer.Matching, 10, 64); err == nil {
				out.Report.TotalRowsEstimate = &n
			}
		}
	}
	r := &out.Report
	for _, d := range dims {
		typ := growth.ColumnText
		if d == "DATE" {
			typ = growth.ColumnDate
		}
		r.Columns = append(r.Columns, growth.Column{Name: d, Type: typ, Untrusted: slices.Contains(untrustedValues, d)})
		if slices.Contains(labelled, d) {
			r.Columns = append(r.Columns, growth.Column{Name: d + "_NAME", Type: growth.ColumnText, Untrusted: true})
		}
	}
	for _, m := range metrics {
		typ := growth.ColumnNumber
		if slices.Contains(moneyMetrics, m) {
			typ = growth.ColumnMoney
		}
		r.Columns = append(r.Columns, growth.Column{Name: m, Type: typ})
	}
	// CUT ONLY ON A ROW PAST THE LIMIT: Google says its matching count may be
	// off, so it is passed on as an estimate and decides nothing.
	if len(rows) > limit {
		rows, r.Truncated = rows[:limit], true
	}
	r.Rows = make([][]any, 0, len(rows))
	for _, row := range rows {
		var cells []any
		for _, d := range dims {
			v := row.Dimensions[d]
			value := v.Value
			if d == "DATE" && len(value) == 8 {
				value = value[:4] + "-" + value[4:6] + "-" + value[6:]
			}
			cells = append(cells, value)
			if slices.Contains(labelled, d) {
				cells = append(cells, v.Label)
			}
		}
		for _, m := range metrics {
			cells = append(cells, metricCell(m, row))
		}
		r.Rows = append(r.Rows, cells)
	}
	if len(r.Rows) == 0 {
		r.EmptyReason = growth.EmptyNoData
	}
	return out
}

// metricCell is one metric's value: money as a decimal of the report's
// currency from Google's micros, never a float; a count as Google's integer;
// a rate as Google's double.
func metricCell(name string, row *googleRow) any {
	v, ok := row.Metrics[name]
	switch {
	case !ok:
		return nil
	case v.Micros != "":
		micros, err := strconv.ParseInt(v.Micros, 10, 64)
		if err != nil {
			return nil
		}
		s, _ := growth.FormatAmount(micros, 6)
		return strings.TrimRight(strings.TrimRight(s, "0"), ".")
	case v.Integer != "":
		n, err := strconv.ParseInt(v.Integer, 10, 64)
		if err != nil {
			return nil
		}
		return n
	case v.Double != nil:
		return *v.Double
	}
	return nil
}
